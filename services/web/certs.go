package web

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Certificate is a TLS certificate found on the server. The monitoring
// module uses Certificates() for expiry alerts.
type Certificate struct {
	Name      string   `json:"name"`
	Domains   []string `json:"domains"`
	NotAfter  int64    `json:"notAfter"` // unix seconds (0 = unknown)
	NotBefore int64    `json:"notBefore"`
	Source    string   `json:"source"` // certbot | nginx | caddy | custom
	Path      string   `json:"path"`
	KeyPath   string   `json:"keyPath"`
	Issuer    string   `json:"issuer"`
	Subject   string   `json:"subject"`
	Serial    string   `json:"serial"`
	KeyType   string   `json:"keyType"`
	DaysLeft  int      `json:"daysLeft"`
	Status    string   `json:"status"` // ok | warn | err | expired | unknown
	Staging   bool     `json:"staging"`
	UsedBy    []string `json:"usedBy"` // site files referencing it
	Error     string   `json:"error"`  // file missing / unreadable
}

// RenewTimer is a systemd timer that renews certificates.
type RenewTimer struct {
	Unit     string `json:"unit"`
	Active   bool   `json:"active"`
	Next     int64  `json:"next"` // unix seconds (0 = unknown)
	NextText string `json:"nextText"`
	Last     int64  `json:"last"`
}

// RenewalStatus tells whether certificates renew automatically.
type RenewalStatus struct {
	Timers    []RenewTimer `json:"timers"`
	Cron      []string     `json:"cron"`
	Automatic bool         `json:"automatic"`
}

// CertOverview is the SSL tab content.
type CertOverview struct {
	Certificates   []Certificate `json:"certificates"`
	Renewal        RenewalStatus `json:"renewal"`
	Certbot        bool          `json:"certbot"`
	CertbotVersion string        `json:"certbotVersion"`
	PkgManager     string        `json:"pkgManager"`
}

// ---- parsing ----

// parseCertbotCertificates parses `certbot certificates` output.
func parseCertbotCertificates(out string) []Certificate {
	var list []Certificate
	var cur *Certificate
	for _, raw := range strings.Split(out, "\n") {
		l := strings.TrimSpace(raw)
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Certificate Name":
			if cur != nil {
				list = append(list, *cur)
			}
			cur = &Certificate{Name: v, Source: "certbot", Domains: []string{}, UsedBy: []string{}}
		case "Serial Number":
			if cur != nil {
				cur.Serial = strings.ToUpper(v)
			}
		case "Key Type":
			if cur != nil {
				cur.KeyType = v
			}
		case "Domains", "Identifiers":
			if cur != nil {
				cur.Domains = strings.Fields(v)
			}
		case "Expiry Date":
			if cur == nil {
				continue
			}
			date := v
			if i := strings.Index(v, "("); i >= 0 {
				date = strings.TrimSpace(v[:i])
				if strings.Contains(v[i:], "TEST_CERT") {
					cur.Staging = true
				}
			}
			for _, layout := range []string{"2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
				if t, err := time.Parse(layout, date); err == nil {
					cur.NotAfter = t.Unix()
					break
				}
			}
		case "Certificate Path":
			if cur != nil {
				cur.Path = v
			}
		case "Private Key Path":
			if cur != nil {
				cur.KeyPath = v
			}
		}
	}
	if cur != nil {
		list = append(list, *cur)
	}
	return list
}

type opensslInfo struct {
	Subject   map[string]string
	Issuer    map[string]string
	NotBefore int64
	NotAfter  int64
	Serial    string
	SANs      []string
}

// parseDN handles "CN = a, O = b" (OpenSSL 1.1/3), "CN=a,O=b" and the
// legacy "/C=US/O=b/CN=a" (OpenSSL 1.0) forms.
func parseDN(s string) map[string]string {
	m := map[string]string{}
	s = strings.TrimSpace(s)
	var parts []string
	if strings.HasPrefix(s, "/") {
		parts = strings.Split(s[1:], "/")
	} else {
		// split on commas not escaped and not inside quotes
		var b strings.Builder
		inQ := false
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c == '\\' && i+1 < len(s):
				b.WriteByte(s[i+1])
				i++
			case c == '"':
				inQ = !inQ
			case c == ',' && !inQ:
				parts = append(parts, b.String())
				b.Reset()
			default:
				b.WriteByte(c)
			}
		}
		parts = append(parts, b.String())
	}
	for _, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return m
}

func dnDisplay(m map[string]string) string {
	o, cn := m["O"], m["CN"]
	switch {
	case o != "" && cn != "" && !strings.Contains(cn, o):
		return o + " " + cn
	case cn != "":
		return cn
	default:
		return o
	}
}

func parseOpensslDate(v string) int64 {
	v = strings.Join(strings.Fields(v), " ")
	for _, layout := range []string{"Jan 2 15:04:05 2006 MST", "Jan 2 15:04:05.000 2006 MST", "2006-01-02 15:04:05Z"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// parseOpenssl parses `openssl x509 -noout -subject -issuer -startdate
// -enddate -serial -ext subjectAltName` (or -text) output.
func parseOpenssl(out string) (opensslInfo, bool) {
	info := opensslInfo{SANs: []string{}}
	lines := strings.Split(out, "\n")
	found := false
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(l, "subject="):
			info.Subject = parseDN(strings.TrimPrefix(l, "subject="))
			found = true
		case strings.HasPrefix(l, "issuer="):
			info.Issuer = parseDN(strings.TrimPrefix(l, "issuer="))
		case strings.HasPrefix(l, "notBefore="):
			info.NotBefore = parseOpensslDate(strings.TrimPrefix(l, "notBefore="))
		case strings.HasPrefix(l, "notAfter="):
			info.NotAfter = parseOpensslDate(strings.TrimPrefix(l, "notAfter="))
		case strings.HasPrefix(l, "serial="):
			info.Serial = strings.ToUpper(strings.TrimPrefix(l, "serial="))
		case strings.HasPrefix(l, "X509v3 Subject Alternative Name"):
			if i+1 < len(lines) {
				for _, e := range strings.Split(lines[i+1], ",") {
					e = strings.TrimSpace(e)
					if v, ok := strings.CutPrefix(e, "DNS:"); ok {
						info.SANs = append(info.SANs, v)
					} else if v, ok := strings.CutPrefix(e, "IP Address:"); ok {
						info.SANs = append(info.SANs, v)
					}
				}
				i++
			}
		}
	}
	return info, found && info.NotAfter > 0
}

func certStatus(notAfter int64, now time.Time) (int, string) {
	if notAfter == 0 {
		return 0, "unknown"
	}
	d := time.Unix(notAfter, 0).Sub(now)
	days := int(math.Floor(d.Hours() / 24))
	switch {
	case d <= 0:
		return days, "expired"
	case days < 7:
		return days, "err"
	case days < 14:
		return days, "warn"
	}
	return days, "ok"
}

// ---- gathering ----

const certGather = `
M=%s
echo "$M certbot"; command -v certbot >/dev/null 2>&1 && certbot certificates 2>&1
echo "$M certbotver"; certbot --version 2>&1 | grep -i certbot | head -n1
echo "$M pkg"; for p in apt-get dnf yum apk zypper pacman; do command -v $p >/dev/null 2>&1 && { echo $p; break; }; done
echo "$M custom"; for d in /etc/ssl/server-manager/*/; do [ -f "${d}fullchain.pem" ] && echo "${d}fullchain.pem"; done
echo "$M caddyauto"; for d in /var/lib/caddy/.local/share/caddy/certificates /root/.local/share/caddy/certificates; do [ -d "$d" ] && find "$d" -maxdepth 3 -name '*.crt' 2>/dev/null; done | head -n 200
echo "$M timers"; systemctl show certbot.timer snap.certbot.renew.timer -p Id -p LoadState -p ActiveState -p NextElapseUSecRealtime -p LastTriggerUSec 2>/dev/null
echo "$M cron"; grep -Hs certbot /etc/crontab /etc/cron.d/* /etc/periodic/*/* /var/spool/cron/crontabs/root /var/spool/cron/root 2>/dev/null | grep -Ev '^[^:]*:[[:space:]]*#' | head -n 20
echo "$M end"
`

var tsLayouts = []string{"Mon 2006-01-02 15:04:05 MST", "Mon 2006-01-02 15:04:05 -0700"}

func parseSystemdTime(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" || v == "0" {
		return 0
	}
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func parseTimers(out string) []RenewTimer {
	list := []RenewTimer{}
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		kv := map[string]string{}
		for _, l := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				kv[k] = v
			}
		}
		if kv["Id"] == "" || kv["LoadState"] != "loaded" {
			continue
		}
		list = append(list, RenewTimer{
			Unit: kv["Id"], Active: kv["ActiveState"] == "active",
			Next: parseSystemdTime(kv["NextElapseUSecRealtime"]), NextText: kv["NextElapseUSecRealtime"],
			Last: parseSystemdTime(kv["LastTriggerUSec"]),
		})
	}
	return list
}

// Certificates lists every certificate found on the server (certbot
// lineages, files referenced by nginx/caddy sites, custom certificates).
// It works without an open UI connection (used by monitoring).
func (s *WebService) Certificates(connID string, sudoPassword string) ([]Certificate, error) {
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	conn, err := s.core.AnyConn(ctx, connID)
	if err != nil {
		return []Certificate{}, err
	}
	ov, err := s.certOverview(ctx, conn, sudoPassword)
	if err != nil {
		return []Certificate{}, err
	}
	return ov.Certificates, nil
}

// CertOverview is the SSL tab: certificates plus auto-renewal status.
func (s *WebService) CertOverview(connID, sudoPassword string) (CertOverview, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return CertOverview{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	return s.certOverview(ctx, conn, sudoPassword)
}

func (s *WebService) certOverview(ctx context.Context, conn *sshx.Conn, pw string) (CertOverview, error) {
	ov := CertOverview{Certificates: []Certificate{}, Renewal: RenewalStatus{Timers: []RenewTimer{}, Cron: []string{}}}
	m := nonce()
	res, err := s.sudo(ctx, conn, pw, strings.Replace(certGather, "%s", m, 1), "")
	if err != nil {
		return ov, err
	}
	sec := sectionMap(splitSections(res.Stdout, m))
	ov.CertbotVersion = versionIn(sec["certbotver"])
	ov.Certbot = ov.CertbotVersion != "" || strings.TrimSpace(sec["certbot"]) != ""
	ov.PkgManager = strings.TrimSuffix(strings.TrimSpace(sec["pkg"]), "-get")
	ov.Renewal.Timers = parseTimers(sec["timers"])
	ov.Renewal.Cron = nonEmptyLines(sec["cron"])
	for _, t := range ov.Renewal.Timers {
		if t.Active {
			ov.Renewal.Automatic = true
		}
	}
	if len(ov.Renewal.Cron) > 0 {
		ov.Renewal.Automatic = true
	}

	byPath := map[string]*Certificate{}
	var order []string
	add := func(c Certificate) *Certificate {
		if c.Path == "" {
			return nil
		}
		if ex, ok := byPath[c.Path]; ok {
			return ex
		}
		if c.Domains == nil {
			c.Domains = []string{}
		}
		if c.UsedBy == nil {
			c.UsedBy = []string{}
		}
		cp := c
		byPath[c.Path] = &cp
		order = append(order, c.Path)
		return &cp
	}
	for _, c := range parseCertbotCertificates(sec["certbot"]) {
		add(c)
	}
	for _, p := range nonEmptyLines(sec["custom"]) {
		add(Certificate{Name: path.Base(path.Dir(p)), Source: "custom", Path: p, KeyPath: path.Join(path.Dir(p), "privkey.pem")})
	}
	for _, p := range nonEmptyLines(sec["caddyauto"]) {
		add(Certificate{Name: strings.TrimSuffix(path.Base(p), ".crt"), Source: "caddy", Path: p, KeyPath: strings.TrimSuffix(p, ".crt") + ".key"})
	}
	// Certificates referenced by sites.
	sites, err := s.sites(ctx, conn, pw)
	if err != nil {
		return ov, err
	}
	for _, st := range sites.Sites {
		for _, b := range st.Blocks {
			if b.SSLCert == "" || hasCtl(b.SSLCert) || strings.Contains(b.SSLCert, "$") {
				continue
			}
			src := st.Engine
			if strings.HasPrefix(b.SSLCert, customCertDir+"/") {
				src = "custom"
			}
			c := add(Certificate{Name: path.Base(path.Dir(b.SSLCert)), Source: src, Path: b.SSLCert, KeyPath: b.SSLKey})
			if c != nil {
				c.UsedBy = appendUnique(c.UsedBy, st.File)
			}
		}
	}
	// Read every certificate file with openssl (falling back to Go).
	if len(order) > 0 {
		if err := s.inspectFiles(ctx, conn, pw, order, byPath); err != nil {
			return ov, err
		}
	}
	now := time.Now()
	for _, p := range order {
		c := byPath[p]
		c.DaysLeft, c.Status = certStatus(c.NotAfter, now)
		if strings.Contains(c.Issuer, "STAGING") || strings.Contains(c.Issuer, "Fake LE") {
			c.Staging = true
		}
		ov.Certificates = append(ov.Certificates, *c)
	}
	sort.SliceStable(ov.Certificates, func(i, j int) bool {
		a, b := ov.Certificates[i], ov.Certificates[j]
		if (a.NotAfter == 0) != (b.NotAfter == 0) {
			return b.NotAfter == 0
		}
		return a.NotAfter < b.NotAfter
	})
	return ov, nil
}

func (s *WebService) inspectFiles(ctx context.Context, conn *sshx.Conn, pw string, paths []string, byPath map[string]*Certificate) error {
	m := nonce()
	var b strings.Builder
	b.WriteString("HAVE=; command -v openssl >/dev/null 2>&1 && HAVE=1\n")
	for _, p := range paths {
		q := core.Q(p)
		fmt.Fprintf(&b, "echo %s; if [ ! -r %s ]; then echo @@missing; elif [ -n \"$HAVE\" ]; then openssl x509 -in %s -noout -subject -issuer -startdate -enddate -serial -ext subjectAltName 2>/dev/null || openssl x509 -in %s -noout -subject -issuer -startdate -enddate -serial -text 2>&1; else echo @@pem; head -c 65536 %s; fi\n",
			core.Q(m+" cert "+p), q, q, q, q)
	}
	res, err := s.sudo(ctx, conn, pw, b.String(), "")
	if err != nil {
		return err
	}
	for _, sc := range splitSections(res.Stdout, m) {
		if sc.Name != "cert" {
			continue
		}
		c := byPath[sc.Arg]
		if c == nil {
			continue
		}
		body := strings.TrimSpace(sc.Body)
		switch {
		case strings.HasPrefix(body, "@@missing"):
			c.Error = "missing"
		case strings.HasPrefix(body, "@@pem"):
			if leaf := firstCert(strings.TrimPrefix(body, "@@pem")); leaf != nil {
				fillFromX509(c, leaf)
			} else {
				c.Error = "unreadable"
			}
		default:
			info, ok := parseOpenssl(body)
			if !ok {
				c.Error = "unreadable"
				continue
			}
			c.Subject = info.Subject["CN"]
			c.Issuer = dnDisplay(info.Issuer)
			if c.NotAfter == 0 || c.Source != "certbot" {
				c.NotAfter = info.NotAfter
			}
			c.NotBefore = info.NotBefore
			if c.Serial == "" {
				c.Serial = info.Serial
			}
			if len(c.Domains) == 0 {
				c.Domains = info.SANs
				if len(c.Domains) == 0 && c.Subject != "" {
					c.Domains = []string{c.Subject}
				}
			}
		}
	}
	return nil
}

func firstCert(pemText string) *x509.Certificate {
	rest := []byte(pemText)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return nil
		}
		if blk.Type == "CERTIFICATE" {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
				return c
			}
		}
	}
}

func fillFromX509(c *Certificate, x *x509.Certificate) {
	c.Subject = x.Subject.CommonName
	iss := map[string]string{"CN": x.Issuer.CommonName}
	if len(x.Issuer.Organization) > 0 {
		iss["O"] = x.Issuer.Organization[0]
	}
	c.Issuer = dnDisplay(iss)
	c.NotAfter, c.NotBefore = x.NotAfter.Unix(), x.NotBefore.Unix()
	c.Serial = strings.ToUpper(x.SerialNumber.Text(16))
	if len(c.Domains) == 0 {
		c.Domains = append([]string{}, x.DNSNames...)
		for _, ip := range x.IPAddresses {
			c.Domains = append(c.Domains, ip.String())
		}
	}
}

// ---- actions ----

// certUsers returns the enabled sites that reference files of a certbot
// lineage or custom certificate directory.
func (s *WebService) certUsers(ctx context.Context, conn *sshx.Conn, pw, dir string) ([]string, error) {
	sites, err := s.sites(ctx, conn, pw)
	if err != nil {
		return nil, err
	}
	users := []string{}
	for _, st := range sites.Sites {
		if !st.Enabled {
			continue
		}
		for _, b := range st.Blocks {
			if strings.HasPrefix(b.SSLCert, dir+"/") || strings.HasPrefix(b.SSLKey, dir+"/") {
				users = appendUnique(users, st.File)
			}
		}
		if st.Spec != nil && st.Spec.SSL == SSLLetsEncrypt && "/etc/letsencrypt/live/"+st.Spec.CertName == dir {
			users = appendUnique(users, st.File)
		}
	}
	return users, nil
}

var certbotReload = `if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then echo "systemctl reload nginx"; else echo "nginx -s reload"; fi`

// DeleteCertificate removes a certbot lineage (refused while a site uses it).
func (s *WebService) DeleteCertificate(connID, name, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return err
	}
	err := s.deleteCertificate(connID, name, sudoPassword)
	s.core.Audit(connID, "web.cert.delete", name, "certbot", err)
	return err
}

func (s *WebService) deleteCertificate(connID, name, pw string) error {
	if err := validCertName(name); err != nil {
		return err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	users, err := s.certUsers(ctx, conn, pw, "/etc/letsencrypt/live/"+name)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return apperr.New("web.certInUse", "sites", strings.Join(users, ", "))
	}
	_, err = s.core.RunOK(ctx, conn, pathPrefix+"certbot delete --non-interactive --cert-name "+core.Q(name)+" 2>&1", true, pw, "")
	return err
}

// DeleteCustomCertificate removes /etc/ssl/server-manager/<name>.
func (s *WebService) DeleteCustomCertificate(connID, name, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return err
	}
	err := s.deleteCustom(connID, name, sudoPassword)
	s.core.Audit(connID, "web.cert.deleteCustom", name, customCertDir+"/"+name, err)
	return err
}

func (s *WebService) deleteCustom(connID, name, pw string) error {
	if err := validCertName(name); err != nil {
		return err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	dir := customCertDir + "/" + name
	users, err := s.certUsers(ctx, conn, pw, dir)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return apperr.New("web.certInUse", "sites", strings.Join(users, ", "))
	}
	_, err = s.core.RunOK(ctx, conn, "rm -rf "+core.Q(dir), true, pw, "")
	return err
}

var jobKindRe = regexp.MustCompile(`^[a-zA-Z.]+$`)

// startCertJob runs a certbot command as a job, then reloads nginx.
func (s *WebService) startCertJob(connID, kind, title, target, detail, cmd, pw string, reload bool) (string, error) {
	if !jobKindRe.MatchString(kind) {
		return "", invalid("kind", kind)
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	j := s.core.Jobs.Start(connID, kind, title, func(ctx context.Context, j *core.Job) error {
		j.Step("%s", "certbot")
		j.Logf("$ %s", cmd)
		err := s.core.JobRun(ctx, j, conn, pathPrefix+cmd+" 2>&1", true, pw)
		if err == nil && reload {
			j.Step("reload")
			rctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			if r, _, rerr := s.reloadNginx(rctx, conn, pw); rerr != nil {
				j.Logf("reload: %v", rerr)
			} else if r {
				j.Logf("nginx reloaded")
			}
		}
		s.core.Audit(connID, kind, target, detail, err)
		switch {
		case kind == "web.cert.renew" && err == nil:
			s.core.AddEvent(core.Event{Server: connID, Kind: "action", Severity: "ok", Code: "web.certRenewed", Params: map[string]string{"name": target}})
		case kind == "web.cert.renew" && ctx.Err() == nil:
			s.core.AddEvent(core.Event{Server: connID, Kind: "alert", Severity: "warn", Code: "web.certRenewFailed", Params: map[string]string{"name": target}})
		}
		return err
	})
	return j.ID(), nil
}

// RenewCertificate runs `certbot renew --cert-name X` as a job.
func (s *WebService) RenewCertificate(connID, name string, force bool, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return "", err
	}
	if err := validCertName(name); err != nil {
		return "", err
	}
	cmd := "certbot renew --non-interactive --cert-name " + core.Q(name)
	detail := "certbot renew"
	if force {
		cmd += " --force-renewal"
		detail += " --force-renewal"
	}
	return s.startCertJob(connID, "web.cert.renew", "certbot renew "+name, name, detail, cmd, sudoPassword, true)
}

// TestRenewal runs `certbot renew --dry-run` (all certificates, or one).
func (s *WebService) TestRenewal(connID, name, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return "", err
	}
	cmd := "certbot renew --dry-run --non-interactive"
	target := "*"
	if name != "" {
		if err := validCertName(name); err != nil {
			return "", err
		}
		cmd += " --cert-name " + core.Q(name)
		target = name
	}
	return s.startCertJob(connID, "web.cert.renewTest", "certbot renew --dry-run", target, "dry run", cmd, sudoPassword, false)
}

// RevokeCertificate revokes a lineage at Let's Encrypt and deletes it.
func (s *WebService) RevokeCertificate(connID, name, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return "", err
	}
	if err := validCertName(name); err != nil {
		return "", err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(time.Minute)
	defer cancel()
	users, err := s.certUsers(ctx, conn, sudoPassword, "/etc/letsencrypt/live/"+name)
	if err != nil {
		return "", err
	}
	if len(users) > 0 {
		err := apperr.New("web.certInUse", "sites", strings.Join(users, ", "))
		s.core.Audit(connID, "web.cert.revoke", name, "", err)
		return "", err
	}
	cmd := "certbot revoke --non-interactive --cert-name " + core.Q(name) + " --delete-after-revoke"
	return s.startCertJob(connID, "web.cert.revoke", "certbot revoke "+name, name, "revoke + delete", cmd, sudoPassword, false)
}

// InstallCertbot installs certbot with the distro package manager (job).
func (s *WebService) InstallCertbot(connID, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return "", err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	st, err := s.status(ctx, conn, sudoPassword, false)
	if err != nil {
		return "", err
	}
	var cmd string
	switch st.PkgManager {
	case "apt":
		cmd = "export DEBIAN_FRONTEND=noninteractive; apt-get update && apt-get install -y certbot"
	case "dnf":
		cmd = "dnf install -y certbot || { dnf install -y epel-release && dnf install -y certbot; }"
	case "yum":
		cmd = "yum install -y certbot || { yum install -y epel-release && yum install -y certbot; }"
	case "apk":
		cmd = "apk add --no-cache certbot"
	case "zypper":
		cmd = "zypper --non-interactive install certbot"
	case "pacman":
		cmd = "pacman -S --noconfirm certbot"
	default:
		err := apperr.New("web.noPkgManager")
		s.core.Audit(connID, "web.certbot.install", "certbot", "", err)
		return "", err
	}
	j := s.core.Jobs.Start(connID, "web.certbot.install", "install certbot", func(ctx context.Context, j *core.Job) error {
		j.Step("%s", st.PkgManager)
		j.Logf("$ %s", cmd)
		err := s.core.JobRun(ctx, j, conn, pathPrefix+cmd+" 2>&1", true, sudoPassword)
		s.core.Audit(connID, "web.certbot.install", "certbot", st.PkgManager, err)
		return err
	})
	return j.ID(), nil
}

// IssueCertificate obtains a Let's Encrypt certificate for a managed nginx
// site (HTTP-01 through the shared webroot, so nginx keeps running), then
// regenerates the site with TLS enabled and reloads. Runs as a job.
func (s *WebService) IssueCertificate(connID, file, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return "", err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(time.Minute)
	defer cancel()
	content, envAny, err := s.knownFile(ctx, conn, sudoPassword, EngineNginx, file)
	if err != nil {
		return "", err
	}
	env := envAny.(*nginxEnv)
	_, spec := parseMeta(content)
	if spec == nil {
		return "", apperr.New("web.notManaged", "file", file)
	}
	sp := *spec
	sp.Engine = EngineNginx
	if err := sp.Normalize(); err != nil {
		return "", err
	}
	if sp.SSL != SSLLetsEncrypt {
		return "", apperr.New("web.notLetsEncrypt")
	}
	if r, err := s.plain(ctx, conn, "command -v certbot"); err != nil || strings.TrimSpace(r.Stdout) == "" {
		if err != nil {
			return "", err
		}
		return "", apperr.New("web.certbotMissing")
	}
	hook, _ := s.plain(ctx, conn, certbotReload)
	deploy := strings.TrimSpace(hook.Stdout)
	if deploy != "nginx -s reload" {
		deploy = "systemctl reload nginx"
	}
	args := []string{"certbot", "certonly", "--webroot", "-w", acmeWebroot, "--cert-name", core.Q(sp.CertName)}
	for _, d := range sp.Domains {
		args = append(args, "-d", core.Q(d))
	}
	args = append(args, "--email", core.Q(sp.Email), "--agree-tos", "--no-eff-email", "--non-interactive", "--keep-until-expiring",
		"--key-type", sp.KeyType, "--deploy-hook", core.Q(deploy))
	if sp.Staging {
		args = append(args, "--staging")
	}
	cmd := strings.Join(args, " ")
	title := "Let's Encrypt: " + sp.Domains[0]
	j := s.core.Jobs.Start(connID, "web.cert.issue", title, func(jctx context.Context, j *core.Job) error {
		err := s.issueJob(jctx, j, conn, connID, env, file, sp, cmd, sudoPassword)
		detail := fmt.Sprintf("%s; domains %s; key %s", sp.CertName, strings.Join(sp.Domains, " "), sp.KeyType)
		if sp.Staging {
			detail += "; staging"
		}
		s.core.Audit(connID, "web.cert.issue", sp.Domains[0], detail, err)
		if err == nil {
			s.core.AddEvent(core.Event{Server: connID, Kind: "action", Severity: "ok", Code: "web.certIssued", Params: map[string]string{"name": sp.Domains[0]}})
		} else if jctx.Err() == nil {
			s.core.AddEvent(core.Event{Server: connID, Kind: "action", Severity: "warn", Code: "web.certIssueFailed", Params: map[string]string{"name": sp.Domains[0]}, Detail: apperr.From(err).Error()})
		}
		return err
	})
	return j.ID(), nil
}

func (s *WebService) issueJob(ctx context.Context, j *core.Job, conn *sshx.Conn, connID string, env *nginxEnv, file string, sp SiteSpec, cmd, pw string) error {
	j.Step("webroot %s", acmeWebroot)
	if _, err := s.core.RunOK(ctx, conn, fmt.Sprintf("mkdir -p %s/.well-known/acme-challenge && chmod 755 %s", acmeWebroot, acmeWebroot), true, pw, ""); err != nil {
		return err
	}
	j.Step("certbot")
	j.Logf("$ %s", cmd)
	if err := s.core.JobRun(ctx, j, conn, pathPrefix+cmd+" 2>&1", true, pw); err != nil {
		return apperr.New("web.issueFailed").WithDetail(tail(j.Info().Log, 15))
	}
	j.Step("nginx: %s", file)
	res, err := s.applyNginx(ctx, conn, connID, file, sp, pw)
	if res.TestOutput != "" {
		j.Logf("%s", res.TestOutput)
	}
	if err != nil {
		return err
	}
	if res.Reloaded {
		j.Logf("nginx reloaded")
	}
	_ = env
	return nil
}
