package web

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
)

// ---- live check (from this computer) ----

// LiveCert is what a TLS handshake with domain:443 returned.
type LiveCert struct {
	Domain      string   `json:"domain"`
	OK          bool     `json:"ok"`          // handshake done and chain valid for the name
	Error       string   `json:"error"`       // connection/handshake error
	VerifyError string   `json:"verifyError"` // chain/name verification error
	Issuer      string   `json:"issuer"`
	Subject     string   `json:"subject"`
	Names       []string `json:"names"`
	NotAfter    int64    `json:"notAfter"`
	DaysLeft    int      `json:"daysLeft"`
	Status      string   `json:"status"`
}

const liveTimeout = 8 * time.Second

// LiveCheck connects to each domain on port 443 with SNI (8 s timeout, at
// most 8 at a time) and reports the certificate actually served.
func (s *WebService) LiveCheck(domains []string) ([]LiveCert, error) {
	if len(domains) > 200 {
		domains = domains[:200]
	}
	out := make([]LiveCert, len(domains))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, d := range domains {
		host, err := normalizeHost(d, false)
		if err != nil || net.ParseIP(host) != nil {
			out[i] = LiveCert{Domain: d, Error: "invalid", Names: []string{}, Status: "unknown"}
			continue
		}
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = liveCheck(host)
		}(i, host)
	}
	wg.Wait()
	return out, nil
}

func liveCheck(host string) LiveCert {
	lc := LiveCert{Domain: host, Names: []string{}, Status: "unknown"}
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: liveTimeout},
		// Verification is done below so the certificate can be reported
		// even when it is invalid.
		Config: &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS10},
	}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		lc.Error = err.Error()
		return lc
	}
	defer c.Close()
	st := c.(*tls.Conn).ConnectionState()
	if len(st.PeerCertificates) == 0 {
		lc.Error = "no certificate"
		return lc
	}
	leaf := st.PeerCertificates[0]
	lc.Subject = leaf.Subject.CommonName
	iss := map[string]string{"CN": leaf.Issuer.CommonName}
	if len(leaf.Issuer.Organization) > 0 {
		iss["O"] = leaf.Issuer.Organization[0]
	}
	lc.Issuer = dnDisplay(iss)
	lc.Names = append(lc.Names, leaf.DNSNames...)
	lc.NotAfter = leaf.NotAfter.Unix()
	lc.DaysLeft, lc.Status = certStatus(lc.NotAfter, time.Now())
	inter := x509.NewCertPool()
	for _, ic := range st.PeerCertificates[1:] {
		inter.AddCert(ic)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter}); err != nil {
		lc.VerifyError = err.Error()
		if lc.Status == "ok" || lc.Status == "warn" {
			lc.Status = "err"
		}
	} else {
		lc.OK = true
	}
	return lc
}

// ---- custom certificates ----

// CertInspect describes a pasted certificate + key (validated locally).
type CertInspect struct {
	Subject     string   `json:"subject"`
	Issuer      string   `json:"issuer"`
	Domains     []string `json:"domains"`
	NotBefore   int64    `json:"notBefore"`
	NotAfter    int64    `json:"notAfter"`
	DaysLeft    int      `json:"daysLeft"`
	Status      string   `json:"status"`
	ChainLen    int      `json:"chainLen"`
	SelfSigned  bool     `json:"selfSigned"`
	KeyType     string   `json:"keyType"`
	Fingerprint string   `json:"fingerprint"` // SHA-256 of the leaf
	Warnings    []string `json:"warnings"`    // noIntermediate | notYetValid | expired | selfSigned
}

type parsedPair struct {
	chainPEM []byte // leaf first, normalized
	keyPEM   []byte // PKCS#8
	info     CertInspect
}

// parseCertPair validates a PEM chain and private key. Key material is
// never included in errors.
func parseCertPair(chain, key string) (*parsedPair, error) {
	if len(chain) > 64<<10 || len(key) > 16<<10 {
		return nil, apperr.New("web.pemTooLarge")
	}
	var certs []*x509.Certificate
	var keyBlocks []*pem.Block
	rest := []byte(strings.TrimSpace(chain) + "\n" + strings.TrimSpace(key))
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch {
		case b.Type == "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, apperr.New("web.certParse").WithDetail(err.Error())
			}
			certs = append(certs, c)
		case b.Type == "ENCRYPTED PRIVATE KEY" || b.Headers["Proc-Type"] != "":
			return nil, apperr.New("web.keyEncrypted")
		case strings.HasSuffix(b.Type, "PRIVATE KEY"):
			keyBlocks = append(keyBlocks, b)
		}
	}
	if len(certs) == 0 {
		return nil, apperr.New("web.certParse")
	}
	if len(keyBlocks) != 1 {
		return nil, apperr.New("web.keyParse")
	}
	priv, err := parsePrivateKey(keyBlocks[0])
	if err != nil {
		return nil, apperr.New("web.keyParse")
	}
	// The leaf is the certificate matching the key.
	leafIdx := -1
	for i, c := range certs {
		if pubEqual(c.PublicKey, priv) {
			leafIdx = i
			break
		}
	}
	if leafIdx < 0 {
		return nil, apperr.New("web.keyMismatch")
	}
	ordered := append([]*x509.Certificate{certs[leafIdx]}, append(append([]*x509.Certificate{}, certs[:leafIdx]...), certs[leafIdx+1:]...)...)
	var cb bytes.Buffer
	for _, c := range ordered {
		_ = pem.Encode(&cb, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, apperr.New("web.keyParse")
	}
	kb := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := tls.X509KeyPair(cb.Bytes(), kb); err != nil {
		return nil, apperr.New("web.keyMismatch")
	}
	leaf := ordered[0]
	info := CertInspect{
		Subject: leaf.Subject.CommonName, Domains: append([]string{}, leaf.DNSNames...),
		NotBefore: leaf.NotBefore.Unix(), NotAfter: leaf.NotAfter.Unix(), ChainLen: len(ordered), Warnings: []string{},
	}
	for _, ip := range leaf.IPAddresses {
		info.Domains = append(info.Domains, ip.String())
	}
	iss := map[string]string{"CN": leaf.Issuer.CommonName}
	if len(leaf.Issuer.Organization) > 0 {
		iss["O"] = leaf.Issuer.Organization[0]
	}
	info.Issuer = dnDisplay(iss)
	sum := sha256.Sum256(leaf.Raw)
	info.Fingerprint = strings.ToUpper(hex.EncodeToString(sum[:]))
	info.DaysLeft, info.Status = certStatus(info.NotAfter, time.Now())
	info.SelfSigned = bytes.Equal(leaf.RawIssuer, leaf.RawSubject) && leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		info.KeyType = fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PrivateKey:
		info.KeyType = "ECDSA " + k.Curve.Params().Name
	case ed25519.PrivateKey:
		info.KeyType = "Ed25519"
	}
	if info.SelfSigned {
		info.Warnings = append(info.Warnings, "selfSigned")
	} else {
		hasIssuer := false
		for _, c := range ordered[1:] {
			if bytes.Equal(c.RawSubject, leaf.RawIssuer) {
				hasIssuer = true
			}
		}
		if !hasIssuer {
			info.Warnings = append(info.Warnings, "noIntermediate")
		}
	}
	if time.Now().Before(leaf.NotBefore) {
		info.Warnings = append(info.Warnings, "notYetValid")
	}
	if info.Status == "expired" {
		info.Warnings = append(info.Warnings, "expired")
	}
	return &parsedPair{chainPEM: cb.Bytes(), keyPEM: kb, info: info}, nil
}

func parsePrivateKey(b *pem.Block) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(b.Bytes); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
	}
	if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	return nil, apperr.New("web.keyParse")
}

func pubEqual(pub any, priv crypto.Signer) bool {
	type eq interface{ Equal(crypto.PublicKey) bool }
	if e, ok := priv.Public().(eq); ok {
		return e.Equal(pub)
	}
	return false
}

// InspectCertificate validates a pasted PEM chain and key locally (nothing
// is sent to the server) and describes the certificate.
func (s *WebService) InspectCertificate(chainPEM, keyPEM string) (CertInspect, error) {
	p, err := parseCertPair(chainPEM, keyPEM)
	if err != nil {
		return CertInspect{}, err
	}
	return p.info, nil
}

// InstallCertificate writes a validated certificate to
// /etc/ssl/server-manager/<name>/{fullchain,privkey}.pem (root, 0600 in a
// 0700 directory). replace allows overwriting an existing one (renewal);
// nginx/caddy are then reloaded so they pick it up.
func (s *WebService) InstallCertificate(connID, name, chainPEM, keyPEM string, replace bool, sudoPassword string) (CertInspect, error) {
	if err := s.core.Require(connID, core.PermWeb); err != nil {
		return CertInspect{}, err
	}
	info, err := s.installCertificate(connID, name, chainPEM, keyPEM, replace, sudoPassword)
	detail := customCertDir + "/" + name
	if err == nil {
		detail += fmt.Sprintf("; %s; expires %s; sha256 %s", strings.Join(info.Domains, " "), time.Unix(info.NotAfter, 0).UTC().Format("2006-01-02"), info.Fingerprint[:16])
	}
	s.core.Audit(connID, "web.cert.install", name, detail, err)
	return info, err
}

func (s *WebService) installCertificate(connID, name, chainPEM, keyPEM string, replace bool, pw string) (CertInspect, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := validCertName(name); err != nil {
		return CertInspect{}, err
	}
	p, err := parseCertPair(chainPEM, keyPEM)
	if err != nil {
		return CertInspect{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return CertInspect{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	dir := customCertDir + "/" + name
	D := core.Q(dir)
	res, err := s.sudo(ctx, conn, pw, fmt.Sprintf(`if [ -e %[1]s/fullchain.pem ]; then echo EXISTS; fi; umask 077; mkdir -p %[2]s && chmod 711 %[2]s && mkdir -p %[1]s && chmod 700 %[1]s && chown root:root %[1]s && rm -f %[1]s/.fullchain.tmp %[1]s/.privkey.tmp`, D, customCertDir), "")
	if err != nil {
		return CertInspect{}, err
	}
	if res.ExitCode != 0 {
		return CertInspect{}, apperr.New("web.applyFailed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	existed := strings.Contains(res.Stdout, "EXISTS")
	if existed && !replace {
		return CertInspect{}, apperr.New("web.certExists", "name", name)
	}
	// Each file goes through its own stdin; the key never appears in a
	// command line or in logs.
	for _, f := range []struct {
		name string
		data []byte
	}{{".fullchain.tmp", p.chainPEM}, {".privkey.tmp", p.keyPEM}} {
		r, err := s.sudo(ctx, conn, pw, "umask 077; cat > "+core.Q(dir+"/"+f.name), string(f.data))
		if err == nil && r.ExitCode != 0 {
			err = apperr.New("web.applyFailed").WithDetail(core.FirstNonEmpty(r.Stderr, r.Stdout))
		}
		if err != nil {
			_, _ = s.sudo(context.Background(), conn, pw, "rm -f "+D+"/.fullchain.tmp "+D+"/.privkey.tmp", "")
			return CertInspect{}, err
		}
	}
	// Keep caddy's group access if a caddy site already uses this cert.
	commit := fmt.Sprintf(`G=root; [ "$(stat -c %%G %[1]s/privkey.pem 2>/dev/null)" = caddy ] && G=caddy
M=600; [ "$G" = caddy ] && M=640
chown root:"$G" %[1]s/.fullchain.tmp %[1]s/.privkey.tmp && chmod "$M" %[1]s/.fullchain.tmp %[1]s/.privkey.tmp &&
mv -f %[1]s/.fullchain.tmp %[1]s/fullchain.pem && mv -f %[1]s/.privkey.tmp %[1]s/privkey.pem`, D)
	if _, err := s.core.RunOK(ctx, conn, commit, true, pw, ""); err != nil {
		_, _ = s.sudo(context.Background(), conn, pw, "rm -f "+D+"/.fullchain.tmp "+D+"/.privkey.tmp", "")
		return CertInspect{}, err
	}
	if existed {
		// Servers keep the old certificate in memory until reloaded.
		if r, _ := s.sudo(ctx, conn, pw, "command -v nginx >/dev/null 2>&1 && nginx -t >/dev/null 2>&1 && echo OK", ""); strings.Contains(r.Stdout, "OK") {
			_, _, _ = s.reloadNginx(ctx, conn, pw)
		}
		if r, _ := s.sudo(ctx, conn, pw, "command -v caddy >/dev/null 2>&1 && echo OK", ""); strings.Contains(r.Stdout, "OK") {
			_, _, _ = s.reloadCaddy(ctx, conn, pw)
		}
	}
	return p.info, nil
}
