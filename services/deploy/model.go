package deploy

import (
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"server-manager/internal/apperr"
)

// App types.
const (
	TypeGit     = "git"     // git checkout + build/test/restart commands
	TypeCompose = "compose" // existing compose project; deploy = change an image tag variable
	TypeImage   = "image"   // simple Docker image app; the app generates <dir>/compose.yml
)

// Namespaces in the kv store.
const (
	nsApp = "deploy.app"
)

// App is a deployable application on one server.
type App struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	Name   string `json:"name"`
	// Stage: production | staging | development.
	Stage string `json:"stage"`
	Type  string `json:"type"`

	// Dir is the deploy directory: the git checkout, or the compose project dir.
	Dir string `json:"dir"`
	// EnvFile is where variables and secrets are written ("" = <dir>/.env;
	// relative paths are relative to Dir).
	EnvFile string `json:"envFile"`
	// LoadEnv exports the env file's variables into build/test/restart/health commands.
	LoadEnv bool `json:"loadEnv"`
	// Sudo runs every step as root; RestartSudo only the restart command.
	Sudo        bool `json:"sudo"`
	RestartSudo bool `json:"restartSudo"`

	// git
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`    // default branch or tag to deploy
	TokenUser  string `json:"tokenUser"` // username sent with the HTTPS token
	HasToken   bool   `json:"hasToken"`  // output only
	Submodules bool   `json:"submodules"`

	// Multi-line shell scripts, run in Dir with `set -e`.
	BuildCmd   string `json:"buildCmd"`
	TestCmd    string `json:"testCmd"`
	RestartCmd string `json:"restartCmd"`

	// compose / image
	ComposeFile string    `json:"composeFile"` // relative to Dir (default compose.yml)
	Project     string    `json:"project"`     // compose project name ("" = compose default)
	TagVar      string    `json:"tagVar"`      // env var holding the image tag
	DefaultTag  string    `json:"defaultTag"`
	SkipPull    bool      `json:"skipPull"` // images are built/loaded locally
	Image       ImageSpec `json:"image"`

	Vars    []EnvVar `json:"vars"`
	Secrets []string `json:"secrets"` // names only; values live in the keychain

	Health       HealthCheck `json:"health"`
	AutoRollback bool        `json:"autoRollback"`

	// Advanced
	MinFreeMB      int `json:"minFreeMB"`      // preflight disk-space floor
	CommandTimeout int `json:"commandTimeout"` // minutes per build/test/restart step

	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// ImageSpec describes the container of a TypeImage app.
type ImageSpec struct {
	Image         string   `json:"image"` // without tag, e.g. "ghcr.io/org/app"
	ContainerName string   `json:"containerName"`
	Ports         []string `json:"ports"`   // "8080:80", "127.0.0.1:8080:80/tcp"
	Volumes       []string `json:"volumes"` // "./data:/data", "name:/path:ro"
	Restart       string   `json:"restart"` // no | always | unless-stopped | on-failure
	Command       string   `json:"command"` // optional command override (sh -c)
	// Optional Docker HEALTHCHECK (CMD-SHELL).
	HealthCmd      string `json:"healthCmd"`
	HealthInterval int    `json:"healthInterval"` // seconds
	HealthTimeout  int    `json:"healthTimeout"`
	HealthRetries  int    `json:"healthRetries"`
}

// EnvVar is a plain (non-secret) environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HealthCheck runs after the restart step.
type HealthCheck struct {
	Type string `json:"type"` // none | http | command
	// http: checked from the server with curl or wget.
	URL          string `json:"url"`
	ExpectStatus int    `json:"expectStatus"` // 0 = any 2xx/3xx
	BodyContains string `json:"bodyContains"`
	Insecure     bool   `json:"insecure"` // skip TLS verification
	// command: exit 0 = healthy.
	Command  string `json:"command"`
	Timeout  int    `json:"timeout"`  // seconds per attempt
	Retries  int    `json:"retries"`  // attempts
	Interval int    `json:"interval"` // seconds between attempts
	Delay    int    `json:"delay"`    // seconds before the first attempt
}

// ---- validation ----

var (
	reEnvName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reAppID     = regexp.MustCompile(`^[a-f0-9-]{8,64}$`)
	reRunID     = regexp.MustCompile(`^[a-f0-9-]{8,64}$`)
	rePath      = regexp.MustCompile(`^[A-Za-z0-9._@+,=/-]+$`)
	reSHA       = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	reTag       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	reProject   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	reContainer = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	reVolName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	rePort      = regexp.MustCompile(`^(?:(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]):)?(?:\d{1,5}(?:-\d{1,5})?:)?\d{1,5}(?:-\d{1,5})?(?:/(?:tcp|udp|sctp))?$`)
	reTokenUser = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,100}$`)
	// Docker reference: [domain[:port]/]path, lowercase path components.
	reImage = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]+)?/)?[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	// scp-like git remote: user@host:path
	reSCP = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/@+][A-Za-z0-9._~/@+-]*$`)
)

// Directories an app may never be deployed into (or be).
var forbiddenDirs = map[string]bool{
	"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true, "/lib": true, "/lib64": true,
	"/opt": true, "/proc": true, "/root": true, "/run": true, "/sbin": true, "/srv": true, "/sys": true, "/tmp": true,
	"/usr": true, "/var": true, "/var/lib": true, "/var/www": true, "/usr/local": true, "/usr/bin": true, "/var/log": true,
}

// System files and directories an env file may never be written to (it is
// replaced wholesale with mode 0600, possibly as root).
var forbiddenEnvPrefixes = []string{"/etc/ssh/", "/etc/pam.d/", "/etc/sudoers", "/etc/security/", "/root/.ssh/", "/boot/", "/proc/", "/sys/", "/dev/",
	"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/lib/", "/lib64/", "/usr/lib/"}
var forbiddenEnvFiles = map[string]bool{"/etc/passwd": true, "/etc/shadow": true, "/etc/group": true, "/etc/gshadow": true, "/etc/hosts": true,
	"/etc/fstab": true, "/etc/crontab": true, "/etc/resolv.conf": true, "/etc/hostname": true, "/etc/environment": true, "/etc/profile": true}

func safeEnvPath(p string) bool {
	p = path.Clean(p)
	if forbiddenEnvFiles[p] || strings.Contains(p, "/.ssh/") || path.Dir(p) == "/" || path.Dir(p) == "/etc" && !strings.HasSuffix(p, ".env") {
		return false
	}
	for _, pre := range forbiddenEnvPrefixes {
		if strings.HasPrefix(p, pre) {
			return false
		}
	}
	return true
}

func invalid(field string) error { return apperr.New("deploy.invalid", "field", field) }

// ValidPath checks an absolute server path: safe charset, no "." / ".."
// components, not a system directory.
func validDir(p string) bool {
	if !strings.HasPrefix(p, "/") || len(p) > 1024 || !rePath.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg == "." || seg == ".." || seg == "" {
			return false
		}
	}
	return !forbiddenDirs[path.Clean(p)]
}

// validRelOrAbs checks a file path that is either absolute or relative to
// the app dir (without escaping it).
func validRelOrAbs(p string) bool {
	if p == "" || len(p) > 1024 || !rePath.MatchString(p) || strings.HasSuffix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == ".." || seg == "" {
			return false
		}
	}
	return true
}

// ValidRef implements the rules of `git check-ref-format --allow-onelevel`
// (plus a length limit and no leading "-").
func ValidRef(r string) bool {
	if r == "" || len(r) > 200 || r == "@" || strings.HasPrefix(r, "-") || strings.HasPrefix(r, "/") ||
		strings.HasSuffix(r, "/") || strings.HasSuffix(r, ".") || strings.HasSuffix(r, ".lock") ||
		strings.Contains(r, "..") || strings.Contains(r, "//") || strings.Contains(r, "@{") {
		return false
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f || c > 0x7e || strings.ContainsRune(" ~^:?*[\\", c) {
			return false
		}
	}
	for _, seg := range strings.Split(r, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}

// ValidRepoURL accepts https://, http://, ssh://, git://, file:// URLs,
// scp-like user@host:path and absolute paths. HTTPS URLs must not embed a
// password (use the token field).
func ValidRepoURL(s string) bool {
	if s == "" || len(s) > 2048 || strings.HasPrefix(s, "-") {
		return false
	}
	for _, c := range s {
		if c <= 0x20 || c == 0x7f || c > 0x7e || c == '\'' || c == '"' || c == '\\' || c == '`' {
			return false
		}
	}
	if strings.HasPrefix(s, "/") {
		return validDirLoose(s)
	}
	if reSCP.MatchString(s) {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https", "http", "git", "ssh":
		if u.Host == "" || strings.HasPrefix(u.Host, "-") {
			return false
		}
		if _, hasPW := u.User.Password(); hasPW {
			return false
		}
		return true
	case "file":
		return u.Host == "" && validDirLoose(u.Path)
	}
	return false
}

func validDirLoose(p string) bool {
	if !strings.HasPrefix(p, "/") || !rePath.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// ValidImage checks an image reference without tag/digest.
func ValidImage(s string) bool { return len(s) <= 255 && reImage.MatchString(s) }

// ValidTag checks a Docker image tag.
func ValidTag(s string) bool { return reTag.MatchString(s) }

// ValidEnvName checks an environment variable name.
func ValidEnvName(s string) bool { return len(s) <= 128 && reEnvName.MatchString(s) }

// validHTTPURL checks a health-check URL.
func validHTTPURL(s string) bool {
	if len(s) > 2048 {
		return false
	}
	for _, c := range s {
		if c <= 0x20 || c == 0x7f || c > 0x7e {
			return false
		}
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && !strings.HasPrefix(u.Host, "-")
}

func validVolume(v string) bool {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	src, dst := parts[0], parts[1]
	switch {
	case strings.HasPrefix(src, "/"):
		if !validDirLoose(src) {
			return false
		}
	case strings.HasPrefix(src, "./"):
		if !validRelOrAbs(strings.TrimPrefix(src, "./")) && src != "./" {
			return false
		}
	default:
		if !reVolName.MatchString(src) {
			return false
		}
	}
	if !validDirLoose(dst) {
		return false
	}
	if len(parts) == 3 {
		for _, o := range strings.Split(parts[2], ",") {
			switch o {
			case "ro", "rw", "z", "Z", "cached", "delegated", "consistent", "nocopy":
			default:
				return false
			}
		}
	}
	return true
}

func noControl(s string, allowNewline bool) bool {
	for _, c := range s {
		if c == '\n' && allowNewline {
			continue
		}
		if c == '\t' {
			continue
		}
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// normalize fills defaults and validates an app definition.
func (a *App) normalize() error {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" || len(a.Name) > 64 || !noControl(a.Name, false) {
		return invalid("name")
	}
	switch a.Stage {
	case "production", "staging", "development":
	case "":
		a.Stage = "production"
	default:
		return invalid("stage")
	}
	a.Dir = strings.TrimRight(strings.TrimSpace(a.Dir), "/")
	if !validDir(a.Dir) {
		return invalid("dir")
	}
	a.EnvFile = strings.TrimSpace(a.EnvFile)
	if a.EnvFile != "" && !validRelOrAbs(a.EnvFile) {
		return invalid("envFile")
	}
	if !safeEnvPath(a.envPath()) {
		return invalid("envFile")
	}
	for _, s := range []*string{&a.BuildCmd, &a.TestCmd, &a.RestartCmd, &a.Health.Command, &a.Image.Command, &a.Image.HealthCmd} {
		*s = strings.TrimSpace(strings.ReplaceAll(*s, "\r\n", "\n"))
		if len(*s) > 64<<10 || !noControl(*s, true) {
			return invalid("command")
		}
	}
	switch a.Type {
	case TypeGit:
		a.Repo = strings.TrimSpace(a.Repo)
		if !ValidRepoURL(a.Repo) {
			return invalid("repo")
		}
		a.Branch = strings.TrimSpace(a.Branch)
		if a.Branch == "" {
			a.Branch = "main"
		}
		if !ValidRef(a.Branch) {
			return invalid("branch")
		}
		a.TokenUser = strings.TrimSpace(a.TokenUser)
		if a.TokenUser == "" {
			a.TokenUser = "x-access-token"
		}
		if !reTokenUser.MatchString(a.TokenUser) {
			return invalid("tokenUser")
		}
		a.ComposeFile, a.Project, a.TagVar, a.DefaultTag, a.Image = "", "", "", "", ImageSpec{}
	case TypeCompose, TypeImage:
		a.Repo, a.Branch, a.TokenUser, a.Submodules = "", "", "", false
		a.TagVar = strings.TrimSpace(a.TagVar)
		if a.TagVar == "" {
			a.TagVar = "APP_TAG"
		}
		if !ValidEnvName(a.TagVar) {
			return invalid("tagVar")
		}
		a.DefaultTag = strings.TrimSpace(a.DefaultTag)
		if a.DefaultTag == "" {
			a.DefaultTag = "latest"
		}
		if !ValidTag(a.DefaultTag) {
			return invalid("tag")
		}
		a.Project = strings.TrimSpace(a.Project)
		if a.Project != "" && !reProject.MatchString(a.Project) {
			return invalid("project")
		}
		if a.Type == TypeImage {
			a.ComposeFile = "compose.yml"
			if err := a.Image.normalize(); err != nil {
				return err
			}
		} else {
			a.Image = ImageSpec{}
			a.ComposeFile = strings.TrimSpace(a.ComposeFile)
			if a.ComposeFile == "" {
				a.ComposeFile = "compose.yml"
			}
			if !validRelOrAbs(a.ComposeFile) {
				return invalid("composeFile")
			}
		}
	default:
		return invalid("type")
	}
	// Variables and secrets.
	seen := map[string]bool{}
	if a.Type != TypeGit {
		seen[a.TagVar] = true
	}
	vars := []EnvVar{}
	for _, v := range a.Vars {
		v.Name = strings.TrimSpace(v.Name)
		if v.Name == "" && v.Value == "" {
			continue
		}
		if !ValidEnvName(v.Name) {
			return apperr.New("deploy.invalidEnvName", "name", v.Name)
		}
		if seen[v.Name] {
			return apperr.New("deploy.duplicateEnv", "name", v.Name)
		}
		if err := checkEnvValue(v.Name, v.Value); err != nil {
			return err
		}
		seen[v.Name] = true
		vars = append(vars, v)
	}
	a.Vars = vars
	secrets := []string{}
	for _, n := range a.Secrets {
		if !ValidEnvName(n) {
			return apperr.New("deploy.invalidEnvName", "name", n)
		}
		if seen[n] {
			return apperr.New("deploy.duplicateEnv", "name", n)
		}
		seen[n] = true
		secrets = append(secrets, n)
	}
	a.Secrets = secrets
	if err := a.Health.normalize(); err != nil {
		return err
	}
	if a.MinFreeMB < 0 || a.MinFreeMB > 1<<20 {
		return invalid("minFreeMB")
	}
	if a.MinFreeMB == 0 {
		a.MinFreeMB = 100
	}
	if a.CommandTimeout <= 0 {
		a.CommandTimeout = 30
	}
	if a.CommandTimeout > 24*60 {
		return invalid("commandTimeout")
	}
	return nil
}

func (s *ImageSpec) normalize() error {
	s.Image = strings.TrimSpace(s.Image)
	if !ValidImage(s.Image) {
		return invalid("image")
	}
	s.ContainerName = strings.TrimSpace(s.ContainerName)
	if s.ContainerName != "" && !reContainer.MatchString(s.ContainerName) {
		return invalid("containerName")
	}
	ports := []string{}
	for _, p := range s.Ports {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !rePort.MatchString(p) {
			return apperr.New("deploy.invalidPort", "value", p)
		}
		ports = append(ports, p)
	}
	s.Ports = ports
	vols := []string{}
	for _, v := range s.Volumes {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !validVolume(v) {
			return apperr.New("deploy.invalidVolume", "value", v)
		}
		vols = append(vols, v)
	}
	s.Volumes = vols
	switch s.Restart {
	case "":
		s.Restart = "unless-stopped"
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return invalid("restart")
	}
	if strings.Contains(s.Command, "\n") {
		return invalid("command")
	}
	if strings.Contains(s.HealthCmd, "\n") {
		return invalid("healthCmd")
	}
	for _, n := range []*int{&s.HealthInterval, &s.HealthTimeout, &s.HealthRetries} {
		if *n < 0 || *n > 3600 {
			return invalid("healthcheck")
		}
	}
	return nil
}

func (h *HealthCheck) normalize() error {
	switch h.Type {
	case "", "none":
		*h = HealthCheck{Type: "none"}
		return nil
	case "http":
		h.URL = strings.TrimSpace(h.URL)
		if !validHTTPURL(h.URL) {
			return invalid("healthUrl")
		}
		if h.ExpectStatus != 0 && (h.ExpectStatus < 100 || h.ExpectStatus > 599) {
			return invalid("expectStatus")
		}
		if len(h.BodyContains) > 1024 || !noControl(h.BodyContains, false) {
			return invalid("bodyContains")
		}
		h.Command = ""
	case "command":
		if h.Command == "" {
			return invalid("healthCommand")
		}
		h.URL, h.BodyContains, h.ExpectStatus, h.Insecure = "", "", 0, false
	default:
		return invalid("healthType")
	}
	if h.Timeout <= 0 {
		h.Timeout = 5
	}
	if h.Retries <= 0 {
		h.Retries = 5
	}
	if h.Interval <= 0 {
		h.Interval = 3
	}
	if h.Timeout > 300 || h.Retries > 100 || h.Interval > 600 || h.Delay < 0 || h.Delay > 600 {
		return invalid("health")
	}
	return nil
}

// envPath is the absolute path of the app's env file.
func (a *App) envPath() string {
	if a.EnvFile == "" {
		return a.Dir + "/.env"
	}
	if strings.HasPrefix(a.EnvFile, "/") {
		return a.EnvFile
	}
	return a.Dir + "/" + strings.TrimPrefix(a.EnvFile, "./")
}

// composePath is the absolute path of the compose file.
func (a *App) composePath() string {
	if strings.HasPrefix(a.ComposeFile, "/") {
		return a.ComposeFile
	}
	return a.Dir + "/" + strings.TrimPrefix(a.ComposeFile, "./")
}

// shortVersion shortens a git sha for display; tags are kept.
func shortVersion(v string) string {
	if reSHA.MatchString(v) && len(v) > 12 {
		return v[:12]
	}
	return v
}

func itoa(i int) string { return strconv.Itoa(i) }
