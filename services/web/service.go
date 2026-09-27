// Package web: Reverse proxy sites (Nginx/Caddy) and TLS certificates (Let's Encrypt/custom).
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

type WebService struct {
	core *core.Core
}

func New(c *core.Core) *WebService { return &WebService{core: c} }

// Non-root logins often lack the sbin directories in PATH.
const pathPrefix = `export PATH="$PATH:/usr/local/sbin:/usr/sbin:/sbin:/snap/bin"; `

// nonce returns a random marker so command output sections can't be
// confused with file contents.
func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "@@SM" + hex.EncodeToString(b)
}

// sections splits output on lines "<marker> <name> [args]" into name → body.
// Repeated names get their args joined into the key ("file /x").
type section struct {
	Name string
	Arg  string
	Body string
}

func splitSections(out, marker string) []section {
	var res []section
	var cur *section
	var b strings.Builder
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSuffix(b.String(), "\n")
			res = append(res, *cur)
		}
		b.Reset()
	}
	for _, line := range strings.SplitAfter(out, "\n") {
		trim := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trim, marker+" ") {
			flush()
			rest := strings.TrimPrefix(trim, marker+" ")
			name, arg, _ := strings.Cut(rest, " ")
			cur = &section{Name: name, Arg: arg}
			continue
		}
		if cur != nil {
			b.WriteString(line)
		}
	}
	flush()
	return res
}

func sectionMap(secs []section) map[string]string {
	m := map[string]string{}
	for _, s := range secs {
		k := s.Name
		if s.Arg != "" {
			k += " " + s.Arg
		}
		m[k] = s.Body
	}
	return m
}

func (s *WebService) sudo(ctx context.Context, conn *sshx.Conn, pw, cmd, stdin string) (sshx.ExecResult, error) {
	return s.core.Run(ctx, conn, pathPrefix+cmd, true, pw, stdin)
}

func (s *WebService) plain(ctx context.Context, conn *sshx.Conn, cmd string) (sshx.ExecResult, error) {
	return s.core.Run(ctx, conn, pathPrefix+cmd, false, "", "")
}

// ---- detection ----

const detectScript = `
M=%s
echo "$M systemd"; [ -d /run/systemd/system ] && echo yes
echo "$M pkg"; for p in apt-get dnf yum apk zypper pacman; do command -v $p >/dev/null 2>&1 && { echo $p; break; }; done
for e in nginx caddy apache2 httpd traefik certbot; do echo "$M bin $e"; command -v $e 2>/dev/null; done
echo "$M ver nginx"; nginx -v 2>&1 | head -n1
echo "$M ver caddy"; caddy version 2>/dev/null | head -n1
echo "$M ver apache"; (apache2 -v 2>/dev/null || apache2ctl -v 2>/dev/null || httpd -v 2>/dev/null) | head -n1
echo "$M ver traefik"; traefik version 2>/dev/null | grep -i version | head -n1
echo "$M ver certbot"; certbot --version 2>&1 | grep -i certbot | head -n1
for u in nginx caddy apache2 httpd traefik; do echo "$M unit $u"; systemctl is-active $u.service 2>/dev/null | head -n1; systemctl is-enabled $u.service 2>/dev/null | head -n1; done
echo "$M proc"; ps -eo comm= 2>/dev/null | sort -u | grep -E '^(nginx|caddy|apache2|httpd|traefik)$'
echo "$M docker"; docker ps --format '{{.Image}} {{.Names}}' 2>/dev/null | grep -i traefik | head -n3
echo "$M nginxconf"; C=$(nginx -V 2>&1 | sed -n 's/.*--conf-path=\([^ ]*\).*/\1/p'); echo "${C:-/etc/nginx/nginx.conf}"
echo "$M nginxlayout"; C=${C:-/etc/nginx/nginx.conf}; grep -Eqs '^[[:space:]]*include[[:space:]]+[^;]*sites-enabled' "$C" && echo debian || echo confd
echo "$M caddyimport"; grep -Es '^[[:space:]]*import[[:space:]]+(/etc/caddy/)?sites/\*\.caddy' /etc/caddy/Caddyfile | head -n1
echo "$M listen"; (ss -Hltn 2>/dev/null || netstat -ltn 2>/dev/null) | awk '{for(i=1;i<=NF;i++) if ($i ~ /:(80|443)$/) print $i}' | sort -u
echo "$M end"
`

// Status detects the web servers, certbot and the package manager, and
// validates the nginx/caddy configurations.
func (s *WebService) Status(connID, sudoPassword string) (WebStatus, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return WebStatus{}, err
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	return s.status(ctx, conn, sudoPassword, true)
}

func (s *WebService) status(ctx context.Context, conn *sshx.Conn, pw string, test bool) (WebStatus, error) {
	m := nonce()
	res, err := s.plain(ctx, conn, strings.Replace(detectScript, "%s", m, 1))
	if err != nil {
		return WebStatus{}, err
	}
	sec := sectionMap(splitSections(res.Stdout, m))
	line := func(k string) string {
		v := strings.TrimSpace(sec[k])
		if i := strings.IndexByte(v, '\n'); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	st := WebStatus{
		Systemd:    line("systemd") == "yes",
		PkgManager: strings.TrimSuffix(line("pkg"), "-get"),
		Listeners:  nonEmptyLines(sec["listen"]),
		Webroot:    acmeWebroot,
		Engines:    []EngineInfo{},
	}
	procs := map[string]bool{}
	for _, p := range nonEmptyLines(sec["proc"]) {
		procs[p] = true
	}
	unit := func(name string) (active, enabled bool) {
		ls := nonEmptyLines(sec["unit "+name])
		if len(ls) > 0 {
			active = ls[0] == "active" || ls[0] == "reloading"
		}
		if len(ls) > 1 {
			enabled = ls[1] == "enabled" || ls[1] == "static" || ls[1] == "alias"
		}
		return
	}
	st.CertbotVer = versionIn(line("ver certbot"))
	st.Certbot = line("bin certbot") != ""

	// nginx
	ng := EngineInfo{Name: EngineNginx, Binary: line("bin nginx"), Service: "nginx", Editable: true, Config: line("nginxconf"), Layout: line("nginxlayout")}
	ng.Installed = ng.Binary != ""
	ng.Version = versionAfter(line("ver nginx"), "nginx/")
	a, e := unit("nginx")
	ng.Running, ng.Enabled = a || procs["nginx"], e
	// caddy
	cd := EngineInfo{Name: EngineCaddy, Binary: line("bin caddy"), Service: "caddy", Editable: true, Config: "/etc/caddy/Caddyfile", Layout: "caddyfile"}
	cd.Installed = cd.Binary != ""
	cd.Version = strings.TrimPrefix(firstField(line("ver caddy")), "v")
	a, e = unit("caddy")
	cd.Running, cd.Enabled = a || procs["caddy"], e
	cd.ImportsSites = line("caddyimport") != ""
	// apache (read-only)
	ap := EngineInfo{Name: "apache", Binary: core.FirstNonEmpty(line("bin apache2"), line("bin httpd"))}
	ap.Installed = ap.Binary != ""
	ap.Version = versionAfter(line("ver apache"), "Apache/")
	a1, e1 := unit("apache2")
	a2, e2 := unit("httpd")
	ap.Running, ap.Enabled = a1 || a2 || procs["apache2"] || procs["httpd"], e1 || e2
	ap.Service = "apache2"
	if line("bin httpd") != "" && line("bin apache2") == "" {
		ap.Service = "httpd"
	}
	// traefik (read-only)
	tr := EngineInfo{Name: "traefik", Binary: line("bin traefik"), Service: "traefik"}
	tr.Docker = strings.TrimSpace(sec["docker"]) != ""
	tr.Installed = tr.Binary != "" || tr.Docker
	tr.Version = versionIn(line("ver traefik"))
	a, e = unit("traefik")
	tr.Running, tr.Enabled = a || procs["traefik"] || tr.Docker, e

	if test && ng.Installed {
		r, err := s.core.RunAuto(ctx, conn, pathPrefix+"nginx -t 2>&1", pw, "")
		if err != nil {
			return st, err
		}
		ng.TestRun, ng.TestOK, ng.TestOutput = true, r.ExitCode == 0, tail(r.Stdout+r.Stderr, 40)
	}
	if test && cd.Installed {
		r, err := s.sudo(ctx, conn, pw, caddyValidateCmd, "")
		if err != nil {
			return st, err
		}
		cd.TestRun, cd.TestOK, cd.TestOutput = true, r.ExitCode == 0, caddyOutput(r.Stdout+r.Stderr)
	}
	for _, e := range []EngineInfo{ng, cd, ap, tr} {
		st.Engines = append(st.Engines, e)
	}
	return st, nil
}

// caddyValidateCmd validates as the caddy service user when it exists, so
// files caddy creates (logs) get the right owner and unreadable certificate
// files are caught like the real service would.
const caddyValidateCmd = `cd / && if id caddy >/dev/null 2>&1; then su -s /bin/sh -c 'HOME=/var/lib/caddy XDG_DATA_HOME=/var/lib/caddy/.local/share caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile' caddy 2>&1; else caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1; fi`

// caddyOutput drops caddy's JSON info log lines, keeping errors/warnings.
func caddyOutput(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || (strings.HasPrefix(t, "{") && strings.Contains(t, `"level":"info"`)) {
			continue
		}
		keep = append(keep, l)
	}
	return tail(strings.Join(keep, "\n"), 40)
}

func nonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// versionAfter extracts "1.22.1" from "nginx version: nginx/1.22.1 (Ubuntu)".
func versionAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return versionIn(s)
	}
	return firstField(s[i+len(marker):])
}

// versionIn finds the first dotted version number in s.
func versionIn(s string) string {
	for _, f := range strings.Fields(s) {
		f = strings.TrimPrefix(strings.TrimPrefix(f, "v"), "V")
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			return strings.TrimRight(f, ",;")
		}
	}
	return ""
}

// errNotInstalled is returned when an engine is required but missing.
func errNotInstalled(engine string) error {
	return apperr.New("web.notInstalled", "engine", engine)
}
