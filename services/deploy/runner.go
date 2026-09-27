package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// Triggers.
const (
	TriggerManual       = "manual"
	TriggerRedeploy     = "redeploy"
	TriggerRollback     = "rollback"
	TriggerAutoRollback = "auto-rollback"
)

// Remote lock: a directory in /tmp (mkdir is atomic) holding who deploys.
// The holder touches it every lockHeartbeat; a lock not touched for
// lockStale is considered abandoned (crashed app, lost connection).
const (
	lockHeartbeat = 60 * time.Second
	lockStale     = 10 * time.Minute
	quietTimeout  = 90 * time.Second
	runLogMax     = 1 << 20
)

// errSkip marks a step as skipped.
var errSkip = errors.New("skip")

// ---- live state ----

type liveRun struct {
	mu     sync.Mutex
	server string
	appID  string
	dir    string
	jobID  string
	cur    string
	runs   []string
	steps  map[string][]Step
}

func newLiveRun(server, appID string) *liveRun {
	return &liveRun{server: server, appID: appID, steps: map[string][]Step{}}
}

func (l *liveRun) currentRun() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

func (l *liveRun) setJob(id string) {
	l.mu.Lock()
	l.jobID = id
	l.mu.Unlock()
}

func (l *liveRun) begin(runID string) {
	l.mu.Lock()
	steps := make([]Step, len(stepIDs))
	for i, id := range stepIDs {
		steps[i] = Step{ID: id, State: StatePending}
	}
	l.steps[runID] = steps
	l.cur = runID
	l.runs = append(l.runs, runID)
	job := l.jobID
	l.mu.Unlock()
	for _, st := range steps {
		core.Emit(EventStep, StepEvent{Server: l.server, AppID: l.appID, JobID: job, RunID: runID, Step: st.ID, State: st.State})
	}
}

func (l *liveRun) set(runID, step, state, note string) {
	l.mu.Lock()
	now := time.Now().UnixMilli()
	var ev StepEvent
	for i := range l.steps[runID] {
		st := &l.steps[runID][i]
		if st.ID != step {
			continue
		}
		st.State = state
		if note != "" {
			st.Note = note
		}
		switch state {
		case StateRunning:
			st.StartedAt, st.FinishedAt = now, 0
		case StateOK, StateFailed:
			if st.StartedAt == 0 {
				st.StartedAt = now
			}
			st.FinishedAt = now
		case StateSkipped:
			st.FinishedAt = now
		}
		ev = StepEvent{Server: l.server, AppID: l.appID, JobID: l.jobID, RunID: runID, Step: step, State: state,
			StartedAt: st.StartedAt, FinishedAt: st.FinishedAt, Note: st.Note}
	}
	l.mu.Unlock()
	if ev.Step != "" {
		core.Emit(EventStep, ev)
	}
}

func (l *liveRun) stepsOf(runID string) []Step {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Step{}, l.steps[runID]...)
}

func (l *liveRun) snapshot(runID string, d RunData) RunData {
	if st := l.stepsOf(runID); len(st) > 0 {
		d.Steps = st
	}
	return d
}

// ---- output ----

// runLog tees job output into the current run's log (tail kept).
type runLog struct {
	mu  sync.Mutex
	job io.Writer
	buf []byte
}

func (t *runLog) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > runLogMax {
		t.buf = append([]byte("…\n"), t.buf[len(t.buf)-runLogMax/2:]...)
	}
	t.mu.Unlock()
	return t.job.Write(p)
}

func (t *runLog) reset() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := string(t.buf)
	t.buf = nil
	return s
}

type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// ---- runner ----

type runSpec struct {
	id         string
	trigger    string
	ref        string // requested ref / tag
	target     string // exact sha / tag (rollback, redeploy)
	rollbackOf string
}

type runResult struct {
	status     string
	version    string
	data       RunData
	err        error
	failedStep string
}

type runner struct {
	s     *DeployService
	conn  *sshx.Conn
	app   App
	pw    string
	j     *core.Job
	lr    *liveRun
	actor string

	red   *Redactor
	log   *runLog
	out   *RedactWriter
	errw  *RedactWriter
	token string
	env   []EnvVar // secret values

	composeBin string
	sudoMode   string // "" unknown, "root", "n" (NOPASSWD), "S" (password)

	lockPath  string
	lockToken string
	lockHeld  bool
	hbStop    chan struct{}
	hbDone    chan struct{}
}

func (s *DeployService) newRunner(conn *sshx.Conn, a App, pw string, j *core.Job, lr *liveRun) *runner {
	r := &runner{s: s, conn: conn, app: a, pw: pw, j: j, lr: lr, actor: s.core.Actor(), red: NewRedactor()}
	r.log = &runLog{job: j}
	r.out = r.red.Writer(r.log)
	r.errw = r.red.Writer(r.log)
	for _, n := range a.Secrets {
		v := store.Keychain(secretKey(a.ID, n))
		r.env = append(r.env, EnvVar{Name: n, Value: v})
		r.red.Add(v)
	}
	if a.Type == TypeGit {
		r.token = store.Keychain(tokenKey(a.ID))
		r.red.Add(r.token)
	}
	r.lockPath = lockPathFor(a.Dir)
	r.lockToken = uuid.NewString()
	return r
}

func (r *runner) logf(format string, args ...any) {
	fmt.Fprintf(r.out, format+"\n", args...)
	r.out.Flush()
}

func (r *runner) header(format string, args ...any) {
	r.logf("\x1b[1;36m▶ "+format+"\x1b[0m", args...)
}

// cleanErr redacts secrets from an error's detail and params.
func (r *runner) cleanErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	ae := apperr.From(err)
	out := apperr.New(ae.Code)
	for k, v := range ae.Params {
		if out.Params == nil {
			out.Params = map[string]string{}
		}
		out.Params[k] = r.red.Redact(v)
	}
	out.Detail = r.red.Redact(ae.Detail)
	return out
}

// sudoPrefix returns how to run a command as root.
func (r *runner) sudo(ctx context.Context) (string, error) {
	if r.sudoMode != "" {
		return r.sudoMode, nil
	}
	if r.s.core.IsRoot(ctx, r.conn) {
		r.sudoMode = "root"
		return r.sudoMode, nil
	}
	probe, err := r.conn.Exec(ctx, "sudo -n true", nil)
	if err != nil {
		return "", err
	}
	switch {
	case probe.ExitCode == 0:
		r.sudoMode = "n"
	case strings.Contains(probe.Stderr, "not found") && strings.Contains(probe.Stderr, "sudo"):
		return "", apperr.New("sudo.missing")
	default:
		pw := r.s.core.SudoPassword(r.conn, r.pw)
		if pw == "" {
			return "", apperr.New("sudo.required")
		}
		r.red.Add(pw)
		r.sudoMode = "S"
	}
	return r.sudoMode, nil
}

// stream runs script with its output redacted into the job and run log.
func (r *runner) stream(ctx context.Context, script string, sudo bool, stdin string) (int, error) {
	full := "sh -c " + core.Q(script)
	in := stdin
	mode := ""
	if sudo {
		m, err := r.sudo(ctx)
		if err != nil {
			return -1, err
		}
		mode = m
		switch m {
		case "n":
			full = "sudo -n -- " + full
		case "S":
			full = "sudo -S -p '' -- " + full
			in = r.s.core.SudoPassword(r.conn, r.pw) + "\n" + stdin
		}
	}
	var tail tailBuf
	code, err := r.conn.Stream(ctx, full, strings.NewReader(in), r.out, io.MultiWriter(r.errw, &tail))
	r.out.Flush()
	r.errw.Flush()
	if err == nil && code != 0 && mode == "S" {
		t := tail.String()
		if strings.Contains(t, "incorrect password") || strings.Contains(t, "Sorry, try again") {
			return code, apperr.New("sudo.wrongPassword")
		}
		if strings.Contains(t, "is not in the sudoers") || strings.Contains(t, "not allowed to execute") {
			return code, apperr.New("sudo.notAllowed")
		}
	}
	return code, err
}

// quiet runs a helper command whose output is parsed, not shown.
func (r *runner) quiet(ctx context.Context, script string, sudo bool, stdin string) (sshx.ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, quietTimeout)
	defer cancel()
	return r.s.core.Run(ctx, r.conn, script, sudo, r.pw, stdin)
}

// runScript streams a user script (build/test/restart) with a timeout.
func (r *runner) runScript(ctx context.Context, step, script string, sudo bool) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.app.CommandTimeout)*time.Minute)
	defer cancel()
	code, err := r.stream(ctx, script, sudo, "")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || (ctx.Err() == context.DeadlineExceeded) {
			return apperr.New("deploy.timeout", "step", step, "min", itoa(r.app.CommandTimeout))
		}
		return err
	}
	if code != 0 {
		return apperr.New("deploy.stepFailed", "step", step, "code", itoa(code))
	}
	return nil
}

// userScript wraps multi-line commands: cd to the app dir, optionally
// export the env file, stop at the first failing command.
func (r *runner) userScript(cmds string) string {
	a := &r.app
	var b strings.Builder
	b.WriteString("cd " + core.Q(a.Dir) + " || exit 1\n")
	if a.LoadEnv {
		env := core.Q(a.envPath())
		b.WriteString("if [ -r " + env + " ]; then set -a; . " + env + "; set +a; fi\n")
	}
	b.WriteString("set -e\n")
	b.WriteString(cmds)
	b.WriteString("\n")
	return b.String()
}

// writeFileScript atomically replaces path with stdin (mode, owner kept or
// inherited from the directory when running as root).
func writeFileScript(path, mode string) string {
	return `set -e
f=` + core.Q(path) + `
d=$(dirname "$f")
mkdir -p "$d"
umask 077
tmp=$(mktemp "$d/.sm-tmp.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
chmod ` + mode + ` "$tmp"
if [ "$(id -u)" = 0 ]; then
  if [ -e "$f" ]; then o=$(stat -c %u:%g "$f"); else o=$(stat -c %u:%g "$d"); fi
  chown "$o" "$tmp" 2>/dev/null || true
fi
mv -f "$tmp" "$f"
trap - EXIT
`
}

// ---- lock ----

// lockPathFor is the remote lock of a deploy directory (shared by every app
// instance deploying there).
func lockPathFor(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return "/tmp/.sm-deploy-" + hex.EncodeToString(h[:8]) + ".lock"
}

func (r *runner) acquireLock(ctx context.Context, runID string) error {
	host, _ := os.Hostname()
	info := fmt.Sprintf("token=%s\nrun=%s\napp=%s\nactor=%s\nhost=%s\npid=%d\nstarted=%d\n",
		r.lockToken, runID, oneLine(r.app.Name), oneLine(r.actor), oneLine(host), os.Getpid(), time.Now().Unix())
	script := `L=` + core.Q(r.lockPath) + `
TTL=` + strconv.Itoa(int(lockStale.Seconds())) + `
try() { if mkdir "$L" 2>/dev/null; then cat > "$L/info"; echo ACQUIRED; exit 0; fi; }
try
now=$(date +%s); m=$(stat -c %Y "$L" 2>/dev/null || echo "$now"); age=$((now-m))
if [ "$age" -gt "$TTL" ]; then
  echo "STALE $age"; cat "$L/info" 2>/dev/null
  mv "$L" "$L.stale.$$" 2>/dev/null && rm -rf "$L.stale.$$"
  try
fi
echo "HELD $age"; cat "$L/info" 2>/dev/null
exit 3
`
	res, err := r.quiet(ctx, script, r.app.Sudo, info)
	if err != nil {
		return err
	}
	out := res.Stdout
	if strings.Contains(out, "ACQUIRED") {
		if strings.HasPrefix(out, "STALE") {
			held := parseKV(out)
			r.logf("\x1b[33m! Broke a stale deploy lock (%s, %s@%s, idle %ss)\x1b[0m", r.lockPath, held["actor"], held["host"], firstField(out))
		}
		r.lockHeld = true
		r.logf("✓ lock %s", r.lockPath)
		r.startHeartbeat()
		return nil
	}
	if strings.Contains(out, "HELD") {
		held := parseKV(out)
		age := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "HELD ") {
				age = strings.TrimPrefix(l, "HELD ")
			}
		}
		started := ""
		if ts, err := strconv.ParseInt(held["started"], 10, 64); err == nil {
			started = time.Unix(ts, 0).Format(time.RFC3339)
		}
		return apperr.New("deploy.locked", "actor", held["actor"], "host", held["host"], "app", held["app"], "since", started, "idle", age)
	}
	return apperr.New("deploy.lockFailed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
}

func firstField(out string) string {
	f := strings.Fields(out)
	if len(f) > 1 {
		return f[1]
	}
	return ""
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			m[k] = v
		}
	}
	return m
}

func (r *runner) startHeartbeat() {
	r.hbStop = make(chan struct{})
	r.hbDone = make(chan struct{})
	go func() {
		defer close(r.hbDone)
		t := time.NewTicker(lockHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-r.hbStop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, _ = r.s.core.Run(ctx, r.conn, `L=`+core.Q(r.lockPath)+`; grep -qx `+core.Q("token="+r.lockToken)+` "$L/info" 2>/dev/null && touch "$L"`, r.app.Sudo, r.pw, "")
				cancel()
			}
		}
	}()
}

func (r *runner) releaseLock() {
	if !r.lockHeld {
		return
	}
	close(r.hbStop)
	<-r.hbDone
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = r.s.core.Run(ctx, r.conn, `L=`+core.Q(r.lockPath)+`; grep -qx `+core.Q("token="+r.lockToken)+` "$L/info" 2>/dev/null && rm -rf "$L"`, r.app.Sudo, r.pw, "")
	r.lockHeld = false
}

// ---- pipeline ----

func (r *runner) runOnce(ctx context.Context, spec runSpec) runResult {
	a := &r.app
	r.log.reset()
	r.lr.begin(spec.id)
	d := RunData{AppName: a.Name, AppType: a.Type, Trigger: spec.trigger, RollbackOf: spec.rollbackOf, Ref: spec.ref, JobID: r.j.ID()}
	started := time.Now()
	r.logf("\x1b[1m%s\x1b[0m · %s · %s · %s", a.Name, a.Type, spec.trigger, r.actor)

	type stepFn struct {
		id    string
		label string
		fn    func(context.Context, *RunData, runSpec) error
	}
	var steps []stepFn
	if a.Type == TypeGit {
		steps = []stepFn{
			{"preflight", "Preflight checks", r.preflight},
			{"fetch", "Fetch & checkout", r.gitFetch},
			{"env", "Write environment", r.writeEnv},
			{"build", "Build", r.build},
			{"test", "Test", r.test},
			{"restart", "Restart", r.restart},
			{"health", "Health check", r.health},
		}
	} else {
		steps = []stepFn{
			{"preflight", "Preflight checks", r.preflight},
			{"fetch", "Prepare release", r.composeRelease},
			{"env", "Write environment", r.writeEnv},
			{"build", "Pull images", r.build},
			{"test", "Test", r.test},
			{"restart", "Start containers", r.restart},
			{"health", "Health check", r.health},
		}
	}
	res := runResult{}
	for _, st := range steps {
		if res.err != nil {
			r.lr.set(spec.id, st.id, StateSkipped, "")
			continue
		}
		if ctx.Err() != nil {
			res.err, res.failedStep = ctx.Err(), st.id
			r.lr.set(spec.id, st.id, StateFailed, "")
			continue
		}
		r.lr.set(spec.id, st.id, StateRunning, "")
		r.header("%s", st.label)
		err := st.fn(ctx, &d, spec)
		switch {
		case errors.Is(err, errSkip):
			r.logf("\x1b[90m– skipped\x1b[0m")
			r.lr.set(spec.id, st.id, StateSkipped, "")
		case err != nil:
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			err = r.cleanErr(err)
			res.err, res.failedStep = err, st.id
			r.logf("\x1b[1;31m✕ %s\x1b[0m", errText(err))
			r.lr.set(spec.id, st.id, StateFailed, "")
		default:
			r.lr.set(spec.id, st.id, StateOK, "")
		}
	}
	if res.err != nil {
		r.lr.set(spec.id, "done", StateFailed, "")
	} else {
		r.lr.set(spec.id, "done", StateOK, "")
	}
	d.Steps = r.lr.stepsOf(spec.id)
	res.version = d.Target
	if d.Commit != nil {
		res.version = d.Commit.Short
	}
	if res.version == "" {
		res.version = spec.ref
	}
	switch {
	case res.err == nil:
		res.status = "ok"
		r.logf("\x1b[1;32m✓ Deployed %s in %s\x1b[0m", res.version, time.Since(started).Round(time.Second))
	case errors.Is(res.err, context.Canceled):
		res.status = "cancelled"
		r.logf("\x1b[33m■ Cancelled\x1b[0m")
	default:
		res.status = "failed"
		ae := apperr.From(res.err)
		d.Error = ae
	}
	res.data = d
	r.s.finishRun(spec.id, res, r.log.reset())
	r.record(spec, res)
	return res
}

// logText renders deploy error codes readably in the job log (the UI shows
// the translated error separately).
var logText = map[string]string{
	"deploy.gitMissing":         "git is not installed on the server",
	"deploy.dockerMissing":      "docker is not installed on the server",
	"deploy.dockerDenied":       "no access to the Docker daemon (enable \"run as root\" or add the user to the docker group)",
	"deploy.composeMissing":     "docker compose is not installed on the server",
	"deploy.httpToolMissing":    "neither curl nor wget is installed for the health check",
	"deploy.dirNotWritable":     "{dir} is not writable",
	"deploy.dirNotEmpty":        "{dir} exists, is not empty and is not a git checkout",
	"deploy.lowDisk":            "only {free} MB free, {need} MB required",
	"deploy.secretMissing":      "secret {name} has no value in the keychain",
	"deploy.locked":             "another deployment is running ({actor}@{host}, since {since})",
	"deploy.lockFailed":         "cannot take the deploy lock",
	"deploy.fetchFailed":        "fetching the repository failed (exit {code})",
	"deploy.checkoutFailed":     "checkout failed (exit {code})",
	"deploy.refNotFound":        "{ref} is not a branch, tag or commit of the repository",
	"deploy.invalidRef":         "invalid git ref {ref}",
	"deploy.invalidTag":         "invalid image tag {tag}",
	"deploy.composeFileMissing": "compose file {path} not found",
	"deploy.writeFailed":        "cannot write {path}",
	"deploy.stepFailed":         "{step} failed (exit {code})",
	"deploy.timeout":            "{step} timed out after {min} min",
	"deploy.healthFailed":       "health check failed after {attempts} attempts",
}

func errText(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	ae := apperr.From(err)
	msg, ok := logText[ae.Code]
	if !ok {
		return ae.Error()
	}
	for k, v := range ae.Params {
		msg = strings.ReplaceAll(msg, "{"+k+"}", v)
	}
	if ae.Detail != "" {
		msg += ": " + ae.Detail
	}
	return msg
}

// record audits the run and puts it on the timeline.
func (r *runner) record(spec runSpec, res runResult) {
	a := &r.app
	action := "deploy.run"
	if spec.trigger == TriggerRollback || spec.trigger == TriggerAutoRollback {
		action = "deploy.rollback"
	}
	detail := fmt.Sprintf("version=%s ref=%s trigger=%s", res.version, spec.ref, spec.trigger)
	if res.data.PreviousVersion != "" {
		detail += " previous=" + res.data.PreviousVersion
	}
	if res.failedStep != "" {
		detail += " failedStep=" + res.failedStep
	}
	r.s.core.Audit(r.conn.Server().ID, action, a.Name, detail, res.err)
	params := map[string]string{"app": a.Name, "version": res.version}
	switch {
	case res.status == "ok" && action == "deploy.rollback":
		r.s.core.AddEvent(core.Event{Server: r.conn.Server().ID, Kind: "deploy", Severity: "warn", Code: "deploy.rollback", Params: params, Actor: r.actor})
	case res.status == "ok":
		r.s.core.AddEvent(core.Event{Server: r.conn.Server().ID, Kind: "deploy", Severity: "ok", Code: "deploy.ok", Params: params, Actor: r.actor})
	case res.status == "failed":
		r.s.core.AddEvent(core.Event{Server: r.conn.Server().ID, Kind: "deploy", Severity: "crit", Code: "deploy.failed", Params: params,
			Detail: errText(res.err), Actor: r.actor})
	}
}

// ---- steps ----

func (r *runner) preflight(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	var b strings.Builder
	b.WriteString("D=" + core.Q(a.Dir) + "\n")
	b.WriteString(`p="$D"; while [ ! -e "$p" ]; do p=$(dirname "$p"); done
if [ -d "$D" ]; then t="$D"; else t="$p"; fi
if [ -w "$t" ]; then echo WRITABLE=1; else echo WRITABLE=0; fi
echo "AVAIL=$(df -Pk "$p" 2>/dev/null | awk 'NR==2{print $4}')"
`)
	if a.Type == TypeGit {
		b.WriteString(`if command -v git >/dev/null 2>&1; then echo "GIT=$(git --version)"; else echo GIT=; fi` + "\n")
	} else {
		b.WriteString(`if command -v docker >/dev/null 2>&1; then
  if docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then echo DOCKER=ok; else echo DOCKER=denied; fi
else echo DOCKER=missing; fi
if docker compose version >/dev/null 2>&1; then echo "COMPOSE=docker compose $(docker compose version --short 2>/dev/null)"
elif command -v docker-compose >/dev/null 2>&1; then echo COMPOSE=docker-compose; else echo COMPOSE=; fi
`)
	}
	if a.Health.Type == "http" {
		b.WriteString(`if command -v curl >/dev/null 2>&1; then echo HTTP=curl; elif command -v wget >/dev/null 2>&1; then echo HTTP=wget; else echo HTTP=; fi` + "\n")
	}
	res, err := r.quiet(ctx, b.String(), a.Sudo, "")
	if err != nil {
		return err
	}
	kv := parseKV(res.Stdout)
	if a.Type == TypeGit {
		if kv["GIT"] == "" {
			return apperr.New("deploy.gitMissing")
		}
		r.logf("✓ %s", kv["GIT"])
	} else {
		switch kv["DOCKER"] {
		case "missing":
			return apperr.New("deploy.dockerMissing")
		case "denied":
			return apperr.New("deploy.dockerDenied")
		}
		c := kv["COMPOSE"]
		if c == "" {
			return apperr.New("deploy.composeMissing")
		}
		if strings.HasPrefix(c, "docker compose") {
			r.composeBin = "docker compose"
		} else {
			r.composeBin = "docker-compose"
		}
		r.logf("✓ %s", c)
	}
	if a.Health.Type == "http" {
		if kv["HTTP"] == "" {
			return apperr.New("deploy.httpToolMissing")
		}
		r.logf("✓ health check via %s", kv["HTTP"])
	}
	if kv["WRITABLE"] != "1" {
		return apperr.New("deploy.dirNotWritable", "dir", a.Dir)
	}
	r.logf("✓ %s writable", a.Dir)
	if avail, err := strconv.ParseInt(kv["AVAIL"], 10, 64); err == nil {
		free := avail / 1024
		if free < int64(a.MinFreeMB) {
			return apperr.New("deploy.lowDisk", "free", strconv.FormatInt(free, 10), "need", itoa(a.MinFreeMB))
		}
		r.logf("✓ %d MB free", free)
	}
	for _, v := range r.env {
		if v.Value == "" {
			return apperr.New("deploy.secretMissing", "name", v.Name)
		}
	}
	if a.Type == TypeGit && a.HasToken && r.token == "" {
		return apperr.New("deploy.secretMissing", "name", "git token")
	}
	if !r.lockHeld {
		return r.acquireLock(ctx, spec.id)
	}
	return nil
}

// gitPrelude defines G (git with safe.directory and, when a token is set,
// an inline credential helper reading it from $SM_GIT_TOKEN, which is read
// from stdin — the token never appears on a command line or on disk).
func (r *runner) gitPrelude() string {
	a := &r.app
	var b strings.Builder
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")
	b.WriteString(`export GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new}"` + "\n")
	cred := ""
	if r.token != "" {
		b.WriteString("IFS= read -r SM_GIT_TOKEN || true\nexport SM_GIT_TOKEN\n")
		b.WriteString("SM_GIT_USER=" + core.Q(a.TokenUser) + "\nexport SM_GIT_USER\n")
		helper := `!f() { test "$1" = get || exit 0; echo "username=$SM_GIT_USER"; echo "password=$SM_GIT_TOKEN"; }; f`
		cred = " -c credential.helper= -c " + core.Q("credential.helper="+helper)
	}
	b.WriteString("G() { git -c " + core.Q("safe.directory="+a.Dir) + cred + ` "$@"; }` + "\n")
	return b.String()
}

func (r *runner) gitStdin() string {
	if r.token != "" {
		return r.token + "\n"
	}
	return ""
}

func (r *runner) gitFetch(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	D := core.Q(a.Dir)
	G := "git -c " + core.Q("safe.directory="+a.Dir)
	// What is deployed now.
	if res, err := r.quiet(ctx, "cd "+D+" 2>/dev/null && "+G+" log -1 --format=%H%x1f%h HEAD 2>/dev/null", a.Sudo, ""); err == nil && res.ExitCode == 0 {
		if f := strings.Split(strings.TrimSpace(res.Stdout), "\x1f"); len(f) == 2 {
			d.PreviousTarget, d.PreviousVersion = f[0], f[1]
			r.logf("Current: %s", f[1])
		}
	}
	ref := spec.ref
	if spec.target != "" {
		ref = spec.target
	}
	if ref == "" {
		ref = a.Branch
	}
	if !ValidRef(ref) {
		return apperr.New("deploy.invalidRef", "ref", ref)
	}
	d.Ref = core.FirstNonEmpty(spec.ref, ref)

	// Rollbacks don't need the network when the commit is already here.
	haveLocal := false
	if spec.target != "" && reSHA.MatchString(spec.target) {
		res, err := r.quiet(ctx, "cd "+D+" 2>/dev/null && "+G+" cat-file -e "+core.Q(spec.target+"^{commit}"), a.Sudo, "")
		haveLocal = err == nil && res.ExitCode == 0
	}
	if !haveLocal {
		script := "set -e\n" + r.gitPrelude() + "D=" + D + "\n" + `if [ ! -d "$D/.git" ]; then
  if [ -d "$D" ] && [ -n "$(ls -A "$D" 2>/dev/null)" ]; then exit 90; fi
  mkdir -p "$D"
  echo "Cloning into $D"
  G clone --no-checkout ` + core.Q(a.Repo) + ` "$D"
fi
cd "$D"
G remote set-url origin ` + core.Q(a.Repo) + ` 2>/dev/null || G remote add origin ` + core.Q(a.Repo) + `
echo "Fetching origin"
G fetch --prune --tags --force origin '+refs/heads/*:refs/remotes/origin/*'
`
		fctx, cancel := context.WithTimeout(ctx, time.Duration(a.CommandTimeout)*time.Minute)
		code, err := r.stream(fctx, script, a.Sudo, r.gitStdin())
		cancel()
		if err != nil {
			return err
		}
		switch code {
		case 0:
		case 90:
			return apperr.New("deploy.dirNotEmpty", "dir", a.Dir)
		default:
			return apperr.New("deploy.fetchFailed", "code", itoa(code))
		}
	}

	// Resolve the ref: remote branch, tag, then commit.
	resolve := `cd ` + D + ` || exit 1
R=` + core.Q(ref) + `
for c in "refs/remotes/origin/$R" "refs/tags/$R" "$R"; do
  if s=$(` + G + ` rev-parse -q --verify "$c^{commit}" 2>/dev/null); then echo "$c $s"; exit 0; fi
done
exit 1`
	res, err := r.quiet(ctx, resolve, a.Sudo, "")
	if err != nil {
		return err
	}
	f := strings.Fields(res.Stdout)
	if res.ExitCode != 0 || len(f) != 2 {
		return apperr.New("deploy.refNotFound", "ref", ref)
	}
	sha := f[1]
	switch {
	case strings.HasPrefix(f[0], "refs/remotes/origin/"):
		d.RefKind, d.Branch = "branch", ref
	case strings.HasPrefix(f[0], "refs/tags/"):
		d.RefKind = "tag"
	default:
		d.RefKind = "commit"
	}
	d.Target = sha

	checkout := "set -e\n" + r.gitPrelude() + "cd " + D + "\nG checkout --force --detach " + core.Q(sha) + "\n"
	if a.Submodules {
		checkout += "if [ -f .gitmodules ]; then G submodule sync --recursive; G submodule update --init --recursive --force; fi\n"
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(a.CommandTimeout)*time.Minute)
	code, err := r.stream(cctx, checkout, a.Sudo, r.gitStdin())
	cancel()
	if err != nil {
		return err
	}
	if code != 0 {
		return apperr.New("deploy.checkoutFailed", "code", itoa(code))
	}
	res, err = r.quiet(ctx, "cd "+D+" && "+G+" log -1 --format=%H%x1f%h%x1f%an%x1f%ct%x1f%s "+core.Q(sha), a.Sudo, "")
	if err == nil && res.ExitCode == 0 {
		p := strings.SplitN(strings.TrimRight(res.Stdout, "\n"), "\x1f", 5)
		if len(p) == 5 {
			ts, _ := strconv.ParseInt(p[3], 10, 64)
			d.Commit = &Commit{SHA: p[0], Short: p[1], Author: p[2], Time: ts, Subject: p[4]}
		}
	}
	if d.Commit == nil {
		d.Commit = &Commit{SHA: sha, Short: sha[:min(len(sha), 7)]}
	}
	r.logf("✓ %s %s — %s (%s)", d.Commit.Short, d.RefKind, d.Commit.Subject, d.Commit.Author)
	return nil
}

func (r *runner) composeRelease(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	tag := core.FirstNonEmpty(spec.target, spec.ref, a.DefaultTag)
	if !ValidTag(tag) {
		return apperr.New("deploy.invalidTag", "tag", tag)
	}
	d.Ref, d.RefKind, d.Target = core.FirstNonEmpty(spec.ref, tag), "image", tag
	// The tag currently in the env file.
	res, err := r.quiet(ctx, "grep -E "+core.Q("^(export )?"+a.TagVar+"=")+" "+core.Q(a.envPath())+" 2>/dev/null | tail -n 1", a.Sudo, "")
	if err == nil {
		if prev := parseEnvValue(res.Stdout, a.TagVar); prev != "" {
			d.PreviousVersion, d.PreviousTarget = prev, prev
			r.logf("Current: %s=%s", a.TagVar, prev)
		}
	}
	if a.Type == TypeImage {
		content := renderCompose(a)
		res, err := r.quiet(ctx, writeFileScript(a.composePath(), "644"), a.Sudo, content)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return apperr.New("deploy.writeFailed", "path", a.composePath()).WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
		}
		r.logf("✓ wrote %s", a.composePath())
	}
	res, err = r.quiet(ctx, "test -f "+core.Q(a.composePath()), a.Sudo, "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.New("deploy.composeFileMissing", "path", a.composePath())
	}
	img := ""
	if a.Type == TypeImage {
		img = a.Image.Image + ":"
	}
	r.logf("✓ release %s%s (%s)", img, tag, a.TagVar)
	return nil
}

func (r *runner) writeEnv(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	var extra []EnvVar
	if a.Type != TypeGit {
		extra = []EnvVar{{Name: a.TagVar, Value: d.Target}}
	}
	if len(a.Vars) == 0 && len(r.env) == 0 && len(extra) == 0 {
		return errSkip
	}
	content, err := renderEnv(a.Vars, r.env, extra)
	if err != nil {
		return err
	}
	res, err := r.quiet(ctx, writeFileScript(a.envPath(), "600"), a.Sudo, content)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.New("deploy.writeFailed", "path", a.envPath()).WithDetail(r.red.Redact(core.FirstNonEmpty(res.Stderr, res.Stdout)))
	}
	r.logf("✓ %s (0600): %d variables, %d secrets", a.envPath(), len(a.Vars)+len(extra), len(r.env))
	return nil
}

func (r *runner) build(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	if a.Type == TypeGit {
		if a.BuildCmd == "" {
			return errSkip
		}
		return r.runScript(ctx, "build", r.userScript(a.BuildCmd), a.Sudo)
	}
	if a.BuildCmd != "" {
		if err := r.runScript(ctx, "build", r.userScript(a.BuildCmd), a.Sudo); err != nil {
			return err
		}
	}
	dc := composeCmd(r.composeBin, a)
	script := "cd " + core.Q(a.Dir) + " || exit 1\nset -e\n" + dc + " config -q\necho \"✓ compose file is valid\"\n"
	if a.SkipPull {
		script += "echo \"(pull skipped)\"\n"
	} else {
		script += dc + " pull\n"
	}
	return r.runScript(ctx, "pull", script, a.Sudo)
}

func (r *runner) test(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	if a.TestCmd == "" {
		return errSkip
	}
	return r.runScript(ctx, "test", r.userScript(a.TestCmd), a.Sudo)
}

func (r *runner) restart(ctx context.Context, d *RunData, spec runSpec) error {
	a := &r.app
	if a.Type != TypeGit {
		dc := composeCmd(r.composeBin, a)
		if err := r.runScript(ctx, "restart", "cd "+core.Q(a.Dir)+" || exit 1\nset -e\n"+dc+" up -d --remove-orphans\n", a.Sudo); err != nil {
			return err
		}
	} else if a.RestartCmd == "" {
		return errSkip
	}
	if a.RestartCmd == "" {
		return nil
	}
	return r.runScript(ctx, "restart", r.userScript(a.RestartCmd), a.Sudo || a.RestartSudo)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *runner) health(ctx context.Context, d *RunData, spec runSpec) error {
	h := r.app.Health
	if h.Type != "http" && h.Type != "command" {
		return errSkip
	}
	if h.Delay > 0 {
		r.logf("waiting %ds", h.Delay)
		if err := sleepCtx(ctx, time.Duration(h.Delay)*time.Second); err != nil {
			return err
		}
	}
	for i := 1; i <= h.Retries; i++ {
		ok, msg, err := r.healthOnce(ctx)
		if err != nil {
			return err
		}
		mark := "\x1b[31m✕\x1b[0m"
		if ok {
			mark = "\x1b[32m✓\x1b[0m"
		}
		r.logf("%s attempt %d/%d: %s", mark, i, h.Retries, msg)
		if ok {
			return nil
		}
		if i < h.Retries {
			if err := sleepCtx(ctx, time.Duration(h.Interval)*time.Second); err != nil {
				return err
			}
		}
	}
	return apperr.New("deploy.healthFailed", "attempts", itoa(h.Retries))
}

func (r *runner) healthOnce(ctx context.Context) (bool, string, error) {
	h := r.app.Health
	actx, cancel := context.WithTimeout(ctx, time.Duration(h.Timeout+15)*time.Second)
	defer cancel()
	if h.Type == "command" {
		code, err := r.stream(actx, r.userScript(h.Command), r.app.Sudo, "")
		if err != nil {
			if ctx.Err() == nil && actx.Err() != nil {
				return false, "timeout", nil
			}
			return false, "", err
		}
		return code == 0, "exit " + itoa(code), nil
	}
	t := itoa(h.Timeout)
	k, wk := "", ""
	if h.Insecure {
		k, wk = " -k", " --no-check-certificate"
	}
	script := `U=` + core.Q(h.URL) + `
B=$(mktemp); H=$(mktemp); trap 'rm -f "$B" "$H"' EXIT
if command -v curl >/dev/null 2>&1; then
  code=$(curl -sS -o "$B" -w '%{http_code}' --max-time ` + t + k + ` "$U" 2>"$H") || true
else
  wget -S -O "$B" -T ` + t + wk + ` "$U" 2>"$H" || true
  code=$(sed -n 's/^ *HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$H" | tail -n 1)
fi
echo "STATUS=${code:-000}"
echo "ERR=$(grep -iE 'curl:|wget:|error|refused|timed out' "$H" | head -n 1 | tr -d '\r')"
`
	if h.BodyContains != "" {
		script += `if grep -F -q -e ` + core.Q(h.BodyContains) + ` "$B"; then echo BODY=1; else echo BODY=0; fi` + "\n"
	}
	res, err := r.quiet(actx, script, false, "")
	if err != nil {
		if ctx.Err() == nil && actx.Err() != nil {
			return false, "timeout", nil
		}
		return false, "", err
	}
	kv := parseKV(res.Stdout)
	code, _ := strconv.Atoi(kv["STATUS"])
	if code == 0 {
		return false, "no response " + r.red.Redact(kv["ERR"]), nil
	}
	okStatus := code >= 200 && code < 400
	if h.ExpectStatus != 0 {
		okStatus = code == h.ExpectStatus
	}
	msg := "HTTP " + itoa(code)
	if !okStatus {
		if h.ExpectStatus != 0 {
			msg += " (expected " + itoa(h.ExpectStatus) + ")"
		}
		return false, msg, nil
	}
	if h.BodyContains != "" && kv["BODY"] != "1" {
		return false, msg + ", body does not contain the expected text", nil
	}
	return true, msg, nil
}

// ---- persistence of runs ----

func (s *DeployService) insertRun(id string, a *App, actor string, d RunData) error {
	if d.Steps == nil {
		d.Steps = []Step{}
		for _, sid := range stepIDs {
			d.Steps = append(d.Steps, Step{ID: sid, State: StatePending})
		}
	}
	b, _ := json.Marshal(d)
	_, err := s.core.DB.Exec(`INSERT INTO runs(id, kind, server, ref, started, finished, status, actor, version, data, log)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, "deploy", a.Server, a.ID, time.Now().UnixMilli(), 0, "running", actor, d.Ref, string(b), "")
	return err
}

func (s *DeployService) finishRun(id string, res runResult, log string) {
	b, _ := json.Marshal(res.data)
	if _, err := s.core.DB.Exec(`UPDATE runs SET finished=?, status=?, version=?, data=?, log=? WHERE id=?`,
		time.Now().UnixMilli(), res.status, res.version, string(b), log, id); err != nil {
		fmt.Fprintf(os.Stderr, "deploy: saving run %s: %v\n", id, err)
	}
}

func (s *DeployService) setRolledBackBy(id, by string) {
	var data string
	if err := s.core.DB.QueryRow(`SELECT data FROM runs WHERE id=?`, id).Scan(&data); err != nil {
		return
	}
	var d RunData
	if json.Unmarshal([]byte(data), &d) != nil {
		return
	}
	d.RolledBackBy = by
	b, _ := json.Marshal(d)
	_, _ = s.core.DB.Exec(`UPDATE runs SET data=? WHERE id=?`, string(b), id)
}
