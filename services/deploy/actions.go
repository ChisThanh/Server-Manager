package deploy

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// DeployOptions chooses what to deploy.
type DeployOptions struct {
	// Ref: git branch, tag or commit sha; or the image tag. "" = the app's
	// default branch / tag.
	Ref string `json:"ref"`
	// RedeployOf: deploy exactly the version of this earlier run.
	RedeployOf string `json:"redeployOf"`
}

// Deploy starts a deployment job. The job id and run id are returned; the
// pipeline reports progress with "deploy:step" events.
func (s *DeployService) Deploy(connID, appID string, opts DeployOptions, sudoPassword string) (DeployStart, error) {
	if err := s.core.Require(connID, core.PermDeploy); err != nil {
		return DeployStart{}, err
	}
	a, err := s.loadApp(connID, appID)
	if err != nil {
		return DeployStart{}, err
	}
	spec := runSpec{id: uuid.NewString(), trigger: TriggerManual, ref: strings.TrimSpace(opts.Ref)}
	if opts.RedeployOf != "" {
		prev, err := s.runOf(connID, a.ID, opts.RedeployOf)
		if err != nil {
			return DeployStart{}, err
		}
		if prev.Data.Target == "" || prev.Data.AppType != a.Type {
			return DeployStart{}, apperr.New("deploy.noTarget")
		}
		spec.trigger, spec.target, spec.ref = TriggerRedeploy, prev.Data.Target, prev.Data.Ref
		spec.rollbackOf = prev.ID
	}
	if spec.ref != "" {
		if a.Type == TypeGit && !ValidRef(spec.ref) {
			return DeployStart{}, apperr.New("deploy.invalidRef", "ref", spec.ref)
		}
		if a.Type != TypeGit && !ValidTag(spec.ref) {
			return DeployStart{}, apperr.New("deploy.invalidTag", "tag", spec.ref)
		}
	}
	return s.start(connID, a, spec, sudoPassword)
}

// Rollback redeploys the version of a successful run: runID, or "" for the
// previous successful version.
func (s *DeployService) Rollback(connID, appID, runID, sudoPassword string) (DeployStart, error) {
	if err := s.core.Require(connID, core.PermDeploy); err != nil {
		return DeployStart{}, err
	}
	a, err := s.loadApp(connID, appID)
	if err != nil {
		return DeployStart{}, err
	}
	var target Run
	if runID != "" {
		target, err = s.runOf(connID, a.ID, runID)
		if err != nil {
			return DeployStart{}, err
		}
		if target.Status != "ok" {
			return DeployStart{}, apperr.New("deploy.rollbackNotOk")
		}
	} else {
		t, ok := s.previousTarget(&a)
		if !ok {
			return DeployStart{}, apperr.New("deploy.noPrevious")
		}
		target = t
	}
	if target.Data.Target == "" || target.Data.AppType != a.Type {
		return DeployStart{}, apperr.New("deploy.noTarget")
	}
	spec := runSpec{id: uuid.NewString(), trigger: TriggerRollback, ref: target.Data.Ref, target: target.Data.Target, rollbackOf: target.ID}
	return s.start(connID, a, spec, sudoPassword)
}

// previousTarget finds what "roll back" means now: if the last deployment
// failed, the last good one; otherwise the newest good one with another
// version than the current.
func (s *DeployService) previousTarget(a *App) (Run, bool) {
	runs, err := s.queryRuns(`ref=? AND server=? AND status<>'running'`, 200, a.ID, a.Server)
	if err != nil || len(runs) == 0 {
		return Run{}, false
	}
	var current *Run
	for i := range runs {
		if runs[i].Status == "ok" {
			current = &runs[i]
			break
		}
	}
	if current == nil {
		return Run{}, false
	}
	if runs[0].Status != "ok" && runs[0].Data.Target != current.Data.Target {
		return *current, true
	}
	for _, r := range runs {
		if r.Status == "ok" && r.Started < current.Started && r.Data.Target != current.Data.Target && r.Data.AppType == a.Type {
			return r, true
		}
	}
	return Run{}, false
}

func (s *DeployService) runOf(connID, appID, runID string) (Run, error) {
	if !reRunID.MatchString(runID) {
		return Run{}, apperr.New("deploy.runNotFound")
	}
	runs, err := s.queryRuns(`id=? AND ref=? AND server=?`, 1, runID, appID, connID)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, apperr.New("deploy.runNotFound")
	}
	return runs[0], nil
}

func (s *DeployService) start(connID string, a App, spec runSpec, pw string) (DeployStart, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return DeployStart{}, err
	}
	// Check sudo now so the UI can prompt for the password before the job.
	if a.Sudo || a.RestartSudo {
		ctx, cancel := core.Timeout(30 * time.Second)
		_, err := s.core.Run(ctx, conn, "true", true, pw, "")
		cancel()
		if err != nil {
			return DeployStart{}, err
		}
	}
	for _, n := range a.Secrets {
		if store.Keychain(secretKey(a.ID, n)) == "" {
			return DeployStart{}, apperr.New("deploy.secretMissing", "name", n)
		}
	}
	s.mu.Lock()
	if s.active[a.ID] != nil {
		s.mu.Unlock()
		return DeployStart{}, apperr.New("deploy.busy", "app", a.Name)
	}
	for _, other := range s.active {
		if other.server == connID && other.dir == a.Dir {
			s.mu.Unlock()
			return DeployStart{}, apperr.New("deploy.busy", "app", a.Name)
		}
	}
	lr := newLiveRun(connID, a.ID)
	lr.dir = a.Dir
	s.active[a.ID] = lr
	s.live[spec.id] = lr
	s.mu.Unlock()

	actor := s.core.Actor()
	if err := s.insertRun(spec.id, &a, actor, RunData{AppName: a.Name, AppType: a.Type, Trigger: spec.trigger, Ref: spec.ref, RollbackOf: spec.rollbackOf}); err != nil {
		s.finishLive(lr)
		return DeployStart{}, err
	}
	kind := "deploy.run"
	if spec.trigger == TriggerRollback {
		kind = "deploy.rollback"
	}
	title := a.Name
	if v := core.FirstNonEmpty(spec.ref, shortVersion(spec.target)); v != "" {
		title += " @ " + v
	}
	ready := make(chan struct{})
	j := s.core.Jobs.Start(connID, kind, title, func(ctx context.Context, j *core.Job) error {
		<-ready
		return s.runJob(ctx, j, conn, a, spec, pw, lr, actor)
	})
	lr.setJob(j.ID())
	close(ready)
	return DeployStart{JobID: j.ID(), RunID: spec.id, AppID: a.ID}, nil
}

func (s *DeployService) finishLive(lr *liveRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[lr.appID] == lr {
		delete(s.active, lr.appID)
	}
	for id, l := range s.live {
		if l == lr {
			delete(s.live, id)
		}
	}
}

func (s *DeployService) runJob(ctx context.Context, j *core.Job, conn *sshx.Conn, a App, spec runSpec, pw string, lr *liveRun, actor string) error {
	defer s.finishLive(lr)
	r := s.newRunner(conn, a, pw, j, lr)
	r.actor = actor
	defer r.releaseLock()
	res := r.runOnce(ctx, spec)
	if res.err == nil || ctx.Err() != nil {
		return res.err
	}
	if !a.AutoRollback || spec.trigger == TriggerAutoRollback || (res.failedStep != "restart" && res.failedStep != "health") {
		return res.err
	}
	// Auto-rollback to the version that was live before this run.
	prev, ok := s.lastGood(&a, spec.id)
	if !ok || prev.Data.Target == "" || prev.Data.Target == res.data.Target {
		r.logf("\x1b[33m! auto-rollback: no previous successful version to return to\x1b[0m")
		return res.err
	}
	rb := runSpec{id: uuid.NewString(), trigger: TriggerAutoRollback, ref: prev.Data.Ref, target: prev.Data.Target, rollbackOf: spec.id}
	s.mu.Lock()
	s.live[rb.id] = lr
	s.mu.Unlock()
	if err := s.insertRun(rb.id, &a, actor, RunData{AppName: a.Name, AppType: a.Type, Trigger: rb.trigger, Ref: rb.ref, RollbackOf: spec.id}); err != nil {
		return res.err
	}
	s.setRolledBackBy(spec.id, rb.id)
	r.logf("")
	r.logf("\x1b[1;33m↺ Auto-rollback to %s\x1b[0m", prev.Version)
	rres := r.runOnce(ctx, rb)
	if rres.err != nil {
		return apperr.New("deploy.rollbackFailed", "version", prev.Version, "step", res.failedStep)
	}
	return apperr.New("deploy.rolledBack", "version", prev.Version, "step", res.failedStep)
}

// lastGood is the newest successful run other than exclude.
func (s *DeployService) lastGood(a *App, exclude string) (Run, bool) {
	runs, err := s.queryRuns(`ref=? AND server=? AND status='ok' AND id<>?`, 1, a.ID, a.Server, exclude)
	if err != nil || len(runs) == 0 {
		return Run{}, false
	}
	return runs[0], runs[0].Data.AppType == a.Type
}

// RemoteRefs lists the repository's branches and tags (for the deploy dialog).
func (s *DeployService) RemoteRefs(connID, appID, sudoPassword string) ([]GitRef, error) {
	a, err := s.loadApp(connID, appID)
	if err != nil {
		return nil, err
	}
	if a.Type != TypeGit {
		return []GitRef{}, nil
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	r := &runner{s: s, conn: conn, app: a, pw: sudoPassword, red: NewRedactor()}
	r.token = store.Keychain(tokenKey(a.ID))
	r.red.Add(r.token)
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	res, err := s.core.Run(ctx, conn, "set -e\n"+r.gitPrelude()+"cd / && G ls-remote --heads --tags "+core.Q(a.Repo), a.Sudo, sudoPassword, r.gitStdin())
	if err != nil {
		return nil, r.cleanErr(err)
	}
	if res.ExitCode != 0 {
		return nil, apperr.New("deploy.fetchFailed", "code", itoa(res.ExitCode)).WithDetail(r.red.Redact(core.FirstNonEmpty(res.Stderr, res.Stdout)))
	}
	return parseLsRemote(res.Stdout), nil
}

func parseLsRemote(out string) []GitRef {
	refs := map[string]*GitRef{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		sha, name := f[0], f[1]
		var kind string
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			kind, name = "branch", strings.TrimPrefix(name, "refs/heads/")
		case strings.HasPrefix(name, "refs/tags/"):
			kind, name = "tag", strings.TrimPrefix(name, "refs/tags/")
		default:
			continue
		}
		peeled := strings.HasSuffix(name, "^{}")
		name = strings.TrimSuffix(name, "^{}")
		key := kind + ":" + name
		if g, ok := refs[key]; ok {
			if peeled {
				g.SHA = sha
			}
			continue
		}
		refs[key] = &GitRef{Name: name, Kind: kind, SHA: sha}
	}
	out2 := []GitRef{}
	for _, g := range refs {
		out2 = append(out2, *g)
	}
	sort.Slice(out2, func(i, j int) bool {
		if out2[i].Kind != out2[j].Kind {
			return out2[i].Kind == "branch"
		}
		if out2[i].Kind == "tag" {
			return out2[i].Name > out2[j].Name
		}
		return out2[i].Name < out2[j].Name
	})
	if len(out2) > 500 {
		out2 = out2[:500]
	}
	return out2
}

// ReleaseLock force-removes the app's remote deploy lock (left by a crashed
// or disconnected app instance). Refused while this instance deploys it.
func (s *DeployService) ReleaseLock(connID, appID, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDeploy); err != nil {
		return err
	}
	a, err := s.loadApp(connID, appID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	busy := s.active[a.ID] != nil
	s.mu.Unlock()
	if busy {
		return apperr.New("deploy.busy", "app", a.Name)
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	lock := lockPathFor(a.Dir)
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	res, err := s.core.Run(ctx, conn, "L="+core.Q(lock)+`; if [ -d "$L" ]; then cat "$L/info" 2>/dev/null; rm -rf "$L"; fi`, a.Sudo, sudoPassword, "")
	if err == nil && res.ExitCode != 0 {
		err = core.CmdError(res)
	}
	kv := parseKV(res.Stdout)
	detail := lock
	if kv["actor"] != "" {
		detail += " held by " + kv["actor"] + "@" + kv["host"]
	}
	s.core.Audit(connID, "deploy.lock.release", a.Name, detail, err)
	return err
}
