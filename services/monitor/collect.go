package monitor

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// collectScript gathers every metric in one SSH exec using only /proc and
// standard tools. Rates (CPU %, disk and network throughput) are computed
// from the difference between two consecutive samples, so nothing sleeps
// on the server. Watched units are appended by the collector.
const collectScript = `
echo @@t; date +%s
echo @@uptime; cat /proc/uptime 2>/dev/null
echo @@load; cat /proc/loadavg 2>/dev/null
echo @@cpus; nproc 2>/dev/null || grep -c '^processor' /proc/cpuinfo
echo @@stat; grep '^cpu' /proc/stat 2>/dev/null
echo @@mem; cat /proc/meminfo 2>/dev/null
echo @@df; df -kP 2>/dev/null
echo @@dfi; df -iP 2>/dev/null
echo @@blocks; ls /sys/block 2>/dev/null
echo @@diskstats; cat /proc/diskstats 2>/dev/null
echo @@net; cat /proc/net/dev 2>/dev/null
if ps -eo pid,user,pcpu,pmem,rss,comm --sort=-pcpu >/dev/null 2>&1; then
echo @@pscpu; ps -eo pid,user,pcpu,pmem,rss,comm --sort=-pcpu 2>/dev/null | head -n 11
echo @@psmem; ps -eo pid,user,pcpu,pmem,rss,comm --sort=-rss 2>/dev/null | head -n 11
else
echo @@pstat; cat /proc/[0-9]*/stat 2>/dev/null
fi
echo @@failed; command -v systemctl >/dev/null 2>&1 && systemctl list-units --state=failed --no-legend --plain --no-pager 2>/dev/null | head -n 50
if command -v docker >/dev/null 2>&1 && dk=$(docker ps -a --format '{{.Names}}	{{.State}}	{{.Status}}' 2>/dev/null); then echo @@docker; printf '%s\n' "$dk" | head -n 200; fi
`

// ---- raw sample (parsed output) ----

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal float64
}

func (c cpuTimes) total() float64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

type diskCounters struct {
	reads, writes, rsect, wsect, rms, wms, ioticks float64
}

type netCounters struct {
	rxBytes, rxPkts, rxErr, rxDrop, txBytes, txPkts, txErr, txDrop float64
}

// raw holds counters from one collection; two raws make a Snapshot.
type raw struct {
	ts      float64
	uptime  float64
	load    [3]float64
	running int
	procs   int
	cpus    int
	cpu     cpuTimes
	cores   []cpuTimes
	mem     map[string]int64
	mounts  []Mount
	disks   map[string]diskCounters
	nets    map[string]netCounters
	topCPU  []Proc
	topMem  []Proc
	// From /proc/*/stat when ps can't sort (BusyBox): pid → ticks, for
	// computing per-process CPU between samples.
	pticks  map[int]float64
	pstat   []Proc
	failed  []string
	units   map[string]string
	docker  []Container
	hasDock bool
}

func sections(out string) map[string][]string {
	m := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@@") {
			cur = strings.TrimSpace(line[2:])
			if _, ok := m[cur]; !ok {
				m[cur] = []string{}
			}
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			m[cur] = append(m[cur], line)
		}
	}
	return m
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

var skipFS = regexp.MustCompile(`^(tmpfs|devtmpfs|udev|overlay|none|shm|run|efivarfs|squashfs|cgroup|proc|sysfs)`)

func skipMount(fs, mount string) bool {
	if mount == "/" {
		return false
	}
	return skipFS.MatchString(fs) || strings.HasPrefix(mount, "/etc/") || strings.HasPrefix(mount, "/snap/") || strings.HasPrefix(mount, "/sys") ||
		strings.HasPrefix(mount, "/proc") || strings.HasPrefix(mount, "/dev") || strings.HasPrefix(mount, "/run") ||
		strings.HasPrefix(mount, "/var/lib/docker/") || strings.HasPrefix(mount, "/boot/efi")
}

func parseRaw(out string) raw {
	s := sections(out)
	first := func(k string) string {
		if v := s[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	r := raw{mem: map[string]int64{}, disks: map[string]diskCounters{}, nets: map[string]netCounters{}, units: map[string]string{}}
	r.ts = atof(first("t"))
	if f := strings.Fields(first("uptime")); len(f) > 0 {
		r.uptime = atof(f[0])
	}
	if f := strings.Fields(first("load")); len(f) >= 4 {
		for i := 0; i < 3; i++ {
			r.load[i] = atof(f[i])
		}
		if a, b, ok := strings.Cut(f[3], "/"); ok {
			r.running, _ = strconv.Atoi(a)
			r.procs, _ = strconv.Atoi(b)
		}
	}
	r.cpus, _ = strconv.Atoi(first("cpus"))
	for _, l := range s["stat"] {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		v := make([]float64, 8)
		for i := 0; i < 8 && i+1 < len(f); i++ {
			v[i] = atof(f[i+1])
		}
		ct := cpuTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
		if f[0] == "cpu" {
			r.cpu = ct
		} else {
			r.cores = append(r.cores, ct)
		}
	}
	if r.cpus == 0 {
		r.cpus = len(r.cores)
	}
	for _, l := range s["mem"] {
		f := strings.Fields(l)
		if len(f) >= 2 {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			r.mem[strings.TrimSuffix(f[0], ":")] = v * 1024
		}
	}
	inodes := map[string][2]int64{}
	for i, l := range s["dfi"] {
		f := strings.Fields(l)
		if i == 0 || len(f) < 6 {
			continue
		}
		total, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseInt(f[2], 10, 64)
		inodes[strings.Join(f[5:], " ")] = [2]int64{total, used}
	}
	seen := map[string]bool{}
	// "/" first so a device mounted several times is reported as "/".
	dfLines := append([]string(nil), s["df"]...)
	sort.SliceStable(dfLines, func(i, j int) bool {
		fi, fj := strings.Fields(dfLines[i]), strings.Fields(dfLines[j])
		return len(fi) >= 6 && fi[len(fi)-1] == "/" && !(len(fj) >= 6 && fj[len(fj)-1] == "/")
	})
	for i, l := range dfLines {
		f := strings.Fields(l)
		if len(f) < 6 || f[1] == "1024-blocks" || f[0] == "Filesystem" {
			continue
		}
		_ = i
		mount := strings.Join(f[5:], " ")
		if skipMount(f[0], mount) || seen[f[0]] {
			continue
		}
		total, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseInt(f[2], 10, 64)
		avail, _ := strconv.ParseInt(f[3], 10, 64)
		if total == 0 {
			continue
		}
		seen[f[0]] = true
		m := Mount{FS: f[0], Mount: mount, Total: total * 1024, Used: used * 1024, Avail: avail * 1024}
		// Percent the way df reports it: used / (used + avail).
		if used+avail > 0 {
			m.Pct = float64(used) / float64(used+avail) * 100
		}
		if in, ok := inodes[mount]; ok && in[0] > 0 {
			m.InodesTotal, m.InodesUsed = in[0], in[1]
			m.InodePct = float64(in[1]) / float64(in[0]) * 100
		}
		r.mounts = append(r.mounts, m)
	}
	blocks := map[string]bool{}
	for _, b := range s["blocks"] {
		for _, n := range strings.Fields(b) {
			if !strings.HasPrefix(n, "loop") && !strings.HasPrefix(n, "ram") && !strings.HasPrefix(n, "zram") && !strings.HasPrefix(n, "sr") {
				blocks[n] = true
			}
		}
	}
	for _, l := range s["diskstats"] {
		f := strings.Fields(l)
		if len(f) < 14 || !blocks[f[2]] {
			continue
		}
		r.disks[f[2]] = diskCounters{reads: atof(f[3]), rsect: atof(f[5]), rms: atof(f[6]), writes: atof(f[7]), wsect: atof(f[9]), wms: atof(f[10]), ioticks: atof(f[12])}
	}
	for _, l := range s["net"] {
		name, rest, ok := strings.Cut(l, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		r.nets[name] = netCounters{rxBytes: atof(f[0]), rxPkts: atof(f[1]), rxErr: atof(f[2]), rxDrop: atof(f[3]),
			txBytes: atof(f[8]), txPkts: atof(f[9]), txErr: atof(f[10]), txDrop: atof(f[11])}
	}
	r.topCPU = parsePS(s["pscpu"])
	r.topMem = parsePS(s["psmem"])
	if lines, ok := s["pstat"]; ok {
		r.pticks = map[int]float64{}
		for _, l := range lines {
			// pid (comm) state ppid … utime(14) stime(15) … rss(24)
			open, close := strings.IndexByte(l, '('), strings.LastIndexByte(l, ')')
			if open < 0 || close < open {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(l[:open]))
			if err != nil {
				continue
			}
			f := strings.Fields(l[close+1:])
			if len(f) < 22 {
				continue
			}
			ticks := atof(f[11]) + atof(f[12])
			r.pticks[pid] = ticks
			r.pstat = append(r.pstat, Proc{PID: pid, User: "", Command: l[open+1 : close], RSS: int64(atof(f[21])) * 4096})
		}
	}
	for _, l := range s["failed"] {
		f := strings.Fields(strings.TrimLeft(l, "●* "))
		if len(f) > 0 {
			r.failed = append(r.failed, f[0])
		}
	}
	for _, l := range s["units"] {
		if name, state, ok := strings.Cut(l, "\t"); ok {
			r.units[name] = strings.TrimSpace(state)
		}
	}
	if _, ok := s["docker"]; ok {
		r.hasDock = true
		for _, l := range s["docker"] {
			f := strings.SplitN(l, "\t", 3)
			if len(f) < 3 {
				continue
			}
			c := Container{Name: f[0], State: f[1], Status: f[2]}
			switch {
			case strings.Contains(f[2], "(unhealthy)"):
				c.Health = "unhealthy"
			case strings.Contains(f[2], "(healthy)"):
				c.Health = "healthy"
			case strings.Contains(f[2], "(health: starting)"):
				c.Health = "starting"
			}
			r.docker = append(r.docker, c)
		}
	}
	return r
}

func parsePS(lines []string) []Proc {
	out := []Proc{}
	for i, l := range lines {
		f := strings.Fields(l)
		if i == 0 || len(f) < 6 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		rss, _ := strconv.ParseInt(f[4], 10, 64)
		out = append(out, Proc{PID: pid, User: f[1], CPU: atof(f[2]), Mem: atof(f[3]), RSS: rss * 1024, Command: strings.Join(f[5:], " ")})
	}
	return out
}

// ---- snapshot (what the UI and the history see) ----

type Mount struct {
	FS          string  `json:"fs"`
	Mount       string  `json:"mount"`
	Total       int64   `json:"total"`
	Used        int64   `json:"used"`
	Avail       int64   `json:"avail"`
	Pct         float64 `json:"pct"`
	InodesTotal int64   `json:"inodesTotal"`
	InodesUsed  int64   `json:"inodesUsed"`
	InodePct    float64 `json:"inodePct"`
}

type DiskIO struct {
	Device    string  `json:"device"`
	ReadBps   float64 `json:"readBps"`
	WriteBps  float64 `json:"writeBps"`
	ReadIOPS  float64 `json:"readIops"`
	WriteIOPS float64 `json:"writeIops"`
	LatencyMs float64 `json:"latencyMs"`
	Util      float64 `json:"util"`
}

type NetIO struct {
	Iface   string  `json:"iface"`
	RxBps   float64 `json:"rxBps"`
	TxBps   float64 `json:"txBps"`
	RxPps   float64 `json:"rxPps"`
	TxPps   float64 `json:"txPps"`
	Errors  float64 `json:"errors"` // per second, rx+tx
	Drops   float64 `json:"drops"`
	RxTotal float64 `json:"rxTotal"`
	TxTotal float64 `json:"txTotal"`
}

type Proc struct {
	PID     int     `json:"pid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSS     int64   `json:"rss"`
	Command string  `json:"command"`
}

type Container struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Status string `json:"status"`
	Health string `json:"health"`
}

type UnitState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// Snapshot is the full, current picture of a server.
type Snapshot struct {
	Server  string     `json:"server"`
	TS      int64      `json:"ts"` // unix seconds (server clock)
	Uptime  int64      `json:"uptime"`
	CPUs    int        `json:"cpus"`
	CPU     float64    `json:"cpu"` // % busy
	User    float64    `json:"user"`
	System  float64    `json:"system"`
	IOWait  float64    `json:"iowait"`
	Steal   float64    `json:"steal"`
	Cores   []float64  `json:"cores"`
	Load    [3]float64 `json:"load"`
	Procs   int        `json:"procs"`
	Running int        `json:"running"`

	MemTotal  int64   `json:"memTotal"`
	MemUsed   int64   `json:"memUsed"`
	MemAvail  int64   `json:"memAvail"`
	MemCache  int64   `json:"memCache"`
	MemBuffer int64   `json:"memBuffer"`
	MemPct    float64 `json:"memPct"`
	SwapTotal int64   `json:"swapTotal"`
	SwapUsed  int64   `json:"swapUsed"`
	SwapPct   float64 `json:"swapPct"`

	Mounts []Mount  `json:"mounts"`
	Disks  []DiskIO `json:"disks"`
	Nets   []NetIO  `json:"nets"`

	// Totals across devices/interfaces.
	DiskRead  float64 `json:"diskRead"`
	DiskWrite float64 `json:"diskWrite"`
	IOPS      float64 `json:"iops"`
	DiskLat   float64 `json:"diskLat"`
	NetRx     float64 `json:"netRx"`
	NetTx     float64 `json:"netTx"`
	NetErrors float64 `json:"netErrors"`
	NetDrops  float64 `json:"netDrops"`

	TopCPU     []Proc      `json:"topCpu"`
	TopMem     []Proc      `json:"topMem"`
	Failed     []string    `json:"failed"`
	Units      []UnitState `json:"units"`
	Containers []Container `json:"containers"`
	HasDocker  bool        `json:"hasDocker"`
	// Rates are only meaningful once two samples exist.
	HasRates bool `json:"hasRates"`
}

// snapshot builds the current view from cur and the previous raw sample.
func snapshot(server string, prev *raw, cur raw) Snapshot {
	s := Snapshot{
		Server: server, TS: int64(cur.ts), Uptime: int64(cur.uptime), CPUs: cur.cpus, Load: cur.load,
		Procs: cur.procs, Running: cur.running, Mounts: cur.mounts, TopCPU: cur.topCPU, TopMem: cur.topMem,
		Failed: cur.failed, Containers: cur.docker, HasDocker: cur.hasDock,
		Cores: []float64{}, Disks: []DiskIO{}, Nets: []NetIO{}, Units: []UnitState{},
	}
	if s.Mounts == nil {
		s.Mounts = []Mount{}
	}
	if s.Failed == nil {
		s.Failed = []string{}
	}
	if s.Containers == nil {
		s.Containers = []Container{}
	}
	for name, st := range cur.units {
		s.Units = append(s.Units, UnitState{Name: name, State: st})
	}
	sort.Slice(s.Units, func(i, j int) bool { return s.Units[i].Name < s.Units[j].Name })

	m := cur.mem
	s.MemTotal = m["MemTotal"]
	avail, ok := m["MemAvailable"]
	if !ok {
		avail = m["MemFree"] + m["Buffers"] + m["Cached"]
	}
	s.MemAvail = avail
	s.MemUsed = s.MemTotal - avail
	s.MemCache = m["Cached"] + m["SReclaimable"]
	s.MemBuffer = m["Buffers"]
	if s.MemTotal > 0 {
		s.MemPct = float64(s.MemUsed) / float64(s.MemTotal) * 100
	}
	s.SwapTotal = m["SwapTotal"]
	s.SwapUsed = m["SwapTotal"] - m["SwapFree"]
	if s.SwapTotal > 0 {
		s.SwapPct = float64(s.SwapUsed) / float64(s.SwapTotal) * 100
	}

	if cur.pstat != nil && len(s.TopMem) == 0 {
		procs := append([]Proc(nil), cur.pstat...)
		if prev != nil && prev.pticks != nil && cur.ts > prev.ts {
			for i := range procs {
				if old, ok := prev.pticks[procs[i].PID]; ok {
					procs[i].CPU = (cur.pticks[procs[i].PID] - old) / (cur.ts - prev.ts) // USER_HZ = 100 → %
				}
			}
		}
		for i := range procs {
			if s.MemTotal > 0 {
				procs[i].Mem = float64(procs[i].RSS) / float64(s.MemTotal) * 100
			}
		}
		byCPU := append([]Proc(nil), procs...)
		sort.Slice(byCPU, func(i, j int) bool { return byCPU[i].CPU > byCPU[j].CPU })
		sort.Slice(procs, func(i, j int) bool { return procs[i].RSS > procs[j].RSS })
		s.TopCPU = byCPU[:min(10, len(byCPU))]
		s.TopMem = procs[:min(10, len(procs))]
	}
	if s.TopCPU == nil {
		s.TopCPU = []Proc{}
	}
	if s.TopMem == nil {
		s.TopMem = []Proc{}
	}
	if prev == nil || cur.ts <= prev.ts {
		return s
	}
	dt := cur.ts - prev.ts
	s.HasRates = true
	pct := func(a, b cpuTimes) (busy, user, sys, iow, steal float64) {
		tot := b.total() - a.total()
		if tot <= 0 {
			return
		}
		idle := (b.idle - a.idle) + (b.iowait - a.iowait)
		busy = clamp((tot - idle) / tot * 100)
		user = clamp((b.user + b.nice - a.user - a.nice) / tot * 100)
		sys = clamp((b.system + b.irq + b.softirq - a.system - a.irq - a.softirq) / tot * 100)
		iow = clamp((b.iowait - a.iowait) / tot * 100)
		steal = clamp((b.steal - a.steal) / tot * 100)
		return
	}
	s.CPU, s.User, s.System, s.IOWait, s.Steal = pct(prev.cpu, cur.cpu)
	for i := range cur.cores {
		if i < len(prev.cores) {
			b, _, _, _, _ := pct(prev.cores[i], cur.cores[i])
			s.Cores = append(s.Cores, b)
		}
	}
	rate := func(a, b float64) float64 {
		if b < a { // counter reset (reboot, wrap)
			return 0
		}
		return (b - a) / dt
	}
	var latSum, iosSum float64
	for name, b := range cur.disks {
		a, ok := prev.disks[name]
		if !ok {
			continue
		}
		d := DiskIO{Device: name, ReadBps: rate(a.rsect, b.rsect) * 512, WriteBps: rate(a.wsect, b.wsect) * 512,
			ReadIOPS: rate(a.reads, b.reads), WriteIOPS: rate(a.writes, b.writes)}
		ios := (b.reads - a.reads) + (b.writes - a.writes)
		if ios > 0 {
			d.LatencyMs = ((b.rms - a.rms) + (b.wms - a.wms)) / ios
			latSum += (b.rms - a.rms) + (b.wms - a.wms)
			iosSum += ios
		}
		d.Util = clamp(rate(a.ioticks, b.ioticks) / 10) // ms busy per s → %
		s.Disks = append(s.Disks, d)
		s.DiskRead += d.ReadBps
		s.DiskWrite += d.WriteBps
		s.IOPS += d.ReadIOPS + d.WriteIOPS
	}
	if iosSum > 0 {
		s.DiskLat = latSum / iosSum
	}
	sort.Slice(s.Disks, func(i, j int) bool { return s.Disks[i].Device < s.Disks[j].Device })
	for name, b := range cur.nets {
		a, ok := prev.nets[name]
		if !ok {
			continue
		}
		n := NetIO{Iface: name, RxBps: rate(a.rxBytes, b.rxBytes), TxBps: rate(a.txBytes, b.txBytes),
			RxPps: rate(a.rxPkts, b.rxPkts), TxPps: rate(a.txPkts, b.txPkts),
			Errors: rate(a.rxErr, b.rxErr) + rate(a.txErr, b.txErr), Drops: rate(a.rxDrop, b.rxDrop) + rate(a.txDrop, b.txDrop),
			RxTotal: b.rxBytes, TxTotal: b.txBytes}
		s.Nets = append(s.Nets, n)
		// Container veth/bridge traffic would double count host traffic.
		if !virtualIface(name) {
			s.NetRx += n.RxBps
			s.NetTx += n.TxBps
			s.NetErrors += n.Errors
			s.NetDrops += n.Drops
		}
	}
	sort.Slice(s.Nets, func(i, j int) bool { return s.Nets[i].Iface < s.Nets[j].Iface })
	return s
}

func virtualIface(n string) bool {
	for _, p := range []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "vxlan", "tun", "tap", "lxc", "kube"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
