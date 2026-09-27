package security

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// AuthorizedKey is one line of an authorized_keys file.
type AuthorizedKey struct {
	Line        int      `json:"line"`
	Type        string   `json:"type"`
	Bits        int      `json:"bits"`
	Comment     string   `json:"comment"`
	Fingerprint string   `json:"fingerprint"`
	Options     []string `json:"options"`
	Invalid     bool     `json:"invalid"`
	Raw         string   `json:"raw"` // only for invalid lines (truncated)
	// InUse: the current session authenticated with this key ("yes"), maybe
	// did (agent keys: "maybe") or not ("").
	InUse string `json:"inUse"`
}

type KeyList struct {
	User    string          `json:"user"`
	Home    string          `json:"home"`
	Path    string          `json:"path"`
	Exists  bool            `json:"exists"`
	Keys    []AuthorizedKey `json:"keys"`
	IsLogin bool            `json:"isLogin"`
	// Perm problems found (e.g. "~/.ssh 755", "owner root").
	Problems []string `json:"problems"`
	// PasswordAuth is sshd's effective PasswordAuthentication ("" if unknown).
	PasswordAuth string `json:"passwordAuth"`
}

// parseAuthorizedKeys parses an authorized_keys file.
func parseAuthorizedKeys(content string) []AuthorizedKey {
	out := []AuthorizedKey{}
	for i, raw := range strings.Split(content, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k := AuthorizedKey{Line: i + 1, Options: []string{}}
		pub, comment, opts, _, err := ssh.ParseAuthorizedKey([]byte(l))
		if err != nil {
			k.Invalid = true
			r := []rune(l)
			if len(r) > 80 {
				r = append(r[:77], '…')
			}
			k.Raw = string(r)
			out = append(out, k)
			continue
		}
		k.Type = pub.Type()
		k.Bits = keyBits(pub)
		k.Comment = comment
		k.Fingerprint = ssh.FingerprintSHA256(pub)
		if opts != nil {
			k.Options = opts
		}
		out = append(out, k)
	}
	return out
}

func keyBits(pub ssh.PublicKey) int {
	switch pub.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoSKED25519:
		return 256
	}
	if cp, ok := pub.(ssh.CryptoPublicKey); ok {
		switch k := cp.CryptoPublicKey().(type) {
		case *rsa.PublicKey:
			return k.N.BitLen()
		case *ecdsa.PublicKey:
			return k.Curve.Params().BitSize
		}
	}
	return 0
}

// normalizeKey validates a pasted public key line and returns it in
// canonical form ("[options ]type base64 comment") with its fingerprint.
func normalizeKey(line string) (string, string, error) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", "", apperr.New("sec.keys.invalid")
	}
	if len(line) > 16384 {
		return "", "", apperr.New("sec.keys.invalid")
	}
	pub, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return "", "", apperr.New("sec.keys.invalid")
	}
	if pub.Type() == ssh.KeyAlgoDSA {
		return "", "", apperr.New("sec.keys.weak", "type", pub.Type())
	}
	if b := keyBits(pub); strings.HasPrefix(pub.Type(), "ssh-rsa") && b > 0 && b < 2048 {
		return "", "", apperr.New("sec.keys.weak", "type", "RSA "+strconv.Itoa(b))
	}
	var b strings.Builder
	for i, o := range opts {
		if reCtl.MatchString(o) {
			return "", "", apperr.New("sec.keys.invalid")
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(o)
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))))
	if c := strings.TrimSpace(reCtl.ReplaceAllString(comment, "")); c != "" {
		if r := []rune(c); len(r) > 200 {
			c = string(r[:200])
		}
		b.WriteString(" " + c)
	}
	return b.String(), ssh.FingerprintSHA256(pub), nil
}

// sessionKeys returns the fingerprints of the key(s) this session may have
// authenticated with; certain is true when there is exactly one candidate.
func sessionKeys(sv store.Server, passphrase string) (fps []string, certain bool) {
	switch sv.AuthType {
	case store.AuthKey:
		p := expandHome(sv.KeyPath)
		if b, err := os.ReadFile(p + ".pub"); err == nil {
			if pub, _, _, _, err := ssh.ParseAuthorizedKey(b); err == nil {
				return []string{ssh.FingerprintSHA256(pub)}, true
			}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, false
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil && passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(b, []byte(passphrase))
		}
		if err != nil {
			return nil, false
		}
		return []string{ssh.FingerprintSHA256(signer.PublicKey())}, true
	case store.AuthAgent:
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, false
		}
		c, err := net.DialTimeout("unix", sock, 2*time.Second)
		if err != nil {
			return nil, false
		}
		defer c.Close()
		keys, err := agent.NewClient(c).List()
		if err != nil {
			return nil, false
		}
		for _, k := range keys {
			fps = append(fps, ssh.FingerprintSHA256(k))
		}
		return fps, len(fps) == 1
	}
	return nil, false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

// ---- reading ----

// keyTarget resolves the account whose keys are managed.
type keyTarget struct {
	User    string
	Home    string
	Group   string // primary group name/gid for chown
	IsLogin bool
}

func (s *SecurityService) keyTarget(ctx context.Context, conn *sshx.Conn, user string) (keyTarget, error) {
	login := loginUser(conn)
	if user == "" {
		user = login
	}
	if err := existingUser(user); err != nil {
		return keyTarget{}, err
	}
	res, err := s.user(ctx, conn, `e=$(getent passwd `+core.Q(user)+` 2>/dev/null || awk -F: -v u=`+core.Q(user)+` '$1==u' /etc/passwd | head -n1); echo "$e"; [ -n "$e" ] && id -g `+core.Q(user), "")
	if err != nil {
		return keyTarget{}, err
	}
	ls := lines(res.Stdout)
	if len(ls) < 1 {
		return keyTarget{}, apperr.New("sec.users.notFound", "name", user)
	}
	f := strings.Split(ls[0], ":")
	if len(f) < 7 || f[0] != user {
		return keyTarget{}, apperr.New("sec.users.notFound", "name", user)
	}
	t := keyTarget{User: user, Home: f[5], Group: f[3], IsLogin: user == login}
	if len(ls) > 1 {
		t.Group = strings.TrimSpace(ls[1])
	}
	if !strings.HasPrefix(t.Home, "/") || t.Home == "/" || strings.ContainsAny(t.Home, "\n") {
		return keyTarget{}, apperr.New("sec.keys.noHome", "user", user)
	}
	return t, nil
}

// asTarget runs cmd as the login user when the target is the login user,
// otherwise as root.
func (s *SecurityService) asTarget(ctx context.Context, conn *sshx.Conn, t keyTarget, cmd, pw, stdin string) (sshx.ExecResult, error) {
	if t.IsLogin {
		return s.user(ctx, conn, cmd, stdin)
	}
	return s.root(ctx, conn, cmd, pw, stdin)
}

const keysReadScript = `D="$H/.ssh"; F="$D/authorized_keys"
echo @@stat; stat -c '%a %U %n' "$D" "$F" 2>/dev/null || ls -ldn "$D" "$F" 2>/dev/null
echo @@keys; [ -f "$F" ] && { echo EXISTS; cat "$F"; }
echo @@end`

func (s *SecurityService) readKeysAs(ctx context.Context, conn *sshx.Conn, user, pw string) ([]AuthorizedKey, error) {
	kl, err := s.readKeys(ctx, conn, user, pw)
	return kl.Keys, err
}

func (s *SecurityService) readKeys(ctx context.Context, conn *sshx.Conn, user, pw string) (KeyList, error) {
	t, err := s.keyTarget(ctx, conn, user)
	if err != nil {
		return KeyList{}, err
	}
	res, err := s.asTarget(ctx, conn, t, "H="+core.Q(t.Home)+"; "+keysReadScript, pw, "")
	if err != nil {
		return KeyList{}, err
	}
	sec := sections(res.Stdout)
	kl := KeyList{User: t.User, Home: t.Home, Path: t.Home + "/.ssh/authorized_keys", IsLogin: t.IsLogin, Keys: []AuthorizedKey{}, Problems: []string{}}
	body := sec["keys"]
	if rest, ok := strings.CutPrefix(body, "EXISTS\n"); ok {
		kl.Exists = true
		kl.Keys = parseAuthorizedKeys(rest)
	}
	for _, l := range lines(sec["stat"]) {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		mode, owner, p := f[0], f[1], f[len(f)-1]
		if len(mode) > 4 || strings.ContainsAny(mode, "rwx-") { // ls fallback: skip
			continue
		}
		if owner != t.User {
			kl.Problems = append(kl.Problems, p+" owner="+owner)
		}
		m := 0
		for _, c := range mode {
			m = m*8 + int(c-'0')
		}
		if m&0o022 != 0 {
			kl.Problems = append(kl.Problems, p+" mode="+mode)
		}
	}
	return kl, nil
}

// ListKeys lists the authorized keys of user ("" = the login user).
func (s *SecurityService) ListKeys(connID, user, sudoPassword string) (KeyList, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return KeyList{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	kl, err := s.readKeys(ctx, conn, user, sudoPassword)
	if err != nil {
		return kl, err
	}
	s.markSessionKey(conn, &kl)
	// Best effort: sshd's password setting (needs root; skipped without).
	if r, err := s.root(ctx, conn, "sshd -T 2>/dev/null | grep -i '^passwordauthentication '", sudoPassword, ""); err == nil {
		if f := strings.Fields(r.Stdout); len(f) == 2 {
			kl.PasswordAuth = f[1]
		}
	}
	return kl, nil
}

func (s *SecurityService) markSessionKey(conn *sshx.Conn, kl *KeyList) {
	if !kl.IsLogin {
		return
	}
	sv := conn.Server()
	fps, certain := sessionKeys(sv, s.core.Store.Secret(sv.ID))
	for i := range kl.Keys {
		if slices.Contains(fps, kl.Keys[i].Fingerprint) {
			if certain {
				kl.Keys[i].InUse = "yes"
			} else {
				kl.Keys[i].InUse = "maybe"
			}
		}
	}
}

// ---- writing ----

// keysWriteScript replaces authorized_keys with stdin, creating ~/.ssh
// (700) and the file (600) owned by the user; refuses symlinks.
func keysWriteScript(t keyTarget, asRoot bool) string {
	chown := ""
	if asRoot {
		chown = `chown ` + core.Q(t.User) + `:` + core.Q(t.Group) + ` "$D" "$T" || { rm -f "$T"; exit 4; }`
	}
	return `H=` + core.Q(t.Home) + `; D="$H/.ssh"; F="$D/authorized_keys"
[ -d "$H" ] || { echo "@@nohome"; exit 5; }
[ -L "$D" ] || [ -L "$F" ] && { echo "@@symlink"; exit 6; }
[ -d "$D" ] || mkdir -m 700 "$D" || exit 3
chmod 700 "$D" || exit 3
T=$(mktemp "$D/.authorized_keys.XXXXXX") || exit 3
cat > "$T" || { rm -f "$T"; exit 3; }
chmod 600 "$T"
` + chown + `
mv -f "$T" "$F" || { rm -f "$T"; exit 3; }
command -v restorecon >/dev/null 2>&1 && restorecon -R "$D" >/dev/null 2>&1
echo @@written`
}

func (s *SecurityService) writeKeys(ctx context.Context, conn *sshx.Conn, t keyTarget, content, pw string) error {
	asRoot := !t.IsLogin
	var res sshx.ExecResult
	var err error
	if asRoot {
		res, err = s.root(ctx, conn, keysWriteScript(t, true), pw, content)
	} else {
		res, err = s.user(ctx, conn, keysWriteScript(t, false), content)
	}
	if err != nil {
		return err
	}
	switch {
	case strings.Contains(res.Stdout, "@@written"):
		return nil
	case strings.Contains(res.Stdout, "@@symlink"):
		return apperr.New("sec.keys.symlink", "user", t.User)
	case strings.Contains(res.Stdout, "@@nohome"):
		return apperr.New("sec.keys.noHome", "user", t.User)
	}
	return apperr.New("sec.keys.writeFailed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
}

// rawKeys reads the current file content (for read-modify-write).
func (s *SecurityService) rawKeys(ctx context.Context, conn *sshx.Conn, t keyTarget, pw string) (string, error) {
	res, err := s.asTarget(ctx, conn, t, `F=`+core.Q(t.Home+"/.ssh/authorized_keys")+`; [ -L "$F" ] && { echo @@symlink; exit 0; }; [ -f "$F" ] && { echo @@content; cat "$F"; }; true`, pw, "")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(res.Stdout, "@@symlink") {
		return "", apperr.New("sec.keys.symlink", "user", t.User)
	}
	if res.ExitCode != 0 {
		return "", core.CmdError(res)
	}
	return strings.TrimPrefix(res.Stdout, "@@content\n"), nil
}

// AddKey appends a public key to user's authorized_keys ("" = login user).
func (s *SecurityService) AddKey(connID, user, key, sudoPassword string) (AuthorizedKey, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return AuthorizedKey{}, err
	}
	k, target, err := s.addKey(connID, user, key, sudoPassword)
	s.core.Audit(connID, "sec.keys.add", target, k.Type+" "+k.Fingerprint+" "+k.Comment, err)
	return k, err
}

func (s *SecurityService) addKey(connID, user, key, pw string) (AuthorizedKey, string, error) {
	line, fp, err := normalizeKey(key)
	if err != nil {
		return AuthorizedKey{}, user, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return AuthorizedKey{}, user, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	t, err := s.keyTarget(ctx, conn, user)
	if err != nil {
		return AuthorizedKey{}, user, err
	}
	cur, err := s.rawKeys(ctx, conn, t, pw)
	if err != nil {
		return AuthorizedKey{}, t.User, err
	}
	for _, k := range parseAuthorizedKeys(cur) {
		if k.Fingerprint == fp {
			return k, t.User, apperr.New("sec.keys.duplicate", "fingerprint", fp)
		}
	}
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		cur += "\n"
	}
	if err := s.writeKeys(ctx, conn, t, cur+line+"\n", pw); err != nil {
		return AuthorizedKey{}, t.User, err
	}
	added := parseAuthorizedKeys(line)
	if len(added) == 1 {
		return added[0], t.User, nil
	}
	return AuthorizedKey{Fingerprint: fp}, t.User, nil
}

// RemoveKey removes every line with the given fingerprint. It refuses the
// key the current session authenticated with, and removing the last key
// while sshd has password login disabled unless confirmLast is set.
func (s *SecurityService) RemoveKey(connID, user, fingerprint string, confirmLast bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	target, detail, err := s.removeKey(connID, user, fingerprint, confirmLast, sudoPassword)
	s.core.Audit(connID, "sec.keys.remove", target, detail, err)
	return err
}

func (s *SecurityService) removeKey(connID, user, fp string, confirmLast bool, pw string) (string, string, error) {
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) > 80 || reCtl.MatchString(fp) {
		return user, fp, apperr.New("sec.keys.invalid")
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return user, fp, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	t, err := s.keyTarget(ctx, conn, user)
	if err != nil {
		return user, fp, err
	}
	cur, err := s.rawKeys(ctx, conn, t, pw)
	if err != nil {
		return t.User, fp, err
	}
	keys := parseAuthorizedKeys(cur)
	kl := KeyList{User: t.User, IsLogin: t.IsLogin, Keys: keys}
	s.markSessionKey(conn, &kl)
	found, valid := false, 0
	detail := fp
	for _, k := range kl.Keys {
		if k.Invalid {
			continue
		}
		valid++
		if k.Fingerprint == fp {
			found = true
			detail = k.Type + " " + fp + " " + k.Comment
			if k.InUse == "yes" {
				return t.User, detail, apperr.New("sec.keys.inUse")
			}
		}
	}
	if !found {
		return t.User, detail, apperr.New("sec.keys.notFound")
	}
	if valid == 1 && !confirmLast {
		r, _ := s.root(ctx, conn, "sshd -T 2>/dev/null | grep -i '^passwordauthentication '", pw, "")
		if f := strings.Fields(r.Stdout); len(f) == 2 && f[1] == "no" {
			return t.User, detail, apperr.New("sec.keys.lastKey", "user", t.User)
		}
	}
	var b strings.Builder
	for i, raw := range strings.Split(strings.TrimSuffix(cur, "\n"), "\n") {
		drop := false
		for _, k := range keys {
			if k.Line == i+1 && !k.Invalid && k.Fingerprint == fp {
				drop = true
			}
		}
		if !drop {
			b.WriteString(raw + "\n")
		}
	}
	return t.User, detail, s.writeKeys(ctx, conn, t, b.String(), pw)
}
