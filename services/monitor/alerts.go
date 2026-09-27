package monitor

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/store"
)

// Rule is an alert rule. It applies to every monitored server matching its
// scope (empty scope lists = all).
type Rule struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Enabled   bool    `json:"enabled"`
	Metric    string  `json:"metric"` // see metricInfo
	Op        string  `json:"op"`     // ">" | "<"
	Threshold float64 `json:"threshold"`
	// ForSec: the condition must hold this long before the alert fires.
	ForSec   int    `json:"forSec"`
	Severity string `json:"severity"` // warn | crit

	Servers      []string `json:"servers"`
	Environments []string `json:"environments"`
	Tags         []string `json:"tags"`
	Groups       []string `json:"groups"`

	Channels []string `json:"channels"`
	// RepeatMin re-notifies while firing and unacknowledged (0 = never).
	RepeatMin      int  `json:"repeatMin"`
	NotifyResolved bool `json:"notifyResolved"`
	Builtin        bool `json:"builtin"`
}

// metricInfo: unit and whether a value is boolean (1 = bad).
var metricInfo = map[string]struct {
	unit string
	bool bool
}{
	"cpu": {"%", false}, "memory": {"%", false}, "swap": {"%", false}, "disk": {"%", false}, "inode": {"%", false},
	"load": {"", false}, "load_per_core": {"", false}, "iowait": {"%", false}, "steal": {"%", false},
	"net_rx": {"Mbps", false}, "net_tx": {"Mbps", false}, "net_errors": {"/s", false}, "processes": {"", false},
	"failed_units": {"", false}, "service_down": {"", true}, "unreachable": {"", true}, "http_down": {"", true},
	"ssl_days": {"d", false}, "container_unhealthy": {"", true}, "backup_age": {"h", false},
}

// Alert is the state of one (rule, server, target) combination.
type Alert struct {
	Key          string  `json:"key"`
	Rule         string  `json:"rule"`
	RuleName     string  `json:"ruleName"`
	Metric       string  `json:"metric"`
	Op           string  `json:"op"`
	Threshold    float64 `json:"threshold"`
	Server       string  `json:"server"`
	ServerName   string  `json:"serverName"`
	Target       string  `json:"target"`
	State        string  `json:"state"` // pending | firing | resolved
	Severity     string  `json:"severity"`
	Value        float64 `json:"value"`
	Since        int64   `json:"since"` // unix ms: condition first true
	Fired        int64   `json:"fired"`
	Resolved     int64   `json:"resolved"`
	Acked        int64   `json:"acked"`
	AckedBy      string  `json:"ackedBy"`
	LastNotified int64   `json:"lastNotified"`
	okCount      int
}

type alertMsg struct {
	RuleName  string  `json:"ruleName"`
	Metric    string  `json:"metric"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
	Name      string  `json:"serverName"`
}

func defaultRules() []Rule {
	r := func(name, metric, op string, th float64, forSec int, sev string) Rule {
		return Rule{ID: "builtin-" + metric + "-" + strings.ReplaceAll(fmt.Sprint(th), ".", "_"), Name: name, Enabled: true, Metric: metric, Op: op,
			Threshold: th, ForSec: forSec, Severity: sev, RepeatMin: 240, NotifyResolved: true, Builtin: true,
			Servers: []string{}, Environments: []string{}, Tags: []string{}, Groups: []string{}, Channels: []string{}}
	}
	return []Rule{
		r("CPU > 85% (5m)", "cpu", ">", 85, 300, "warn"),
		r("RAM > 90% (5m)", "memory", ">", 90, 300, "warn"),
		r("Disk > 80%", "disk", ">", 80, 0, "warn"),
		r("Disk > 90%", "disk", ">", 90, 0, "crit"),
		r("Load / core > 2 (5m)", "load_per_core", ">", 2, 300, "warn"),
		r("Service down", "service_down", ">", 0, 60, "crit"),
		r("Server unreachable", "unreachable", ">", 0, 60, "crit"),
		r("SSL expires < 14 days", "ssl_days", "<", 14, 0, "warn"),
		r("HTTP health check failed", "http_down", ">", 0, 120, "crit"),
		r("Container unhealthy", "container_unhealthy", ">", 0, 120, "warn"),
		r("Backup older than 26h", "backup_age", ">", 26, 0, "warn"),
	}
}

type alerting struct {
	c        *core.Core
	notifier *notifier

	mu     sync.Mutex
	rules  []Rule
	alerts map[string]*Alert
}

func newAlerting(c *core.Core, n *notifier) *alerting {
	a := &alerting{c: c, notifier: n, alerts: map[string]*Alert{}}
	a.loadRules()
	a.loadAlerts()
	return a
}

func (a *alerting) loadRules() {
	rules, err := db.List[Rule](a.c.DB, "mon.rule")
	if err == nil && len(rules) == 0 {
		var seeded bool
		if a.c.DB.Get("mon.meta", "rulesSeeded", &seeded) != nil {
			for _, r := range defaultRules() {
				_ = a.c.DB.Put("mon.rule", r.ID, r)
			}
			_ = a.c.DB.Put("mon.meta", "rulesSeeded", true)
			rules, _ = db.List[Rule](a.c.DB, "mon.rule")
		}
	}
	a.mu.Lock()
	a.rules = rules
	a.mu.Unlock()
}

func (a *alerting) loadAlerts() {
	rows, err := a.c.DB.Query(`SELECT key, rule, server, target, state, severity, value, message, since, fired, resolved, acked, acked_by, last_notified FROM alerts WHERE state != 'resolved'`)
	if err != nil {
		return
	}
	defer rows.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	for rows.Next() {
		al, err := scanAlert(rows)
		if err == nil {
			a.alerts[al.Key] = al
		}
	}
}

func scanAlert(rows interface{ Scan(...any) error }) (*Alert, error) {
	var al Alert
	var msg string
	if err := rows.Scan(&al.Key, &al.Rule, &al.Server, &al.Target, &al.State, &al.Severity, &al.Value, &msg, &al.Since, &al.Fired, &al.Resolved, &al.Acked, &al.AckedBy, &al.LastNotified); err != nil {
		return nil, err
	}
	var m alertMsg
	_ = json.Unmarshal([]byte(msg), &m)
	al.RuleName, al.Metric, al.Op, al.Threshold, al.ServerName = m.RuleName, m.Metric, m.Op, m.Threshold, m.Name
	return &al, nil
}

func (a *alerting) save(al *Alert) {
	msg, _ := json.Marshal(alertMsg{RuleName: al.RuleName, Metric: al.Metric, Op: al.Op, Threshold: al.Threshold, Name: al.ServerName})
	_, err := a.c.DB.Exec(`INSERT INTO alerts(key, rule, server, target, state, severity, value, message, since, fired, resolved, acked, acked_by, last_notified)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET state=excluded.state, severity=excluded.severity, value=excluded.value, message=excluded.message,
		since=excluded.since, fired=excluded.fired, resolved=excluded.resolved, acked=excluded.acked, acked_by=excluded.acked_by, last_notified=excluded.last_notified`,
		al.Key, al.Rule, al.Server, al.Target, al.State, al.Severity, al.Value, string(msg), al.Since, al.Fired, al.Resolved, al.Acked, al.AckedBy, al.LastNotified)
	if err != nil {
		fmt.Println("alert save:", err)
	}
}

// Rules returns a copy of the rules.
func (a *alerting) Rules() []Rule {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rules)
}

func (a *alerting) SaveRule(r Rule) (Rule, error) {
	r.Name = strings.TrimSpace(r.Name)
	if _, ok := metricInfo[r.Metric]; !ok {
		return r, apperr.New("mon.invalidMetric", "metric", r.Metric)
	}
	if r.Op != "<" {
		r.Op = ">"
	}
	if r.Severity != "crit" {
		r.Severity = "warn"
	}
	if r.ForSec < 0 || r.ForSec > 86400 {
		return r, apperr.New("mon.invalidDuration")
	}
	if r.RepeatMin < 0 {
		r.RepeatMin = 0
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return r, apperr.New("mon.invalidThreshold")
	}
	for _, l := range []*[]string{&r.Servers, &r.Environments, &r.Tags, &r.Groups, &r.Channels} {
		if *l == nil {
			*l = []string{}
		}
	}
	if r.Name == "" {
		r.Name = r.Metric
	}
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if err := a.c.DB.Put("mon.rule", r.ID, r); err != nil {
		return r, err
	}
	a.loadRules()
	return r, nil
}

func (a *alerting) DeleteRule(id string) error {
	if err := a.c.DB.Delete("mon.rule", id); err != nil {
		return err
	}
	a.loadRules()
	// Resolve its alerts silently.
	a.mu.Lock()
	for k, al := range a.alerts {
		if al.Rule == id {
			al.State, al.Resolved = "resolved", time.Now().UnixMilli()
			a.save(al)
			delete(a.alerts, k)
		}
	}
	a.mu.Unlock()
	return nil
}

func inScope(r Rule, sv store.Server) bool {
	if len(r.Servers) > 0 && !slices.Contains(r.Servers, sv.ID) {
		return false
	}
	if len(r.Environments) > 0 && !slices.Contains(r.Environments, sv.Environment) {
		return false
	}
	if len(r.Groups) > 0 && !slices.Contains(r.Groups, sv.Group) {
		return false
	}
	if len(r.Tags) > 0 {
		hit := false
		for _, t := range sv.Tags {
			if slices.Contains(r.Tags, t) {
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

type observation struct {
	target string
	value  float64
}

// observe extracts a rule's metric values from the server state.
func observe(metric string, snap *Snapshot, h Health, recent []Point, http []HTTPResult, backup *backupState, reachable bool) []observation {
	return observeCerts(metric, snap, h, recent, http, backup, reachable, nil)
}

func observeCerts(metric string, snap *Snapshot, h Health, recent []Point, http []HTTPResult, backup *backupState, reachable bool, certs []CertInfo) []observation {
	if metric == "unreachable" {
		v := 0.0
		if !reachable {
			v = 1
		}
		return []observation{{"", v}}
	}
	if !reachable {
		return nil // no data; the unreachable rule covers it
	}
	var out []observation
	switch metric {
	case "cpu", "iowait", "steal":
		if snap == nil || !snap.HasRates {
			return nil
		}
		v := map[string]float64{"cpu": snap.CPU, "iowait": snap.IOWait, "steal": snap.Steal}[metric]
		out = append(out, observation{"", v})
	case "memory":
		if snap != nil {
			out = append(out, observation{"", snap.MemPct})
		}
	case "swap":
		if snap != nil && snap.SwapTotal > 0 {
			out = append(out, observation{"", snap.SwapPct})
		}
	case "disk", "inode":
		if snap != nil {
			for _, m := range snap.Mounts {
				v := m.Pct
				if metric == "inode" {
					v = m.InodePct
				}
				out = append(out, observation{m.Mount, v})
			}
		}
	case "load":
		if snap != nil {
			out = append(out, observation{"", snap.Load[0]})
		}
	case "load_per_core":
		if snap != nil && snap.CPUs > 0 {
			out = append(out, observation{"", snap.Load[0] / float64(snap.CPUs)})
		}
	case "net_rx", "net_tx":
		if snap != nil && snap.HasRates {
			v := snap.NetRx
			if metric == "net_tx" {
				v = snap.NetTx
			}
			out = append(out, observation{"", v * 8 / 1e6})
		}
	case "net_errors":
		if snap != nil && snap.HasRates {
			out = append(out, observation{"", snap.NetErrors + snap.NetDrops})
		}
	case "processes":
		if snap != nil {
			out = append(out, observation{"", float64(snap.Procs)})
		}
	case "failed_units":
		if snap != nil {
			out = append(out, observation{"", float64(len(snap.Failed))})
		}
	case "service_down":
		for _, c := range h.Checks {
			if c.Kind == "service" && c.Status != "unknown" {
				v := 0.0
				if c.Status != "ok" {
					v = 1
				}
				out = append(out, observation{c.Name, v})
			}
		}
	case "container_unhealthy":
		if snap != nil {
			for _, c := range snap.Containers {
				v := 0.0
				if c.Health == "unhealthy" {
					v = 1
				}
				out = append(out, observation{c.Name, v})
			}
		}
	case "http_down":
		for _, r := range http {
			v := 0.0
			if !r.OK {
				v = 1
			}
			out = append(out, observation{r.URL, v})
		}
	case "ssl_days":
		seen := map[string]bool{}
		for _, r := range http {
			if r.CertDays >= 0 {
				seen[hostOf(r.URL)] = true
				out = append(out, observation{hostOf(r.URL), float64(r.CertDays)})
			}
		}
		for _, c := range certs {
			name := c.Name
			if len(c.Domains) > 0 {
				name = c.Domains[0]
			}
			if c.NotAfter > 0 && !seen[name] {
				seen[name] = true
				out = append(out, observation{name, time.Until(time.Unix(c.NotAfter, 0)).Hours() / 24})
			}
		}
	case "backup_age":
		if backup != nil {
			out = append(out, observation{backup.name, backup.ageHours()})
		}
	}
	return out
}

func breach(r Rule, v float64) bool {
	if r.Op == "<" {
		return v < r.Threshold
	}
	return v > r.Threshold
}

// evaluate updates alert states for one server after a collection.
func (a *alerting) evaluate(sv store.Server, snap *Snapshot, h Health, recent []Point, http []HTTPResult, backup *backupState, reachable bool, certs ...[]CertInfo) {
	var cl []CertInfo
	if len(certs) > 0 {
		cl = certs[0]
	}
	now := time.Now().UnixMilli()
	a.mu.Lock()
	rules := slices.Clone(a.rules)
	a.mu.Unlock()
	seen := map[string]bool{}
	var fire, resolve []*Alert
	for _, r := range rules {
		if !r.Enabled || !inScope(r, sv) {
			continue
		}
		for _, o := range observeCerts(r.Metric, snap, h, recent, http, backup, reachable, cl) {
			key := r.ID + "|" + sv.ID + "|" + o.target
			seen[key] = true
			bad := breach(r, o.value)
			a.mu.Lock()
			al := a.alerts[key]
			switch {
			case bad && al == nil:
				al = &Alert{Key: key, Rule: r.ID, RuleName: r.Name, Metric: r.Metric, Op: r.Op, Threshold: r.Threshold,
					Server: sv.ID, ServerName: sv.Name, Target: o.target, State: "pending", Severity: r.Severity, Value: o.value, Since: now}
				a.alerts[key] = al
				if r.ForSec == 0 {
					al.State, al.Fired = "firing", now
					fire = append(fire, al)
				}
				a.save(al)
			case bad:
				al.Value, al.okCount = o.value, 0
				al.RuleName, al.Severity, al.Threshold, al.Op = r.Name, r.Severity, r.Threshold, r.Op
				if al.State == "pending" && now-al.Since >= int64(r.ForSec)*1000 {
					al.State, al.Fired = "firing", now
					fire = append(fire, al)
				} else if al.State == "firing" && al.Acked == 0 && r.RepeatMin > 0 && now-al.LastNotified >= int64(r.RepeatMin)*60000 {
					fire = append(fire, al) // reminder
				}
				a.save(al)
			case al != nil:
				// Hysteresis: two good observations in a row before resolving.
				al.okCount++
				al.Value = o.value
				if al.State == "pending" {
					delete(a.alerts, key)
					_, _ = a.c.DB.Exec(`DELETE FROM alerts WHERE key = ?`, key)
				} else if al.okCount >= 2 {
					al.State, al.Resolved = "resolved", now
					a.save(al)
					delete(a.alerts, key)
					resolve = append(resolve, al)
				}
			}
			a.mu.Unlock()
		}
	}
	// Targets that disappeared (service unwatched, container removed, mount
	// gone) resolve their alerts; while unreachable nothing is observed, so
	// keep those alerts as they are.
	if reachable {
		a.mu.Lock()
		for key, al := range a.alerts {
			if al.Server != sv.ID || seen[key] || al.Metric == "unreachable" {
				continue
			}
			if !slices.ContainsFunc(rules, func(r Rule) bool { return r.ID == al.Rule && r.Enabled && inScope(r, sv) }) || al.State == "pending" || observedMetric(al.Metric, http, backup) {
				delete(a.alerts, key)
				if al.State == "firing" {
					al.State, al.Resolved = "resolved", now
					a.save(al)
					resolve = append(resolve, al)
				} else {
					_, _ = a.c.DB.Exec(`DELETE FROM alerts WHERE key = ?`, key)
				}
			}
		}
		a.mu.Unlock()
	}
	for _, al := range fire {
		a.onFire(al, rules)
	}
	for _, al := range resolve {
		a.onResolve(al, rules)
	}
	if len(fire) > 0 || len(resolve) > 0 {
		core.Emit(EventAlerts, a.count())
	}
}

// observedMetric: metrics whose targets are all enumerated every sample,
// so a missing target really disappeared.
func observedMetric(m string, http []HTTPResult, backup *backupState) bool {
	switch m {
	case "disk", "inode", "service_down", "container_unhealthy", "http_down", "ssl_days":
		return true
	case "backup_age":
		return backup == nil
	}
	return false
}

func ruleByID(rules []Rule, id string) *Rule {
	for i := range rules {
		if rules[i].ID == id {
			return &rules[i]
		}
	}
	return nil
}

func (a *alerting) onFire(al *Alert, rules []Rule) {
	r := ruleByID(rules, al.Rule)
	reminder := al.LastNotified > 0
	al.LastNotified = time.Now().UnixMilli()
	a.mu.Lock()
	a.save(al)
	a.mu.Unlock()
	if !reminder {
		a.c.AddEvent(core.Event{Server: al.Server, Kind: "alert", Severity: al.Severity, Code: "alert.firing",
			Params: map[string]string{"rule": al.RuleName, "target": al.Target, "value": fmtValue(al.Metric, al.Value)}})
	}
	if r != nil && a.suppressed(al) {
		return
	}
	if r != nil {
		a.notifier.send(r.Channels, notification{Alert: *al, Kind: map[bool]string{true: "reminder", false: "firing"}[reminder]})
	}
}

func (a *alerting) onResolve(al *Alert, rules []Rule) {
	a.c.AddEvent(core.Event{Server: al.Server, Kind: "alert", Severity: "ok", Code: "alert.resolved",
		Params: map[string]string{"rule": al.RuleName, "target": al.Target, "value": fmtValue(al.Metric, al.Value)}})
	r := ruleByID(rules, al.Rule)
	if r != nil && r.NotifyResolved && al.Fired > 0 && al.LastNotified > 0 {
		a.notifier.send(r.Channels, notification{Alert: *al, Kind: "resolved"})
	}
}

// suppressed: while a server is unreachable only that alert notifies, so a
// dead server produces one message instead of dozens.
func (a *alerting) suppressed(al *Alert) bool {
	if al.Metric == "unreachable" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.alerts {
		if x.Server == al.Server && x.Metric == "unreachable" && x.State == "firing" {
			return true
		}
	}
	return false
}

func (a *alerting) Active() []Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []Alert{}
	for _, al := range a.alerts {
		out = append(out, *al)
	}
	slices.SortFunc(out, func(x, y Alert) int {
		if sevRank[x.Severity] != sevRank[y.Severity] {
			return sevRank[y.Severity] - sevRank[x.Severity]
		}
		return int(y.Since - x.Since)
	})
	return out
}

// AlertCount is pushed to the UI for the sidebar badge.
type AlertCount struct {
	Firing   int `json:"firing"`
	Unacked  int `json:"unacked"`
	Critical int `json:"critical"`
}

func (a *alerting) count() AlertCount {
	a.mu.Lock()
	defer a.mu.Unlock()
	var c AlertCount
	for _, al := range a.alerts {
		if al.State != "firing" {
			continue
		}
		c.Firing++
		if al.Acked == 0 {
			c.Unacked++
		}
		if al.Severity == "crit" {
			c.Critical++
		}
	}
	return c
}

func (a *alerting) Ack(key, actor string) error {
	a.mu.Lock()
	al, ok := a.alerts[key]
	if !ok {
		a.mu.Unlock()
		return apperr.New("mon.alertNotFound")
	}
	al.Acked, al.AckedBy = time.Now().UnixMilli(), actor
	a.save(al)
	cp := *al
	a.mu.Unlock()
	a.c.Audit(cp.Server, "alert.ack", cp.RuleName, cp.Target, nil)
	core.Emit(EventAlerts, a.count())
	return nil
}

// History returns resolved alerts, newest first.
func (a *alerting) History(server string, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT key, rule, server, target, state, severity, value, message, since, fired, resolved, acked, acked_by, last_notified FROM alerts WHERE state = 'resolved'`
	args := []any{}
	if server != "" {
		q += ` AND server = ?`
		args = append(args, server)
	}
	q += ` ORDER BY resolved DESC LIMIT ?`
	rows, err := a.c.DB.Query(q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		al, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *al)
	}
	return out, rows.Err()
}

// forgetServer drops alerts of a deleted server.
func (a *alerting) forgetServer(id string) {
	a.mu.Lock()
	for k, al := range a.alerts {
		if al.Server == id {
			delete(a.alerts, k)
		}
	}
	a.mu.Unlock()
	_, _ = a.c.DB.Exec(`DELETE FROM alerts WHERE server = ?`, id)
}

func (a *alerting) prune() {
	_, _ = a.c.DB.Exec(`DELETE FROM alerts WHERE state = 'resolved' AND resolved < ?`, time.Now().AddDate(0, 0, -90).UnixMilli())
}

func fmtValue(metric string, v float64) string {
	info := metricInfo[metric]
	if info.bool {
		return ""
	}
	switch {
	case math.Abs(v) >= 100:
		return fmt.Sprintf("%.0f%s", v, info.unit)
	case math.Abs(v) >= 10:
		return fmt.Sprintf("%.1f%s", v, info.unit)
	}
	return fmt.Sprintf("%.2f%s", v, info.unit)
}
