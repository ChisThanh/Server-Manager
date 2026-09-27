package command

import (
	"regexp"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
)

// PresetParam describes one input of a preset. Labels are translated by the
// frontend as "cmd.preset.param.<name>".
type PresetParam struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"` // unit | int | enum | bool
	Required bool     `json:"required"`
	Default  string   `json:"default"`
	Options  []string `json:"options"` // enum values
	Min      int      `json:"min"`
	Max      int      `json:"max"`
}

// Preset is a predefined action. Its title is "cmd.preset.<id>".
type Preset struct {
	ID       string        `json:"id"`
	Category string        `json:"category"` // service | info | docker | power
	Params   []PresetParam `json:"params"`
	// Sudo means the action needs root (sudo is forced on).
	Sudo bool `json:"sudo"`
	// Danger marks actions that always need a typed confirmation.
	Danger bool `json:"danger"`
}

// Rendered is a preset turned into the shell script that will run.
type Rendered struct {
	Command string `json:"command"`
	Sudo    bool   `json:"sudo"`
	Danger  bool   `json:"danger"`
}

var unitParam = PresetParam{Name: "unit", Kind: "unit", Required: true}

var presets = []Preset{
	{ID: "service.status", Category: "service", Params: []PresetParam{unitParam}},
	{ID: "service.restart", Category: "service", Params: []PresetParam{unitParam}, Sudo: true},
	{ID: "service.reload", Category: "service", Params: []PresetParam{unitParam}, Sudo: true},
	{ID: "service.start", Category: "service", Params: []PresetParam{unitParam}, Sudo: true},
	{ID: "service.stop", Category: "service", Params: []PresetParam{unitParam}, Sudo: true},
	{ID: "disk", Category: "info"},
	{ID: "memory", Category: "info"},
	{ID: "uptime", Category: "info"},
	{ID: "os", Category: "info"},
	{ID: "updates", Category: "info", Params: []PresetParam{{Name: "refresh", Kind: "bool", Default: "false"}}},
	{ID: "docker.ps", Category: "docker", Params: []PresetParam{{Name: "all", Kind: "bool", Default: "false"}}},
	{ID: "top", Category: "info", Params: []PresetParam{
		{Name: "sort", Kind: "enum", Default: "cpu", Options: []string{"cpu", "mem"}},
		{Name: "count", Kind: "int", Default: "10", Min: 1, Max: 100},
	}},
	{ID: "reboot", Category: "power", Sudo: true, Danger: true},
}

// unitRe accepts systemd/OpenRC unit names ("nginx", "php8.2-fpm.service",
// "getty@tty1.service"); no leading dash, spaces or shell metacharacters.
var unitRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:+-]{0,127}$`)

// ValidUnit reports whether u is an acceptable service name.
func ValidUnit(u string) bool { return unitRe.MatchString(u) }

func findPreset(id string) (Preset, bool) {
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// resolveParams applies defaults and validates every parameter.
func resolveParams(p Preset, in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, def := range p.Params {
		v := strings.TrimSpace(in[def.Name])
		if v == "" {
			v = def.Default
		}
		if v == "" {
			if def.Required {
				return nil, apperr.New("cmd.paramRequired", "name", def.Name)
			}
			out[def.Name] = ""
			continue
		}
		switch def.Kind {
		case "unit":
			if !ValidUnit(v) {
				return nil, apperr.New("cmd.invalidUnit", "name", v)
			}
		case "int":
			n, err := strconv.Atoi(v)
			if err != nil || n < def.Min || n > def.Max {
				return nil, apperr.New("cmd.invalidParam", "name", def.Name, "value", v)
			}
			v = strconv.Itoa(n)
		case "enum":
			ok := false
			for _, o := range def.Options {
				ok = ok || o == v
			}
			if !ok {
				return nil, apperr.New("cmd.invalidParam", "name", def.Name, "value", v)
			}
		case "bool":
			switch strings.ToLower(v) {
			case "true", "1", "yes", "on":
				v = "true"
			case "false", "0", "no", "off":
				v = "false"
			default:
				return nil, apperr.New("cmd.invalidParam", "name", def.Name, "value", v)
			}
		}
		out[def.Name] = v
	}
	for k := range in {
		known := false
		for _, def := range p.Params {
			known = known || def.Name == k
		}
		if !known && strings.TrimSpace(in[k]) != "" {
			return nil, apperr.New("cmd.invalidParam", "name", k, "value", in[k])
		}
	}
	return out, nil
}

// RenderPreset validates the parameters and returns the script to run.
func RenderPreset(id string, params map[string]string) (Rendered, error) {
	p, ok := findPreset(id)
	if !ok {
		return Rendered{}, apperr.New("cmd.unknownPreset", "id", id)
	}
	v, err := resolveParams(p, params)
	if err != nil {
		return Rendered{}, err
	}
	r := Rendered{Sudo: p.Sudo, Danger: p.Danger}
	switch id {
	case "service.status", "service.restart", "service.reload", "service.start", "service.stop":
		r.Command = serviceScript(strings.TrimPrefix(id, "service."), v["unit"])
	case "disk":
		r.Command = "df -hP"
	case "memory":
		r.Command = "free -m"
	case "uptime":
		r.Command = "uptime\necho \"loadavg: $(cat /proc/loadavg)\"\necho \"cpus: $(grep -c ^processor /proc/cpuinfo)\""
	case "os":
		r.Command = strings.Join([]string{
			`if [ -r /etc/os-release ]; then . /etc/os-release; fi`,
			`echo "OS: ${PRETTY_NAME:-$(uname -s)}"`,
			`echo "Kernel: $(uname -r)"`,
			`echo "Arch: $(uname -m)"`,
			`echo "Hostname: $(hostname 2>/dev/null || cat /etc/hostname)"`,
		}, "\n")
	case "updates":
		r.Command = updatesScript(v["refresh"] == "true")
		r.Sudo = v["refresh"] == "true"
	case "docker.ps":
		all := ""
		if v["all"] == "true" {
			all = " -a"
		}
		r.Command = "command -v docker >/dev/null 2>&1 || { echo 'docker: command not found' >&2; exit 127; }\n" +
			"docker ps" + all + " --format 'table {{.Names}}\\t{{.Image}}\\t{{.Status}}\\t{{.Ports}}'"
	case "top":
		key := "-%cpu"
		if v["sort"] == "mem" {
			key = "-%mem"
		}
		n := v["count"]
		// procps ps sorts itself; BusyBox ps has no -o/--sort, fall back to top.
		r.Command = "if ps -eo pid,user,%cpu,%mem,rss,etime,comm --sort=" + key + " >/dev/null 2>&1; then\n" +
			"  ps -eo pid,user,%cpu,%mem,rss,etime,comm --sort=" + key + " | head -n " + strconv.Itoa(mustAtoi(n)+1) + "\n" +
			"else\n" +
			"  top -b -n 1 | head -n " + strconv.Itoa(mustAtoi(n)+4) + "\n" +
			"fi"
	case "reboot":
		// Detach so the command reports success before the connection drops.
		r.Command = "nohup sh -c 'sleep 2; if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then systemctl reboot; else reboot; fi' >/dev/null 2>&1 &\necho 'reboot scheduled in 2s'"
	}
	return r, nil
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// serviceScript runs a service action with systemd, OpenRC or SysV init.
func serviceScript(action, unit string) string {
	q := core.Q(unit)
	sysd := "systemctl " + action + " " + q
	if action == "status" {
		sysd = "systemctl status --no-pager -l " + q
	}
	return "if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then\n" +
		"  " + sysd + "\n" +
		"elif command -v rc-service >/dev/null 2>&1; then\n" +
		"  rc-service " + q + " " + action + "\n" +
		"else\n" +
		"  service " + q + " " + action + "\n" +
		"fi"
}

// updatesScript lists pending package updates with whichever package
// manager exists; refresh first updates the package index (needs root).
func updatesScript(refresh bool) string {
	ref := func(cmd string) string {
		if !refresh {
			return ""
		}
		return "  " + cmd + " || exit $?\n"
	}
	return "if command -v apt-get >/dev/null 2>&1; then\n" +
		ref("apt-get update -qq >/dev/null") +
		"  list=$(apt-get -s -o Debug::NoLocking=true upgrade 2>/dev/null | awk '/^Inst /{print $2\" \"$3\" -> \"$4}' | tr -d '[]()')\n" +
		"  mgr=apt\n" +
		"elif command -v dnf >/dev/null 2>&1; then\n" +
		ref("dnf -q makecache >/dev/null") +
		"  list=$(dnf -q check-update 2>/dev/null | awk 'NF==3 && $1 !~ /^(Obsoleting|Security)/{print $1\" -> \"$2}')\n" +
		"  mgr=dnf\n" +
		"elif command -v yum >/dev/null 2>&1; then\n" +
		ref("yum -q makecache >/dev/null") +
		"  list=$(yum -q check-update 2>/dev/null | awk 'NF==3{print $1\" -> \"$2}')\n" +
		"  mgr=yum\n" +
		"elif command -v apk >/dev/null 2>&1; then\n" +
		ref("apk update -q >/dev/null") +
		"  list=$(apk version -l '<' 2>/dev/null | awk 'NR>1{print $1\" -> \"$3}')\n" +
		"  mgr=apk\n" +
		"else\n" +
		"  echo 'no supported package manager (apt, dnf, yum, apk)' >&2; exit 2\n" +
		"fi\n" +
		"n=$(printf '%s' \"$list\" | grep -c . || true)\n" +
		"echo \"manager: $mgr\"\n" +
		"echo \"pending updates: $n\"\n" +
		"[ -n \"$list\" ] && printf '%s\\n' \"$list\"\n" +
		"exit 0"
}
