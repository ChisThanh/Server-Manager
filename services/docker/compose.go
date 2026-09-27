package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/sshx"
)

// ComposeProject is a Compose project found on the server (from
// `docker compose ls -a` and/or container labels).
type ComposeProject struct {
	Name        string   `json:"name"`
	Status      string   `json:"status"` // "running(2), exited(1)"
	ConfigFiles []string `json:"configFiles"`
	EnvFiles    []string `json:"envFiles"`
	WorkingDir  string   `json:"workingDir"`
	Running     int      `json:"running"`
	Total       int      `json:"total"`
	Unhealthy   int      `json:"unhealthy"`
	Services    []string `json:"services"` // services with containers
}

type composeLsRow struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

func (x *sess) projects(ctx context.Context) ([]ComposeProject, []Container, error) {
	cs, err := x.containers(ctx)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]*ComposeProject{}
	get := func(name string) *ComposeProject {
		p := byName[name]
		if p == nil {
			p = &ComposeProject{Name: name, ConfigFiles: []string{}, EnvFiles: []string{}, Services: []string{}}
			byName[name] = p
		}
		return p
	}
	for _, c := range cs {
		if c.Project == "" {
			continue
		}
		p := get(c.Project)
		p.Total++
		if c.State == "running" {
			p.Running++
		}
		if c.Health == "unhealthy" {
			p.Unhealthy++
		}
		if c.Service != "" && !contains(p.Services, c.Service) {
			p.Services = append(p.Services, c.Service)
		}
		if p.WorkingDir == "" {
			p.WorkingDir = c.WorkingDir
		}
		if len(p.ConfigFiles) == 0 && len(c.ConfigFiles) > 0 {
			p.ConfigFiles = c.ConfigFiles
		}
		if len(p.EnvFiles) == 0 && len(c.EnvFiles) > 0 {
			p.EnvFiles = c.EnvFiles
		}
	}
	if x.a.st.Compose == "plugin" {
		res, err := x.run(ctx, "docker compose ls -a --format json", "")
		if err != nil {
			return nil, nil, err
		}
		if res.ExitCode == 0 {
			for _, r := range jsonLines[composeLsRow](res.Stdout) {
				p := get(r.Name)
				p.Status = r.Status
				if files := splitList(r.ConfigFiles); len(files) > 0 {
					p.ConfigFiles = files
				}
			}
		}
	}
	out := []ComposeProject{}
	for _, p := range byName {
		if p.WorkingDir == "" && len(p.ConfigFiles) > 0 {
			p.WorkingDir = path.Dir(p.ConfigFiles[0])
		}
		if p.Status == "" {
			p.Status = statusSummary(p.Running, p.Total)
		}
		sort.Strings(p.Services)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, cs, nil
}

func statusSummary(running, total int) string {
	switch {
	case total == 0:
		return ""
	case running == total:
		return fmt.Sprintf("running(%d)", running)
	case running == 0:
		return fmt.Sprintf("exited(%d)", total)
	}
	return fmt.Sprintf("running(%d), exited(%d)", running, total-running)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ComposeProjects lists Compose projects.
func (s *DockerService) ComposeProjects(connID, sudoPassword string) ([]ComposeProject, error) {
	ctx, cancel := core.Timeout(45 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	ps, _, err := x.projects(ctx)
	return ps, err
}

func (x *sess) findProject(ctx context.Context, name string) (ComposeProject, []Container, error) {
	ps, cs, err := x.projects(ctx)
	if err != nil {
		return ComposeProject{}, nil, err
	}
	for _, p := range ps {
		if p.Name == name {
			return p, cs, nil
		}
	}
	return ComposeProject{}, nil, apperr.New("docker.projectNotFound", "name", name)
}

func (p ComposeProject) ref() (composeRef, error) {
	return composeRef{Project: p.Name, WorkingDir: p.WorkingDir, Files: p.ConfigFiles, EnvFiles: p.EnvFiles}.validate()
}

// ComposeContainer is one row of `docker compose ps -a`.
type ComposeContainer struct {
	Name     string `json:"name"`
	Service  string `json:"service"`
	Image    string `json:"image"`
	State    string `json:"state"`
	Status   string `json:"status"`
	Health   string `json:"health"`
	ExitCode int    `json:"exitCode"`
	Ports    string `json:"ports"`
	Created  int64  `json:"created"`
}

type composePsRow struct {
	Name       string `json:"Name"`
	Service    string `json:"Service"`
	Image      string `json:"Image"`
	State      string `json:"State"`
	Status     string `json:"Status"`
	Health     string `json:"Health"`
	ExitCode   int    `json:"ExitCode"`
	CreatedAt  string `json:"CreatedAt"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

func parseComposePS(out string) []ComposeContainer {
	list := []ComposeContainer{}
	for _, r := range jsonLines[composePsRow](out) {
		c := ComposeContainer{Name: r.Name, Service: r.Service, Image: r.Image, State: strings.ToLower(r.State), Status: r.Status,
			Health: r.Health, ExitCode: r.ExitCode, Created: parseTime(r.CreatedAt)}
		if c.Health == "" {
			c.Health = healthOf(r.Status)
		}
		var ports []string
		seen := map[string]bool{}
		for _, p := range r.Publishers {
			var s string
			if p.PublishedPort > 0 {
				s = fmt.Sprintf("%s:%d->%d/%s", p.URL, p.PublishedPort, p.TargetPort, p.Protocol)
			} else {
				s = fmt.Sprintf("%d/%s", p.TargetPort, p.Protocol)
			}
			if !seen[s] {
				seen[s] = true
				ports = append(ports, s)
			}
		}
		c.Ports = strings.Join(ports, ", ")
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Service != list[j].Service {
			return list[i].Service < list[j].Service
		}
		return list[i].Name < list[j].Name
	})
	return list
}

// ComposeDetail is a project with its containers and declared services.
type ComposeDetail struct {
	Project    ComposeProject     `json:"project"`
	Containers []ComposeContainer `json:"containers"`
	// Defined lists the services declared in the config files.
	Defined     []string `json:"defined"`
	ConfigError string   `json:"configError"`
}

// ComposeProjectDetail returns a project's containers (`compose ps -a`) and
// the services its files declare.
func (s *DockerService) ComposeProjectDetail(connID, name, sudoPassword string) (ComposeDetail, error) {
	if err := checkProject(name); err != nil {
		return ComposeDetail{}, err
	}
	ctx, cancel := core.Timeout(45 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return ComposeDetail{}, err
	}
	bin, err := x.compose()
	if err != nil {
		return ComposeDetail{}, err
	}
	p, cs, err := x.findProject(ctx, name)
	if err != nil {
		return ComposeDetail{}, err
	}
	d := ComposeDetail{Project: p, Containers: []ComposeContainer{}, Defined: []string{}}
	if x.a.st.Compose == "plugin" {
		res, err := x.run(ctx, "docker compose -p "+core.Q(name)+" ps -a --format json", "")
		if err != nil {
			return d, err
		}
		if res.ExitCode == 0 {
			d.Containers = parseComposePS(res.Stdout)
		}
	}
	if len(d.Containers) == 0 {
		// legacy docker-compose / older plugin: derive from labels
		for _, c := range cs {
			if c.Project == name {
				d.Containers = append(d.Containers, ComposeContainer{Name: c.Name, Service: c.Service, Image: c.Image,
					State: c.State, Status: c.Status, Health: c.Health, Ports: c.Ports, Created: c.Created})
			}
		}
	}
	if ref, err := p.ref(); err == nil && len(ref.Files) > 0 {
		sudo, err := x.composeSudo(ctx, ref)
		if err != nil {
			return d, err
		}
		res, err := x.s.core.Run(ctx, x.conn, ref.cd()+ref.args(bin)+" config --services", sudo, x.pw, "")
		if err != nil {
			return d, err
		}
		if res.ExitCode == 0 {
			for _, l := range strings.Split(res.Stdout, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					d.Defined = append(d.Defined, l)
				}
			}
			sort.Strings(d.Defined)
		} else {
			d.ConfigError = core.FirstNonEmpty(strings.TrimSpace(res.Stderr), strings.TrimSpace(res.Stdout))
		}
	} else if err != nil {
		d.ConfigError = apperr.From(err).Error()
	}
	return d, nil
}

// ComposeActionOptions tunes ComposeAction.
type ComposeActionOptions struct {
	Pull          bool `json:"pull"`          // up: pull images first (--pull always)
	RemoveVolumes bool `json:"removeVolumes"` // down: also remove named volumes (-v)
}

var composeActions = map[string]bool{"up": true, "pull": true, "restart": true, "stop": true, "start": true, "down": true}

// ComposeAction runs up | pull | restart | stop | start | down on a project
// as a job and returns the job id.
func (s *DockerService) ComposeAction(connID, name, action string, opts ComposeActionOptions, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return "", err
	}
	if !composeActions[action] {
		return "", apperr.New("docker.invalidAction", "action", action)
	}
	if err := checkProject(name); err != nil {
		return "", err
	}
	audit := "docker.compose." + action
	detail := ""
	switch action {
	case "up":
		detail = fmt.Sprintf("pull=%t", opts.Pull)
	case "down":
		detail = fmt.Sprintf("volumes=%t", opts.RemoveVolumes)
	}
	ctx, cancel := core.Timeout(45 * time.Second)
	defer cancel()
	fail := func(err error) (string, error) {
		s.core.Audit(connID, audit, name, detail, err)
		return "", err
	}
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return fail(err)
	}
	bin, err := x.compose()
	if err != nil {
		return fail(err)
	}
	p, _, err := x.findProject(ctx, name)
	if err != nil {
		return fail(err)
	}
	ref, err := p.ref()
	if err != nil {
		return fail(err)
	}
	needFiles := action == "up" || action == "pull" || x.a.st.Compose == "legacy"
	if needFiles && len(ref.Files) == 0 {
		return fail(apperr.New("docker.noComposeFile", "name", name))
	}
	sudo, err := x.composeSudo(ctx, ref)
	if err != nil {
		return fail(err)
	}
	if err := x.checkSudo(ctx, sudo); err != nil {
		return fail(err)
	}
	full := ref.cd() + ref.args(bin)
	short := bin + " -p " + core.Q(name)
	if needFiles {
		short = full
	}
	var cmds []string
	switch action {
	case "up":
		if opts.Pull && x.a.st.Compose == "legacy" {
			cmds = append(cmds, full+" pull")
		}
		up := full + " up -d --remove-orphans"
		if opts.Pull && x.a.st.Compose != "legacy" {
			up += " --pull always"
		}
		cmds = append(cmds, up)
	case "pull":
		cmds = append(cmds, full+" pull")
	case "down":
		c := short + " down --remove-orphans"
		if opts.RemoveVolumes {
			c += " -v"
		}
		cmds = append(cmds, c)
	default:
		cmds = append(cmds, short+" "+action)
	}
	if detail != "" {
		detail += " "
	}
	detail += "files=" + strings.Join(ref.Files, ",")
	return x.job(audit, "compose "+action+" "+name, func(ctx context.Context, j *core.Job) error {
		var err error
		for _, c := range cmds {
			if err = x.jobRun(ctx, j, c, sudo); err != nil {
				break
			}
		}
		s.core.Audit(connID, audit, name, detail, err)
		return err
	}), nil
}

// ---- compose files: read / validate / write / history ----

// ComposeFile is a compose file's content and metadata.
type ComposeFile struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Content string `json:"content"`
	Sha     string `json:"sha"` // sha256 of Content ("" when missing)
	Mode    string `json:"mode"`
	Owner   string `json:"owner"`
	// Writable: the login user can replace it without sudo.
	Writable bool `json:"writable"`
}

const maxComposeSize = 1 << 20

var tmpNameRe = regexp.MustCompile(`\.sm-compose\.[A-Za-z0-9]{6}`)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// readFile reads a compose file (through sudo when not readable).
func (s *DockerService) readFile(ctx context.Context, conn *sshx.Conn, p, pw string) (ComposeFile, error) {
	f := ComposeFile{Path: p}
	script := `f=` + core.Q(p) + `
if [ ! -e "$f" ]; then echo "@@missing"; exit 0; fi
if [ ! -f "$f" ]; then echo "@@notfile"; exit 0; fi
if [ ! -r "$f" ]; then echo "cat: $f: Permission denied" >&2; exit 1; fi
n=$(wc -c < "$f")
if [ "$n" -gt ` + itoa(maxComposeSize) + ` ]; then echo "@@toolarge $n"; exit 0; fi
echo "@@meta $(stat -c '%a %U:%G' "$f" 2>/dev/null)"
cat "$f"`
	res, err := s.core.RunAuto(ctx, conn, script, pw, "")
	if err != nil {
		return f, err
	}
	if res.ExitCode != 0 {
		return f, cmdErr(res)
	}
	head, body, _ := strings.Cut(res.Stdout, "\n")
	switch {
	case head == "@@missing":
	case head == "@@notfile":
		return f, apperr.New("docker.notAFile", "path", p)
	case strings.HasPrefix(head, "@@toolarge"):
		return f, apperr.New("docker.fileTooLarge", "path", p)
	case strings.HasPrefix(head, "@@meta"):
		f.Exists = true
		m := strings.Fields(strings.TrimPrefix(head, "@@meta"))
		if len(m) > 0 {
			f.Mode = m[0]
		}
		if len(m) > 1 {
			f.Owner = m[1]
		}
		f.Content = body
		f.Sha = sha(body)
	default:
		return f, apperr.New("docker.parseFailed").WithDetail(firstLine(res.Stdout))
	}
	w, err := s.writable(ctx, conn, p)
	if err != nil {
		return f, err
	}
	f.Writable = w
	return f, nil
}

// writable reports whether the login user can replace p (and create its
// directory) without sudo while keeping the file's owner.
func (s *DockerService) writable(ctx context.Context, conn *sshx.Conn, p string) (bool, error) {
	if s.core.IsRoot(ctx, conn) {
		return true, nil
	}
	script := `f=` + core.Q(p) + `; d=$(dirname "$f")
while [ ! -e "$d" ]; do d=$(dirname "$d"); done
if [ -d "$d" ] && [ -w "$d" ] && [ -x "$d" ] && { [ ! -e "$f" ] || [ "$(stat -c %u "$f")" = "$(id -u)" ]; }; then echo yes; else echo no; fi`
	res, err := s.core.Run(ctx, conn, script, false, "", "")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == "yes", nil
}

// ReadComposeFile returns a compose file's content (Exists=false when absent).
func (s *DockerService) ReadComposeFile(connID, filePath, sudoPassword string) (ComposeFile, error) {
	p, err := checkPath(filePath)
	if err != nil {
		return ComposeFile{}, err
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ComposeFile{}, err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	return s.readFile(ctx, conn, p, sudoPassword)
}

// ComposeSpec identifies a compose file within its project.
type ComposeSpec struct {
	Project    string   `json:"project"`
	WorkingDir string   `json:"workingDir"` // "" = directory of Path
	Files      []string `json:"files"`      // all config files, in order ("" = [Path])
	EnvFiles   []string `json:"envFiles"`
	Path       string   `json:"path"` // the file being edited (one of Files)
}

func (c ComposeSpec) normalize() (ComposeSpec, error) {
	var err error
	if c.Path, err = checkPath(c.Path); err != nil {
		return c, err
	}
	if c.WorkingDir == "" {
		c.WorkingDir = path.Dir(c.Path)
	}
	if len(c.Files) == 0 {
		c.Files = []string{c.Path}
	}
	ref, err := composeRef{Project: c.Project, WorkingDir: c.WorkingDir, Files: c.Files, EnvFiles: c.EnvFiles}.validate()
	if err != nil {
		return c, err
	}
	c.WorkingDir, c.Files, c.EnvFiles = ref.WorkingDir, ref.Files, ref.EnvFiles
	if !contains(c.Files, c.Path) {
		return c, apperr.New("docker.invalidPath", "path", c.Path)
	}
	return c, nil
}

// configCmd builds `compose … config -q` with the edited file replaced by
// "$tmp" and the project directory by "$pd" (shell variables).
func (c ComposeSpec) configCmd(bin string) string {
	var b strings.Builder
	b.WriteString(bin + " -p " + core.Q(c.Project) + ` --project-directory "$pd"`)
	for _, f := range c.Files {
		if f == c.Path {
			b.WriteString(` -f "$tmp"`)
		} else {
			b.WriteString(" -f " + core.Q(f))
		}
	}
	for _, f := range c.EnvFiles {
		b.WriteString(" --env-file " + core.Q(f))
	}
	b.WriteString(" config -q")
	return b.String()
}

// ValidateResult is the outcome of `compose config -q` on a candidate.
type ValidateResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"` // errors, or warnings when OK
}

// ValidateCompose checks candidate content for spec.Path: it is written to a
// temporary file in the same directory (so relative paths and env files
// resolve), checked with `compose config -q`, and removed.
func (s *DockerService) ValidateCompose(connID string, spec ComposeSpec, content, sudoPassword string) (ValidateResult, error) {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return ValidateResult{}, err
	}
	spec, err := spec.normalize()
	if err != nil {
		return ValidateResult{}, err
	}
	if len(content) > maxComposeSize {
		return ValidateResult{}, apperr.New("docker.fileTooLarge", "path", spec.Path)
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return ValidateResult{}, err
	}
	bin, err := x.compose()
	if err != nil {
		return ValidateResult{}, err
	}
	w, err := s.writable(ctx, x.conn, spec.Path)
	if err != nil {
		return ValidateResult{}, err
	}
	d := path.Dir(spec.Path)
	script := `d=` + core.Q(d) + `; rmd=
if [ -d "$d" ]; then vd="$d"; else vd=$(mktemp -d /tmp/.sm-compose.XXXXXX) || exit 97; rmd=1; fi
tmp=$(mktemp "$vd/.sm-compose.XXXXXX") || exit 97
cleanup() { rm -f "$tmp"; if [ -n "$rmd" ]; then rm -rf "$vd"; fi; }
trap 'cleanup; exit 130' INT TERM HUP
if ! cat > "$tmp"; then cleanup; exit 97; fi
pd=` + core.Q(spec.WorkingDir) + `; [ -d "$pd" ] || pd="$vd"
cd "$pd" || { cleanup; exit 97; }
` + spec.configCmd(bin) + ` 2>&1
rc=$?; cleanup; exit $rc`
	res, err := s.core.Run(ctx, x.conn, script, !w, sudoPassword, content)
	if err != nil {
		return ValidateResult{}, err
	}
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	switch res.ExitCode {
	case 0:
		return ValidateResult{OK: true, Output: out}, nil
	case 97:
		return ValidateResult{}, apperr.New("docker.writeFailed", "path", spec.Path).WithDetail(out)
	}
	// Show the user's file name instead of the temporary one.
	return ValidateResult{OK: false, Output: tmpNameRe.ReplaceAllString(out, path.Base(spec.Path))}, nil
}

// DeployRequest saves a compose file and deploys the project.
type DeployRequest struct {
	Spec    ComposeSpec `json:"spec"`
	Content string      `json:"content"`
	// ExpectedSha is the sha of the content the editor started from; the
	// save is refused (docker.fileChanged) if the file changed meanwhile.
	// "" skips the check.
	ExpectedSha string `json:"expectedSha"`
	Pull        bool   `json:"pull"`
	// Kind: "save" (edit), "rollback" (restore a stored version) or "new"
	// (create a project; refused when the file exists).
	Kind      string `json:"kind"`
	VersionID string `json:"versionId"` // rollback: the restored version
}

// DeployCompose validates and atomically writes a compose file (keeping the
// previous content in the history), then runs `up -d --remove-orphans`, as
// a job. Validation errors abort before anything is written.
func (s *DockerService) DeployCompose(connID string, req DeployRequest, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return "", err
	}
	action := "docker.compose.deploy"
	switch req.Kind {
	case "", "save":
		req.Kind = "save"
	case "rollback":
		action = "docker.compose.rollback"
	case "new":
		action = "docker.compose.create"
	default:
		return "", apperr.New("docker.invalidAction", "action", req.Kind)
	}
	spec, err := req.Spec.normalize()
	if err != nil {
		return "", err
	}
	target := spec.Project
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	fail := func(err error) (string, error) {
		s.core.Audit(connID, action, target, "path="+spec.Path, err)
		return "", err
	}
	if len(req.Content) > maxComposeSize {
		return fail(apperr.New("docker.fileTooLarge", "path", spec.Path))
	}
	if strings.TrimSpace(req.Content) == "" {
		return fail(apperr.New("docker.emptyFile"))
	}
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return fail(err)
	}
	bin, err := x.compose()
	if err != nil {
		return fail(err)
	}
	cur, err := s.readFile(ctx, x.conn, spec.Path, sudoPassword)
	if err != nil {
		return fail(err)
	}
	if req.Kind == "new" && cur.Exists {
		return fail(apperr.New("docker.fileExists", "path", spec.Path))
	}
	if req.ExpectedSha != "" && cur.Sha != req.ExpectedSha {
		return fail(apperr.New("docker.fileChanged", "path", spec.Path))
	}
	var version *ComposeVersion
	if req.Kind == "rollback" {
		v, err := s.version(connID, spec.Path, req.VersionID)
		if err != nil {
			return fail(err)
		}
		version = &v
		req.Content = v.Content
	}
	fileSudo := !cur.Writable
	upRef := composeRef{Project: spec.Project, WorkingDir: spec.WorkingDir, Files: spec.Files, EnvFiles: spec.EnvFiles}
	upSudo, err := x.composeSudo(ctx, upRef)
	if err != nil {
		return fail(err)
	}
	upSudo = upSudo || fileSudo
	if fileSudo || upSudo {
		if _, err := s.core.RunOK(ctx, x.conn, "true", true, sudoPassword, ""); err != nil {
			return fail(err)
		}
	}
	d := path.Dir(spec.Path)
	script := `d=` + core.Q(d) + `; f=` + core.Q(spec.Path) + `
mkdir -p "$d" || exit 97
tmp=$(mktemp "$d/.sm-compose.XXXXXX") || exit 97
cleanup() { rm -f "$tmp"; }
trap 'cleanup; exit 130' INT TERM HUP
if ! cat > "$tmp"; then cleanup; exit 97; fi
pd=` + core.Q(spec.WorkingDir) + `; [ -d "$pd" ] || pd="$d"
cd "$pd" || { cleanup; exit 97; }
if ! ` + spec.configCmd(bin) + `; then cleanup; exit 98; fi
if [ -e "$f" ]; then
  chmod "$(stat -c %a "$f")" "$tmp" 2>/dev/null
  chown "$(stat -c %u:%g "$f")" "$tmp" 2>/dev/null
else
  chmod 644 "$tmp"
fi
mv -f "$tmp" "$f" || { cleanup; exit 97; }
echo "saved $f"`
	up := "cd " + core.Q(spec.WorkingDir) + " && " + upRef.args(bin)
	newSha := sha(req.Content)
	detail := fmt.Sprintf("path=%s sha=%s→%s pull=%t", spec.Path, short(cur.Sha), short(newSha), req.Pull)
	if version != nil {
		detail += fmt.Sprintf(" version=%s (%s)", version.ID, time.UnixMilli(version.TS).UTC().Format(time.RFC3339))
	}
	actor := s.core.Actor()
	return x.job(action, "compose deploy "+spec.Project, func(ctx context.Context, j *core.Job) error {
		err := func() error {
			j.Step("validate + write %s", spec.Path)
			code, err := s.core.JobExecInput(ctx, j, x.conn, script, fileSudo, sudoPassword, strings.NewReader(req.Content))
			if err != nil {
				return err
			}
			switch code {
			case 0:
			case 98:
				return apperr.New("docker.validateFailed", "path", spec.Path)
			default:
				return apperr.New("docker.writeFailed", "path", spec.Path)
			}
			if cur.Exists && cur.Sha != newSha {
				note := req.Kind
				if err := s.addVersion(connID, spec.Path, ComposeVersion{TS: time.Now().UnixMilli(), Actor: actor, Sha: cur.Sha, Content: cur.Content, Note: note}); err != nil {
					j.Logf("warning: could not store the previous version: %v", err)
				}
			}
			j.SetResult(newSha)
			if req.Pull && x.a.st.Compose == "legacy" {
				if err := x.jobRun(ctx, j, up+" pull", upSudo); err != nil {
					return err
				}
			}
			cmd := up + " up -d --remove-orphans"
			if req.Pull && x.a.st.Compose != "legacy" {
				cmd += " --pull always"
			}
			return x.jobRun(ctx, j, cmd, upSudo)
		}()
		s.core.Audit(connID, action, target, detail, err)
		return err
	}), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "∅"
	}
	return sha
}

// ---- history ----

const historyNS = "docker.compose.history"
const historyMax = 20

// ComposeVersion is a stored previous content of a compose file.
type ComposeVersion struct {
	ID      string `json:"id"`
	TS      int64  `json:"ts"` // unix ms (when it was replaced)
	Actor   string `json:"actor"`
	Sha     string `json:"sha"`
	Size    int    `json:"size"`
	Note    string `json:"note"` // save | rollback: what replaced it
	Content string `json:"content,omitempty"`
}

func historyKey(connID, p string) string { return connID + "|" + p }

func (s *DockerService) history(connID, p string) ([]ComposeVersion, error) {
	var list []ComposeVersion
	err := s.core.DB.Get(historyNS, historyKey(connID, p), &list)
	if errors.Is(err, db.ErrNotFound) {
		return []ComposeVersion{}, nil
	}
	if list == nil {
		list = []ComposeVersion{}
	}
	return list, err
}

func (s *DockerService) addVersion(connID, p string, v ComposeVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.history(connID, p)
	if err != nil {
		return err
	}
	if len(list) > 0 && list[0].Sha == v.Sha {
		return nil // already the latest stored version
	}
	v.ID = uuid.NewString()
	v.Size = len(v.Content)
	list = append([]ComposeVersion{v}, list...)
	if len(list) > historyMax {
		list = list[:historyMax]
	}
	return s.core.DB.Put(historyNS, historyKey(connID, p), list)
}

func (s *DockerService) version(connID, p, id string) (ComposeVersion, error) {
	s.mu.Lock()
	list, err := s.history(connID, p)
	s.mu.Unlock()
	if err != nil {
		return ComposeVersion{}, err
	}
	for _, v := range list {
		if v.ID == id {
			return v, nil
		}
	}
	return ComposeVersion{}, apperr.New("docker.versionNotFound")
}

// ComposeHistory lists stored versions of a compose file (newest first,
// without content).
func (s *DockerService) ComposeHistory(connID, filePath string) ([]ComposeVersion, error) {
	p, err := checkPath(filePath)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	list, err := s.history(connID, p)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Content = ""
	}
	return list, nil
}

// ComposeVersionContent returns a stored version's content.
func (s *DockerService) ComposeVersionContent(connID, filePath, id string) (string, error) {
	p, err := checkPath(filePath)
	if err != nil {
		return "", err
	}
	v, err := s.version(connID, p, id)
	return v.Content, err
}
