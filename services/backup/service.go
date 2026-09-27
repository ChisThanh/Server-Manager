// Package backup: Scheduled, encrypted backups of files, databases and Docker volumes to local/S3 storage, and restore.
//
// A backup run streams the output of a dump/tar pipeline on the server
// (conn.Stream) through a sha256 counter and optional age encryption (in the
// app, so plaintext never reaches the destination) into a sink: a folder on
// this computer, a directory on the server, or S3-compatible storage. Each
// backup object gets a .manifest.json sidecar; runs are recorded in the runs
// table and executed as core Jobs (live output, cancel).
package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// AppVersion is recorded in manifests; set at build time with
// -ldflags "-X server-manager/services/backup.AppVersion=1.2.3".
var AppVersion = "dev"

// EventRun is emitted when a backup/restore run starts or ends.
const EventRun = "backup:run"

type RunEvent struct {
	Server string `json:"server"`
	JobID  string `json:"jobId"`
	RunID  string `json:"runId"`
	Status string `json:"status"`
}

func init() {
	application.RegisterEvent[RunEvent](EventRun)
}

// maxConcurrent is the global limit of simultaneous backup runs.
const maxConcurrent = 2

type BackupService struct {
	core *core.Core

	cfgMu   sync.Mutex // serializes config read-modify-write
	mu      sync.Mutex
	running map[string]string // backup job id → core job id ("" while starting)
	sem     chan struct{}
	wg      sync.WaitGroup
	sched   *scheduler
}

func New(c *core.Core) *BackupService {
	return &BackupService{core: c, running: map[string]string{}, sem: make(chan struct{}, maxConcurrent)}
}

// ServiceStartup recovers interrupted runs and starts the scheduler.
func (s *BackupService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	s.recoverRuns()
	s.startScheduler(30 * time.Second)
	return nil
}

// ServiceShutdown stops the scheduler and cancels running backups, giving
// them a moment to clean up partial uploads.
func (s *BackupService) ServiceShutdown() error {
	s.stopScheduler()
	s.mu.Lock()
	ids := []string{}
	for _, id := range s.running {
		if id != "" {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.core.Jobs.Cancel(id)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	return nil
}

// ---- remote execution ----

// stream runs a sh script on the server (as root when root is set and the
// login user isn't), wiring stdin/stdout/stderr. It mirrors core's sudo
// handling but lets the caller own stdout, which carries the backup data.
func (s *BackupService) stream(ctx context.Context, conn *sshx.Conn, script string, root bool, password string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := "sh -c " + core.Q(script)
	in := stdin
	usedPW := ""
	if root && !s.core.IsRoot(ctx, conn) {
		probe, err := conn.Exec(ctx, "sudo -n true", nil)
		if err != nil {
			return -1, err
		}
		switch {
		case probe.ExitCode == 0:
			cmd = "sudo -n -- " + cmd
		case strings.Contains(probe.Stderr, "not found") && strings.Contains(probe.Stderr, "sudo"):
			return -1, apperr.New("sudo.missing")
		default:
			pw := s.core.SudoPassword(conn, password)
			if pw == "" {
				return -1, apperr.New("sudo.required")
			}
			usedPW = pw
			cmd = "sudo -S -p '' -- " + cmd
			if stdin != nil {
				in = io.MultiReader(strings.NewReader(pw+"\n"), stdin)
			} else {
				in = strings.NewReader(pw + "\n")
			}
		}
	}
	tail := &tailBuffer{max: 2048}
	if stderr == nil {
		stderr = io.Discard
	}
	code, err := conn.Stream(ctx, cmd, in, stdout, io.MultiWriter(stderr, tail))
	if err == nil && code != 0 && usedPW != "" {
		msg := tail.String()
		if strings.Contains(msg, "incorrect password") || strings.Contains(msg, "Sorry, try again") {
			return code, apperr.New("sudo.wrongPassword")
		}
	}
	return code, err
}

// checkSudo verifies up front that root commands will work (so the UI can
// prompt for the sudo password before a job starts).
func (s *BackupService) checkSudo(connID string, root bool, pw string) error {
	if !root {
		return nil
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	conn, err := s.core.AnyConn(ctx, connID)
	if err != nil {
		return err
	}
	_, err = s.core.RunOK(ctx, conn, "true", true, pw, "")
	return err
}

// ---- run history (runs table) ----

const (
	kindBackup  = "backup"
	kindRestore = "backup.restore"
)

// RunData is the JSON stored in runs.data.
type RunData struct {
	JobName      string        `json:"jobName"`
	Type         string        `json:"type"`
	Trigger      string        `json:"trigger"` // manual | schedule | missed | restore
	CoreJob      string        `json:"coreJob"`
	Destination  string        `json:"destination"`
	Key          string        `json:"key"`
	Location     string        `json:"location"`
	Size         int64         `json:"size"`
	ObjectSize   int64         `json:"objectSize"`
	SHA256       string        `json:"sha256"`
	ObjectSHA256 string        `json:"objectSha256"`
	Encrypted    bool          `json:"encrypted"`
	Compression  string        `json:"compression"`
	Deleted      int           `json:"deleted"`
	Warnings     []string      `json:"warnings"`
	Error        *apperr.Error `json:"error"`
}

// Run is one backup or restore run.
type Run struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // backup | backup.restore
	Server   string `json:"server"`
	JobID    string `json:"jobId"`
	Started  int64  `json:"started"`  // unix ms
	Finished int64  `json:"finished"` // 0 while running
	Status   string `json:"status"`   // running | ok | failed | cancelled
	Actor    string `json:"actor"`
	RunData
}

func (s *BackupService) insertRun(id, kind string, job *Job, started time.Time, actor string, d RunData) {
	b, _ := json.Marshal(d)
	_, _ = s.core.DB.Exec(`INSERT INTO runs(id, kind, server, ref, started, finished, status, actor, version, data, log) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, kind, job.Server, job.ID, started.UnixMilli(), 0, "running", actor, AppVersion, string(b), "")
	core.Emit(EventRun, RunEvent{Server: job.Server, JobID: job.ID, RunID: id, Status: "running"})
}

func (s *BackupService) finishRun(id string, job *Job, status string, d RunData, log string) {
	b, _ := json.Marshal(d)
	_, _ = s.core.DB.Exec(`UPDATE runs SET finished=?, status=?, data=?, log=? WHERE id=?`,
		time.Now().UnixMilli(), status, string(b), logTail(log, 64<<10), id)
	core.Emit(EventRun, RunEvent{Server: job.Server, JobID: job.ID, RunID: id, Status: status})
}

// recoverRuns marks runs left "running" by a crash as failed.
func (s *BackupService) recoverRuns() {
	rows, err := s.core.DB.Query(`SELECT id, data FROM runs WHERE kind IN (?, ?) AND status='running'`, kindBackup, kindRestore)
	if err != nil {
		return
	}
	type row struct{ id, data string }
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.data) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	for _, r := range list {
		var d RunData
		_ = json.Unmarshal([]byte(r.data), &d)
		d.Error = apperr.New("backup.interrupted")
		b, _ := json.Marshal(d)
		_, _ = s.core.DB.Exec(`UPDATE runs SET status='failed', finished=?, data=? WHERE id=?`, time.Now().UnixMilli(), string(b), r.id)
	}
}

func scanRuns(rows *sql.Rows) ([]Run, error) {
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var data string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Server, &r.JobID, &r.Started, &r.Finished, &r.Status, &r.Actor, &data); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(data), &r.RunData)
		if r.Warnings == nil {
			r.Warnings = []string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const runCols = `id, kind, server, ref, started, finished, status, actor, data`

// lastRun returns the newest run of a job with the given status ("" = any).
func (s *BackupService) lastRun(jobID, status string) (*Run, error) {
	q := `SELECT ` + runCols + ` FROM runs WHERE kind=? AND ref=?`
	args := []any{kindBackup, jobID}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	rows, err := s.core.DB.Query(q+` ORDER BY started DESC LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	runs, err := scanRuns(rows)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

var reANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func logTail(s string, n int) string {
	s = reANSI.ReplaceAllString(s, "")
	if len(s) > n {
		s = "…\n" + s[len(s)-n:]
	}
	return s
}

// errOf renders an error for events/logs.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return apperr.From(err).Error()
}

func isCancel(err error, ctx context.Context) bool {
	return errors.Is(err, context.Canceled) || (ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled))
}
