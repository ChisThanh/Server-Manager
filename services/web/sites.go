package web

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// nginxEnv is everything known about the nginx installation.
type nginxEnv struct {
	Installed      bool
	Version        string
	HTTP2Directive bool
	IPv6           bool
	Main           string // nginx.conf
	Prefix         string // its directory
	Available      string // sites-available ("" when not using that layout)
	Enabled        string // sites-enabled
	ConfDir        string // http-level *.conf directory (conf.d / http.d)
	User           string
	Group          string
	Cfg            *ngxConfig
	TestOK         bool
	TestOutput     string
	// Files of disabled sites (not loaded by nginx): path → content.
	Disabled map[string]string
	// Enabled links: link path → resolved target.
	Links map[string]string
}

const nginxGather = `
M=%s
C=$(nginx -V 2>&1 | sed -n 's/.*--conf-path=\([^ ]*\).*/\1/p'); C=${C:-/etc/nginx/nginx.conf}; P=$(dirname "$C")
echo "$M bin"; command -v nginx
echo "$M version"; nginx -v 2>&1 | head -n1
echo "$M conf"; echo "$C"
echo "$M ipv6"; [ -f /proc/net/if_inet6 ] && echo yes
T=$(mktemp); nginx -T >"$T.out" 2>"$T"; rc=$?
echo "$M test $rc"; cat "$T"
if [ $rc -eq 0 ]; then echo "$M dump"; cat "$T.out"; else
  echo "$M dump"
  for f in "$C" "$P"/conf.d/*.conf "$P"/http.d/*.conf "$P"/sites-enabled/*; do [ -f "$f" ] && { echo "# configuration file $f:"; cat "$f"; echo; }; done
fi
rm -f "$T" "$T.out"
echo "$M links"; for l in "$P"/sites-enabled/*; do [ -L "$l" ] || [ -f "$l" ] || continue; printf '%s\t%s\n' "$l" "$(readlink -f "$l")"; done
for f in "$P"/sites-available/* "$P"/conf.d/*.conf.disabled "$P"/http.d/*.conf.disabled; do
  [ -f "$f" ] || continue
  case "$f" in *.sm-bak*|*~) continue;; esac
  en=0; for l in "$P"/sites-enabled/*; do [ "$(readlink -f "$l")" = "$(readlink -f "$f")" ] && en=1; done
  [ $en = 0 ] && { echo "$M file $f"; cat "$f"; }
done
echo "$M dirs"; for d in sites-available sites-enabled conf.d http.d; do [ -d "$P/$d" ] && echo "$d"; done
U=$(sed -n 's/^[[:space:]]*user[[:space:]]\{1,\}\([^;[:space:]]*\).*/\1/p' "$C" | head -n1); echo "$M user"; echo "$U"
echo "$M group"; [ -n "$U" ] && id -gn "$U" 2>/dev/null
echo "$M end"
`

func (s *WebService) nginxEnv(ctx context.Context, conn *sshx.Conn, pw string) (*nginxEnv, error) {
	m := nonce()
	res, err := s.sudo(ctx, conn, pw, strings.Replace(nginxGather, "%s", m, 1), "")
	if err != nil {
		return nil, err
	}
	env := &nginxEnv{Disabled: map[string]string{}, Links: map[string]string{}}
	secs := splitSections(res.Stdout, m)
	dirs := map[string]bool{}
	for _, sc := range secs {
		switch sc.Name {
		case "bin":
			env.Installed = strings.TrimSpace(sc.Body) != ""
		case "version":
			env.Version = versionAfter(sc.Body, "nginx/")
		case "conf":
			env.Main = strings.TrimSpace(sc.Body)
		case "ipv6":
			env.IPv6 = strings.TrimSpace(sc.Body) == "yes"
		case "test":
			env.TestOK = sc.Arg == "0"
			env.TestOutput = tail(sc.Body, 40)
		case "dump":
			env.Cfg = analyzeNginx(splitNginxDump(sc.Body))
		case "links":
			for _, l := range nonEmptyLines(sc.Body) {
				if a, b, ok := strings.Cut(l, "\t"); ok {
					env.Links[a] = b
				}
			}
		case "file":
			env.Disabled[sc.Arg] = sc.Body
		case "dirs":
			for _, d := range nonEmptyLines(sc.Body) {
				dirs[d] = true
			}
		case "user":
			env.User = strings.TrimSpace(sc.Body)
		case "group":
			env.Group = strings.TrimSpace(sc.Body)
		}
	}
	if !env.Installed {
		return env, errNotInstalled(EngineNginx)
	}
	if env.Main == "" {
		env.Main = "/etc/nginx/nginx.conf"
	}
	env.Prefix = path.Dir(env.Main)
	env.HTTP2Directive = versionAtLeast(env.Version, 1, 25, 1)
	if env.Cfg == nil {
		env.Cfg = analyzeNginx(nil)
	}
	if env.Cfg.User != "" {
		env.User = env.Cfg.User
	}
	// Layout: where does the http context include site files from?
	for _, inc := range env.Cfg.HTTPIncludes {
		dir, pat := path.Split(inc)
		dir = strings.TrimSuffix(dir, "/")
		switch {
		case path.Base(dir) == "sites-enabled" && env.Enabled == "":
			env.Enabled = dir
			if dirs["sites-available"] || path.Dir(dir) != env.Prefix {
				env.Available = path.Join(path.Dir(dir), "sites-available")
			}
		case pat == "*.conf" && env.ConfDir == "" && (path.Base(dir) == "conf.d" || path.Base(dir) == "http.d"):
			env.ConfDir = dir
		}
	}
	if env.Cfg.Files == nil || len(env.Cfg.Files) == 0 {
		// Couldn't read the configuration at all: fall back to the usual layout.
		if dirs["sites-enabled"] && dirs["sites-available"] {
			env.Enabled, env.Available = path.Join(env.Prefix, "sites-enabled"), path.Join(env.Prefix, "sites-available")
		}
		if dirs["conf.d"] {
			env.ConfDir = path.Join(env.Prefix, "conf.d")
		}
	}
	return env, nil
}

// siteFile is where a managed site named name lives when enabled.
func (e *nginxEnv) siteFile(name string) (string, error) {
	switch {
	case e.Available != "" && e.Enabled != "":
		return path.Join(e.Available, name+".conf"), nil
	case e.ConfDir != "":
		return path.Join(e.ConfDir, name+".conf"), nil
	}
	return "", apperr.New("web.nginxLayoutUnknown")
}

func (e *nginxEnv) linkFor(file string) string {
	if e.Available != "" && path.Dir(file) == e.Available {
		return path.Join(e.Enabled, path.Base(file))
	}
	return ""
}

// canToggle: the file can be enabled/disabled by the app.
func (e *nginxEnv) canToggle(file string) bool {
	d := path.Dir(file)
	return (e.Available != "" && d == e.Available) || (e.ConfDir != "" && d == e.ConfDir && (strings.HasSuffix(file, ".conf") || strings.HasSuffix(file, ".conf.disabled")))
}

// sites builds the site list from the environment.
func (e *nginxEnv) sites() []Site {
	byFile := map[string][]ServerBlock{}
	order := []string{}
	contents := map[string]string{}
	for _, f := range e.Cfg.Files {
		contents[f.Path] = f.Content
	}
	for _, sv := range e.Cfg.Servers {
		if _, ok := byFile[sv.File]; !ok {
			order = append(order, sv.File)
		}
		byFile[sv.File] = append(byFile[sv.File], summarizeServer(sv.Block))
	}
	out := []Site{}
	enabledTargets := map[string]bool{}
	for _, t := range e.Links {
		enabledTargets[t] = true
	}
	add := func(file, content string, blocks []ServerBlock, enabled bool) {
		st := buildSite(EngineNginx, e.displayPath(file), blocks, e.Cfg.Upstreams)
		st.Enabled = enabled
		st.CanToggle = e.canToggle(st.File)
		st.Managed, st.Spec = parseMeta(content)
		if st.Spec != nil {
			st.Name = st.Spec.Domains[0]
			if st.Spec.SSL == SSLLetsEncrypt && !st.HTTPS {
				st.PendingCert = true
			}
		}
		out = append(out, st)
	}
	for _, f := range order {
		add(f, contents[f], byFile[f], true)
	}
	// Managed files without a server block (shouldn't happen) are skipped;
	// disabled files are parsed on their own (top level = http context).
	dis := make([]string, 0, len(e.Disabled))
	for f := range e.Disabled {
		dis = append(dis, f)
	}
	sort.Strings(dis)
	for _, f := range dis {
		c := e.Disabled[f]
		var blocks []ServerBlock
		for _, d := range ngxParse(c) {
			if d.Name == "server" && d.HasBlock {
				blocks = append(blocks, summarizeServer(d))
			}
		}
		if len(blocks) == 0 {
			continue
		}
		add(f, c, blocks, false)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Enabled != out[j].Enabled {
			return out[i].Enabled
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// displayPath maps a sites-enabled link path to the sites-available file.
func (e *nginxEnv) displayPath(file string) string {
	if t, ok := e.Links[file]; ok && t != "" {
		return t
	}
	return file
}

// fileContent returns the content of a known configuration file.
func (e *nginxEnv) fileContent(file string) (string, bool) {
	for _, f := range e.Cfg.Files {
		if f.Path == file || e.displayPath(f.Path) == file {
			return f.Content, true
		}
	}
	c, ok := e.Disabled[file]
	return c, ok
}

// ---- caddy ----

type caddyEnv struct {
	Installed  bool
	Version    string
	Running    bool
	Main       string
	Caddyfile  string
	Files      []ngxFile // Caddyfile + imported files
	Disabled   map[string]string
	Imports    bool // Caddyfile imports sites/*.caddy
	SitesDir   string
	HasCaddyGr bool
}

var caddyImportRe = regexp.MustCompile(`(?m)^[ \t]*import[ \t]+(/etc/caddy/)?sites/\*\.caddy[ \t]*$`)
var globSafe = regexp.MustCompile(`^[A-Za-z0-9._/*?\[\]-]+$`)

const caddyGather = `
M=%s
echo "$M bin"; command -v caddy
echo "$M version"; caddy version 2>/dev/null | head -n1
echo "$M running"; (systemctl is-active caddy 2>/dev/null || true) | head -n1; pgrep -x caddy >/dev/null 2>&1 && echo proc
echo "$M group"; getent group caddy >/dev/null 2>&1 && echo yes
echo "$M main"; cat /etc/caddy/Caddyfile 2>/dev/null
for f in /etc/caddy/sites/*.caddy.disabled; do [ -f "$f" ] && { echo "$M file $f"; cat "$f"; }; done
echo "$M end"
`

func (s *WebService) caddyEnv(ctx context.Context, conn *sshx.Conn, pw string) (*caddyEnv, error) {
	m := nonce()
	res, err := s.sudo(ctx, conn, pw, strings.Replace(caddyGather, "%s", m, 1), "")
	if err != nil {
		return nil, err
	}
	env := &caddyEnv{Main: "/etc/caddy/Caddyfile", SitesDir: "/etc/caddy/sites", Disabled: map[string]string{}}
	for _, sc := range splitSections(res.Stdout, m) {
		switch sc.Name {
		case "bin":
			env.Installed = strings.TrimSpace(sc.Body) != ""
		case "version":
			env.Version = strings.TrimPrefix(firstField(sc.Body), "v")
		case "running":
			b := strings.TrimSpace(sc.Body)
			env.Running = strings.HasPrefix(b, "active") || strings.Contains(b, "proc")
		case "group":
			env.HasCaddyGr = strings.TrimSpace(sc.Body) == "yes"
		case "main":
			env.Caddyfile = sc.Body
		case "file":
			env.Disabled[sc.Arg] = sc.Body
		}
	}
	if !env.Installed {
		return env, errNotInstalled(EngineCaddy)
	}
	env.Imports = caddyImportRe.MatchString(env.Caddyfile)
	env.Files = []ngxFile{{Path: env.Main, Content: env.Caddyfile}}
	// Follow file imports (one level of globbing, like caddy does).
	_, imports, snippets := cdySites(cdyParse(env.Caddyfile))
	var globs []string
	for _, imp := range imports {
		if snippets[imp] || !globSafe.MatchString(imp) || strings.Contains(imp, "..") {
			continue
		}
		if !strings.HasPrefix(imp, "/") {
			imp = path.Join(path.Dir(env.Main), imp)
		}
		globs = append(globs, imp)
	}
	if len(globs) > 0 {
		m2 := nonce()
		// Globs are whitelisted above (no quotes/spaces/$), so they can be
		// left unquoted for the shell to expand.
		script := "for f in " + strings.Join(globs, " ") + "; do [ -f \"$f\" ] && { echo \"" + m2 + " file $f\"; cat \"$f\"; }; done; echo \"" + m2 + " end\""
		r, err := s.sudo(ctx, conn, pw, script, "")
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{env.Main: true}
		for _, sc := range splitSections(r.Stdout, m2) {
			if sc.Name == "file" && !seen[sc.Arg] {
				seen[sc.Arg] = true
				env.Files = append(env.Files, ngxFile{Path: sc.Arg, Content: sc.Body})
			}
		}
	}
	return env, nil
}

func (e *caddyEnv) sites() []Site {
	out := []Site{}
	add := func(file, content string, enabled bool) {
		nodes := cdyParse(content)
		sites, _, _ := cdySites(nodes)
		if len(sites) == 0 {
			return
		}
		var blocks []ServerBlock
		for _, n := range sites {
			blocks = append(blocks, summarizeCaddySite(n))
		}
		st := buildSite(EngineCaddy, file, blocks, nil)
		st.Enabled = enabled
		st.CanToggle = path.Dir(file) == e.SitesDir
		st.Managed, st.Spec = parseMeta(content)
		if st.Spec != nil {
			st.Name = st.Spec.Domains[0]
		}
		for _, b := range blocks {
			if b.SSLCert != "" {
				st.Certs = appendUnique(st.Certs, b.SSLCert)
			}
		}
		out = append(out, st)
	}
	for _, f := range e.Files {
		add(f.Path, f.Content, true)
	}
	dis := make([]string, 0, len(e.Disabled))
	for f := range e.Disabled {
		dis = append(dis, f)
	}
	sort.Strings(dis)
	for _, f := range dis {
		add(f, e.Disabled[f], false)
	}
	return out
}

func (e *caddyEnv) fileContent(file string) (string, bool) {
	for _, f := range e.Files {
		if f.Path == file {
			return f.Content, true
		}
	}
	c, ok := e.Disabled[file]
	return c, ok
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// SitesResult is the Domains tab content.
type SitesResult struct {
	Sites       []Site   `json:"sites"`
	NginxLayout string   `json:"nginxLayout"` // debian | confd | ""
	NginxTestOK bool     `json:"nginxTestOk"`
	NginxTest   string   `json:"nginxTest"`
	Errors      []string `json:"errors"` // per-engine read problems (codes)
}

// Sites lists the virtual hosts of nginx and caddy.
func (s *WebService) Sites(connID, sudoPassword string) (SitesResult, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return SitesResult{}, err
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	return s.sites(ctx, conn, sudoPassword)
}

func (s *WebService) sites(ctx context.Context, conn *sshx.Conn, pw string) (SitesResult, error) {
	out := SitesResult{Sites: []Site{}, Errors: []string{}}
	ng, err := s.nginxEnv(ctx, conn, pw)
	switch {
	case err == nil:
		out.Sites = append(out.Sites, ng.sites()...)
		out.NginxTestOK, out.NginxTest = ng.TestOK, ng.TestOutput
		if ng.Available != "" {
			out.NginxLayout = "debian"
		} else if ng.ConfDir != "" {
			out.NginxLayout = "confd"
		}
	case apperr.HasCode(err, "web.notInstalled"):
	default:
		return out, err
	}
	cd, err := s.caddyEnv(ctx, conn, pw)
	switch {
	case err == nil:
		out.Sites = append(out.Sites, cd.sites()...)
	case apperr.HasCode(err, "web.notInstalled"):
	default:
		return out, err
	}
	return out, nil
}

// ReadSite returns the raw content of a site's configuration file.
func (s *WebService) ReadSite(connID, engine, file, sudoPassword string) (string, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	c, _, err := s.knownFile(ctx, conn, sudoPassword, engine, file)
	return c, err
}

// knownFile returns the content of a configuration file the engine loads
// (or a disabled site file). Anything else is refused, so the raw editor
// can't be used to write arbitrary files as root.
func (s *WebService) knownFile(ctx context.Context, conn *sshx.Conn, pw, engine, file string) (string, any, error) {
	if err := validAbsPath(file); err != nil {
		return "", nil, err
	}
	switch engine {
	case EngineNginx:
		env, err := s.nginxEnv(ctx, conn, pw)
		if err != nil {
			return "", nil, err
		}
		c, ok := env.fileContent(file)
		if !ok {
			return "", nil, apperr.New("web.siteNotFound", "file", file)
		}
		return c, env, nil
	case EngineCaddy:
		env, err := s.caddyEnv(ctx, conn, pw)
		if err != nil {
			return "", nil, err
		}
		c, ok := env.fileContent(file)
		if !ok {
			return "", nil, apperr.New("web.siteNotFound", "file", file)
		}
		return c, env, nil
	}
	return "", nil, invalid("engine", engine)
}
