//go:build integration

package command

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
	"server-manager/internal/testutil"
)

// env is a core with two profiles: the Debian server (full) and the Alpine
// Docker server (dind), both reachable only through background connections.
type env struct {
	c    *core.Core
	s    *CommandService
	full string
	dind string

	mu      sync.Mutex
	updates []ServerResult
}

func setup(t *testing.T) *env {
	t.Helper()
	c, full := testutil.Connect(t, testutil.FullPort())
	sv, _ := c.Store.Get(full)
	sv.Name = "smtest-cmd-full"
	if _, err := c.Store.Save(sv, "", false); err != nil {
		t.Fatal(err)
	}
	dsv, err := c.Store.Save(store.Server{Name: "smtest-cmd-dind", Host: "127.0.0.1", Port: testutil.DindPort(),
		User: testutil.User, AuthType: store.AuthPassword, Environment: "production"}, testutil.Password, true)
	if err != nil {
		t.Fatal(err)
	}
	// Trust the second server's host key (shared by the UI and background managers).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = c.Bg.Connect(ctx, dsv, testutil.Password)
	var hk *sshx.HostKeyError
	if errors.As(err, &hk) {
		if err := c.Manager.HostKeys().Trust(hk.Host, hk.Port, hk.KeyBase64); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	c.Bg.Disconnect(dsv.ID)
	// Prove AnyConn: no UI connection from here on.
	c.Manager.Disconnect(full)

	e := &env{c: c, s: New(c), full: full, dind: dsv.ID}
	core.SetEmitHook(func(name string, data any) {
		if name == EventUpdate {
			e.mu.Lock()
			e.updates = append(e.updates, data.(ServerResult))
			e.mu.Unlock()
		}
	})
	t.Cleanup(func() {
		core.SetEmitHook(nil)
		_ = e.s.ServiceShutdown()
	})
	return e
}

func (e *env) wait(t *testing.T, id string, max time.Duration) RunInfo {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		info, err := e.s.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != "running" {
			return info
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within %s", id, max)
	return RunInfo{}
}

func (e *env) run(t *testing.T, spec Spec, max time.Duration) RunInfo {
	t.Helper()
	id, err := e.s.Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	return e.wait(t, id, max)
}

func result(t *testing.T, info RunInfo, id string) ServerResult {
	t.Helper()
	for _, r := range info.Results {
		if r.ServerID == id {
			return r
		}
	}
	t.Fatalf("no result for %s", id)
	return ServerResult{}
}

func errCode(e *apperr.Error) string {
	if e == nil {
		return ""
	}
	return e.Code
}

func TestRunOnBothServers(t *testing.T) {
	e := setup(t)
	targets := e.s.Targets()
	if len(targets) != 2 {
		t.Fatalf("targets: %+v", targets)
	}
	for _, tg := range targets {
		if tg.Readiness != "auto" || !tg.CanExec || tg.Sudo != "stored" {
			t.Errorf("target %s: %+v", tg.Name, tg)
		}
	}

	info := e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "echo smtest-cmd-hello\nid -un\necho oops >&2\nexit 3"}, 60*time.Second)
	if info.Status != "failed" || info.Summary.Failed != 2 {
		t.Fatalf("expected both failed with exit 3: %+v", info)
	}
	for _, id := range []string{e.full, e.dind} {
		r := result(t, info, id)
		if r.ExitCode != 3 || !strings.Contains(r.Stdout, "smtest-cmd-hello\ntester") || strings.TrimSpace(r.Stderr) != "oops" {
			t.Errorf("%s: %+v", r.Name, r)
		}
		if r.StartedAt == 0 || r.FinishedAt < r.StartedAt {
			t.Errorf("%s: bad times %d %d", r.Name, r.StartedAt, r.FinishedAt)
		}
	}
	if _, err := e.c.Manager.Get(e.full); err == nil {
		t.Error("the command center must not open UI connections")
	}
	info = e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "uname -s"}, 60*time.Second)
	if info.Status != "done" || info.Summary.OK != 2 {
		t.Fatalf("expected done: %+v", info)
	}
	// Every state change was emitted.
	e.mu.Lock()
	states := map[string]bool{}
	for _, u := range e.updates {
		if u.RunID == info.ID {
			states[u.State] = true
		}
	}
	e.mu.Unlock()
	for _, st := range []string{StateQueued, StateConnecting, StateRunning, StateDone} {
		if !states[st] {
			t.Errorf("state %s not emitted (%v)", st, states)
		}
	}
	// Audit: one entry per server plus the summary.
	list, _ := e.c.AuditList(core.AuditQuery{Action: "cmd.run"})
	if len(list) != 4 {
		t.Errorf("cmd.run audit entries: %d", len(list))
	}
	list, _ = e.c.AuditList(core.AuditQuery{Action: "cmd.batch"})
	if len(list) != 2 || !strings.Contains(list[0].Detail, "run="+info.ID) || !list[0].OK {
		t.Errorf("cmd.batch audit: %+v", list)
	}
}

func TestSudo(t *testing.T) {
	e := setup(t)
	info := e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "id -u", Sudo: true}, 60*time.Second)
	for _, id := range []string{e.full, e.dind} {
		r := result(t, info, id)
		if r.State != StateDone || strings.TrimSpace(r.Stdout) != "0" {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
}

func TestSudoRequired(t *testing.T) {
	e := setup(t)
	// Connect in the UI with the password, then forget it: sudo has no
	// password to use and the server needs one.
	sv, _ := e.c.Store.Get(e.full)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.c.Manager.Connect(ctx, sv, testutil.Password); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Store.Save(sv, "", true); err != nil {
		t.Fatal(err)
	}
	for _, tg := range e.s.Targets() {
		if tg.ID == e.full && (tg.Readiness != "connected" || tg.Via != "ui") {
			t.Errorf("full should be connected via ui: %+v", tg)
		}
	}
	info := e.run(t, Spec{Servers: []string{e.full}, Command: "id -u", Sudo: true}, 60*time.Second)
	r := result(t, info, e.full)
	if r.State != StateFailed || errCode(r.Error) != "sudo.required" {
		t.Errorf("want sudo.required: %+v", r)
	}
	// Without the UI connection the server can't be reached at all.
	e.c.Manager.Disconnect(e.full)
	info = e.run(t, Spec{Servers: []string{e.full}, Command: "true"}, 60*time.Second)
	r = result(t, info, e.full)
	if r.State != StateSkipped || errCode(r.Error) != "auth.needSecret" {
		t.Errorf("want skipped auth.needSecret: %+v", r)
	}
}

func TestTimeoutKills(t *testing.T) {
	e := setup(t)
	start := time.Now()
	info := e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "echo started; (sleep 26; echo child) & sleep 27; echo after", TimeoutSec: 5}, 40*time.Second)
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("timeout took %s", d)
	}
	for _, id := range []string{e.full, e.dind} {
		r := result(t, info, id)
		if r.State != StateTimeout || errCode(r.Error) != "cmd.timeout" {
			t.Errorf("%s: %+v", r.Name, r)
		}
		if !strings.Contains(r.Stdout, "started") {
			t.Errorf("%s: output before the timeout was lost: %q", r.Name, r.Stdout)
		}
	}
	// The remote process is gone.
	info = e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "{ ps -eo args 2>/dev/null || ps -o args; } | grep -c '^sleep 2[67]' || true"}, 30*time.Second)
	for _, r := range info.Results {
		if strings.TrimSpace(r.Stdout) != "0" {
			t.Errorf("%s: sleep still running: %q", r.Name, r.Stdout)
		}
	}
}

func TestCancel(t *testing.T) {
	e := setup(t)
	id, err := e.s.Run(Spec{Servers: []string{e.full, e.dind}, Command: "sleep 29; echo after", Sudo: true, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the first server is running.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		info, _ := e.s.GetRun(id)
		if info.Results[0].State == StateRunning && info.Results[1].State == StateRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	if err := e.s.Cancel(id); err != nil {
		t.Fatal(err)
	}
	info := e.wait(t, id, 15*time.Second)
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("cancel took %s", d)
	}
	if info.Status != "cancelled" || info.Summary.Cancelled != 2 {
		t.Errorf("want 2 cancelled: %+v", info.Summary)
	}
	if err := e.s.Cancel(id); err != nil {
		t.Errorf("cancelling a finished run: %v", err)
	}
	// The remote processes (running as root through sudo) are gone.
	info = e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "{ ps -eo args 2>/dev/null || ps -o args; } | grep -c '^sleep 29' || true"}, 30*time.Second)
	for _, r := range info.Results {
		if strings.TrimSpace(r.Stdout) != "0" {
			t.Errorf("%s: sleep still running after cancel: %q", r.Name, r.Stdout)
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	e := setup(t)
	measure := func(conc int) (int, time.Duration) {
		start := time.Now()
		info := e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "sleep 2", Concurrency: conc}, 60*time.Second)
		elapsed := time.Since(start)
		if info.Summary.OK != 2 {
			t.Fatalf("conc %d: %+v", conc, info.Summary)
		}
		// Replay the events to find the maximum number of busy servers.
		e.mu.Lock()
		defer e.mu.Unlock()
		busy := map[string]bool{}
		max := 0
		for _, u := range e.updates {
			if u.RunID != info.ID {
				continue
			}
			busy[u.ServerID] = u.State == StateConnecting || u.State == StateRunning
			n := 0
			for _, b := range busy {
				if b {
					n++
				}
			}
			if n > max {
				max = n
			}
		}
		return max, elapsed
	}
	max, d := measure(1)
	if max != 1 || d < 4*time.Second {
		t.Errorf("concurrency 1: max busy %d, took %s", max, d)
	}
	max, d = measure(2)
	if max != 2 || d > 4*time.Second+1500*time.Millisecond {
		t.Errorf("concurrency 2: max busy %d, took %s", max, d)
	}
}

func TestViewerDenied(t *testing.T) {
	e := setup(t)
	testutil.SetRole(t, e.c, e.dind, "viewer")
	for _, tg := range e.s.Targets() {
		if tg.ID == e.dind && tg.CanExec {
			t.Error("viewer must not be able to exec")
		}
	}
	pv, err := e.s.Preview(Spec{Servers: []string{e.full, e.dind}, Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ps := range pv.Servers {
		if ps.ServerID == e.dind && (ps.Action != "skip" || ps.Reason != "access.denied") {
			t.Errorf("preview: %+v", ps)
		}
	}
	info := e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "echo ok"}, 60*time.Second)
	if r := result(t, info, e.full); r.State != StateDone {
		t.Errorf("full: %+v", r)
	}
	if r := result(t, info, e.dind); r.State != StateSkipped || errCode(r.Error) != "access.denied" {
		t.Errorf("dind: %+v", r)
	}
	list, _ := e.c.AuditList(core.AuditQuery{Server: e.dind, Action: "cmd.run"})
	if len(list) != 1 || list[0].OK {
		t.Errorf("denied attempt must be audited as failed: %+v", list)
	}
}

func TestOutputTruncation(t *testing.T) {
	e := setup(t)
	info := e.run(t, Spec{Servers: []string{e.full}, Command: "head -c 400000 /dev/zero | tr '\\0' a; head -c 1000 /dev/zero | tr '\\0' b >&2"}, 60*time.Second)
	id := info.ID
	// The final event carries 256 KB.
	e.mu.Lock()
	var last ServerResult
	for _, u := range e.updates {
		if u.RunID == id && u.State == StateDone {
			last = u
		}
	}
	e.mu.Unlock()
	if len(last.Stdout) != liveOutputMax || !last.Truncated || len(last.Stderr) != 1000 {
		t.Errorf("live: stdout %d truncated %v stderr %d", len(last.Stdout), last.Truncated, len(last.Stderr))
	}
	// History keeps 16 KB.
	r := result(t, info, e.full)
	if len(r.Stdout) != storedOutputMax || !r.Truncated {
		t.Errorf("stored: stdout %d truncated %v", len(r.Stdout), r.Truncated)
	}
}

func TestHistoryAndRerun(t *testing.T) {
	e := setup(t)
	info := e.run(t, Spec{Servers: []string{e.full, e.dind}, Command: "test -f /etc/debian_version"}, 60*time.Second)
	if info.Summary.OK != 1 || info.Summary.Failed != 1 || info.Status != "failed" {
		t.Fatalf("want 1 ok 1 failed: %+v", info.Summary)
	}
	if r := result(t, info, e.dind); r.ExitCode != 1 {
		t.Errorf("dind exit: %+v", r)
	}
	hist, err := e.s.History(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ID != info.ID || hist[0].Summary.OK != 1 || hist[0].Summary.Failed != 1 ||
		hist[0].Title != "test -f /etc/debian_version" || hist[0].Actor == "" || hist[0].Status != "failed" {
		t.Fatalf("history: %+v", hist)
	}
	pv, err := e.s.PreviewRerun(info.ID, true)
	if err != nil || len(pv.Servers) != 1 || pv.Servers[0].ServerID != e.dind {
		t.Fatalf("preview rerun: %+v %v", pv, err)
	}
	id, err := e.s.Rerun(info.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	again := e.wait(t, id, 60*time.Second)
	if len(again.Results) != 1 || again.Results[0].ServerID != e.dind {
		t.Fatalf("rerun failed only: %+v", again.Results)
	}
	id, _ = e.s.Rerun(info.ID, false)
	all := e.wait(t, id, 60*time.Second)
	if len(all.Results) != 2 {
		t.Fatalf("rerun all: %+v", all.Results)
	}
	hist, _ = e.s.History(10)
	if len(hist) != 3 {
		t.Errorf("history size %d", len(hist))
	}
	if _, err := e.s.GetRun("nope"); !apperr.HasCode(err, "cmd.runNotFound") {
		t.Errorf("GetRun unknown: %v", err)
	}
}

func TestStopOnFailure(t *testing.T) {
	e := setup(t)
	info := e.run(t, Spec{Servers: []string{e.dind, e.full}, Command: "test -f /etc/debian_version", Concurrency: 1, StopOnFailure: true}, 60*time.Second)
	if r := result(t, info, e.dind); r.State != StateFailed {
		t.Errorf("dind: %+v", r)
	}
	if r := result(t, info, e.full); r.State != StateSkipped || errCode(r.Error) != "cmd.stoppedAfterFailure" {
		t.Errorf("full: %+v", r)
	}
}

func TestPresets(t *testing.T) {
	e := setup(t)
	both := []string{e.full, e.dind}
	check := func(preset string, params map[string]string, servers []string, want ...string) {
		t.Helper()
		info := e.run(t, Spec{Servers: servers, Kind: "preset", Preset: preset, Params: params}, 90*time.Second)
		for _, r := range info.Results {
			if r.State != StateDone {
				t.Errorf("%s on %s: %+v", preset, r.Name, r)
				continue
			}
			for _, w := range want {
				if !strings.Contains(r.Stdout, w) {
					t.Errorf("%s on %s: %q missing in %q", preset, r.Name, w, r.Stdout)
				}
			}
		}
	}
	check("disk", nil, both, "Filesystem", "Mounted on")
	check("memory", nil, both, "Mem:")
	check("uptime", nil, both, "load", "loadavg:")
	check("os", nil, both, "OS: ", "Kernel: ")
	check("top", map[string]string{"count": "5"}, both, "PID")
	check("top", map[string]string{"sort": "mem", "count": "3"}, both, "PID")
	check("updates", nil, []string{e.full}, "manager: apt", "pending updates:")
	check("updates", nil, []string{e.dind}, "manager: apk", "pending updates:")
	check("docker.ps", map[string]string{"all": "true"}, []string{e.dind}, "NAMES")
	check("service.status", map[string]string{"unit": "nginx"}, []string{e.full}, "nginx")
	check("service.reload", map[string]string{"unit": "nginx"}, []string{e.full})

	// A command with a sudo-only preset forces sudo.
	info := e.run(t, Spec{Servers: []string{e.full}, Kind: "preset", Preset: "service.restart", Params: map[string]string{"unit": "smtest-cmd-missing"}}, 60*time.Second)
	if !info.Sudo || info.Results[0].State != StateFailed || info.Results[0].ExitCode == 0 {
		t.Errorf("restart of a missing unit: %+v", info)
	}
}

func TestInvalidInput(t *testing.T) {
	e := setup(t)
	bad := []struct {
		spec Spec
		code string
	}{
		{Spec{Servers: []string{e.full}, Command: "true", TimeoutSec: 2}, "cmd.invalidTimeout"},
		{Spec{Servers: []string{e.full}, Command: "true", TimeoutSec: 4000}, "cmd.invalidTimeout"},
		{Spec{Servers: []string{e.full}, Command: "true", Concurrency: 51}, "cmd.invalidConcurrency"},
		{Spec{Servers: []string{}, Command: "true"}, "cmd.noServers"},
		{Spec{Servers: []string{e.full}, Command: "   "}, "cmd.empty"},
		{Spec{Servers: []string{e.full}, Command: "a\x00b"}, "cmd.invalidChars"},
		{Spec{Servers: []string{e.full}, Command: strings.Repeat("x", MaxCommandBytes+1)}, "cmd.tooLong"},
		{Spec{Servers: []string{"nope"}, Command: "true"}, "server.notFound"},
		{Spec{Servers: []string{e.full}, Kind: "preset", Preset: "service.restart", Params: map[string]string{"unit": "x;id"}}, "cmd.invalidUnit"},
		{Spec{Servers: []string{e.full}, Kind: "weird"}, "cmd.invalidKind"},
	}
	for _, b := range bad {
		if _, err := e.s.Run(b.spec); !apperr.HasCode(err, b.code) {
			t.Errorf("Run(%+v) = %v, want %s", b.spec.Command, err, b.code)
		}
	}
	pv, err := e.s.Preview(Spec{Servers: []string{e.full, e.full, e.dind}, Command: "rm -rf /", Sudo: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Danger || len(pv.Servers) != 2 || pv.TimeoutSec != 60 || pv.Concurrency != e.c.Settings().CommandConcurrency ||
		!strings.HasPrefix(pv.Servers[0].Command, "sudo -- sh -c ") {
		t.Errorf("preview: %+v", pv)
	}
	if h, _ := e.s.History(10); len(h) != 0 {
		t.Errorf("dry run must not record history: %+v", h)
	}
}

func TestSnippets(t *testing.T) {
	e := setup(t)
	sn, err := e.s.SaveSnippet(Snippet{Name: "smtest-cmd-snippet", Command: "uptime", Sudo: true, Description: "d"})
	if err != nil || sn.ID == "" {
		t.Fatal(sn, err)
	}
	sn.Command = "uptime -p"
	if _, err := e.s.SaveSnippet(sn); err != nil {
		t.Fatal(err)
	}
	list, _ := e.s.Snippets()
	if len(list) != 1 || list[0].Command != "uptime -p" || !list[0].Sudo || list[0].Created == 0 {
		t.Errorf("snippets: %+v", list)
	}
	if _, err := e.s.SaveSnippet(Snippet{Name: " ", Command: "x"}); !apperr.HasCode(err, "cmd.snippetName") {
		t.Errorf("empty name: %v", err)
	}
	if _, err := e.s.SaveSnippet(Snippet{ID: "missing", Name: "a", Command: "x"}); !apperr.HasCode(err, "cmd.snippetNotFound") {
		t.Errorf("unknown id: %v", err)
	}
	if err := e.s.DeleteSnippet(sn.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.s.Snippets(); len(list) != 0 {
		t.Errorf("not deleted: %+v", list)
	}
}
