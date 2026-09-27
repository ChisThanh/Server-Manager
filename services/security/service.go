// Package security: SSH daemon settings, authorized keys, firewalls, open
// ports, security events and Linux users & groups.
//
// Everything is agentless: the service drives tools that already exist on
// the server (sshd, ufw, firewall-cmd, iptables, nft, ss/netstat, journalctl,
// last, useradd/adduser…) over SSH. Reading needs no permission; every change
// requires PermSecurity (sshd, keys, firewall, fail2ban) or PermUsers (Linux
// accounts) and is recorded in the audit log.
package security

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

type SecurityService struct {
	core *core.Core
}

func New(c *core.Core) *SecurityService { return &SecurityService{core: c} }

// pathPrefix makes admin tools in sbin reachable for non-root logins and
// forces a stable C locale so outputs parse the same everywhere.
const pathPrefix = `PATH="$PATH:/usr/local/sbin:/usr/sbin:/sbin"; export PATH; LC_ALL=C; export LC_ALL; `

// run executes a snippet as root (through sudo unless the login is root).
func (s *SecurityService) root(ctx context.Context, conn *sshx.Conn, cmd, pw, stdin string) (sshx.ExecResult, error) {
	return s.core.Run(ctx, conn, pathPrefix+cmd, true, pw, stdin)
}

// rootOK is root() that fails on a non-zero exit.
func (s *SecurityService) rootOK(ctx context.Context, conn *sshx.Conn, cmd, pw, stdin string) (sshx.ExecResult, error) {
	return s.core.RunOK(ctx, conn, pathPrefix+cmd, true, pw, stdin)
}

// user executes a snippet as the login user.
func (s *SecurityService) user(ctx context.Context, conn *sshx.Conn, cmd, stdin string) (sshx.ExecResult, error) {
	return s.core.Run(ctx, conn, pathPrefix+cmd, false, "", stdin)
}

// rootOrUser runs cmd as root when possible; when the account cannot use
// sudo at all (not in sudoers / sudo missing) it falls back to the login
// user and reports limited=true. A missing or wrong password is returned so
// the UI can prompt.
func (s *SecurityService) rootOrUser(ctx context.Context, conn *sshx.Conn, cmd, pw, stdin string) (res sshx.ExecResult, limited bool, err error) {
	res, err = s.root(ctx, conn, cmd, pw, stdin)
	if err != nil && (apperr.HasCode(err, "sudo.notAllowed") || apperr.HasCode(err, "sudo.missing")) {
		res, err = s.user(ctx, conn, cmd, stdin)
		return res, true, err
	}
	return res, false, err
}

// sections splits a script output made of "@@name" markers.
func sections(out string) map[string]string {
	m := map[string]string{}
	cur := ""
	var b strings.Builder
	flush := func() {
		if cur != "" {
			m[cur] = b.String()
		}
		b.Reset()
	}
	for _, line := range strings.SplitAfter(out, "\n") {
		trim := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trim, "@@") && !strings.ContainsAny(trim[2:], " \t") && len(trim) > 2 {
			flush()
			cur = trim[2:]
			continue
		}
		if cur != "" {
			b.WriteString(line)
		}
	}
	flush()
	return m
}

func lines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// ---- server context ----

// hostInfo describes the distro and the login used by the app.
type hostInfo struct {
	ID       string // os-release ID
	Like     string // ID_LIKE
	Family   string // debian | rhel | alpine | suse | arch | other
	Pkg      string // apt | dnf | yum | apk | zypper | pacman | ""
	Busybox  bool   // BusyBox user tools (adduser/deluser) instead of shadow
	TZOffset int    // seconds east of UTC (server local time)
}

const hostScript = `. /etc/os-release 2>/dev/null; echo "id=$ID"; echo "like=$ID_LIKE"
for p in apt-get dnf yum apk zypper pacman; do command -v $p >/dev/null 2>&1 && { echo "pkg=$p"; break; }; done
command -v useradd >/dev/null 2>&1 && echo shadow=1
echo "tz=$(date +%z)"`

func (s *SecurityService) host(ctx context.Context, conn *sshx.Conn) hostInfo {
	res, _ := s.user(ctx, conn, hostScript, "")
	return parseHost(res.Stdout)
}

func parseHost(out string) hostInfo {
	h := hostInfo{Busybox: true}
	for _, l := range lines(out) {
		k, v, _ := strings.Cut(l, "=")
		v = strings.TrimSpace(v)
		switch k {
		case "id":
			h.ID = v
		case "like":
			h.Like = v
		case "pkg":
			h.Pkg = strings.TrimSuffix(v, "-get")
		case "shadow":
			h.Busybox = false
		case "tz":
			h.TZOffset = parseTZ(v)
		}
	}
	all := " " + h.ID + " " + h.Like + " "
	switch {
	case strings.Contains(all, " debian ") || strings.Contains(all, " ubuntu "):
		h.Family = "debian"
	case strings.Contains(all, " rhel ") || strings.Contains(all, " fedora ") || strings.Contains(all, " centos "):
		h.Family = "rhel"
	case strings.Contains(all, " alpine "):
		h.Family = "alpine"
	case strings.Contains(all, "suse"):
		h.Family = "suse"
	case strings.Contains(all, " arch "):
		h.Family = "arch"
	default:
		h.Family = "other"
	}
	return h
}

func parseTZ(v string) int {
	if len(v) != 5 || (v[0] != '+' && v[0] != '-') {
		return 0
	}
	hh, m := atoi(v[1:3]), atoi(v[3:5])
	off := hh*3600 + m*60
	if v[0] == '-' {
		off = -off
	}
	return off
}

// sessionPort returns the server-side port of the current SSH session
// (from $SSH_CONNECTION), falling back to the profile's port.
func (s *SecurityService) sessionPort(ctx context.Context, conn *sshx.Conn) int {
	res, _ := s.user(ctx, conn, `echo "$SSH_CONNECTION"`, "")
	if f := strings.Fields(res.Stdout); len(f) == 4 {
		if p := atoi(f[3]); p > 0 {
			return p
		}
	}
	return conn.Server().Port
}

// loginUser is the account the app is connected as.
func loginUser(conn *sshx.Conn) string { return conn.Server().User }

// ---- validation ----

var (
	reUser      = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reUserLoose = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}\$?$`) // existing accounts (may predate the strict rule)
	reGroup     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
	reIface     = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,15}$`)
	reCtl       = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	reComment   = regexp.MustCompile(`[^\p{L}\p{N} _.,:/()@+#=-]`)
)

// validPort checks a single TCP/UDP port.
func validPort(p int) bool { return p >= 1 && p <= 65535 }

// sanitizeComment keeps a short, shell- and config-safe comment.
func sanitizeComment(c string, max int) string {
	c = reComment.ReplaceAllString(strings.TrimSpace(c), "")
	c = strings.Join(strings.Fields(c), " ")
	if r := []rune(c); len(r) > max {
		c = string(r[:max])
	}
	return c
}

// existingUser validates a user name read from the UI that must already
// exist (older systems allow upper case or dots).
func existingUser(name string) error {
	if !reUserLoose.MatchString(name) {
		return apperr.New("sec.invalidUser", "name", name)
	}
	return nil
}

// defaultTimeout for most remote operations.
const defaultTimeout = 45 * time.Second
