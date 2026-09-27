package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"server-manager/internal/apperr"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update)", err, t.Name())
	}
	if string(want) != got {
		t.Errorf("%s differs from golden file:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return apperr.From(err).Code
}

func TestPunycode(t *testing.T) {
	cases := map[string]string{
		"bücher.example":    "xn--bcher-kva.example",
		"MÜNCHEN.de":        "xn--mnchen-3ya.de",
		"例え.テスト":            "xn--r8jz45g.xn--zckzah",
		"tiếngviệt.vn":      "xn--tingvit-5t4cyc.vn",
		"plain.example.com": "plain.example.com",
	}
	for in, want := range cases {
		got, ok := toASCIIHost(in)
		if !ok || got != want {
			t.Errorf("toASCIIHost(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	good := map[string]string{
		"Example.COM":    "example.com",
		"*.example.com":  "*.example.com",
		"a-b.c-d.io":     "a-b.c-d.io",
		"localhost":      "localhost",
		"10.0.0.1":       "10.0.0.1",
		"bücher.example": "xn--bcher-kva.example",
		"example.com.":   "example.com",
	}
	for in, want := range good {
		got, err := normalizeHost(in, true)
		if err != nil || got != want {
			t.Errorf("normalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "-a.com", "a-.com", "a..com", "a.com;evil", "a.com\nserver", `a"b.com`, "a b.com", "*.*.com", "a.*.com",
		"*", "a.com/x", "a.com:80", "$host", "a.123", "{a}.com", strings.Repeat("a", 64) + ".com", "::1", "*.10.0.0.1"}
	for _, in := range bad {
		if got, err := normalizeHost(in, true); err == nil {
			t.Errorf("normalizeHost(%q) = %q, want error", in, got)
		}
	}
}

func TestParseUpstream(t *testing.T) {
	good := map[string]Upstream{
		"127.0.0.1:3000":            {Scheme: "http", Host: "127.0.0.1", Port: 3000},
		"http://localhost:8080/api": {Scheme: "http", Host: "localhost", Port: 8080, Path: "/api"},
		"https://backend.internal":  {Scheme: "https", Host: "backend.internal", Port: 443},
		"unix:/run/app.sock":        {Scheme: "unix", Socket: "/run/app.sock"},
		"http://[::1]:9000":         {Scheme: "http", Host: "::1", Port: 9000},
	}
	for in, want := range good {
		got, err := parseUpstream(in)
		if err != nil || got != want {
			t.Errorf("parseUpstream(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := []string{"", "ftp://a", "http://a:0", "http://a:70000", "http://u:p@a", "http://a/x?y=1", "unix:relative/../x",
		"http://a;return 200", "http://a b", "http://a/$uri", `http://a/"`, "unix:/run/a sock", "http://a\n:80"}
	for _, in := range bad {
		if got, err := parseUpstream(in); err == nil {
			t.Errorf("parseUpstream(%q) = %+v, want error", in, got)
		}
	}
}

func TestCheckBraces(t *testing.T) {
	ok := []string{"location /x { return 200; }", "add_header X \"}\";", "# } comment\nproxy_buffering off;", "set $a '{';"}
	for _, s := range ok {
		if err := checkBraces(s, false); err != nil {
			t.Errorf("checkBraces(%q) = %v", s, err)
		}
	}
	bad := []string{"}", "} server {", "location / {", "} } server { listen 81; } location / {", `add_header X "a;`}
	for _, s := range bad {
		if err := checkBraces(s, false); err == nil {
			t.Errorf("checkBraces(%q) accepted", s)
		}
	}
}

func baseSpec() SiteSpec {
	return SiteSpec{Engine: EngineNginx, Domains: []string{"app.example.com", "www.app.example.com"}, Type: TypeProxy, Upstreams: []string{"http://127.0.0.1:3000"}}
}

var nginxOpts = ngxGenOpts{Name: "app.example.com", HasCert: true, IPv6: true, ConfDir: "/etc/nginx/conf.d", Htpasswd: "/etc/nginx/htpasswd/app.example.com", LogDir: "/var/log/nginx"}

func gen(t *testing.T, sp SiteSpec, o ngxGenOpts) genOutput {
	t.Helper()
	if err := sp.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if sp.Engine == EngineCaddy {
		return genCaddy(sp, cdyGenOpts{Name: o.Name, AuthHashes: map[string]string{"alice": "$2a$12$abcdefghijklmnopqrstuuMqL6vH4aF6T5Yb0yJ1sZ2u3v4w5x6y7z"}, BasicAuthV: "basicauth", LogDir: "/var/log/caddy"})
	}
	return genNginx(sp, o)
}

func TestGenNginxGolden(t *testing.T) {
	proxy := baseSpec()

	static := SiteSpec{Domains: []string{"static.example.com"}, Type: TypeStatic, Root: "/var/www/static/", SPA: true, Gzip: true}

	redirect := SiteSpec{Domains: []string{"old.example.com"}, Type: TypeRedirect, RedirectTo: "https://new.example.com/", RedirectCode: 308, PreservePath: true}

	lePending := baseSpec()
	lePending.SSL, lePending.Email, lePending.ForceHTTPS = SSLLetsEncrypt, "ops@example.com", true

	full := baseSpec()
	full.Upstreams = []string{"http://10.0.0.1:8080", "http://10.0.0.2:8080", "unix:/run/app.sock"}
	full.LBMethod = "least_conn"
	full.WebSocket = true
	full.SSL, full.Email, full.ForceHTTPS, full.HTTP2 = SSLLetsEncrypt, "ops@example.com", true, true
	full.HSTS, full.HSTSSubdomains = true, true
	full.RequestHeaders = []Header{{Name: "X-App", Value: `say "hi"`}, {Name: "Host", Value: "internal.example"}}
	full.ResponseHeaders = []Header{{Name: "X-Frame-Options", Value: "DENY"}}
	full.MaxBodyMB, full.ConnectTimeout, full.ReadTimeout = 50, 5, 120
	full.Gzip = true
	full.RateLimit, full.RateRPS, full.RateBurst, full.RateNoDelay = true, 20, 40, true
	full.BasicAuth, full.AuthUsers = true, []AuthUser{{User: "alice", Password: "secret"}}
	full.AccessLog, full.ErrorLog = "off", "/var/log/nginx/custom-error.log"
	full.Extra = "location /healthz {\n    return 200 'ok';\n}"

	custom := baseSpec()
	custom.Domains = []string{"*.example.org"}
	custom.SSL, custom.CertPath, custom.KeyPath, custom.HTTP2 = SSLCustom, "/etc/ssl/server-manager/wild/fullchain.pem", "/etc/ssl/server-manager/wild/privkey.pem", true
	o125 := nginxOpts
	o125.HTTP2Directive, o125.IPv6, o125.ConfDir = true, false, ""
	o125.Name = "wildcard.example.org"

	cases := []struct {
		name string
		sp   SiteSpec
		o    ngxGenOpts
	}{
		{"nginx_proxy", proxy, nginxOpts},
		{"nginx_static_spa", static, ngxGenOpts{Name: "static.example.com", LogDir: "/var/log/nginx"}},
		{"nginx_redirect", redirect, ngxGenOpts{Name: "old.example.com", LogDir: "/var/log/nginx", IPv6: true}},
		{"nginx_le_pending", lePending, func() ngxGenOpts { o := nginxOpts; o.HasCert = false; return o }()},
		{"nginx_full", full, nginxOpts},
		{"nginx_custom_http2on", custom, o125},
	}
	for _, c := range cases {
		out := gen(t, c.sp, c.o)
		var b strings.Builder
		b.WriteString(out.Content)
		for _, x := range out.Extras {
			b.WriteString("\n### " + x.Path + "\n" + x.Content)
		}
		golden(t, c.name, b.String())
		if c.name == "nginx_le_pending" && !out.PendingCert {
			t.Error("expected pendingCert")
		}
	}
}

func TestGenCaddyGolden(t *testing.T) {
	proxy := baseSpec()
	proxy.Engine = EngineCaddy
	proxy.SSL, proxy.Email = SSLLetsEncrypt, "ops@example.com"
	proxy.Upstreams = []string{"127.0.0.1:3000", "127.0.0.1:3001"}
	proxy.LBMethod = "ip_hash"
	proxy.HSTS = true
	proxy.Gzip = true
	proxy.RequestHeaders = []Header{{Name: "X-Real-IP", Value: "{remote_host}"}}
	proxy.ReadTimeout = 60
	proxy.BasicAuth, proxy.AuthUsers = true, []AuthUser{{User: "alice"}}
	proxy.MaxBodyMB = 10

	static := SiteSpec{Engine: EngineCaddy, Domains: []string{"static.example.com"}, Type: TypeStatic, Root: "/srv/www", SPA: true, SSL: SSLInternal}
	redirect := SiteSpec{Engine: EngineCaddy, Domains: []string{"old.example.com"}, Type: TypeRedirect, RedirectTo: "https://new.example.com", PreservePath: true, AccessLog: "off"}

	for name, sp := range map[string]SiteSpec{"caddy_proxy": proxy, "caddy_static": static, "caddy_redirect": redirect} {
		out := gen(t, sp, ngxGenOpts{Name: sp.Domains[0]})
		golden(t, name, out.Content)
	}
}

// Every directive of a generated nginx config must come from the
// generator's vocabulary: user values must never create directives.
var allowedDirectives = map[string]bool{
	"server": true, "listen": true, "server_name": true, "location": true, "root": true, "default_type": true, "try_files": true,
	"return": true, "access_log": true, "error_log": true, "client_max_body_size": true, "gzip": true, "gzip_vary": true,
	"gzip_proxied": true, "gzip_comp_level": true, "gzip_min_length": true, "gzip_types": true, "auth_basic": true,
	"auth_basic_user_file": true, "limit_req": true, "limit_req_status": true, "add_header": true, "proxy_pass": true,
	"proxy_http_version": true, "proxy_set_header": true, "proxy_ssl_server_name": true, "proxy_connect_timeout": true,
	"proxy_read_timeout": true, "proxy_send_timeout": true, "index": true, "upstream": true, "least_conn": true, "ip_hash": true,
	"keepalive": true, "ssl_certificate": true, "ssl_certificate_key": true, "ssl_protocols": true, "ssl_session_timeout": true,
	"http2": true, "limit_req_zone": true,
}

func directiveNames(list []*ngxDirective, out map[string]int) {
	for _, d := range list {
		out[d.Name]++
		directiveNames(d.Block, out)
	}
}

func TestNginxInjection(t *testing.T) {
	attacks := []string{
		`"; return 200 "pwned`,
		`x"; } server { listen 8081; } #`,
		`a\"; include /etc/passwd; #`,
		`\`,
		`' ; deny all; '`,
		"${host}; allow all",
	}
	for _, a := range attacks {
		sp := baseSpec()
		sp.RequestHeaders = []Header{{Name: "X-A", Value: a}}
		sp.ResponseHeaders = []Header{{Name: "X-B", Value: a}}
		sp.BasicAuth, sp.AuthRealm, sp.AuthUsers = true, "", []AuthUser{{User: "bob", Password: a}}
		out := gen(t, sp, nginxOpts)
		names := map[string]int{}
		directiveNames(ngxParse(out.Content), names)
		for n := range names {
			if !allowedDirectives[n] {
				t.Errorf("value %q produced directive %q:\n%s", a, n, out.Content)
			}
		}
		if names["server"] != 1 || names["return"] != 0 {
			t.Errorf("value %q changed the structure: %v", a, names)
		}
		// The value must round-trip through nginx's tokenizer unchanged.
		found := false
		for _, d := range ngxFindDeep(ngxParse(out.Content), "add_header") {
			if d.arg(0) == "X-B" && d.arg(1) == a {
				found = true
			}
		}
		if !found {
			t.Errorf("value %q not preserved", a)
		}
		if strings.Contains(out.Content, "\n"+a) {
			t.Errorf("raw value leaked onto its own line")
		}
	}
	// Values/fields with newlines or bad characters are rejected outright.
	rejects := []func(*SiteSpec){
		func(s *SiteSpec) { s.RequestHeaders = []Header{{Name: "X", Value: "a\nreturn 200;"}} },
		func(s *SiteSpec) { s.ResponseHeaders = []Header{{Name: "X-Y;", Value: "a"}} },
		func(s *SiteSpec) { s.ResponseHeaders = []Header{{Name: "X Y", Value: "a"}} },
		func(s *SiteSpec) { s.Domains = []string{"a.com; return 200"} },
		func(s *SiteSpec) { s.Upstreams = []string{"http://127.0.0.1:3000; return 200"} },
		func(s *SiteSpec) { s.Type, s.Root = TypeStatic, "/var/www; return 200" },
		func(s *SiteSpec) { s.Type, s.Root = TypeStatic, "/var/www/../../etc" },
		func(s *SiteSpec) { s.Type, s.RedirectTo = TypeRedirect, "https://a.com/\"; return 200" },
		func(s *SiteSpec) { s.Type, s.RedirectTo = TypeRedirect, "https://a.com/$host" },
		func(s *SiteSpec) { s.Type, s.RedirectTo = TypeRedirect, "javascript:alert(1)" },
		func(s *SiteSpec) { s.AccessLog = "/var/log/x.log; return 200" },
		func(s *SiteSpec) { s.Extra = "} server { listen 9999; " },
		func(s *SiteSpec) { s.Extra = "}" },
		func(s *SiteSpec) { s.Extra = "# sm-meta: {}" },
		func(s *SiteSpec) { s.BasicAuth, s.AuthUsers = true, []AuthUser{{User: "a:b", Password: "x"}} },
		func(s *SiteSpec) {
			s.BasicAuth, s.AuthRealm, s.AuthUsers = true, "a\"b", []AuthUser{{User: "a", Password: "x"}}
		},
		func(s *SiteSpec) { s.SSL, s.CertPath, s.KeyPath = SSLCustom, "/etc/ssl/a\n.pem", "/k" },
		func(s *SiteSpec) { s.SSL, s.Email = SSLLetsEncrypt, "not-an-email" },
		func(s *SiteSpec) { s.SSL, s.Email, s.Domains = SSLLetsEncrypt, "a@b.co", []string{"*.example.com"} },
		func(s *SiteSpec) { s.SSL, s.Email, s.Domains = SSLLetsEncrypt, "a@b.co", []string{"localhost"} },
		func(s *SiteSpec) { s.SSL, s.Email, s.CertName = SSLLetsEncrypt, "a@b.co", "../x" },
		func(s *SiteSpec) { s.MaxBodyMB = -1 },
		func(s *SiteSpec) { s.ConnectTimeout = 100 },
		func(s *SiteSpec) { s.LBMethod = "random; return 200" },
		func(s *SiteSpec) { s.Upstreams = []string{"http://a:1/x", "http://b:1"} },
		func(s *SiteSpec) { s.Upstreams = []string{"http://a:1", "https://b:1"} },
		func(s *SiteSpec) { s.Engine = "apache" },
		func(s *SiteSpec) { s.Domains = nil },
	}
	for i, f := range rejects {
		sp := baseSpec()
		f(&sp)
		if err := sp.Normalize(); err == nil {
			t.Errorf("case %d accepted: %+v", i, sp)
		}
	}
}

func TestCaddyInjection(t *testing.T) {
	sp := baseSpec()
	sp.Engine = EngineCaddy
	sp.ResponseHeaders = []Header{{Name: "X-B", Value: "a\" }\nrespond 200 {"}}
	if err := sp.Normalize(); err == nil {
		t.Fatal("newline accepted")
	}
	sp.ResponseHeaders = []Header{{Name: "X-B", Value: `a" } respond "pwned`}}
	out := gen(t, sp, ngxGenOpts{Name: "app.example.com"})
	for _, n := range cdyFindDeep(cdyParse(out.Content), "respond") {
		t.Errorf("injected directive: %v", n.Tokens)
	}
	sites, _, _ := cdySites(cdyParse(out.Content))
	if len(sites) != 1 {
		t.Fatalf("sites = %d", len(sites))
	}
	found := false
	for _, h := range cdyFindDeep(sites[0].Children, "header") {
		for _, c := range h.Children {
			if len(c.Tokens) == 2 && c.Tokens[0] == "X-B" && c.Tokens[1] == `a" } respond "pwned` {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("header value not preserved:\n%s", out.Content)
	}
	sp.RateLimit = true
	if code(sp.Normalize()) != "web.caddyNoRateLimit" {
		t.Error("caddy rate limit accepted")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	sp := baseSpec()
	sp.BasicAuth, sp.AuthUsers = true, []AuthUser{{User: "alice", Password: "topsecret"}}
	sp.Extra = "proxy_buffering off;\n# multi\nclient_body_timeout 30s;"
	out := gen(t, sp, nginxOpts)
	if strings.Contains(out.Content, "topsecret") {
		t.Fatal("password written to the config")
	}
	managed, got := parseMeta(out.Content)
	if !managed || got == nil {
		t.Fatal("meta not found")
	}
	want := sp
	_ = want.Normalize()
	want.AuthUsers = []AuthUser{{User: "alice"}}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", *got, want)
	}
	// Generating again from the parsed spec gives the same file.
	again := genNginx(*got, nginxOpts)
	if again.Content != out.Content {
		t.Error("regeneration differs")
	}
	if m, _ := parseMeta("server { }"); m {
		t.Error("unmanaged file detected as managed")
	}
}

const sampleDump = `nginx: the configuration file /etc/nginx/nginx.conf syntax is ok
# configuration file /etc/nginx/nginx.conf:
user www-data;
worker_processes auto;
events { worker_connections 768; }
http {
	include /etc/nginx/mime.types;
	upstream backend { server 10.0.0.1:80; server 10.0.0.2:80 backup; }
	include /etc/nginx/conf.d/*.conf;
	include /etc/nginx/sites-enabled/*;
}
stream {
	server { listen 5432; proxy_pass db:5432; }
}

# configuration file /etc/nginx/mime.types:
types { text/html html; }

# configuration file /etc/nginx/conf.d/sm-ratelimit-x.conf:
limit_req_zone $binary_remote_addr zone=sm_rl_x:10m rate=5r/s;

# configuration file /etc/nginx/sites-enabled/default:
# Default server
server {
	listen 80 default_server;
	listen [::]:80 default_server;
	root /var/www/html;
	server_name _;
	location / { try_files $uri $uri/ =404; }
}

# configuration file /etc/nginx/sites-enabled/shop.conf:
server {
	listen 443 ssl http2;
	server_name shop.example.com "www.shop.example.com"; # trailing comment
	ssl_certificate /etc/letsencrypt/live/shop.example.com/fullchain.pem;
	ssl_certificate_key /etc/letsencrypt/live/shop.example.com/privkey.pem;
	add_header X-Test "brace } in ; string" always;
	location / { proxy_pass http://backend; set $x "${host}{"; }
}
server {
	listen 80;
	server_name shop.example.com www.shop.example.com;
	return 301 https://$host$request_uri;
}
`

func TestAnalyzeNginxDump(t *testing.T) {
	files := splitNginxDump(sampleDump)
	if len(files) != 5 {
		t.Fatalf("files = %d", len(files))
	}
	cfg := analyzeNginx(files)
	if cfg.User != "www-data" {
		t.Errorf("user = %q", cfg.User)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("http servers = %d (stream server must be ignored)", len(cfg.Servers))
	}
	env := &nginxEnv{Cfg: cfg, Links: map[string]string{
		"/etc/nginx/sites-enabled/default":   "/etc/nginx/sites-available/default",
		"/etc/nginx/sites-enabled/shop.conf": "/etc/nginx/sites-available/shop.conf",
	}, Disabled: map[string]string{
		"/etc/nginx/sites-available/off.conf": managedMarker + "\n# sm-meta: {\"v\":1,\"spec\":{\"engine\":\"nginx\",\"domains\":[\"off.example.com\"],\"type\":\"static\",\"root\":\"/srv\"}}\nserver { listen 80; server_name off.example.com; root /srv; }\n",
	}, Available: "/etc/nginx/sites-available", Enabled: "/etc/nginx/sites-enabled", ConfDir: "/etc/nginx/conf.d"}
	sites := env.sites()
	if len(sites) != 3 {
		t.Fatalf("sites = %d", len(sites))
	}
	var shop, def, off *Site
	for i := range sites {
		switch sites[i].File {
		case "/etc/nginx/sites-available/shop.conf":
			shop = &sites[i]
		case "/etc/nginx/sites-available/default":
			def = &sites[i]
		case "/etc/nginx/sites-available/off.conf":
			off = &sites[i]
		}
	}
	if shop == nil || def == nil || off == nil {
		t.Fatalf("missing sites: %+v", sites)
	}
	if !reflect.DeepEqual(shop.Domains, []string{"shop.example.com", "www.shop.example.com"}) {
		t.Errorf("shop domains = %v", shop.Domains)
	}
	if shop.Kind != TypeProxy || shop.Target != "10.0.0.1:80, 10.0.0.2:80" || !shop.HTTPS || !shop.HTTP2 || !shop.Enabled || shop.Managed {
		t.Errorf("shop = %+v", shop)
	}
	if len(shop.Certs) != 1 || shop.Certs[0] != "/etc/letsencrypt/live/shop.example.com/fullchain.pem" {
		t.Errorf("certs = %v", shop.Certs)
	}
	if def.Kind != TypeStatic || def.Target != "/var/www/html" || !def.Default || len(def.Domains) != 0 || !def.CanToggle {
		t.Errorf("default = %+v", def)
	}
	if off.Enabled || !off.Managed || off.Spec == nil || off.Spec.Root != "/srv" {
		t.Errorf("off = %+v", off)
	}
	// Directive values with quotes/braces are parsed as single tokens.
	var hdr string
	for _, d := range ngxFindDeep(ngxParse(files[4].Content), "add_header") {
		hdr = d.arg(1)
	}
	if hdr != "brace } in ; string" {
		t.Errorf("quoted arg = %q", hdr)
	}
	sets := ngxFindDeep(ngxParse(files[4].Content), "set")
	if len(sets) != 1 || sets[0].arg(1) != "${host}{" {
		t.Errorf("set = %+v", sets)
	}
}

const sampleCaddyfile = `{
	email admin@example.com
}

(common) {
	encode gzip
}

:80 {
	root * /usr/share/caddy
	file_server
}

example.com, www.example.com {
	import common
	reverse_proxy /api/* 127.0.0.1:8080 127.0.0.1:8081 {
		lb_policy round_robin
	}
	header X-A "quoted { brace"
}

http://old.example.com {
	redir https://new.example.com{uri} permanent
}

import sites/*.caddy
`

func TestCaddyParse(t *testing.T) {
	sites, imports, snippets := cdySites(cdyParse(sampleCaddyfile))
	if len(sites) != 3 || !reflect.DeepEqual(imports, []string{"sites/*.caddy"}) || !snippets["common"] {
		t.Fatalf("sites=%d imports=%v snippets=%v", len(sites), imports, snippets)
	}
	b := summarizeCaddySite(sites[1])
	if !reflect.DeepEqual(b.ServerNames, []string{"example.com", "www.example.com"}) || !reflect.DeepEqual(b.ProxyPass, []string{"127.0.0.1:8080", "127.0.0.1:8081"}) {
		t.Errorf("site = %+v", b)
	}
	r := summarizeCaddySite(sites[2])
	if r.Return != "301 https://new.example.com{uri}" || len(r.Listens) != 1 || r.Listens[0].Port != 80 || r.Listens[0].SSL {
		t.Errorf("redirect = %+v", r)
	}
	if !caddyImportRe.MatchString(sampleCaddyfile) {
		t.Error("import not detected")
	}
	// Generated caddy files parse back into one site with the meta intact.
	sp := baseSpec()
	sp.Engine = EngineCaddy
	out := gen(t, sp, ngxGenOpts{Name: "app.example.com"})
	st := (&caddyEnv{SitesDir: "/etc/caddy/sites", Files: []ngxFile{{Path: "/etc/caddy/sites/app.example.com.caddy", Content: out.Content}}}).sites()
	if len(st) != 1 || !st[0].Managed || st[0].Spec == nil || st[0].Kind != TypeProxy || st[0].Target != "127.0.0.1:3000" || !st[0].CanToggle {
		t.Errorf("caddy site = %+v", st)
	}
	if h := caddyHashes("x.com {\n\tbasicauth {\n\t\talice $2a$14$abc\n\t}\n}\n"); h["alice"] != "$2a$14$abc" {
		t.Errorf("hashes = %v", h)
	}
}

const certbotOut = `Saving debug log to /var/log/letsencrypt/letsencrypt.log

- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -
Found the following certs:
  Certificate Name: example.com
    Serial Number: 4a1b2c3d
    Key Type: ECDSA
    Domains: example.com www.example.com
    Expiry Date: 2026-12-01 10:20:30+00:00 (VALID: 67 days)
    Certificate Path: /etc/letsencrypt/live/example.com/fullchain.pem
    Private Key Path: /etc/letsencrypt/live/example.com/privkey.pem
  Certificate Name: test.example.com
    Domains: test.example.com
    Expiry Date: 2026-01-01 00:00:00+00:00 (INVALID: TEST_CERT)
    Certificate Path: /etc/letsencrypt/live/test.example.com/fullchain.pem
    Private Key Path: /etc/letsencrypt/live/test.example.com/privkey.pem
- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -
`

func TestParseCertbot(t *testing.T) {
	list := parseCertbotCertificates(certbotOut)
	if len(list) != 2 {
		t.Fatalf("certs = %d", len(list))
	}
	a := list[0]
	want := time.Date(2026, 12, 1, 10, 20, 30, 0, time.UTC).Unix()
	if a.Name != "example.com" || a.NotAfter != want || a.KeyType != "ECDSA" || a.Serial != "4A1B2C3D" ||
		!reflect.DeepEqual(a.Domains, []string{"example.com", "www.example.com"}) || a.Path != "/etc/letsencrypt/live/example.com/fullchain.pem" {
		t.Errorf("cert = %+v", a)
	}
	if !list[1].Staging {
		t.Error("TEST_CERT not detected")
	}
	if len(parseCertbotCertificates("No certificates found.\n")) != 0 {
		t.Error("empty output parsed")
	}
}

func TestParseOpenssl(t *testing.T) {
	outs := map[string]string{
		"3.0": `subject=CN = example.com
issuer=C = US, O = Let's Encrypt, CN = R3
notBefore=Sep  1 00:00:00 2026 GMT
notAfter=Nov 30 23:59:59 2026 GMT
serial=04A1B2
X509v3 Subject Alternative Name:
    DNS:example.com, DNS:www.example.com, IP Address:10.0.0.1
`,
		"1.0": `subject= /CN=example.com
issuer= /C=US/O=Let's Encrypt/CN=R3
notBefore=Sep  1 00:00:00 2026 GMT
notAfter=Nov 30 23:59:59 2026 GMT
serial=04A1B2
Certificate:
    Data:
        X509v3 extensions:
            X509v3 Subject Alternative Name:
                DNS:example.com, DNS:www.example.com, IP Address:10.0.0.1
`,
		"rfc2253": `subject=CN=example.com
issuer=C=US,O=Let's Encrypt,CN=R3
notBefore=Sep  1 00:00:00 2026 GMT
notAfter=Nov 30 23:59:59 2026 GMT
serial=04A1B2
X509v3 Subject Alternative Name:
    DNS:example.com, DNS:www.example.com, IP Address:10.0.0.1
`,
	}
	want := time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC).Unix()
	for v, out := range outs {
		info, ok := parseOpenssl(out)
		if !ok || info.NotAfter != want || info.Subject["CN"] != "example.com" || dnDisplay(info.Issuer) != "Let's Encrypt R3" ||
			!reflect.DeepEqual(info.SANs, []string{"example.com", "www.example.com", "10.0.0.1"}) || info.Serial != "04A1B2" {
			t.Errorf("%s: %+v ok=%v", v, info, ok)
		}
	}
	if _, ok := parseOpenssl("unable to load certificate\n"); ok {
		t.Error("error output parsed")
	}
}

func TestParseTimers(t *testing.T) {
	out := `Id=certbot.timer
LoadState=loaded
ActiveState=active
NextElapseUSecRealtime=Fri 2026-09-25 17:50:08 UTC
LastTriggerUSec=n/a

Id=snap.certbot.renew.timer
LoadState=not-found
ActiveState=inactive
NextElapseUSecRealtime=
LastTriggerUSec=n/a`
	list := parseTimers(out)
	if len(list) != 1 || list[0].Unit != "certbot.timer" || !list[0].Active || list[0].Next != time.Date(2026, 9, 25, 17, 50, 8, 0, time.UTC).Unix() {
		t.Errorf("timers = %+v", list)
	}
}

func TestCertStatus(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		d    time.Duration
		want string
	}{{-time.Hour, "expired"}, {3 * 24 * time.Hour, "err"}, {10 * 24 * time.Hour, "warn"}, {60 * 24 * time.Hour, "ok"}} {
		if _, s := certStatus(now.Add(c.d).Unix(), now); s != c.want {
			t.Errorf("%v: %s, want %s", c.d, s, c.want)
		}
	}
}

func selfSigned(t *testing.T, cn string, days int) (string, string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Duration(days) * 24 * time.Hour),
		DNSNames: []string{cn, "www." + cn}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), key
}

func TestParseCertPair(t *testing.T) {
	cert, key, _ := selfSigned(t, "smtest.example", 30)
	p, err := parseCertPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if p.info.Subject != "smtest.example" || !p.info.SelfSigned || p.info.KeyType != "ECDSA P-256" || p.info.Status != "ok" ||
		!reflect.DeepEqual(p.info.Domains, []string{"smtest.example", "www.smtest.example"}) || !strings.Contains(string(p.keyPEM), "BEGIN PRIVATE KEY") {
		t.Errorf("info = %+v", p.info)
	}
	// key pasted together with the chain
	if _, err := parseCertPair(cert+key, ""); err != nil {
		t.Errorf("combined PEM: %v", err)
	}
	_, other, _ := selfSigned(t, "other.example", 30)
	if code(func() error { _, err := parseCertPair(cert, other); return err }()) != "web.keyMismatch" {
		t.Error("mismatched key accepted")
	}
	if code(func() error {
		_, err := parseCertPair(cert, "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n")
		return err
	}()) != "web.keyEncrypted" {
		t.Error("encrypted key accepted")
	}
	if code(func() error { _, err := parseCertPair("garbage", key); return err }()) != "web.certParse" {
		t.Error("garbage accepted")
	}
	exp, ekey, _ := selfSigned(t, "old.example", -1)
	p, err = parseCertPair(exp, ekey)
	if err != nil || p.info.Status != "expired" {
		t.Errorf("expired: %+v %v", p, err)
	}
}

func TestTxnScript(t *testing.T) {
	s := buildTxnScript("/etc/nginx/.sm-txn.abc", []string{"mkdir -p /x"}, []txOp{
		{Kind: "write", Path: "/etc/nginx/sites-available/a b.conf", Mode: "0644", MustNotExist: true},
		{Kind: "link", Path: "/etc/nginx/sites-enabled/a b.conf", Target: "/etc/nginx/sites-available/a b.conf"},
		{Kind: "rename", Path: "/etc/nginx/conf.d/x.conf", Target: "/etc/nginx/conf.d/x.conf.disabled"},
	}, "nginx -t")
	for _, want := range []string{"'/etc/nginx/sites-available/a b.conf'", "if [ $A -ge 2 ]; then mv -f '/etc/nginx/conf.d/x.conf.disabled' '/etc/nginx/conf.d/x.conf'", "OUT=$( { nginx -t; } 2>&1 )", "exit 3"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

func TestSplitSections(t *testing.T) {
	m := "@@SMx"
	secs := splitSections("junk\n@@SMx a\nline1\n@@SMx file /a b\nx\ny\n@@SMx end\n", m)
	if len(secs) != 3 || secs[0].Body != "line1" || secs[1].Arg != "/a b" || secs[1].Body != "x\ny" {
		t.Errorf("secs = %+v", secs)
	}
}
