//go:build integration

package monitor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

func TestCollectParse(t *testing.T) {
	for _, port := range []int{testutil.FullPort(), testutil.DindPort()} {
		c, id := testutil.Connect(t, port)
		conn, _ := c.Conn(id)
		ctx, cancel := core.Timeout(20 * time.Second)
		res1, err := conn.Exec(ctx, "sh -c "+core.Q(collectScript), nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		ctx, cancel = core.Timeout(20 * time.Second)
		res2, err := conn.Exec(ctx, "sh -c "+core.Q(collectScript), nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		a, b := parseRaw(res1.Stdout), parseRaw(res2.Stdout)
		s := snapshot(id, &a, b)
		if !s.HasRates || s.CPUs == 0 || len(s.Cores) != s.CPUs || s.MemTotal == 0 || len(s.Mounts) == 0 || s.Procs == 0 || len(s.TopCPU) == 0 {
			t.Fatalf("port %d: incomplete snapshot: %+v", port, s)
		}
		if s.CPU < 0 || s.CPU > 100 || s.MemPct <= 0 || s.MemPct > 100 {
			t.Fatalf("port %d: bad values cpu=%v mem=%v", port, s.CPU, s.MemPct)
		}
		if len(s.Nets) == 0 {
			t.Fatalf("port %d: no network interfaces", port)
		}
		t.Logf("port %d: cpu=%.1f%% cores=%d mem=%.1f%% mounts=%d disks=%d nets=%d procs=%d docker=%v failed=%v",
			port, s.CPU, s.CPUs, s.MemPct, len(s.Mounts), len(s.Disks), len(s.Nets), s.Procs, s.HasDocker, s.Failed)
	}
}

func TestWorkerAlertsNotify(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	// Webhook receiver.
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer srv.Close()
	settings := c.Settings()
	settings.CollectInterval = 5
	settings.NotifyLang = "en"
	if _, err := c.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	s := New(c)
	ch, err := s.SaveChannel(Channel{Name: "hook", Type: "webhook", Enabled: true, Config: map[string]string{"url": srv.URL}}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TestChannel(ch, ""); err != nil {
		t.Fatal(err)
	}
	// A rule that always fires (memory > 0%) and one on a watched service.
	always, err := s.SaveRule(Rule{Name: "mem>0", Enabled: true, Metric: "memory", Op: ">", Threshold: 0, Severity: "warn", Channels: []string{ch.ID}, NotifyResolved: true})
	if err != nil {
		t.Fatal(err)
	}
	sv, _ := c.Store.Get(id)
	sv.WatchServices = []string{"nginx", "smtest-monitor-missing"}
	sv.HTTPChecks = nil
	if _, err := c.Store.Save(sv, "", false); err != nil {
		t.Fatal(err)
	}
	s.start()
	defer s.shutdown()

	var ev SampleEvent
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		s.Live(id)
		if e, ok := s.Latest(id); ok && e.Snapshot != nil && e.Snapshot.HasRates {
			ev = e
			break
		}
		time.Sleep(time.Second)
	}
	if ev.Snapshot == nil {
		t.Fatal("no sample with rates")
	}
	var nginx, missing string
	for _, ck := range ev.Health.Checks {
		if ck.ID == "service:nginx" {
			nginx = ck.Status
		}
		if ck.ID == "service:smtest-monitor-missing" {
			missing = ck.Status
		}
	}
	if nginx != "ok" || missing != "crit" {
		t.Fatalf("service checks nginx=%q missing=%q: %+v", nginx, missing, ev.Health.Checks)
	}
	// Alerts: mem>0 fires immediately; builtin "Service down" fires after 60s,
	// so only check that it is pending.
	active := s.ActiveAlerts()
	var memFiring, svcPending bool
	for _, a := range active {
		if a.Rule == always.ID && a.State == "firing" {
			memFiring = true
		}
		if a.Metric == "service_down" && a.Target == "smtest-monitor-missing" && a.State == "pending" {
			svcPending = true
		}
	}
	if !memFiring || !svcPending {
		t.Fatalf("alerts: %+v", active)
	}
	// Notification delivered (after the 5 s batching window), exactly once.
	time.Sleep(7 * time.Second)
	mu.Lock()
	n := len(got)
	var firing int
	for _, m := range got {
		if m["kind"] == "firing" {
			firing++
		}
	}
	mu.Unlock()
	if n < 2 || firing != 1 {
		t.Fatalf("webhook calls=%d firing=%d: %+v", n, firing, got)
	}
	// History has points.
	ser, err := s.History(id, time.Now().Add(-10*time.Minute).Unix(), time.Now().Unix())
	if err != nil || len(ser.TS) == 0 || len(ser.Values["cpu"]) != len(ser.TS) {
		t.Fatalf("history: %v %+v", err, ser)
	}
	// Ack, then make the rule unreachable to resolve.
	var key string
	for _, a := range s.ActiveAlerts() {
		if a.Rule == always.ID {
			key = a.Key
		}
	}
	if err := s.Ack(key); err != nil {
		t.Fatal(err)
	}
	always.Threshold = 101
	if _, err := s.SaveRule(always); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(30 * time.Second)
	resolved := false
	for time.Now().Before(deadline) && !resolved {
		s.CheckNow(id)
		time.Sleep(2 * time.Second)
		resolved = true
		for _, a := range s.ActiveAlerts() {
			if a.Rule == always.ID {
				resolved = false
			}
		}
	}
	if !resolved {
		t.Fatal("alert did not resolve")
	}
	time.Sleep(6 * time.Second)
	mu.Lock()
	var res int
	for _, m := range got {
		if m["kind"] == "resolved" || strings.Contains(m["text"].(string), "RESOLVED") {
			res++
		}
	}
	mu.Unlock()
	if res == 0 {
		t.Fatal("no resolved notification")
	}
	evs, _ := c.Events(core.EventQuery{Server: id, Kinds: []string{"alert"}})
	if len(evs) < 2 {
		t.Fatalf("timeline: %+v", evs)
	}
	hist, _ := s.AlertHistory(id, 10)
	if len(hist) == 0 {
		t.Fatal("empty alert history")
	}
}

func TestUnreachable(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	sv, _ := c.Store.Get(id)
	sv.Monitor = true
	sv.Port = 1 // nothing listens there
	if _, err := c.Store.Save(sv, "", false); err != nil {
		t.Fatal(err)
	}
	c.Manager.Disconnect(id)
	c.Bg.Disconnect(id)
	rule := defaultRules()[6]
	rule.ForSec = 0
	if rule.Metric != "unreachable" {
		t.Fatal("rule order changed")
	}
	if _, err := s.SaveRule(rule); err != nil {
		t.Fatal(err)
	}
	s.start()
	defer s.shutdown()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		s.CheckNow(id)
		time.Sleep(2 * time.Second)
		for _, a := range s.ActiveAlerts() {
			if a.Metric == "unreachable" && a.State == "firing" {
				if e, _ := s.Latest(id); e.Reachable || e.Health.Status != "crit" {
					t.Fatalf("health: %+v", e.Health)
				}
				return
			}
		}
	}
	t.Fatalf("unreachable alert not firing: %+v", s.ActiveAlerts())
}
