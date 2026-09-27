package web

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
)

// Every value the user types ends up in a config file or a shell command,
// so it is validated against a strict grammar here before any generator
// sees it. Generators additionally quote/escape values, and the result is
// always checked with `nginx -t` / `caddy validate` before going live.

var (
	labelRe      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	headerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	pathRe       = regexp.MustCompile(`^/[A-Za-z0-9._/@+=,~-]*$`)
	certNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	emailRe      = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}\.[A-Za-z]{2,24}$`)
	indexFileRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	authUserRe   = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
	siteNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,200}$`)
	upPathRe     = regexp.MustCompile(`^/[A-Za-z0-9._~/%@+=,!-]*$`)
)

func invalid(field, value string) error {
	if len(value) > 120 {
		value = value[:120] + "…"
	}
	return apperr.New("web.invalid", "field", field, "value", value)
}

// normalizeHost validates a host name (IDN allowed, converted to punycode)
// and returns its lower-case ASCII form. A leading "*." wildcard is
// accepted when allowWildcard is set.
func normalizeHost(h string, allowWildcard bool) (string, error) {
	orig := h
	h = strings.TrimSpace(h)
	wild := false
	if strings.HasPrefix(h, "*.") {
		if !allowWildcard {
			return "", invalid("domain", orig)
		}
		wild = true
		h = h[2:]
	}
	if h == "" || strings.ContainsAny(h, " \t\r\n\"'`;{}$\\/:*") {
		return "", invalid("domain", orig)
	}
	ascii, ok := toASCIIHost(h)
	if !ok || ascii == "" || len(ascii) > 253 {
		return "", invalid("domain", orig)
	}
	if ip := net.ParseIP(ascii); ip != nil {
		if wild || ip.To4() == nil {
			return "", invalid("domain", orig)
		}
		return ascii, nil
	}
	labels := strings.Split(ascii, ".")
	for _, l := range labels {
		if !labelRe.MatchString(l) {
			return "", invalid("domain", orig)
		}
	}
	// The top-level label of a real name is never all digits.
	if len(labels) > 1 {
		if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil {
			return "", invalid("domain", orig)
		}
	}
	if wild {
		if len(labels) < 2 {
			return "", invalid("domain", orig)
		}
		return "*." + ascii, nil
	}
	return ascii, nil
}

// isPublicName: a name Let's Encrypt could issue for (has a dot, not an IP,
// not a wildcard — wildcards need a DNS challenge).
func isPublicName(h string) bool {
	return strings.Contains(h, ".") && net.ParseIP(h) == nil && !strings.HasPrefix(h, "*.") &&
		!strings.HasSuffix(h, ".local") && !strings.HasSuffix(h, ".localhost") && !strings.HasSuffix(h, ".test") && h != "localhost"
}

// Upstream is a parsed proxy target.
type Upstream struct {
	Scheme string // http | https | unix
	Host   string
	Port   int
	Path   string // http(s) only, may be ""
	Socket string // unix only
}

// HostPort renders host:port (IPv6 bracketed).
func (u Upstream) HostPort() string {
	return net.JoinHostPort(u.Host, strconv.Itoa(u.Port))
}

func parseUpstream(raw string) (Upstream, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 300 || hasCtl(s) || strings.ContainsAny(s, " \"'`;{}$\\") {
		return Upstream{}, invalid("upstream", raw)
	}
	if strings.HasPrefix(s, "unix:") {
		p := strings.TrimPrefix(strings.TrimPrefix(s, "unix:"), "//")
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if err := validAbsPath(p); err != nil {
			return Upstream{}, invalid("upstream", raw)
		}
		return Upstream{Scheme: "unix", Socket: p}, nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return Upstream{}, invalid("upstream", raw)
	}
	host := u.Hostname()
	if host == "" {
		return Upstream{}, invalid("upstream", raw)
	}
	if ip := net.ParseIP(host); ip == nil {
		h, err := normalizeHost(host, false)
		if err != nil {
			return Upstream{}, invalid("upstream", raw)
		}
		host = h
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if ps := u.Port(); ps != "" {
		p, err := strconv.Atoi(ps)
		if err != nil || p < 1 || p > 65535 {
			return Upstream{}, invalid("upstream", raw)
		}
		port = p
	}
	path := u.EscapedPath()
	if path == "/" {
		path = ""
	}
	if path != "" && !upPathRe.MatchString(path) {
		return Upstream{}, invalid("upstream", raw)
	}
	return Upstream{Scheme: u.Scheme, Host: host, Port: port, Path: path}, nil
}

func hasCtl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 || r == 0x85 {
			return true
		}
	}
	return false
}

func validAbsPath(p string) error {
	if len(p) > 1024 || !pathRe.MatchString(p) || strings.Contains(p, "//") {
		return invalid("path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return invalid("path", p)
		}
	}
	return nil
}

func validHeader(h Header) (Header, error) {
	h.Name = strings.TrimSpace(h.Name)
	if !headerNameRe.MatchString(h.Name) {
		return h, invalid("headerName", h.Name)
	}
	if len(h.Value) > 1024 || hasCtl(h.Value) {
		return h, invalid("headerValue", h.Value)
	}
	return h, nil
}

// validRedirect checks an absolute http(s) URL and returns it normalized.
func validRedirect(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 2000 || hasCtl(s) || strings.ContainsAny(s, " \"'`\\${}<>^|") {
		return "", invalid("redirectTo", raw)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || u.Opaque != "" {
		return "", invalid("redirectTo", raw)
	}
	host, err := normalizeHost(u.Hostname(), false)
	if err != nil {
		return "", invalid("redirectTo", raw)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid("redirectTo", raw)
		}
		host = net.JoinHostPort(host, p)
	}
	u.Host = host
	return u.String(), nil
}

func validLogPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "off" {
		return p, nil
	}
	if err := validAbsPath(p); err != nil {
		return "", invalid("logPath", p)
	}
	return p, nil
}

func validCertName(n string) error {
	if !certNameRe.MatchString(n) || strings.Contains(n, "..") {
		return apperr.New("web.invalidCertName", "name", n)
	}
	return nil
}

// siteName is the file name stem for a site: its primary domain in ASCII,
// "*.example.com" becoming "wildcard.example.com".
func siteName(primary string) string {
	if strings.HasPrefix(primary, "*.") {
		return "wildcard." + primary[2:]
	}
	return primary
}

// checkBraces verifies that raw directives are balanced so they cannot close
// the enclosing block and inject top-level configuration.
func checkBraces(raw string, caddy bool) error {
	depth := 0
	inQ := byte(0)
	comment := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == 0 {
			return apperr.New("web.extraInvalid")
		}
		if comment {
			if c == '\n' {
				comment = false
			}
			continue
		}
		if inQ != 0 {
			if c == '\\' && i+1 < len(raw) {
				i++
				continue
			}
			if c == inQ {
				inQ = 0
			}
			continue
		}
		switch c {
		case '#':
			comment = true
		case '"':
			inQ = c
		case '\'':
			if !caddy {
				inQ = c
			}
		case '`':
			if caddy {
				inQ = c
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return apperr.New("web.extraUnbalanced")
			}
		}
	}
	if depth != 0 || inQ != 0 {
		return apperr.New("web.extraUnbalanced")
	}
	return nil
}

func inRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return invalid(field, strconv.Itoa(v))
	}
	return nil
}

// Normalize validates a spec in place, filling defaults and converting
// domains to ASCII. It is pure (no remote access).
func (sp *SiteSpec) Normalize() error {
	switch sp.Engine {
	case "":
		sp.Engine = EngineNginx
	case EngineNginx, EngineCaddy:
	default:
		return invalid("engine", sp.Engine)
	}
	// Domains
	if len(sp.Domains) == 0 {
		return apperr.New("web.domainRequired")
	}
	if len(sp.Domains) > 50 {
		return invalid("domains", strconv.Itoa(len(sp.Domains)))
	}
	seen := map[string]bool{}
	doms := []string{}
	for _, d := range sp.Domains {
		if strings.TrimSpace(d) == "" {
			continue
		}
		n, err := normalizeHost(d, true)
		if err != nil {
			return err
		}
		if !seen[n] {
			seen[n] = true
			doms = append(doms, n)
		}
	}
	if len(doms) == 0 {
		return apperr.New("web.domainRequired")
	}
	sp.Domains = doms

	switch sp.Type {
	case "":
		sp.Type = TypeProxy
	case TypeProxy, TypeStatic, TypeRedirect:
	default:
		return invalid("type", sp.Type)
	}

	// Type-specific
	switch sp.Type {
	case TypeProxy:
		ups := []string{}
		for _, u := range sp.Upstreams {
			if strings.TrimSpace(u) != "" {
				ups = append(ups, strings.TrimSpace(u))
			}
		}
		if len(ups) == 0 {
			return apperr.New("web.upstreamRequired")
		}
		if len(ups) > 32 {
			return invalid("upstreams", strconv.Itoa(len(ups)))
		}
		var first Upstream
		for i, raw := range ups {
			u, err := parseUpstream(raw)
			if err != nil {
				return err
			}
			if sp.Engine == EngineCaddy && u.Path != "" {
				return apperr.New("web.caddyUpstreamPath", "value", raw)
			}
			if i == 0 {
				first = u
			} else {
				if u.Path != "" || first.Path != "" {
					return apperr.New("web.upstreamPathMulti")
				}
				if (u.Scheme == "https") != (first.Scheme == "https") {
					return apperr.New("web.upstreamMixed")
				}
			}
		}
		sp.Upstreams = ups
		switch sp.LBMethod {
		case "", "round_robin":
			sp.LBMethod = ""
		case "least_conn", "ip_hash":
		default:
			return invalid("lbMethod", sp.LBMethod)
		}
		for _, f := range []struct {
			n string
			v int
		}{{"connectTimeout", sp.ConnectTimeout}, {"readTimeout", sp.ReadTimeout}, {"sendTimeout", sp.SendTimeout}} {
			if err := inRange(f.n, f.v, 0, 86400); err != nil {
				return err
			}
		}
		if sp.ConnectTimeout > 75 && sp.Engine == EngineNginx {
			return invalid("connectTimeout", strconv.Itoa(sp.ConnectTimeout)) // nginx caps it at 75s
		}
		hs, err := cleanHeaders(sp.RequestHeaders)
		if err != nil {
			return err
		}
		sp.RequestHeaders = hs
	case TypeStatic:
		sp.Root = strings.TrimRight(strings.TrimSpace(sp.Root), "/")
		if sp.Root == "" {
			return apperr.New("web.rootRequired")
		}
		if err := validAbsPath(sp.Root); err != nil {
			return err
		}
		idx := strings.Fields(sp.Index)
		if len(idx) == 0 {
			idx = []string{"index.html", "index.htm"}
		}
		if len(idx) > 8 {
			return invalid("index", sp.Index)
		}
		for _, f := range idx {
			if !indexFileRe.MatchString(f) {
				return invalid("index", f)
			}
		}
		sp.Index = strings.Join(idx, " ")
	case TypeRedirect:
		u, err := validRedirect(sp.RedirectTo)
		if err != nil {
			return err
		}
		sp.RedirectTo = u
		switch sp.RedirectCode {
		case 0:
			sp.RedirectCode = 301
		case 301, 302, 307, 308:
		default:
			return invalid("redirectCode", strconv.Itoa(sp.RedirectCode))
		}
	}
	if sp.Type != TypeProxy {
		sp.Upstreams, sp.LBMethod, sp.WebSocket, sp.RequestHeaders = []string{}, "", false, []Header{}
		sp.ConnectTimeout, sp.ReadTimeout, sp.SendTimeout = 0, 0, 0
	}
	if sp.Type != TypeStatic {
		sp.Root, sp.Index, sp.SPA = "", "", false
	}
	if sp.Type != TypeRedirect {
		sp.RedirectTo, sp.RedirectCode, sp.PreservePath = "", 0, false
	}

	// TLS
	switch sp.SSL {
	case "":
		sp.SSL = SSLNone
	case SSLNone, SSLLetsEncrypt, SSLCustom:
	case SSLInternal:
		if sp.Engine != EngineCaddy {
			return invalid("ssl", sp.SSL)
		}
	default:
		return invalid("ssl", sp.SSL)
	}
	switch sp.SSL {
	case SSLLetsEncrypt:
		for _, d := range sp.Domains {
			if strings.HasPrefix(d, "*.") {
				return apperr.New("web.leWildcard", "domain", d)
			}
			if !isPublicName(d) {
				return apperr.New("web.leNotPublic", "domain", d)
			}
		}
		sp.Email = strings.TrimSpace(sp.Email)
		if !emailRe.MatchString(sp.Email) && !(sp.Engine == EngineCaddy && sp.Email == "") {
			return apperr.New("web.emailRequired")
		}
		if sp.CertName == "" {
			sp.CertName = siteName(sp.Domains[0])
		}
		if err := validCertName(sp.CertName); err != nil {
			return err
		}
		switch sp.KeyType {
		case "":
			sp.KeyType = "ecdsa"
		case "ecdsa", "rsa":
		default:
			return invalid("keyType", sp.KeyType)
		}
		if sp.Engine == EngineCaddy {
			sp.CertName, sp.KeyType = "", ""
		}
		sp.CertPath, sp.KeyPath = "", ""
	case SSLCustom:
		sp.CertPath = strings.TrimSpace(sp.CertPath)
		sp.KeyPath = strings.TrimSpace(sp.KeyPath)
		if sp.CertPath == "" || sp.KeyPath == "" {
			return apperr.New("web.certRequired")
		}
		if err := validAbsPath(sp.CertPath); err != nil {
			return err
		}
		if err := validAbsPath(sp.KeyPath); err != nil {
			return err
		}
		if sp.CertName != "" {
			if err := validCertName(sp.CertName); err != nil {
				return err
			}
		}
		sp.Email, sp.KeyType, sp.Staging = "", "", false
	default:
		sp.CertName, sp.CertPath, sp.KeyPath, sp.KeyType, sp.Staging = "", "", "", "", false
		if sp.SSL == SSLNone {
			sp.Email = ""
		} else if sp.Email != "" && !emailRe.MatchString(strings.TrimSpace(sp.Email)) {
			return apperr.New("web.emailRequired")
		}
	}
	if sp.SSL == SSLNone {
		sp.ForceHTTPS, sp.HTTP2, sp.HSTS, sp.HSTSSubdomains, sp.HSTSMaxAge = false, false, false, false, 0
	}
	if sp.HSTS {
		if sp.HSTSMaxAge == 0 {
			sp.HSTSMaxAge = 31536000
		}
		if err := inRange("hstsMaxAge", sp.HSTSMaxAge, 60, 126144000); err != nil {
			return err
		}
	} else {
		sp.HSTSMaxAge, sp.HSTSSubdomains = 0, false
	}

	// Misc
	hs, err := cleanHeaders(sp.ResponseHeaders)
	if err != nil {
		return err
	}
	sp.ResponseHeaders = hs
	if err := inRange("maxBodyMb", sp.MaxBodyMB, 0, 102400); err != nil {
		return err
	}
	if sp.RateLimit {
		if sp.Engine == EngineCaddy {
			return apperr.New("web.caddyNoRateLimit")
		}
		if sp.RateRPS == 0 {
			sp.RateRPS = 10
		}
		if err := inRange("rateRps", sp.RateRPS, 1, 100000); err != nil {
			return err
		}
		if err := inRange("rateBurst", sp.RateBurst, 0, 100000); err != nil {
			return err
		}
	} else {
		sp.RateRPS, sp.RateBurst, sp.RateNoDelay = 0, 0, false
	}
	if sp.BasicAuth {
		sp.AuthRealm = strings.TrimSpace(sp.AuthRealm)
		if sp.AuthRealm == "" {
			sp.AuthRealm = "Restricted"
		}
		if len(sp.AuthRealm) > 64 || hasCtl(sp.AuthRealm) || strings.ContainsAny(sp.AuthRealm, "\"\\$`{}") {
			return invalid("authRealm", sp.AuthRealm)
		}
		users := []AuthUser{}
		seenU := map[string]bool{}
		for _, u := range sp.AuthUsers {
			u.User = strings.TrimSpace(u.User)
			if u.User == "" && u.Password == "" {
				continue
			}
			if !authUserRe.MatchString(u.User) || seenU[u.User] {
				return invalid("authUser", u.User)
			}
			if len(u.Password) > 72 || hasCtl(u.Password) {
				return apperr.New("web.passwordInvalid", "user", u.User)
			}
			seenU[u.User] = true
			users = append(users, u)
		}
		if len(users) == 0 {
			return apperr.New("web.authUserRequired")
		}
		sp.AuthUsers = users
	} else {
		sp.AuthRealm, sp.AuthUsers = "", []AuthUser{}
	}
	if sp.AccessLog, err = validLogPath(sp.AccessLog); err != nil {
		return err
	}
	if sp.ErrorLog, err = validLogPath(sp.ErrorLog); err != nil {
		return err
	}
	if len(sp.Extra) > 16384 {
		return apperr.New("web.extraInvalid")
	}
	sp.Extra = strings.TrimSpace(strings.ReplaceAll(sp.Extra, "\r\n", "\n"))
	if sp.Extra != "" {
		for _, r := range sp.Extra {
			if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
				return apperr.New("web.extraInvalid")
			}
		}
		if err := checkBraces(sp.Extra, sp.Engine == EngineCaddy); err != nil {
			return err
		}
		if strings.Contains(sp.Extra, "sm-meta:") || strings.Contains(sp.Extra, "managed-by:") {
			return apperr.New("web.extraInvalid")
		}
	}
	if !siteNameRe.MatchString(siteName(sp.Domains[0])) {
		return invalid("domain", sp.Domains[0])
	}
	return nil
}

func cleanHeaders(in []Header) ([]Header, error) {
	out := []Header{}
	if len(in) > 50 {
		return nil, invalid("headers", strconv.Itoa(len(in)))
	}
	for _, h := range in {
		if strings.TrimSpace(h.Name) == "" && strings.TrimSpace(h.Value) == "" {
			continue
		}
		v, err := validHeader(h)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
