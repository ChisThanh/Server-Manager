package services

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/sshx"
)

// SystemService reads server metrics and manages processes/services using
// only tools that ship with the OS (/proc, df, ps, systemctl).
type SystemService struct {
	core *Core
}

func NewSystemService(core *Core) *SystemService { return &SystemService{core: core} }

type DiskInfo struct {
	Filesystem string `json:"filesystem"`
	Mount      string `json:"mount"`
	Total      int64  `json:"total"`
	Used       int64  `json:"used"`
	Avail      int64  `json:"avail"`
}

type SysInfo struct {
	Hostname  string     `json:"hostname"`
	OS        string     `json:"os"`
	Kernel    string     `json:"kernel"`
	Uptime    int64      `json:"uptime"`
	Load      [3]float64 `json:"load"`
	CPUs      int        `json:"cpus"`
	CPUModel  string     `json:"cpuModel"`
	CPUUsage  float64    `json:"cpuUsage"`
	MemTotal  int64      `json:"memTotal"`
	MemUsed   int64      `json:"memUsed"`
	MemAvail  int64      `json:"memAvail"`
	SwapTotal int64      `json:"swapTotal"`
	SwapUsed  int64      `json:"swapUsed"`
	Disks     []DiskInfo `json:"disks"`
	NetRx     int64      `json:"netRx"`
	NetTx     int64      `json:"netTx"`
	Users     int        `json:"users"`
	Time      int64      `json:"time"`
}

const infoScript = `
echo @@hostname; hostname 2>/dev/null
echo @@os; ( . /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" ) || uname -s
echo @@kernel; uname -srm
echo @@uptime; cat /proc/uptime 2>/dev/null
echo @@load; cat /proc/loadavg 2>/dev/null
echo @@cpus; nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null
echo @@cpumodel; grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2
echo @@mem; cat /proc/meminfo 2>/dev/null
echo @@stat1; head -n1 /proc/stat 2>/dev/null
sleep 0.5
echo @@stat2; head -n1 /proc/stat 2>/dev/null
echo @@disk; df -kP 2>/dev/null
echo @@net; cat /proc/net/dev 2>/dev/null
echo @@users; who 2>/dev/null | wc -l
echo @@end
`

func (s *SystemService) Info(connID string) (SysInfo, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return SysInfo{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, "sh -c "+sshx.ShellQuote(infoScript), nil)
	if err != nil {
		return SysInfo{}, err
	}
	sections := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "@@") {
			cur = strings.TrimPrefix(line, "@@")
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			sections[cur] = append(sections[cur], line)
		}
	}
	first := func(k string) string {
		if v := sections[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	info := SysInfo{
		Hostname: first("hostname"),
		OS:       first("os"),
		Kernel:   first("kernel"),
		CPUModel: first("cpumodel"),
		Time:     time.Now().Unix(),
	}
	if f := strings.Fields(first("uptime")); len(f) > 0 {
		v, _ := strconv.ParseFloat(f[0], 64)
		info.Uptime = int64(v)
	}
	if f := strings.Fields(first("load")); len(f) >= 3 {
		for i := 0; i < 3; i++ {
			info.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	info.CPUs, _ = strconv.Atoi(first("cpus"))
	info.Users, _ = strconv.Atoi(first("users"))

	mem := map[string]int64{}
	for _, l := range sections["mem"] {
		parts := strings.Fields(l)
		if len(parts) >= 2 {
			v, _ := strconv.ParseInt(parts[1], 10, 64)
			mem[strings.TrimSuffix(parts[0], ":")] = v * 1024
		}
	}
	info.MemTotal = mem["MemTotal"]
	avail, ok := mem["MemAvailable"]
	if !ok {
		avail = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	info.MemAvail = avail
	info.MemUsed = info.MemTotal - avail
	info.SwapTotal = mem["SwapTotal"]
	info.SwapUsed = mem["SwapTotal"] - mem["SwapFree"]

	if a, b := cpuTimes(first("stat1")), cpuTimes(first("stat2")); a != nil && b != nil {
		var totalA, totalB int64
		for i := range a {
			totalA += a[i]
			totalB += b[i]
		}
		idleA, idleB := a[3], b[3]
		if len(a) > 4 {
			idleA += a[4]
			idleB += b[4]
		}
		if dt := totalB - totalA; dt > 0 {
			info.CPUUsage = float64(dt-(idleB-idleA)) / float64(dt) * 100
		}
	}

	skipFS := regexp.MustCompile(`^(tmpfs|devtmpfs|udev|overlay|none|shm|run|efivarfs|squashfs)`)
	for i, l := range sections["disk"] {
		if i == 0 {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 6 || skipFS.MatchString(f[0]) || strings.HasPrefix(f[5], "/snap/") ||
			strings.HasPrefix(f[5], "/sys") || strings.HasPrefix(f[5], "/proc") || strings.HasPrefix(f[5], "/dev") {
			continue
		}
		total, _ := strconv.ParseInt(f[1], 10, 64)
		used, _ := strconv.ParseInt(f[2], 10, 64)
		av, _ := strconv.ParseInt(f[3], 10, 64)
		if total == 0 {
			continue
		}
		info.Disks = append(info.Disks, DiskInfo{Filesystem: f[0], Mount: strings.Join(f[5:], " "), Total: total * 1024, Used: used * 1024, Avail: av * 1024})
	}

	for _, l := range sections["net"] {
		name, rest, ok := strings.Cut(l, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) >= 9 {
			rx, _ := strconv.ParseInt(f[0], 10, 64)
			tx, _ := strconv.ParseInt(f[8], 10, 64)
			info.NetRx += rx
			info.NetTx += tx
		}
	}
	return info, nil
}

func cpuTimes(line string) []int64 {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return nil
	}
	out := make([]int64, 0, len(f)-1)
	for _, v := range f[1:] {
		n, _ := strconv.ParseInt(v, 10, 64)
		out = append(out, n)
	}
	return out
}

type Process struct {
	PID     int     `json:"pid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSS     int64   `json:"rss"`
	Elapsed string  `json:"elapsed"`
	Command string  `json:"command"`
}

func (s *SystemService) Processes(connID string) ([]Process, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, "(ps -eo pid,user,pcpu,pmem,rss,etime,args --sort=-pcpu 2>/dev/null || ps -eo pid,user,pcpu,pmem,rss,etime,args) | head -n 201", nil)
	if err != nil {
		return nil, err
	}
	var out []Process
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	header := true
	for sc.Scan() {
		if header {
			header = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 7 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		p := Process{PID: pid, User: f[1], Elapsed: f[5], Command: strings.Join(f[6:], " ")}
		p.CPU, _ = strconv.ParseFloat(f[2], 64)
		p.Mem, _ = strconv.ParseFloat(f[3], 64)
		rss, _ := strconv.ParseInt(f[4], 10, 64)
		p.RSS = rss * 1024
		out = append(out, p)
	}
	return out, nil
}

func (s *SystemService) kill(connID string, pid int, signal string, useSudo bool, sudoPassword string) error {
	switch signal {
	case "TERM", "KILL", "HUP", "INT":
	default:
		signal = "TERM"
	}
	if pid <= 1 {
		return apperr.New("sys.invalidPid")
	}
	return s.run(connID, fmt.Sprintf("kill -%s %d", signal, pid), useSudo, sudoPassword)
}

type ServiceUnit struct {
	Name        string `json:"name"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
	// UnitFileState comes from list-unit-files: enabled, disabled, static,
	// masked, indirect… ("" when unknown).
	UnitFileState string `json:"unitFileState"`
}

// Services lists systemd services (loaded units merged with the unit-file
// state). The Logs module's LogsService.ServiceList also covers OpenRC/SysV.
func (s *SystemService) Services(connID string) ([]ServiceUnit, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, "systemctl list-units --type=service --all --no-legend --no-pager --plain 2>&1", nil)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, apperr.New("sys.noSystemd").WithDetail(res.Stdout)
	}
	fileState := map[string]string{}
	if files, err := conn.Exec(ctx, "systemctl list-unit-files --type=service --no-legend --no-pager 2>/dev/null", nil); err == nil {
		for _, l := range strings.Split(files.Stdout, "\n") {
			if f := strings.Fields(l); len(f) >= 2 {
				fileState[f[0]] = f[1]
			}
		}
	}
	out := []ServiceUnit{}
	for _, l := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(strings.TrimLeft(l, "●* "))
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		u := ServiceUnit{Name: f[0], Load: f[1], Active: f[2], Sub: f[3], Description: strings.Join(f[4:], " "), UnitFileState: fileState[f[0]]}
		if at := strings.IndexByte(u.Name, '@'); u.UnitFileState == "" && at > 0 {
			u.UnitFileState = fileState[u.Name[:at+1]+".service"]
		}
		out = append(out, u)
	}
	return out, nil
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+$`)

func (s *SystemService) serviceAction(connID, name, action string, useSudo bool, sudoPassword string) error {
	switch action {
	case "start", "stop", "restart", "reload", "enable", "disable":
	default:
		return apperr.New("sys.invalidAction")
	}
	if !unitName.MatchString(name) || strings.HasPrefix(name, "-") {
		return apperr.New("sys.invalidUnit")
	}
	return s.run(connID, "systemctl "+action+" -- "+sshx.ShellQuote(name), useSudo, sudoPassword)
}

// ServiceLogs returns the last lines of a unit's journal.
func (s *SystemService) ServiceLogs(connID, name string, lines int, useSudo bool, sudoPassword string) (string, error) {
	if !unitName.MatchString(name) || strings.HasPrefix(name, "-") {
		return "", apperr.New("sys.invalidUnit")
	}
	if lines <= 0 || lines > 5000 {
		lines = 200
	}
	cmd := fmt.Sprintf("journalctl -u %s -n %d --no-pager 2>&1", sshx.ShellQuote(name), lines)
	res, err := s.exec(connID, cmd, useSudo, sudoPassword)
	return res.Stdout, err
}

func (s *SystemService) exec(connID, cmd string, useSudo bool, sudoPassword string) (sshx.ExecResult, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return sshx.ExecResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if useSudo {
		res, err := sudoRun(ctx, conn, s.core.SudoPassword(conn, sudoPassword), cmd, "")
		if err != nil && res.ExitCode != 0 && res.Stdout+res.Stderr != "" {
			return res, nil // let the caller see the command output
		}
		return res, err
	}
	return conn.Exec(ctx, cmd, nil)
}

func (s *SystemService) run(connID, cmd string, useSudo bool, sudoPassword string) error {
	res, err := s.exec(connID, cmd, useSudo, sudoPassword)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return cmdError(res)
	}
	return nil
}
