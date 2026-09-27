package monitor

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// Check is one line of a server's health report.
type Check struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`   // cpu | memory | disk | inode | load | swap | network | ssh | service | failed-units | container | http | ssl | backup
	Name   string  `json:"name"`   // target (mount, unit, URL…), "" for server-wide checks
	Status string  `json:"status"` // ok | warn | crit | unknown
	Value  float64 `json:"value"`
	Detail string  `json:"detail"` // raw detail (error text, status code…)
}

// Health answers "is this server really OK right now?".
type Health struct {
	Server    string  `json:"server"`
	TS        int64   `json:"ts"`
	Status    string  `json:"status"` // ok | warn | crit | unknown
	OK        int     `json:"ok"`
	Total     int     `json:"total"`
	Checks    []Check `json:"checks"`
	Reachable bool    `json:"reachable"`
	Error     string  `json:"error"`
}

var sevRank = map[string]int{"ok": 0, "unknown": 1, "warn": 2, "crit": 3}

func worst(a, b string) string {
	if sevRank[b] > sevRank[a] {
		return b
	}
	return a
}

func level(v, warn, crit float64) string {
	switch {
	case v >= crit:
		return "crit"
	case v >= warn:
		return "warn"
	}
	return "ok"
}

// HTTPResult is the latest outcome of an HTTP health check.
type HTTPResult struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	OK       bool   `json:"ok"`
	Status   int    `json:"status"`
	Ms       int64  `json:"ms"`
	Error    string `json:"error"`
	CertDays int    `json:"certDays"` // days until the TLS certificate expires (-1 = n/a)
	At       int64  `json:"at"`
}

// evalHealth builds the report from the latest snapshot and recent history.
func evalHealth(sv store.Server, snap *Snapshot, recent []Point, http []HTTPResult, backup *backupState, reachable bool, errText string, collectMs int64) Health {
	h := Health{Server: sv.ID, TS: time.Now().Unix(), Reachable: reachable, Error: errText, Checks: []Check{}}
	add := func(c Check) { h.Checks = append(h.Checks, c) }
	if !reachable {
		add(Check{ID: "ssh", Kind: "ssh", Status: "crit", Detail: errText})
	} else {
		st := "ok"
		if collectMs > 8000 {
			st = "warn"
		}
		add(Check{ID: "ssh", Kind: "ssh", Status: st, Value: float64(collectMs)})
	}
	if snap != nil && reachable {
		// CPU and load use a 5-minute average so short spikes don't flap.
		cpu, load := snap.CPU, snap.Load[1]
		if len(recent) > 0 {
			var s float64
			for _, p := range recent {
				s += p.CPU
			}
			cpu = s / float64(len(recent))
		}
		if snap.HasRates || len(recent) > 0 {
			add(Check{ID: "cpu", Kind: "cpu", Status: level(cpu, 80, 95), Value: cpu})
		}
		add(Check{ID: "memory", Kind: "memory", Status: level(snap.MemPct, 85, 95), Value: snap.MemPct})
		if snap.SwapTotal > 0 {
			add(Check{ID: "swap", Kind: "swap", Status: level(snap.SwapPct, 60, 90), Value: snap.SwapPct})
		}
		if snap.CPUs > 0 {
			perCore := load / float64(snap.CPUs)
			add(Check{ID: "load", Kind: "load", Status: level(perCore, 1.5, 3), Value: load})
		}
		diskSt, inodeSt := "ok", "ok"
		var diskMax, inodeMax float64
		diskName, inodeName := "", ""
		for _, m := range snap.Mounts {
			if m.Pct > diskMax {
				diskMax, diskName = m.Pct, m.Mount
			}
			if m.InodePct > inodeMax {
				inodeMax, inodeName = m.InodePct, m.Mount
			}
			diskSt = worst(diskSt, level(m.Pct, 85, 95))
			inodeSt = worst(inodeSt, level(m.InodePct, 85, 95))
		}
		if len(snap.Mounts) > 0 {
			add(Check{ID: "disk", Kind: "disk", Name: diskName, Status: diskSt, Value: diskMax})
			if inodeMax > 0 {
				add(Check{ID: "inode", Kind: "inode", Name: inodeName, Status: inodeSt, Value: inodeMax})
			}
		}
		if snap.HasRates {
			bad := snap.NetErrors + snap.NetDrops
			st := "ok"
			if bad > 100 {
				st = "crit"
			} else if bad > 5 {
				st = "warn"
			}
			add(Check{ID: "network", Kind: "network", Status: st, Value: bad})
		}
		units := map[string]string{}
		for _, u := range snap.Units {
			units[u.Name] = u.State
		}
		for _, name := range sv.WatchServices {
			st := units[name]
			status := "ok"
			if st != "active" {
				status = "crit"
			}
			if st == "" {
				status, st = "unknown", "unknown"
			}
			add(Check{ID: "service:" + name, Kind: "service", Name: name, Status: status, Detail: st})
		}
		if len(snap.Failed) > 0 {
			add(Check{ID: "failed-units", Kind: "failed-units", Status: "warn", Value: float64(len(snap.Failed)), Detail: strings.Join(snap.Failed, ", ")})
		}
		for _, c := range snap.Containers {
			st := ""
			switch {
			case c.Health == "unhealthy":
				st = "crit"
			case c.State == "restarting":
				st = "warn"
			}
			if st != "" {
				add(Check{ID: "container:" + c.Name, Kind: "container", Name: c.Name, Status: st, Detail: c.Status})
			}
		}
	}
	for i, r := range http {
		id := "http:" + strconv.Itoa(i)
		st := "ok"
		if !r.OK {
			st = "crit"
		}
		name := r.Name
		if name == "" {
			name = r.URL
		}
		add(Check{ID: id, Kind: "http", Name: name, Status: st, Value: float64(r.Ms), Detail: httpDetail(r)})
		if r.CertDays >= 0 && strings.HasPrefix(r.URL, "https://") {
			add(Check{ID: "ssl:" + r.URL, Kind: "ssl", Name: hostOf(r.URL), Status: sslLevel(r.CertDays), Value: float64(r.CertDays)})
		}
	}
	if backup != nil {
		st := "ok"
		age := backup.ageHours()
		switch {
		case backup.lastFailed:
			st = "crit"
		case age > 72:
			st = "crit"
		case age > 26:
			st = "warn"
		}
		add(Check{ID: "backup", Kind: "backup", Status: st, Value: age, Detail: backup.name})
	}
	h.Status = "ok"
	for _, c := range h.Checks {
		h.Total++
		if c.Status == "ok" {
			h.OK++
		}
		h.Status = worst(h.Status, c.Status)
	}
	return h
}

func sslLevel(days int) string {
	switch {
	case days < 7:
		return "crit"
	case days < 14:
		return "warn"
	}
	return "ok"
}

func httpDetail(r HTTPResult) string {
	if r.Error != "" {
		return r.Error
	}
	return strconv.Itoa(r.Status)
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// ---- HTTP checks ----

var httpClient = &http.Client{
	Timeout: 12 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 8 * time.Second,
		DisableKeepAlives:   true,
	},
}

func expectOK(want, got int) bool {
	if want > 0 {
		return got == want
	}
	return got >= 200 && got < 400
}

// runHTTPCheck performs one check from this computer, or on the server
// with curl/wget when ViaServer is set.
func runHTTPCheck(ctx context.Context, c *core.Core, conn *sshx.Conn, chk store.HTTPCheck) HTTPResult {
	r := HTTPResult{Name: chk.Name, URL: chk.URL, CertDays: -1, At: time.Now().Unix()}
	start := time.Now()
	if chk.ViaServer {
		if conn == nil {
			r.Error = "no connection"
			return r
		}
		script := `u=$1
if command -v curl >/dev/null 2>&1; then
  body=$(curl -sS -k -L --max-time 10 -o - -w '\n@@SM@@%{http_code}' "$u" 2>&1 | tail -c 262144)
else
  body=$(wget -q -S -O - -T 10 "$u" 2>&1 | tail -c 262144); code=$(printf '%s' "$body" | grep -o 'HTTP/[0-9.]* [0-9]*' | tail -n1 | awk '{print $2}'); body="$body
@@SM@@$code"
fi
printf '%s' "$body"`
		res, err := conn.Exec(ctx, "sh -c "+core.Q(script)+" sh "+core.Q(chk.URL), nil)
		r.Ms = time.Since(start).Milliseconds()
		if err != nil {
			r.Error = err.Error()
			return r
		}
		out := res.Stdout
		i := strings.LastIndex(out, "@@SM@@")
		if i < 0 {
			r.Error = strings.TrimSpace(out)
			return r
		}
		r.Status, _ = strconv.Atoi(strings.TrimSpace(out[i+6:]))
		body := out[:i]
		r.OK = expectOK(chk.Expect, r.Status)
		if r.Status == 0 {
			r.Error = firstLine(body)
			r.OK = false
		} else if r.OK && chk.Contains != "" && !strings.Contains(body, chk.Contains) {
			r.OK = false
			r.Error = "body does not contain the expected text"
		}
		return r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chk.URL, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	req.Header.Set("User-Agent", "ServerManager-HealthCheck/1.0")
	resp, err := httpClient.Do(req)
	r.Ms = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = cleanErr(err)
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	r.Status = resp.StatusCode
	r.OK = expectOK(chk.Expect, r.Status)
	if r.OK && chk.Contains != "" && !strings.Contains(string(body), chk.Contains) {
		r.OK = false
		r.Error = "body does not contain the expected text"
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.CertDays = int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
	}
	return r
}

var urlInErr = regexp.MustCompile(`^Get "[^"]*": `)

func cleanErr(err error) string {
	return urlInErr.ReplaceAllString(err.Error(), "")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- backup freshness (from the backup module's run history) ----

type backupState struct {
	name       string
	lastOK     int64 // unix ms
	lastFailed bool
}

func (b *backupState) ageHours() float64 {
	if b.lastOK == 0 {
		return 1e6
	}
	return time.Since(time.UnixMilli(b.lastOK)).Hours()
}

func loadBackupState(c *core.Core, server string) *backupState {
	var n int
	if err := c.DB.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'backup' AND server = ?`, server).Scan(&n); err != nil || n == 0 {
		return nil
	}
	b := &backupState{}
	_ = c.DB.QueryRow(`SELECT COALESCE(MAX(finished), 0) FROM runs WHERE kind = 'backup' AND server = ? AND status IN ('done','success','ok')`, server).Scan(&b.lastOK)
	var status, ref string
	if err := c.DB.QueryRow(`SELECT status, ref FROM runs WHERE kind = 'backup' AND server = ? AND finished > 0 ORDER BY finished DESC LIMIT 1`, server).Scan(&status, &ref); err == nil {
		b.lastFailed = status == "error" || status == "failed"
		b.name = ref
		var job struct {
			Name string `json:"name"`
		}
		if c.DB.Get("backup.job", ref, &job) == nil && job.Name != "" {
			b.name = job.Name
		}
	}
	return b
}

func fmtPct(v float64) string { return fmt.Sprintf("%.0f%%", v) }

// addCertChecks adds an SSL check per installed certificate that isn't
// already covered by an HTTPS health check, and recomputes the totals.
func addCertChecks(h *Health, certs []CertInfo, http []HTTPResult) {
	covered := map[string]bool{}
	for _, r := range http {
		if r.CertDays >= 0 {
			covered[hostOf(r.URL)] = true
		}
	}
	for _, c := range certs {
		if c.NotAfter <= 0 {
			continue
		}
		name := c.Name
		if len(c.Domains) > 0 {
			name = c.Domains[0]
		}
		if covered[name] {
			continue
		}
		covered[name] = true
		days := int(time.Until(time.Unix(c.NotAfter, 0)).Hours() / 24)
		h.Checks = append(h.Checks, Check{ID: "cert:" + c.Name, Kind: "ssl", Name: name, Status: sslLevel(days), Value: float64(days)})
	}
	h.Status, h.OK, h.Total = "ok", 0, 0
	for _, c := range h.Checks {
		h.Total++
		if c.Status == "ok" {
			h.OK++
		}
		h.Status = worst(h.Status, c.Status)
	}
}
