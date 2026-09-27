package docker

import (
	"context"
	"regexp"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Status describes the Docker installation on a server.
type Status struct {
	Installed      bool   `json:"installed"`
	ClientVersion  string `json:"clientVersion"`
	ServerVersion  string `json:"serverVersion"`
	DaemonRunning  bool   `json:"daemonRunning"`
	DaemonError    string `json:"daemonError"`
	NeedSudo       bool   `json:"needSudo"`       // the login user can't reach the daemon socket
	Compose        string `json:"compose"`        // "plugin" (docker compose) | "legacy" (docker-compose) | ""
	ComposeVersion string `json:"composeVersion"` // e.g. "2.33.0"
}

// access is the cached result of the detection for one connection.
type access struct {
	at   time.Time
	st   Status
	sudo bool
}

// composeBin is the command that runs Compose ("" when missing).
func (a *access) composeBin() string {
	switch a.st.Compose {
	case "plugin":
		return "docker compose"
	case "legacy":
		return "docker-compose"
	}
	return ""
}

const accessTTL = 60 * time.Second

var (
	daemonDownRe = regexp.MustCompile(`(?i)cannot connect to the docker daemon|is the docker daemon running|error during connect|docker daemon is not running`)
	sockDeniedRe = regexp.MustCompile(`(?i)permission denied.*docker|docker.*permission denied|connect: permission denied`)
)

const detectScript = `
if ! command -v docker >/dev/null 2>&1; then echo "@@missing"; exit 0; fi
echo "@@client $(docker version --format '{{.Client.Version}}' 2>/dev/null)"
if v=$(docker compose version --short 2>/dev/null) && [ -n "$v" ]; then echo "@@compose plugin $v"
elif command -v docker-compose >/dev/null 2>&1; then echo "@@compose legacy $(docker-compose version --short 2>/dev/null)"; fi
out=$(docker version --format '{{.Server.Version}}' 2>&1); rc=$?
echo "@@server $rc"
printf '%s\n' "$out"
`

// detect runs the detection script (and the sudo probe when the socket is
// not accessible). Errors are only transport/sudo errors; a missing docker
// or a stopped daemon is reported in Status.
func (s *DockerService) detect(ctx context.Context, conn *sshx.Conn, pw string) (*access, error) {
	res, err := s.core.Run(ctx, conn, detectScript, false, "", "")
	if err != nil {
		return nil, err
	}
	a := &access{at: time.Now()}
	st := &a.st
	var serverRC string
	var serverOut []string
	inServer := false
	for _, line := range strings.Split(res.Stdout, "\n") {
		switch {
		case line == "@@missing":
			return a, nil
		case strings.HasPrefix(line, "@@client "):
			st.Installed = true
			st.ClientVersion = strings.TrimSpace(strings.TrimPrefix(line, "@@client "))
		case strings.HasPrefix(line, "@@compose "):
			f := strings.Fields(strings.TrimPrefix(line, "@@compose "))
			if len(f) > 0 {
				st.Compose = f[0]
			}
			if len(f) > 1 {
				st.ComposeVersion = strings.TrimPrefix(f[1], "v")
			}
		case strings.HasPrefix(line, "@@server "):
			serverRC = strings.TrimSpace(strings.TrimPrefix(line, "@@server "))
			inServer = true
		case inServer && strings.TrimSpace(line) != "":
			serverOut = append(serverOut, line)
		}
	}
	if !st.Installed {
		// docker exists but printed nothing parseable; treat as installed.
		st.Installed = strings.Contains(res.Stdout, "@@server")
	}
	out := strings.TrimSpace(strings.Join(serverOut, "\n"))
	if serverRC == "0" {
		st.DaemonRunning = true
		st.ServerVersion = firstLine(out)
		return a, nil
	}
	if sockDeniedRe.MatchString(out) && !s.core.IsRoot(ctx, conn) {
		st.NeedSudo = true
		a.sudo = true
		r2, err := s.core.Run(ctx, conn, "docker version --format '{{.Server.Version}}' 2>&1", true, pw, "")
		if err != nil {
			return nil, err
		}
		o2 := strings.TrimSpace(r2.Stdout + r2.Stderr)
		if r2.ExitCode == 0 {
			st.DaemonRunning = true
			st.ServerVersion = firstLine(o2)
			return a, nil
		}
		st.DaemonError = o2
		return a, nil
	}
	st.DaemonError = out
	return a, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

func (s *DockerService) cached(conn *sshx.Conn) *access {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.cache[conn]; ok && time.Since(a.at) < accessTTL {
		return a
	}
	return nil
}

func (s *DockerService) remember(conn *sshx.Conn, a *access) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.cache {
		if time.Since(v.at) > 10*accessTTL {
			delete(s.cache, k)
		}
	}
	s.cache[conn] = a
}

func (s *DockerService) forget(conn *sshx.Conn) {
	s.mu.Lock()
	delete(s.cache, conn)
	s.mu.Unlock()
}

// Status detects Docker on the server (always fresh). A missing docker or a
// stopped daemon is not an error; sudo errors are (so the UI can prompt).
func (s *DockerService) Status(connID, sudoPassword string) (Status, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return Status{}, err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	a, err := s.detect(ctx, conn, sudoPassword)
	if err != nil {
		return Status{}, err
	}
	s.remember(conn, a)
	return a.st, nil
}

// sess is a connection that is known to reach a running daemon.
type sess struct {
	s      *DockerService
	connID string
	conn   *sshx.Conn
	a      *access
	pw     string
}

// open returns a session, detecting Docker when the cache is cold. It fails
// with docker.notInstalled / docker.daemonDown.
func (s *DockerService) open(ctx context.Context, connID, pw string) (*sess, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	a := s.cached(conn)
	if a == nil || !a.st.DaemonRunning {
		if a, err = s.detect(ctx, conn, pw); err != nil {
			return nil, err
		}
		s.remember(conn, a)
	}
	if !a.st.Installed {
		return nil, apperr.New("docker.notInstalled")
	}
	if !a.st.DaemonRunning {
		return nil, apperr.New("docker.daemonDown").WithDetail(a.st.DaemonError)
	}
	return &sess{s: s, connID: connID, conn: conn, a: a, pw: pw}, nil
}

// compose returns the Compose command or docker.composeMissing.
func (x *sess) compose() (string, error) {
	if b := x.a.composeBin(); b != "" {
		return b, nil
	}
	return "", apperr.New("docker.composeMissing")
}

// run executes a docker command (through sudo when the socket needs it).
// A non-zero exit is not an error, except a stopped daemon.
func (x *sess) run(ctx context.Context, cmd, stdin string) (sshx.ExecResult, error) {
	res, err := x.s.core.Run(ctx, x.conn, cmd, x.a.sudo, x.pw, stdin)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 && daemonDownRe.MatchString(res.Stderr) {
		x.s.forget(x.conn)
		return res, apperr.New("docker.daemonDown").WithDetail(res.Stderr)
	}
	return res, nil
}

// ok is run that turns a non-zero exit into docker.cmdFailed.
func (x *sess) ok(ctx context.Context, cmd, stdin string) (sshx.ExecResult, error) {
	res, err := x.run(ctx, cmd, stdin)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, cmdErr(res)
	}
	return res, nil
}

// checkSudo makes sure sudo works now (so the UI can prompt for a password)
// before a job that needs it starts.
func (x *sess) checkSudo(ctx context.Context, need bool) error {
	if !need {
		return nil
	}
	_, err := x.s.core.RunOK(ctx, x.conn, "true", true, x.pw, "")
	return err
}

// job starts a background job bound to this session.
func (x *sess) job(kind, title string, fn func(ctx context.Context, j *core.Job) error) string {
	return x.s.core.Jobs.Start(x.connID, kind, title, fn).ID()
}

// jobRun streams a docker command into the job (through sudo when asked);
// a non-zero exit → docker.cmdFailed.
func (x *sess) jobRun(ctx context.Context, j *core.Job, cmd string, sudo bool) error {
	j.Step("$ %s", cmd)
	code, err := x.s.core.JobExec(ctx, j, x.conn, cmd, sudo, x.pw)
	if err != nil {
		return err
	}
	if code != 0 {
		return apperr.New("docker.cmdFailed", "code", itoa(code))
	}
	return nil
}

// composeSudo reports whether Compose must run through sudo: the daemon
// needs it, or the login user can't read the project's files.
func (x *sess) composeSudo(ctx context.Context, ref composeRef) (bool, error) {
	if x.a.sudo {
		return true, nil
	}
	if x.s.core.IsRoot(ctx, x.conn) {
		return false, nil
	}
	var b strings.Builder
	files := append(append([]string{}, ref.Files...), ref.EnvFiles...)
	if ref.WorkingDir != "" {
		files = append(files, ref.WorkingDir+"/.env")
		b.WriteString("[ ! -e " + core.Q(ref.WorkingDir) + " ] || [ -x " + core.Q(ref.WorkingDir) + " ] || echo no\n")
	}
	for _, f := range files {
		b.WriteString("[ ! -e " + core.Q(f) + " ] || [ -r " + core.Q(f) + " ] || echo no\n")
	}
	if b.Len() == 0 {
		return false, nil
	}
	res, err := x.s.core.Run(ctx, x.conn, b.String()+"true", false, "", "")
	if err != nil {
		return false, err
	}
	return strings.Contains(res.Stdout, "no"), nil
}

func cmdErr(res sshx.ExecResult) error {
	return apperr.New("docker.cmdFailed", "code", itoa(res.ExitCode)).
		WithDetail(core.FirstNonEmpty(strings.TrimSpace(res.Stderr), strings.TrimSpace(res.Stdout)))
}
