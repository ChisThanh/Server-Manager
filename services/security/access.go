package security

// IP access control: one view of the addresses that are blocked or allowed,
// whichever tool does it (ufw, firewalld, iptables, fail2ban), bulk
// block / allow / remove, "SSH only from the allowlist", and fail2ban setup.
//
// Rules created here carry an "sm:" comment (ufw) or "sm-block:"/"sm-allow:"
// (iptables) so the UI can tell them from rules made by hand. The address of
// the current session is never blocked, and nothing that would cut the
// session off is done without an explicit force.

import (
	"context"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

type AccessEntry struct {
	Addr string `json:"addr"` // IP or CIDR, canonical
	Kind string `json:"kind"` // block | allow
	// Source is what enforces it: ufw | firewalld | iptables | fail2ban.
	Source  string `json:"source"`
	Jail    string `json:"jail"`  // fail2ban jail
	Ports   string `json:"ports"` // "" = every port, else "22/tcp", "80,443/tcp"…
	Comment string `json:"comment"`
	Managed bool   `json:"managed"` // added by Server Manager
	// Ref identifies the rule for removal (ufw "show added" line, firewalld
	// zone / rich rule, iptables -S line, jail name).
	Ref string `json:"ref"`
}

type Fail2banConfig struct {
	MaxRetry   int      `json:"maxRetry"`
	FindTime   int      `json:"findTime"` // seconds
	BanTime    int      `json:"banTime"`  // seconds, -1 = forever
	Increment  bool     `json:"increment"`
	MaxTime    int      `json:"maxTime"` // seconds, cap of incremental bans
	Recidive   bool     `json:"recidive"`
	Aggressive bool     `json:"aggressive"`
	IgnoreIP   []string `json:"ignoreIp"`
}

type Fail2banInfo struct {
	Installed bool     `json:"installed"`
	Running   bool     `json:"running"`
	Version   string   `json:"version"`
	Jails     []string `json:"jails"`
	SSHJail   string   `json:"sshJail"`
	// Managed: the app's jail file exists (Config then comes from it).
	Managed bool           `json:"managed"`
	Config  Fail2banConfig `json:"config"`
	// JournalOnly: no auth log file, sshd logs only to the journal (the sshd
	// jail then needs backend = systemd).
	JournalOnly bool   `json:"journalOnly"`
	PkgManager  string `json:"pkgManager"`
}

type AccessState struct {
	// Backend is the firewall the app writes to: firewalld (running) › ufw ›
	// iptables › none.
	Backend string `json:"backend"`
	// Active: that firewall is filtering (an inactive ufw keeps rules but
	// does not apply them).
	Active   bool          `json:"active"`
	ClientIP string        `json:"clientIp"`
	SSHPorts []int         `json:"sshPorts"`
	Entries  []AccessEntry `json:"entries"`
	Fail2ban Fail2banInfo  `json:"fail2ban"`
	// Persist: how iptables rules survive a reboot ("" = they don't).
	Persist string `json:"persist"`
	// SSHLocked: SSH is reachable only from allowlisted addresses.
	SSHLocked bool `json:"sshLocked"`
	// CanLock: the backend supports the SSH lock (ufw, firewalld).
	CanLock bool `json:"canLock"`
	// DefaultDeny: incoming traffic not matched by a rule is dropped.
	DefaultDeny bool `json:"defaultDeny"`
	Docker      bool `json:"docker"`

	ufwPrepend bool
	ufwAdded   []string
	fwdZone    string
	fwdSvc     []string
	fwdPorts   []string
}

type AccessRequest struct {
	Addrs   []string `json:"addrs"`
	Comment string   `json:"comment"`
	// Temporary (block): ban with fail2ban, which lifts it after the jail's
	// ban time, instead of a permanent firewall rule.
	Temporary bool `json:"temporary"`
	// Ports (allow): "" = every port, "ssh" = the SSH ports, or a list/range.
	Ports string `json:"ports"`
}

type AccessSkip struct {
	Addr string `json:"addr"`
	// Reason: invalid | self | local | tooBroad | allowlisted | already |
	// cidrTemp | failed | gone | conflict
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

type AccessResult struct {
	Applied []string     `json:"applied"`
	Skipped []AccessSkip `json:"skipped"`
	// Saved: iptables persistence method used; NotSaved: the rules are live
	// but will not survive a reboot.
	Saved    string `json:"saved"`
	NotSaved bool   `json:"notSaved"`
}

// f2bFile is the app's jail file; the "zz-" prefix makes it load last.
const f2bFile = "/etc/fail2ban/jail.d/zz-server-manager.local"

// maxAccessBatch caps addresses per request.
const maxAccessBatch = 500

const accessScript = `echo @@ufw; command -v ufw >/dev/null 2>&1 && { echo installed; ufw status verbose 2>&1 | head -n 4; }
echo @@ufwadded; command -v ufw >/dev/null 2>&1 && ufw show added 2>&1
echo @@ufwprepend; command -v ufw >/dev/null 2>&1 && ufw --help 2>&1 | grep -c prepend
echo @@firewalld; if command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
  echo running
  echo @@fwdzone; firewall-cmd --get-default-zone 2>/dev/null
  for z in drop block trusted; do echo "@@fwdsrc:$z"; firewall-cmd --permanent --zone=$z --list-sources 2>/dev/null | tr ' ' '\n'; done
  echo @@fwdrich; firewall-cmd --permanent --list-rich-rules 2>/dev/null
  echo @@fwdsvc; firewall-cmd --permanent --list-services 2>/dev/null | tr ' ' '\n'
  echo @@fwdports; firewall-cmd --permanent --list-ports 2>/dev/null | tr ' ' '\n'
fi
echo @@iptables; command -v iptables >/dev/null 2>&1 && echo installed
echo @@ipt4; command -v iptables >/dev/null 2>&1 && iptables -S INPUT 2>/dev/null
echo @@ipt6; command -v ip6tables >/dev/null 2>&1 && ip6tables -S INPUT 2>/dev/null
echo @@persist; ` + persistDetect + `
echo @@ssh; sshd -T 2>/dev/null | awk '$1=="port"{print $2}'
if command -v fail2ban-client >/dev/null 2>&1; then
  echo @@f2b
  fail2ban-client --version 2>/dev/null | head -n 1
  if fail2ban-client ping >/dev/null 2>&1; then
    echo running
    for j in $(fail2ban-client status 2>/dev/null | sed -n 's/.*Jail list:[[:space:]]*//p' | tr ',' ' '); do echo "@@jail:$j"; fail2ban-client status "$j" 2>/dev/null; done
    if fail2ban-client status sshd >/dev/null 2>&1; then
      echo @@f2bget
      for k in bantime findtime maxretry; do echo "$k=$(fail2ban-client get sshd $k 2>/dev/null)"; done
      echo @@f2bignore; fail2ban-client get sshd ignoreip 2>/dev/null
    fi
  else echo stopped; fi
fi
echo @@f2bfile; cat ` + f2bFile + ` 2>/dev/null
echo @@pkg; for p in apt-get dnf yum apk zypper pacman; do command -v $p >/dev/null 2>&1 && { echo $p; break; }; done
echo @@logs; for f in /var/log/auth.log /var/log/secure; do [ -f $f ] && echo $f; done; command -v journalctl >/dev/null 2>&1 && echo journal
echo @@docker; command -v docker >/dev/null 2>&1 && echo yes
echo @@end`

// ---- reading ----

// AccessList returns every blocked/allowed address and the fail2ban setup.
func (s *SecurityService) AccessList(connID, sudoPassword string) (AccessState, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return AccessState{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	return s.access(ctx, conn, sudoPassword)
}

func (s *SecurityService) access(ctx context.Context, conn *sshx.Conn, pw string) (AccessState, error) {
	res, err := s.root(ctx, conn, accessScript, pw, "")
	if err != nil {
		return AccessState{}, err
	}
	st := parseAccess(res.Stdout)
	ip, port := s.sessionClient(ctx, conn)
	st.ClientIP = ip
	if validPort(port) && !slices.Contains(st.SSHPorts, port) {
		st.SSHPorts = append(st.SSHPorts, port)
	}
	st.SSHLocked = st.sshLocked()
	return st, nil
}

func parseAccess(out string) AccessState {
	sec := sections(out)
	st := AccessState{Backend: "none", Entries: []AccessEntry{}, SSHPorts: []int{}}
	st.Fail2ban = Fail2banInfo{Jails: []string{}, Config: defaultF2B()}
	for _, l := range lines(sec["ssh"]) {
		if n := atoi(l); validPort(n) && !slices.Contains(st.SSHPorts, n) {
			st.SSHPorts = append(st.SSHPorts, n)
		}
	}
	ufw := lines(sec["ufw"])
	ufwInstalled := len(ufw) > 0 && ufw[0] == "installed"
	ufwActive := false
	ufwDeny := false
	for _, l := range ufw {
		if v, ok := strings.CutPrefix(l, "Status:"); ok {
			ufwActive = strings.TrimSpace(v) == "active"
		}
		if v, ok := strings.CutPrefix(l, "Default:"); ok {
			in, _, _ := strings.Cut(v, ",")
			ufwDeny = strings.Contains(in, "deny") || strings.Contains(in, "reject")
		}
	}
	fwd := strings.TrimSpace(sec["firewalld"]) == "running"
	switch {
	case fwd:
		st.Backend, st.Active, st.CanLock, st.DefaultDeny = "firewalld", true, true, true
	case ufwInstalled:
		st.Backend, st.Active, st.CanLock, st.DefaultDeny = "ufw", ufwActive, true, ufwDeny
	case strings.TrimSpace(sec["iptables"]) == "installed":
		st.Backend, st.Active = "iptables", true
	}
	st.Persist = strings.TrimSpace(sec["persist"])
	st.Docker = strings.TrimSpace(sec["docker"]) == "yes"

	switch st.Backend {
	case "ufw":
		st.ufwPrepend = atoi(strings.TrimSpace(sec["ufwprepend"])) > 0
		for _, l := range lines(sec["ufwadded"]) {
			l = strings.TrimSpace(l)
			if !strings.HasPrefix(l, "ufw ") {
				continue
			}
			st.ufwAdded = append(st.ufwAdded, l)
			if e, ok := parseUFWAccess(l); ok {
				st.Entries = append(st.Entries, e)
			}
		}
	case "firewalld":
		st.fwdZone = strings.TrimSpace(sec["fwdzone"])
		st.fwdSvc = lines(sec["fwdsvc"])
		st.fwdPorts = lines(sec["fwdports"])
		for _, z := range []string{"drop", "block", "trusted"} {
			for _, a := range lines(sec["fwdsrc:"+z]) {
				addr, err := normAddr(a)
				if err != nil || addr == "any" {
					continue
				}
				kind := "block"
				if z == "trusted" {
					kind = "allow"
				}
				st.Entries = append(st.Entries, AccessEntry{Addr: addr, Kind: kind, Source: "firewalld", Comment: "zone " + z, Ref: "zone:" + z + ":" + addr})
			}
		}
		for _, l := range lines(sec["fwdrich"]) {
			if e, ok := parseRichAccess(l, st.fwdZone); ok {
				st.Entries = append(st.Entries, e)
			}
		}
	case "iptables":
		for _, fam := range []string{"4", "6"} {
			for _, l := range lines(sec["ipt"+fam]) {
				if e, ok := parseIptAccess(l, fam); ok {
					st.Entries = append(st.Entries, e)
				}
			}
		}
	}

	// fail2ban
	f := &st.Fail2ban
	if v, ok := sec["f2b"]; ok {
		f.Installed = true
		ls := lines(v)
		for _, l := range ls {
			switch {
			case l == "running":
				f.Running = true
			case strings.Contains(strings.ToLower(l), "fail2ban v") || strings.HasPrefix(l, "Fail2Ban"):
				f.Version = strings.TrimSpace(l[strings.LastIndex(l, "v")+1:])
			}
		}
	}
	jails := []string{}
	for k := range sec {
		if n, ok := strings.CutPrefix(k, "jail:"); ok {
			jails = append(jails, n)
		}
	}
	sort.Strings(jails)
	f.Jails = jails
	if slices.Contains(jails, "sshd") {
		f.SSHJail = "sshd"
	}
	for _, j := range parseFail2ban(sec).Jails {
		for _, ip := range j.Banned {
			if addr, err := normAddr(ip); err == nil && addr != "any" {
				st.Entries = append(st.Entries, AccessEntry{Addr: addr, Kind: "block", Source: "fail2ban", Jail: j.Name, Ref: j.Name})
			}
		}
	}
	if file := sec["f2bfile"]; strings.TrimSpace(file) != "" {
		f.Managed = true
		f.Config = parseF2BFile(file)
	} else if get := kv(sec["f2bget"]); len(get) > 0 {
		c := f.Config
		if n, err := strconv.Atoi(get["bantime"]); err == nil {
			c.BanTime = n
		}
		if n, err := strconv.Atoi(get["findtime"]); err == nil {
			c.FindTime = n
		}
		if n, err := strconv.Atoi(get["maxretry"]); err == nil {
			c.MaxRetry = n
		}
		c.IgnoreIP = parseF2BIgnore(sec["f2bignore"])
		c.Increment, c.Recidive = false, slices.Contains(jails, "recidive")
		f.Config = c
	}
	logs := lines(sec["logs"])
	f.JournalOnly = len(logs) == 1 && logs[0] == "journal"
	f.PkgManager = strings.TrimSuffix(strings.TrimSpace(sec["pkg"]), "-get")
	return st
}

// parseUFWAccess reads an address-specific rule from `ufw show added`.
func parseUFWAccess(line string) (AccessEntry, bool) {
	r, ok := parseUFWLine(line)
	if !ok || r.from == "any" || r.dir == "out" {
		return AccessEntry{}, false
	}
	kind := ""
	switch r.action {
	case "allow":
		kind = "allow"
	case "deny", "reject":
		kind = "block"
	default:
		return AccessEntry{}, false
	}
	e := AccessEntry{Addr: r.from, Kind: kind, Source: "ufw", Comment: r.comment, Ref: line, Managed: strings.HasPrefix(r.comment, "sm:")}
	if r.port != "" {
		e.Ports = r.port
		if r.proto != "" && r.proto != "any" {
			e.Ports += "/" + r.proto
		}
	}
	return e, true
}

type ufwLine struct {
	action, dir, from, to, port, proto, app, comment string
	simple                                           string // "ufw allow 22/tcp" form: the port spec
}

// parseUFWLine parses a `ufw show added` command line.
func parseUFWLine(line string) (ufwLine, bool) {
	w, ok := shellFields(line)
	if !ok || len(w) < 3 || w[0] != "ufw" {
		return ufwLine{}, false
	}
	r := ufwLine{action: w[1], from: "any", to: "any"}
	i := 2
	if w[i] == "in" || w[i] == "out" {
		r.dir = w[i]
		i++
	}
	if i+1 < len(w) && w[i] == "on" {
		i += 2
	}
	if i < len(w) && !slices.Contains([]string{"from", "to", "proto", "app", "comment"}, w[i]) {
		r.simple = w[i]
		i++
	}
	last := ""
	for i < len(w) {
		k := w[i]
		v := ""
		if i+1 < len(w) {
			v = w[i+1]
		}
		switch k {
		case "from", "to":
			last = k
			if k == "from" {
				r.from = v
			} else {
				r.to = v
			}
			i += 2
		case "port":
			if last == "to" || last == "" {
				r.port = v
			}
			i += 2
		case "proto":
			r.proto = v
			i += 2
		case "app":
			r.app = v
			i += 2
		case "comment":
			r.comment = v
			i += 2
		default:
			i++
		}
	}
	if r.from != "any" {
		a, err := normAddr(r.from)
		if err != nil {
			return ufwLine{}, false
		}
		r.from = a
	}
	return r, true
}

// ufwOpensSSH: a rule that lets everyone reach an SSH port.
func ufwOpensSSH(line string, sshPorts []int) bool {
	r, ok := parseUFWLine(line)
	if !ok || r.dir == "out" || (r.action != "allow" && r.action != "limit") || r.from != "any" {
		return false
	}
	spec := r.simple
	if spec == "" {
		spec = r.port
		if r.app != "" {
			spec = r.app
		}
		if spec == "" && r.to == "any" {
			return true // allow from any: everything
		}
	}
	if strings.EqualFold(spec, "OpenSSH") || strings.EqualFold(spec, "ssh") {
		return slices.Contains(sshPorts, 22)
	}
	pp, pr, _ := strings.Cut(spec, "/")
	if pr == "udp" || r.proto == "udp" {
		return false
	}
	ps, err := parsePortSpec(pp, ":")
	if err != nil {
		return false
	}
	for _, p := range sshPorts {
		if ps.covers(p) {
			return true
		}
	}
	return false
}

var reRich = regexp.MustCompile(`^rule family="(ipv4|ipv6)" source address="([^"]+)"(?: port port="([^"]+)" protocol="(tcp|udp)")? (accept|drop|reject)\b`)

func parseRichAccess(rule, zone string) (AccessEntry, bool) {
	m := reRich.FindStringSubmatch(strings.TrimSpace(rule))
	if m == nil {
		return AccessEntry{}, false
	}
	addr, err := normAddr(m[2])
	if err != nil || addr == "any" {
		return AccessEntry{}, false
	}
	kind := "block"
	if m[5] == "accept" {
		kind = "allow"
	}
	e := AccessEntry{Addr: addr, Kind: kind, Source: "firewalld", Comment: "rich rule", Ref: "rich:" + zone + ":" + strings.TrimSpace(rule)}
	if m[3] != "" {
		e.Ports = m[3] + "/" + m[4]
	}
	return e, true
}

// parseIptAccess reads "-A INPUT -s ADDR … -j DROP|REJECT|ACCEPT".
func parseIptAccess(line, fam string) (AccessEntry, bool) {
	w, ok := shellFields(line)
	if !ok || len(w) < 4 || w[0] != "-A" || w[1] != "INPUT" || slices.Contains(w, "!") {
		return AccessEntry{}, false
	}
	var src, target, proto, ports, comment string
	for i := 2; i < len(w)-1; i++ {
		switch w[i] {
		case "-s", "--source":
			src = w[i+1]
		case "-j":
			target = w[i+1]
		case "-p":
			proto = w[i+1]
		case "--dport", "--dports":
			ports = w[i+1]
		case "--comment":
			comment = w[i+1]
		case "-i", "-m", "--state", "--ctstate", "--sport":
		}
	}
	if src == "" {
		return AccessEntry{}, false
	}
	kind := ""
	switch target {
	case "DROP", "REJECT":
		kind = "block"
	case "ACCEPT":
		kind = "allow"
	default:
		return AccessEntry{}, false
	}
	addr, err := normAddr(strings.TrimSuffix(strings.TrimSuffix(src, "/32"), "/128"))
	if err != nil || addr == "any" {
		return AccessEntry{}, false
	}
	e := AccessEntry{Addr: addr, Kind: kind, Source: "iptables", Comment: comment, Ref: fam + "|" + line,
		Managed: strings.HasPrefix(comment, "sm-")}
	if ports != "" {
		e.Ports = ports + "/" + proto
	} else if proto != "" && proto != "all" {
		e.Ports = proto
	}
	return e, true
}

func defaultF2B() Fail2banConfig {
	return Fail2banConfig{MaxRetry: 5, FindTime: 600, BanTime: 3600, Increment: true, MaxTime: 7 * 86400, Recidive: true, IgnoreIP: []string{}}
}

// parseF2BIgnore reads `fail2ban-client get JAIL ignoreip`.
func parseF2BIgnore(out string) []string {
	res := []string{}
	for _, l := range lines(out) {
		l = strings.TrimLeft(l, "|`- \t")
		if a, err := normAddr(l); err == nil && a != "any" && !isDefaultIgnore(a) {
			res = append(res, a)
		}
	}
	return res
}

func isDefaultIgnore(a string) bool {
	return a == "127.0.0.0/8" || a == "127.0.0.1/8" || a == "::1" || a == "127.0.0.1"
}

// parseF2BFile reads the app's jail file back into a config.
func parseF2BFile(s string) Fail2banConfig {
	c := defaultF2B()
	c.Recidive, c.Increment = false, false
	sect := ""
	for _, l := range lines(s) {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "[") {
			sect = strings.Trim(l, "[]")
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch sect + "." + k {
		case "DEFAULT.ignoreip":
			c.IgnoreIP = []string{}
			for _, a := range strings.Fields(v) {
				if n, err := normAddr(a); err == nil && n != "any" && !isDefaultIgnore(n) {
					c.IgnoreIP = append(c.IgnoreIP, n)
				}
			}
		case "DEFAULT.bantime":
			c.BanTime = atoi(v)
		case "DEFAULT.findtime":
			c.FindTime = atoi(v)
		case "DEFAULT.maxretry":
			c.MaxRetry = atoi(v)
		case "DEFAULT.bantime.increment":
			c.Increment = v == "true"
		case "DEFAULT.bantime.maxtime":
			c.MaxTime = atoi(v)
		case "sshd.mode":
			c.Aggressive = v == "aggressive"
		case "recidive.enabled":
			c.Recidive = v == "true"
		}
	}
	return c
}

// sshLocked: the firewall filters and nothing lets everyone reach SSH.
func (st AccessState) sshLocked() bool {
	switch st.Backend {
	case "ufw":
		if !st.Active || !st.DefaultDeny {
			return false
		}
		for _, l := range st.ufwAdded {
			if ufwOpensSSH(l, st.SSHPorts) {
				return false
			}
		}
		return true
	case "firewalld":
		return !fwdOpensSSH(st.fwdSvc, st.fwdPorts, st.SSHPorts)
	}
	return false
}

func fwdOpensSSH(svc, ports []string, sshPorts []int) bool {
	if slices.Contains(svc, "ssh") && slices.Contains(sshPorts, 22) {
		return true
	}
	for _, p := range ports {
		pp, pr, _ := strings.Cut(p, "/")
		if pr != "tcp" {
			continue
		}
		ps, err := parsePortSpec(pp, "-")
		if err != nil {
			continue
		}
		for _, sp := range sshPorts {
			if ps.covers(sp) {
				return true
			}
		}
	}
	return false
}

// ---- validation ----

// accessAddr validates an address to block/allow.
func accessAddr(v string) (string, string) {
	a, err := normAddr(v)
	if err != nil || a == "any" {
		return "", "invalid"
	}
	ip, n, isNet := net.ParseIP(a), (*net.IPNet)(nil), false
	if ip == nil {
		var err error
		ip, n, err = net.ParseCIDR(a)
		if err != nil {
			return "", "invalid"
		}
		isNet = true
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return "", "local"
	}
	if isNet {
		ones, bits := n.Mask.Size()
		if (bits == 32 && ones < 8) || (bits == 128 && ones < 16) {
			return "", "tooBroad"
		}
	}
	return a, ""
}

// addrIn reports whether outer (IP or CIDR) contains inner (IP or CIDR).
func addrIn(outer, inner string) bool {
	if outer == inner {
		return true
	}
	_, on, err := net.ParseCIDR(outer)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(inner); ip != nil {
		return on.Contains(ip)
	}
	ii, in, err := net.ParseCIDR(inner)
	if err != nil {
		return false
	}
	oo, _ := on.Mask.Size()
	io, _ := in.Mask.Size()
	return io >= oo && on.Contains(ii)
}

func isV6(addr string) bool {
	return net.ParseIP(strings.Split(addr, "/")[0]).To4() == nil
}

func (st AccessState) covered(addr, kind, source string) bool {
	for _, e := range st.Entries {
		if e.Kind == kind && (source == "" || e.Source == source) && addrIn(e.Addr, addr) {
			return true
		}
	}
	return false
}

// ---- batch runner ----

// batchScript runs each command, printing "@@ok i" or "@@fail i" + output.
func batchScript(cmds []string) string {
	var b strings.Builder
	b.WriteString("t() { k=\"$1\"; shift; if out=$(\"$@\" 2>&1); then echo \"@@ok $k\"; else echo \"@@fail $k\"; printf '%s\\n' \"$out\" | head -n 3; fi; }\n")
	for i, c := range cmds {
		if c == "" {
			continue
		}
		b.WriteString("t " + strconv.Itoa(i) + " " + c + "\n")
	}
	return b.String()
}

// batchResult reads the markers: index → error detail ("" = ok).
func batchResult(out string) map[int]string {
	res := map[int]string{}
	cur := -1
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(l, "@@ok "):
			cur = -1
			res[atoi(l[5:])] = ""
		case strings.HasPrefix(l, "@@fail "):
			cur = atoi(l[7:])
			res[cur] = "failed"
		case cur >= 0 && strings.TrimSpace(l) != "":
			if res[cur] == "failed" {
				res[cur] = strings.TrimSpace(l)
			} else {
				res[cur] += "; " + strings.TrimSpace(l)
			}
		}
	}
	return res
}

const iptablesSaveScript = `M=$(` + persistDetect + `)
case "$M" in
netfilter-persistent) netfilter-persistent save >/dev/null 2>&1 || exit 3 ;;
openrc) rc-service iptables save >/dev/null 2>&1 || exit 3; [ -x /etc/init.d/ip6tables ] && rc-service ip6tables save >/dev/null 2>&1 ;;
sysconfig) if [ -x /usr/libexec/iptables/iptables.init ]; then /usr/libexec/iptables/iptables.init save || exit 3; else iptables-save > /etc/sysconfig/iptables || exit 3; fi ;;
rules.v4) iptables-save > /etc/iptables/rules.v4 || exit 3; command -v ip6tables-save >/dev/null 2>&1 && ip6tables-save > /etc/iptables/rules.v6 ;;
*) echo @@none; exit 0 ;;
esac
echo "@@method $M"`

// saveIptables persists iptables rules when the server has a way to.
func (s *SecurityService) saveIptables(ctx context.Context, conn *sshx.Conn, pw string, r *AccessResult) {
	res, err := s.root(ctx, conn, iptablesSaveScript, pw, "")
	if err != nil || res.ExitCode != 0 || strings.Contains(res.Stdout, "@@none") {
		r.NotSaved = true
		return
	}
	_, m, _ := strings.Cut(res.Stdout, "@@method ")
	r.Saved = strings.TrimSpace(m)
}

func accessComment(prefix, c string) string {
	c = sanitizeComment(c, 60)
	if c == "" {
		return prefix // the bare marker still identifies the app's rules
	}
	return prefix + " " + c
}

// ---- block ----

// AccessBlock blocks addresses: a firewall rule placed before every allow
// rule, or (Temporary) a fail2ban ban that expires.
func (s *SecurityService) AccessBlock(connID string, req AccessRequest, sudoPassword string) (AccessResult, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return AccessResult{}, err
	}
	backend := ""
	res, err := func() (AccessResult, error) {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return AccessResult{}, err
		}
		ctx, cancel := core.Timeout(5 * time.Minute)
		defer cancel()
		st, err := s.access(ctx, conn, sudoPassword)
		if err != nil {
			return AccessResult{}, err
		}
		backend = st.Backend
		if req.Temporary {
			backend = "fail2ban"
		}
		return s.block(ctx, conn, st, req, sudoPassword)
	}()
	s.core.Audit(connID, "sec.access.block", backend, auditList(res.Applied, req.Addrs, err), err)
	return res, err
}

func auditList(applied, asked []string, err error) string {
	l := applied
	if err != nil || len(l) == 0 {
		l = asked
	}
	if len(l) > 20 {
		return strings.Join(l[:20], " ") + " … (+" + strconv.Itoa(len(l)-20) + ")"
	}
	return strings.Join(l, " ")
}

func (s *SecurityService) block(ctx context.Context, conn *sshx.Conn, st AccessState, req AccessRequest, pw string) (AccessResult, error) {
	r := AccessResult{Applied: []string{}, Skipped: []AccessSkip{}}
	if len(req.Addrs) > maxAccessBatch {
		return r, apperr.New("sec.acc.tooMany", "max", strconv.Itoa(maxAccessBatch))
	}
	if req.Temporary && (!st.Fail2ban.Running || st.Fail2ban.SSHJail == "") {
		return r, apperr.New("sec.acc.f2bNotRunning")
	}
	if !req.Temporary && st.Backend == "none" {
		return r, apperr.New("sec.acc.noFirewall")
	}
	source := st.Backend
	if req.Temporary {
		source = "fail2ban"
	}
	addrs := []string{}
	for _, raw := range req.Addrs {
		a, why := accessAddr(raw)
		switch {
		case why != "":
			r.Skipped = append(r.Skipped, AccessSkip{Addr: strings.TrimSpace(raw), Reason: why})
		case slices.Contains(addrs, a):
		case st.ClientIP != "" && addrIn(a, st.ClientIP):
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "self"})
		case st.covered(a, "allow", ""):
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "allowlisted"})
		case st.covered(a, "block", source):
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "already"})
		case req.Temporary && strings.Contains(a, "/"):
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "cidrTemp"})
		default:
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return r, nil
	}
	cmds := make([]string, len(addrs))
	tail := ""
	for i, a := range addrs {
		switch {
		case req.Temporary:
			cmds[i] = "fail2ban-client set " + core.Q(st.Fail2ban.SSHJail) + " banip " + core.Q(a)
		case source == "ufw":
			cmds[i] = ufwInsert(st, isV6(a)) + " deny from " + core.Q(a) + " comment " + core.Q(accessComment("sm:", req.Comment))
		case source == "firewalld":
			cmds[i] = "firewall-cmd --permanent --zone=drop --add-source=" + core.Q(a)
			tail = "firewall-cmd --reload >/dev/null 2>&1"
		case source == "iptables":
			cmds[i] = iptBin(a) + " -I INPUT 1 -s " + core.Q(a) + " -m comment --comment " + core.Q(accessComment("sm-block:", req.Comment)) + " -j DROP"
		}
	}
	if err := s.runBatch(ctx, conn, pw, cmds, tail, addrs, &r); err != nil {
		return r, err
	}
	if source == "iptables" && len(r.Applied) > 0 {
		s.saveIptables(ctx, conn, pw, &r)
	}
	return r, nil
}

// ufwInsert returns the ufw verb that puts a rule before the others of
// its IP family ("insert 1" cannot target v6 rules on old ufw: append).
func ufwInsert(st AccessState, v6 bool) string {
	switch {
	case st.ufwPrepend:
		return "ufw prepend"
	case v6:
		return "ufw"
	default:
		return "ufw insert 1"
	}
}

func iptBin(addr string) string {
	if isV6(addr) {
		return "ip6tables"
	}
	return "iptables"
}

// runBatch runs cmds (one per addr) plus a tail command, filling r.
func (s *SecurityService) runBatch(ctx context.Context, conn *sshx.Conn, pw string, cmds []string, tail string, addrs []string, r *AccessResult) error {
	script := batchScript(cmds)
	if tail != "" {
		script += tail + "\n"
	}
	res, err := s.root(ctx, conn, script, pw, "")
	if err != nil {
		return err
	}
	out := batchResult(res.Stdout)
	for i, a := range addrs {
		if cmds[i] == "" {
			continue
		}
		msg, ran := out[i]
		switch {
		case !ran:
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "failed", Detail: "no result"})
		case msg == "":
			r.Applied = append(r.Applied, a)
		case strings.Contains(msg, "ZONE_CONFLICT"):
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "conflict", Detail: msg})
		default:
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "failed", Detail: msg})
		}
	}
	if len(r.Applied) == 0 && len(r.Skipped) > 0 && r.Skipped[len(r.Skipped)-1].Reason == "failed" {
		return apperr.New("sec.acc.failed").WithDetail(r.Skipped[len(r.Skipped)-1].Detail)
	}
	return nil
}

// ---- allow ----

// AccessAllow allowlists addresses: on every port, the SSH ports or given
// ports. Allow rules go before block rules; fail2ban also ignores them.
func (s *SecurityService) AccessAllow(connID string, req AccessRequest, sudoPassword string) (AccessResult, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return AccessResult{}, err
	}
	backend := ""
	res, err := func() (AccessResult, error) {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return AccessResult{}, err
		}
		ctx, cancel := core.Timeout(5 * time.Minute)
		defer cancel()
		st, err := s.access(ctx, conn, sudoPassword)
		if err != nil {
			return AccessResult{}, err
		}
		backend = st.Backend
		return s.allow(ctx, conn, st, req, sudoPassword)
	}()
	s.core.Audit(connID, "sec.access.allow", backend, auditList(res.Applied, req.Addrs, err)+portsNote(req.Ports), err)
	return res, err
}

func portsNote(p string) string {
	if p == "" {
		return ""
	}
	return " port " + p
}

// allowPorts resolves AccessRequest.Ports to a port spec ("" = all).
func allowPorts(p string, sshPorts []int) (portSpec, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return portSpec{}, nil
	}
	if strings.EqualFold(p, "ssh") {
		ss := []string{}
		for _, n := range sshPorts {
			ss = append(ss, strconv.Itoa(n))
		}
		if len(ss) == 0 {
			ss = []string{"22"}
		}
		p = strings.Join(ss, ",")
	}
	return parsePortSpec(p, ":")
}

func (s *SecurityService) allow(ctx context.Context, conn *sshx.Conn, st AccessState, req AccessRequest, pw string) (AccessResult, error) {
	r := AccessResult{Applied: []string{}, Skipped: []AccessSkip{}}
	if len(req.Addrs) > maxAccessBatch {
		return r, apperr.New("sec.acc.tooMany", "max", strconv.Itoa(maxAccessBatch))
	}
	if st.Backend == "none" && !st.Fail2ban.Running {
		return r, apperr.New("sec.acc.noFirewall")
	}
	ports, err := allowPorts(req.Ports, st.SSHPorts)
	if err != nil {
		return r, err
	}
	addrs := []string{}
	for _, raw := range req.Addrs {
		a, why := accessAddr(raw)
		switch {
		case why == "local":
			// allowing localhost is harmless and pointless
			r.Skipped = append(r.Skipped, AccessSkip{Addr: strings.TrimSpace(raw), Reason: why})
		case why != "":
			r.Skipped = append(r.Skipped, AccessSkip{Addr: strings.TrimSpace(raw), Reason: why})
		case slices.Contains(addrs, a):
		case ports.Raw == "" && st.covered(a, "allow", st.Backend):
			r.Skipped = append(r.Skipped, AccessSkip{Addr: a, Reason: "already"})
		default:
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return r, nil
	}
	cmds := make([]string, len(addrs))
	tail := ""
	for i, a := range addrs {
		v6 := isV6(a)
		switch st.Backend {
		case "ufw":
			c := ufwInsert(st, v6) + " allow from " + core.Q(a)
			if ports.Raw != "" {
				c += " to any port " + core.Q(ports.Raw) + " proto tcp"
			}
			cmds[i] = c + " comment " + core.Q(accessComment("sm:", req.Comment))
		case "firewalld":
			fam := "ipv4"
			if v6 {
				fam = "ipv6"
			}
			pre := ""
			if st.covered(a, "block", "firewalld") {
				// a source can only be in one zone
				pre = "{ firewall-cmd --permanent --zone=drop --remove-source=" + core.Q(a) + " >/dev/null 2>&1; true; } && "
			}
			if ports.Raw == "" {
				cmds[i] = "sh -c " + core.Q(pre+"firewall-cmd --permanent --zone=trusted --add-source="+core.Q(a))
			} else {
				rules := []string{}
				for _, rg := range ports.Ranges {
					pp := strconv.Itoa(rg[0])
					if rg[1] != rg[0] {
						pp += "-" + strconv.Itoa(rg[1])
					}
					rule := `rule family="` + fam + `" source address="` + a + `" port port="` + pp + `" protocol="tcp" accept`
					rules = append(rules, "firewall-cmd --permanent --add-rich-rule="+core.Q(rule))
				}
				cmds[i] = "sh -c " + core.Q(pre+strings.Join(rules, " && "))
			}
			tail = "firewall-cmd --reload >/dev/null 2>&1"
		case "iptables":
			c := iptBin(a) + " -I INPUT 1 -s " + core.Q(a)
			if ports.Raw != "" {
				c += " -p tcp -m multiport --dports " + core.Q(strings.ReplaceAll(ports.Raw, "-", ":"))
			}
			cmds[i] = c + " -m comment --comment " + core.Q(accessComment("sm-allow:", req.Comment)) + " -j ACCEPT"
		default:
			cmds[i] = "true"
		}
	}
	if err := s.runBatch(ctx, conn, pw, cmds, tail, addrs, &r); err != nil {
		return r, err
	}
	if st.Backend == "iptables" && len(r.Applied) > 0 {
		s.saveIptables(ctx, conn, pw, &r)
	}
	// fail2ban must never ban allowlisted addresses.
	if len(r.Applied) > 0 && st.Fail2ban.Installed {
		s.f2bIgnore(ctx, conn, st, r.Applied, true, pw)
	}
	return r, nil
}

// f2bIgnore adds/removes addresses from fail2ban's ignore list: in the
// app's jail file (persistent) and in the running jails.
func (s *SecurityService) f2bIgnore(ctx context.Context, conn *sshx.Conn, st AccessState, addrs []string, add bool, pw string) {
	f := st.Fail2ban
	var b strings.Builder
	if f.Running {
		verb := "delignoreip"
		if add {
			verb = "addignoreip"
		}
		for _, j := range f.Jails {
			for _, a := range addrs {
				b.WriteString("fail2ban-client set " + core.Q(j) + " " + verb + " " + core.Q(a) + " >/dev/null 2>&1\n")
			}
		}
	}
	if f.Managed {
		c := f.Config
		for _, a := range addrs {
			i := slices.Index(c.IgnoreIP, a)
			switch {
			case add && i < 0:
				c.IgnoreIP = append(c.IgnoreIP, a)
			case !add && i >= 0:
				c.IgnoreIP = slices.Delete(c.IgnoreIP, i, i+1)
			}
		}
		b.WriteString(heredoc(f2bFile, fail2banFile(c, st.SSHPorts, f.JournalOnly)))
	}
	if b.Len() > 0 {
		_, _ = s.root(ctx, conn, b.String(), pw, "")
	}
}

// ---- remove ----

// AccessRemove removes block/allow entries (as listed by AccessList).
// Removing an allow entry that the current session relies on while SSH is
// locked to the allowlist needs force.
func (s *SecurityService) AccessRemove(connID string, entries []AccessEntry, force bool, sudoPassword string) (AccessResult, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return AccessResult{}, err
	}
	asked := []string{}
	for _, e := range entries {
		asked = append(asked, e.Kind+":"+e.Addr)
	}
	res, err := func() (AccessResult, error) {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return AccessResult{}, err
		}
		ctx, cancel := core.Timeout(5 * time.Minute)
		defer cancel()
		st, err := s.access(ctx, conn, sudoPassword)
		if err != nil {
			return AccessResult{}, err
		}
		return s.remove(ctx, conn, st, entries, force, sudoPassword)
	}()
	s.core.Audit(connID, "sec.access.remove", "", auditList(res.Applied, asked, err), err)
	return res, err
}

func (s *SecurityService) remove(ctx context.Context, conn *sshx.Conn, st AccessState, entries []AccessEntry, force bool, pw string) (AccessResult, error) {
	r := AccessResult{Applied: []string{}, Skipped: []AccessSkip{}}
	if len(entries) > maxAccessBatch {
		return r, apperr.New("sec.acc.tooMany", "max", strconv.Itoa(maxAccessBatch))
	}
	// Only entries that still exist, exactly as listed.
	live := func(e AccessEntry) bool {
		for _, x := range st.Entries {
			if x.Source == e.Source && x.Ref == e.Ref && x.Addr == e.Addr && x.Kind == e.Kind {
				return true
			}
		}
		return false
	}
	if !force && st.SSHLocked && st.ClientIP != "" {
		for _, e := range entries {
			if e.Kind == "allow" && addrIn(e.Addr, st.ClientIP) && live(e) && !st.otherAllowCovers(e, st.ClientIP) {
				return r, apperr.New("sec.acc.removeSelf", "addr", e.Addr)
			}
		}
	}
	cmds := []string{}
	addrs := []string{}
	unignored := []string{}
	fwd, ipt := false, false
	for _, e := range entries {
		if !live(e) {
			r.Skipped = append(r.Skipped, AccessSkip{Addr: e.Addr, Reason: "gone"})
			continue
		}
		c := ""
		switch e.Source {
		case "ufw":
			words, ok := shellFields(e.Ref)
			if !ok || len(words) < 3 || words[0] != "ufw" {
				r.Skipped = append(r.Skipped, AccessSkip{Addr: e.Addr, Reason: "gone"})
				continue
			}
			args := []string{"ufw", "--force", "delete"}
			bad := false
			for _, w := range words[1:] {
				if reCtl.MatchString(w) {
					bad = true
				}
				args = append(args, core.Q(w))
			}
			if bad {
				continue
			}
			c = strings.Join(args, " ")
		case "firewalld":
			fwd = true
			if rest, ok := strings.CutPrefix(e.Ref, "zone:"); ok {
				zone, _, _ := strings.Cut(rest, ":")
				if !reZone.MatchString(zone) {
					continue
				}
				c = "firewall-cmd --permanent --zone=" + core.Q(zone) + " --remove-source=" + core.Q(e.Addr)
			} else if rest, ok := strings.CutPrefix(e.Ref, "rich:"); ok {
				zone, rule, _ := strings.Cut(rest, ":")
				if !reZone.MatchString(zone) || reCtl.MatchString(rule) {
					continue
				}
				c = "firewall-cmd --permanent --zone=" + core.Q(zone) + " --remove-rich-rule=" + core.Q(rule)
			}
		case "iptables":
			fam, line, _ := strings.Cut(e.Ref, "|")
			words, ok := shellFields(line)
			if !ok || len(words) < 3 || words[0] != "-A" || words[1] != "INPUT" {
				continue
			}
			bin := "iptables"
			if fam == "6" {
				bin = "ip6tables"
			}
			args := []string{bin, "-D", "INPUT"}
			for _, w := range words[2:] {
				if reCtl.MatchString(w) {
					continue
				}
				args = append(args, core.Q(w))
			}
			c = strings.Join(args, " ")
			ipt = true
		case "fail2ban":
			if !reJail.MatchString(e.Jail) {
				continue
			}
			c = "fail2ban-client set " + core.Q(e.Jail) + " unbanip " + core.Q(e.Addr)
		}
		if c == "" {
			r.Skipped = append(r.Skipped, AccessSkip{Addr: e.Addr, Reason: "failed"})
			continue
		}
		cmds = append(cmds, c)
		addrs = append(addrs, e.Addr)
		if e.Kind == "allow" {
			unignored = append(unignored, e.Addr)
		}
	}
	if len(cmds) == 0 {
		return r, nil
	}
	tail := ""
	if fwd {
		tail = "firewall-cmd --reload >/dev/null 2>&1"
	}
	if err := s.runBatch(ctx, conn, pw, cmds, tail, addrs, &r); err != nil {
		return r, err
	}
	if ipt && len(r.Applied) > 0 {
		s.saveIptables(ctx, conn, pw, &r)
	}
	if len(unignored) > 0 && st.Fail2ban.Installed {
		keep := []string{}
		for _, a := range unignored {
			// still allowlisted by another rule: keep ignoring it
			still := false
			for _, e := range st.Entries {
				if e.Kind == "allow" && e.Addr == a && !slices.ContainsFunc(entries, func(x AccessEntry) bool { return x.Ref == e.Ref && x.Source == e.Source }) {
					still = true
				}
			}
			if !still {
				keep = append(keep, a)
			}
		}
		if len(keep) > 0 {
			s.f2bIgnore(ctx, conn, st, keep, false, pw)
		}
	}
	return r, nil
}

// otherAllowCovers: another allow entry (not e) still lets ip reach SSH.
func (st AccessState) otherAllowCovers(e AccessEntry, ip string) bool {
	for _, x := range st.Entries {
		if x.Kind != "allow" || (x.Ref == e.Ref && x.Source == e.Source) || !addrIn(x.Addr, ip) {
			continue
		}
		if x.Ports == "" || portsCoverSSH(x.Ports, st.SSHPorts) {
			return true
		}
	}
	return false
}

func portsCoverSSH(ports string, sshPorts []int) bool {
	pp, pr, _ := strings.Cut(ports, "/")
	if pr == "udp" {
		return false
	}
	ps, err := parsePortSpec(strings.ReplaceAll(pp, "-", ":"), ":")
	if err != nil {
		return false
	}
	for _, p := range sshPorts {
		if ps.covers(p) {
			return true
		}
	}
	return false
}

// ---- SSH lock ----

// AccessSSHLock makes SSH reachable only from allowlisted addresses (lock)
// or from everywhere again (unlock). Locking requires that the current
// session's address is allowlisted for SSH and that the firewall filters.
func (s *SecurityService) AccessSSHLock(connID string, lock bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	backend := ""
	err := func() error {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(2 * time.Minute)
		defer cancel()
		st, err := s.access(ctx, conn, sudoPassword)
		if err != nil {
			return err
		}
		backend = st.Backend
		if !st.CanLock {
			return apperr.New("sec.acc.lockUnsupported", "backend", st.Backend)
		}
		if lock {
			return s.sshLock(ctx, conn, st, sudoPassword)
		}
		return s.sshUnlock(ctx, conn, st, sudoPassword)
	}()
	action := "sec.access.sshUnlock"
	if lock {
		action = "sec.access.sshLock"
	}
	s.core.Audit(connID, action, backend, "", err)
	return err
}

func (s *SecurityService) sshLock(ctx context.Context, conn *sshx.Conn, st AccessState, pw string) error {
	if st.SSHLocked {
		return nil
	}
	if !st.Active {
		return apperr.New("sec.acc.lockInactive")
	}
	if st.Backend == "ufw" && !st.DefaultDeny {
		return apperr.New("sec.acc.lockDefaultAllow")
	}
	if st.ClientIP == "" || !st.otherAllowCovers(AccessEntry{}, st.ClientIP) {
		return apperr.New("sec.acc.lockNeedsSelf", "ip", st.ClientIP)
	}
	var cmds []string
	switch st.Backend {
	case "ufw":
		for _, l := range st.ufwAdded {
			if !ufwOpensSSH(l, st.SSHPorts) {
				continue
			}
			words, ok := shellFields(l)
			if !ok {
				continue
			}
			args := []string{"ufw", "--force", "delete"}
			for _, w := range words[1:] {
				args = append(args, core.Q(w))
			}
			cmds = append(cmds, strings.Join(args, " "))
		}
	case "firewalld":
		z := st.fwdZone
		if !reZone.MatchString(z) {
			return apperr.New("sec.fw.invalidZone", "zone", z)
		}
		if slices.Contains(st.fwdSvc, "ssh") {
			cmds = append(cmds, "firewall-cmd --permanent --zone="+core.Q(z)+" --remove-service=ssh")
		}
		for _, p := range st.fwdPorts {
			if fwdOpensSSH(nil, []string{p}, st.SSHPorts) {
				cmds = append(cmds, "firewall-cmd --permanent --zone="+core.Q(z)+" --remove-port="+core.Q(p))
			}
		}
		cmds = append(cmds, "firewall-cmd --reload")
	}
	if len(cmds) == 0 {
		return nil
	}
	res, err := s.root(ctx, conn, strings.Join(cmds, " && "), pw, "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.New("sec.fw.failed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	return nil
}

func (s *SecurityService) sshUnlock(ctx context.Context, conn *sshx.Conn, st AccessState, pw string) error {
	if !st.SSHLocked {
		return nil
	}
	var cmds []string
	switch st.Backend {
	case "ufw":
		for _, p := range st.SSHPorts {
			cmds = append(cmds, "ufw allow "+strconv.Itoa(p)+"/tcp comment "+core.Q("SSH (Server Manager)"))
		}
	case "firewalld":
		z := st.fwdZone
		if !reZone.MatchString(z) {
			return apperr.New("sec.fw.invalidZone", "zone", z)
		}
		for _, p := range st.SSHPorts {
			if p == 22 {
				cmds = append(cmds, "firewall-cmd --permanent --zone="+core.Q(z)+" --add-service=ssh")
			} else {
				cmds = append(cmds, "firewall-cmd --permanent --zone="+core.Q(z)+" --add-port="+strconv.Itoa(p)+"/tcp")
			}
		}
		cmds = append(cmds, "firewall-cmd --reload")
	}
	if len(cmds) == 0 {
		return nil
	}
	res, err := s.root(ctx, conn, strings.Join(cmds, " && "), pw, "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.New("sec.fw.failed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	return nil
}

// ---- fail2ban setup ----

// fail2banFile renders the app's jail file.
func fail2banFile(c Fail2banConfig, sshPorts []int, journalOnly bool) string {
	var b strings.Builder
	b.WriteString("# Managed by Server Manager (Security → IP access). Changes made here\n# are overwritten when the settings are saved from the app.\n\n[DEFAULT]\n")
	ignore := []string{"127.0.0.1/8", "::1"}
	for _, a := range c.IgnoreIP {
		if !slices.Contains(ignore, a) {
			ignore = append(ignore, a)
		}
	}
	b.WriteString("ignoreip = " + strings.Join(ignore, " ") + "\n")
	b.WriteString("bantime = " + strconv.Itoa(c.BanTime) + "\n")
	b.WriteString("findtime = " + strconv.Itoa(c.FindTime) + "\n")
	b.WriteString("maxretry = " + strconv.Itoa(c.MaxRetry) + "\n")
	if c.Increment && c.BanTime > 0 {
		b.WriteString("bantime.increment = true\n")
		b.WriteString("bantime.maxtime = " + strconv.Itoa(c.MaxTime) + "\n")
	} else {
		b.WriteString("bantime.increment = false\n")
	}
	ports := []string{}
	for _, p := range sshPorts {
		ports = append(ports, strconv.Itoa(p))
	}
	if len(ports) == 0 {
		ports = []string{"ssh"}
	}
	b.WriteString("\n[sshd]\nenabled = true\nport = " + strings.Join(ports, ",") + "\n")
	if c.Aggressive {
		b.WriteString("mode = aggressive\n")
	}
	if journalOnly {
		b.WriteString("backend = systemd\n")
	}
	b.WriteString("\n[recidive]\n")
	if c.Recidive {
		b.WriteString("enabled = true\nbantime = 1w\nfindtime = 1d\nmaxretry = 5\n")
	} else {
		b.WriteString("enabled = false\n")
	}
	return b.String()
}

func validF2B(c *Fail2banConfig) error {
	bad := func(field string) error { return apperr.New("sec.f2b.invalidSetting", "field", field) }
	if c.MaxRetry < 1 || c.MaxRetry > 100 {
		return bad("maxRetry")
	}
	if c.FindTime < 60 || c.FindTime > 7*86400 {
		return bad("findTime")
	}
	if c.BanTime != -1 && (c.BanTime < 60 || c.BanTime > 365*86400) {
		return bad("banTime")
	}
	if c.Increment && c.BanTime > 0 && (c.MaxTime < c.BanTime || c.MaxTime > 365*86400) {
		return bad("maxTime")
	}
	if len(c.IgnoreIP) > 200 {
		return bad("ignoreIp")
	}
	out := []string{}
	for _, a := range c.IgnoreIP {
		n, err := normAddr(a)
		if err != nil || n == "any" {
			return apperr.New("sec.fw.invalidAddr", "addr", a)
		}
		if !slices.Contains(out, n) && !isDefaultIgnore(n) {
			out = append(out, n)
		}
	}
	c.IgnoreIP = out
	return nil
}

// Fail2banApply writes the app's jail file (sshd jail, ban policy, ignore
// list, recidive), checks the configuration and (re)starts fail2ban. On
// failure the previous file is restored.
func (s *SecurityService) Fail2banApply(connID string, cfg Fail2banConfig, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	err := func() error {
		if err := validF2B(&cfg); err != nil {
			return err
		}
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(2 * time.Minute)
		defer cancel()
		st, err := s.access(ctx, conn, sudoPassword)
		if err != nil {
			return err
		}
		if !st.Fail2ban.Installed {
			return apperr.New("sec.f2b.notInstalled")
		}
		// Keep this session's address out of reach of bans.
		if st.ClientIP != "" && !slices.ContainsFunc(cfg.IgnoreIP, func(a string) bool { return addrIn(a, st.ClientIP) }) {
			cfg.IgnoreIP = append(cfg.IgnoreIP, st.ClientIP)
		}
		content := fail2banFile(cfg, st.SSHPorts, st.Fail2ban.JournalOnly)
		script := `F=` + f2bFile + `
restore() { if [ -f "$F.bak" ]; then mv -f "$F.bak" "$F"; else rm -f "$F"; fi; fail2ban-client reload >/dev/null 2>&1 || { systemctl restart fail2ban || rc-service fail2ban restart || service fail2ban restart; } >/dev/null 2>&1; }
mkdir -p /etc/fail2ban/jail.d
if [ -f "$F" ]; then cp -p "$F" "$F.bak"; else rm -f "$F.bak"; fi
` + heredoc(`"$F.new"`, content) + `mv -f "$F.new" "$F"; chmod 644 "$F"
if fail2ban-client -h 2>&1 | grep -q -- '--test'; then
  if ! out=$(fail2ban-client -t 2>&1); then echo @@testfail; printf '%s\n' "$out" | grep -v '^OK' | tail -n 15; restore; exit 0; fi
fi
if fail2ban-client ping >/dev/null 2>&1; then
  fail2ban-client reload >/dev/null 2>&1
else
  { systemctl enable fail2ban; systemctl restart fail2ban; } >/dev/null 2>&1 || { rc-update add fail2ban default; rc-service fail2ban restart; } >/dev/null 2>&1 || service fail2ban restart >/dev/null 2>&1
fi
i=0; while [ $i -lt 20 ]; do fail2ban-client status sshd >/dev/null 2>&1 && break; sleep 1; i=$((i+1)); done
if fail2ban-client status sshd >/dev/null 2>&1; then rm -f "$F.bak"; echo @@ok; else
  echo @@startfail
  { journalctl -u fail2ban -n 15 --no-pager 2>/dev/null || tail -n 15 /var/log/fail2ban.log 2>/dev/null; } | tail -n 15
  restore
fi`
		res, err := s.root(ctx, conn, script, sudoPassword, "")
		if err != nil {
			return err
		}
		out := res.Stdout
		switch {
		case strings.Contains(out, "@@ok"):
			return nil
		case strings.Contains(out, "@@testfail"):
			_, d, _ := strings.Cut(out, "@@testfail")
			return apperr.New("sec.f2b.configInvalid").WithDetail(strings.TrimSpace(d))
		default:
			_, d, _ := strings.Cut(out, "@@startfail")
			return apperr.New("sec.f2b.startFailed").WithDetail(strings.TrimSpace(core.FirstNonEmpty(d, res.Stderr)))
		}
	}()
	s.core.Audit(connID, "sec.f2b.config", "sshd", f2bSummary(cfg), err)
	return err
}

func f2bSummary(c Fail2banConfig) string {
	return "maxretry=" + strconv.Itoa(c.MaxRetry) + " findtime=" + strconv.Itoa(c.FindTime) + " bantime=" + strconv.Itoa(c.BanTime) +
		" increment=" + strconv.FormatBool(c.Increment) + " recidive=" + strconv.FormatBool(c.Recidive)
}

// Fail2banSetRunning starts (and enables at boot) or stops fail2ban.
func (s *SecurityService) Fail2banSetRunning(connID string, on bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	err := func() error {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(time.Minute)
		defer cancel()
		cmd := `{ systemctl disable --now fail2ban; } >/dev/null 2>&1 || { rc-service fail2ban stop; rc-update del fail2ban default; } >/dev/null 2>&1 || service fail2ban stop >/dev/null 2>&1; fail2ban-client ping >/dev/null 2>&1 && exit 3; exit 0`
		if on {
			cmd = `{ systemctl enable fail2ban; systemctl restart fail2ban; } >/dev/null 2>&1 || { rc-update add fail2ban default; rc-service fail2ban restart; } >/dev/null 2>&1 || service fail2ban restart >/dev/null 2>&1
i=0; while [ $i -lt 15 ]; do fail2ban-client ping >/dev/null 2>&1 && exit 0; sleep 1; i=$((i+1)); done
{ journalctl -u fail2ban -n 15 --no-pager 2>/dev/null || tail -n 15 /var/log/fail2ban.log 2>/dev/null; } | tail -n 15; exit 3`
		}
		res, err := s.root(ctx, conn, cmd, sudoPassword, "")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return apperr.New("sec.f2b.startFailed").WithDetail(core.FirstNonEmpty(res.Stdout, res.Stderr))
		}
		return nil
	}()
	action := "sec.f2b.stop"
	if on {
		action = "sec.f2b.start"
	}
	s.core.Audit(connID, action, "fail2ban", "", err)
	return err
}

// Fail2banInstall installs fail2ban with the package manager (job). It is
// configured and started afterwards with Fail2banApply.
func (s *SecurityService) Fail2banInstall(connID, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return "", err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	st, err := s.access(ctx, conn, sudoPassword)
	if err != nil {
		return "", err
	}
	var cmd string
	switch st.Fail2ban.PkgManager {
	case "apt":
		cmd = "export DEBIAN_FRONTEND=noninteractive; apt-get update && apt-get install -y fail2ban && { apt-get install -y python3-systemd || true; }"
	case "dnf":
		cmd = "dnf install -y fail2ban || { dnf install -y epel-release && dnf install -y fail2ban; }"
	case "yum":
		cmd = "yum install -y fail2ban || { yum install -y epel-release && yum install -y fail2ban; }"
	case "apk":
		cmd = "apk add --no-cache fail2ban"
	case "zypper":
		cmd = "zypper --non-interactive install fail2ban"
	case "pacman":
		cmd = "pacman -S --noconfirm fail2ban"
	default:
		err := apperr.New("sec.acc.noPkgManager")
		s.core.Audit(connID, "sec.f2b.install", "fail2ban", "", err)
		return "", err
	}
	j := s.core.Jobs.Start(connID, "sec.f2b.install", "install fail2ban", func(ctx context.Context, j *core.Job) error {
		j.Step("%s", st.Fail2ban.PkgManager)
		j.Logf("$ %s", cmd)
		err := s.core.JobRun(ctx, j, conn, pathPrefix+cmd+" 2>&1", true, sudoPassword)
		s.core.Audit(connID, "sec.f2b.install", "fail2ban", st.Fail2ban.PkgManager, err)
		return err
	})
	return j.ID(), nil
}
