package monitor

import (
	"strings"
	"testing"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/store"

	"github.com/zalando/go-keyring"
)

func newTestAlerting(t *testing.T) (*alerting, *core.Core) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.NewForTest(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	core.SetEmitHook(func(string, any) {})
	t.Cleanup(func() { core.SetEmitHook(nil); c.Close() })
	n := newNotifier(c)
	return newAlerting(c, n), c
}

func TestDefaultRulesSeededOnce(t *testing.T) {
	a, c := newTestAlerting(t)
	if len(a.Rules()) != len(defaultRules()) {
		t.Fatalf("seeded %d rules", len(a.Rules()))
	}
	// Deleting all rules must not re-seed them on the next start.
	for _, r := range a.Rules() {
		_ = a.DeleteRule(r.ID)
	}
	b := newAlerting(c, a.notifier)
	if len(b.Rules()) != 0 {
		t.Fatalf("rules re-seeded: %d", len(b.Rules()))
	}
}

func TestScopeAndBreach(t *testing.T) {
	sv := store.Server{ID: "s1", Environment: "production", Group: "api", Tags: []string{"web", "eu"}}
	cases := []struct {
		r    Rule
		want bool
	}{
		{Rule{}, true},
		{Rule{Environments: []string{"staging"}}, false},
		{Rule{Environments: []string{"production"}, Tags: []string{"eu"}}, true},
		{Rule{Tags: []string{"db"}}, false},
		{Rule{Groups: []string{"api"}, Servers: []string{"s1"}}, true},
		{Rule{Servers: []string{"s2"}}, false},
	}
	for i, c := range cases {
		if got := inScope(c.r, sv); got != c.want {
			t.Errorf("case %d: inScope=%v want %v", i, got, c.want)
		}
	}
	if !breach(Rule{Op: ">", Threshold: 80}, 81) || breach(Rule{Op: ">", Threshold: 80}, 80) || !breach(Rule{Op: "<", Threshold: 14}, 3) {
		t.Fatal("breach")
	}
}

func TestLifecyclePendingFiringResolved(t *testing.T) {
	a, c := newTestAlerting(t)
	for _, r := range a.Rules() {
		_ = a.DeleteRule(r.ID)
	}
	r, err := a.SaveRule(Rule{Name: "disk", Enabled: true, Metric: "disk", Op: ">", Threshold: 80, ForSec: 1, Severity: "crit", NotifyResolved: true})
	if err != nil {
		t.Fatal(err)
	}
	sv, _ := c.Store.Save(store.Server{Name: "web", Host: "h", User: "u"}, "", false)
	snap := &Snapshot{Mounts: []Mount{{Mount: "/", Pct: 91}, {Mount: "/data", Pct: 50}}}
	h := Health{}
	a.evaluate(sv, snap, h, nil, nil, nil, true)
	act := a.Active()
	if len(act) != 1 || act[0].State != "pending" || act[0].Target != "/" {
		t.Fatalf("want pending on /: %+v", act)
	}
	time.Sleep(1100 * time.Millisecond)
	a.evaluate(sv, snap, h, nil, nil, nil, true)
	if act = a.Active(); act[0].State != "firing" {
		t.Fatalf("want firing: %+v", act)
	}
	if cnt := a.count(); cnt.Firing != 1 || cnt.Critical != 1 || cnt.Unacked != 1 {
		t.Fatalf("count %+v", cnt)
	}
	// Survives a restart (state persisted).
	b := newAlerting(c, a.notifier)
	if len(b.Active()) != 1 || b.Active()[0].State != "firing" {
		t.Fatalf("not persisted: %+v", b.Active())
	}
	// One good sample is not enough (hysteresis), two are.
	snap.Mounts[0].Pct = 40
	a.evaluate(sv, snap, h, nil, nil, nil, true)
	if len(a.Active()) != 1 {
		t.Fatal("resolved too early")
	}
	a.evaluate(sv, snap, h, nil, nil, nil, true)
	if len(a.Active()) != 0 {
		t.Fatalf("not resolved: %+v", a.Active())
	}
	hist, _ := a.History(sv.ID, 10)
	if len(hist) != 1 || hist[0].Rule != r.ID {
		t.Fatalf("history %+v", hist)
	}
	evs, _ := c.Events(core.EventQuery{Server: sv.ID, Kinds: []string{"alert"}})
	if len(evs) != 2 || evs[0].Code != "alert.resolved" || evs[1].Code != "alert.firing" {
		t.Fatalf("events %+v", evs)
	}
}

func TestUnreachableSuppressesOthers(t *testing.T) {
	a, c := newTestAlerting(t)
	for _, r := range a.Rules() {
		_ = a.DeleteRule(r.ID)
	}
	_, _ = a.SaveRule(Rule{Name: "down", Enabled: true, Metric: "unreachable", Severity: "crit"})
	sv, _ := c.Store.Save(store.Server{Name: "db", Host: "h", User: "u", Monitor: true}, "", false)
	a.evaluate(sv, nil, Health{}, nil, nil, nil, false)
	act := a.Active()
	if len(act) != 1 || act[0].Metric != "unreachable" || act[0].State != "firing" {
		t.Fatalf("%+v", act)
	}
	other := &Alert{Server: sv.ID, Metric: "cpu"}
	if !a.suppressed(other) {
		t.Fatal("cpu alert should be suppressed while unreachable")
	}
}

func TestRenderMessages(t *testing.T) {
	al := Alert{ServerName: "api-01", RuleName: "CPU > 85%", Metric: "cpu", Op: ">", Threshold: 85, Value: 92.34, Severity: "crit", Since: time.Now().UnixMilli()}
	title, body := render("vi", []notification{{Alert: al, Kind: "firing"}}, 0)
	if !strings.Contains(title, "api-01") || !strings.Contains(title, "NGHIÊM TRỌNG") || !strings.Contains(body, "92.3% > 85.0%") {
		t.Fatalf("%q / %q", title, body)
	}
	title, body = render("en", []notification{{Alert: al, Kind: "firing"}, {Alert: Alert{ServerName: "db", Metric: "unreachable", RuleName: "x"}, Kind: "resolved"}}, 3)
	if !strings.Contains(title, "2 alerts") || !strings.Contains(body, "db: unreachable") || !strings.Contains(body, "3 more") {
		t.Fatalf("%q / %q", title, body)
	}
	if got := redactURL(`Post "https://api.telegram.org/bot123:SECRET/sendMessage": timeout`); strings.Contains(got, "SECRET") {
		t.Fatal(got)
	}
	if mdEscape("a_b*c.") != `a\_b\*c\.` {
		t.Fatal(mdEscape("a_b*c."))
	}
}

func TestNotifierRateLimit(t *testing.T) {
	a, _ := newTestAlerting(t)
	n := a.notifier
	n.mu.Lock()
	for i := 0; i < 30; i++ {
		n.sent["x"] = append(n.sent["x"], time.Now())
	}
	n.pending["x"] = []notification{{Kind: "firing"}}
	n.mu.Unlock()
	n.flush("x")
	if n.dropped["x"] != 1 {
		t.Fatalf("dropped=%d", n.dropped["x"])
	}
}
