package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// Manifest is the .manifest.json sidecar of a backup object. It never
// contains secrets.
type Manifest struct {
	Format       int               `json:"format"`
	JobID        string            `json:"jobId"`
	JobName      string            `json:"jobName"`
	Type         string            `json:"type"`
	Server       string            `json:"server"`
	ServerName   string            `json:"serverName"`
	Database     string            `json:"database,omitempty"`
	Volume       string            `json:"volume,omitempty"`
	Paths        []string          `json:"paths,omitempty"`
	DumpFormat   string            `json:"dumpFormat"` // tar | pg-custom | sql | rdb | raw
	Created      int64             `json:"created"`    // unix ms
	Object       string            `json:"object"`
	Size         int64             `json:"size"`   // payload (after compression, before encryption)
	SHA256       string            `json:"sha256"` // of the payload
	ObjectSize   int64             `json:"objectSize"`
	ObjectSHA256 string            `json:"objectSha256"` // of the stored bytes
	Compression  string            `json:"compression"`
	Encrypted    bool              `json:"encrypted"`
	Encryption   string            `json:"encryption,omitempty"`
	Tools        map[string]string `json:"tools"`
	AppVersion   string            `json:"appVersion"`
}

func dumpFormat(j *Job) string {
	switch j.Type {
	case TypeFiles, TypeVolume:
		return "tar"
	case TypePostgres:
		if j.Database == "" {
			return "sql"
		}
		return "pg-custom"
	case TypeMySQL:
		return "sql"
	case TypeRedis:
		return "rdb"
	}
	return "raw"
}

func jobSecret(id, name string) string { return "backup:job:" + id + ":" + name }

// startRun starts a backup run as a core job. It refuses to overlap runs of
// the same backup job.
func (s *BackupService) startRun(job Job, trigger, actor, pw string) (*core.Job, error) {
	s.mu.Lock()
	if _, busy := s.running[job.ID]; busy {
		s.mu.Unlock()
		return nil, apperr.New("backup.alreadyRunning", "name", job.Name)
	}
	s.running[job.ID] = ""
	s.wg.Add(1)
	s.mu.Unlock()
	cj := s.core.Jobs.Start(job.Server, "backup.run", job.Name, func(ctx context.Context, j *core.Job) error {
		defer func() {
			s.mu.Lock()
			delete(s.running, job.ID)
			s.mu.Unlock()
			s.wg.Done()
		}()
		return s.execute(ctx, j, job, trigger, actor, pw)
	})
	s.mu.Lock()
	if _, ok := s.running[job.ID]; ok {
		s.running[job.ID] = cj.ID()
	}
	s.mu.Unlock()
	return cj, nil
}

// acquire waits for a free run slot.
func (s *BackupService) acquire(ctx context.Context, j *core.Job) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	default:
	}
	j.Logf("Waiting for a free slot (at most %d backups run at the same time)…", maxConcurrent)
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// execute performs one backup run.
func (s *BackupService) execute(jctx context.Context, j *core.Job, job Job, trigger, actor, pw string) (err error) {
	runID := uuid.NewString()
	d := RunData{JobName: job.Name, Type: job.Type, Trigger: trigger, CoreJob: j.ID(), Destination: job.Destination, Warnings: []string{}}
	s.insertRun(runID, kindBackup, &job, time.Now(), actor, d)
	j.SetResult(runID)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
		status := "ok"
		switch {
		case err != nil && isCancel(err, jctx):
			status = "cancelled"
			d.Error = apperr.New("backup.cancelled")
		case err != nil:
			status = "failed"
			d.Error = apperr.From(err)
		}
		if err != nil {
			j.Logf("\x1b[31m✖ %s\x1b[0m", errText(err))
		}
		s.finishRun(runID, &job, status, d, j.Info().Log)
		switch status {
		case "ok":
			s.core.AddEvent(core.Event{Server: job.Server, Kind: "backup", Severity: "info", Code: "backup.ok",
				Params: map[string]string{"name": job.Name}, Detail: d.Location, Actor: actor})
		case "failed":
			s.core.AddEvent(core.Event{Server: job.Server, Kind: "backup", Severity: "crit", Code: "backup.failed",
				Params: map[string]string{"name": job.Name}, Detail: errText(err), Actor: actor})
		}
		if trigger == "manual" {
			s.core.Audit(job.Server, "backup.run", job.Name, d.Location, err)
		}
	}()

	if err := s.acquire(jctx, j); err != nil {
		return err
	}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(jctx, time.Duration(job.TimeoutMin)*time.Minute)
	defer cancel()
	defer func() {
		if err != nil && ctx.Err() == context.DeadlineExceeded && jctx.Err() == nil {
			err = apperr.New("backup.timeout", "min", strconv.Itoa(job.TimeoutMin))
		}
	}()

	if err := s.core.Require(job.Server, core.PermBackup); err != nil {
		return err
	}
	dest, err := s.getDest(job.Destination)
	if err != nil {
		return err
	}
	var passphrase, dbpw string
	if job.Encrypt {
		if passphrase = store.Keychain(jobSecret(job.ID, "passphrase")); passphrase == "" {
			return apperr.New("backup.noPassphrase")
		}
	}
	if job.AuthMode == "password" {
		if dbpw = store.Keychain(jobSecret(job.ID, "dbpass")); dbpw == "" && job.Type != TypeRedis {
			return apperr.New("backup.noDbPassword")
		}
	}

	j.Step("Connecting to %s", s.core.ServerName(job.Server))
	conn, err := s.core.AnyConn(ctx, job.Server)
	if err != nil {
		return err
	}
	root := job.runsAsRoot()

	// Tools and versions.
	tctx, tcancel := context.WithTimeout(ctx, 30*time.Second)
	res, err := s.core.Run(tctx, conn, toolsScript(&job), false, "", "")
	tcancel()
	if err != nil {
		return err
	}
	have, tools := parseTools(res.Stdout)
	compression := job.Compression
	if compression == "zstd" && !have["zstd"] {
		j.Logf("\x1b[33mzstd is not installed on the server; using gzip instead\x1b[0m")
		d.Warnings = append(d.Warnings, "zstdMissing")
		compression = "gzip"
	}
	if compression == "gzip" && !have["gzip"] {
		return apperr.New("backup.toolMissing", "tool", "gzip")
	}
	direct := dest.Type == DestServer && !job.Encrypt
	if direct && !have["sha256sum"] {
		return apperr.New("backup.toolMissing", "tool", "sha256sum")
	}
	for k, v := range tools {
		j.Logf("  %s: %s", k, v)
	}

	// Remote stderr goes to the job log; the tail explains failures.
	errTail := &tailBuffer{max: 4000}
	stderr := io.MultiWriter(j, errTail)

	if job.PreHook != "" {
		j.Step("Running the pre-backup hook")
		code, err := s.stream(ctx, conn, job.PreHook, root, pw, nil, j, j)
		if err != nil {
			return err
		}
		if code != 0 {
			return apperr.New("backup.hookFailed", "hook", "pre", "code", strconv.Itoa(code))
		}
	}
	if job.PostHook != "" {
		defer func() {
			status := "ok"
			if err != nil {
				status = "failed"
			}
			hctx, hcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
			defer hcancel()
			j.Step("Running the post-backup hook")
			script := "SM_BACKUP_STATUS=" + status + "; SM_BACKUP_FILE=" + core.Q(d.Location) + "; export SM_BACKUP_STATUS SM_BACKUP_FILE\n" + job.PostHook
			code, herr := s.stream(hctx, conn, script, root, pw, nil, j, j)
			if herr != nil || code != 0 {
				j.Logf("\x1b[33mpost-backup hook failed (exit %d) %s\x1b[0m", code, errText(herr))
				d.Warnings = append(d.Warnings, "postHookFailed")
			}
		}()
	}

	if job.Type == TypeVolume && job.StopContainers {
		ids, err := s.volumeContainers(ctx, conn, job.Volume, pw)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			j.Step("Stopping %d container(s) using volume %s", len(ids), job.Volume)
			if _, err := s.core.RunOK(ctx, conn, "docker stop "+strings.Join(ids, " "), true, pw, ""); err != nil {
				s.startContainers(conn, ids, pw, j)
				return err
			}
			defer s.startContainers(conn, ids, pw, j)
		}
	}

	started := time.Now()
	name := objectName(started, dumpExt(&job), compression, job.Encrypt)
	key := job.Folder + "/" + name
	sk, err := s.sinkFor(dest, conn, root, pw)
	if err != nil {
		return err
	}
	d.Key, d.Location, d.Compression, d.Encrypted = key, sk.Describe(key), compression, job.Encrypt
	j.Step("Backing up to %s", d.Location)

	var stdin io.Reader
	if job.AuthMode == "password" {
		stdin = strings.NewReader(dbpw + "\n")
	}

	m := Manifest{
		Format: 1, JobID: job.ID, JobName: job.Name, Type: job.Type, Server: job.Server, ServerName: s.core.ServerName(job.Server),
		Database: job.Database, Volume: job.Volume, DumpFormat: dumpFormat(&job), Created: started.UnixMilli(), Object: name,
		Compression: compression, Encrypted: job.Encrypt, Tools: tools, AppVersion: AppVersion,
	}
	if job.Type == TypeFiles {
		m.Paths = job.Paths
	}
	if job.Encrypt {
		m.Encryption = "age-scrypt"
	}

	if direct {
		var out bytes.Buffer
		code, err := s.stream(ctx, conn, directScript(&job, compression, path.Join(dest.Path, key)), root, pw, stdin, &out, stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return apperr.New("backup.commandFailed", "code", strconv.Itoa(code)).WithDetail(lastLines(errTail.String(), 6))
		}
		sum, size, ok := parseDirect(out.String())
		if !ok {
			return apperr.New("backup.commandFailed", "code", "?").WithDetail(out.String())
		}
		m.Size, m.SHA256, m.ObjectSize, m.ObjectSHA256 = size, sum, size, sum
	} else {
		w, err := sk.Create(ctx, key)
		if err != nil {
			return err
		}
		objHC := newHashCounter()
		sctx, scancel := context.WithCancel(ctx)
		fw := &failWriter{w: io.MultiWriter(w, objHC), onErr: scancel}
		var payloadDst io.Writer = fw
		var enc io.WriteCloser
		if job.Encrypt {
			if enc, err = encryptWriter(fw, passphrase); err != nil {
				scancel()
				w.Abort()
				return err
			}
			payloadDst = enc
		}
		payHC := newHashCounter()
		out := io.MultiWriter(payloadDst, payHC)
		code, serr := s.stream(sctx, conn, streamScript(&job, compression), root, pw, stdin, out, stderr)
		scancel()
		var failure error
		switch {
		case fw.Err() != nil:
			failure = apperr.Wrap(fw.Err(), "backup.writeFailed")
		case serr != nil:
			failure = serr
		case code != 0:
			failure = apperr.New("backup.commandFailed", "code", strconv.Itoa(code)).WithDetail(lastLines(errTail.String(), 6))
		case payHC.n == 0:
			failure = apperr.New("backup.emptyOutput")
		}
		if failure == nil && enc != nil {
			if cerr := enc.Close(); cerr != nil {
				failure = apperr.Wrap(cerr, "backup.encryptFailed")
			}
		}
		if failure == nil && fw.Err() != nil {
			failure = apperr.Wrap(fw.Err(), "backup.writeFailed")
		}
		if failure != nil {
			w.Abort()
			return failure
		}
		if err := w.Commit(); err != nil {
			return err
		}
		m.Size, m.SHA256, m.ObjectSize, m.ObjectSHA256 = payHC.n, payHC.Sum(), objHC.n, objHC.Sum()
	}

	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := sk.PutSmall(ctx, key+manifestSuffix, mb); err != nil {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		_ = sk.Remove(rctx, key)
		rcancel()
		return err
	}
	d.Size, d.ObjectSize, d.SHA256, d.ObjectSHA256 = m.Size, m.ObjectSize, m.SHA256, m.ObjectSHA256
	j.Logf("\x1b[32m✔ Stored %s (%s, sha256 %s) in %s\x1b[0m", name, humanBytes(m.ObjectSize), m.SHA256[:16]+"…", time.Since(started).Round(time.Second))

	if job.VerifyAfter {
		j.Step("Verifying the stored backup")
		if _, err := s.verifyObject(ctx, j, sk, key, &m, passphrase, false); err != nil {
			return err
		}
		j.Logf("\x1b[32m✔ Verified\x1b[0m")
	}

	if job.Retention.KeepLast > 0 || job.Retention.MaxAgeDays > 0 {
		j.Step("Applying retention (keep last %d, max age %d days)", job.Retention.KeepLast, job.Retention.MaxAgeDays)
		n, rerr := s.applyRetention(ctx, j, &job, sk, time.Now())
		d.Deleted = n
		if rerr != nil {
			j.Logf("\x1b[33mretention: %s\x1b[0m", errText(rerr))
			d.Warnings = append(d.Warnings, "retentionFailed")
		}
	}
	return nil
}

// volumeContainers lists running containers that use a volume.
func (s *BackupService) volumeContainers(ctx context.Context, conn *sshx.Conn, volume, pw string) ([]string, error) {
	res, err := s.core.RunOK(ctx, conn, "docker ps -q --filter volume="+core.Q(volume), true, pw, "")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, f := range strings.Fields(res.Stdout) {
		if reContainerID.MatchString(f) {
			ids = append(ids, f)
		}
	}
	return ids, nil
}

func (s *BackupService) startContainers(conn *sshx.Conn, ids []string, pw string, j *core.Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	j.Step("Starting container(s) again")
	if _, err := s.core.RunOK(ctx, conn, "docker start "+strings.Join(ids, " "), true, pw, ""); err != nil {
		j.Logf("\x1b[31mcould not restart containers %s: %s\x1b[0m", strings.Join(ids, " "), errText(err))
	}
}

var reContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

func parseTools(out string) (map[string]bool, map[string]string) {
	have := map[string]bool{}
	tools := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if k == "have" {
			have[v] = true
		} else if v != "" {
			tools[k] = v
		}
	}
	return have, tools
}

// parseDirect reads "SMBK <sha256> <size>".
func parseDirect(out string) (string, int64, bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "SMBK" && len(f[1]) == 64 {
			n, err := strconv.ParseInt(f[2], 10, 64)
			return f[1], n, err == nil
		}
	}
	return "", 0, false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
