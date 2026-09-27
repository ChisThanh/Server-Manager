//go:build integration

package logs

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

// events captures "logs:lines" events (the emit hook is process-wide, so
// tests using it don't run in parallel).
type events struct {
	mu   sync.Mutex
	list []LinesEvent
}

func captureEvents(t *testing.T) *events {
	ev := &events{}
	core.SetEmitHook(func(name string, data any) {
		if name != EventLines {
			return
		}
		ev.mu.Lock()
		ev.list = append(ev.list, data.(LinesEvent))
		ev.mu.Unlock()
	})
	t.Cleanup(func() { core.SetEmitHook(nil) })
	return ev
}

// wait polls until cond holds for the events of stream id.
func (e *events) wait(t *testing.T, id string, d time.Duration, cond func(lines []LogLine, done *LinesEvent) bool) ([]LogLine, *LinesEvent) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		e.mu.Lock()
		var lines []LogLine
		var done *LinesEvent
		for i, ev := range e.list {
			if ev.StreamID != id {
				continue
			}
			lines = append(lines, ev.Lines...)
			if ev.Done {
				done = &e.list[i]
			}
		}
		e.mu.Unlock()
		if cond(lines, done) {
			return lines, done
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for stream %s (%d lines, done=%v)", id, len(lines), done != nil)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func contains(lines []LogLine, s string) bool {
	for _, l := range lines {
		if strings.Contains(l.Raw, s) || strings.Contains(l.Message, s) {
			return true
		}
	}
	return false
}

func tag() string { return fmt.Sprintf("smtest-logs-%06x", rand.Intn(1<<24)) }

func code(err error) string {
	if err == nil {
		return ""
	}
	return apperr.From(err).Code
}

func run(t *testing.T, c *core.Core, id, cmd string, sudo bool) string {
	t.Helper()
	conn, err := c.Conn(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := c.RunOK(ctx, conn, cmd, sudo, "", "")
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return res.Stdout
}

func TestSourcesFull(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	sl, err := s.Sources(id)
	if err != nil {
		t.Fatal(err)
	}
	if !sl.Journal || !sl.Systemd || !sl.JournalSudo {
		t.Fatalf("journal=%v systemd=%v journalSudo=%v", sl.Journal, sl.Systemd, sl.JournalSudo)
	}
	find := func(kind, target string) *Source {
		for i := range sl.Sources {
			if sl.Sources[i].Kind == kind && sl.Sources[i].Target == target {
				return &sl.Sources[i]
			}
		}
		return nil
	}
	for _, k := range []string{"journal", "kernel", "auth"} {
		if find(k, "") == nil {
			t.Errorf("missing %s source", k)
		}
	}
	if u := find("unit", "nginx.service"); u == nil || u.State != "active" {
		t.Errorf("nginx unit: %+v", u)
	}
	sys := find("file", "/var/log/syslog")
	if sys == nil || !sys.NeedsSudo || !sys.Common || sys.Size <= 0 || sys.MTime <= 0 {
		t.Errorf("syslog: %+v", sys)
	}
	if f := find("file", "/var/log/wtmp"); f != nil {
		t.Error("binary wtmp listed")
	}
	if f := find("file", "/var/log/dpkg.log"); f == nil || f.NeedsSudo {
		t.Errorf("dpkg.log: %+v", f)
	}
}

func TestJournalQuery(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	tg := tag()
	run(t, c, id, "logger -t smtest-logs -p user.warning "+core.Q(tg+" Hello Warning")+"; logger -t smtest-logs -p user.info "+core.Q(tg+" plain info"), false)
	time.Sleep(500 * time.Millisecond)

	r, err := s.Query(id, QuerySpec{Kind: "journal", Lines: 30}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Sudo || len(r.Lines) == 0 || len(r.Lines) > 30 {
		t.Fatalf("sudo=%v lines=%d", r.Sudo, len(r.Lines))
	}
	for i := 1; i < len(r.Lines); i++ {
		if r.Lines[i].TS < r.Lines[i-1].TS {
			t.Fatal("lines not in chronological order")
		}
	}

	// Fixed-string search, case-insensitive.
	r, err = s.Query(id, QuerySpec{Kind: "journal", Lines: 50, Search: strings.ToUpper(tg)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lines) != 2 || !contains(r.Lines, tg+" Hello Warning") {
		t.Fatalf("search: %+v", r.Lines)
	}
	if r.Lines[1].Level != "info" || r.Lines[0].Level != "warning" || r.Lines[0].Source != "smtest-logs" {
		t.Fatalf("levels/source: %+v", r.Lines)
	}
	// Case-sensitive search misses.
	r, err = s.Query(id, QuerySpec{Kind: "journal", Search: strings.ToUpper(tg), CaseSensitive: true}, "")
	if err != nil || len(r.Lines) != 0 {
		t.Fatalf("case-sensitive: %v %+v", err, r.Lines)
	}
	// Regex + level filter: only the warning.
	r, err = s.Query(id, QuerySpec{Kind: "journal", Search: tg + ` (hello|plain) \w+`, Regex: true, Level: "warning"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lines) != 1 || r.Lines[0].Level != "warning" {
		t.Fatalf("regex+level: %+v", r.Lines)
	}
	// Time range excluding the messages.
	past := time.Now().Add(-48 * time.Hour).Unix()
	r, err = s.Query(id, QuerySpec{Kind: "journal", Search: tg, Since: past - 3600, Until: past}, "")
	if err != nil || len(r.Lines) != 0 {
		t.Fatalf("range: %v %+v", err, r.Lines)
	}
	// Time range including them.
	since := time.Now().Add(-10 * time.Minute).Unix()
	r, err = s.Query(id, QuerySpec{Kind: "journal", Search: tg, Since: since}, "")
	if err != nil || len(r.Lines) != 2 {
		t.Fatalf("range incl: %v %+v", err, r.Lines)
	}
	for _, l := range r.Lines {
		if l.TS < since*1000 {
			t.Fatalf("line before since: %+v", l)
		}
	}
	// Level filter on everything.
	r, err = s.Query(id, QuerySpec{Kind: "journal", Lines: 100, Level: "err"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range r.Lines {
		if levelRank(l.Level) > 3 {
			t.Fatalf("level filter leaked %+v", l)
		}
	}
	// Unit, kernel and auth sources.
	r, err = s.Query(id, QuerySpec{Kind: "unit", Target: "nginx.service", Lines: 10}, "")
	if err != nil || len(r.Lines) == 0 {
		t.Fatalf("unit: %v %d", err, len(r.Lines))
	}
	if _, err := s.Query(id, QuerySpec{Kind: "kernel", Lines: 10}, ""); err != nil {
		t.Fatal(err)
	}
	r, err = s.Query(id, QuerySpec{Kind: "auth", Lines: 20}, "")
	if err != nil || len(r.Lines) == 0 {
		t.Fatalf("auth: %v %d", err, len(r.Lines))
	}
	if r.LimitReached != (len(r.Lines) == 20) {
		t.Fatal("limitReached")
	}
}

func TestFileQuery(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	tg := tag()
	run(t, c, id, "logger -t smtest-logs -p user.err "+core.Q(tg+" ERROR something broke")+"; logger -t smtest-logs "+core.Q(tg+" all fine"), false)

	var r QueryResult
	var err error
	// rsyslog writes asynchronously.
	for i := 0; i < 20; i++ {
		r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/syslog", Lines: 100, Search: tg}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Lines) == 2 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !r.Sudo || len(r.Lines) != 2 {
		t.Fatalf("sudo=%v lines=%+v", r.Sudo, r.Lines)
	}
	l := r.Lines[0]
	if l.TS == 0 || l.Source != "smtest-logs" || l.Level != "err" || !strings.HasPrefix(l.Message, tg) {
		t.Fatalf("parsed line: %+v", l)
	}
	// Level filter (server-side grep).
	r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/syslog", Search: tg, Level: "err"}, "")
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("level: %v %+v", err, r.Lines)
	}
	// Regex with Perl classes.
	r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/syslog", Search: strings.TrimPrefix(tg, "smtest-logs-") + `\s+all\s\w+`, Regex: true}, "")
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("regex: %v %+v", err, r.Lines)
	}
	// Time filtering in Go.
	since := time.Now().Add(-5 * time.Minute).Unix()
	r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/syslog", Lines: 500, Since: since}, "")
	if err != nil || len(r.Lines) == 0 {
		t.Fatalf("since: %v %d", err, len(r.Lines))
	}
	for _, l := range r.Lines {
		if l.TS < since*1000 {
			t.Fatalf("line before since: %+v", l)
		}
	}
	r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/syslog", Search: tg, Until: time.Now().Add(-time.Hour).Unix()}, "")
	if err != nil || len(r.Lines) != 0 {
		t.Fatalf("until: %v %+v", err, r.Lines)
	}
	// auth.log (adm group only) with sudo, plain tail.
	r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/auth.log", Lines: 5}, "")
	if err != nil || !r.Sudo || len(r.Lines) == 0 || len(r.Lines) > 5 {
		t.Fatalf("auth.log: %v %+v", err, r)
	}
	// Readable file without sudo.
	r, err = s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/dpkg.log", Lines: 3}, "")
	if err != nil || r.Sudo || len(r.Lines) != 3 {
		t.Fatalf("dpkg.log: %v %+v", err, r)
	}
	// Errors.
	if _, err := s.Query(id, QuerySpec{Kind: "file", Target: "/var/log/smtest-logs-missing.log"}, ""); code(err) != "logs.fileNotFound" {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := s.Query(id, QuerySpec{Kind: "file", Target: "/var/log"}, ""); code(err) != "logs.notAFile" {
		t.Fatalf("dir: %v", err)
	}
	for _, sp := range []QuerySpec{
		{Kind: "file", Target: "/var/log/../../etc/shadow"},
		{Kind: "file", Target: "/proc/kmsg"},
		{Kind: "unit", Target: "--help"},
		{Kind: "unit", Target: "x;reboot"},
		{Kind: "journal", Search: "(", Regex: true},
		{Kind: "docker", Target: "$(id)"},
	} {
		if _, err := s.Query(id, sp, ""); err == nil || !strings.HasPrefix(code(err), "logs.") {
			t.Errorf("accepted %+v: %v", sp, err)
		}
	}
}

func TestDownload(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	conn, _ := c.Conn(id)
	dir := t.TempDir()
	dest := filepath.Join(dir, "journal.log")
	r, err := s.downloadTo(conn, QuerySpec{Kind: "journal", Since: time.Now().Add(-24 * time.Hour).Unix()}, "", dest)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if r.Lines == 0 || int64(len(b)) != r.Bytes || !strings.Contains(string(b), ": ") {
		t.Fatalf("%+v (%d bytes)", r, len(b))
	}
	dest2 := filepath.Join(dir, "syslog.log")
	r, err = s.downloadTo(conn, QuerySpec{Kind: "file", Target: "/var/log/syslog", Since: time.Now().Add(-time.Hour).Unix()}, "", dest2)
	if err != nil || r.Lines == 0 {
		t.Fatalf("%v %+v", err, r)
	}
	if st, _ := os.Stat(dest2); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestStreams(t *testing.T) {
	ev := captureEvents(t)
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	defer s.ServiceShutdown()
	tg := tag()

	jid, err := s.StartStream(id, QuerySpec{Kind: "journal", Search: tg, Lines: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	fid, err := s.StartStream(id, QuerySpec{Kind: "file", Target: "/var/log/syslog", Search: tg, Level: "warning", Lines: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	for i := 0; i < 3; i++ {
		run(t, c, id, fmt.Sprintf("logger -t smtest-logs %s", core.Q(fmt.Sprintf("%s WARNING line %d", tg, i))), false)
	}
	run(t, c, id, "logger -t smtest-logs "+core.Q(tg+" just info"), false)
	ev.wait(t, jid, 10*time.Second, func(l []LogLine, _ *LinesEvent) bool { return len(l) >= 4 })
	ev.wait(t, fid, 10*time.Second, func(l []LogLine, _ *LinesEvent) bool { return len(l) >= 3 })

	// Concurrency limit: 2 running + 2 more = 4; the 5th is refused.
	a, err1 := s.StartStream(id, QuerySpec{Kind: "kernel"}, "")
	b, err2 := s.StartStream(id, QuerySpec{Kind: "auth"}, "")
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, err := s.StartStream(id, QuerySpec{Kind: "journal"}, ""); code(err) != "logs.tooManyStreams" {
		t.Fatalf("limit: %v", err)
	}
	for _, x := range []string{a, b} {
		if err := s.StopStream(x); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.StopStream(jid); err != nil {
		t.Fatal(err)
	}
	lines, done := ev.wait(t, jid, 10*time.Second, func(_ []LogLine, d *LinesEvent) bool { return d != nil })
	if done.Error != nil {
		t.Fatalf("stopped stream reported %v", done.Error)
	}
	if contains(lines, "just info") == false || !contains(lines, "WARNING line 2") {
		t.Fatalf("journal lines: %+v", lines)
	}
	lines, _ = ev.wait(t, fid, time.Second, func(l []LogLine, _ *LinesEvent) bool { return true })
	if contains(lines, "just info") {
		t.Fatal("level filter not applied to file stream")
	}

	// Closing the connection ends the remaining stream.
	c.Manager.Disconnect(id)
	ev.wait(t, fid, 10*time.Second, func(_ []LogLine, d *LinesEvent) bool { return d != nil })
	s.streamsMu.Lock()
	n := len(s.streams)
	s.streamsMu.Unlock()
	if n != 0 {
		t.Fatalf("%d streams left", n)
	}

	// The follow processes are gone on the server.
	c2, id2 := testutil.Connect(t, testutil.FullPort())
	time.Sleep(time.Second)
	out := run(t, c2, id2, "ps -eo args | grep -E 'journalctl .*-f|tail -n 10 -F' | grep -v grep || true", false)
	if strings.Contains(out, tg) || strings.Contains(out, "tail -n 10 -F") {
		t.Fatalf("follow processes left: %s", out)
	}
}

func TestDocker(t *testing.T) {
	ev := captureEvents(t)
	c, id := testutil.Connect(t, testutil.DindPort())
	s := New(c)
	defer s.ServiceShutdown()
	name := tag()
	run(t, c, id, "docker image inspect busybox >/dev/null 2>&1 || docker pull -q busybox >/dev/null", false)
	run(t, c, id, "docker run -d --name "+name+` busybox sh -c 'i=0; while true; do echo "line $i INFO hello"; echo "line $i ERROR bad" >&2; i=$((i+1)); sleep 0.2; done' >/dev/null`, false)
	t.Cleanup(func() {
		conn, err := c.AnyConn(context.Background(), id)
		if err == nil {
			ctx, cancel := core.Timeout(30 * time.Second)
			defer cancel()
			_, _ = c.Run(ctx, conn, "docker rm -f "+name, false, "", "")
		}
	})
	time.Sleep(1500 * time.Millisecond)

	sl, err := s.Sources(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, src := range sl.Sources {
		if src.Kind == "docker" && src.Target == name {
			found = src.State == "running" && !src.NeedsSudo
		}
	}
	if !found || sl.Journal || sl.Systemd {
		t.Fatalf("sources: %+v", sl)
	}

	r, err := s.Query(id, QuerySpec{Kind: "docker", Target: name, Lines: 4}, "")
	if err != nil || len(r.Lines) != 4 || r.Lines[0].TS == 0 || !strings.HasPrefix(r.Lines[0].Message, "line ") {
		t.Fatalf("%v %+v", err, r.Lines)
	}
	r, err = s.Query(id, QuerySpec{Kind: "docker", Target: name, Lines: 5, Level: "err"}, "")
	if err != nil || len(r.Lines) != 5 {
		t.Fatalf("%v %+v", err, r.Lines)
	}
	for _, l := range r.Lines {
		if l.Level != "err" || !strings.Contains(l.Message, "ERROR") {
			t.Fatalf("level: %+v", l)
		}
	}
	r, err = s.Query(id, QuerySpec{Kind: "docker", Target: name, Search: "line 3 info", Since: time.Now().Add(-time.Minute).Unix()}, "")
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("search: %v %+v", err, r.Lines)
	}
	if _, err := s.Query(id, QuerySpec{Kind: "docker", Target: "smtest-logs-nope"}, ""); code(err) != "logs.containerNotFound" {
		t.Fatalf("missing container: %v", err)
	}
	if _, err := s.Query(id, QuerySpec{Kind: "journal"}, ""); code(err) != "logs.noJournal" {
		t.Fatalf("no journal: %v", err)
	}

	sid, err := s.StartStream(id, QuerySpec{Kind: "docker", Target: name, Lines: 2, Search: "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := ev.wait(t, sid, 10*time.Second, func(l []LogLine, _ *LinesEvent) bool { return len(l) >= 6 })
	for _, l := range lines {
		if !strings.Contains(l.Message, "hello") {
			t.Fatalf("unfiltered: %+v", l)
		}
	}
	// A stream ends by itself when the container goes away.
	run(t, c, id, "docker rm -f "+name+" >/dev/null", false)
	ev.wait(t, sid, 15*time.Second, func(_ []LogLine, d *LinesEvent) bool { return d != nil })

	// Services on a server without an init system.
	list, err := s.ServiceList(id)
	if err != nil || list.Init != "none" || len(list.Units) != 0 {
		t.Fatalf("%v %+v", err, list)
	}
	if _, err := s.ServiceDetail(id, "sshd.service"); code(err) != "sys.noSystemd" {
		t.Fatalf("detail: %v", err)
	}
	if err := s.ServiceControl(id, "sshd", "restart", ""); code(err) != "logs.svc.noInit" {
		t.Fatalf("control: %v", err)
	}
}

func TestServices(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)

	list, err := s.ServiceList(id)
	if err != nil || list.Init != "systemd" {
		t.Fatalf("%v %+v", err, list.Init)
	}
	units := map[string]Unit{}
	for _, u := range list.Units {
		units[u.Name] = u
	}
	if u := units["nginx.service"]; u.Sub != "running" || u.UnitFileState != "enabled" || u.Description == "" {
		t.Errorf("nginx: %+v", u)
	}
	if u := units["caddy.service"]; u.UnitFileState != "disabled" || u.Active == "active" {
		t.Errorf("caddy: %+v", u)
	}

	st, err := s.ServiceStats(id)
	if err != nil || st.SampleNs <= 0 {
		t.Fatalf("%v %+v", err, st)
	}
	var ng *UnitStat
	for i := range st.Units {
		if st.Units[i].Name == "nginx.service" {
			ng = &st.Units[i]
		}
	}
	if ng == nil || ng.Memory <= 0 || ng.CPUNSec < 0 || ng.MainPID <= 0 {
		t.Fatalf("nginx stats: %+v", ng)
	}

	d, err := s.ServiceDetail(id, "nginx.service")
	if err != nil {
		t.Fatal(err)
	}
	if d.ActiveState != "active" || d.SubState != "running" || d.MainPID <= 0 || d.Uptime < 0 || d.Memory <= 0 || d.CPUPercent < 0 ||
		d.FragmentPath == "" || !strings.Contains(d.UnitFile, "[Service]") || d.UnitFileState != "enabled" || d.Restart == "" ||
		!contains2(d.WantedBy, "multi-user.target") || len(d.After) == 0 || d.LogsNeedSudo || d.ExecStart == "" {
		t.Fatalf("detail: %+v", d)
	}
	if _, err := s.ServiceDetail(id, "smtest-logs-missing.service"); code(err) != "logs.unitNotFound" {
		t.Fatalf("missing unit: %v", err)
	}
	if _, err := s.ServiceDetail(id, "-x"); code(err) != "sys.invalidUnit" {
		t.Fatalf("invalid: %v", err)
	}
}

func contains2(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestServiceChanges(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	const unit = "smtest-logs.service"
	run(t, c, id, `cat > /etc/systemd/system/smtest-logs.service <<'EOF'
[Unit]
Description=Server Manager logs test unit

[Service]
ExecStart=/bin/sleep infinity
EOF
systemctl daemon-reload`, true)
	t.Cleanup(func() {
		conn, err := c.AnyConn(context.Background(), id)
		if err != nil {
			return
		}
		ctx, cancel := core.Timeout(time.Minute)
		defer cancel()
		_, _ = c.Run(ctx, conn, "systemctl stop smtest-logs.service; rm -rf /etc/systemd/system/smtest-logs.service /etc/systemd/system/smtest-logs.service.d; systemctl daemon-reload; systemctl reset-failed smtest-logs.service 2>/dev/null; true", true, "", "")
	})

	if err := s.ServiceControl(id, unit, "start", ""); err != nil {
		t.Fatal(err)
	}
	d, err := s.ServiceDetail(id, unit)
	if err != nil || d.ActiveState != "active" || d.Restart != "no" || d.AutoRestart {
		t.Fatalf("%v %+v", err, d)
	}
	if err := s.SetAutoRestart(id, unit, true, ""); err != nil {
		t.Fatal(err)
	}
	d, err = s.ServiceDetail(id, unit)
	if err != nil || !d.AutoRestart || d.Restart != "on-failure" || d.RestartSec != "5s" || !contains2(d.DropInPaths, dropInPath(unit)) ||
		!strings.Contains(d.UnitFile, "Restart=on-failure") {
		t.Fatalf("auto-restart on: %v %+v", err, d)
	}
	// The unit actually restarts after a crash.
	run(t, c, id, "systemctl kill -s KILL smtest-logs.service", true)
	time.Sleep(6500 * time.Millisecond)
	d, _ = s.ServiceDetail(id, unit)
	if d.ActiveState != "active" || d.NRestarts < 1 {
		t.Fatalf("not restarted: %+v", d)
	}
	if err := s.SetAutoRestart(id, unit, false, ""); err != nil {
		t.Fatal(err)
	}
	d, _ = s.ServiceDetail(id, unit)
	if d.AutoRestart || d.Restart != "no" || len(d.DropInPaths) != 0 {
		t.Fatalf("auto-restart off: %+v", d)
	}
	out := run(t, c, id, "ls /etc/systemd/system/smtest-logs.service.d 2>&1 || true", false)
	if !strings.Contains(out, "No such file") {
		t.Fatalf("drop-in dir left: %s", out)
	}
	if err := s.ServiceControl(id, unit, "stop", ""); err != nil {
		t.Fatal(err)
	}
	d, _ = s.ServiceDetail(id, unit)
	if d.ActiveState != "inactive" {
		t.Fatalf("stop: %+v", d)
	}
	if err := s.ServiceControl(id, unit, "explode", ""); code(err) != "sys.invalidAction" {
		t.Fatal(err)
	}
	if err := s.SetAutoRestart(id, "../../etc/passwd", true, ""); code(err) != "sys.invalidUnit" {
		t.Fatal(err)
	}
	if err := s.SetAutoRestart(id, "smtest-logs.socket", true, ""); code(err) != "sys.invalidUnit" {
		t.Fatal(err)
	}

	// Audit trail.
	entries, err := c.AuditList(core.AuditQuery{Server: id, Action: "service."})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, e := range entries {
		if e.OK {
			actions[e.Action]++
		}
	}
	if actions["service.autoRestart"] != 2 || actions["service.start"] != 1 || actions["service.stop"] != 1 {
		t.Fatalf("audit: %v", actions)
	}

	// Viewers can read but not change.
	testutil.SetRole(t, c, id, "viewer")
	if err := s.ServiceControl(id, unit, "start", ""); code(err) != "access.denied" {
		t.Fatalf("viewer control: %v", err)
	}
	if err := s.SetAutoRestart(id, unit, true, ""); code(err) != "access.denied" {
		t.Fatalf("viewer auto-restart: %v", err)
	}
	if _, err := s.Query(id, QuerySpec{Kind: "unit", Target: unit, Lines: 5}, ""); err != nil {
		t.Fatalf("viewer read: %v", err)
	}
}
