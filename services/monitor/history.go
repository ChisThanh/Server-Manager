package monitor

import (
	"encoding/json"
	"sync"
	"time"

	"server-manager/internal/db"
)

// Point is one stored sample of the headline metrics. Raw points are kept
// for 2 days, 1-minute averages for 8 days and 5-minute averages for 35
// days, so any range from 5 minutes to 30 days can be charted.
type Point struct {
	TS        int64   `json:"t"`
	CPU       float64 `json:"cpu"`
	CPUMax    float64 `json:"cpuMax"`
	IOWait    float64 `json:"iow"`
	Steal     float64 `json:"st"`
	Load1     float64 `json:"l1"`
	Load5     float64 `json:"l5"`
	Load15    float64 `json:"l15"`
	MemPct    float64 `json:"mem"`
	MemUsed   float64 `json:"memU"`
	MemCache  float64 `json:"memC"`
	SwapPct   float64 `json:"swap"`
	DiskPct   float64 `json:"disk"` // fullest mount
	InodePct  float64 `json:"inode"`
	DiskRead  float64 `json:"dr"`
	DiskWrite float64 `json:"dw"`
	IOPS      float64 `json:"iops"`
	DiskLat   float64 `json:"lat"`
	NetRx     float64 `json:"rx"`
	NetTx     float64 `json:"tx"`
	NetErr    float64 `json:"nerr"`
	NetDrop   float64 `json:"ndrop"`
	Procs     float64 `json:"procs"`
}

func pointOf(s Snapshot) Point {
	p := Point{TS: s.TS, CPU: s.CPU, CPUMax: s.CPU, IOWait: s.IOWait, Steal: s.Steal, Load1: s.Load[0], Load5: s.Load[1], Load15: s.Load[2],
		MemPct: s.MemPct, MemUsed: float64(s.MemUsed), MemCache: float64(s.MemCache), SwapPct: s.SwapPct,
		DiskRead: s.DiskRead, DiskWrite: s.DiskWrite, IOPS: s.IOPS, DiskLat: s.DiskLat,
		NetRx: s.NetRx, NetTx: s.NetTx, NetErr: s.NetErrors, NetDrop: s.NetDrops, Procs: float64(s.Procs)}
	for _, m := range s.Mounts {
		if m.Pct > p.DiskPct {
			p.DiskPct = m.Pct
		}
		if m.InodePct > p.InodePct {
			p.InodePct = m.InodePct
		}
	}
	return p
}

// fields lists every averaged value of a Point (by pointer) so rollups and
// the columnar query result can be written generically.
func fields(p *Point) []*float64 {
	return []*float64{&p.CPU, &p.CPUMax, &p.IOWait, &p.Steal, &p.Load1, &p.Load5, &p.Load15, &p.MemPct, &p.MemUsed, &p.MemCache,
		&p.SwapPct, &p.DiskPct, &p.InodePct, &p.DiskRead, &p.DiskWrite, &p.IOPS, &p.DiskLat, &p.NetRx, &p.NetTx, &p.NetErr, &p.NetDrop, &p.Procs}
}

var fieldNames = []string{"cpu", "cpuMax", "iowait", "steal", "load1", "load5", "load15", "memPct", "memUsed", "memCache",
	"swapPct", "diskPct", "inodePct", "diskRead", "diskWrite", "iops", "diskLat", "netRx", "netTx", "netErr", "netDrop", "procs"}

const (
	resRaw = 0
	res1m  = 60
	res5m  = 300
)

var retention = map[int]time.Duration{resRaw: 48 * time.Hour, res1m: 8 * 24 * time.Hour, res5m: 35 * 24 * time.Hour}

// bucket accumulates points for one rollup interval.
type bucket struct {
	start int64
	n     int
	sum   Point
}

func (b *bucket) add(p Point) {
	b.n++
	sf, pf := fields(&b.sum), fields(&p)
	for i := range sf {
		*sf[i] += *pf[i]
	}
}

// history writes points and rollups for all servers.
type history struct {
	db  *db.DB
	mu  sync.Mutex
	bk  map[string]map[int]*bucket // server → res → open bucket
	max map[string]map[int]float64 // server → res → CPU max within the bucket
}

func newHistory(d *db.DB) *history {
	return &history{db: d, bk: map[string]map[int]*bucket{}, max: map[string]map[int]float64{}}
}

func (h *history) add(server string, p Point) {
	h.write(server, resRaw, p)
	h.mu.Lock()
	if h.bk[server] == nil {
		h.bk[server] = map[int]*bucket{}
		h.max[server] = map[int]float64{}
	}
	var flush []struct {
		res int
		p   Point
	}
	for _, res := range []int{res1m, res5m} {
		start := p.TS - p.TS%int64(res)
		b := h.bk[server][res]
		if b != nil && b.start != start && b.n > 0 {
			avg := b.sum
			for _, f := range fields(&avg) {
				*f /= float64(b.n)
			}
			avg.CPUMax = h.max[server][res]
			avg.TS = b.start
			flush = append(flush, struct {
				res int
				p   Point
			}{res, avg})
			b = nil
		}
		if b == nil {
			b = &bucket{start: start}
			h.bk[server][res] = b
			h.max[server][res] = 0
		}
		b.add(p)
		if p.CPU > h.max[server][res] {
			h.max[server][res] = p.CPU
		}
	}
	h.mu.Unlock()
	for _, f := range flush {
		h.write(server, f.res, f.p)
	}
}

func (h *history) write(server string, res int, p Point) {
	b, _ := json.Marshal(p)
	_, _ = h.db.Exec(`INSERT OR REPLACE INTO metrics(server, res, ts, data) VALUES(?,?,?,?)`, server, res, p.TS, string(b))
}

func (h *history) prune() {
	now := time.Now()
	for res, keep := range retention {
		_, _ = h.db.Exec(`DELETE FROM metrics WHERE res = ? AND ts < ?`, res, now.Add(-keep).Unix())
	}
}

func (h *history) forget(server string) {
	h.mu.Lock()
	delete(h.bk, server)
	delete(h.max, server)
	h.mu.Unlock()
	_, _ = h.db.Exec(`DELETE FROM metrics WHERE server = ?`, server)
}

// Series is a columnar time series: TS[i] with Values[field][i].
type Series struct {
	Res    int                  `json:"res"`
	TS     []int64              `json:"ts"`
	Values map[string][]float64 `json:"values"`
}

// query returns points in [from, to] at the finest resolution that keeps the
// result under ~2000 points and still covers the range.
func (h *history) query(server string, from, to int64) (Series, error) {
	span := to - from
	res := resRaw
	switch {
	case span > 3*86400:
		res = res5m
	case span > 6*3600:
		res = res1m
	}
	// Raw data only goes back 48h, 1-minute data 8 days.
	now := time.Now().Unix()
	if res == resRaw && from < now-int64(retention[resRaw].Seconds()) {
		res = res1m
	}
	if res == res1m && from < now-int64(retention[res1m].Seconds()) {
		res = res5m
	}
	out := Series{Res: res, TS: []int64{}, Values: map[string][]float64{}}
	for _, n := range fieldNames {
		out.Values[n] = []float64{}
	}
	rows, err := h.db.Query(`SELECT data FROM metrics WHERE server = ? AND res = ? AND ts BETWEEN ? AND ? ORDER BY ts`, server, res, from, to)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return out, err
		}
		var p Point
		if json.Unmarshal([]byte(s), &p) != nil {
			continue
		}
		out.TS = append(out.TS, p.TS)
		for i, f := range fields(&p) {
			out.Values[fieldNames[i]] = append(out.Values[fieldNames[i]], *f)
		}
	}
	// Include the still-open bucket so recent minutes aren't missing.
	if res != resRaw {
		h.mu.Lock()
		if b := h.bk[server][res]; b != nil && b.n > 0 && b.start >= from && b.start <= to && (len(out.TS) == 0 || out.TS[len(out.TS)-1] < b.start) {
			avg := b.sum
			for _, f := range fields(&avg) {
				*f /= float64(b.n)
			}
			avg.CPUMax = h.max[server][res]
			out.TS = append(out.TS, b.start)
			for i, f := range fields(&avg) {
				out.Values[fieldNames[i]] = append(out.Values[fieldNames[i]], *f)
			}
		}
		h.mu.Unlock()
	}
	return out, rows.Err()
}

// recent returns the raw points of the last d, oldest first.
func (h *history) recent(server string, d time.Duration) []Point {
	rows, err := h.db.Query(`SELECT data FROM metrics WHERE server = ? AND res = 0 AND ts >= ? ORDER BY ts`, server, time.Now().Add(-d).Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			var p Point
			if json.Unmarshal([]byte(s), &p) == nil {
				out = append(out, p)
			}
		}
	}
	return out
}
