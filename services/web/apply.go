package web

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// GenContext holds the server facts the generator needs; the form loads
// it once so previews are computed locally without a round trip.
type GenContext struct {
	Engine             string   `json:"engine"`
	HTTP2Directive     bool     `json:"http2Directive"`
	IPv6               bool     `json:"ipv6"`
	ConfDir            string   `json:"confDir"`
	HtpasswdDir        string   `json:"htpasswdDir"`
	LogDir             string   `json:"logDir"`
	BasicAuthDirective string   `json:"basicAuthDirective"`
	Layout             string   `json:"layout"`
	Version            string   `json:"version"`
	LECerts            []string `json:"leCerts"` // existing Let's Encrypt lineages
	CustomCerts        []string `json:"customCerts"`
	Email              string   `json:"email"` // last Let's Encrypt e-mail used
	Certbot            bool     `json:"certbot"`
}

const customCertDir = "/etc/ssl/server-manager"

// FormContext gathers what the site form needs for an engine.
func (s *WebService) FormContext(connID, engine, sudoPassword string) (GenContext, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return GenContext{}, err
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	return s.formContext(ctx, conn, connID, engine, sudoPassword)
}

func (s *WebService) formContext(ctx context.Context, conn *sshx.Conn, connID, engine, pw string) (GenContext, error) {
	gc := GenContext{Engine: engine, LECerts: []string{}, CustomCerts: []string{}}
	switch engine {
	case EngineNginx:
		env, err := s.nginxEnv(ctx, conn, pw)
		if err != nil {
			return gc, err
		}
		gc.HTTP2Directive, gc.IPv6, gc.ConfDir, gc.Version = env.HTTP2Directive, env.IPv6, env.ConfDir, env.Version
		gc.HtpasswdDir = path.Join(env.Prefix, "htpasswd")
		gc.LogDir = "/var/log/nginx"
		if env.Available != "" {
			gc.Layout = "debian"
		} else if env.ConfDir != "" {
			gc.Layout = "confd"
		}
	case EngineCaddy:
		env, err := s.caddyEnv(ctx, conn, pw)
		if err != nil {
			return gc, err
		}
		gc.Version, gc.LogDir, gc.Layout = env.Version, "/var/log/caddy", "caddyfile"
		gc.BasicAuthDirective = "basicauth"
		if versionAtLeast(env.Version, 2, 8) {
			gc.BasicAuthDirective = "basic_auth"
		}
	default:
		return gc, invalid("engine", engine)
	}
	m := nonce()
	res, err := s.sudo(ctx, conn, pw, fmt.Sprintf(`echo "%[1]s le"; for d in /etc/letsencrypt/live/*/; do [ -s "${d}fullchain.pem" ] && basename "$d"; done; echo "%[1]s custom"; for d in %[2]s/*/; do [ -s "${d}fullchain.pem" ] && basename "$d"; done; echo "%[1]s certbot"; command -v certbot`, m, customCertDir), "")
	if err != nil {
		return gc, err
	}
	sec := sectionMap(splitSections(res.Stdout, m))
	for _, n := range nonEmptyLines(sec["le"]) {
		if certNameRe.MatchString(n) {
			gc.LECerts = append(gc.LECerts, n)
		}
	}
	for _, n := range nonEmptyLines(sec["custom"]) {
		if certNameRe.MatchString(n) {
			gc.CustomCerts = append(gc.CustomCerts, n)
		}
	}
	gc.Certbot = strings.TrimSpace(sec["certbot"]) != ""
	var email string
	if s.core.DB.Get("web.email", connID, &email) == nil {
		gc.Email = email
	}
	return gc, nil
}

// nameFromFile derives the site name (file stem) from a site file path.
func nameFromFile(file string) string {
	b := path.Base(file)
	for _, suf := range []string{".disabled", ".conf", ".caddy"} {
		b = strings.TrimSuffix(b, suf)
	}
	return b
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Preview renders the configuration for a spec (pure; no server access).
// id is the file of the site being edited ("" for a new site).
func (s *WebService) Preview(gc GenContext, id string, spec SiteSpec) (Preview, error) {
	spec.Engine = gc.Engine
	if err := spec.Normalize(); err != nil {
		return Preview{}, err
	}
	name := siteName(spec.Domains[0])
	if id != "" {
		name = nameFromFile(id)
	}
	return render(gc, name, spec, nil), nil
}

func render(gc GenContext, name string, spec SiteSpec, hashes map[string]string) Preview {
	var out genOutput
	file := ""
	switch spec.Engine {
	case EngineCaddy:
		out = genCaddy(spec, cdyGenOpts{Name: name, AuthHashes: hashes, BasicAuthV: gc.BasicAuthDirective, LogDir: gc.LogDir})
		file = "/etc/caddy/sites/" + name + ".caddy"
	default:
		out = genNginx(spec, ngxGenOpts{
			Name: name, HasCert: spec.SSL != SSLLetsEncrypt || contains(gc.LECerts, spec.CertName),
			HTTP2Directive: gc.HTTP2Directive, IPv6: gc.IPv6, ConfDir: gc.ConfDir,
			Htpasswd: path.Join(gc.HtpasswdDir, name), LogDir: gc.LogDir,
		})
	}
	extras := out.Extras
	if extras == nil {
		extras = []PreviewFile{}
	}
	return Preview{File: file, Content: out.Content, Extras: extras, PendingCert: out.PendingCert}
}

// ---- apply ----

// Apply creates (id = "") or updates a managed site, validates it with
// nginx -t / caddy validate (rolling back on failure) and reloads.
func (s *WebService) Apply(connID, id string, spec SiteSpec, sudoPassword string) (ApplyResult, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return ApplyResult{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ApplyResult{}, err
	}
	ctx, cancel := core.Timeout(3 * time.Minute)
	defer cancel()
	res, err := s.apply(ctx, conn, connID, id, spec, sudoPassword)
	action := "web.site.update"
	if id == "" {
		action = "web.site.create"
	}
	target := strings.Join(spec.Domains, ", ")
	if len(spec.Domains) > 0 {
		target = spec.Domains[0]
	}
	s.core.Audit(connID, action, target, specSummary(spec, res.File), err)
	return res, err
}

// specSummary describes a site for the audit log (never secrets).
func specSummary(sp SiteSpec, file string) string {
	parts := []string{sp.Engine, sp.Type}
	switch sp.Type {
	case TypeProxy:
		parts = append(parts, "→ "+strings.Join(sp.Upstreams, ", "))
	case TypeStatic:
		parts = append(parts, "root "+sp.Root)
	case TypeRedirect:
		parts = append(parts, fmt.Sprintf("→ %s (%d)", sp.RedirectTo, sp.RedirectCode))
	}
	parts = append(parts, "domains "+strings.Join(sp.Domains, " "), "ssl "+sp.SSL)
	var flags []string
	for _, f := range []struct {
		on bool
		n  string
	}{{sp.ForceHTTPS, "force-https"}, {sp.HTTP2, "http2"}, {sp.HSTS, "hsts"}, {sp.WebSocket, "websocket"}, {sp.Gzip, "gzip"}, {sp.RateLimit, fmt.Sprintf("ratelimit %dr/s", sp.RateRPS)}, {sp.BasicAuth, fmt.Sprintf("basic-auth %d user(s)", len(sp.AuthUsers))}, {sp.Extra != "", "extra directives"}} {
		if f.on {
			flags = append(flags, f.n)
		}
	}
	if len(flags) > 0 {
		parts = append(parts, strings.Join(flags, ", "))
	}
	if file != "" {
		parts = append(parts, "file "+file)
	}
	return strings.Join(parts, "; ")
}

func (s *WebService) apply(ctx context.Context, conn *sshx.Conn, connID, id string, spec SiteSpec, pw string) (ApplyResult, error) {
	if id != "" {
		if err := validAbsPath(id); err != nil {
			return ApplyResult{}, err
		}
	}
	if spec.Engine == "" {
		spec.Engine = EngineNginx
	}
	if err := spec.Normalize(); err != nil {
		return ApplyResult{}, err
	}
	if spec.SSL == SSLLetsEncrypt && spec.Email != "" {
		_ = s.core.DB.Put("web.email", connID, spec.Email)
	}
	switch spec.Engine {
	case EngineCaddy:
		return s.applyCaddy(ctx, conn, connID, id, spec, pw)
	default:
		return s.applyNginx(ctx, conn, connID, id, spec, pw)
	}
}

func (s *WebService) applyNginx(ctx context.Context, conn *sshx.Conn, connID, id string, spec SiteSpec, pw string) (ApplyResult, error) {
	env, err := s.nginxEnv(ctx, conn, pw)
	if err != nil {
		return ApplyResult{}, err
	}
	gc, err := s.formContext(ctx, conn, connID, EngineNginx, pw)
	if err != nil {
		return ApplyResult{}, err
	}
	var file, old string
	var oldSpec *SiteSpec
	isNew := id == ""
	if isNew {
		if file, err = env.siteFile(siteName(spec.Domains[0])); err != nil {
			return ApplyResult{}, err
		}
		if _, exists := env.fileContent(file); exists {
			return ApplyResult{}, apperr.New("web.siteExists").WithDetail(file)
		}
	} else {
		file = id
		c, ok := env.fileContent(file)
		if !ok {
			return ApplyResult{}, apperr.New("web.siteNotFound", "file", file)
		}
		managed, sp := parseMeta(c)
		if !managed {
			return ApplyResult{}, apperr.New("web.notManaged", "file", file)
		}
		old, oldSpec = c, sp
	}
	if err := s.checkDomainConflicts(env.sites(), nil, file, spec.Domains); err != nil {
		return ApplyResult{}, err
	}
	name := nameFromFile(file)
	res := ApplyResult{ID: file, File: file, Warnings: []string{}}

	// Basic auth: bcrypt new passwords, keep hashes of unchanged users.
	htpasswd := path.Join(gc.HtpasswdDir, name)
	var htContent string
	if spec.BasicAuth {
		existing, err := s.readHtpasswd(ctx, conn, pw, htpasswd)
		if err != nil {
			return res, err
		}
		htContent, err = buildHtpasswd(spec.AuthUsers, existing)
		if err != nil {
			return res, err
		}
	}
	pv := render(gc, name, spec, nil)
	res.PendingCert = pv.PendingCert

	pre := []string{
		fmt.Sprintf("mkdir -p %s/.well-known/acme-challenge && chmod 755 %s", acmeWebroot, acmeWebroot),
	}
	var ops []txOp
	for _, x := range pv.Extras {
		ops = append(ops, txOp{Kind: "write", Path: x.Path, Content: x.Content, Mode: "0644"})
	}
	if spec.BasicAuth {
		grp := env.Group
		pre = append(pre, fmt.Sprintf("mkdir -p %s && chmod 750 %s", core.Q(gc.HtpasswdDir), core.Q(gc.HtpasswdDir)))
		if grp != "" && groupRe.MatchString(grp) {
			pre = append(pre, fmt.Sprintf("chgrp %s %s", core.Q(grp), core.Q(gc.HtpasswdDir)))
			ops = append(ops, txOp{Kind: "write", Path: htpasswd, Content: htContent, Mode: "0640", Group: grp})
		} else {
			pre[len(pre)-1] = fmt.Sprintf("mkdir -p %s && chmod 755 %s", core.Q(gc.HtpasswdDir), core.Q(gc.HtpasswdDir))
			ops = append(ops, txOp{Kind: "write", Path: htpasswd, Content: htContent, Mode: "0644"})
		}
	}
	ops = append(ops, txOp{Kind: "write", Path: file, Content: pv.Content, Mode: "0644", MustNotExist: isNew})
	if isNew {
		if link := env.linkFor(file); link != "" {
			ops = append(ops, txOp{Kind: "link", Path: link, Target: file, MustNotExist: true})
		}
	}
	// Remove helper files the new version no longer uses.
	if oldSpec != nil {
		if oldSpec.BasicAuth && !spec.BasicAuth {
			ops = append(ops, txOp{Kind: "delete", Path: htpasswd})
		}
		if oldSpec.RateLimit && !spec.RateLimit && gc.ConfDir != "" {
			ops = append(ops, txOp{Kind: "delete", Path: rateLimitFile(gc.ConfDir, name)})
		}
	}
	tr, err := s.transact(ctx, conn, pw, env.Prefix, pre, ops, "nginx -t")
	res.TestOutput = tr.TestOutput
	if err != nil {
		return res, err
	}
	if old != "" {
		s.pushHistory(connID, file, "update", old)
	}
	res.Reloaded, res.NotRunning, err = s.reloadNginx(ctx, conn, pw)
	return res, err
}

// checkDomainConflicts refuses a domain another enabled site already serves.
func (s *WebService) checkDomainConflicts(sites []Site, _ any, file string, domains []string) error {
	for _, st := range sites {
		if st.File == file || !st.Enabled {
			continue
		}
		for _, d := range st.Domains {
			if contains(domains, strings.ToLower(d)) {
				return apperr.New("web.domainInUse", "domain", d, "file", st.File)
			}
		}
	}
	return nil
}

func (s *WebService) readHtpasswd(ctx context.Context, conn *sshx.Conn, pw, file string) (map[string]string, error) {
	res, err := s.sudo(ctx, conn, pw, "cat "+core.Q(file)+" 2>/dev/null || true", "")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range nonEmptyLines(res.Stdout) {
		if u, h, ok := strings.Cut(l, ":"); ok {
			m[u] = h
		}
	}
	return m, nil
}

// buildHtpasswd hashes new passwords with bcrypt (cost 12) and keeps the
// existing hash of users whose password was left empty.
func buildHtpasswd(users []AuthUser, existing map[string]string) (string, error) {
	var b strings.Builder
	for _, u := range users {
		h := existing[u.User]
		if u.Password != "" {
			hb, err := bcrypt.GenerateFromPassword([]byte(u.Password), 12)
			if err != nil {
				return "", apperr.New("web.passwordInvalid", "user", u.User)
			}
			h = string(hb)
		}
		if h == "" {
			return "", apperr.New("web.passwordRequired", "user", u.User)
		}
		b.WriteString(u.User + ":" + h + "\n")
	}
	return b.String(), nil
}

// caddyHashes reads the basic-auth hashes from an existing caddy site file.
func caddyHashes(content string) map[string]string {
	m := map[string]string{}
	for _, dir := range []string{"basicauth", "basic_auth"} {
		for _, n := range cdyFindDeep(cdyParse(content), dir) {
			for _, c := range n.Children {
				if len(c.Tokens) >= 2 {
					m[c.Tokens[0]] = c.Tokens[1]
				}
			}
		}
	}
	return m
}

func (s *WebService) applyCaddy(ctx context.Context, conn *sshx.Conn, connID, id string, spec SiteSpec, pw string) (ApplyResult, error) {
	env, err := s.caddyEnv(ctx, conn, pw)
	if err != nil {
		return ApplyResult{}, err
	}
	gc, err := s.formContext(ctx, conn, connID, EngineCaddy, pw)
	if err != nil {
		return ApplyResult{}, err
	}
	var file, old string
	isNew := id == ""
	if isNew {
		file = path.Join(env.SitesDir, siteName(spec.Domains[0])+".caddy")
		if _, exists := env.fileContent(file); exists {
			return ApplyResult{}, apperr.New("web.siteExists").WithDetail(file)
		}
	} else {
		file = id
		c, ok := env.fileContent(file)
		if !ok {
			return ApplyResult{}, apperr.New("web.siteNotFound", "file", file)
		}
		if managed, _ := parseMeta(c); !managed {
			return ApplyResult{}, apperr.New("web.notManaged", "file", file)
		}
		old = c
	}
	if err := s.checkDomainConflicts(env.sites(), nil, file, spec.Domains); err != nil {
		return ApplyResult{}, err
	}
	name := nameFromFile(file)
	res := ApplyResult{ID: file, File: file, Warnings: []string{}}
	var hashes map[string]string
	if spec.BasicAuth {
		existing := caddyHashes(old)
		ht, err := buildHtpasswd(spec.AuthUsers, existing)
		if err != nil {
			return res, err
		}
		hashes = map[string]string{}
		for _, l := range nonEmptyLines(ht) {
			u, h, _ := strings.Cut(l, ":")
			hashes[u] = h
		}
	}
	pv := render(gc, name, spec, hashes)
	pre := []string{"mkdir -p " + core.Q(env.SitesDir)}
	var ops []txOp
	if !env.Imports {
		// Add the import once, keeping a copy of the original Caddyfile.
		pre = append(pre, fmt.Sprintf("[ -e %[1]s.sm-orig ] || cp -a %[1]s %[1]s.sm-orig", core.Q(env.Main)))
		nc := strings.TrimRight(env.Caddyfile, "\n") + "\n\n# Sites managed by Server Manager\nimport sites/*.caddy\n"
		ops = append(ops, txOp{Kind: "write", Path: env.Main, Content: nc})
		res.Warnings = append(res.Warnings, "caddyImportAdded")
	}
	if spec.SSL == SSLCustom && strings.HasPrefix(spec.CertPath, customCertDir+"/") && env.HasCaddyGr {
		// Caddy runs unprivileged: let its group read this certificate.
		d := path.Dir(spec.CertPath)
		pre = append(pre, fmt.Sprintf("chmod 711 %s && chgrp caddy %s %s %s && chmod 750 %s && chmod 640 %s %s",
			customCertDir, core.Q(d), core.Q(spec.CertPath), core.Q(spec.KeyPath), core.Q(d), core.Q(spec.CertPath), core.Q(spec.KeyPath)))
	}
	w := txOp{Kind: "write", Path: file, Content: pv.Content, Mode: "0644", MustNotExist: isNew}
	if spec.BasicAuth && env.HasCaddyGr {
		w.Mode, w.Group = "0640", "caddy"
	}
	ops = append(ops, w)
	tr, err := s.transact(ctx, conn, pw, "/etc/caddy", pre, ops, caddyValidateCmd)
	res.TestOutput = caddyOutput(tr.TestOutput)
	if err != nil {
		return res, err
	}
	if old != "" {
		s.pushHistory(connID, file, "update", old)
	}
	res.Reloaded, res.NotRunning, err = s.reloadCaddy(ctx, conn, pw)
	return res, err
}

// ---- raw edit, enable/disable, delete ----

// SaveRaw replaces a configuration file with edited text (validated like
// any change). Only files the engine loads or disabled site files qualify.
func (s *WebService) SaveRaw(connID, engine, file, content, sudoPassword string) (ApplyResult, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return ApplyResult{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ApplyResult{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := s.saveRaw(ctx, conn, connID, engine, file, content, sudoPassword, "raw")
	s.core.Audit(connID, "web.site.raw", file, fmt.Sprintf("%s, %d bytes", engine, len(content)), err)
	return res, err
}

func (s *WebService) saveRaw(ctx context.Context, conn *sshx.Conn, connID, engine, file, content, pw, action string) (ApplyResult, error) {
	if len(content) > 1<<20 || strings.ContainsRune(content, 0) {
		return ApplyResult{}, apperr.New("web.contentInvalid")
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	old, envAny, err := s.knownFile(ctx, conn, pw, engine, file)
	if err != nil {
		return ApplyResult{}, err
	}
	res := ApplyResult{ID: file, File: file, Warnings: []string{}}
	ops := []txOp{{Kind: "write", Path: file, Content: content}}
	var base, test string
	switch env := envAny.(type) {
	case *nginxEnv:
		base, test = env.Prefix, "nginx -t"
		// Managed file restored from history: recreate its helper files.
		if _, sp := parseMeta(content); sp != nil && sp.Normalize() == nil {
			gc, err := s.formContext(ctx, conn, connID, EngineNginx, pw)
			if err == nil {
				for _, x := range render(gc, nameFromFile(file), *sp, nil).Extras {
					ops = append([]txOp{{Kind: "write", Path: x.Path, Content: x.Content, Mode: "0644"}}, ops...)
				}
			}
		}
	case *caddyEnv:
		base, test = "/etc/caddy", caddyValidateCmd
	}
	tr, err := s.transact(ctx, conn, pw, base, nil, ops, test)
	res.TestOutput = tr.TestOutput
	if err != nil {
		return res, err
	}
	s.pushHistory(connID, file, action, old)
	if engine == EngineCaddy {
		res.TestOutput = caddyOutput(res.TestOutput)
		res.Reloaded, res.NotRunning, err = s.reloadCaddy(ctx, conn, pw)
	} else {
		res.Reloaded, res.NotRunning, err = s.reloadNginx(ctx, conn, pw)
	}
	return res, err
}

// SetEnabled enables or disables a site: a sites-enabled link (Debian
// layout) or a ".disabled" suffix (conf.d, caddy sites/).
func (s *WebService) SetEnabled(connID, engine, file string, enabled bool, sudoPassword string) (ApplyResult, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return ApplyResult{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ApplyResult{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := s.setEnabled(ctx, conn, engine, file, enabled, sudoPassword)
	action := "web.site.disable"
	if enabled {
		action = "web.site.enable"
	}
	s.core.Audit(connID, action, file, engine, err)
	return res, err
}

func (s *WebService) setEnabled(ctx context.Context, conn *sshx.Conn, engine, file string, enabled bool, pw string) (ApplyResult, error) {
	_, envAny, err := s.knownFile(ctx, conn, pw, engine, file)
	if err != nil {
		return ApplyResult{}, err
	}
	res := ApplyResult{ID: file, File: file, Warnings: []string{}}
	var ops []txOp
	var base, test string
	switch env := envAny.(type) {
	case *nginxEnv:
		base, test = env.Prefix, "nginx -t"
		if !env.canToggle(file) {
			return res, apperr.New("web.cannotToggle", "file", file)
		}
		if link := env.linkFor(file); link != "" {
			links := []string{}
			for l, t := range env.Links {
				if t == file {
					links = append(links, l)
				}
			}
			sort.Strings(links)
			switch {
			case enabled && len(links) == 0:
				ops = append(ops, txOp{Kind: "link", Path: link, Target: file, MustNotExist: true})
			case !enabled:
				for _, l := range links {
					ops = append(ops, txOp{Kind: "delete", Path: l})
				}
			}
		} else {
			ops, res.ID = toggleSuffix(file, ".conf", enabled)
		}
	case *caddyEnv:
		base, test = "/etc/caddy", caddyValidateCmd
		if path.Dir(file) != env.SitesDir {
			return res, apperr.New("web.cannotToggle", "file", file)
		}
		ops, res.ID = toggleSuffix(file, ".caddy", enabled)
	}
	res.File = res.ID
	if len(ops) == 0 {
		return res, nil // already in the requested state
	}
	tr, err := s.transact(ctx, conn, pw, base, nil, ops, test)
	res.TestOutput = tr.TestOutput
	if err != nil {
		return res, err
	}
	if engine == EngineCaddy {
		res.TestOutput = caddyOutput(res.TestOutput)
		res.Reloaded, res.NotRunning, err = s.reloadCaddy(ctx, conn, pw)
	} else {
		res.Reloaded, res.NotRunning, err = s.reloadNginx(ctx, conn, pw)
	}
	return res, err
}

func toggleSuffix(file, ext string, enabled bool) ([]txOp, string) {
	switch {
	case enabled && strings.HasSuffix(file, ext+".disabled"):
		n := strings.TrimSuffix(file, ".disabled")
		return []txOp{{Kind: "rename", Path: file, Target: n}}, n
	case !enabled && strings.HasSuffix(file, ext):
		n := file + ".disabled"
		return []txOp{{Kind: "rename", Path: file, Target: n}}, n
	}
	return nil, file
}

// DeleteSite removes a site file (and its link and helper files).
func (s *WebService) DeleteSite(connID, engine, file, sudoPassword string) (ApplyResult, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return ApplyResult{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ApplyResult{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := s.deleteSite(ctx, conn, connID, engine, file, sudoPassword)
	s.core.Audit(connID, "web.site.delete", file, engine, err)
	return res, err
}

func (s *WebService) deleteSite(ctx context.Context, conn *sshx.Conn, connID, engine, file, pw string) (ApplyResult, error) {
	content, envAny, err := s.knownFile(ctx, conn, pw, engine, file)
	if err != nil {
		return ApplyResult{}, err
	}
	res := ApplyResult{ID: file, File: file, Warnings: []string{}}
	_, spec := parseMeta(content)
	name := nameFromFile(file)
	var ops []txOp
	var base, test string
	switch env := envAny.(type) {
	case *nginxEnv:
		base, test = env.Prefix, "nginx -t"
		if !env.canToggle(file) {
			return res, apperr.New("web.cannotDelete", "file", file)
		}
		for l, t := range env.Links {
			if t == file {
				ops = append(ops, txOp{Kind: "delete", Path: l})
			}
		}
		ops = append(ops, txOp{Kind: "delete", Path: file})
		if spec != nil {
			if spec.RateLimit && env.ConfDir != "" {
				ops = append(ops, txOp{Kind: "delete", Path: rateLimitFile(env.ConfDir, name)})
			}
			if spec.BasicAuth {
				ops = append(ops, txOp{Kind: "delete", Path: path.Join(env.Prefix, "htpasswd", name)})
			}
		}
	case *caddyEnv:
		base, test = "/etc/caddy", caddyValidateCmd
		if path.Dir(file) != env.SitesDir {
			return res, apperr.New("web.cannotDelete", "file", file)
		}
		ops = append(ops, txOp{Kind: "delete", Path: file})
	}
	tr, err := s.transact(ctx, conn, pw, base, nil, ops, test)
	res.TestOutput = tr.TestOutput
	if err != nil {
		return res, err
	}
	s.pushHistory(connID, file, "delete", content)
	if engine == EngineCaddy {
		res.TestOutput = caddyOutput(res.TestOutput)
		res.Reloaded, res.NotRunning, err = s.reloadCaddy(ctx, conn, pw)
	} else {
		res.Reloaded, res.NotRunning, err = s.reloadNginx(ctx, conn, pw)
	}
	return res, err
}

// ---- history (undo) ----

const historyNS = "web.history"
const historyMax = 10

func historyKey(connID, file string) string { return connID + "|" + file }

func (s *WebService) pushHistory(connID, file, action, content string) {
	if content == "" {
		return
	}
	var list []HistoryEntry
	_ = s.core.DB.Get(historyNS, historyKey(connID, file), &list)
	list = append([]HistoryEntry{{TS: time.Now().UnixMilli(), Action: action, Actor: s.core.Actor(), Content: content}}, list...)
	if len(list) > historyMax {
		list = list[:historyMax]
	}
	_ = s.core.DB.Put(historyNS, historyKey(connID, file), list)
}

// History lists the previous versions of a site file, newest first.
func (s *WebService) History(connID, file string) ([]HistoryEntry, error) {
	list := []HistoryEntry{}
	if err := s.core.DB.Get(historyNS, historyKey(connID, file), &list); err != nil {
		return []HistoryEntry{}, nil
	}
	return list, nil
}

// RestoreVersion puts a previous version of a site file back (validated
// and reloaded like any change; the current version joins the history).
func (s *WebService) RestoreVersion(connID, engine, file string, ts int64, sudoPassword string) (ApplyResult, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return ApplyResult{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ApplyResult{}, err
	}
	list, _ := s.History(connID, file)
	var entry *HistoryEntry
	for i := range list {
		if list[i].TS == ts {
			entry = &list[i]
		}
	}
	if entry == nil {
		err := apperr.New("web.versionNotFound")
		s.core.Audit(connID, "web.site.restore", file, "", err)
		return ApplyResult{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := s.saveRaw(ctx, conn, connID, engine, file, entry.Content, sudoPassword, "restore")
	s.core.Audit(connID, "web.site.restore", file, "version "+time.UnixMilli(ts).UTC().Format(time.RFC3339), err)
	return res, err
}
