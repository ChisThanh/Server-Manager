package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/store"
)

// ---- destinations ----

func (s *BackupService) getDest(id string) (Destination, error) {
	var d Destination
	if err := s.core.DB.Get(nsDest, id, &d); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return d, apperr.New("backup.destNotFound")
		}
		return d, err
	}
	d.HasSecret = d.Type == DestS3 && store.Keychain(destSecretName(d.ID)) != ""
	return d, nil
}

// ListDestinations returns every destination (secrets are never returned).
func (s *BackupService) ListDestinations() ([]Destination, error) {
	list, err := db.List[Destination](s.core.DB, nsDest)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].HasSecret = list[i].Type == DestS3 && store.Keychain(destSecretName(list[i].ID)) != ""
	}
	sort.Slice(list, func(a, b int) bool { return strings.ToLower(list[a].Name) < strings.ToLower(list[b].Name) })
	return list, nil
}

// SaveDestination creates or updates a destination. secret is the S3
// secret key; "" keeps the stored one.
func (s *BackupService) SaveDestination(d Destination, secret string) (Destination, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := normalizeDest(&d); err != nil {
		return d, err
	}
	now := time.Now().UnixMilli()
	isNew := d.ID == ""
	if isNew {
		d.ID = uuid.NewString()
		d.Created = now
	} else {
		if !reID.MatchString(d.ID) {
			return d, invalid("id")
		}
		old, err := s.getDest(d.ID)
		if err != nil {
			return d, err
		}
		if old.Type != d.Type {
			return d, invalid("type")
		}
		d.Created = old.Created
	}
	d.Updated = now
	if d.Type == DestS3 {
		if secret != "" {
			if len(secret) > 1024 || hasCtl(secret) {
				return d, invalid("secret")
			}
			if err := store.SetKeychain(destSecretName(d.ID), secret); err != nil {
				return d, err
			}
		} else if store.Keychain(destSecretName(d.ID)) == "" {
			return d, apperr.New("backup.s3.noSecret")
		}
	}
	d.HasSecret = false
	err := s.core.DB.Put(nsDest, d.ID, d)
	detail := fmt.Sprintf("type=%s", d.Type)
	switch d.Type {
	case DestS3:
		detail += fmt.Sprintf(" endpoint=%s bucket=%s prefix=%s", d.Endpoint, d.Bucket, d.Prefix)
		if secret != "" {
			detail += " (secret key updated)"
		}
	default:
		detail += " path=" + d.Path
	}
	s.core.Audit("", "backup.dest.save", d.Name, detail, err)
	if err != nil {
		return d, err
	}
	d.HasSecret = d.Type == DestS3
	return d, nil
}

// DeleteDestination removes a destination that no job uses. Stored
// backups are not touched.
func (s *BackupService) DeleteDestination(id string) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	d, err := s.getDest(id)
	if err != nil {
		return err
	}
	jobs, err := db.List[Job](s.core.DB, nsJob)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Destination == id {
			return apperr.New("backup.destInUse", "job", j.Name, "server", s.core.ServerName(j.Server))
		}
	}
	err = s.core.DB.Delete(nsDest, id)
	if err == nil {
		_ = store.SetKeychain(destSecretName(id), "")
	}
	s.core.Audit("", "backup.dest.delete", d.Name, "", err)
	return err
}

// DestTest is the result of a successful destination test.
type DestTest struct {
	Millis int64 `json:"millis"`
	// NeedsRoot: the server directory is only writable with sudo.
	NeedsRoot bool `json:"needsRoot"`
}

// TestDestination writes, reads back and deletes a small probe object.
// connID is required for server destinations. secret "" uses the stored one.
func (s *BackupService) TestDestination(connID string, d Destination, secret string) (DestTest, error) {
	var res DestTest
	if err := normalizeDest(&d); err != nil {
		return res, err
	}
	if secret == "" && d.ID != "" && reID.MatchString(d.ID) {
		secret = store.Keychain(destSecretName(d.ID))
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	start := time.Now()
	probe := []byte("server-manager backup probe " + time.Now().UTC().Format(time.RFC3339) + "\n")
	key := ".sm-probe-" + uuid.NewString()[:8]
	try := func(sk sink) error {
		if err := sk.PutSmall(ctx, key, probe); err != nil {
			return err
		}
		got, err := sk.GetSmall(ctx, key, 1<<16)
		if err != nil {
			_ = sk.Remove(ctx, key)
			return err
		}
		if err := sk.Remove(ctx, key); err != nil {
			return err
		}
		if !bytes.Equal(got, probe) {
			return apperr.New("backup.probeMismatch")
		}
		return nil
	}
	var err error
	switch d.Type {
	case DestLocal:
		if err = os.MkdirAll(d.Path, 0o700); err != nil {
			return res, apperr.Wrap(err, "backup.writeFailed")
		}
		err = try(&localSink{base: d.Path})
	case DestServer:
		conn, cerr := s.core.AnyConn(ctx, connID)
		if cerr != nil {
			return res, cerr
		}
		err = try(&serverSink{s: s, conn: conn, base: d.Path})
		if err != nil && !apperr.HasCode(err, "sudo.required") {
			if err2 := try(&serverSink{s: s, conn: conn, base: d.Path, root: true}); err2 == nil {
				err, res.NeedsRoot = nil, true
			}
		}
	case DestS3:
		sk, serr := newS3(d, secret)
		if serr != nil {
			return res, serr
		}
		ok, berr := sk.cl.BucketExists(ctx, sk.bucket)
		if berr != nil {
			return res, s3Err(berr)
		}
		if !ok {
			return res, apperr.New("backup.s3.noBucket")
		}
		err = try(sk)
	}
	res.Millis = time.Since(start).Milliseconds()
	return res, err
}

// PickLocalFolder opens a native folder picker ("" when cancelled).
func (s *BackupService) PickLocalFolder(title string) (string, error) {
	app := application.Get()
	if app == nil {
		return "", nil
	}
	p, err := app.Dialog.OpenFile().SetTitle(title).
		CanChooseDirectories(true).CanChooseFiles(false).CanCreateDirectories(true).
		PromptForSingleSelection()
	if err != nil {
		return "", nil
	}
	return p, nil
}

// ---- jobs ----

func (s *BackupService) fillSecrets(j *Job) {
	j.HasPassphrase = store.Keychain(jobSecret(j.ID, "passphrase")) != ""
	j.HasDBPassword = store.Keychain(jobSecret(j.ID, "dbpass")) != ""
	if j.Paths == nil {
		j.Paths = []string{}
	}
	if j.Excludes == nil {
		j.Excludes = []string{}
	}
}

func (s *BackupService) getJob(connID, id string) (Job, error) {
	var j Job
	if !reID.MatchString(id) {
		return j, apperr.New("backup.jobNotFound")
	}
	if err := s.core.DB.Get(nsJob, id, &j); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return j, apperr.New("backup.jobNotFound")
		}
		return j, err
	}
	if j.Server != connID {
		return j, apperr.New("backup.jobNotFound")
	}
	s.fillSecrets(&j)
	return j, nil
}

// ListJobs returns the server's backup jobs.
func (s *BackupService) ListJobs(connID string) ([]Job, error) {
	all, err := db.List[Job](s.core.DB, nsJob)
	if err != nil {
		return nil, err
	}
	out := []Job{}
	for _, j := range all {
		if j.Server == connID {
			s.fillSecrets(&j)
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out, nil
}

// JobSecrets carries secrets entered in the job editor ("" = keep).
type JobSecrets struct {
	Passphrase string `json:"passphrase"`
	DBPassword string `json:"dbPassword"`
	// ClearDBPassword removes the stored database password.
	ClearDBPassword bool `json:"clearDbPassword"`
}

// SaveJob creates or updates a backup job.
func (s *BackupService) SaveJob(connID string, j Job, sec JobSecrets) (Job, error) {
	if err := s.core.Require(connID, core.PermBackup); err != nil {
		return j, err
	}
	if _, err := s.core.Store.Get(connID); err != nil {
		return j, err
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := normalizeJob(&j); err != nil {
		return j, err
	}
	// Custom commands and hooks run arbitrary commands on the server.
	if j.Type == TypeCustom || j.PreHook != "" || j.PostHook != "" || j.RestoreCommand != "" {
		if err := s.core.Require(connID, core.PermExec); err != nil {
			return j, err
		}
	}
	if _, err := s.getDest(j.Destination); err != nil {
		return j, err
	}
	all, err := db.List[Job](s.core.DB, nsJob)
	if err != nil {
		return j, err
	}
	now := time.Now().UnixMilli()
	isNew := j.ID == ""
	if isNew {
		j.ID = uuid.NewString()
		j.Created = now
		j.Folder = uniqueFolder(all, slug(s.core.ServerName(connID))+"/"+slug(j.Name), j.ID)
	} else {
		old, err := s.getJob(connID, j.ID)
		if err != nil {
			return j, err
		}
		j.Created, j.Folder = old.Created, old.Folder
		if old.Type != j.Type {
			return j, invalid("type")
		}
	}
	j.Server = connID
	j.Updated = now
	for _, o := range all {
		if o.ID != j.ID && o.Server == connID && strings.EqualFold(o.Name, j.Name) {
			return j, apperr.New("backup.nameTaken", "name", j.Name)
		}
	}
	if sec.Passphrase != "" && (len(sec.Passphrase) < 8 || len(sec.Passphrase) > 1024 || strings.ContainsRune(sec.Passphrase, 0)) {
		return j, apperr.New("backup.weakPassphrase")
	}
	if j.Encrypt && sec.Passphrase == "" && store.Keychain(jobSecret(j.ID, "passphrase")) == "" {
		return j, apperr.New("backup.noPassphrase")
	}
	if sec.DBPassword != "" && (len(sec.DBPassword) > 1024 || strings.ContainsAny(sec.DBPassword, "\x00\n\r")) {
		return j, invalid("dbPassword")
	}
	needDBPW := j.AuthMode == "password" && (j.Type == TypePostgres || j.Type == TypeMySQL)
	if needDBPW && sec.DBPassword == "" && (sec.ClearDBPassword || store.Keychain(jobSecret(j.ID, "dbpass")) == "") {
		return j, apperr.New("backup.noDbPassword")
	}
	changed := []string{}
	if sec.Passphrase != "" {
		if err := store.SetKeychain(jobSecret(j.ID, "passphrase"), sec.Passphrase); err != nil {
			return j, err
		}
		changed = append(changed, "passphrase")
	}
	if sec.DBPassword != "" {
		if err := store.SetKeychain(jobSecret(j.ID, "dbpass"), sec.DBPassword); err != nil {
			return j, err
		}
		changed = append(changed, "database password")
	} else if sec.ClearDBPassword {
		_ = store.SetKeychain(jobSecret(j.ID, "dbpass"), "")
	}
	j.HasPassphrase, j.HasDBPassword = false, false
	err = s.core.DB.Put(nsJob, j.ID, j)
	detail := fmt.Sprintf("type=%s schedule=%q destination=%s encrypt=%t compression=%s keepLast=%d maxAgeDays=%d",
		j.Type, j.Schedule.Cron, j.Destination, j.Encrypt, j.Compression, j.Retention.KeepLast, j.Retention.MaxAgeDays)
	if len(changed) > 0 {
		detail += " (updated: " + strings.Join(changed, ", ") + ")"
	}
	action := "backup.job.update"
	if isNew {
		action = "backup.job.create"
	}
	s.core.Audit(connID, action, j.Name, detail, err)
	if err != nil {
		return j, err
	}
	s.mu.Lock()
	sc := s.sched
	s.mu.Unlock()
	if sc != nil {
		sc.reset(j.ID, time.Now())
	}
	s.fillSecrets(&j)
	return j, nil
}

func uniqueFolder(all []Job, want, id string) string {
	for _, o := range all {
		if o.Folder == want {
			parts := strings.SplitN(want, "/", 2)
			seg := parts[1]
			if len(seg) > 56 {
				seg = strings.TrimRight(seg[:56], "-")
			}
			return parts[0] + "/" + seg + "-" + strings.ReplaceAll(id, "-", "")[:6]
		}
	}
	return want
}

// DeleteJob removes a job and its secrets. Backups already stored at the
// destination are kept.
func (s *BackupService) DeleteJob(connID, id string) error {
	if err := s.core.Require(connID, core.PermBackup); err != nil {
		return err
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	j, err := s.getJob(connID, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	_, busy := s.running[id]
	s.mu.Unlock()
	if busy {
		return apperr.New("backup.alreadyRunning", "name", j.Name)
	}
	err = s.core.DB.Delete(nsJob, id)
	if err == nil {
		_ = store.SetKeychain(jobSecret(id, "passphrase"), "")
		_ = store.SetKeychain(jobSecret(id, "dbpass"), "")
	}
	s.core.Audit(connID, "backup.job.delete", j.Name, "", err)
	return err
}

// RevealPassphrase returns a job's encryption passphrase (to keep a copy
// in a password manager). Audited.
func (s *BackupService) RevealPassphrase(connID, id string) (string, error) {
	if err := s.core.Require(connID, core.PermBackup); err != nil {
		return "", err
	}
	j, err := s.getJob(connID, id)
	if err != nil {
		return "", err
	}
	p := store.Keychain(jobSecret(id, "passphrase"))
	if p == "" {
		return "", apperr.New("backup.noPassphrase")
	}
	s.core.Audit(connID, "backup.passphrase.reveal", j.Name, "", nil)
	return p, nil
}

// CronPreview is the cron expression of a schedule and its next runs.
type CronPreview struct {
	Cron string  `json:"cron"`
	Next []int64 `json:"next"` // unix ms
}

// PreviewSchedule validates a schedule and returns its next n runs.
func (s *BackupService) PreviewSchedule(sched Schedule, n int) (CronPreview, error) {
	res := CronPreview{Next: []int64{}}
	expr, err := cronOf(sched)
	if err != nil {
		return res, err
	}
	res.Cron = expr
	if expr == "" {
		return res, nil
	}
	if n <= 0 || n > 20 {
		n = 3
	}
	times, err := nextRuns(expr, time.Now(), n)
	if err != nil {
		return res, err
	}
	for _, t := range times {
		res.Next = append(res.Next, t.UnixMilli())
	}
	return res, nil
}

// Detection lists what the server offers for backups.
type Detection struct {
	Tools   []string `json:"tools"`
	Volumes []string `json:"volumes"`
}

// Detect checks which backup tools exist on the server and lists Docker
// volumes (when docker is usable).
func (s *BackupService) Detect(connID string) (Detection, error) {
	res := Detection{Tools: []string{}, Volumes: []string{}}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return res, err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	out, err := s.core.Run(ctx, conn, `for t in tar gzip zstd sha256sum pg_dump pg_dumpall pg_restore psql mysqldump mariadb-dump mysql mariadb docker redis-cli runuser systemctl; do command -v $t >/dev/null 2>&1 && echo "$t"; done; exit 0`, false, "", "")
	if err != nil {
		return res, err
	}
	res.Tools = strings.Fields(out.Stdout)
	for _, t := range res.Tools {
		if t == "docker" {
			v, err := s.core.RunAuto(ctx, conn, "docker volume ls -q", "", "")
			if err == nil && v.ExitCode == 0 {
				for _, name := range strings.Fields(v.Stdout) {
					if reVolume.MatchString(name) {
						res.Volumes = append(res.Volumes, name)
					}
				}
			}
		}
	}
	return res, nil
}

// ---- runs ----

// RunNow starts a backup job now and returns the core job id.
func (s *BackupService) RunNow(connID, jobID, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermBackup); err != nil {
		return "", err
	}
	j, err := s.getJob(connID, jobID)
	if err != nil {
		return "", err
	}
	if err := s.checkSudo(connID, j.runsAsRoot(), sudoPassword); err != nil {
		return "", err
	}
	cj, err := s.startRun(j, "manual", s.core.Actor(), sudoPassword)
	if err != nil {
		return "", err
	}
	return cj.ID(), nil
}

// JobStatus summarizes a job's recent runs (used by the health module:
// "last successful backup age").
type JobStatus struct {
	JobID       string        `json:"jobId"`
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Enabled     bool          `json:"enabled"`
	Cron        string        `json:"cron"`
	LastRun     int64         `json:"lastRun"` // unix ms (start)
	LastStatus  string        `json:"lastStatus"`
	LastSuccess int64         `json:"lastSuccess"` // unix ms (finish)
	LastFailure int64         `json:"lastFailure"`
	LastSize    int64         `json:"lastSize"`
	LastError   *apperr.Error `json:"lastError"`
	NextRun     int64         `json:"nextRun"` // unix ms, 0 = none
	Running     bool          `json:"running"`
	RunningJob  string        `json:"runningJob"` // core job id
}

// Status returns per-job last success/failure and next run for a server.
func (s *BackupService) Status(connID string) ([]JobStatus, error) {
	jobs, err := s.ListJobs(connID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []JobStatus{}
	for _, j := range jobs {
		st := JobStatus{JobID: j.ID, Name: j.Name, Type: j.Type, Enabled: j.Enabled, Cron: j.Schedule.Cron}
		if nr := nextRun(&j, now); !nr.IsZero() {
			st.NextRun = nr.UnixMilli()
		}
		if r, _ := s.lastRun(j.ID, ""); r != nil {
			st.LastRun, st.LastStatus, st.LastError = r.Started, r.Status, r.Error
		}
		if r, _ := s.lastRun(j.ID, "ok"); r != nil {
			st.LastSuccess, st.LastSize = r.Finished, r.ObjectSize
		}
		if r, _ := s.lastRun(j.ID, "failed"); r != nil {
			st.LastFailure = r.Finished
		}
		s.mu.Lock()
		cid, busy := s.running[j.ID]
		s.mu.Unlock()
		st.Running, st.RunningJob = busy, cid
		out = append(out, st)
	}
	return out, nil
}

// History lists backup and restore runs of a server (jobID "" = all jobs).
func (s *BackupService) History(connID, jobID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ` + runCols + ` FROM runs WHERE kind IN (?, ?) AND server=?`
	args := []any{kindBackup, kindRestore, connID}
	if jobID != "" {
		q += ` AND ref=?`
		args = append(args, jobID)
	}
	rows, err := s.core.DB.Query(q+` ORDER BY started DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// RunLog returns the stored output of a finished run.
func (s *BackupService) RunLog(connID, runID string) (string, error) {
	var log string
	err := s.core.DB.QueryRow(`SELECT log FROM runs WHERE id=? AND server=? AND kind IN (?, ?)`, runID, connID, kindBackup, kindRestore).Scan(&log)
	if err != nil {
		return "", apperr.New("backup.runNotFound")
	}
	return log, nil
}

// ---- stored backups ----

// BackupItem is one stored backup of a job.
type BackupItem struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Time      int64     `json:"time"` // unix ms (from the name)
	Size      int64     `json:"size"` // stored size
	Encrypted bool      `json:"encrypted"`
	Complete  bool      `json:"complete"` // data + manifest present
	Manifest  *Manifest `json:"manifest"`
}

// maxManifestLoads bounds the manifests fetched per listing.
const maxManifestLoads = 100

// ListBackups lists the backups stored for a job, newest first.
func (s *BackupService) ListBackups(connID, jobID, sudoPassword string) ([]BackupItem, error) {
	j, err := s.getJob(connID, jobID)
	if err != nil {
		return nil, err
	}
	dest, err := s.getDest(j.Destination)
	if err != nil {
		return nil, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	var sk sink
	if dest.Type == DestServer {
		conn, err := s.core.AnyConn(ctx, connID)
		if err != nil {
			return nil, err
		}
		sk, err = s.sinkFor(dest, conn, j.runsAsRoot(), sudoPassword)
		if err != nil {
			return nil, err
		}
	} else if sk, err = s.sinkFor(dest, nil, false, ""); err != nil {
		return nil, err
	}
	list, err := sk.List(ctx, j.Folder)
	if err != nil {
		return nil, err
	}
	out := []BackupItem{}
	loads := 0
	for _, b := range groupObjects(list) {
		if !b.HasData {
			continue
		}
		it := BackupItem{Key: j.Folder + "/" + b.Name, Name: b.Name, Time: b.Time.UnixMilli(), Size: b.Size,
			Encrypted: strings.HasSuffix(b.Name, ".age"), Complete: b.complete()}
		if b.HasManifest && loads < maxManifestLoads {
			loads++
			if m, err := readManifest(ctx, sk, it.Key, b.Name); err == nil {
				it.Manifest = m
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// openForJob resolves a job's sink for reading a backup (UI actions).
func (s *BackupService) openForJob(ctx context.Context, j *Job, pw string) (sink, error) {
	dest, err := s.getDest(j.Destination)
	if err != nil {
		return nil, err
	}
	if dest.Type != DestServer {
		return s.sinkFor(dest, nil, false, "")
	}
	conn, err := s.core.AnyConn(ctx, j.Server)
	if err != nil {
		return nil, err
	}
	return s.sinkFor(dest, conn, j.runsAsRoot(), pw)
}

// DeleteBackup deletes one stored backup (and its manifest).
func (s *BackupService) DeleteBackup(connID, jobID, key, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermBackup); err != nil {
		return err
	}
	j, err := s.getJob(connID, jobID)
	if err != nil {
		return err
	}
	if _, err := checkKey(&j, key); err != nil {
		return err
	}
	ctx, cancel := core.Timeout(time.Minute)
	defer cancel()
	sk, err := s.openForJob(ctx, &j, sudoPassword)
	if err != nil {
		return err
	}
	err = sk.Remove(ctx, key)
	if err == nil {
		err = sk.Remove(ctx, key+manifestSuffix)
	}
	s.core.Audit(connID, "backup.delete", sk.Describe(key), j.Name, err)
	return err
}

// Verify downloads a backup, decrypts it and checks its checksums and
// structure without restoring anything. passphrase "" uses the stored one.
func (s *BackupService) Verify(connID, jobID, key, passphrase, sudoPassword string) (string, error) {
	j, err := s.getJob(connID, jobID)
	if err != nil {
		return "", err
	}
	name, err := checkKey(&j, key)
	if err != nil {
		return "", err
	}
	if passphrase == "" {
		passphrase = store.Keychain(jobSecret(j.ID, "passphrase"))
	}
	cj := s.core.Jobs.Start(connID, "backup.verify", j.Name+" · "+name, func(ctx context.Context, cj *core.Job) error {
		sk, err := s.openForJob(ctx, &j, sudoPassword)
		if err != nil {
			return err
		}
		cj.Step("Reading the manifest")
		m, err := readManifest(ctx, sk, key, name)
		if err != nil {
			return err
		}
		cj.Step("Downloading, decrypting and checking %s", sk.Describe(key))
		res, err := s.verifyObject(ctx, cj, sk, key, m, passphrase, true)
		if err != nil {
			return err
		}
		if res.Entries > 0 {
			cj.Logf("archive OK: %d entries", res.Entries)
		}
		cj.Logf("\x1b[32m✔ Backup verified\x1b[0m")
		return nil
	})
	return cj.ID(), nil
}

// Download asks where to save a backup and copies it to this computer,
// decrypted (decrypt) or as stored. Returns the job id, or "" if the user
// cancelled the dialog.
func (s *BackupService) Download(connID, jobID, key string, decrypt bool, passphrase, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermBackup); err != nil {
		return "", err
	}
	j, err := s.getJob(connID, jobID)
	if err != nil {
		return "", err
	}
	name, err := checkKey(&j, key)
	if err != nil {
		return "", err
	}
	if passphrase == "" {
		passphrase = store.Keychain(jobSecret(j.ID, "passphrase"))
	}
	fileName := name
	if decrypt {
		fileName = strings.TrimSuffix(name, ".age")
	}
	fileName = slug(s.core.ServerName(connID)) + "-" + slug(j.Name) + "-" + fileName
	app := application.Get()
	if app == nil {
		return "", nil
	}
	local, err := app.Dialog.SaveFile().SetFilename(fileName).CanCreateDirectories(true).PromptForSingleSelection()
	if err != nil || local == "" {
		return "", nil
	}
	return s.startDownload(connID, j, key, name, decrypt, passphrase, sudoPassword, local), nil
}

func (s *BackupService) startDownload(connID string, j Job, key, name string, decrypt bool, passphrase, pw, local string) string {
	cj := s.core.Jobs.Start(connID, "backup.download", j.Name+" · "+name, func(ctx context.Context, cj *core.Job) (err error) {
		defer func() { s.core.Audit(connID, "backup.download", key, local, err) }()
		sk, err := s.openForJob(ctx, &j, pw)
		if err != nil {
			return err
		}
		m, err := readManifest(ctx, sk, key, name)
		if err != nil {
			return err
		}
		cj.Step("Downloading %s → %s", sk.Describe(key), local)
		if err := s.download(ctx, cj, sk, key, m, passphrase, decrypt && m.Encrypted, local); err != nil {
			return err
		}
		if !decrypt || !m.Encrypted {
			cj.Logf("sha256 OK (%s)", m.ObjectSHA256)
		}
		cj.Logf("\x1b[32m✔ Saved %s\x1b[0m", local)
		cj.SetResult(local)
		if !strings.HasSuffix(local, filepath.Ext(name)) {
			cj.Logf("note: file extension differs from %s", name)
		}
		return nil
	})
	return cj.ID()
}

// Restore restores a backup onto the server as a job (PermRestore). The
// payload is verified against its manifest before anything is changed.
func (s *BackupService) Restore(connID string, opts RestoreOptions, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermRestore); err != nil {
		return "", err
	}
	j, err := s.getJob(connID, opts.JobID)
	if err != nil {
		return "", err
	}
	name, err := checkKey(&j, opts.Key)
	if err != nil {
		return "", err
	}
	if j.Type == TypeCustom && opts.Target == "command" {
		if err := s.core.Require(connID, core.PermExec); err != nil {
			return "", err
		}
	}
	// Early validation with the job's type; the manifest is checked again.
	pre := &Manifest{Type: j.Type, DumpFormat: dumpFormat(&j), Database: j.Database, Volume: j.Volume}
	o := opts
	if err := validateRestore(pre, &j, &o); err != nil {
		return "", err
	}
	if err := s.checkSudo(connID, restoreRoot(&j, pre, &o) || j.runsAsRoot(), sudoPassword); err != nil {
		return "", err
	}
	actor := s.core.Actor()
	cj := s.core.Jobs.Start(connID, "backup.restore", j.Name+" · "+name, func(ctx context.Context, cj *core.Job) (err error) {
		runID := uuid.NewString()
		d := RunData{JobName: j.Name, Type: j.Type, Trigger: "restore", CoreJob: cj.ID(), Destination: j.Destination, Key: opts.Key, Warnings: []string{}}
		s.insertRun(runID, kindRestore, &j, time.Now(), actor, d)
		cj.SetResult(runID)
		var m *Manifest
		defer func() {
			status := "ok"
			switch {
			case err != nil && isCancel(err, ctx):
				status, d.Error = "cancelled", apperr.New("backup.cancelled")
			case err != nil:
				status, d.Error = "failed", apperr.From(err)
			}
			if err != nil {
				cj.Logf("\x1b[31m✖ %s\x1b[0m", errText(err))
			}
			if m != nil {
				d.Size, d.SHA256, d.Encrypted, d.Compression = m.Size, m.SHA256, m.Encrypted, m.Compression
			}
			s.finishRun(runID, &j, status, d, cj.Info().Log)
			s.core.Audit(connID, "backup.restore", j.Name, restoreDetail(opts), err)
		}()
		m, err = s.restore(ctx, cj, j, opts, sudoPassword)
		return err
	})
	return cj.ID(), nil
}

func restoreDetail(o RestoreOptions) string {
	parts := []string{"backup=" + o.Key}
	if o.Target != "" {
		parts = append(parts, "target="+o.Target)
	}
	if o.Dir != "" {
		parts = append(parts, "dir="+o.Dir)
	}
	if o.Database != "" {
		parts = append(parts, "database="+o.Database)
	}
	if o.Volume != "" {
		parts = append(parts, "volume="+o.Volume)
	}
	return strings.Join(parts, " ")
}
