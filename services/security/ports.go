package security

import (
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
)

// Listener is one listening socket.
type Listener struct {
	Proto   string `json:"proto"` // tcp | udp
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Scope: any (all interfaces) | loopback | specific (one address).
	Scope   string `json:"scope"`
	Public  bool   `json:"public"`
	Process string `json:"process"`
	PIDs    []int  `json:"pids"`
	V6      bool   `json:"v6"`
}

type PortsResult struct {
	Tool      string     `json:"tool"` // ss | netstat
	Listeners []Listener `json:"listeners"`
	// Limited: process names of other users are hidden (no root).
	Limited bool `json:"limited"`
}

var reSSUser = regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

// splitHostPort handles "0.0.0.0:80", "[::]:80", "*:3306", "127.0.0.53%lo:53",
// ":::22" (netstat) and "[fe80::1%eth0]:546".
func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	host, p := s[:i], s[i+1:]
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, false
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if j := strings.Index(host, "%"); j >= 0 {
		host = host[:j]
	}
	if host == "" || host == "::" {
		host = "::"
	}
	return host, port, true
}

func classify(l *Listener) {
	h := l.Address
	switch h {
	case "*", "0.0.0.0", "::":
		l.Scope, l.Public = "any", true
		l.V6 = h == "::"
		return
	}
	ip := net.ParseIP(h)
	if ip == nil {
		l.Scope = "specific"
		return
	}
	l.V6 = ip.To4() == nil
	switch {
	case ip.IsLoopback():
		l.Scope = "loopback"
	case ip.IsLinkLocalUnicast():
		l.Scope, l.Public = "specific", false
	default:
		l.Scope, l.Public = "specific", true
	}
}

// parseSS parses `ss -tulpnH` (Netid State Recv-Q Send-Q Local Peer Process).
func parseSS(out string) []Listener {
	res := []Listener{}
	for _, line := range lines(out) {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		proto := f[0]
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if proto == "tcp" && f[1] != "LISTEN" {
			continue
		}
		host, port, ok := splitHostPort(f[4])
		if !ok {
			continue
		}
		l := Listener{Proto: proto, Address: host, Port: port, PIDs: []int{}}
		if len(f) > 6 {
			rest := strings.Join(f[6:], " ")
			for _, m := range reSSUser.FindAllStringSubmatch(rest, -1) {
				if l.Process == "" {
					l.Process = m[1]
				}
				if pid := atoi(m[2]); !slices.Contains(l.PIDs, pid) {
					l.PIDs = append(l.PIDs, pid)
				}
			}
		}
		classify(&l)
		res = append(res, l)
	}
	return res
}

// parseNetstat parses `netstat -tulpn` (GNU or BusyBox).
func parseNetstat(out string) []Listener {
	res := []Listener{}
	for _, line := range lines(out) {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		proto := f[0]
		v6 := strings.HasSuffix(proto, "6")
		proto = strings.TrimSuffix(proto, "6")
		if proto != "tcp" && proto != "udp" {
			continue
		}
		if proto == "tcp" && (len(f) < 6 || f[5] != "LISTEN") {
			continue
		}
		host, port, ok := splitHostPort(f[3])
		if !ok {
			continue
		}
		l := Listener{Proto: proto, Address: host, Port: port, PIDs: []int{}}
		last := f[len(f)-1]
		if pid, name, ok := strings.Cut(last, "/"); ok {
			if n := atoi(pid); n > 0 {
				l.PIDs = append(l.PIDs, n)
				l.Process = name
			}
		}
		classify(&l)
		l.V6 = l.V6 || v6
		res = append(res, l)
	}
	return res
}

// OpenPorts lists listening TCP/UDP sockets. With useSudo the process of
// every socket is shown; without, only the login user's.
func (s *SecurityService) OpenPorts(connID string, useSudo bool, sudoPassword string) (PortsResult, error) {
	out := PortsResult{Listeners: []Listener{}}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return out, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	script := `if command -v ss >/dev/null 2>&1; then echo @@ss; ss -tulpnH 2>/dev/null || ss -tulpn 2>/dev/null | tail -n +2
elif command -v netstat >/dev/null 2>&1; then echo @@netstat; netstat -tulpn 2>/dev/null || netstat -tuln
else echo @@none; fi`
	var raw string
	if useSudo {
		res, limited, err := s.rootOrUser(ctx, conn, script, sudoPassword, "")
		if err != nil {
			return out, err
		}
		out.Limited = limited && !s.core.IsRoot(ctx, conn)
		raw = res.Stdout
	} else {
		res, err := s.user(ctx, conn, script, "")
		if err != nil {
			return out, err
		}
		out.Limited = !s.core.IsRoot(ctx, conn)
		raw = res.Stdout
	}
	sec := sections(raw)
	switch {
	case strings.Contains(raw, "@@ss"):
		out.Tool = "ss"
		out.Listeners = parseSS(sec["ss"])
	case strings.Contains(raw, "@@netstat"):
		out.Tool = "netstat"
		out.Listeners = parseNetstat(sec["netstat"])
	default:
		return out, apperr.New("sec.ports.noTool")
	}
	slices.SortFunc(out.Listeners, func(a, b Listener) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		if a.Proto != b.Proto {
			return strings.Compare(a.Proto, b.Proto)
		}
		return strings.Compare(a.Address, b.Address)
	})
	return out, nil
}
