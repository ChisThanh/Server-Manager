package security

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

const (
	defaultSSHDConfig = "/etc/ssh/sshd_config"
	dropinName        = "00-server-manager.conf"
	blockBegin        = "# BEGIN server-manager (managed block, edit from the app)"
	blockEnd          = "# END server-manager"
	disabledMark      = "#[server-manager] "
)

// SSHConfig is the effective sshd configuration (from `sshd -T`) plus the
// context the UI needs to change it safely.
type SSHConfig struct {
	Version                      string   `json:"version"`
	Ports                        []int    `json:"ports"`
	ListenAddresses              []string `json:"listenAddresses"`
	PermitRootLogin              string   `json:"permitRootLogin"`
	PasswordAuthentication       string   `json:"passwordAuthentication"`
	PubkeyAuthentication         string   `json:"pubkeyAuthentication"`
	KbdInteractiveAuthentication string   `json:"kbdInteractiveAuthentication"`
	UsePAM                       string   `json:"usePam"`
	MaxAuthTries                 int      `json:"maxAuthTries"`
	AllowUsers                   []string `json:"allowUsers"`
	AllowGroups                  []string `json:"allowGroups"`
	DenyUsers                    []string `json:"denyUsers"`
	DenyGroups                   []string `json:"denyGroups"`
	X11Forwarding                string   `json:"x11Forwarding"`
	ClientAliveInterval          int      `json:"clientAliveInterval"`
	ClientAliveCountMax          int      `json:"clientAliveCountMax"`
	AuthorizedKeysFile           string   `json:"authorizedKeysFile"`

	// Mode is "dropin" (sshd_config includes sshd_config.d first; the app
	// writes ConfigPath = sshd_config.d/00-server-manager.conf) or "main"
	// (a managed block at the top of sshd_config).
	Mode       string `json:"mode"`
	ConfigPath string `json:"configPath"`
	// Managed lists the directives currently set by the app.
	Managed map[string][]string `json:"managed"`
	// Files are the config files that were read (for display).
	Files []string `json:"files"`

	SessionPort   int    `json:"sessionPort"`
	LoginUser     string `json:"loginUser"`
	LoginIsRoot   bool   `json:"loginIsRoot"`
	AuthType      string `json:"authType"` // password | key | agent
	LoginKeyCount int    `json:"loginKeyCount"`
	SELinux       string `json:"selinux"` // Enforcing | Permissive | Disabled | ""
}

// SSHConfirm carries the user's explicit acknowledgements for risky changes.
type SSHConfirm struct {
	NoKeys            bool `json:"noKeys"`            // disable password login although no key is installed
	RootLogin         bool `json:"rootLogin"`         // restrict root login while connected as root
	PubkeySelf        bool `json:"pubkeySelf"`        // disable public keys while connected with a key
	NewPorts          bool `json:"newPorts"`          // new ports are allowed by the firewall
	RemoveCurrentPort bool `json:"removeCurrentPort"` // stop listening on the port of this session
	Lockout           bool `json:"lockout"`           // AllowUsers/AllowGroups exclude the login user
}

// SSHUpdate sets directives: keyword (case-insensitive) → values. An empty
// list removes the app's directive (the server default or other files apply).
type SSHUpdate struct {
	Set     map[string][]string `json:"set"`
	Confirm SSHConfirm          `json:"confirm"`
}

type SSHUpdateResult struct {
	Config SSHConfig `json:"config"`
	// Overridden lists directives whose effective value differs from what
	// was written (another file wins).
	Overridden []string `json:"overridden"`
	// Listening are the ports sshd listens on after the reload.
	Listening []int  `json:"listening"`
	Reloaded  bool   `json:"reloaded"`
	Backup    string `json:"backup"`
}

// managedKeys are the directives the app may write, in output order, with
// their canonical spelling.
var managedKeys = []string{"Port", "PermitRootLogin", "PasswordAuthentication", "KbdInteractiveAuthentication",
	"PubkeyAuthentication", "MaxAuthTries", "AllowUsers", "AllowGroups", "X11Forwarding", "ClientAliveInterval", "ClientAliveCountMax"}

// cumulative directives add up across lines/files instead of "first wins".
var cumulativeKeys = map[string]bool{"port": true, "allowusers": true, "allowgroups": true}

func canonicalKey(k string) (string, bool) {
	for _, m := range managedKeys {
		if strings.EqualFold(m, k) {
			return m, true
		}
	}
	return "", false
}

var (
	reSSHPattern = regexp.MustCompile(`^[A-Za-z0-9_.*?@%:/!\[\]-]{1,64}$`)
	yesNo        = []string{"yes", "no"}
	rootValues   = []string{"yes", "no", "prohibit-password", "forced-commands-only"}
)

// validateDirective checks and normalizes the values of one directive.
func validateDirective(key string, vals []string) ([]string, error) {
	bad := func(v string) error { return apperr.New("sec.ssh.invalidValue", "key", key, "value", v) }
	out := []string{}
	one := func() (string, error) {
		if len(vals) != 1 {
			return "", bad(strings.Join(vals, " "))
		}
		return strings.TrimSpace(vals[0]), nil
	}
	num := func(lo, hi int) error {
		v, err := one()
		if err != nil {
			return err
		}
		n, e := strconv.Atoi(v)
		if e != nil || n < lo || n > hi {
			return bad(v)
		}
		out = append(out, strconv.Itoa(n))
		return nil
	}
	enum := func(allowed []string) error {
		v, err := one()
		if err != nil {
			return err
		}
		v = strings.ToLower(v)
		if v == "without-password" {
			v = "prohibit-password"
		}
		if !slices.Contains(allowed, v) {
			return bad(v)
		}
		out = append(out, v)
		return nil
	}
	switch key {
	case "Port":
		seen := map[int]bool{}
		for _, v := range vals {
			n, e := strconv.Atoi(strings.TrimSpace(v))
			if e != nil || !validPort(n) {
				return nil, apperr.New("sec.invalidPort", "port", v)
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, strconv.Itoa(n))
			}
		}
		if len(out) > 8 {
			return nil, bad(strings.Join(vals, " "))
		}
	case "PermitRootLogin":
		return out, enum(rootValues)
	case "PasswordAuthentication", "KbdInteractiveAuthentication", "PubkeyAuthentication", "X11Forwarding":
		return out, enum(yesNo)
	case "MaxAuthTries":
		return out, num(1, 100)
	case "ClientAliveInterval":
		return out, num(0, 86400)
	case "ClientAliveCountMax":
		return out, num(0, 1000)
	case "AllowUsers", "AllowGroups":
		for _, v := range vals {
			for _, f := range strings.Fields(v) {
				if !reSSHPattern.MatchString(f) {
					return nil, bad(f)
				}
				out = append(out, f)
			}
		}
		if len(out) > 64 {
			return nil, bad("…")
		}
	default:
		return nil, apperr.New("sec.ssh.unsupportedKey", "key", key)
	}
	return out, nil
}

// ---- reading ----

type sshFile struct {
	Path    string
	Content string
}

// sshdState is everything read from the server before a change.
type sshdState struct {
	Main      string
	Files     []sshFile // main first, then include-dir files
	Effective map[string][]string
	Version   string
	Mode      string
	DropinDir string
	Managed   map[string][]string
	SELinux   string
}

func (st *sshdState) dropinPath() string { return path.Join(st.DropinDir, dropinName) }

func (st *sshdState) file(p string) (sshFile, bool) {
	for _, f := range st.Files {
		if f.Path == p {
			return f, true
		}
	}
	return sshFile{}, false
}

const fileMark = "@@SMFILE "

func sshdReadScript(main string) string {
	return `SSHD=$(command -v sshd 2>/dev/null || echo /usr/sbin/sshd)
M=` + core.Q(main) + `
command -v "$SSHD" >/dev/null 2>&1 || { echo @@nosshd; exit 0; }
echo @@version; $SSHD -V 2>&1 | head -n1; ssh -V 2>&1 | head -n1
echo @@selinux; getenforce 2>/dev/null
echo @@T; $SSHD -T -f "$M" 2>&1; echo "@@rc $?"
echo @@files
for f in "$M" $(sed -n 's/^[[:space:]]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][[:space:]]\{1,\}//p' "$M" 2>/dev/null | while read -r a; do case "$a" in /*) echo "$a";; *) echo "$(dirname "$M")/$a";; esac; done); do
  [ -f "$f" ] || continue
  printf '\n` + fileMark + `%s\n' "$f"; cat "$f"
done`
}

func (s *SecurityService) readSSHD(ctx context.Context, conn *sshx.Conn, pw, main string) (*sshdState, error) {
	res, err := s.root(ctx, conn, sshdReadScript(main), pw, "")
	if err != nil {
		return nil, err
	}
	return parseSSHDRead(res.Stdout, main)
}

func parseSSHDRead(out, main string) (*sshdState, error) {
	if strings.HasPrefix(out, "@@nosshd") {
		return nil, apperr.New("sec.ssh.notInstalled")
	}
	head, files, _ := strings.Cut(out, "@@files\n")
	sec := sections(head)
	st := &sshdState{Main: main, Managed: map[string][]string{}}
	tOut := sec["T"]
	rc := ""
	if i := strings.LastIndex(tOut, "@@rc "); i >= 0 {
		rc = strings.TrimSpace(tOut[i+5:])
		tOut = tOut[:i]
	}
	if rc != "0" {
		return nil, apperr.New("sec.ssh.readFailed").WithDetail(tOut)
	}
	st.Effective = parseSSHDT(tOut)
	if v := lines(sec["version"]); len(v) > 0 {
		st.Version = strings.TrimSpace(v[0])
		if i := strings.Index(st.Version, "OpenSSH"); i >= 0 {
			st.Version = strings.Fields(st.Version[i:])[0]
			st.Version = strings.TrimSuffix(st.Version, ",")
		}
	}
	st.SELinux = strings.TrimSpace(sec["selinux"])
	// Files: "\n@@SMFILE path\ncontent" chunks.
	chunks := strings.Split("\n"+files, "\n"+fileMark)
	seen := map[string]bool{}
	for _, ch := range chunks[1:] {
		p, content, _ := strings.Cut(ch, "\n")
		content = strings.TrimSuffix(content, "\n") // the separator newline added by printf
		if seen[p] {
			continue
		}
		seen[p] = true
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		st.Files = append(st.Files, sshFile{Path: p, Content: content})
	}
	if len(st.Files) == 0 || st.Files[0].Path != main {
		return nil, apperr.New("sec.ssh.readFailed").WithDetail(main)
	}
	st.Mode, st.DropinDir = detectMode(st.Files[0].Content, path.Dir(main))
	if st.Mode == "dropin" {
		if f, ok := st.file(st.dropinPath()); ok {
			st.Managed = parseDirectives(f.Content)
		}
	} else {
		st.Managed = parseDirectives(extractBlock(st.Files[0].Content))
	}
	return st, nil
}

// parseSSHDT parses `sshd -T` (lower-case keywords, one value per line for
// list options).
func parseSSHDT(out string) map[string][]string {
	m := map[string][]string{}
	for _, l := range lines(out) {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		k := strings.ToLower(f[0])
		m[k] = append(m[k], f[1:]...)
	}
	return m
}

// detectMode decides where the app writes: a drop-in when sshd_config
// includes sshd_config.d/*.conf before any directive the app manages
// (Debian/Ubuntu/RHEL 9 layout), else a block at the top of sshd_config.
func detectMode(main, baseDir string) (mode, dropinDir string) {
	firstManaged, include := -1, -1
	for i, raw := range strings.Split(main, "\n") {
		f := strings.Fields(stripComment(raw))
		if len(f) == 0 {
			continue
		}
		kw := strings.ToLower(f[0])
		if kw == "match" {
			break
		}
		if kw == "include" && include < 0 {
			for _, a := range f[1:] {
				if strings.HasSuffix(a, "/*.conf") || strings.HasSuffix(a, "/*") {
					dir := path.Dir(a)
					if !strings.HasPrefix(dir, "/") {
						dir = path.Join(baseDir, dir)
					}
					include = i
					dropinDir = dir
					break
				}
			}
		}
		if _, ok := canonicalKey(kw); ok && firstManaged < 0 {
			firstManaged = i
		}
	}
	if include >= 0 && (firstManaged < 0 || include < firstManaged) {
		return "dropin", dropinDir
	}
	return "main", ""
}

func stripComment(l string) string {
	if i := strings.Index(l, "#"); i >= 0 {
		return l[:i]
	}
	return l
}

// parseDirectives reads global directives (until the first Match) that the
// app manages.
func parseDirectives(content string) map[string][]string {
	m := map[string][]string{}
	for _, raw := range strings.Split(content, "\n") {
		f := strings.Fields(stripComment(raw))
		if len(f) < 2 {
			continue
		}
		if strings.EqualFold(f[0], "match") {
			break
		}
		if k, ok := canonicalKey(f[0]); ok {
			m[k] = append(m[k], f[1:]...)
		}
	}
	return m
}

func extractBlock(content string) string {
	i := strings.Index(content, blockBegin)
	if i < 0 {
		return ""
	}
	rest := content[i+len(blockBegin):]
	j := strings.Index(rest, blockEnd)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func removeBlock(content string) string {
	i := strings.Index(content, blockBegin)
	if i < 0 {
		return content
	}
	j := strings.Index(content[i:], blockEnd)
	if j < 0 {
		return content
	}
	end := i + j + len(blockEnd)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:i] + content[end:]
}

func renderDirectives(m map[string][]string) string {
	var b strings.Builder
	for _, k := range managedKeys {
		vals := m[k]
		if len(vals) == 0 {
			continue
		}
		if k == "Port" {
			for _, v := range vals {
				fmt.Fprintf(&b, "%s %s\n", k, v)
			}
			continue
		}
		fmt.Fprintf(&b, "%s %s\n", k, strings.Join(vals, " "))
	}
	return b.String()
}

// toggleCumulative comments out (disable=true) or restores the global
// occurrences of a cumulative keyword in a file.
func toggleCumulative(content, key string, disable bool) string {
	ls := strings.Split(content, "\n")
	inMatch := false
	for i, l := range ls {
		if disable {
			f := strings.Fields(stripComment(l))
			if len(f) > 0 && strings.EqualFold(f[0], "match") {
				inMatch = true
			}
			if !inMatch && len(f) > 1 && strings.EqualFold(f[0], key) {
				ls[i] = disabledMark + l
			}
			continue
		}
		if rest, ok := strings.CutPrefix(l, disabledMark); ok {
			f := strings.Fields(rest)
			if len(f) > 0 && strings.EqualFold(f[0], key) {
				ls[i] = rest
			}
		}
	}
	return strings.Join(ls, "\n")
}

// fileEdit is one file change of an sshd transaction.
type fileEdit struct {
	Path    string
	Content string
	Remove  bool
	Create  bool
}

// planSSH merges set into the managed directives and returns the file edits.
func planSSH(st *sshdState, set map[string][]string) (map[string][]string, []fileEdit, error) {
	managed := map[string][]string{}
	for k, v := range st.Managed {
		managed[k] = slices.Clone(v)
	}
	for rawKey, vals := range set {
		k, ok := canonicalKey(rawKey)
		if !ok {
			return nil, nil, apperr.New("sec.ssh.unsupportedKey", "key", rawKey)
		}
		if len(vals) == 0 {
			delete(managed, k)
			continue
		}
		norm, err := validateDirective(k, vals)
		if err != nil {
			return nil, nil, err
		}
		if len(norm) == 0 {
			delete(managed, k)
			continue
		}
		managed[k] = norm
	}

	contents := map[string]string{}
	for _, f := range st.Files {
		contents[f.Path] = f.Content
	}
	own := ""
	if st.Mode == "dropin" {
		own = st.dropinPath()
	}
	// Cumulative keys: comment out other occurrences while the app manages
	// them, restore them when it stops.
	for _, k := range managedKeys {
		lk := strings.ToLower(k)
		if !cumulativeKeys[lk] {
			continue
		}
		_, now := managed[k]
		for _, f := range st.Files {
			if f.Path == own {
				continue
			}
			c := contents[f.Path]
			if st.Mode == "main" && f.Path == st.Main {
				c = removeBlock(c) // the block is re-added below
			}
			contents[f.Path] = toggleCumulative(c, k, now)
		}
	}
	body := renderDirectives(managed)
	header := "# Managed by Server Manager. sshd uses the first value it reads for most\n# settings, so this file/block must stay first. Edit from the app.\n"
	edits := []fileEdit{}
	if st.Mode == "dropin" {
		p := st.dropinPath()
		_, exists := st.file(p)
		switch {
		case body == "" && exists:
			edits = append(edits, fileEdit{Path: p, Remove: true})
		case body != "":
			edits = append(edits, fileEdit{Path: p, Content: header + body, Create: !exists})
		}
	} else {
		c := removeBlock(contents[st.Main])
		if body != "" {
			c = blockBegin + "\n" + header + body + blockEnd + "\n" + c
		}
		contents[st.Main] = c
	}
	for _, f := range st.Files {
		if f.Path == own {
			continue
		}
		if contents[f.Path] != f.Content {
			edits = append(edits, fileEdit{Path: f.Path, Content: contents[f.Path]})
		}
	}
	return managed, edits, nil
}

func randToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// heredoc returns a shell fragment writing content to the file named by
// the shell expression dst.
func heredoc(dst, content string) string {
	tok := "SM_EOF_" + randToken()
	for strings.Contains(content, tok) {
		tok = "SM_EOF_" + randToken()
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if content == "" {
		return ": > " + dst + "\n"
	}
	return "cat > " + dst + " <<'" + tok + "'\n" + content + tok + "\n"
}

func restoreBody(edits []fileEdit) string {
	var b strings.Builder
	for i, e := range edits {
		v := fmt.Sprintf("F%d", i)
		fmt.Fprintf(&b, "%s=%s\n", v, core.Q(e.Path))
	}
	b.WriteString("restore() {\n  :\n")
	for i, e := range edits {
		v := fmt.Sprintf("F%d", i)
		if e.Create {
			fmt.Fprintf(&b, "  rm -f \"$%s\"\n", v)
		} else {
			fmt.Fprintf(&b, "  [ -f \"$%s.sm-bak-$STAMP\" ] && cat \"$%s.sm-bak-$STAMP\" > \"$%s\" && rm -f \"$%s.sm-bak-$STAMP\"\n", v, v, v, v)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// writeScript writes the edits (keeping one backup per file, named
// <file>.sm-bak-<stamp>) and validates the whole configuration with
// `sshd -t -f main`, restoring everything on failure. It prints @@ok or
// "@@invalid <reason>".
func writeScript(main string, edits []fileEdit, stamp string) string {
	var b strings.Builder
	b.WriteString("set -u\numask 022\nSSHD=$(command -v sshd 2>/dev/null || echo /usr/sbin/sshd)\n")
	b.WriteString("M=" + core.Q(main) + "\nSTAMP=" + core.Q(stamp) + "\n")
	b.WriteString(restoreBody(edits))
	for i, e := range edits {
		v := fmt.Sprintf("F%d", i)
		fmt.Fprintf(&b, "[ -L \"$%s\" ] && { echo '@@invalid symlink '\"$%s\"; exit 0; }\n", v, v)
		if !e.Create {
			fmt.Fprintf(&b, "rm -f \"$%s\".sm-bak-*; cp -p \"$%s\" \"$%s.sm-bak-$STAMP\" || { echo '@@invalid backup failed'; exit 0; }\n", v, v, v)
		}
	}
	for i, e := range edits {
		v := fmt.Sprintf("F%d", i)
		switch {
		case e.Remove:
			fmt.Fprintf(&b, "rm -f \"$%s\"\n", v)
		case e.Create:
			fmt.Fprintf(&b, "T=$(mktemp \"$(dirname \"$%s\")/.sm-XXXXXX\") || { restore; echo '@@invalid mktemp failed'; exit 0; }\n", v)
			b.WriteString(heredoc(`"$T"`, e.Content))
			fmt.Fprintf(&b, "chmod 644 \"$T\"; mv -f \"$T\" \"$%s\"\n", v)
		default:
			// cat > keeps the file's owner, mode and SELinux label.
			b.WriteString(heredoc(`"$`+v+`"`, e.Content))
		}
	}
	b.WriteString("if ! OUT=$($SSHD -t -f \"$M\" 2>&1); then restore; echo \"@@invalid $OUT\"; exit 0; fi\n")
	b.WriteString("echo @@ok\n")
	return b.String()
}

// restoreScript puts back the files saved by writeScript.
func restoreScript(edits []fileEdit, stamp string) string {
	return "STAMP=" + core.Q(stamp) + "\n" + restoreBody(edits) + "restore\necho @@restored\n"
}

// reloadScript reloads sshd (restarting ssh.socket for port changes under
// socket activation) and lists the TCP ports listening afterwards.
func reloadScript(portsChanged bool) string {
	var b strings.Builder
	if portsChanged {
		b.WriteString("if command -v systemctl >/dev/null 2>&1 && systemctl is-active -q ssh.socket 2>/dev/null; then systemctl daemon-reload; systemctl restart ssh.socket && echo @@socket; fi\n")
	}
	b.WriteString(`if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  if systemctl is-active -q ssh.service 2>/dev/null; then systemctl reload ssh.service && echo @@reloaded
  elif systemctl is-active -q sshd.service 2>/dev/null; then systemctl reload sshd.service && echo @@reloaded
  elif systemctl is-active -q ssh.socket 2>/dev/null; then echo @@reloaded
  fi
elif command -v rc-service >/dev/null 2>&1; then rc-service sshd reload >/dev/null 2>&1 && echo @@reloaded
elif command -v service >/dev/null 2>&1 && (service ssh reload >/dev/null 2>&1 || service sshd reload >/dev/null 2>&1); then echo @@reloaded
elif [ -f /var/run/sshd.pid ]; then kill -HUP "$(cat /var/run/sshd.pid)" && echo @@reloaded
fi
sleep 1
echo @@listen; (ss -tlnH 2>/dev/null || netstat -tln 2>/dev/null) | awk '{for(i=1;i<=NF;i++) if ($i ~ /:[0-9]+$/) {print $i; break}}'
`)
	return b.String()
}

// ---- public API ----

// SSHConfig reads the effective sshd configuration.
func (s *SecurityService) SSHConfig(connID, sudoPassword string) (SSHConfig, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return SSHConfig{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	st, err := s.readSSHD(ctx, conn, sudoPassword, defaultSSHDConfig)
	if err != nil {
		return SSHConfig{}, err
	}
	return s.buildConfig(ctx, conn, st), nil
}

func (s *SecurityService) buildConfig(ctx context.Context, conn *sshx.Conn, st *sshdState) SSHConfig {
	e := st.Effective
	one := func(k string) string {
		if v := e[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	list := func(k string) []string {
		if v := e[k]; v != nil {
			return v
		}
		return []string{}
	}
	cfg := SSHConfig{
		Version:                      st.Version,
		Ports:                        []int{},
		ListenAddresses:              list("listenaddress"),
		PermitRootLogin:              one("permitrootlogin"),
		PasswordAuthentication:       one("passwordauthentication"),
		PubkeyAuthentication:         one("pubkeyauthentication"),
		KbdInteractiveAuthentication: core.FirstNonEmpty(one("kbdinteractiveauthentication"), one("challengeresponseauthentication")),
		UsePAM:                       one("usepam"),
		MaxAuthTries:                 atoi(one("maxauthtries")),
		AllowUsers:                   list("allowusers"),
		AllowGroups:                  list("allowgroups"),
		DenyUsers:                    list("denyusers"),
		DenyGroups:                   list("denygroups"),
		X11Forwarding:                one("x11forwarding"),
		ClientAliveInterval:          atoi(one("clientaliveinterval")),
		ClientAliveCountMax:          atoi(one("clientalivecountmax")),
		AuthorizedKeysFile:           strings.Join(e["authorizedkeysfile"], " "),
		Mode:                         st.Mode,
		Managed:                      st.Managed,
		Files:                        []string{},
		LoginUser:                    loginUser(conn),
		LoginIsRoot:                  s.core.IsRoot(ctx, conn),
		AuthType:                     string(conn.Server().AuthType),
		SELinux:                      st.SELinux,
	}
	if cfg.PermitRootLogin == "without-password" {
		cfg.PermitRootLogin = "prohibit-password"
	}
	for _, p := range e["port"] {
		if n := atoi(p); n > 0 && !slices.Contains(cfg.Ports, n) {
			cfg.Ports = append(cfg.Ports, n)
		}
	}
	for _, f := range st.Files {
		cfg.Files = append(cfg.Files, f.Path)
	}
	if st.Mode == "dropin" {
		cfg.ConfigPath = st.dropinPath()
	} else {
		cfg.ConfigPath = st.Main
	}
	cfg.SessionPort = s.sessionPort(ctx, conn)
	if keys, err := s.readKeysAs(ctx, conn, "", ""); err == nil {
		for _, k := range keys {
			if !k.Invalid {
				cfg.LoginKeyCount++
			}
		}
	}
	return cfg
}

// UpdateSSH changes sshd settings safely: guards against locking the app
// out, writes the drop-in/managed block, validates with `sshd -t` (restoring
// the previous files on failure) and reloads sshd.
func (s *SecurityService) UpdateSSH(connID string, req SSHUpdate, sudoPassword string) (SSHUpdateResult, error) {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return SSHUpdateResult{}, err
	}
	res, err := s.updateSSH(connID, req, sudoPassword, defaultSSHDConfig, true)
	detail := describeSet(req.Set)
	if res.Backup != "" {
		detail += " (backup " + res.Backup + ")"
	}
	s.core.Audit(connID, "sec.ssh.update", res.Config.ConfigPath, detail, err)
	return res, err
}

func describeSet(set map[string][]string) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		v := strings.Join(set[k], " ")
		if v == "" {
			v = "(default)"
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}

func (s *SecurityService) updateSSH(connID string, req SSHUpdate, pw, main string, reload bool) (SSHUpdateResult, error) {
	out := SSHUpdateResult{Overridden: []string{}, Listening: []int{}}
	if len(req.Set) == 0 {
		return out, apperr.New("sec.ssh.nothing")
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return out, err
	}
	ctx, cancel := core.Timeout(90 * time.Second)
	defer cancel()
	st, err := s.readSSHD(ctx, conn, pw, main)
	if err != nil {
		return out, err
	}
	before := s.buildConfig(ctx, conn, st)
	out.Config = before
	managed, edits, err := planSSH(st, req.Set)
	if err != nil {
		return out, err
	}
	if len(edits) == 0 {
		return out, nil
	}
	portsChanged := false
	for k := range req.Set {
		if strings.EqualFold(k, "port") {
			portsChanged = true
		}
	}
	stamp := time.Now().Format("20060102-150405")
	r, err := s.root(ctx, conn, writeScript(main, edits, stamp), pw, "")
	if err != nil {
		return out, err
	}
	if strings.Contains(r.Stdout, "@@invalid") || !strings.Contains(r.Stdout, "@@ok") {
		detail := ""
		if i := strings.Index(r.Stdout, "@@invalid"); i >= 0 {
			detail = strings.TrimSpace(strings.TrimPrefix(r.Stdout[i:], "@@invalid"))
		}
		return out, apperr.New("sec.ssh.invalidConfig").WithDetail(core.FirstNonEmpty(detail, r.Stderr))
	}
	undo := func(e error) (SSHUpdateResult, error) {
		if _, rerr := s.root(ctx, conn, restoreScript(edits, stamp), pw, ""); rerr != nil {
			return out, apperr.Wrap(rerr, "sec.ssh.restoreFailed")
		}
		return out, e
	}
	// Guard on the configuration sshd will really use.
	st2, err := s.readSSHD(ctx, conn, pw, main)
	if err != nil {
		return undo(err)
	}
	after := s.buildConfig(ctx, conn, st2)
	if err := s.guardSSH(ctx, conn, before, after, req); err != nil {
		return undo(err)
	}
	// SELinux only lets sshd bind ports labelled ssh_port_t.
	if portsChanged && strings.EqualFold(st.SELinux, "enforcing") {
		for _, p := range after.Ports {
			if p == 22 || slices.Contains(before.Ports, p) {
				continue
			}
			ps := strconv.Itoa(p)
			cmd := "command -v semanage >/dev/null 2>&1 || { echo nosemanage; exit 3; }; semanage port -l | grep -Eq '^ssh_port_t.*[ ,]" + ps + "(,|$)' || semanage port -a -t ssh_port_t -p tcp " + ps + " 2>&1 || semanage port -m -t ssh_port_t -p tcp " + ps
			r, err := s.root(ctx, conn, cmd, pw, "")
			if err == nil && r.ExitCode != 0 {
				err = apperr.New("sec.ssh.selinuxPort", "port", ps)
				if !strings.Contains(r.Stdout, "nosemanage") {
					err = apperr.New("sec.ssh.selinuxPort", "port", ps).WithDetail(core.FirstNonEmpty(r.Stderr, r.Stdout))
				}
			}
			if err != nil {
				return undo(err)
			}
		}
	}
	for _, e := range edits {
		if !e.Create {
			out.Backup = e.Path + ".sm-bak-" + stamp
			break
		}
	}
	out.Config = after
	for k, vals := range managed {
		eff := st2.Effective[strings.ToLower(k)]
		if k == "KbdInteractiveAuthentication" && eff == nil {
			eff = st2.Effective["challengeresponseauthentication"]
		}
		if !sameValues(k, vals, eff) {
			out.Overridden = append(out.Overridden, k)
		}
	}
	sort.Strings(out.Overridden)
	if !reload {
		return out, nil
	}
	r, err = s.root(ctx, conn, reloadScript(portsChanged), pw, "")
	if err != nil {
		return out, err
	}
	out.Reloaded = strings.Contains(r.Stdout, "@@reloaded") || strings.Contains(r.Stdout, "@@socket")
	for _, l := range lines(sections(r.Stdout)["listen"]) {
		i := strings.LastIndex(l, ":")
		if n := atoi(l[i+1:]); n > 0 && !slices.Contains(out.Listening, n) {
			out.Listening = append(out.Listening, n)
		}
	}
	sort.Ints(out.Listening)
	if !out.Reloaded {
		return out, apperr.New("sec.ssh.reloadFailed").WithDetail(r.Stderr)
	}
	return out, nil
}

func sameValues(key string, want, got []string) bool {
	norm := func(v []string) []string {
		o := []string{}
		for _, x := range v {
			x = strings.ToLower(x)
			if x == "without-password" {
				x = "prohibit-password"
			}
			o = append(o, x)
		}
		if key == "Port" || key == "AllowUsers" || key == "AllowGroups" {
			sort.Strings(o)
			o = slices.Compact(o)
		}
		return o
	}
	return slices.Equal(norm(want), norm(got))
}

func passwordLogin(c SSHConfig) bool {
	return c.PasswordAuthentication == "yes" || (c.KbdInteractiveAuthentication == "yes" && c.UsePAM == "yes")
}

// loginAllowed reports whether AllowUsers/AllowGroups let user in.
func loginAllowed(c SSHConfig, user string, groups []string) bool {
	if len(c.AllowUsers) > 0 {
		ok := false
		for _, pat := range c.AllowUsers {
			u, _, _ := strings.Cut(pat, "@")
			if m, _ := path.Match(u, user); m {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(c.AllowGroups) > 0 {
		for _, g := range groups {
			for _, pat := range c.AllowGroups {
				if m, _ := path.Match(pat, g); m {
					return true
				}
			}
		}
		return false
	}
	return true
}

// guardSSH compares the configuration before and after a change and
// refuses what could lock the app out, unless the user acknowledged it.
func (s *SecurityService) guardSSH(ctx context.Context, conn *sshx.Conn, before, after SSHConfig, req SSHUpdate) error {
	c := req.Confirm
	auth := conn.Server().AuthType
	if passwordLogin(before) && !passwordLogin(after) && auth == store.AuthPassword && before.LoginKeyCount == 0 && !c.NoKeys {
		return apperr.New("sec.ssh.noKeys", "user", before.LoginUser)
	}
	if before.PubkeyAuthentication != "no" && after.PubkeyAuthentication == "no" && auth != store.AuthPassword && !c.PubkeySelf {
		return apperr.New("sec.ssh.pubkeySelf")
	}
	if before.LoginIsRoot && after.PermitRootLogin != before.PermitRootLogin && !c.RootLogin {
		if v := after.PermitRootLogin; v == "no" || (v != "yes" && auth == store.AuthPassword) {
			return apperr.New("sec.ssh.rootSelf", "value", v)
		}
	}
	if !slices.Contains(after.Ports, before.SessionPort) && slices.Contains(before.Ports, before.SessionPort) && !c.RemoveCurrentPort {
		return apperr.New("sec.ssh.removeCurrentPort", "port", strconv.Itoa(before.SessionPort))
	}
	added := []string{}
	for _, p := range after.Ports {
		if !slices.Contains(before.Ports, p) {
			added = append(added, strconv.Itoa(p))
		}
	}
	if len(added) > 0 && !c.NewPorts {
		return apperr.New("sec.ssh.confirmPorts", "ports", strings.Join(added, ", "))
	}
	if !c.Lockout && (!slices.Equal(before.AllowUsers, after.AllowUsers) || !slices.Equal(before.AllowGroups, after.AllowGroups)) {
		r, _ := s.user(ctx, conn, "id -Gn", "")
		if !loginAllowed(after, before.LoginUser, strings.Fields(r.Stdout)) {
			return apperr.New("sec.ssh.lockout", "user", before.LoginUser)
		}
	}
	return nil
}
