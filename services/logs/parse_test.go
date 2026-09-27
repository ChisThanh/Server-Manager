package logs

import (
	"strings"
	"testing"
	"time"
)

func TestParseTS(t *testing.T) {
	loc := time.FixedZone("+0700", 7*3600)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, loc)
	cases := []struct {
		line string
		want time.Time
		rest string
	}{
		{"2026-09-25T03:07:11.335515+00:00 host sshd[85]: Server listening", time.Date(2026, 9, 25, 3, 7, 11, 335515000, time.UTC), "host sshd[85]: Server listening"},
		{"2026-09-25 10:00:00,123 INFO started", time.Date(2026, 9, 25, 10, 0, 0, 123000000, loc), "INFO started"},
		{"2026-09-25T03:28:01.123456789Z hello", time.Date(2026, 9, 25, 3, 28, 1, 123456789, time.UTC), "hello"},
		{"Sep 25 03:28:01 host CRON[1]: x", time.Date(2026, 9, 25, 3, 28, 1, 0, loc), "host CRON[1]: x"},
		{"Dec 31 23:59:59 host a: y", time.Date(2025, 12, 31, 23, 59, 59, 0, loc), "host a: y"},
		{"2026/09/25 03:28:01 [error] 12#12: *1 open() failed", time.Date(2026, 9, 25, 3, 28, 1, 0, loc), "[error] 12#12: *1 open() failed"},
		{"[Thu Sep 25 03:28:01.123456 2026] [core:error] [pid 1] x", time.Date(2026, 9, 25, 3, 28, 1, 123456000, loc), "[core:error] [pid 1] x"},
		{`1.2.3.4 - - [25/Sep/2026:03:28:01 +0000] "GET / HTTP/1.1" 200 1`, time.Date(2026, 9, 25, 3, 28, 1, 0, time.UTC), `1.2.3.4 - - [25/Sep/2026:03:28:01 +0000] "GET / HTTP/1.1" 200 1`},
	}
	for _, c := range cases {
		ms, rest, _ := parseTS(c.line, loc, now)
		if ms != c.want.UnixMilli() || rest != c.rest {
			t.Errorf("%q: got %v %q, want %v %q", c.line, time.UnixMilli(ms).UTC(), rest, c.want.UTC(), c.rest)
		}
	}
	if ms, _, _ := parseTS("no timestamp here", loc, now); ms != 0 {
		t.Errorf("unexpected ts %d", ms)
	}
}

func TestTextParser(t *testing.T) {
	p := &textParser{loc: time.UTC, now: time.Now(), syslog: true}
	l := p.parse("2026-09-25T03:07:11.335515+00:00 host sshd[85]: error: kex failed")
	if l.Source != "sshd" || l.Message != "error: kex failed" || l.Level != "err" || l.TS == 0 {
		t.Fatalf("%+v", l)
	}
	cont := p.parse("\tat java.lang.Thread.run")
	if cont.TS != l.TS {
		t.Fatal("continuation line should inherit the timestamp")
	}
	app := p.parse("2026-09-25 10:00:00 INFO main: started")
	if app.Source != "" || app.Level != "info" {
		t.Fatalf("app line misparsed as syslog: %+v", app)
	}
}

func TestDetectLevel(t *testing.T) {
	cases := map[string]string{
		"[error] 12#12: open() failed":           "err",
		"[core:warn] [pid 1] x":                  "warning",
		`level=info msg="ok"`:                    "info",
		`{"level":"debug","msg":"x"}`:            "debug",
		"2026 WARN something":                    "warning",
		"FATAL: out of memory":                   "crit",
		"connection failed for user x":           "err",
		"all good":                               "",
		"Accepted password for tester from 1.2.": "",
	}
	for in, want := range cases {
		if got := detectLevel(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestParseJournal(t *testing.T) {
	l, ok := parseJournal([]byte(`{"__REALTIME_TIMESTAMP":"1790307455741294","PRIORITY":"3","_SYSTEMD_UNIT":"nginx.service","SYSLOG_IDENTIFIER":"nginx","_PID":"72","MESSAGE":"boom"}`), time.UTC)
	if !ok || l.TS != 1790307455741 || l.Level != "err" || l.Source != "nginx" || l.Message != "boom" || !strings.HasSuffix(l.Raw, "nginx[72]: boom") {
		t.Fatalf("%+v", l)
	}
	// MESSAGE as a byte array (non-UTF-8 or control characters).
	l, ok = parseJournal([]byte(`{"MESSAGE":[104,105,10],"PRIORITY":"6","SYSLOG_IDENTIFIER":"x"}`), time.UTC)
	if !ok || l.Message != "hi" || l.Level != "info" {
		t.Fatalf("%+v", l)
	}
	l, ok = parseJournal([]byte(`{"MESSAGE":null}`), time.UTC)
	if !ok || l.Message != "" {
		t.Fatalf("%+v", l)
	}
	if _, ok := parseJournal([]byte(`not json`), time.UTC); ok {
		t.Fatal("expected failure")
	}
}

func TestNormalize(t *testing.T) {
	bad := []QuerySpec{
		{Kind: "unit", Target: "-x"},
		{Kind: "unit", Target: "a b"},
		{Kind: "file", Target: "relative.log"},
		{Kind: "file", Target: "/var/log/../etc/shadow"},
		{Kind: "file", Target: "/proc/kmsg"},
		{Kind: "docker", Target: "a;b"},
		{Kind: "nope"},
		{Kind: "journal", Level: "loud"},
		{Kind: "journal", Search: "(", Regex: true},
		{Kind: "journal", Since: 10, Until: 5},
		{Kind: "journal", Search: "a\nb"},
	}
	for _, sp := range bad {
		if _, err := normalize(sp, false); err == nil {
			t.Errorf("accepted %+v", sp)
		}
	}
	sp, err := normalize(QuerySpec{Kind: "journal", Lines: 99999}, false)
	if err != nil || sp.Lines != maxLines {
		t.Fatalf("%+v %v", sp, err)
	}
	sp, _ = normalize(QuerySpec{Kind: "journal"}, false)
	if sp.Lines != defaultLines {
		t.Fatal(sp.Lines)
	}
}

func TestToERE(t *testing.T) {
	if got := toERE(`\d+\s[\d\w]`); got != `[0-9]+[[:space:]][0-9[:alnum:]_]` {
		t.Fatal(got)
	}
}

func TestLevelPattern(t *testing.T) {
	if levelPattern("") != "" || levelPattern("debug") != "" {
		t.Fatal("no filter expected")
	}
	lp := levelPattern("warning")
	for _, w := range []string{"ERROR", "WARN", "FATAL"} {
		if !strings.Contains(lp, w) {
			t.Fatal(lp)
		}
	}
	if strings.Contains(lp, "INFO") {
		t.Fatal(lp)
	}
}

func TestParseUnits(t *testing.T) {
	units := ParseUnits(
		[]string{
			"nginx.service loaded active running A high performance web server",
			"auditd.service not-found inactive dead auditd.service",
			"getty@tty1.service loaded active running Getty on tty1",
			"● broken.service loaded failed failed Broken",
		},
		[]string{"nginx.service enabled enabled", "caddy.service disabled enabled", "getty@.service enabled enabled", "sshd.service alias -", "broken.service static -"},
	)
	got := map[string]Unit{}
	for _, u := range units {
		got[u.Name] = u
	}
	if _, ok := got["auditd.service"]; ok {
		t.Error("not-found unit kept")
	}
	if _, ok := got["sshd.service"]; ok {
		t.Error("alias kept")
	}
	if got["nginx.service"].UnitFileState != "enabled" || got["caddy.service"].UnitFileState != "disabled" || got["caddy.service"].Active != "inactive" {
		t.Errorf("%+v", got)
	}
	if got["getty@tty1.service"].UnitFileState != "enabled" {
		t.Error("instance should inherit template state")
	}
	if got["broken.service"].Active != "failed" {
		t.Error("failed unit")
	}
}

func TestParseOpenRC(t *testing.T) {
	out := "Runlevel: default\n sshd  [  started  ]\n crond [  stopped  ]\nDynamic Runlevel: manual\n nginx [  crashed  ]\n@@init\nsshd\nnginx\nredis\n"
	units := parseOpenRC(out)
	m := map[string]Unit{}
	for _, u := range units {
		m[u.Name] = u
	}
	if m["sshd"].Active != "active" || m["sshd"].UnitFileState != "enabled" || m["crond"].Active != "inactive" ||
		m["nginx"].Active != "failed" || m["nginx"].UnitFileState != "disabled" || m["redis"].Sub != "stopped" {
		t.Fatalf("%+v", m)
	}
}

func TestExecArgv(t *testing.T) {
	v := "{ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -g daemon on; master_process on; ; ignore_errors=no ; start_time=[n/a] }"
	if got := execArgv(v); got != "/usr/sbin/nginx -g daemon on; master_process on;" {
		t.Fatal(got)
	}
}
