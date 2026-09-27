package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"server-manager/internal/apperr"
)

func init() { scryptWorkFactor = 10 } // fast tests

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Sao lưu DB":             "sao-luu-db",
		"  Web 01 / Production ": "web-01-production",
		"Đà Nẵng":                "da-nang",
		"***":                    "x",
		"UPPER_case.name":        "upper-case-name",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := slug(strings.Repeat("abc", 40)); len(got) > 48 {
		t.Errorf("slug too long: %d", len(got))
	}
}

func TestObjectNaming(t *testing.T) {
	ts := time.Date(2026, 9, 25, 14, 3, 7, 0, time.UTC)
	cases := []struct {
		ext, comp string
		enc       bool
		want      string
	}{
		{"tar", "gzip", true, "20260925-140307.tar.gz.age"},
		{"dump", "none", false, "20260925-140307.dump"},
		{"sql", "zstd", false, "20260925-140307.sql.zst"},
		{"rdb", "none", true, "20260925-140307.rdb.age"},
	}
	for _, c := range cases {
		got := objectName(ts, c.ext, c.comp, c.enc)
		if got != c.want {
			t.Errorf("objectName = %q, want %q", got, c.want)
		}
		back, ok := parseObjectName(got)
		if !ok || !back.Equal(ts) {
			t.Errorf("parseObjectName(%q) = %v %v", got, back, ok)
		}
	}
	for _, bad := range []string{"x.tar", "20260925-140307.tar.part", "20260925-140307.tar.gz.age.manifest.json", "../20260925-140307.tar", "20261325-140307.tar"} {
		if _, ok := parseObjectName(bad); ok {
			t.Errorf("parseObjectName(%q) accepted", bad)
		}
	}
	j := &Job{Folder: "web-01/nightly"}
	if _, err := checkKey(j, "web-01/nightly/20260925-140307.tar.gz"); err != nil {
		t.Errorf("checkKey valid: %v", err)
	}
	for _, bad := range []string{"web-01/other/20260925-140307.tar", "web-01/nightly/../x/20260925-140307.tar", "/web-01/nightly/20260925-140307.tar", "web-01/nightly/evil"} {
		if _, err := checkKey(j, bad); err == nil {
			t.Errorf("checkKey(%q) accepted", bad)
		}
	}
	if dumpExt(&Job{Type: TypePostgres, Database: "app"}) != "dump" || dumpExt(&Job{Type: TypePostgres}) != "sql" {
		t.Error("postgres ext")
	}
}

func TestUniqueFolder(t *testing.T) {
	all := []Job{{ID: "a", Folder: "srv/db"}}
	if got := uniqueFolder(all, "srv/web", "b"); got != "srv/web" {
		t.Errorf("got %q", got)
	}
	got := uniqueFolder(all, "srv/db", "0123456789")
	if got != "srv/db-012345" || !validFolder(got) {
		t.Errorf("got %q", got)
	}
}

func mkSets(now time.Time, ages ...time.Duration) []backupSet {
	var list []objInfo
	for _, a := range ages {
		n := objectName(now.Add(-a), "tar", "gzip", false)
		list = append(list, objInfo{Name: n}, objInfo{Name: n + manifestSuffix})
	}
	return groupObjects(list)
}

func names(sets []backupSet) []string {
	out := []string{}
	for _, s := range sets {
		out = append(out, s.Name)
	}
	return out
}

func TestRetentionSelection(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	sets := mkSets(now, 0, day, 2*day, 3*day, 10*day, 40*day)
	if len(sets) != 6 || !sets[0].Time.Equal(now) {
		t.Fatalf("grouping/sorting: %v", names(sets))
	}

	if del := selectForDeletion(sets, Retention{}, now); len(del) != 0 {
		t.Errorf("no policy deleted %v", names(del))
	}
	del := selectForDeletion(sets, Retention{KeepLast: 3}, now)
	if len(del) != 3 || del[0].Name != sets[3].Name {
		t.Errorf("keepLast 3: %v", names(del))
	}
	del = selectForDeletion(sets, Retention{MaxAgeDays: 7}, now)
	if len(del) != 2 {
		t.Errorf("maxAge 7: %v", names(del))
	}
	// Both are limits.
	del = selectForDeletion(sets, Retention{KeepLast: 5, MaxAgeDays: 2}, now)
	if len(del) != 3 { // 3d, 10d, 40d
		t.Errorf("keep 5 / 2 days: %v", names(del))
	}
	// Never the newest successful backup, even if everything is too old.
	old := mkSets(now, 50*day, 60*day)
	del = selectForDeletion(old, Retention{MaxAgeDays: 1}, now)
	if len(del) != 1 || del[0].Name != old[1].Name {
		t.Errorf("newest must be kept: %v", names(del))
	}
	del = selectForDeletion(old, Retention{KeepLast: 1}, now)
	if len(del) != 1 || del[0].Name != old[1].Name {
		t.Errorf("keepLast 1: %v", names(del))
	}
	// Incomplete sets don't count as the newest success; old ones are cleaned.
	list := []objInfo{
		{Name: objectName(now, "tar", "none", false)},                              // newest, no manifest (in progress / failed)
		{Name: objectName(now.Add(-2*day), "tar", "none", false)},                  // incomplete, old
		{Name: objectName(now.Add(-3*day), "tar", "none", false)},                  // complete
		{Name: objectName(now.Add(-3*day), "tar", "none", false) + manifestSuffix}, //
		{Name: objectName(now.Add(-4*day), "tar", "none", false) + manifestSuffix}, // orphan manifest
		{Name: "unrelated.txt"}, {Name: objectName(now, "tar", "none", false) + ".part"}, // ignored
	}
	sets = groupObjects(list)
	del = selectForDeletion(sets, Retention{KeepLast: 1}, now)
	got := strings.Join(names(del), ",")
	want := objectName(now.Add(-2*day), "tar", "none", false) + "," + objectName(now.Add(-4*day), "tar", "none", false)
	if got != want {
		t.Errorf("incomplete handling: got %s want %s", got, want)
	}
}

func TestCron(t *testing.T) {
	cases := []struct {
		s    Schedule
		want string
		bad  bool
	}{
		{Schedule{Mode: "manual"}, "", false},
		{Schedule{Mode: "hourly", Minute: 15}, "15 * * * *", false},
		{Schedule{Mode: "daily", Time: "02:30"}, "30 2 * * *", false},
		{Schedule{Mode: "weekly", Time: "23:05", Weekday: 0}, "5 23 * * 0", false},
		{Schedule{Mode: "cron", Cron: "*/10 1-5 * * 1-5"}, "*/10 1-5 * * 1-5", false},
		{Schedule{Mode: "cron", Cron: "@daily"}, "@daily", false},
		{Schedule{Mode: "hourly", Minute: 60}, "", true},
		{Schedule{Mode: "daily", Time: "24:00"}, "", true},
		{Schedule{Mode: "weekly", Time: "10:00", Weekday: 7}, "", true},
		{Schedule{Mode: "cron", Cron: "61 * * * *"}, "", true},
		{Schedule{Mode: "cron", Cron: "0 0 30 2 *"}, "", true}, // never fires
		{Schedule{Mode: "cron", Cron: "* * * * *\nrm -rf /"}, "", true},
		{Schedule{Mode: "sometimes"}, "", true},
	}
	for _, c := range cases {
		got, err := cronOf(c.s)
		if c.bad {
			if err == nil {
				t.Errorf("cronOf(%+v) accepted: %q", c.s, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("cronOf(%+v) = %q, %v; want %q", c.s, got, err, c.want)
		}
	}
	from := time.Date(2026, 9, 25, 1, 0, 0, 0, time.Local)
	next, err := nextRuns("30 2 * * *", from, 3)
	if err != nil || len(next) != 3 {
		t.Fatal(next, err)
	}
	for i, n := range next {
		want := time.Date(2026, 9, 25+i, 2, 30, 0, 0, time.Local)
		if !n.Equal(want) {
			t.Errorf("next[%d] = %v, want %v", i, n, want)
		}
	}
}

func TestSchedulerDue(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 30, 0, time.Local)
	sc := &scheduler{from: map[string]time.Time{}}
	daily := Job{ID: "d", Enabled: true, Schedule: Schedule{Cron: "0 9 * * *"}, Updated: now.Add(-72 * time.Hour).UnixMilli()}
	off := Job{ID: "o", Enabled: false, Schedule: Schedule{Cron: "* * * * *"}}
	manual := Job{ID: "m", Enabled: true}

	// Fresh start, 09:00 was missed an hour ago (< 24 h): fires once, as "missed".
	d := sc.due([]Job{daily, off, manual}, now)
	if len(d) != 1 || d[0].job.ID != "d" || d[0].trigger != "missed" {
		t.Fatalf("missed: %+v", d)
	}
	if d = sc.due([]Job{daily}, now.Add(30*time.Second)); len(d) != 0 {
		t.Fatalf("fired twice: %+v", d)
	}
	// Next day at 09:00:10 it fires on schedule.
	if d = sc.due([]Job{daily}, time.Date(2026, 9, 26, 9, 0, 10, 0, time.Local)); len(d) != 1 || d[0].trigger != "schedule" {
		t.Fatalf("on time: %+v", d)
	}

	// Missed by more than 24 h: no catch-up run.
	sc2 := &scheduler{from: map[string]time.Time{}, lastAlive: now.Add(-72 * time.Hour)}
	weekly := Job{ID: "w", Enabled: true, Schedule: Schedule{Cron: "0 9 * * *"}, Updated: now.Add(-100 * time.Hour).UnixMilli()}
	late := time.Date(2026, 9, 25, 9, 30, 0, 0, time.Local).Add(24 * time.Hour).Add(-time.Minute) // 09:29 next day… still < 24h since 09:00 today
	_ = late
	d = sc2.due([]Job{weekly}, time.Date(2026, 9, 26, 8, 59, 0, 0, time.Local))
	// 09:00 on the 25th was 23h59m ago → catch-up.
	if len(d) != 1 {
		t.Fatalf("23h59m: %+v", d)
	}
	sc3 := &scheduler{from: map[string]time.Time{}, lastAlive: now.Add(-72 * time.Hour)}
	d = sc3.due([]Job{{ID: "x", Enabled: true, Schedule: Schedule{Cron: "0 9 * * 1"}, Updated: 0}},
		time.Date(2026, 9, 25, 10, 0, 0, 0, time.Local)) // Friday; Monday 09:00 was 4 days ago
	if len(d) != 0 {
		t.Fatalf("stale catch-up fired: %+v", d)
	}
	// A job saved just now doesn't fire for past activations.
	sc4 := &scheduler{from: map[string]time.Time{}}
	sc4.reset("d", now)
	if d = sc4.due([]Job{daily}, now.Add(time.Second)); len(d) != 0 {
		t.Fatalf("reset job fired: %+v", d)
	}
}

func TestAgeRoundTrip(t *testing.T) {
	plain := make([]byte, 3<<20+123)
	rand.Read(plain)
	var enc bytes.Buffer
	w, err := encryptWriter(&enc, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader(plain)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc.Bytes()[:4096], plain[:64]) {
		t.Fatal("ciphertext contains plaintext")
	}
	r, err := decryptReader(bytes.NewReader(enc.Bytes()), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip mismatch (%v)", err)
	}
	if _, err := decryptReader(bytes.NewReader(enc.Bytes()), "wrong"); !apperr.HasCode(err, "backup.wrongPassphrase") {
		t.Fatalf("wrong passphrase: %v", err)
	}
	// Tampering is detected.
	bad := append([]byte(nil), enc.Bytes()...)
	bad[len(bad)-100] ^= 1
	r, err = decryptReader(bytes.NewReader(bad), "correct horse battery")
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

// runSh runs a script with the local /bin/sh (and dash when installed).
func runSh(t *testing.T, sh, script string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(sh, "-c", script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func shells() []string {
	out := []string{"/bin/sh"}
	for _, s := range []string{"dash", "busybox"} {
		if p, err := exec.LookPath(s); err == nil {
			if s == "busybox" {
				continue // needs "busybox sh"; covered by the Alpine integration server
			}
			out = append(out, p)
		}
	}
	return out
}

func TestPipelineFailureDetection(t *testing.T) {
	for _, sh := range shells() {
		// All stages succeed.
		out, _, code := runSh(t, sh, pipelineScript("", []stage{{"dump", "printf hello"}, {"compress", "tr a-z A-Z"}}, "", "", ""))
		if code != 0 || out != "HELLO" {
			t.Errorf("%s ok: %q %d", sh, out, code)
		}
		// The producer fails but the compressor succeeds: must fail.
		_, errOut, code := runSh(t, sh, pipelineScript("", []stage{{"dump", "echo partial; exit 3"}, {"compress", "cat"}}, "", "", ""))
		if code == 0 || !strings.Contains(errOut, "stage 'dump' failed (exit 3)") {
			t.Errorf("%s failing producer: code %d, stderr %q", sh, code, errOut)
		}
		// The compressor fails.
		_, _, code = runSh(t, sh, pipelineScript("", []stage{{"dump", "echo x"}, {"compress", "cat >/dev/null; exit 2"}}, "", "", ""))
		if code == 0 {
			t.Errorf("%s failing compressor passed", sh)
		}
		// A producer killed by a signal never reports a status.
		_, errOut, code = runSh(t, sh, pipelineScript("", []stage{{"dump", "kill -9 $$"}, {"compress", "cat"}}, "", "", ""))
		if code == 0 {
			t.Errorf("%s killed producer passed: %q", sh, errOut)
		}
		// Setup failure aborts.
		_, _, code = runSh(t, sh, pipelineScript("exit 5", []stage{{"dump", "echo x"}}, "", "", ""))
		if code != 5 {
			t.Errorf("%s pre exit: %d", sh, code)
		}
		// Direct mode: nothing left behind on failure, file + marker on success.
		dir := t.TempDir()
		dst := filepath.Join(dir, "sub", "out.tar")
		j := &Job{Type: TypeCustom, Command: "echo data; exit 1"}
		_, _, code = runSh(t, sh, directScript(j, "none", dst))
		if code == 0 {
			t.Errorf("%s direct failing produced success", sh)
		}
		if ents, _ := os.ReadDir(filepath.Join(dir, "sub")); len(ents) != 0 {
			t.Errorf("%s direct failure left files: %v", sh, ents)
		}
		j.Command = "true"
		if _, _, code = runSh(t, sh, directScript(j, "none", dst)); code == 0 {
			t.Errorf("%s empty output accepted", sh)
		}
		if _, err := exec.LookPath("sha256sum"); err == nil {
			j.Command = "printf 'hello\\n'"
			out, errOut, code = runSh(t, sh, directScript(j, "gzip", dst))
			sum, size, ok := parseDirect(out)
			if code != 0 || !ok || size == 0 || len(sum) != 64 {
				t.Errorf("%s direct ok: %q %q %d", sh, out, errOut, code)
			}
			if st, err := os.Stat(dst); err != nil || st.Size() != size {
				t.Errorf("%s direct file: %v", sh, err)
			}
		}
	}
}

func TestLocalSink(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	sk := &localSink{base: base}
	w, err := sk.Create(ctx, "srv/job/20260101-000000.tar")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("abc"))
	w.Abort()
	if list, _ := sk.List(ctx, "srv/job"); len(list) != 0 {
		t.Fatalf("aborted write visible: %+v", list)
	}
	w, _ = sk.Create(ctx, "srv/job/20260101-000000.tar")
	w.Write([]byte("abc"))
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := sk.PutSmall(ctx, "srv/job/20260101-000000.tar"+manifestSuffix, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	list, _ := sk.List(ctx, "srv/job")
	sets := groupObjects(list)
	if len(sets) != 1 || !sets[0].complete() || sets[0].Size != 3 {
		t.Fatalf("list: %+v", sets)
	}
	if _, err := sk.path("../escape"); err == nil {
		t.Fatal("path escape accepted")
	}
	if list, err := sk.List(ctx, "missing/dir"); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
}

func TestNormalizeValidation(t *testing.T) {
	good := Job{Name: "db", Type: TypePostgres, Database: "app", Destination: "abc-123", Schedule: Schedule{Mode: "daily", Time: "3:00"}}
	if err := normalizeJob(&good); err != nil {
		t.Fatal(err)
	}
	if good.Schedule.Cron != "0 3 * * *" || good.AuthMode != "peer" || good.TimeoutMin != 120 || good.Compression != "none" {
		t.Errorf("normalized: %+v", good)
	}
	bad := []Job{
		{Name: "", Type: TypeFiles, Paths: []string{"/etc"}, Destination: "a"},
		{Name: "x", Type: TypeFiles, Paths: []string{"etc"}, Destination: "a"},
		{Name: "x", Type: TypeFiles, Paths: []string{"/etc\n/x"}, Destination: "a"},
		{Name: "x", Type: TypePostgres, Database: "a'; drop", Destination: "a"},
		{Name: "x", Type: TypeMySQL, AuthMode: "password", DBUser: "-u root", Destination: "a"},
		{Name: "x", Type: TypeVolume, Volume: "../etc", Destination: "a"},
		{Name: "x", Type: TypeCustom, Command: "", Destination: "a"},
		{Name: "x", Type: TypeCustom, Command: "true", Ext: "tar/../x", Destination: "a"},
		{Name: "x", Type: TypeFiles, Paths: []string{"/etc"}, Destination: ""},
		{Name: "x", Type: TypeFiles, Paths: []string{"/etc"}, Destination: "a", Compression: "xz"},
		{Name: "x", Type: "ftp", Destination: "a"},
	}
	for i, j := range bad {
		if err := normalizeJob(&j); err == nil {
			t.Errorf("bad job %d accepted", i)
		}
	}
	dests := []Destination{
		{Name: "s3", Type: DestS3, Endpoint: "https://s3.example.com/", Bucket: "my-bucket", AccessKey: "AK"},
		{Name: "minio", Type: DestS3, Endpoint: "127.0.0.1:9100", Bucket: "bk1", AccessKey: "AK", Prefix: "/a/b/"},
		{Name: "srv", Type: DestServer, Path: "/var/backups/sm/"},
	}
	for _, d := range dests {
		if err := normalizeDest(&d); err != nil {
			t.Errorf("dest %s: %v", d.Name, err)
		}
		if d.Type == DestS3 && strings.Contains(d.Endpoint, "/") {
			t.Errorf("endpoint not normalized: %q", d.Endpoint)
		}
		if d.Name == "minio" && d.Prefix != "a/b" {
			t.Errorf("prefix: %q", d.Prefix)
		}
	}
	badDests := []Destination{
		{Name: "a", Type: DestS3, Endpoint: "exa mple.com", Bucket: "bk1", AccessKey: "AK"},
		{Name: "a", Type: DestS3, Endpoint: "example.com", Bucket: "B", AccessKey: "AK"},
		{Name: "a", Type: DestS3, Endpoint: "example.com", Bucket: "bk1", AccessKey: "AK", Prefix: "a/../b"},
		{Name: "a", Type: DestServer, Path: "relative"},
		{Name: "a", Type: DestServer, Path: "/"},
		{Name: "a", Type: DestLocal, Path: "relative/dir"},
	}
	for i, d := range badDests {
		if err := normalizeDest(&d); err == nil {
			t.Errorf("bad dest %d accepted", i)
		}
	}
}

func TestScriptsQuoteInput(t *testing.T) {
	j := &Job{Type: TypeFiles, Paths: []string{"/srv/it's here"}, Excludes: []string{"/srv/$(reboot)", "*.log"}}
	_, cmd := producer(j)
	if !strings.Contains(cmd, `'./srv/it'\''s here'`) || !strings.Contains(cmd, `--exclude='./srv/$(reboot)'`) {
		t.Errorf("files producer: %s", cmd)
	}
	pg := &Job{Type: TypePostgres, Database: "app", AuthMode: "password", DBUser: "u", DBPort: 5433}
	pre, cmd := producer(pg)
	if !strings.Contains(pre, "read -r PGPASSWORD") || !strings.Contains(cmd, "pg_dump -Fc -w -h '127.0.0.1' -p 5433 -U 'u' 'app'") {
		t.Errorf("pg producer: %s / %s", pre, cmd)
	}
	my := &Job{Type: TypeMySQL, AuthMode: "peer"}
	_, cmd = producer(my)
	if !strings.Contains(cmd, "--all-databases") || !strings.Contains(cmd, "--single-transaction") {
		t.Errorf("mysql producer: %s", cmd)
	}
}
