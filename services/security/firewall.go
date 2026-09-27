package security

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// ---- detection ----

type FirewallBackend struct {
	Name      string `json:"name"` // ufw | firewalld | iptables | nftables
	Installed bool   `json:"installed"`
	Active    bool   `json:"active"`
	Version   string `json:"version"`
}

type FirewallInfo struct {
	// Primary is the backend the app manages: firewalld when running, else
	// ufw, else iptables, else nftables (read-only), else "none".
	Primary  string            `json:"primary"`
	Backends []FirewallBackend `json:"backends"`
	SSHPorts []int             `json:"sshPorts"`
	// ClientIP is this session's address as seen by the server.
	ClientIP string `json:"clientIp"`
}

const detectScript = `echo @@ufw; command -v ufw >/dev/null 2>&1 && { echo installed; ufw version 2>/dev/null | head -n1; ufw status 2>&1 | head -n1; }
echo @@firewalld; command -v firewall-cmd >/dev/null 2>&1 && { echo installed; firewall-cmd --version 2>/dev/null; firewall-cmd --state 2>&1; }
echo @@iptables; command -v iptables >/dev/null 2>&1 && { echo installed; iptables -V 2>/dev/null; iptables -S 2>/dev/null | awk '$1=="-A" && $2=="INPUT" { t=""; for (i=1;i<=NF;i++) if ($i=="-j") t=$(i+1); if (t !~ /^(ufw|DOCKER|f2b-)/) n++ } END { print "rules=" n+0 }'; iptables -S INPUT 2>/dev/null | head -n1; }
echo @@nftables; command -v nft >/dev/null 2>&1 && { echo installed; nft -v 2>/dev/null; echo "tables=$(nft list tables 2>/dev/null | awk '!(($2=="ip" || $2=="ip6") && $3 ~ /^(filter|nat|mangle|raw|security)$/)' | wc -l)"; }
echo @@ssh; sshd -T 2>/dev/null | awk '$1=="port"{print $2}'
echo @@end`

func parseDetect(out string) FirewallInfo {
	sec := sections(out)
	info := FirewallInfo{Primary: "none", Backends: []FirewallBackend{}, SSHPorts: []int{}}
	for _, name := range []string{"ufw", "firewalld", "iptables", "nftables"} {
		ls := lines(sec[name])
		b := FirewallBackend{Name: name}
		if len(ls) > 0 && ls[0] == "installed" {
			b.Installed = true
			rest := ls[1:]
			switch name {
			case "ufw":
				for _, l := range rest {
					if strings.HasPrefix(l, "ufw ") {
						b.Version = strings.TrimPrefix(l, "ufw ")
					}
					if strings.HasPrefix(l, "Status:") {
						b.Active = strings.TrimSpace(strings.TrimPrefix(l, "Status:")) == "active"
					}
				}
			case "firewalld":
				for _, l := range rest {
					if l == "running" {
						b.Active = true
					} else if regexp.MustCompile(`^\d+\.\d+`).MatchString(l) {
						b.Version = l
					}
				}
			case "iptables":
				for _, l := range rest {
					switch {
					case strings.HasPrefix(l, "iptables v"):
						b.Version = strings.TrimPrefix(l, "iptables ")
					case strings.HasPrefix(l, "rules="):
						b.Active = b.Active || atoi(strings.TrimPrefix(l, "rules=")) > 0
					case strings.HasPrefix(l, "-P INPUT"):
						b.Active = b.Active || !strings.HasSuffix(l, "ACCEPT")
					}
				}
			case "nftables":
				for _, l := range rest {
					if strings.HasPrefix(l, "nftables v") {
						b.Version = strings.TrimPrefix(l, "nftables ")
					}
					if strings.HasPrefix(l, "tables=") {
						b.Active = atoi(strings.TrimPrefix(l, "tables=")) > 0
					}
				}
			}
		}
		info.Backends = append(info.Backends, b)
	}
	by := map[string]FirewallBackend{}
	for _, b := range info.Backends {
		by[b.Name] = b
	}
	switch {
	case by["firewalld"].Active:
		info.Primary = "firewalld"
	case by["ufw"].Installed:
		info.Primary = "ufw"
	case by["iptables"].Installed:
		info.Primary = "iptables"
	case by["nftables"].Installed:
		info.Primary = "nftables"
	}
	for _, l := range lines(sec["ssh"]) {
		if n := atoi(l); validPort(n) && !slices.Contains(info.SSHPorts, n) {
			info.SSHPorts = append(info.SSHPorts, n)
		}
	}
	return info
}

// FirewallDetect reports which firewall tools exist and which is active.
func (s *SecurityService) FirewallDetect(connID, sudoPassword string) (FirewallInfo, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return FirewallInfo{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	return s.detect(ctx, conn, sudoPassword)
}

func (s *SecurityService) detect(ctx context.Context, conn *sshx.Conn, pw string) (FirewallInfo, error) {
	res, err := s.root(ctx, conn, detectScript, pw, "")
	if err != nil {
		return FirewallInfo{}, err
	}
	info := parseDetect(res.Stdout)
	// $SSH_CONNECTION is not kept by sudo: read it as the login user.
	ip, port := s.sessionClient(ctx, conn)
	info.ClientIP = ip
	if validPort(port) && !slices.Contains(info.SSHPorts, port) {
		info.SSHPorts = append(info.SSHPorts, port)
	}
	return info, nil
}

// ---- address/port helpers ----

// normAddr validates "any", an IP or a CIDR and returns its canonical form.
func normAddr(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "any") || strings.EqualFold(v, "anywhere") {
		return "any", nil
	}
	if ip := net.ParseIP(v); ip != nil {
		return ip.String(), nil
	}
	if ip, n, err := net.ParseCIDR(v); err == nil {
		_ = ip
		ones, _ := n.Mask.Size()
		return n.IP.String() + "/" + strconv.Itoa(ones), nil
	}
	return "", apperr.New("sec.fw.invalidAddr", "addr", v)
}

// addrCovers reports whether addr ("any", IP or CIDR) matches ip.
func addrCovers(addr, ip string) bool {
	if addr == "any" || addr == "" {
		return true
	}
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	if a := net.ParseIP(addr); a != nil {
		return a.Equal(p)
	}
	if _, n, err := net.ParseCIDR(addr); err == nil {
		return n.Contains(p)
	}
	return false
}

// portSpec is a validated port list/range: "22", "8000:8100", "80,443".
type portSpec struct {
	Raw    string
	Ranges [][2]int
}

func parsePortSpec(v, sep string) (portSpec, error) {
	v = strings.TrimSpace(v)
	ps := portSpec{}
	if v == "" {
		return ps, nil
	}
	parts := strings.Split(v, ",")
	if len(parts) > 15 {
		return ps, apperr.New("sec.invalidPort", "port", v)
	}
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		lo, hi, isRange := strings.Cut(strings.ReplaceAll(p, "-", ":"), ":")
		a, e1 := strconv.Atoi(lo)
		b := a
		var e2 error
		if isRange {
			b, e2 = strconv.Atoi(hi)
		}
		if e1 != nil || e2 != nil || !validPort(a) || !validPort(b) || b < a {
			return ps, apperr.New("sec.invalidPort", "port", p)
		}
		ps.Ranges = append(ps.Ranges, [2]int{a, b})
		if isRange && b != a {
			out = append(out, fmt.Sprintf("%d%s%d", a, sep, b))
		} else {
			out = append(out, strconv.Itoa(a))
		}
	}
	ps.Raw = strings.Join(out, ",")
	return ps, nil
}

func (p portSpec) covers(port int) bool {
	if len(p.Ranges) == 0 {
		return true // all ports
	}
	for _, r := range p.Ranges {
		if port >= r[0] && port <= r[1] {
			return true
		}
	}
	return false
}

func (p portSpec) multi() bool {
	return len(p.Ranges) > 1 || (len(p.Ranges) == 1 && p.Ranges[0][0] != p.Ranges[0][1])
}

func validProto(p string, allowAny bool) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "tcp", "udp":
		return p, nil
	case "", "any", "all":
		if allowAny {
			return "any", nil
		}
	}
	return "", apperr.New("sec.fw.invalidProto", "proto", p)
}

// sessionClient returns this session's client IP as seen by the server.
func (s *SecurityService) sessionClient(ctx context.Context, conn *sshx.Conn) (string, int) {
	res, _ := s.user(ctx, conn, `echo "$SSH_CONNECTION"`, "")
	if f := strings.Fields(res.Stdout); len(f) == 4 {
		return f[0], atoi(f[3])
	}
	return "", conn.Server().Port
}

// sshPorts returns sshd's ports plus the session's port.
func (s *SecurityService) sshPorts(ctx context.Context, conn *sshx.Conn, pw string) []int {
	info, err := s.detect(ctx, conn, pw)
	if err == nil && len(info.SSHPorts) > 0 {
		return info.SSHPorts
	}
	return []int{s.sessionPort(ctx, conn)}
}

// ================= UFW =================

type UFWRule struct {
	Num       int    `json:"num"`
	To        string `json:"to"`
	Action    string `json:"action"`    // ALLOW | DENY | REJECT | LIMIT
	Direction string `json:"direction"` // IN | OUT | FWD
	From      string `json:"from"`
	Comment   string `json:"comment"`
	V6        bool   `json:"v6"`
	Raw       string `json:"raw"` // the line as shown (used to confirm deletes)
}

type UFWStatus struct {
	Installed bool      `json:"installed"`
	Active    bool      `json:"active"`
	Logging   string    `json:"logging"`
	Defaults  UFWPolicy `json:"defaults"`
	Rules     []UFWRule `json:"rules"`
	// Added lists `ufw show added` commands (inactive firewall).
	Added    []string `json:"added"`
	SSHPorts []int    `json:"sshPorts"`
	ClientIP string   `json:"clientIp"`
}

type UFWPolicy struct {
	Incoming string `json:"incoming"`
	Outgoing string `json:"outgoing"`
	Routed   string `json:"routed"`
}

// NewUFWRule describes a rule to add.
type NewUFWRule struct {
	Action    string `json:"action"`    // allow | deny | reject | limit
	Direction string `json:"direction"` // in | out
	Port      string `json:"port"`      // "", "22", "8000:8100", "80,443"
	Proto     string `json:"proto"`     // tcp | udp | any
	From      string `json:"from"`      // any | IP | CIDR
	To        string `json:"to"`        // any | IP | CIDR
	Interface string `json:"interface"`
	Comment   string `json:"comment"`
	Position  int    `json:"position"` // 0 = append, N = insert at N
	Force     bool   `json:"force"`    // accept that it may block SSH
}

var reUFWRule = regexp.MustCompile(`^\[\s*(\d+)\]\s(.+?)\s+(ALLOW|DENY|REJECT|LIMIT)(?:\s(IN|OUT|FWD))?\s+(.*)$`)

func parseUFWStatus(verbose, numbered, added string) UFWStatus {
	st := UFWStatus{Installed: true, Rules: []UFWRule{}, Added: []string{}, SSHPorts: []int{}}
	for _, l := range lines(verbose) {
		switch {
		case strings.HasPrefix(l, "Status:"):
			st.Active = strings.TrimSpace(strings.TrimPrefix(l, "Status:")) == "active"
		case strings.HasPrefix(l, "Logging:"):
			st.Logging = strings.TrimSpace(strings.TrimPrefix(l, "Logging:"))
		case strings.HasPrefix(l, "Default:"):
			for _, part := range strings.Split(strings.TrimPrefix(l, "Default:"), ",") {
				f := strings.Fields(part)
				if len(f) < 2 {
					continue
				}
				pol, dir := f[0], strings.Trim(f[1], "()")
				switch dir {
				case "incoming":
					st.Defaults.Incoming = pol
				case "outgoing":
					st.Defaults.Outgoing = pol
				case "routed":
					st.Defaults.Routed = pol
				}
			}
		}
	}
	for _, l := range lines(numbered) {
		m := reUFWRule.FindStringSubmatch(strings.TrimRight(l, " "))
		if m == nil {
			continue
		}
		r := UFWRule{Num: atoi(m[1]), To: strings.TrimSpace(m[2]), Action: m[3], Direction: m[4], Raw: collapse(l[strings.Index(l, "]")+1:])}
		if r.Direction == "" {
			r.Direction = "IN"
		}
		from := m[5]
		if i := strings.Index(from, " # "); i >= 0 {
			r.Comment = strings.TrimSpace(from[i+3:])
			from = from[:i]
		}
		from = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(from), "(log)"))
		from = strings.TrimSpace(strings.TrimSuffix(from, "(log-all)"))
		r.From = strings.TrimSpace(from)
		r.V6 = strings.Contains(r.To, "(v6)") || strings.Contains(r.From, "(v6)")
		st.Rules = append(st.Rules, r)
	}
	for _, l := range lines(added) {
		if strings.HasPrefix(strings.TrimSpace(l), "ufw ") {
			st.Added = append(st.Added, strings.TrimSpace(l))
		}
	}
	return st
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// ufwCoversPort reports whether a rule's "To" column includes port/proto.
func ufwCoversPort(to string, port int, proto string) bool {
	to = strings.TrimSpace(strings.ReplaceAll(to, "(v6)", ""))
	if i := strings.Index(to, " on "); i >= 0 {
		to = to[:i]
	}
	f := strings.Fields(to)
	if len(f) == 0 {
		return false
	}
	spec := f[len(f)-1]
	if strings.EqualFold(spec, "OpenSSH") {
		return port == 22 && proto != "udp"
	}
	if strings.EqualFold(spec, "Anywhere") || net.ParseIP(spec) != nil || strings.Contains(spec, "/") && net.ParseIP(strings.Split(spec, "/")[0]) != nil && !regexp.MustCompile(`/(tcp|udp)$`).MatchString(spec) {
		return true
	}
	pp, pr, _ := strings.Cut(spec, "/")
	if pr != "" && proto != "" && proto != "any" && pr != proto {
		return false
	}
	ps, err := parsePortSpec(pp, ":")
	if err != nil {
		return false
	}
	return ps.covers(port)
}

func ufwFromCovers(from, ip string) bool {
	from = strings.TrimSpace(strings.ReplaceAll(from, "(v6)", ""))
	if i := strings.Index(from, " "); i >= 0 {
		from = from[:i]
	}
	if strings.EqualFold(from, "Anywhere") {
		return true
	}
	return addrCovers(from, ip)
}

const ufwStatusScript = `command -v ufw >/dev/null 2>&1 || { echo @@missing; exit 0; }
echo @@verbose; ufw status verbose 2>&1
echo @@numbered; ufw status numbered 2>&1
echo @@added; ufw show added 2>&1
echo @@ssh; sshd -T 2>/dev/null | awk '$1=="port"{print $2}'
echo @@end`

func (s *SecurityService) ufwStatus(ctx context.Context, conn *sshx.Conn, pw string) (UFWStatus, error) {
	res, err := s.root(ctx, conn, ufwStatusScript, pw, "")
	if err != nil {
		return UFWStatus{}, err
	}
	if strings.HasPrefix(res.Stdout, "@@missing") {
		return UFWStatus{Rules: []UFWRule{}, Added: []string{}, SSHPorts: []int{}}, apperr.New("sec.fw.notInstalled", "tool", "ufw")
	}
	sec := sections(res.Stdout)
	st := parseUFWStatus(sec["verbose"], sec["numbered"], sec["added"])
	ip, port := s.sessionClient(ctx, conn)
	st.ClientIP = ip
	if validPort(port) {
		st.SSHPorts = append(st.SSHPorts, port)
	}
	for _, l := range lines(sec["ssh"]) {
		if n := atoi(l); validPort(n) && !slices.Contains(st.SSHPorts, n) {
			st.SSHPorts = append(st.SSHPorts, n)
		}
	}
	return st, nil
}

// UFWStatus reads ufw's state, defaults and rules.
func (s *SecurityService) UFWStatus(connID, sudoPassword string) (UFWStatus, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return UFWStatus{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	return s.ufwStatus(ctx, conn, sudoPassword)
}

// ufwCommand builds a validated `ufw` rule command.
func ufwCommand(r NewUFWRule) (string, error) {
	action := strings.ToLower(r.Action)
	if !slices.Contains([]string{"allow", "deny", "reject", "limit"}, action) {
		return "", apperr.New("sec.fw.invalidAction", "action", r.Action)
	}
	dir := strings.ToLower(r.Direction)
	if dir == "" {
		dir = "in"
	}
	if dir != "in" && dir != "out" {
		return "", apperr.New("sec.fw.invalidDirection", "direction", r.Direction)
	}
	if action == "limit" && dir != "in" {
		return "", apperr.New("sec.fw.invalidDirection", "direction", r.Direction)
	}
	proto, err := validProto(r.Proto, true)
	if err != nil {
		return "", err
	}
	ports, err := parsePortSpec(r.Port, ":")
	if err != nil {
		return "", err
	}
	if ports.multi() && proto == "any" {
		return "", apperr.New("sec.fw.rangeNeedsProto")
	}
	from, err := normAddr(r.From)
	if err != nil {
		return "", err
	}
	to, err := normAddr(r.To)
	if err != nil {
		return "", err
	}
	if ports.Raw == "" && from == "any" && to == "any" && action != "deny" && action != "reject" {
		return "", apperr.New("sec.fw.tooBroad")
	}
	parts := []string{"ufw"}
	if r.Position > 0 {
		parts = append(parts, "insert", strconv.Itoa(r.Position))
	}
	parts = append(parts, action, dir)
	if r.Interface != "" {
		if !reIface.MatchString(r.Interface) {
			return "", apperr.New("sec.fw.invalidIface", "iface", r.Interface)
		}
		parts = append(parts, "on", core.Q(r.Interface))
	}
	if proto != "any" {
		parts = append(parts, "proto", proto)
	}
	parts = append(parts, "from", core.Q(from), "to", core.Q(to))
	if ports.Raw != "" {
		parts = append(parts, "port", core.Q(ports.Raw))
	}
	if c := sanitizeComment(r.Comment, 80); c != "" {
		parts = append(parts, "comment", core.Q(c))
	}
	return strings.Join(parts, " "), nil
}

// ruleBlocksSSH reports whether a new deny/reject rule would hit this
// session (its port and client address).
func ruleBlocksSSH(action, dir string, ports portSpec, proto, from, clientIP string, sshPorts []int) bool {
	a := strings.ToLower(action)
	if (a != "deny" && a != "reject" && a != "drop") || strings.ToLower(dir) == "out" || proto == "udp" {
		return false
	}
	if clientIP != "" && !addrCovers(from, clientIP) {
		return false
	}
	for _, p := range sshPorts {
		if ports.covers(p) {
			return true
		}
	}
	return false
}

// UFWAddRule adds a ufw rule.
func (s *SecurityService) UFWAddRule(connID string, rule NewUFWRule, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	cmd, err := s.ufwAdd(connID, rule, sudoPassword)
	s.core.Audit(connID, "sec.firewall.add", "ufw", core.FirstNonEmpty(cmd, rule.Action+" "+rule.Port), err)
	return err
}

func (s *SecurityService) ufwAdd(connID string, rule NewUFWRule, pw string) (string, error) {
	cmd, err := ufwCommand(rule)
	if err != nil {
		return "", err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return cmd, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	if !rule.Force {
		ports, _ := parsePortSpec(rule.Port, ":")
		from, _ := normAddr(rule.From)
		proto, _ := validProto(rule.Proto, true)
		ip, _ := s.sessionClient(ctx, conn)
		if ruleBlocksSSH(rule.Action, rule.Direction, ports, proto, from, ip, s.sshPorts(ctx, conn, pw)) {
			return cmd, apperr.New("sec.fw.blocksSsh")
		}
	}
	res, err := s.rootOK(ctx, conn, cmd, pw, "")
	if err != nil {
		return cmd, err
	}
	if strings.Contains(res.Stdout+res.Stderr, "ERROR") {
		return cmd, apperr.New("sec.fw.failed").WithDetail(res.Stdout + res.Stderr)
	}
	return cmd, nil
}

// UFWDeleteRule deletes rule num after checking it still is the rule the
// user saw (expect = its Raw text).
func (s *SecurityService) UFWDeleteRule(connID string, num int, expect string, force bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	err := s.ufwDelete(connID, num, expect, force, sudoPassword)
	s.core.Audit(connID, "sec.firewall.delete", "ufw", fmt.Sprintf("#%d %s", num, expect), err)
	return err
}

func (s *SecurityService) ufwDelete(connID string, num int, expect string, force bool, pw string) error {
	if num < 1 || num > 100000 {
		return apperr.New("sec.fw.ruleChanged")
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	st, err := s.ufwStatus(ctx, conn, pw)
	if err != nil {
		return err
	}
	var target *UFWRule
	for i := range st.Rules {
		if st.Rules[i].Num == num {
			target = &st.Rules[i]
		}
	}
	if target == nil || collapse(expect) != target.Raw {
		return apperr.New("sec.fw.ruleChanged")
	}
	if !force && st.Active && st.Defaults.Incoming != "allow" && ufwAllowsSSH(*target, st) {
		remaining := 0
		for _, r := range st.Rules {
			if r.Num != num && r.V6 == target.V6 && ufwAllowsSSH(r, st) {
				remaining++
			}
		}
		if remaining == 0 {
			return apperr.New("sec.fw.sshRule")
		}
	}
	_, err = s.rootOK(ctx, conn, "ufw --force delete "+strconv.Itoa(num), pw, "")
	return err
}

// shellFields splits a command line printed by ufw (single/double quotes,
// backslash escapes) into words.
func shellFields(line string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	inWord, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(line) {
				i++
				cur.WriteByte(line[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, true
}

// UFWDeleteAdded deletes a rule of an inactive ufw by its `ufw show added`
// line (which must still be listed). No SSH guard is needed: enabling ufw
// always allows the SSH ports first.
func (s *SecurityService) UFWDeleteAdded(connID, line, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	err := func() error {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(defaultTimeout)
		defer cancel()
		st, err := s.ufwStatus(ctx, conn, sudoPassword)
		if err != nil {
			return err
		}
		if !slices.Contains(st.Added, strings.TrimSpace(line)) {
			return apperr.New("sec.fw.ruleChanged")
		}
		words, ok := shellFields(strings.TrimSpace(line))
		if !ok || len(words) < 3 || words[0] != "ufw" {
			return apperr.New("sec.fw.ruleChanged")
		}
		args := []string{"ufw", "--force", "delete"}
		for _, w := range words[1:] {
			if reCtl.MatchString(w) {
				return apperr.New("sec.fw.ruleChanged")
			}
			args = append(args, core.Q(w))
		}
		_, err = s.rootOK(ctx, conn, strings.Join(args, " "), sudoPassword, "")
		return err
	}()
	s.core.Audit(connID, "sec.firewall.delete", "ufw", line, err)
	return err
}

func ufwAllowsSSH(r UFWRule, st UFWStatus) bool {
	if (r.Action != "ALLOW" && r.Action != "LIMIT") || r.Direction != "IN" {
		return false
	}
	if st.ClientIP != "" && !ufwFromCovers(r.From, st.ClientIP) && !r.V6 {
		return false
	}
	for _, p := range st.SSHPorts {
		if ufwCoversPort(r.To, p, "tcp") {
			return true
		}
	}
	return false
}

// ensureUFWSSH allows every SSH port before the firewall starts filtering.
func (s *SecurityService) ensureUFWSSH(ctx context.Context, conn *sshx.Conn, pw string) ([]string, error) {
	done := []string{}
	for _, p := range s.sshPorts(ctx, conn, pw) {
		cmd := "ufw allow " + strconv.Itoa(p) + "/tcp comment " + core.Q("SSH (Server Manager)")
		if _, err := s.rootOK(ctx, conn, cmd, pw, ""); err != nil {
			return done, err
		}
		done = append(done, strconv.Itoa(p)+"/tcp")
	}
	return done, nil
}

// UFWEnable allows the SSH port(s) first, then enables ufw. Returns the
// SSH rules that were ensured ("22/tcp").
func (s *SecurityService) UFWEnable(connID, sudoPassword string) ([]string, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return nil, err
	}
	allowed, err := func() ([]string, error) {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return []string{}, err
		}
		ctx, cancel := core.Timeout(90 * time.Second)
		defer cancel()
		allowed, err := s.ensureUFWSSH(ctx, conn, sudoPassword)
		if err != nil {
			return allowed, err
		}
		_, err = s.rootOK(ctx, conn, "ufw --force enable", sudoPassword, "")
		return allowed, err
	}()
	s.core.Audit(connID, "sec.firewall.enable", "ufw", "allowed first: "+strings.Join(allowed, ", "), err)
	return allowed, err
}

func (s *SecurityService) ufwSimple(connID, action, cmd, detail, pw string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	err := func() error {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(90 * time.Second)
		defer cancel()
		_, err = s.rootOK(ctx, conn, cmd, pw, "")
		return err
	}()
	s.core.Audit(connID, action, "ufw", detail, err)
	return err
}

// UFWDisable turns ufw off (rules are kept).
func (s *SecurityService) UFWDisable(connID, sudoPassword string) error {
	return s.ufwSimple(connID, "sec.firewall.disable", "ufw disable", "", sudoPassword)
}

// UFWReload reloads ufw's rules.
func (s *SecurityService) UFWReload(connID, sudoPassword string) error {
	return s.ufwSimple(connID, "sec.firewall.reload", "ufw reload", "", sudoPassword)
}

// UFWReset disables ufw and deletes all rules (ufw keeps a backup).
func (s *SecurityService) UFWReset(connID, sudoPassword string) error {
	return s.ufwSimple(connID, "sec.firewall.reset", "ufw --force reset", "", sudoPassword)
}

// UFWSetLogging sets ufw's log level.
func (s *SecurityService) UFWSetLogging(connID, level, sudoPassword string) error {
	if !slices.Contains([]string{"off", "on", "low", "medium", "high", "full"}, level) {
		return apperr.New("sec.fw.invalidValue", "value", level)
	}
	return s.ufwSimple(connID, "sec.firewall.logging", "ufw logging "+level, level, sudoPassword)
}

// UFWSetDefault sets a default policy (direction incoming|outgoing|routed,
// policy allow|deny|reject). Before denying incoming traffic on an active
// firewall the SSH ports are allowed.
func (s *SecurityService) UFWSetDefault(connID, direction, policy, sudoPassword string) ([]string, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return nil, err
	}
	allowed := []string{}
	err := func() error {
		if !slices.Contains([]string{"incoming", "outgoing", "routed"}, direction) || !slices.Contains([]string{"allow", "deny", "reject"}, policy) {
			return apperr.New("sec.fw.invalidValue", "value", direction+" "+policy)
		}
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(90 * time.Second)
		defer cancel()
		if direction == "incoming" && policy != "allow" {
			if allowed, err = s.ensureUFWSSH(ctx, conn, sudoPassword); err != nil {
				return err
			}
		}
		_, err = s.rootOK(ctx, conn, "ufw default "+policy+" "+direction, sudoPassword, "")
		return err
	}()
	s.core.Audit(connID, "sec.firewall.default", "ufw", direction+"="+policy, err)
	return allowed, err
}

// ================= firewalld =================

type FirewalldZone struct {
	Name         string   `json:"name"`
	Active       bool     `json:"active"`
	Default      bool     `json:"default"`
	Target       string   `json:"target"`
	Interfaces   []string `json:"interfaces"`
	Sources      []string `json:"sources"`
	Services     []string `json:"services"`
	Ports        []string `json:"ports"`
	Protocols    []string `json:"protocols"`
	ForwardPorts []string `json:"forwardPorts"`
	Masquerade   bool     `json:"masquerade"`
	RichRules    []string `json:"richRules"`
}

type FirewalldStatus struct {
	Installed   bool            `json:"installed"`
	Running     bool            `json:"running"`
	DefaultZone string          `json:"defaultZone"`
	Zones       []string        `json:"zones"`
	Details     []FirewalldZone `json:"details"`
	SSHPorts    []int           `json:"sshPorts"`
}

var reZone = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

const firewalldScript = `command -v firewall-cmd >/dev/null 2>&1 || { echo @@missing; exit 0; }
S=$(firewall-cmd --state 2>&1); echo @@state; echo "$S"
[ "$S" = running ] || exit 0
echo @@default; firewall-cmd --get-default-zone
echo @@zones; firewall-cmd --get-zones
echo @@active; firewall-cmd --get-active-zones
for z in $( (firewall-cmd --get-default-zone; firewall-cmd --get-active-zones | grep -v '^[[:space:]]') | sort -u); do
  echo "@@zone:$z"; firewall-cmd --zone="$z" --list-all
done
echo @@ssh; sshd -T 2>/dev/null | awk '$1=="port"{print $2}'
echo @@end`

func parseFirewalldZone(name, out string) FirewalldZone {
	z := FirewalldZone{Name: name, Interfaces: []string{}, Sources: []string{}, Services: []string{}, Ports: []string{},
		Protocols: []string{}, ForwardPorts: []string{}, RichRules: []string{}}
	rich := false
	for i, l := range strings.Split(out, "\n") {
		if i == 0 {
			z.Active = strings.Contains(l, "(active)")
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if rich && !strings.Contains(t, ": ") && strings.HasPrefix(t, "rule") {
			z.RichRules = append(z.RichRules, t)
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		f := strings.Fields(v)
		if f == nil {
			f = []string{}
		}
		rich = false
		switch k {
		case "target":
			z.Target = v
		case "interfaces":
			z.Interfaces = f
		case "sources":
			z.Sources = f
		case "services":
			z.Services = f
		case "ports":
			z.Ports = f
		case "protocols":
			z.Protocols = f
		case "forward-ports":
			z.ForwardPorts = f
		case "masquerade":
			z.Masquerade = v == "yes"
		case "rich rules":
			rich = true
		}
	}
	return z
}

// FirewalldStatus reads firewalld zones and their rules.
func (s *SecurityService) FirewalldStatus(connID, sudoPassword string) (FirewalldStatus, error) {
	st := FirewalldStatus{Zones: []string{}, Details: []FirewalldZone{}, SSHPorts: []int{}}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return st, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	res, err := s.root(ctx, conn, firewalldScript, sudoPassword, "")
	if err != nil {
		return st, err
	}
	if strings.HasPrefix(res.Stdout, "@@missing") {
		return st, apperr.New("sec.fw.notInstalled", "tool", "firewalld")
	}
	st.Installed = true
	sec := sections(res.Stdout)
	st.Running = strings.TrimSpace(sec["state"]) == "running"
	st.DefaultZone = strings.TrimSpace(sec["default"])
	st.Zones = strings.Fields(sec["zones"])
	if st.Zones == nil {
		st.Zones = []string{}
	}
	for k, v := range sec {
		if name, ok := strings.CutPrefix(k, "zone:"); ok {
			z := parseFirewalldZone(name, v)
			z.Default = name == st.DefaultZone
			st.Details = append(st.Details, z)
		}
	}
	slices.SortFunc(st.Details, func(a, b FirewalldZone) int {
		if a.Default != b.Default {
			if a.Default {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, l := range lines(sec["ssh"]) {
		if n := atoi(l); validPort(n) {
			st.SSHPorts = append(st.SSHPorts, n)
		}
	}
	return st, nil
}

// firewalldArg validates a firewalld change and returns the option value.
func firewalldArg(kind, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch kind {
	case "port":
		pp, proto, _ := strings.Cut(value, "/")
		pr, err := validProto(proto, false)
		if err != nil {
			return "", err
		}
		ps, err := parsePortSpec(pp, "-")
		if err != nil || len(ps.Ranges) != 1 {
			return "", apperr.New("sec.invalidPort", "port", value)
		}
		return ps.Raw + "/" + pr, nil
	case "service":
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`).MatchString(value) {
			return "", apperr.New("sec.fw.invalidService", "service", value)
		}
		return value, nil
	case "rich-rule":
		if len(value) > 1024 || reCtl.MatchString(value) || !strings.HasPrefix(value, "rule ") {
			return "", apperr.New("sec.fw.invalidRich")
		}
		return value, nil
	case "source":
		a, err := normAddr(value)
		if err != nil || a == "any" {
			return "", apperr.New("sec.fw.invalidAddr", "addr", value)
		}
		return a, nil
	}
	return "", apperr.New("sec.fw.invalidValue", "value", kind)
}

// FirewalldChange adds or removes a port ("80/tcp", "8000-8100/udp"),
// service, rich rule or source in a zone, permanently, then reloads.
func (s *SecurityService) FirewalldChange(connID, zone, kind, value string, add, force bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	verb := "remove"
	action := "sec.firewall.delete"
	if add {
		verb, action = "add", "sec.firewall.add"
	}
	err := s.firewalldChange(connID, zone, kind, value, verb, force, sudoPassword)
	s.core.Audit(connID, action, "firewalld/"+zone, verb+" "+kind+" "+value, err)
	return err
}

func (s *SecurityService) firewalldChange(connID, zone, kind, value, verb string, force bool, pw string) error {
	if !reZone.MatchString(zone) {
		return apperr.New("sec.fw.invalidZone", "zone", zone)
	}
	arg, err := firewalldArg(kind, value)
	if err != nil {
		return err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(90 * time.Second)
	defer cancel()
	if verb == "remove" && !force {
		ports := s.sshPorts(ctx, conn, pw)
		hit := kind == "service" && arg == "ssh" && slices.Contains(ports, 22)
		if kind == "port" {
			pp, pr, _ := strings.Cut(arg, "/")
			ps, _ := parsePortSpec(pp, "-")
			for _, p := range ports {
				if pr == "tcp" && ps.covers(p) {
					hit = true
				}
			}
		}
		if hit {
			return apperr.New("sec.fw.sshRule")
		}
	}
	cmd := "firewall-cmd --permanent --zone=" + core.Q(zone) + " --" + verb + "-" + kind + "=" + core.Q(arg) + " && firewall-cmd --reload"
	res, err := s.root(ctx, conn, cmd, pw, "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.New("sec.fw.failed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	return nil
}

// ================= iptables =================

type IptRule struct {
	Num     int    `json:"num"`
	Pkts    string `json:"pkts"`
	Bytes   string `json:"bytes"`
	Target  string `json:"target"`
	Proto   string `json:"proto"`
	In      string `json:"in"`
	Out     string `json:"out"`
	Source  string `json:"source"`
	Dest    string `json:"dest"`
	Extra   string `json:"extra"`
	Spec    string `json:"spec"` // from iptables -S (used to confirm deletes)
	Comment string `json:"comment"`
}

type IptChain struct {
	Name   string    `json:"name"`
	Policy string    `json:"policy"`
	Rules  []IptRule `json:"rules"`
}

type IptablesStatus struct {
	Installed bool       `json:"installed"`
	V6        bool       `json:"v6"`
	Variant   string     `json:"variant"` // nf_tables | legacy
	Chains    []IptChain `json:"chains"`
	Raw       string     `json:"raw"`
	// Persist is how rules are saved: netfilter-persistent | rules.v4 |
	// sysconfig | openrc | "" (not persistent).
	Persist string `json:"persist"`
	// ManagedBy warns that another tool owns the rules (ufw, firewalld).
	ManagedBy string `json:"managedBy"`
}

const persistDetect = `if command -v netfilter-persistent >/dev/null 2>&1; then echo netfilter-persistent
elif command -v rc-service >/dev/null 2>&1 && [ -x /etc/init.d/iptables ]; then echo openrc
elif [ -x /usr/libexec/iptables/iptables.init ] || [ -f /etc/sysconfig/iptables ]; then echo sysconfig
elif [ -f /etc/iptables/rules.v4 ]; then echo rules.v4
fi`

func iptablesScript(v6 bool) string {
	bin := "iptables"
	if v6 {
		bin = "ip6tables"
	}
	return `command -v ` + bin + ` >/dev/null 2>&1 || { echo @@missing; exit 0; }
echo @@version; ` + bin + ` -V 2>&1
echo @@S; ` + bin + ` -S 2>&1
for c in INPUT FORWARD OUTPUT; do echo "@@L:$c"; ` + bin + ` -L "$c" -n -v -x --line-numbers 2>&1; done
echo @@persist; ` + persistDetect + `
echo @@managed; (command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q 'Status: active' && echo ufw); (command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null)" = running ] && echo firewalld); true
echo @@end`
}

func parseIptables(out string, v6 bool) IptablesStatus {
	sec := sections(out)
	st := IptablesStatus{Installed: true, V6: v6, Chains: []IptChain{}, Raw: sec["S"]}
	ver := strings.TrimSpace(sec["version"])
	switch {
	case strings.Contains(ver, "nf_tables"):
		st.Variant = "nf_tables"
	case strings.Contains(ver, "legacy"):
		st.Variant = "legacy"
	}
	st.Persist = strings.TrimSpace(sec["persist"])
	st.ManagedBy = strings.Join(lines(sec["managed"]), ",")
	specs := map[string][]string{}
	policies := map[string]string{}
	for _, l := range lines(sec["S"]) {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "-P" {
			policies[f[1]] = f[2]
		}
		if len(f) >= 2 && f[0] == "-A" {
			specs[f[1]] = append(specs[f[1]], l)
		}
	}
	for _, chain := range []string{"INPUT", "FORWARD", "OUTPUT"} {
		c := IptChain{Name: chain, Policy: policies[chain], Rules: []IptRule{}}
		rows := lines(sec["L:"+chain])
		idx := 0
		for _, l := range rows {
			f := strings.Fields(l)
			if len(f) < 9 || atoi(f[0]) == 0 {
				continue
			}
			spec := ""
			if idx < len(specs[chain]) {
				spec = specs[chain][idx]
			}
			idx++
			r := IptRule{Num: atoi(f[0]), Pkts: f[1], Bytes: f[2], Spec: spec}
			rest := f[3:]
			if strings.Contains(spec, " -j ") || strings.Contains(spec, " -g ") {
				r.Target, rest = rest[0], rest[1:]
			}
			if len(rest) >= 6 {
				r.Proto, r.In, r.Out, r.Source, r.Dest = rest[0], rest[2], rest[3], rest[4], rest[5]
				r.Extra = strings.Join(rest[6:], " ")
			}
			if m := regexp.MustCompile(`--comment ("[^"]*"|\S+)`).FindStringSubmatch(spec); m != nil {
				r.Comment = strings.Trim(m[1], `"`)
			}
			c.Rules = append(c.Rules, r)
		}
		st.Chains = append(st.Chains, c)
	}
	return st
}

// IptablesStatus lists the filter table's built-in chains (v6 = ip6tables).
func (s *SecurityService) IptablesStatus(connID string, v6 bool, sudoPassword string) (IptablesStatus, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return IptablesStatus{Chains: []IptChain{}}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	res, err := s.root(ctx, conn, iptablesScript(v6), sudoPassword, "")
	if err != nil {
		return IptablesStatus{Chains: []IptChain{}}, err
	}
	if strings.HasPrefix(res.Stdout, "@@missing") {
		return IptablesStatus{Chains: []IptChain{}}, apperr.New("sec.fw.notInstalled", "tool", "iptables")
	}
	return parseIptables(res.Stdout, v6), nil
}

type NewIptRule struct {
	V6        bool   `json:"v6"`
	Chain     string `json:"chain"`  // INPUT | OUTPUT | FORWARD
	Target    string `json:"target"` // ACCEPT | DROP | REJECT
	Proto     string `json:"proto"`  // tcp | udp | icmp | all
	Port      string `json:"port"`   // "", "22", "8000:8100"
	Source    string `json:"source"` // any | IP | CIDR
	Interface string `json:"interface"`
	Comment   string `json:"comment"`
	Top       bool   `json:"top"` // insert at the top instead of appending
	Force     bool   `json:"force"`
}

func iptablesCommand(r NewIptRule) (string, error) {
	bin := "iptables"
	if r.V6 {
		bin = "ip6tables"
	}
	if !slices.Contains([]string{"INPUT", "OUTPUT", "FORWARD"}, r.Chain) {
		return "", apperr.New("sec.fw.invalidValue", "value", r.Chain)
	}
	if !slices.Contains([]string{"ACCEPT", "DROP", "REJECT"}, r.Target) {
		return "", apperr.New("sec.fw.invalidAction", "action", r.Target)
	}
	proto := strings.ToLower(r.Proto)
	if proto == "" || proto == "any" {
		proto = "all"
	}
	if !slices.Contains([]string{"tcp", "udp", "icmp", "all"}, proto) {
		return "", apperr.New("sec.fw.invalidProto", "proto", r.Proto)
	}
	if r.V6 && proto == "icmp" {
		proto = "ipv6-icmp"
	}
	ports, err := parsePortSpec(r.Port, ":")
	if err != nil {
		return "", err
	}
	if len(ports.Ranges) > 1 {
		return "", apperr.New("sec.invalidPort", "port", r.Port)
	}
	if ports.Raw != "" && proto != "tcp" && proto != "udp" {
		return "", apperr.New("sec.fw.rangeNeedsProto")
	}
	src, err := normAddr(r.Source)
	if err != nil {
		return "", err
	}
	if src != "any" && (net.ParseIP(strings.Split(src, "/")[0]).To4() == nil) != r.V6 {
		return "", apperr.New("sec.fw.invalidAddr", "addr", src)
	}
	parts := []string{bin}
	if r.Top {
		parts = append(parts, "-I", r.Chain, "1")
	} else {
		parts = append(parts, "-A", r.Chain)
	}
	if r.Interface != "" {
		if !reIface.MatchString(r.Interface) {
			return "", apperr.New("sec.fw.invalidIface", "iface", r.Interface)
		}
		flag := "-i"
		if r.Chain == "OUTPUT" {
			flag = "-o"
		}
		parts = append(parts, flag, core.Q(r.Interface))
	}
	if proto != "all" {
		parts = append(parts, "-p", proto)
	}
	if src != "any" {
		parts = append(parts, "-s", core.Q(src))
	}
	if ports.Raw != "" {
		parts = append(parts, "-m", proto, "--dport", ports.Raw)
	}
	if c := sanitizeComment(r.Comment, 200); c != "" {
		parts = append(parts, "-m", "comment", "--comment", core.Q(c))
	}
	parts = append(parts, "-j", r.Target)
	return strings.Join(parts, " "), nil
}

// IptablesAddRule adds a simple rule to a built-in chain. Rules are not
// persistent until IptablesSave.
func (s *SecurityService) IptablesAddRule(connID string, rule NewIptRule, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	cmd, err := func() (string, error) {
		cmd, err := iptablesCommand(rule)
		if err != nil {
			return "", err
		}
		conn, err := s.core.Conn(connID)
		if err != nil {
			return cmd, err
		}
		ctx, cancel := core.Timeout(defaultTimeout)
		defer cancel()
		if !rule.Force && rule.Chain == "INPUT" {
			ports, _ := parsePortSpec(rule.Port, ":")
			src, _ := normAddr(rule.Source)
			proto := strings.ToLower(rule.Proto)
			ip, _ := s.sessionClient(ctx, conn)
			if (net.ParseIP(ip).To4() == nil) == rule.V6 && ruleBlocksSSH(rule.Target, "in", ports, proto, src, ip, s.sshPorts(ctx, conn, sudoPassword)) {
				return cmd, apperr.New("sec.fw.blocksSsh")
			}
		}
		_, err = s.rootOK(ctx, conn, cmd, sudoPassword, "")
		return cmd, err
	}()
	s.core.Audit(connID, "sec.firewall.add", "iptables", cmd, err)
	return err
}

// IptablesDeleteRule deletes rule num of chain after checking its spec is
// still expect.
func (s *SecurityService) IptablesDeleteRule(connID string, v6 bool, chain string, num int, expect string, force bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	err := func() error {
		if !slices.Contains([]string{"INPUT", "OUTPUT", "FORWARD"}, chain) || num < 1 {
			return apperr.New("sec.fw.ruleChanged")
		}
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(defaultTimeout)
		defer cancel()
		res, err := s.root(ctx, conn, iptablesScript(v6), sudoPassword, "")
		if err != nil {
			return err
		}
		st := parseIptables(res.Stdout, v6)
		var c *IptChain
		for i := range st.Chains {
			if st.Chains[i].Name == chain {
				c = &st.Chains[i]
			}
		}
		if c == nil || num > len(c.Rules) || c.Rules[num-1].Spec != strings.TrimSpace(expect) {
			return apperr.New("sec.fw.ruleChanged")
		}
		r := c.Rules[num-1]
		if !force && chain == "INPUT" && r.Target == "ACCEPT" && c.Policy != "ACCEPT" {
			for _, p := range s.sshPorts(ctx, conn, sudoPassword) {
				if strings.Contains(r.Spec+" ", "--dport "+strconv.Itoa(p)+" ") {
					return apperr.New("sec.fw.sshRule")
				}
			}
		}
		bin := "iptables"
		if v6 {
			bin = "ip6tables"
		}
		_, err = s.rootOK(ctx, conn, bin+" -D "+chain+" "+strconv.Itoa(num), sudoPassword, "")
		return err
	}()
	s.core.Audit(connID, "sec.firewall.delete", "iptables", fmt.Sprintf("%s #%d %s", chain, num, expect), err)
	return err
}

// IptablesSave persists the current rules; returns the method used.
func (s *SecurityService) IptablesSave(connID, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return "", err
	}
	method, err := func() (string, error) {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return "", err
		}
		ctx, cancel := core.Timeout(defaultTimeout)
		defer cancel()
		script := iptablesSaveScript
		res, err := s.root(ctx, conn, script, sudoPassword, "")
		if err != nil {
			return "", err
		}
		if strings.Contains(res.Stdout, "@@none") {
			return "", apperr.New("sec.fw.noPersist")
		}
		if res.ExitCode != 0 {
			return "", apperr.New("sec.fw.failed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
		}
		_, m, _ := strings.Cut(res.Stdout, "@@method ")
		return strings.TrimSpace(m), nil
	}()
	s.core.Audit(connID, "sec.firewall.save", "iptables", method, err)
	return method, err
}

// NftRuleset returns `nft list ruleset` (read-only view).
func (s *SecurityService) NftRuleset(connID, sudoPassword string) (string, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	res, err := s.root(ctx, conn, `command -v nft >/dev/null 2>&1 || { echo @@missing; exit 0; }; nft list ruleset`, sudoPassword, "")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(res.Stdout, "@@missing") {
		return "", apperr.New("sec.fw.notInstalled", "tool", "nftables")
	}
	if res.ExitCode != 0 {
		return "", core.CmdError(res)
	}
	return res.Stdout, nil
}

// ================= generic =================

// PortCheck tells whether the active firewall lets a port in.
type PortCheck struct {
	Backend string `json:"backend"`
	Active  bool   `json:"active"`
	Allowed string `json:"allowed"` // yes | no | unknown
}

// CheckPort reports whether incoming port/proto is allowed by the active firewall.
func (s *SecurityService) CheckPort(connID string, port int, proto, sudoPassword string) (PortCheck, error) {
	if !validPort(port) {
		return PortCheck{}, apperr.New("sec.invalidPort", "port", strconv.Itoa(port))
	}
	pr, err := validProto(proto, false)
	if err != nil {
		return PortCheck{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return PortCheck{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	info, err := s.detect(ctx, conn, sudoPassword)
	if err != nil {
		return PortCheck{}, err
	}
	pc := PortCheck{Backend: info.Primary, Allowed: "unknown"}
	for _, b := range info.Backends {
		if b.Name == info.Primary {
			pc.Active = b.Active
		}
	}
	if !pc.Active {
		pc.Allowed = "yes"
		return pc, nil
	}
	switch info.Primary {
	case "ufw":
		st, err := s.ufwStatus(ctx, conn, sudoPassword)
		if err != nil {
			return pc, err
		}
		pc.Allowed = "no"
		if st.Defaults.Incoming == "allow" {
			pc.Allowed = "yes"
		}
		for _, r := range st.Rules {
			if (r.Action == "ALLOW" || r.Action == "LIMIT") && r.Direction == "IN" && strings.HasPrefix(r.From, "Anywhere") && ufwCoversPort(r.To, port, pr) {
				pc.Allowed = "yes"
			}
		}
	case "firewalld":
		script := `P=` + core.Q(fmt.Sprintf("%d/%s", port, pr)) + `
for z in $( (firewall-cmd --get-default-zone; firewall-cmd --get-active-zones | grep -v '^[[:space:]]') | sort -u); do
  firewall-cmd --zone="$z" --query-port="$P" >/dev/null 2>&1 && { echo yes; exit 0; }
  for sv in $(firewall-cmd --zone="$z" --list-services); do
    firewall-cmd --info-service="$sv" 2>/dev/null | grep -E '^[[:space:]]*ports:' | tr ' ' '\n' | grep -qx "$P" && { echo yes; exit 0; }
  done
done
echo no`
		res, err := s.root(ctx, conn, script, sudoPassword, "")
		if err != nil {
			return pc, err
		}
		pc.Allowed = strings.TrimSpace(res.Stdout)
	case "iptables":
		res, err := s.root(ctx, conn, iptablesScript(false), sudoPassword, "")
		if err != nil {
			return pc, err
		}
		st := parseIptables(res.Stdout, false)
		for _, c := range st.Chains {
			if c.Name != "INPUT" {
				continue
			}
			blocks := false
			for _, r := range c.Rules {
				if (r.Target == "DROP" || r.Target == "REJECT") && !strings.Contains(r.Spec, "--dport") && !strings.Contains(r.Spec, " -s ") {
					blocks = true
				}
				if r.Target == "ACCEPT" && strings.Contains(r.Spec+" ", "--dport "+strconv.Itoa(port)+" ") && !strings.Contains(r.Spec, " -s ") {
					pc.Allowed = "yes"
					return pc, nil
				}
			}
			if c.Policy == "ACCEPT" && !blocks {
				pc.Allowed = "yes"
			} else if c.Policy != "ACCEPT" {
				pc.Allowed = "no"
			}
		}
	}
	return pc, nil
}

// AllowPort opens an incoming port on the primary firewall (ufw rule,
// firewalld default zone, or iptables INPUT). Returns the backend used.
func (s *SecurityService) AllowPort(connID string, port int, proto, comment, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return "", err
	}
	backend, cmd, err := s.allowPort(connID, port, proto, comment, sudoPassword)
	s.core.Audit(connID, "sec.firewall.add", backend, core.FirstNonEmpty(cmd, fmt.Sprintf("allow %d/%s", port, proto)), err)
	return backend, err
}

func (s *SecurityService) allowPort(connID string, port int, proto, comment, pw string) (string, string, error) {
	if !validPort(port) {
		return "", "", apperr.New("sec.invalidPort", "port", strconv.Itoa(port))
	}
	pr, err := validProto(proto, false)
	if err != nil {
		return "", "", err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := core.Timeout(90 * time.Second)
	defer cancel()
	info, err := s.detect(ctx, conn, pw)
	if err != nil {
		return "", "", err
	}
	c := sanitizeComment(comment, 80)
	var cmd string
	switch info.Primary {
	case "ufw":
		cmd = fmt.Sprintf("ufw allow %d/%s", port, pr)
		if c != "" {
			cmd += " comment " + core.Q(c)
		}
	case "firewalld":
		cmd = fmt.Sprintf("firewall-cmd --permanent --add-port=%d/%s && firewall-cmd --reload", port, pr)
	case "iptables":
		cmd = fmt.Sprintf("iptables -I INPUT 1 -p %s -m %s --dport %d", pr, pr, port)
		if c != "" {
			cmd += " -m comment --comment " + core.Q(c)
		}
		cmd += " -j ACCEPT"
		cmd += fmt.Sprintf("; command -v ip6tables >/dev/null 2>&1 && ip6tables -I INPUT 1 -p %s -m %s --dport %d -j ACCEPT; true", pr, pr, port)
	default:
		return info.Primary, "", apperr.New("sec.fw.none")
	}
	res, err := s.root(ctx, conn, cmd, pw, "")
	if err != nil {
		return info.Primary, cmd, err
	}
	if res.ExitCode != 0 || strings.Contains(res.Stdout, "ERROR") {
		return info.Primary, cmd, apperr.New("sec.fw.failed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	return info.Primary, cmd, nil
}
