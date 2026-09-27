//go:build integration

package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

// Integration tests against the Debian test server (nginx running on :80,
// caddy installed but disabled, certbot installed, no public DNS).
// Everything created is named smtest-web-* and removed at the end.

func setup(t *testing.T) (*WebService, *core.Core, string) {
	t.Helper()
	c, id := testutil.Connect(t, testutil.FullPort())
	return New(c), c, id
}

// sh runs a command on the server as root.
func sh(t *testing.T, c *core.Core, id, cmd string) string {
	t.Helper()
	conn, err := c.Conn(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.Run(ctx, conn, pathPrefix+cmd, true, testutil.Password, "")
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return strings.TrimSpace(res.Stdout)
}

func httpGet(t *testing.T, c *core.Core, id, host, path string) (code, body string) {
	t.Helper()
	out := sh(t, c, id, "curl -s -m 5 -w '\\n@@%{http_code}' -H "+core.Q("Host: "+host)+" "+core.Q("http://127.0.0.1"+path))
	i := strings.LastIndex(out, "\n@@")
	if i < 0 {
		if strings.HasPrefix(out, "@@") {
			return strings.TrimPrefix(out, "@@"), ""
		}
		t.Fatalf("curl output: %q", out)
	}
	return out[i+3:], out[:i]
}

// eventually retries check for a few seconds: nginx reloads gracefully, so
// new workers take a moment to replace the old ones.
func eventually(t *testing.T, desc string, check func() (bool, string)) {
	t.Helper()
	var last string
	for i := 0; i < 40; i++ {
		ok, got := check()
		if ok {
			return
		}
		last = got
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("%s: last result %q", desc, last)
}

func expectHTTP(t *testing.T, c *core.Core, id, host, path, wantCode, wantBody string) {
	t.Helper()
	eventually(t, host+path, func() (bool, string) {
		code, body := httpGet(t, c, id, host, path)
		return code == wantCode && strings.Contains(body, wantBody), code + " " + body
	})
}

func mustCode(t *testing.T, err error, want string) {
	t.Helper()
	if code(err) != want {
		t.Fatalf("error = %v (code %q), want %s", err, code(err), want)
	}
}

func cleanupNginx(t *testing.T, c *core.Core, id string) {
	sh(t, c, id, `rm -f /etc/nginx/sites-enabled/smtest-web-* /etc/nginx/sites-available/smtest-web-* /etc/nginx/conf.d/sm-ratelimit-smtest-web-* /etc/nginx/htpasswd/smtest-web-* /var/log/nginx/smtest-web-*;
rm -rf /var/www/smtest-web-* /etc/ssl/server-manager/smtest-*
grep -rqs sm_connection_upgrade /etc/nginx/sites-enabled/ /etc/nginx/sites-available/ || rm -f /etc/nginx/conf.d/sm-websocket-map.conf
rmdir /etc/nginx/htpasswd /etc/ssl/server-manager /var/www/letsencrypt/.well-known/acme-challenge /var/www/letsencrypt/.well-known /var/www/letsencrypt 2>/dev/null
nginx -t >/dev/null 2>&1 && systemctl reload nginx; true`)
}

func TestIntegrationStatus(t *testing.T) {
	s, _, id := setup(t)
	st, err := s.Status(id, "")
	if err != nil {
		t.Fatal(err)
	}
	eng := map[string]EngineInfo{}
	for _, e := range st.Engines {
		eng[e.Name] = e
	}
	ng, cd := eng["nginx"], eng["caddy"]
	if !ng.Installed || !ng.Running || ng.Version == "" || !ng.TestRun || !ng.TestOK || ng.Layout != "debian" {
		t.Errorf("nginx = %+v", ng)
	}
	if !cd.Installed || cd.Running || cd.Version == "" {
		t.Errorf("caddy = %+v", cd)
	}
	if eng["apache"].Installed || eng["traefik"].Installed {
		t.Errorf("apache/traefik detected: %+v %+v", eng["apache"], eng["traefik"])
	}
	if !st.Certbot || st.PkgManager != "apt" || !st.Systemd {
		t.Errorf("status = %+v", st)
	}
	sites, err := s.Sites(id, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range sites.Sites {
		if x.File == "/etc/nginx/sites-available/default" && x.Enabled && x.Default && x.Kind == TypeStatic && !x.Managed {
			found = true
		}
	}
	if !found || sites.NginxLayout != "debian" || !sites.NginxTestOK {
		t.Errorf("sites = %+v", sites)
	}
}

func findSite(t *testing.T, s *WebService, id, file string) *Site {
	t.Helper()
	res, err := s.Sites(id, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range res.Sites {
		if res.Sites[i].File == file {
			return &res.Sites[i]
		}
	}
	return nil
}

func TestIntegrationNginxSites(t *testing.T) {
	s, c, id := setup(t)
	cleanupNginx(t, c, id)
	t.Cleanup(func() { cleanupNginx(t, c, id) })
	sh(t, c, id, "mkdir -p /var/www/smtest-web-static && echo hello-smtest > /var/www/smtest-web-static/index.html && chmod -R a+rX /var/www/smtest-web-static")

	// static
	static := SiteSpec{Engine: EngineNginx, Domains: []string{"smtest-web-static.test"}, Type: TypeStatic, Root: "/var/www/smtest-web-static", SPA: true, Gzip: true}
	res, err := s.Apply(id, "", static, "")
	if err != nil {
		t.Fatalf("apply static: %v", err)
	}
	if res.File != "/etc/nginx/sites-available/smtest-web-static.test.conf" || !res.Reloaded {
		t.Fatalf("res = %+v", res)
	}
	if l := sh(t, c, id, "readlink /etc/nginx/sites-enabled/smtest-web-static.test.conf"); l != res.File {
		t.Errorf("link -> %q", l)
	}
	expectHTTP(t, c, id, "smtest-web-static.test", "/deep/spa/route", "200", "hello-smtest")
	expectHTTP(t, c, id, "smtest-web-static.test", "/.well-known/acme-challenge/nothing", "404", "")
	staticFile := res.File

	// Creating the same site again is refused.
	if _, err := s.Apply(id, "", static, ""); code(err) != "web.siteExists" {
		t.Errorf("duplicate: %v", err)
	}
	// Another site claiming the same domain is refused.
	dup := SiteSpec{Domains: []string{"smtest-web-other.test", "smtest-web-static.test"}, Type: TypeRedirect, RedirectTo: "https://example.com"}
	if _, err := s.Apply(id, "", dup, ""); code(err) != "web.domainInUse" {
		t.Errorf("domain conflict: %v", err)
	}

	// proxy → the static site, overriding Host; websocket + rate limit + basic auth
	proxy := SiteSpec{Domains: []string{"smtest-web-proxy.test"}, Type: TypeProxy, Upstreams: []string{"http://127.0.0.1:80"},
		RequestHeaders: []Header{{Name: "Host", Value: "smtest-web-static.test"}}, WebSocket: true,
		RateLimit: true, RateRPS: 50, RateBurst: 100, RateNoDelay: true,
		BasicAuth: true, AuthUsers: []AuthUser{{User: "smtest", Password: "pa$$ w\"rd"}}}
	res, err = s.Apply(id, "", proxy, "")
	if err != nil {
		t.Fatalf("apply proxy: %v", err)
	}
	proxyFile := res.File
	expectHTTP(t, c, id, "smtest-web-proxy.test", "/", "401", "")
	eventually(t, "proxy with auth", func() (bool, string) {
		out := sh(t, c, id, `curl -s -m 5 -u 'smtest:pa$$ w"rd' -H 'Host: smtest-web-proxy.test' http://127.0.0.1/`)
		return strings.Contains(out, "hello-smtest"), out
	})
	if st := sh(t, c, id, "stat -c '%a %U:%G' /etc/nginx/htpasswd/smtest-web-proxy.test"); st != "640 root:www-data" {
		t.Errorf("htpasswd perms %q", st)
	}
	if h := sh(t, c, id, "cat /etc/nginx/htpasswd/smtest-web-proxy.test"); !strings.HasPrefix(h, "smtest:$2") || strings.Contains(h, "w\"rd") {
		t.Errorf("htpasswd %q", h)
	}
	if sh(t, c, id, "test -f /etc/nginx/conf.d/sm-ratelimit-smtest-web-proxy.test.conf && test -f /etc/nginx/conf.d/sm-websocket-map.conf && echo ok") != "ok" {
		t.Error("helper files missing")
	}
	site := findSite(t, s, id, proxyFile)
	if site == nil || !site.Managed || site.Spec == nil || site.Kind != TypeProxy || site.Target != "http://127.0.0.1:80" || len(site.Spec.AuthUsers) != 1 || site.Spec.AuthUsers[0].Password != "" {
		t.Fatalf("proxy site = %+v", site)
	}

	// update: keep the password (empty), add a response header, drop rate limit
	upd := *site.Spec
	upd.ResponseHeaders = []Header{{Name: "X-Smtest", Value: "yes"}}
	upd.RateLimit = false
	if _, err := s.Apply(id, proxyFile, upd, ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	eventually(t, "updated headers", func() (bool, string) {
		out := sh(t, c, id, `curl -s -m 5 -D - -o /dev/null -u 'smtest:pa$$ w"rd' -H 'Host: smtest-web-proxy.test' http://127.0.0.1/`)
		return strings.Contains(out, "X-Smtest: yes") && strings.Contains(out, " 200"), out
	})
	if sh(t, c, id, "test -e /etc/nginx/conf.d/sm-ratelimit-smtest-web-proxy.test.conf && echo left") != "" {
		t.Error("rate limit zone file not removed")
	}
	if h, _ := s.History(id, proxyFile); len(h) != 1 || h[0].Action != "update" {
		t.Errorf("history = %d", len(h))
	}

	// redirect
	red := SiteSpec{Domains: []string{"smtest-web-redirect.test"}, Type: TypeRedirect, RedirectTo: "https://example.com/new/", RedirectCode: 302, PreservePath: true}
	res, err = s.Apply(id, "", red, "")
	if err != nil {
		t.Fatalf("apply redirect: %v", err)
	}
	redFile := res.File
	eventually(t, "redirect", func() (bool, string) {
		out := sh(t, c, id, "curl -s -m 5 -o /dev/null -w '%{http_code} %{redirect_url}' -H 'Host: smtest-web-redirect.test' http://127.0.0.1/a/b?c=1")
		return out == "302 https://example.com/new/a/b?c=1", out
	})

	// broken raw edit rolls back
	before := sh(t, c, id, "cat "+core.Q(redFile))
	_, err = s.SaveRaw(id, EngineNginx, redFile, before+"\nsmtest_unknown_directive on;\n", "")
	mustCode(t, err, "web.testFailed")
	if after := sh(t, c, id, "cat "+core.Q(redFile)); after != before {
		t.Error("file not restored after failed test")
	}
	if sh(t, c, id, "nginx -t >/dev/null 2>&1 && echo ok") != "ok" {
		t.Fatal("nginx config left broken")
	}
	// a new site whose extra directives fail is not left behind
	bad := SiteSpec{Domains: []string{"smtest-web-bad.test"}, Type: TypeStatic, Root: "/var/www/smtest-web-static", Extra: "smtest_unknown on;"}
	_, err = s.Apply(id, "", bad, "")
	mustCode(t, err, "web.testFailed")
	if sh(t, c, id, "ls /etc/nginx/sites-available/smtest-web-bad* /etc/nginx/sites-enabled/smtest-web-bad* 2>/dev/null") != "" {
		t.Error("failed new site left files")
	}
	if sh(t, c, id, "ls -a /etc/nginx | grep sm-txn") != "" {
		t.Error("transaction dir left behind")
	}
	// raw edit that is valid, then restore the previous version
	if _, err := s.SaveRaw(id, EngineNginx, redFile, strings.Replace(before, "return 302", "return 301", 1), ""); err != nil {
		t.Fatalf("raw save: %v", err)
	}
	expectHTTP(t, c, id, "smtest-web-redirect.test", "/", "301", "")
	h, _ := s.History(id, redFile)
	if len(h) != 1 {
		t.Fatalf("history = %d", len(h))
	}
	if _, err := s.RestoreVersion(id, EngineNginx, redFile, h[0].TS, ""); err != nil {
		t.Fatalf("restore: %v", err)
	}
	expectHTTP(t, c, id, "smtest-web-redirect.test", "/", "302", "")
	// Files nginx doesn't load can't be edited through the raw editor.
	_, err = s.SaveRaw(id, EngineNginx, "/etc/passwd", "x", "")
	mustCode(t, err, "web.siteNotFound")

	// disable / enable
	if _, err := s.SetEnabled(id, EngineNginx, staticFile, false, ""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	eventually(t, "disabled site not served", func() (bool, string) {
		_, body := httpGet(t, c, id, "smtest-web-static.test", "/")
		return !strings.Contains(body, "hello-smtest"), body
	})
	if st := findSite(t, s, id, staticFile); st == nil || st.Enabled || !st.Managed {
		t.Errorf("disabled site = %+v", st)
	}
	if _, err := s.SetEnabled(id, EngineNginx, staticFile, true, ""); err != nil {
		t.Fatalf("enable: %v", err)
	}
	expectHTTP(t, c, id, "smtest-web-static.test", "/", "200", "hello-smtest")

	// delete everything
	for _, f := range []string{proxyFile, redFile, staticFile} {
		if _, err := s.DeleteSite(id, EngineNginx, f, ""); err != nil {
			t.Fatalf("delete %s: %v", f, err)
		}
	}
	if left := sh(t, c, id, "ls /etc/nginx/sites-available/smtest-web-* /etc/nginx/sites-enabled/smtest-web-* /etc/nginx/htpasswd/smtest-web-* 2>/dev/null"); left != "" {
		t.Errorf("left after delete: %s", left)
	}
	// deleting the main config is refused
	_, err = s.DeleteSite(id, EngineNginx, "/etc/nginx/nginx.conf", "")
	if err == nil {
		t.Error("deleting nginx.conf allowed")
	}
}

func TestIntegrationLetsEncryptPending(t *testing.T) {
	s, c, id := setup(t)
	cleanupNginx(t, c, id)
	t.Cleanup(func() { cleanupNginx(t, c, id) })
	// certbot registers an ACME staging account on the first attempt.
	if sh(t, c, id, "test -d /etc/letsencrypt/accounts/acme-staging-v02.api.letsencrypt.org && echo yes") == "" {
		t.Cleanup(func() {
			sh(t, c, id, "rm -rf /etc/letsencrypt/accounts/acme-staging-v02.api.letsencrypt.org; rmdir /etc/letsencrypt/accounts 2>/dev/null; rm -rf /etc/letsencrypt/renewal/smtest-web-* /etc/letsencrypt/live/smtest-web-* /etc/letsencrypt/archive/smtest-web-*; true")
		})
	}
	sp := SiteSpec{Domains: []string{"smtest-web-le.example.com"}, Type: TypeProxy, Upstreams: []string{"127.0.0.1:9"},
		SSL: SSLLetsEncrypt, Email: "smtest@example.com", Staging: true, ForceHTTPS: true, HTTP2: true, HSTS: true}
	res, err := s.Apply(id, "", sp, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.PendingCert {
		t.Error("expected pending certificate")
	}
	if sh(t, c, id, "test -d /var/www/letsencrypt/.well-known/acme-challenge && echo ok") != "ok" {
		t.Error("webroot missing")
	}
	// The ACME challenge location serves files from the webroot.
	sh(t, c, id, "echo smtest-token > /var/www/letsencrypt/.well-known/acme-challenge/smtest-web-token")
	defer sh(t, c, id, "rm -f /var/www/letsencrypt/.well-known/acme-challenge/smtest-web-token")
	expectHTTP(t, c, id, "smtest-web-le.example.com", "/.well-known/acme-challenge/smtest-web-token", "200", "smtest-token")
	expectHTTP(t, c, id, "smtest-web-le.example.com", "/", "502", "")
	st := findSite(t, s, id, res.File)
	if st == nil || !st.PendingCert {
		t.Fatalf("site = %+v", st)
	}
	// Issuance can't succeed without public DNS: the job fails and the
	// site stays as it was.
	before := sh(t, c, id, "cat "+core.Q(res.File))
	jobID, err := s.IssueCertificate(id, res.File, "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	j, _ := s.core.Jobs.Get(jobID)
	info := j.Wait()
	if info.State != "error" {
		t.Errorf("issue job state %s", info.State)
	}
	if !strings.Contains(info.Log, "certbot certonly --webroot -w /var/www/letsencrypt") {
		t.Errorf("job log: %s", info.Log)
	}
	if after := sh(t, c, id, "cat "+core.Q(res.File)); after != before {
		t.Error("site changed after failed issuance")
	}
	// Unmanaged or non-LE sites can't be issued for.
	_, err = s.IssueCertificate(id, "/etc/nginx/sites-available/default", "")
	mustCode(t, err, "web.notManaged")
}

func TestIntegrationCustomCert(t *testing.T) {
	s, c, id := setup(t)
	cleanupNginx(t, c, id)
	t.Cleanup(func() { cleanupNginx(t, c, id) })
	cert, key, _ := selfSigned(t, "smtest-web-tls.test", 30)
	info, err := s.InspectCertificate(cert, key)
	if err != nil || !info.SelfSigned || info.Subject != "smtest-web-tls.test" {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	if _, err := s.InstallCertificate(id, "smtest-web-cert", cert, key, false, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if st := sh(t, c, id, "stat -c '%a %U:%G' /etc/ssl/server-manager/smtest-web-cert /etc/ssl/server-manager/smtest-web-cert/fullchain.pem /etc/ssl/server-manager/smtest-web-cert/privkey.pem"); st != "700 root:root\n600 root:root\n600 root:root" {
		t.Errorf("perms: %q", st)
	}
	if _, err := s.InstallCertificate(id, "smtest-web-cert", cert, key, false, ""); code(err) != "web.certExists" {
		t.Errorf("reinstall without replace: %v", err)
	}
	ov, err := s.CertOverview(id, "")
	if err != nil {
		t.Fatal(err)
	}
	var found *Certificate
	for i := range ov.Certificates {
		if ov.Certificates[i].Path == "/etc/ssl/server-manager/smtest-web-cert/fullchain.pem" {
			found = &ov.Certificates[i]
		}
	}
	if found == nil || found.Source != "custom" || found.Status != "ok" || found.NotAfter == 0 || found.Issuer != "smtest-web-tls.test" ||
		strings.Join(found.Domains, ",") != "smtest-web-tls.test,www.smtest-web-tls.test" {
		t.Fatalf("custom cert = %+v", found)
	}
	sh(t, c, id, "mkdir -p /var/www/smtest-web-tls && echo tls-smtest > /var/www/smtest-web-tls/index.html && chmod -R a+rX /var/www/smtest-web-tls")
	sp := SiteSpec{Domains: []string{"smtest-web-tls.test"}, Type: TypeStatic, Root: "/var/www/smtest-web-tls", SSL: SSLCustom,
		CertName: "smtest-web-cert", CertPath: "/etc/ssl/server-manager/smtest-web-cert/fullchain.pem", KeyPath: "/etc/ssl/server-manager/smtest-web-cert/privkey.pem", ForceHTTPS: true, HTTP2: true}
	res, err := s.Apply(id, "", sp, "")
	if err != nil {
		t.Fatalf("apply tls: %v", err)
	}
	eventually(t, "https", func() (bool, string) {
		out := sh(t, c, id, "curl -sk -m 5 --resolve smtest-web-tls.test:443:127.0.0.1 https://smtest-web-tls.test/")
		return out == "tls-smtest", out
	})
	expectHTTP(t, c, id, "smtest-web-tls.test", "/", "301", "")
	// In use: deletion refused.
	mustCode(t, s.DeleteCustomCertificate(id, "smtest-web-cert", ""), "web.certInUse")
	certs, err := s.Certificates(id, "")
	if err != nil {
		t.Fatal(err)
	}
	used := false
	for _, x := range certs {
		if x.Path == "/etc/ssl/server-manager/smtest-web-cert/fullchain.pem" && len(x.UsedBy) == 1 && x.UsedBy[0] == res.File {
			used = true
		}
	}
	if !used {
		t.Errorf("usedBy not reported: %+v", certs)
	}
	if _, err := s.DeleteSite(id, EngineNginx, res.File, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCustomCertificate(id, "smtest-web-cert", ""); err != nil {
		t.Fatal(err)
	}
	if sh(t, c, id, "test -e /etc/ssl/server-manager/smtest-web-cert && echo left") != "" {
		t.Error("cert dir left")
	}
}

func TestIntegrationCaddy(t *testing.T) {
	s, c, id := setup(t)
	added := sh(t, c, id, "grep -q 'import sites/\\*.caddy' /etc/caddy/Caddyfile || echo missing") == "missing"
	hadOrig := sh(t, c, id, "test -e /etc/caddy/Caddyfile.sm-orig && echo yes") == "yes"
	hadDir := sh(t, c, id, "test -d /etc/caddy/sites && echo yes") == "yes"
	t.Cleanup(func() {
		sh(t, c, id, "rm -f /etc/caddy/sites/smtest-web-* /var/log/caddy/smtest-web-*")
		if added {
			sh(t, c, id, "cp -a /etc/caddy/Caddyfile.sm-orig /etc/caddy/Caddyfile")
			if !hadOrig {
				sh(t, c, id, "rm -f /etc/caddy/Caddyfile.sm-orig")
			}
		}
		if !hadDir {
			sh(t, c, id, "rmdir /etc/caddy/sites 2>/dev/null; true")
		}
	})
	sp := SiteSpec{Engine: EngineCaddy, Domains: []string{"smtest-web-caddy.test"}, Type: TypeProxy, Upstreams: []string{"127.0.0.1:3000", "127.0.0.1:3001"},
		LBMethod: "least_conn", SSL: SSLInternal, HSTS: true, Gzip: true, BasicAuth: true, AuthUsers: []AuthUser{{User: "smtest", Password: "secret"}}}
	res, err := s.Apply(id, "", sp, "")
	if err != nil {
		t.Fatalf("apply caddy: %v (%s)", err, res.TestOutput)
	}
	if !res.NotRunning || res.Reloaded {
		t.Errorf("caddy is disabled: %+v", res)
	}
	if sh(t, c, id, "grep -c 'import sites/\\*.caddy' /etc/caddy/Caddyfile") != "1" {
		t.Error("import not added exactly once")
	}
	if st := sh(t, c, id, "stat -c '%a %U:%G' /etc/caddy/sites/smtest-web-caddy.test.caddy"); st != "640 root:caddy" {
		t.Errorf("perms %q", st)
	}
	site := findSite(t, s, id, res.File)
	if site == nil || !site.Managed || site.Engine != EngineCaddy || site.Kind != TypeProxy || !site.Enabled {
		t.Fatalf("caddy site = %+v", site)
	}
	// update keeps the stored hash when the password is left empty
	upd := *site.Spec
	upd.Type, upd.Upstreams, upd.RedirectTo = TypeRedirect, nil, "https://example.com"
	if _, err := s.Apply(id, res.File, upd, ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.Contains(sh(t, c, id, "cat /etc/caddy/sites/smtest-web-caddy.test.caddy"), "smtest $2a$") {
		t.Error("hash not kept")
	}
	// invalid raw content is rejected by caddy validate and rolled back
	before := sh(t, c, id, "cat "+core.Q(res.File))
	_, err = s.SaveRaw(id, EngineCaddy, res.File, before+"\nsmtest-web-broken.test {\n\tnot_a_directive\n}\n", "")
	mustCode(t, err, "web.testFailed")
	if sh(t, c, id, "cat "+core.Q(res.File)) != before {
		t.Error("caddy file not restored")
	}
	// disable/enable via suffix
	r2, err := s.SetEnabled(id, EngineCaddy, res.File, false, "")
	if err != nil || r2.ID != res.File+".disabled" {
		t.Fatalf("disable: %+v %v", r2, err)
	}
	r3, err := s.SetEnabled(id, EngineCaddy, r2.ID, true, "")
	if err != nil || r3.ID != res.File {
		t.Fatalf("enable: %+v %v", r3, err)
	}
	if _, err := s.DeleteSite(id, EngineCaddy, res.File, ""); err != nil {
		t.Fatal(err)
	}
	// editing the main Caddyfile through SetEnabled/Delete is refused
	if _, err := s.DeleteSite(id, EngineCaddy, "/etc/caddy/Caddyfile", ""); err == nil {
		t.Error("deleting the Caddyfile allowed")
	}
}

func TestIntegrationCertbot(t *testing.T) {
	s, _, id := setup(t)
	ov, err := s.CertOverview(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ov.Certbot || ov.CertbotVersion == "" || ov.PkgManager != "apt" {
		t.Errorf("overview = %+v", ov)
	}
	timer := false
	for _, tm := range ov.Renewal.Timers {
		if tm.Unit == "certbot.timer" && tm.Active && tm.Next > 0 {
			timer = true
		}
	}
	if !timer || !ov.Renewal.Automatic {
		t.Errorf("renewal = %+v", ov.Renewal)
	}
	for _, c := range ov.Certificates {
		if c.Source == "certbot" && (c.Name == "" || c.Path == "") {
			t.Errorf("bad certbot entry %+v", c)
		}
	}
	jobID, err := s.TestRenewal(id, "", "")
	if err != nil {
		t.Fatal(err)
	}
	j, _ := s.core.Jobs.Get(jobID)
	if info := j.Wait(); info.State != "done" {
		t.Errorf("dry run: %s %v\n%s", info.State, info.Error, info.Log)
	}
	_, err = s.RenewCertificate(id, "../etc", false, "")
	mustCode(t, err, "web.invalidCertName")
	mustCode(t, s.DeleteCertificate(id, "smtest-web-none; rm -rf /", ""), "web.invalidCertName")
}

func TestIntegrationDenied(t *testing.T) {
	s, c, id := setup(t)
	testutil.SetRole(t, c, id, "viewer")
	sp := SiteSpec{Domains: []string{"smtest-web-denied.test"}, Type: TypeRedirect, RedirectTo: "https://example.com"}
	_, err := s.Apply(id, "", sp, "")
	mustCode(t, err, "access.denied")
	_, err = s.SetEnabled(id, EngineNginx, "/etc/nginx/sites-available/default", false, "")
	mustCode(t, err, "access.denied")
	_, err = s.DeleteSite(id, EngineNginx, "/etc/nginx/sites-available/default", "")
	mustCode(t, err, "access.denied")
	_, err = s.SaveRaw(id, EngineNginx, "/etc/nginx/sites-available/default", "x", "")
	mustCode(t, err, "access.denied")
	_, err = s.RenewCertificate(id, "x", false, "")
	mustCode(t, err, "access.denied")
	_, err = s.InstallCertbot(id, "")
	mustCode(t, err, "access.denied")
	cert, key, _ := selfSigned(t, "smtest-web-denied.test", 3)
	_, err = s.InstallCertificate(id, "smtest-web-denied", cert, key, false, "")
	mustCode(t, err, "access.denied")
	// Reading is still allowed.
	if _, err := s.Sites(id, ""); err != nil {
		t.Errorf("viewer read: %v", err)
	}
}

func TestIntegrationInvalidInput(t *testing.T) {
	s, _, id := setup(t)
	for _, sp := range []SiteSpec{
		{Domains: []string{"bad domain.test"}, Type: TypeRedirect, RedirectTo: "https://example.com"},
		{Domains: []string{"smtest-web-x.test"}, Type: TypeProxy, Upstreams: []string{"http://127.0.0.1:3000;return 200"}},
		{Domains: []string{"smtest-web-x.test"}, Type: TypeStatic, Root: "relative/path"},
		{Domains: []string{"smtest-web-x.test"}, Type: TypeRedirect, RedirectTo: "https://example.com", Extra: "} server {"},
	} {
		if _, err := s.Apply(id, "", sp, ""); err == nil || !strings.HasPrefix(code(err), "web.") {
			t.Errorf("accepted %+v: %v", sp, err)
		}
	}
	if _, err := s.Apply(id, "/etc/nginx/../passwd", SiteSpec{Domains: []string{"a.test"}, Type: TypeRedirect, RedirectTo: "https://e.com"}, ""); err == nil {
		t.Error("path traversal id accepted")
	}
	if _, err := s.ReadSite(id, EngineNginx, "/etc/shadow", ""); code(err) != "web.siteNotFound" {
		t.Errorf("read /etc/shadow: %v", err)
	}
}
