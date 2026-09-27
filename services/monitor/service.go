// Package monitor collects metrics from servers over SSH, keeps their
// history, evaluates health checks and alert rules, and sends
// notifications. Servers flagged "monitor" are watched in the background
// on the app's own connection; other servers are sampled while they are
// connected in the UI.
package monitor

import (
	"context"
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

const (
	EventSample = "monitor:sample"
	EventAlerts = "monitor:alerts"
)

// SampleEvent carries the latest state of one server.
type SampleEvent struct {
	Server    string       `json:"server"`
	Snapshot  *Snapshot    `json:"snapshot"`
	Health    Health       `json:"health"`
	HTTP      []HTTPResult `json:"http"`
	Reachable bool         `json:"reachable"`
	Error     string       `json:"error"`
	At        int64        `json:"at"` // unix ms of the attempt
}

func init() {
	application.RegisterEvent[SampleEvent](EventSample)
	application.RegisterEvent[AlertCount](EventAlerts)
}

type MonitorService struct {
	core     *core.Core
	hist     *history
	notifier *notifier
	alerts   *alerting

	mu      sync.Mutex
	workers map[string]*worker
	stop    chan struct{}
	wg      sync.WaitGroup

	certSource func(connID string) ([]CertInfo, error)
}

func New(c *core.Core) *MonitorService {
	n := newNotifier(c)
	return &MonitorService{core: c, hist: newHistory(c.DB), notifier: n, alerts: newAlerting(c, n), workers: map[string]*worker{}}
}

// CertInfo is a TLS certificate installed on a server.
type CertInfo struct {
	Name     string
	Domains  []string
	NotAfter int64 // unix seconds
}

// SetCertSource installs the function listing a server's certificates
// (the web module), checked every 6 hours for expiry.
func SetCertSource(s *MonitorService, fn func(connID string) ([]CertInfo, error)) { s.certSource = fn }

// SetDesktopNotifier installs the OS notification function (from main).
// A plain function, not a method, so it isn't exposed to the frontend.
func SetDesktopNotifier(s *MonitorService, fn func(title, body string) error) {
	s.notifier.desktop = fn
}

func (s *MonitorService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	s.start()
	return nil
}

func (s *MonitorService) ServiceShutdown() error {
	s.shutdown()
	return nil
}

func (s *MonitorService) start() {
	s.stop = make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		reconcile := time.NewTicker(3 * time.Second)
		prune := time.NewTicker(time.Hour)
		defer reconcile.Stop()
		defer prune.Stop()
		s.reconcile()
		s.prune()
		for {
			select {
			case <-s.stop:
				return
			case <-reconcile.C:
				s.reconcile()
			case <-prune.C:
				s.prune()
			}
		}
	}()
}

func (s *MonitorService) shutdown() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	s.mu.Lock()
	for _, w := range s.workers {
		w.halt()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *MonitorService) prune() {
	s.hist.prune()
	s.alerts.prune()
	s.core.Prune()
}

// uiConnected reports whether the server has a live UI connection.
func (s *MonitorService) uiConnected(id string) bool {
	conn, err := s.core.Manager.Get(id)
	return err == nil && conn.Connected()
}

// reconcile starts/stops workers to match profiles and UI connections.
func (s *MonitorService) reconcile() {
	servers := s.core.Store.List()
	want := map[string]store.Server{}
	for _, sv := range servers {
		if sv.Monitor || s.uiConnected(sv.ID) {
			want[sv.ID] = sv
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, w := range s.workers {
		if _, ok := want[id]; !ok {
			w.halt()
			delete(s.workers, id)
		}
	}
	known := map[string]bool{}
	for _, sv := range servers {
		known[sv.ID] = true
	}
	for id := range want {
		if _, ok := s.workers[id]; !ok {
			w := &worker{s: s, id: id, quit: make(chan struct{}), kick: make(chan struct{}, 1)}
			s.workers[id] = w
			s.wg.Add(1)
			go w.run()
		}
	}
	// Profiles deleted since last time: drop their data.
	for _, id := range s.forgotten(known) {
		s.hist.forget(id)
		s.alerts.forgetServer(id)
	}
}

// forgotten returns ids with stored metrics that no longer have a profile.
func (s *MonitorService) forgotten(known map[string]bool) []string {
	rows, err := s.core.DB.Query(`SELECT DISTINCT server FROM metrics`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !known[id] {
			out = append(out, id)
		}
	}
	return out
}

func (s *MonitorService) worker(id string) *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[id]
}

// ---- per-server worker ----

type worker struct {
	s    *MonitorService
	id   string
	quit chan struct{}
	kick chan struct{}
	once sync.Once

	liveUntil atomic.Int64

	mu        sync.Mutex
	prev      *raw
	last      SampleEvent
	has       bool
	fails     int
	http      []HTTPResult
	httpAt    time.Time
	httpBusy  bool
	backup    *backupState
	backupAt  time.Time
	collectMs int64
	certs     []CertInfo
	certsAt   time.Time
	certsBusy bool
}

func (w *worker) halt() { w.once.Do(func() { close(w.quit) }) }

func (w *worker) run() {
	defer w.s.wg.Done()
	for {
		w.collect()
		interval := time.Duration(w.s.core.Settings().CollectInterval) * time.Second
		if time.Now().UnixMilli() < w.liveUntil.Load() {
			interval = 3 * time.Second
		}
		select {
		case <-w.quit:
			return
		case <-w.kick:
		case <-time.After(interval):
		}
	}
}

var unitRe = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+$`)

func (w *worker) conn(ctx context.Context, sv store.Server) (*sshx.Conn, error) {
	if sv.Monitor {
		return w.s.core.AnyConn(ctx, sv.ID)
	}
	conn, err := w.s.core.Manager.Get(sv.ID)
	if err != nil || !conn.Connected() {
		return nil, apperr.New("conn.notConnected")
	}
	return conn, nil
}

func (w *worker) collect() {
	sv, err := w.s.core.Store.Get(w.id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := w.conn(ctx, sv)
	var out sshx.ExecResult
	if err == nil {
		script := collectScript
		var units []string
		for _, u := range sv.WatchServices {
			if unitRe.MatchString(u) {
				units = append(units, core.Q(u))
			}
		}
		if len(units) > 0 {
			script += "echo @@units; for u in " + strings.Join(units, " ") + `; do printf '%s\t%s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)"; done` + "\n"
		}
		out, err = conn.Exec(ctx, "sh -c "+core.Q(script), nil)
	}
	elapsed := time.Since(start).Milliseconds()

	w.mu.Lock()
	ev := SampleEvent{Server: w.id, At: time.Now().UnixMilli()}
	if err != nil {
		w.fails++
		ev.Error = apperr.From(err).Error()
		// One failure can be a blip; report unreachable from the second.
		ev.Reachable = w.fails < 2 && w.has
		if w.has {
			ev.Snapshot = w.last.Snapshot
		}
	} else {
		w.fails = 0
		w.collectMs = elapsed
		cur := parseRaw(out.Stdout)
		snap := snapshot(w.id, w.prev, cur)
		snap.TS = time.Now().Unix()
		w.prev = &cur
		ev.Snapshot = &snap
		ev.Reachable = true
		if snap.HasRates {
			w.s.hist.add(w.id, pointOf(snap))
		}
	}
	// HTTP checks run every minute in the background.
	if len(sv.HTTPChecks) > 0 && !w.httpBusy && time.Since(w.httpAt) > time.Minute {
		w.httpBusy = true
		go w.runHTTP(sv, conn)
	}
	if len(sv.HTTPChecks) == 0 {
		w.http = nil
	}
	if err == nil && w.s.certSource != nil && !w.certsBusy && time.Since(w.certsAt) > 6*time.Hour {
		w.certsBusy = true
		go w.loadCerts()
	}
	if time.Since(w.backupAt) > 2*time.Minute {
		w.backup = loadBackupState(w.s.core, w.id)
		w.backupAt = time.Now()
	}
	httpRes := append([]HTTPResult(nil), w.http...)
	certs := append([]CertInfo(nil), w.certs...)
	backup := w.backup
	collectMs := w.collectMs
	w.mu.Unlock()

	recent := w.s.hist.recent(w.id, 5*time.Minute)
	monitored := sv.Monitor
	reachable := ev.Reachable || !monitored // UI-only servers never alert as unreachable
	ev.Health = evalHealth(sv, ev.Snapshot, recent, httpRes, backup, ev.Reachable, ev.Error, collectMs)
	addCertChecks(&ev.Health, certs, httpRes)
	ev.HTTP = httpRes
	if ev.HTTP == nil {
		ev.HTTP = []HTTPResult{}
	}
	if err == nil || monitored {
		w.s.alerts.evaluate(sv, ev.Snapshot, ev.Health, recent, httpRes, backup, reachable, certs)
	}

	w.mu.Lock()
	w.last, w.has = ev, true
	w.mu.Unlock()
	core.Emit(EventSample, ev)
}

func (w *worker) loadCerts() {
	certs, err := w.s.certSource(w.id)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.certsBusy = false
	if err != nil {
		w.certsAt = time.Now().Add(-5 * time.Hour) // retry in an hour
		return
	}
	w.certs, w.certsAt = certs, time.Now()
}

func (w *worker) runHTTP(sv store.Server, conn *sshx.Conn) {
	res := make([]HTTPResult, len(sv.HTTPChecks))
	var wg sync.WaitGroup
	for i, chk := range sv.HTTPChecks {
		wg.Add(1)
		go func(i int, chk store.HTTPCheck) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res[i] = runHTTPCheck(ctx, w.s.core, conn, chk)
		}(i, chk)
	}
	wg.Wait()
	w.mu.Lock()
	w.http, w.httpAt, w.httpBusy = res, time.Now(), false
	w.mu.Unlock()
}

// ---- API ----

// Latest returns the most recent sample of a server.
func (s *MonitorService) Latest(serverID string) (SampleEvent, bool) {
	w := s.worker(serverID)
	if w == nil {
		return SampleEvent{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last, w.has
}

// Live asks for 3-second sampling for the next 20 seconds (call it
// periodically while a realtime view is open).
func (s *MonitorService) Live(serverID string) {
	if w := s.worker(serverID); w != nil {
		was := w.liveUntil.Swap(time.Now().Add(20 * time.Second).UnixMilli())
		if was < time.Now().UnixMilli() {
			select {
			case w.kick <- struct{}{}:
			default:
			}
		}
	}
}

// CheckNow triggers an immediate collection (and HTTP checks).
func (s *MonitorService) CheckNow(serverID string) {
	if w := s.worker(serverID); w != nil {
		w.mu.Lock()
		w.httpAt = time.Time{}
		w.mu.Unlock()
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// History returns stored metrics between from and to (unix seconds).
func (s *MonitorService) History(serverID string, from, to int64) (Series, error) {
	if to <= 0 {
		to = time.Now().Unix()
	}
	if from <= 0 || from >= to {
		from = to - 3600
	}
	return s.hist.query(serverID, from, to)
}

// FleetItem summarises one server for the fleet view.
type FleetItem struct {
	Server     string  `json:"server"`
	Monitored  bool    `json:"monitored"`
	Collecting bool    `json:"collecting"`
	Reachable  bool    `json:"reachable"`
	Status     string  `json:"status"` // health: ok | warn | crit | unknown | "" (no data)
	OK         int     `json:"ok"`
	Total      int     `json:"total"`
	CPU        float64 `json:"cpu"`
	Mem        float64 `json:"mem"`
	Disk       float64 `json:"disk"`
	Load       float64 `json:"load"`
	CPUs       int     `json:"cpus"`
	Uptime     int64   `json:"uptime"`
	Alerts     int     `json:"alerts"`
	LastSeen   int64   `json:"lastSeen"`
	Error      string  `json:"error"`
}

func (s *MonitorService) Fleet() []FleetItem {
	counts := map[string]int{}
	for _, al := range s.alerts.Active() {
		if al.State == "firing" {
			counts[al.Server]++
		}
	}
	out := []FleetItem{}
	for _, sv := range s.core.Store.List() {
		it := FleetItem{Server: sv.ID, Monitored: sv.Monitor, Alerts: counts[sv.ID]}
		if w := s.worker(sv.ID); w != nil {
			it.Collecting = true
			w.mu.Lock()
			if w.has {
				ev := w.last
				it.Reachable, it.Status, it.OK, it.Total, it.Error = ev.Reachable, ev.Health.Status, ev.Health.OK, ev.Health.Total, ev.Error
				if sn := ev.Snapshot; sn != nil {
					it.CPU, it.Mem, it.Load, it.CPUs, it.Uptime, it.LastSeen = sn.CPU, sn.MemPct, sn.Load[0], sn.CPUs, sn.Uptime, sn.TS
					for _, m := range sn.Mounts {
						if m.Pct > it.Disk {
							it.Disk = m.Pct
						}
					}
				}
			}
			w.mu.Unlock()
		}
		out = append(out, it)
	}
	return out
}

// SetMonitor turns background monitoring on/off for a server.
func (s *MonitorService) SetMonitor(serverID string, on bool) error {
	sv, err := s.core.Store.Get(serverID)
	if err != nil {
		return err
	}
	sv.Monitor = on
	_, err = s.core.Store.Save(sv, "", false)
	action := "monitor.enable"
	if !on {
		action = "monitor.disable"
	}
	s.core.Audit(serverID, action, sv.Name, "", err)
	if err == nil {
		go s.reconcile()
	}
	return err
}

// ---- alerts API ----

func (s *MonitorService) Rules() []Rule { return s.alerts.Rules() }

func (s *MonitorService) SaveRule(r Rule) (Rule, error) {
	isNew := r.ID == ""
	out, err := s.alerts.SaveRule(r)
	action := "alert.rule.update"
	if isNew {
		action = "alert.rule.create"
	}
	s.core.Audit("", action, out.Name, ruleSummary(out), err)
	return out, err
}

func ruleSummary(r Rule) string {
	return r.Metric + " " + r.Op + " " + fmtValue(r.Metric, r.Threshold) + " for " + (time.Duration(r.ForSec) * time.Second).String() + " enabled=" + boolStr(r.Enabled)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (s *MonitorService) DeleteRule(id string) error {
	name := id
	for _, r := range s.alerts.Rules() {
		if r.ID == id {
			name = r.Name
		}
	}
	err := s.alerts.DeleteRule(id)
	s.core.Audit("", "alert.rule.delete", name, "", err)
	return err
}

// RestoreDefaultRules re-adds any missing built-in rule.
func (s *MonitorService) RestoreDefaultRules() error {
	have := map[string]bool{}
	for _, r := range s.alerts.Rules() {
		have[r.ID] = true
	}
	for _, r := range defaultRules() {
		if !have[r.ID] {
			if _, err := s.alerts.SaveRule(r); err != nil {
				return err
			}
		}
	}
	s.core.Audit("", "alert.rule.restoreDefaults", "", "", nil)
	return nil
}

func (s *MonitorService) ActiveAlerts() []Alert { return s.alerts.Active() }

func (s *MonitorService) AlertHistory(serverID string, limit int) ([]Alert, error) {
	return s.alerts.History(serverID, limit)
}

func (s *MonitorService) Ack(key string) error { return s.alerts.Ack(key, s.core.Actor()) }

func (s *MonitorService) AlertCount() AlertCount { return s.alerts.count() }

// Metrics lists the metrics a rule can use with their unit.
func (s *MonitorService) Metrics() map[string]string {
	out := map[string]string{}
	for k, v := range metricInfo {
		u := v.unit
		if v.bool {
			u = "bool"
		}
		out[k] = u
	}
	return out
}

// ---- channels API ----

func (s *MonitorService) Channels() []Channel {
	list := s.notifier.channels()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

func (s *MonitorService) SaveChannel(ch Channel, secret string, setSecret bool) (Channel, error) {
	isNew := ch.ID == ""
	out, err := s.notifier.save(ch, secret, setSecret)
	action := "alert.channel.update"
	if isNew {
		action = "alert.channel.create"
	}
	detail := out.Type
	if setSecret {
		detail += ", secret changed"
	}
	s.core.Audit("", action, out.Name, detail, err)
	return out, err
}

func (s *MonitorService) DeleteChannel(id string) error {
	err := s.notifier.delete(id)
	s.core.Audit("", "alert.channel.delete", id, "", err)
	return err
}

// TestChannel sends a test message. For an unsaved or edited channel pass
// the secret; "" uses the stored one.
func (s *MonitorService) TestChannel(ch Channel, secret string) error {
	if secret != "" {
		tmp := ch
		tmp.ID = "test-" + time.Now().Format("150405.000")
		if err := storeTemp(tmp.ID, secret); err != nil {
			return err
		}
		defer storeTemp(tmp.ID, "")
		return s.notifier.Test(tmp)
	}
	return s.notifier.Test(ch)
}

func storeTemp(id, secret string) error { return store.SetKeychain(secretName(id), secret) }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
