//go:build integration

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

// MinIO for the S3 tests (see the package docs of the test): the app talks
// to it directly at 127.0.0.1:9100.
const (
	minioEndpoint = "127.0.0.1:9100"
	minioUser     = "smtest"
	minioSecret   = "smtestsecret"
	minioBucket   = "smtest-backup"
)

const testPass = "smtest-passphrase-123"

type env struct {
	t  *testing.T
	c  *core.Core
	id string
	s  *BackupService
}

func setup(t *testing.T, port int) *env {
	c, id := testutil.Connect(t, port)
	return &env{t: t, c: c, id: id, s: New(c)}
}

// sh runs a command on the test server (sudo with the tester password).
func (e *env) sh(cmd string, sudo bool) string {
	e.t.Helper()
	conn, err := e.c.Conn(e.id)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := e.c.Run(ctx, conn, cmd, sudo, testutil.Password, "")
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	if res.ExitCode != 0 {
		e.t.Fatalf("%s: exit %d: %s %s", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res.Stdout
}

func (e *env) shQuiet(cmd string, sudo bool) {
	conn, err := e.c.Conn(e.id)
	if err != nil {
		return
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	_, _ = e.c.Run(ctx, conn, cmd, sudo, testutil.Password, "")
}

func (e *env) localDest() Destination {
	e.t.Helper()
	d, err := e.s.SaveDestination(Destination{Name: "local test", Type: DestLocal, Path: e.t.TempDir()}, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) saveJob(j Job, sec JobSecrets) Job {
	e.t.Helper()
	j, err := e.s.SaveJob(e.id, j, sec)
	if err != nil {
		e.t.Fatalf("save job %s: %v", j.Name, err)
	}
	return j
}

// runJob runs a backup synchronously and returns its run record.
func (e *env) runJob(j Job, wantOK bool) Run {
	e.t.Helper()
	cj, err := e.s.startRun(j, "manual", "test", "")
	if err != nil {
		e.t.Fatal(err)
	}
	info := cj.Wait()
	runs, err := e.s.History(e.id, j.ID, 1)
	if err != nil || len(runs) == 0 {
		e.t.Fatalf("history: %v %v", runs, err)
	}
	r := runs[0]
	if wantOK && (info.State != "done" || r.Status != "ok") {
		e.t.Fatalf("run %s: state %s status %s err %v\n%s", j.Name, info.State, r.Status, info.Error, info.Log)
	}
	if !wantOK && (info.State == "done" || r.Status == "ok") {
		e.t.Fatalf("run %s unexpectedly succeeded\n%s", j.Name, info.Log)
	}
	return r
}

func (e *env) restore(o RestoreOptions, wantOK bool) string {
	e.t.Helper()
	id, err := e.s.Restore(e.id, o, "")
	if err != nil {
		e.t.Fatal(err)
	}
	cj, _ := e.c.Jobs.Get(id)
	info := cj.Wait()
	if wantOK && info.State != "done" {
		e.t.Fatalf("restore: %s %v\n%s", info.State, info.Error, info.Log)
	}
	if !wantOK && info.State == "done" {
		e.t.Fatalf("restore unexpectedly succeeded\n%s", info.Log)
	}
	return info.Log
}

func (e *env) verify(j Job, key string) {
	e.t.Helper()
	id, err := e.s.Verify(e.id, j.ID, key, "", "")
	if err != nil {
		e.t.Fatal(err)
	}
	cj, _ := e.c.Jobs.Get(id)
	if info := cj.Wait(); info.State != "done" {
		e.t.Fatalf("verify: %v\n%s", info.Error, info.Log)
	}
}

func (e *env) backups(j Job) []BackupItem {
	e.t.Helper()
	list, err := e.s.ListBackups(e.id, j.ID, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

const srcDir = "/tmp/smtest-backup-src"
const restoreDir = "/tmp/smtest-backup-restore"

func (e *env) makeSource() string {
	e.sh(`rm -rf `+srcDir+` && mkdir -p `+srcDir+`/sub/deep && cd `+srcDir+` &&
printf 'hello\n' > a.txt && printf 'with space\n' > 'b file.txt' && head -c 300000 /dev/urandom > sub/blob.bin &&
printf 'x' > sub/deep/c && printf 'skip me' > sub/ignored.skip && ln -s a.txt link && chmod 640 a.txt`, false)
	return e.sh(`cd `+srcDir+` && find . -type f ! -name '*.skip' | sort | xargs -I{} sha256sum '{}'`, false)
}

func TestFilesEncryptedLocalRoundTrip(t *testing.T) {
	e := setup(t, testutil.FullPort())
	defer e.shQuiet("rm -rf "+srcDir+" "+restoreDir, true)
	want := e.makeSource()
	dest := e.localDest()

	j := e.saveJob(Job{Name: "Files encrypted", Type: TypeFiles, Enabled: true, Paths: []string{srcDir}, Excludes: []string{"*.skip"},
		Compression: "gzip", Encrypt: true, VerifyAfter: true, Destination: dest.ID, Schedule: Schedule{Mode: "manual"}},
		JobSecrets{Passphrase: testPass})
	if !j.HasPassphrase || j.Folder != "test/files-encrypted" {
		t.Fatalf("job: %+v", j)
	}
	r := e.runJob(j, true)
	if !r.Encrypted || r.Size == 0 || r.ObjectSize <= r.Size || len(r.SHA256) != 64 || !strings.HasSuffix(r.Key, ".tar.gz.age") {
		t.Fatalf("run: %+v", r)
	}
	// Stored as an age file with a manifest; no plaintext on the destination.
	local := filepath.Join(dest.Path, filepath.FromSlash(r.Key))
	data, err := os.ReadFile(local)
	if err != nil || !strings.HasPrefix(string(data), "age-encryption.org/v1") {
		t.Fatalf("stored object: %v %q", err, string(data[:min(40, len(data))]))
	}
	mb, err := os.ReadFile(local + manifestSuffix)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil || m.SHA256 != r.SHA256 || !m.Encrypted || m.Tools["tar"] == "" || m.JobID != j.ID {
		t.Fatalf("manifest: %s", mb)
	}
	if strings.Contains(string(mb), testPass) {
		t.Fatal("manifest leaks the passphrase")
	}
	list := e.backups(j)
	if len(list) != 1 || !list[0].Complete || list[0].Manifest == nil {
		t.Fatalf("list: %+v", list)
	}
	e.verify(j, r.Key)

	// Restore into a staging directory and compare.
	e.shQuiet("rm -rf "+restoreDir, true)
	e.restore(RestoreOptions{JobID: j.ID, Key: r.Key, Target: "staging", Dir: restoreDir}, true)
	got := e.sh(`cd `+restoreDir+srcDir+` && find . -type f | sort | xargs -I{} sha256sum '{}'`, true)
	if got != want {
		t.Fatalf("restored content differs:\nwant %s\ngot  %s", want, got)
	}
	if l := e.sh("readlink "+restoreDir+srcDir+"/link", true); strings.TrimSpace(l) != "a.txt" {
		t.Errorf("symlink not preserved: %q", l)
	}
	// The staged payload was removed from the server.
	if out := e.sh("ls /var/tmp | grep smrestore- || true", true); strings.TrimSpace(out) != "" {
		t.Errorf("temp files left: %s", out)
	}
	// Tampering with the stored object is detected before restoring.
	data[len(data)-10] ^= 0xff
	os.WriteFile(local, data, 0o600)
	e.shQuiet("rm -rf "+restoreDir, true)
	e.restore(RestoreOptions{JobID: j.ID, Key: r.Key, Target: "staging", Dir: restoreDir}, false)
	if out := e.sh("ls "+restoreDir+" 2>/dev/null || true", true); strings.TrimSpace(out) != "" {
		t.Errorf("tampered backup was applied: %s", out)
	}
	// Wrong passphrase override is rejected.
	id, _ := e.s.Verify(e.id, j.ID, r.Key, "not-the-passphrase", "")
	cj, _ := e.c.Jobs.Get(id)
	if info := cj.Wait(); info.State != "error" {
		t.Errorf("verify with wrong passphrase: %s", info.State)
	}
}

func TestFilesServerDestination(t *testing.T) {
	e := setup(t, testutil.FullPort())
	const destDir = "/tmp/smtest-backup-dest"
	defer e.shQuiet("rm -rf "+srcDir+" "+restoreDir+" "+destDir, true)
	want := e.makeSource()
	d, err := e.s.SaveDestination(Destination{Name: "on server", Type: DestServer, Path: destDir}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := e.s.TestDestination(e.id, d, ""); err != nil || res.NeedsRoot {
		t.Fatalf("test dest: %+v %v", res, err)
	}
	// Unencrypted: written directly on the server.
	plain := e.saveJob(Job{Name: "srv plain", Type: TypeFiles, Enabled: true, Paths: []string{srcDir}, Compression: "gzip", Destination: d.ID,
		Retention: Retention{KeepLast: 1}}, JobSecrets{})
	r := e.runJob(plain, true)
	sum := strings.Fields(e.sh("sha256sum "+destDir+"/"+r.Key, false))[0]
	if sum != r.SHA256 || r.Location != destDir+"/"+r.Key {
		t.Fatalf("direct sha: %s vs %+v", sum, r)
	}
	e.verify(plain, r.Key)
	e.restore(RestoreOptions{JobID: plain.ID, Key: r.Key, Target: "staging", Dir: restoreDir}, true)
	if got := e.sh(`cd `+restoreDir+srcDir+` && find . -type f ! -name '*.skip' | sort | xargs -I{} sha256sum '{}'`, true); got != want {
		t.Fatalf("restore from server dir differs")
	}
	// Encrypted: streamed through the app and back.
	enc := e.saveJob(Job{Name: "srv enc", Type: TypeFiles, Enabled: true, Paths: []string{srcDir}, Compression: "none", Encrypt: true, Destination: d.ID},
		JobSecrets{Passphrase: testPass})
	r2 := e.runJob(enc, true)
	head := e.sh("head -c 21 "+destDir+"/"+r2.Key, false)
	if head != "age-encryption.org/v1" {
		t.Fatalf("server copy not encrypted: %q", head)
	}
	e.verify(enc, r2.Key)
}

func TestPostgresDumpRestore(t *testing.T) {
	e := setup(t, testutil.FullPort())
	psql := func(db, sql string) string {
		return e.sh("cd / && runuser -u postgres -- psql -X -tA -d "+db+" -c "+core.Q(sql), true)
	}
	cleanup := func() {
		e.shQuiet("cd / && runuser -u postgres -- dropdb --if-exists smtest_backup; runuser -u postgres -- dropdb --if-exists smtest_backup_restored", true)
	}
	cleanup()
	defer cleanup()
	e.sh("cd / && runuser -u postgres -- createdb smtest_backup", true)
	psql("smtest_backup", "CREATE TABLE items(id int primary key, name text); INSERT INTO items SELECT g, 'item '||g FROM generate_series(1,500) g;")
	dest := e.localDest()
	j := e.saveJob(Job{Name: "pg", Type: TypePostgres, Enabled: true, Database: "smtest_backup", Destination: dest.ID, Compression: "gzip", Encrypt: true},
		JobSecrets{Passphrase: testPass})
	r := e.runJob(j, true)
	if !strings.HasSuffix(r.Key, ".dump.gz.age") {
		t.Fatalf("key %s", r.Key)
	}
	e.verify(j, r.Key)
	// Restore into a new database (created on the fly).
	e.restore(RestoreOptions{JobID: j.ID, Key: r.Key, Database: "smtest_backup_restored", CreateDB: true}, true)
	if n := strings.TrimSpace(psql("smtest_backup_restored", "SELECT count(*) FROM items")); n != "500" {
		t.Fatalf("restored rows: %s", n)
	}
	// Restore over the original (--clean) after changing it.
	psql("smtest_backup", "DELETE FROM items WHERE id > 10; INSERT INTO items VALUES (9999, 'new');")
	e.restore(RestoreOptions{JobID: j.ID, Key: r.Key}, true)
	if n := strings.TrimSpace(psql("smtest_backup", "SELECT count(*) FROM items WHERE id <= 500")); n != "500" {
		t.Fatalf("restored-in-place rows: %s", n)
	}
	if n := strings.TrimSpace(psql("smtest_backup", "SELECT count(*) FROM items WHERE id = 9999")); n != "0" {
		t.Fatalf("in-place restore kept newer rows: %s", n)
	}

	// A failing dump (nonexistent database) never produces a success.
	bad := e.saveJob(Job{Name: "pg missing", Type: TypePostgres, Enabled: true, Database: "smtest_nonexistent_db", Destination: dest.ID, Compression: "gzip"}, JobSecrets{})
	br := e.runJob(bad, false)
	if br.Status != "failed" || br.Error == nil || br.Error.Code != "backup.commandFailed" {
		t.Fatalf("failing dump: %+v", br)
	}
	if list := e.backups(bad); len(list) != 0 {
		t.Fatalf("failed run left backups: %+v", list)
	}
	entries, _ := os.ReadDir(filepath.Join(dest.Path, filepath.FromSlash(bad.Folder)))
	if len(entries) != 0 {
		t.Fatalf("failed run left files: %v", entries)
	}
	st, _ := e.s.Status(e.id)
	for _, s := range st {
		if s.JobID == bad.ID && (s.LastSuccess != 0 || s.LastFailure == 0 || s.LastStatus != "failed") {
			t.Fatalf("status: %+v", s)
		}
		if s.JobID == j.ID && s.LastSuccess == 0 {
			t.Fatalf("status: %+v", s)
		}
	}
	evs, _ := e.c.Events(core.EventQuery{Server: e.id, Kinds: []string{"backup"}})
	var sawOK, sawFail bool
	for _, ev := range evs {
		sawOK = sawOK || (ev.Code == "backup.ok" && ev.Params["name"] == "pg")
		sawFail = sawFail || (ev.Code == "backup.failed" && ev.Severity == "crit" && ev.Params["name"] == "pg missing")
	}
	if !sawOK || !sawFail {
		t.Fatalf("events: %+v", evs)
	}
}

func TestMySQLDumpRestore(t *testing.T) {
	e := setup(t, testutil.FullPort())
	my := func(sql string) string { return e.sh("mariadb -N -e "+core.Q(sql), true) }
	cleanup := func() {
		e.shQuiet("mariadb -e 'DROP DATABASE IF EXISTS smtest_backup; DROP DATABASE IF EXISTS smtest_backup_restored'", true)
	}
	cleanup()
	defer cleanup()
	my("CREATE DATABASE smtest_backup; CREATE TABLE smtest_backup.t(id INT PRIMARY KEY, v VARCHAR(20)); INSERT INTO smtest_backup.t VALUES (1,'a'),(2,'b'),(3,'c');")
	dest := e.localDest()
	// zstd is not installed on the test server: falls back to gzip with a warning.
	j := e.saveJob(Job{Name: "mysql", Type: TypeMySQL, Enabled: true, Database: "smtest_backup", Destination: dest.ID, Compression: "zstd"}, JobSecrets{})
	r := e.runJob(j, true)
	if !strings.HasSuffix(r.Key, ".sql.gz") || len(r.Warnings) == 0 {
		t.Fatalf("run: %+v", r)
	}
	e.restore(RestoreOptions{JobID: j.ID, Key: r.Key, Database: "smtest_backup_restored", CreateDB: true}, true)
	if n := strings.TrimSpace(my("SELECT count(*) FROM smtest_backup_restored.t")); n != "3" {
		t.Fatalf("rows: %s", n)
	}
	// All databases.
	all := e.saveJob(Job{Name: "mysql all", Type: TypeMySQL, Enabled: true, Destination: dest.ID, Compression: "gzip"}, JobSecrets{})
	ra := e.runJob(all, true)
	e.verify(all, ra.Key)
}

func TestRetentionAndRedis(t *testing.T) {
	e := setup(t, testutil.FullPort())
	dest := e.localDest()
	j := e.saveJob(Job{Name: "echo", Type: TypeCustom, Enabled: true, Command: "date; echo backup", Ext: "txt", Destination: dest.ID,
		Retention: Retention{KeepLast: 2}}, JobSecrets{})
	var last Run
	for i := 0; i < 4; i++ {
		last = e.runJob(j, true)
		time.Sleep(1100 * time.Millisecond) // object names have 1 s resolution
	}
	list := e.backups(j)
	if len(list) != 2 || list[0].Key != last.Key {
		t.Fatalf("retention kept %d: %+v", len(list), list)
	}
	if last.Deleted != 1 {
		t.Errorf("deleted in last run: %d", last.Deleted)
	}
	audit, _ := e.c.AuditList(core.AuditQuery{Action: "backup.retention.delete"})
	if len(audit) != 2 {
		t.Errorf("retention audit entries: %d", len(audit))
	}

	rj := e.saveJob(Job{Name: "redis", Type: TypeRedis, Enabled: true, Destination: dest.ID, Compression: "gzip"}, JobSecrets{})
	rr := e.runJob(rj, true)
	e.verify(rj, rr.Key) // checks the REDIS magic
	defer e.shQuiet("rm -rf "+restoreDir, true)
	e.restore(RestoreOptions{JobID: rj.ID, Key: rr.Key, Target: "staging", Dir: restoreDir}, true)
	if head := e.sh("head -c 5 "+restoreDir+"/dump.rdb", true); head != "REDIS" {
		t.Fatalf("redis staging restore: %q", head)
	}
}

func TestSchedulerTriggersDueJob(t *testing.T) {
	e := setup(t, testutil.FullPort())
	dest := e.localDest()
	j := e.saveJob(Job{Name: "scheduled", Type: TypeCustom, Enabled: true, Command: "echo scheduled", Destination: dest.ID,
		Schedule: Schedule{Mode: "cron", Cron: "* * * * *"}}, JobSecrets{})
	manual := e.saveJob(Job{Name: "manual only", Type: TypeCustom, Enabled: true, Command: "echo x", Destination: dest.ID}, JobSecrets{})
	sc := &scheduler{s: e.s, from: map[string]time.Time{}}
	now := time.Now()
	sc.reset(j.ID, now.Add(-2*time.Minute))
	sc.tick(now)
	// Wait for the run to finish.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		runs, _ := e.s.History(e.id, j.ID, 5)
		if len(runs) > 0 && runs[0].Status != "running" {
			if runs[0].Status != "ok" || runs[0].Actor != "scheduler" || (runs[0].Trigger != "schedule" && runs[0].Trigger != "missed") {
				t.Fatalf("scheduled run: %+v", runs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled job did not run")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if runs, _ := e.s.History(e.id, manual.ID, 5); len(runs) != 0 {
		t.Fatal("manual job was scheduled")
	}
	st, _ := e.s.Status(e.id)
	for _, s := range st {
		if s.JobID == j.ID && (s.NextRun == 0 || s.NextRun > time.Now().Add(61*time.Second).UnixMilli()) {
			t.Fatalf("next run: %+v", s)
		}
		if s.JobID == manual.ID && s.NextRun != 0 {
			t.Fatalf("manual next run: %+v", s)
		}
	}
	// No overlapping runs of the same job.
	block := e.saveJob(Job{Name: "slow", Type: TypeCustom, Enabled: true, Command: "sleep 3; echo x", Destination: dest.ID}, JobSecrets{})
	cj, err := e.s.startRun(block, "manual", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.startRun(block, "schedule", "scheduler", ""); !apperr.HasCode(err, "backup.alreadyRunning") {
		t.Fatalf("overlap allowed: %v", err)
	}
	cj.Wait()
}

func TestPermissionsAndValidation(t *testing.T) {
	e := setup(t, testutil.FullPort())
	dest := e.localDest()
	if _, err := e.s.SaveJob(e.id, Job{Name: "bad", Type: TypePostgres, Database: "x; rm -rf /", Destination: dest.ID}, JobSecrets{}); !apperr.HasCode(err, "backup.invalid") {
		t.Fatalf("invalid db accepted: %v", err)
	}
	if _, err := e.s.SaveJob(e.id, Job{Name: "enc", Type: TypeFiles, Paths: []string{"/etc"}, Encrypt: true, Destination: dest.ID}, JobSecrets{}); !apperr.HasCode(err, "backup.noPassphrase") {
		t.Fatalf("encryption without passphrase: %v", err)
	}
	if _, err := e.s.SaveJob(e.id, Job{Name: "nodest", Type: TypeFiles, Paths: []string{"/etc"}, Destination: "missing"}, JobSecrets{}); !apperr.HasCode(err, "backup.destNotFound") {
		t.Fatalf("missing destination: %v", err)
	}
	j := e.saveJob(Job{Name: "etc", Type: TypeFiles, Paths: []string{"/etc/hostname"}, Destination: dest.ID}, JobSecrets{})
	if _, err := e.s.SaveJob(e.id, Job{Name: "ETC", Type: TypeFiles, Paths: []string{"/etc"}, Destination: dest.ID}, JobSecrets{}); !apperr.HasCode(err, "backup.nameTaken") {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := e.s.DeleteDestination(dest.ID); !apperr.HasCode(err, "backup.destInUse") {
		t.Fatalf("delete used destination: %v", err)
	}
	if _, err := e.s.Restore(e.id, RestoreOptions{JobID: j.ID, Key: "../../etc/passwd"}, ""); !apperr.HasCode(err, "backup.invalidKey") {
		t.Fatalf("bad key: %v", err)
	}
	testutil.SetRole(t, e.c, e.id, "developer")
	if _, err := e.s.RunNow(e.id, j.ID, ""); !apperr.HasCode(err, "access.denied") {
		t.Fatalf("developer ran a backup: %v", err)
	}
	testutil.SetRole(t, e.c, e.id, "viewer")
	if _, err := e.s.Restore(e.id, RestoreOptions{JobID: j.ID, Key: j.Folder + "/20260101-000000.tar"}, ""); !apperr.HasCode(err, "access.denied") {
		t.Fatalf("viewer restored: %v", err)
	}
	if err := e.s.DeleteJob(e.id, j.ID); !apperr.HasCode(err, "access.denied") {
		t.Fatalf("viewer deleted a job: %v", err)
	}
	if _, err := e.s.SaveJob(e.id, j, JobSecrets{}); !apperr.HasCode(err, "access.denied") {
		t.Fatalf("viewer edited a job: %v", err)
	}
	testutil.SetRole(t, e.c, e.id, "developer")
	testutil.SetRole(t, e.c, e.id, "operator")
	// Operators may not add custom commands? They have exec, so they may.
	if _, err := e.s.SaveJob(e.id, Job{Name: "cmd", Type: TypeCustom, Command: "echo", Destination: dest.ID}, JobSecrets{}); err != nil {
		t.Fatalf("operator custom job: %v", err)
	}
}

func TestDockerVolume(t *testing.T) {
	e := setup(t, testutil.DindPort())
	const vol, vol2 = "smtest-backup-vol", "smtest-backup-vol2"
	cleanup := func() { e.shQuiet("docker volume rm -f "+vol+" "+vol2, true) }
	cleanup()
	defer cleanup()
	e.sh("docker volume create "+vol+" >/dev/null", false)
	e.sh(`mp=$(docker volume inspect -f '{{.Mountpoint}}' `+vol+`) && mkdir -p "$mp/d" && printf 'volume data\n' > "$mp/d/f.txt" && head -c 50000 /dev/urandom > "$mp/blob"`, true)
	want := e.sh(`cd "$(docker volume inspect -f '{{.Mountpoint}}' `+vol+`)" && find . -type f | sort | xargs sha256sum`, true)
	dest := e.localDest()
	j := e.saveJob(Job{Name: "vol", Type: TypeVolume, Enabled: true, Volume: vol, StopContainers: true, Destination: dest.ID, Compression: "gzip", Encrypt: true},
		JobSecrets{Passphrase: testPass})
	r := e.runJob(j, true)
	e.verify(j, r.Key)
	e.restore(RestoreOptions{JobID: j.ID, Key: r.Key, Volume: vol2}, true)
	got := e.sh(`cd "$(docker volume inspect -f '{{.Mountpoint}}' `+vol2+`)" && find . -type f | sort | xargs sha256sum`, true)
	if got != want {
		t.Fatalf("volume restore differs:\n%s\n%s", want, got)
	}
	// BusyBox pipeline failure detection: a missing volume fails.
	bad := e.saveJob(Job{Name: "vol missing", Type: TypeVolume, Enabled: true, Volume: "smtest-backup-nope", Destination: dest.ID, Compression: "gzip"}, JobSecrets{})
	e.runJob(bad, false)
	if list := e.backups(bad); len(list) != 0 {
		t.Fatalf("failed volume backup left objects: %+v", list)
	}
}

func TestS3MinIO(t *testing.T) {
	cl, err := minio.New(minioEndpoint, &minio.Options{Creds: credentials.NewStaticV4(minioUser, minioSecret, "")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if ok, err := cl.BucketExists(ctx, minioBucket); err != nil {
		t.Skipf("MinIO not reachable at %s: %v", minioEndpoint, err)
	} else if !ok {
		if err := cl.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e := setup(t, testutil.FullPort())
	defer e.shQuiet("rm -rf "+srcDir+" "+restoreDir, true)
	want := e.makeSource()
	prefix := "smtest/" + time.Now().Format("150405")
	defer func() {
		for o := range cl.ListObjects(context.Background(), minioBucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			_ = cl.RemoveObject(context.Background(), minioBucket, o.Key, minio.RemoveObjectOptions{})
		}
	}()

	// Wrong secret and missing bucket are reported clearly.
	bad := Destination{Name: "minio", Type: DestS3, Endpoint: "http://" + minioEndpoint, Bucket: minioBucket, Prefix: prefix, AccessKey: minioUser, PathStyle: true}
	if _, err := e.s.TestDestination(e.id, bad, "wrong-secret"); !apperr.HasCode(err, "backup.s3.auth") {
		t.Fatalf("bad secret: %v", err)
	}
	nob := bad
	nob.Bucket = "smtest-no-such-bucket"
	if _, err := e.s.TestDestination(e.id, nob, minioSecret); !apperr.HasCode(err, "backup.s3.noBucket") {
		t.Fatalf("missing bucket: %v", err)
	}
	d, err := e.s.SaveDestination(bad, minioSecret)
	if err != nil || !d.HasSecret || d.UseTLS {
		t.Fatalf("save: %+v %v", d, err)
	}
	if _, err := e.s.TestDestination(e.id, d, ""); err != nil {
		t.Fatalf("test with stored secret: %v", err)
	}
	dl, _ := e.s.ListDestinations()
	for _, x := range dl {
		raw, _ := json.Marshal(x)
		if strings.Contains(string(raw), minioSecret) {
			t.Fatal("secret exposed to the UI")
		}
	}

	j := e.saveJob(Job{Name: "s3 files", Type: TypeFiles, Enabled: true, Paths: []string{srcDir}, Excludes: []string{"*.skip"}, Compression: "gzip", Encrypt: true,
		VerifyAfter: true, Destination: d.ID, Retention: Retention{KeepLast: 1}}, JobSecrets{Passphrase: testPass})
	r1 := e.runJob(j, true)
	time.Sleep(1100 * time.Millisecond)
	r2 := e.runJob(j, true)
	if r2.Deleted != 1 {
		t.Errorf("retention deleted %d", r2.Deleted)
	}
	list := e.backups(j)
	if len(list) != 1 || list[0].Key != r2.Key || list[0].Manifest == nil || list[0].Manifest.SHA256 != r2.SHA256 {
		t.Fatalf("list after retention: %+v", list)
	}
	if _, err := cl.StatObject(ctx, minioBucket, prefix+"/"+r1.Key, minio.StatObjectOptions{}); err == nil {
		t.Fatal("old backup still in the bucket")
	}
	obj, err := cl.GetObject(ctx, minioBucket, prefix+"/"+r2.Key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 21)
	if _, err := obj.Read(head); err != nil && string(head) != "age-encryption.org/v1" {
		t.Fatal(err)
	}
	obj.Close()
	if string(head) != "age-encryption.org/v1" {
		t.Fatalf("object not encrypted: %q", head)
	}
	e.restore(RestoreOptions{JobID: j.ID, Key: r2.Key, Target: "staging", Dir: restoreDir}, true)
	if got := e.sh(`cd `+restoreDir+srcDir+` && find . -type f | sort | xargs -I{} sha256sum '{}'`, true); got != want {
		t.Fatalf("S3 restore differs")
	}
	// Failed dump to S3 leaves nothing behind (multipart upload aborted).
	fj := e.saveJob(Job{Name: "s3 fail", Type: TypeCustom, Enabled: true, Command: "echo partial; exit 4", Destination: d.ID}, JobSecrets{})
	e.runJob(fj, false)
	if l := e.backups(fj); len(l) != 0 {
		t.Fatalf("failed run left objects: %+v", l)
	}
	// Download a decrypted copy.
	local := filepath.Join(t.TempDir(), "copy.tar.gz")
	id := e.s.startDownload(e.id, j, r2.Key, list[0].Name, true, testPass, "", local)
	cj, _ := e.c.Jobs.Get(id)
	if info := cj.Wait(); info.State != "done" {
		t.Fatalf("download: %v\n%s", info.Error, info.Log)
	}
	if b, err := os.ReadFile(local); err != nil || len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("downloaded file is not gzip: %v", err)
	}
}
