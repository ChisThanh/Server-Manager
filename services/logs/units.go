package logs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Unit is a service as listed by the init system.
type Unit struct {
	Name          string `json:"name"`
	Load          string `json:"load"`   // loaded | not-found | masked | "" (unit file only)
	Active        string `json:"active"` // active | inactive | failed | activating…
	Sub           string `json:"sub"`    // running | exited | dead | failed…
	Description   string `json:"description"`
	UnitFileState string `json:"unitFileState"` // enabled | disabled | static | masked | indirect | generated…
}

// ServiceList is the list of services and the init system managing them.
type ServiceList struct {
	Init  string `json:"init"` // systemd | openrc | sysv | none
	Units []Unit `json:"units"`
}

// UnitStat is a running service's resource usage (systemd cgroup accounting).
type UnitStat struct {
	Name    string `json:"name"`
	MainPID int    `json:"mainPid"`
	Memory  int64  `json:"memory"`  // bytes, -1 = not available
	CPUNSec int64  `json:"cpuNSec"` // cumulative CPU time, -1 = not available
	Tasks   int64  `json:"tasks"`   // -1 = not available
}

// UnitStats is one sample of every running service. CPU% is the CPUNSec
// delta between two samples divided by the SampleNs delta.
type UnitStats struct {
	SampleNs int64      `json:"sampleNs"` // server uptime in ns when sampled
	Units    []UnitStat `json:"units"`
}

// ServiceDetail describes one systemd service.
type ServiceDetail struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	LoadState     string   `json:"loadState"`
	ActiveState   string   `json:"activeState"`
	SubState      string   `json:"subState"`
	UnitFileState string   `json:"unitFileState"`
	Type          string   `json:"type"`
	User          string   `json:"user"`
	MainPID       int      `json:"mainPid"`
	Since         string   `json:"since"`  // ActiveEnterTimestamp as printed by systemd
	Uptime        int64    `json:"uptime"` // seconds since the main process started, -1 = not running
	Memory        int64    `json:"memory"` // bytes, -1 = n/a
	CPUNSec       int64    `json:"cpuNSec"`
	CPUPercent    float64  `json:"cpuPercent"` // over ~1 s, 100 = one core, -1 = n/a
	Tasks         int64    `json:"tasks"`
	NRestarts     int      `json:"nRestarts"`
	Restart       string   `json:"restart"`    // no | on-failure | always…
	RestartSec    string   `json:"restartSec"` // "100ms", "5s"
	ExecStart     string   `json:"execStart"`
	FragmentPath  string   `json:"fragmentPath"`
	DropInPaths   []string `json:"dropInPaths"`
	Requires      []string `json:"requires"`
	Wants         []string `json:"wants"`
	After         []string `json:"after"`
	Before        []string `json:"before"`
	WantedBy      []string `json:"wantedBy"`
	RequiredBy    []string `json:"requiredBy"`
	// UnitFile is `systemctl cat` (fragment + drop-ins), read-only.
	UnitFile string `json:"unitFile"`
	// AutoRestart: the app's drop-in (Restart=on-failure) is installed.
	AutoRestart     bool      `json:"autoRestart"`
	AutoRestartPath string    `json:"autoRestartPath"`
	Logs            []LogLine `json:"logs"`
	// LogsNeedSudo: the journal needs sudo and no password is available yet.
	LogsNeedSudo bool   `json:"logsNeedSudo"`
	LogsError    string `json:"logsError"`
}

const initScript = `if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then echo systemd
elif command -v rc-service >/dev/null 2>&1; then echo openrc
elif command -v service >/dev/null 2>&1 || [ -d /etc/init.d ]; then echo sysv
else echo none; fi`

func (s *LogsService) initSystem(ctx context.Context, conn *sshx.Conn) (string, error) {
	res, err := s.core.Run(ctx, conn, initScript, false, "", "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// ServiceList lists services: systemd units merged with their unit-file
// state, or OpenRC / SysV services when systemd isn't used.
func (s *LogsService) ServiceList(connID string) (ServiceList, error) {
	out := ServiceList{Units: []Unit{}}
	conn, err := s.conn(connID)
	if err != nil {
		return out, err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	out.Init, err = s.initSystem(ctx, conn)
	if err != nil {
		return out, err
	}
	switch out.Init {
	case "systemd":
		out.Units, err = s.systemdUnits(ctx, conn)
	case "openrc":
		out.Units, err = s.openrcUnits(ctx, conn)
	case "sysv":
		out.Units, err = s.sysvUnits(ctx, conn)
	}
	if out.Units == nil {
		out.Units = []Unit{}
	}
	return out, err
}

const systemdListScript = `echo @@units
systemctl list-units --type=service --all --no-legend --no-pager --plain 2>&1
echo @@files
systemctl list-unit-files --type=service --no-legend --no-pager 2>/dev/null
echo @@end`

func (s *LogsService) systemdUnits(ctx context.Context, conn *sshx.Conn) ([]Unit, error) {
	res, err := s.core.Run(ctx, conn, systemdListScript, false, "", "")
	if err != nil {
		return nil, err
	}
	sec := sections(res.Stdout)
	units := ParseUnits(sec["units"], sec["files"])
	// Units known only from their file have no description yet.
	var missing []string
	for _, u := range units {
		if u.Load == "" {
			missing = append(missing, core.Q(u.Name))
		}
	}
	if len(missing) > 0 && len(missing) <= 400 {
		r, err := s.core.Run(ctx, conn, "systemctl show -p Id,Description --no-pager -- "+strings.Join(missing, " ")+" 2>/dev/null", false, "", "")
		if err == nil {
			desc := map[string]string{}
			for _, blk := range showBlocks(r.Stdout) {
				desc[blk["Id"]] = blk["Description"]
			}
			for i := range units {
				if d := desc[units[i].Name]; units[i].Description == "" && d != "" && d != units[i].Name {
					units[i].Description = d
				}
			}
		}
	}
	return units, nil
}

// ParseUnits merges `systemctl list-units` and `list-unit-files` output.
func ParseUnits(listUnits, listFiles []string) []Unit {
	fileState := map[string]string{}
	for _, l := range listFiles {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.HasSuffix(f[0], ".service") {
			fileState[f[0]] = f[1]
		}
	}
	seen := map[string]bool{}
	units := []Unit{}
	for _, l := range listUnits {
		f := strings.Fields(strings.TrimLeft(l, "●* "))
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") || !validUnit(f[0]) {
			continue
		}
		u := Unit{Name: f[0], Load: f[1], Active: f[2], Sub: f[3], Description: strings.Join(f[4:], " ")}
		if u.Load == "not-found" && u.Active != "failed" {
			continue // referenced by another unit but not installed
		}
		u.UnitFileState = fileState[u.Name]
		if u.UnitFileState == "" {
			// Instances (getty@tty1) take their template's state.
			if at := strings.IndexByte(u.Name, '@'); at > 0 {
				u.UnitFileState = fileState[u.Name[:at+1]+".service"]
			}
		}
		if u.Description == u.Name {
			u.Description = ""
		}
		seen[u.Name] = true
		units = append(units, u)
	}
	for name, st := range fileState {
		// Templates can't run by themselves; aliases duplicate their target.
		if seen[name] || strings.HasSuffix(name, "@.service") || st == "alias" || !validUnit(name) {
			continue
		}
		units = append(units, Unit{Name: name, Active: "inactive", Sub: "dead", UnitFileState: st})
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units
}

func (s *LogsService) openrcUnits(ctx context.Context, conn *sshx.Conn) ([]Unit, error) {
	res, err := s.core.Run(ctx, conn, "rc-status -a 2>/dev/null; echo @@init; ls -1 /etc/init.d 2>/dev/null", false, "", "")
	if err != nil {
		return nil, err
	}
	return parseOpenRC(res.Stdout), nil
}

func parseOpenRC(out string) []Unit {
	units := map[string]*Unit{}
	runlevel := ""
	inInit := false
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			continue
		case t == "@@init":
			inInit = true
			continue
		case inInit:
			if validUnit(t) && units[t] == nil && t != "functions.sh" {
				units[t] = &Unit{Name: t, Active: "inactive", Sub: "stopped", UnitFileState: "disabled"}
			}
			continue
		case strings.HasPrefix(t, "Runlevel:") || strings.HasPrefix(t, "Dynamic Runlevel:"):
			runlevel = strings.TrimSpace(t[strings.Index(t, ":")+1:])
			continue
		}
		open := strings.LastIndexByte(t, '[')
		if open < 0 || !strings.HasSuffix(t, "]") {
			continue
		}
		name := strings.TrimSpace(t[:open])
		state := strings.TrimSpace(t[open+1 : len(t)-1])
		if !validUnit(name) {
			continue
		}
		u := &Unit{Name: name, Sub: state, UnitFileState: "disabled"}
		switch state {
		case "started":
			u.Active, u.Sub = "active", "running"
		case "crashed":
			u.Active = "failed"
		default:
			u.Active = "inactive"
		}
		if runlevel != "" && !strings.HasPrefix(runlevel, "manual") && !strings.HasPrefix(runlevel, "hotplugged") && !strings.HasPrefix(runlevel, "needed") {
			u.UnitFileState = "enabled"
			u.Description = runlevel
		}
		if old := units[name]; old == nil || old.Load == "" {
			u.Load = "loaded"
			units[name] = u
		}
	}
	list := make([]Unit, 0, len(units))
	for _, u := range units {
		list = append(list, *u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

func (s *LogsService) sysvUnits(ctx context.Context, conn *sshx.Conn) ([]Unit, error) {
	res, err := s.core.Run(ctx, conn, "service --status-all 2>&1 || ls -1 /etc/init.d", false, "", "")
	if err != nil {
		return nil, err
	}
	list := []Unit{}
	for _, l := range strings.Split(res.Stdout, "\n") {
		t := strings.TrimSpace(l)
		u := Unit{Active: "inactive", Sub: "unknown", Load: "loaded"}
		switch {
		case strings.HasPrefix(t, "[ + ]"):
			u.Active, u.Sub = "active", "running"
		case strings.HasPrefix(t, "[ - ]"):
			u.Sub = "stopped"
		case strings.HasPrefix(t, "[ ? ]"):
		default:
			if validUnit(t) {
				u.Name = t
			}
		}
		if u.Name == "" && len(t) > 5 {
			u.Name = strings.TrimSpace(t[5:])
		}
		if validUnit(u.Name) {
			list = append(list, u)
		}
	}
	return list, nil
}

// showBlocks parses `systemctl show` output for one or more units.
func showBlocks(out string) []map[string]string {
	var blocks []map[string]string
	cur := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			if len(cur) > 0 {
				blocks = append(blocks, cur)
				cur = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if ok {
			cur[k] = v
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

// num parses a systemd numeric property; unset values ("[not set]",
// UINT64_MAX) return -1.
func num(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" || v == "[not set]" || v == "infinity" || v == "18446744073709551615" {
		return -1
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func uptimeNs(s string) int64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v * 1e9)
}

const statsScript = `echo "@@up $(cat /proc/uptime)"
systemctl list-units --type=service --state=running --no-legend --no-pager --plain 2>/dev/null | awk '{print $1}' |
  xargs -r systemctl show -p Id,MainPID,MemoryCurrent,CPUUsageNSec,TasksCurrent --no-pager -- 2>/dev/null`

// ServiceStats samples CPU time and memory of every running service.
func (s *LogsService) ServiceStats(connID string) (UnitStats, error) {
	out := UnitStats{Units: []UnitStat{}}
	conn, err := s.conn(connID)
	if err != nil {
		return out, err
	}
	ctx, cancel := core.Timeout(20 * time.Second)
	defer cancel()
	res, err := s.core.Run(ctx, conn, statsScript, false, "", "")
	if err != nil {
		return out, err
	}
	first, rest, _ := strings.Cut(res.Stdout, "\n")
	if v, ok := strings.CutPrefix(first, "@@up "); ok {
		out.SampleNs = uptimeNs(v)
	}
	for _, b := range showBlocks(rest) {
		if b["Id"] == "" {
			continue
		}
		pid, _ := strconv.Atoi(b["MainPID"])
		out.Units = append(out.Units, UnitStat{Name: b["Id"], MainPID: pid, Memory: num(b["MemoryCurrent"]), CPUNSec: num(b["CPUUsageNSec"]), Tasks: num(b["TasksCurrent"])})
	}
	return out, nil
}

const detailProps = "Id,Description,LoadState,ActiveState,SubState,UnitFileState,Type,User,MainPID,ActiveEnterTimestamp," +
	"ActiveEnterTimestampMonotonic,ExecMainStartTimestampMonotonic,MemoryCurrent,CPUUsageNSec,TasksCurrent,NRestarts," +
	"Restart,RestartUSec,ExecStart,FragmentPath,DropInPaths,Requires,Wants,After,Before,WantedBy,RequiredBy"

func dropInPath(unit string) string {
	return "/etc/systemd/system/" + unit + ".d/10-server-manager.conf"
}

// ServiceDetail returns everything about a systemd service: state, resource
// usage (CPU% measured over ~1 s), restart policy, dependencies, unit file
// and its last 50 journal lines.
func (s *LogsService) ServiceDetail(connID, name string) (ServiceDetail, error) {
	d := ServiceDetail{Name: name, DropInPaths: []string{}, Requires: []string{}, Wants: []string{}, After: []string{},
		Before: []string{}, WantedBy: []string{}, RequiredBy: []string{}, Logs: []LogLine{}, Uptime: -1, CPUPercent: -1}
	if !validUnit(name) {
		return d, apperr.New("sys.invalidUnit")
	}
	conn, err := s.conn(connID)
	if err != nil {
		return d, err
	}
	ctx, cancel := core.Timeout(40 * time.Second)
	defer cancel()
	q := core.Q(name)
	script := `[ -d /run/systemd/system ] || { echo __SM_NOSYSTEMD__; exit 0; }
echo @@show
systemctl show --no-pager -p ` + detailProps + ` -- ` + q + `
echo "@@s1 $(cat /proc/uptime)"
echo "@@c1 $(systemctl show -p CPUUsageNSec -- ` + q + ` | cut -d= -f2)"
sleep 1
echo "@@s2 $(cat /proc/uptime)"
echo "@@c2 $(systemctl show -p CPUUsageNSec -- ` + q + ` | cut -d= -f2)"
[ -f ` + core.Q(dropInPath(name)) + ` ] && echo "@@dropin yes"
echo @@cat
systemctl cat --no-pager -- ` + q + ` 2>&1 | head -c 262144
echo
echo @@end`
	res, err := s.core.Run(ctx, conn, script, false, "", "")
	if err != nil {
		return d, err
	}
	if strings.Contains(res.Stdout, "__SM_NOSYSTEMD__") {
		return d, apperr.New("sys.noSystemd")
	}
	parseDetail(&d, res.Stdout)
	if d.LoadState == "not-found" {
		return d, apperr.New("logs.unitNotFound", "name", name)
	}
	// Recent logs: silently through sudo when possible.
	lctx, lcancel := core.Timeout(20 * time.Second)
	defer lcancel()
	r, err := s.query(lctx, conn, QuerySpec{Kind: "unit", Target: name, Lines: 50}, "")
	switch {
	case err == nil:
		d.Logs = r.Lines
	case apperr.HasCode(err, "sudo.required") || apperr.HasCode(err, "sudo.wrongPassword"):
		d.LogsNeedSudo = true
	default:
		d.LogsError = apperr.From(err).Error()
	}
	return d, nil
}

func parseDetail(d *ServiceDetail, out string) {
	// Split off the unit file, which may contain anything.
	body, cat, _ := strings.Cut(out, "\n@@cat\n")
	if i := strings.LastIndex(cat, "\n@@end"); i >= 0 {
		cat = cat[:i]
	}
	d.UnitFile = strings.TrimRight(cat, "\n")
	sec := map[string]string{}
	var show strings.Builder
	inShow := false
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "@@") {
			k, v, _ := strings.Cut(l[2:], " ")
			sec[k] = v
			inShow = k == "show"
			continue
		}
		if inShow {
			show.WriteString(l + "\n")
		}
	}
	p := map[string]string{}
	if b := showBlocks(show.String()); len(b) > 0 {
		p = b[0]
	}
	d.Description = p["Description"]
	d.LoadState = p["LoadState"]
	d.ActiveState = p["ActiveState"]
	d.SubState = p["SubState"]
	d.UnitFileState = p["UnitFileState"]
	d.Type = p["Type"]
	d.User = p["User"]
	d.MainPID, _ = strconv.Atoi(p["MainPID"])
	d.Since = p["ActiveEnterTimestamp"]
	d.Memory = num(p["MemoryCurrent"])
	d.CPUNSec = num(p["CPUUsageNSec"])
	d.Tasks = num(p["TasksCurrent"])
	d.NRestarts, _ = strconv.Atoi(p["NRestarts"])
	d.Restart = p["Restart"]
	d.RestartSec = p["RestartUSec"]
	d.ExecStart = execArgv(p["ExecStart"])
	d.FragmentPath = p["FragmentPath"]
	d.DropInPaths = fieldsOrEmpty(p["DropInPaths"])
	d.Requires = fieldsOrEmpty(p["Requires"])
	d.Wants = fieldsOrEmpty(p["Wants"])
	d.After = fieldsOrEmpty(p["After"])
	d.Before = fieldsOrEmpty(p["Before"])
	d.WantedBy = fieldsOrEmpty(p["WantedBy"])
	d.RequiredBy = fieldsOrEmpty(p["RequiredBy"])
	d.AutoRestartPath = dropInPath(d.Name)
	d.AutoRestart = sec["dropin"] == "yes"

	now := uptimeNs(sec["s2"])
	if d.ActiveState == "active" || d.ActiveState == "reloading" || d.ActiveState == "deactivating" {
		start := num(p["ExecMainStartTimestampMonotonic"])
		if start <= 0 {
			start = num(p["ActiveEnterTimestampMonotonic"])
		}
		if start > 0 && now > 0 {
			if up := now/1e9 - start/1e6; up >= 0 {
				d.Uptime = up
			}
		}
	}
	c1, c2 := num(sec["c1"]), num(sec["c2"])
	t1 := uptimeNs(sec["s1"])
	if c1 >= 0 && c2 >= c1 && now > t1 {
		d.CPUPercent = float64(c2-c1) / float64(now-t1) * 100
		d.CPUNSec = c2
	}
}

func fieldsOrEmpty(s string) []string {
	f := strings.Fields(s)
	if f == nil {
		return []string{}
	}
	return f
}

// execArgv extracts "argv[]=…" from systemd's ExecStart property.
func execArgv(v string) string {
	var cmds []string
	for _, part := range strings.Split(v, "} ; {") {
		if i := strings.Index(part, "argv[]="); i >= 0 {
			a := part[i+len("argv[]="):]
			if j := strings.Index(a, " ; "); j >= 0 {
				a = a[:j]
			}
			cmds = append(cmds, strings.TrimSpace(a))
		}
	}
	if len(cmds) == 0 {
		return strings.TrimSpace(v)
	}
	return strings.Join(cmds, "\n")
}

// ---- changes ----

var serviceActions = map[string]bool{"start": true, "stop": true, "restart": true, "reload": true, "enable": true, "disable": true}

// ServiceControl starts/stops/restarts/reloads/enables/disables a service
// through systemctl, rc-service or service (as root via sudo).
func (s *LogsService) ServiceControl(connID, name, action, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermServices); err != nil {
		return err
	}
	err := s.serviceControl(connID, name, action, sudoPassword)
	s.core.Audit(connID, "service."+action, name, "", err)
	return err
}

func (s *LogsService) serviceControl(connID, name, action, pw string) error {
	if !serviceActions[action] {
		return apperr.New("sys.invalidAction")
	}
	if !validUnit(name) {
		return apperr.New("sys.invalidUnit")
	}
	conn, err := s.conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	init, err := s.initSystem(ctx, conn)
	if err != nil {
		return err
	}
	q := core.Q(name)
	var cmd string
	switch init {
	case "systemd":
		cmd = "systemctl " + action + " -- " + q
	case "openrc":
		switch action {
		case "enable":
			cmd = "rc-update add " + q + " default"
		case "disable":
			cmd = "rc-update del " + q + " default"
		default:
			cmd = "rc-service " + q + " " + action
		}
	case "sysv":
		switch action {
		case "enable", "disable":
			return apperr.New("logs.svc.unsupported", "action", action)
		}
		cmd = "if command -v service >/dev/null 2>&1; then service " + q + " " + action + "; else /etc/init.d/" + q + " " + action + "; fi"
	default:
		return apperr.New("logs.svc.noInit")
	}
	_, err = s.core.RunOK(ctx, conn, cmd, true, pw, "")
	return err
}

const autoRestartConf = "# Managed by Server Manager: restart the service when it fails.\n[Service]\nRestart=on-failure\nRestartSec=5s\n"

// SetAutoRestart installs (or removes) a drop-in making systemd restart the
// service when it fails, then reloads systemd.
func (s *LogsService) SetAutoRestart(connID, name string, enabled bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermServices); err != nil {
		return err
	}
	err := s.setAutoRestart(connID, name, enabled, sudoPassword)
	detail := "off"
	if enabled {
		detail = "on: Restart=on-failure RestartSec=5s"
	}
	s.core.Audit(connID, "service.autoRestart", name, detail, err)
	return err
}

func (s *LogsService) setAutoRestart(connID, name string, enabled bool, pw string) error {
	if !validUnit(name) || !strings.HasSuffix(name, ".service") || strings.Contains(name, "..") {
		return apperr.New("sys.invalidUnit")
	}
	conn, err := s.conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(time.Minute)
	defer cancel()
	if init, err := s.initSystem(ctx, conn); err != nil {
		return err
	} else if init != "systemd" {
		return apperr.New("sys.noSystemd")
	}
	file := dropInPath(name)
	dir := file[:strings.LastIndexByte(file, '/')]
	var cmd, stdin string
	if enabled {
		// Write next to the target (a name systemd ignores), then rename.
		cmd = fmt.Sprintf(`set -e
d=%s; f=%s
mkdir -p "$d"
t=$(mktemp "$d/.smtmp.XXXXXX")
trap 'rm -f "$t"' EXIT
cat > "$t"
chmod 0644 "$t"
mv -f "$t" "$f"
systemctl daemon-reload`, core.Q(dir), core.Q(file))
		stdin = autoRestartConf
	} else {
		cmd = fmt.Sprintf(`d=%s; f=%s
rm -f "$f" || exit 1
rmdir "$d" 2>/dev/null
systemctl daemon-reload`, core.Q(dir), core.Q(file))
	}
	_, err = s.core.RunOK(ctx, conn, cmd, true, pw, stdin)
	return err
}
