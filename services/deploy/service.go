// Package deploy: Application deployments (git / Docker Compose), environment and secrets, health checks, history and rollback.
package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/store"
)

// EventStep is emitted on every pipeline step change.
const EventStep = "deploy:step"

func init() {
	application.RegisterEvent[StepEvent](EventStep)
}

type DeployService struct {
	core *core.Core

	mu     sync.Mutex
	active map[string]*liveRun // appID -> running deployment
	live   map[string]*liveRun // runID -> running deployment (incl. auto-rollback runs)
}

func New(c *core.Core) *DeployService {
	return &DeployService{core: c, active: map[string]*liveRun{}, live: map[string]*liveRun{}}
}

// ServiceStartup marks deployments left "running" by a previous app
// session (crash, quit mid-deploy) as interrupted.
func (s *DeployService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	s.markInterrupted()
	return nil
}

func (s *DeployService) markInterrupted() {
	_, _ = s.core.DB.Exec(`UPDATE runs SET status='interrupted', finished=started WHERE kind='deploy' AND status='running'`)
}

// ---- public types ----

// Step states.
const (
	StatePending = "pending"
	StateRunning = "running"
	StateOK      = "ok"
	StateFailed  = "failed"
	StateSkipped = "skipped"
)

// Pipeline step ids, in order.
var stepIDs = []string{"preflight", "fetch", "env", "build", "test", "restart", "health", "done"}

// Step is one stage of a deployment.
type Step struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	StartedAt  int64  `json:"startedAt"`  // unix ms
	FinishedAt int64  `json:"finishedAt"` // unix ms
	Note       string `json:"note"`
}

// StepEvent is the payload of "deploy:step".
type StepEvent struct {
	Server     string `json:"server"`
	AppID      string `json:"appId"`
	JobID      string `json:"jobId"`
	RunID      string `json:"runId"`
	Step       string `json:"step"`
	State      string `json:"state"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	Note       string `json:"note"`
}

// Commit describes the deployed git commit.
type Commit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Time    int64  `json:"time"` // unix s
}

// RunData is stored as JSON in runs.data.
type RunData struct {
	AppName string `json:"appName"`
	AppType string `json:"appType"`
	// Trigger: manual | redeploy | rollback | auto-rollback.
	Trigger    string `json:"trigger"`
	RollbackOf string `json:"rollbackOf"` // run id this rollback undoes (auto) or the run whose version it restores (manual)
	// Ref is what was asked for (branch, tag, sha or image tag); RefKind is
	// branch | tag | commit | image.
	Ref     string  `json:"ref"`
	RefKind string  `json:"refKind"`
	Branch  string  `json:"branch"`
	Target  string  `json:"target"` // full sha or image tag actually deployed
	Commit  *Commit `json:"commit"`
	// What was deployed before this run.
	PreviousVersion string        `json:"previousVersion"`
	PreviousTarget  string        `json:"previousTarget"`
	Steps           []Step        `json:"steps"`
	Error           *apperr.Error `json:"error"`
	JobID           string        `json:"jobId"`
	// Set on a failed run that was automatically rolled back.
	RolledBackBy string `json:"rolledBackBy"`
}

// Run is a deployment in the history (without its log).
type Run struct {
	ID       string  `json:"id"`
	AppID    string  `json:"appId"`
	Server   string  `json:"server"`
	Started  int64   `json:"started"`  // unix ms
	Finished int64   `json:"finished"` // unix ms, 0 while running
	Status   string  `json:"status"`   // running | ok | failed | cancelled | interrupted
	Actor    string  `json:"actor"`
	Version  string  `json:"version"`
	Data     RunData `json:"data"`
}

// RunDetail is a run with its stored (redacted) log.
type RunDetail struct {
	Run Run    `json:"run"`
	Log string `json:"log"`
}

// DeployStart identifies a started deployment.
type DeployStart struct {
	JobID string `json:"jobId"`
	RunID string `json:"runId"`
	AppID string `json:"appId"`
}

// AppView is an app with its deployment state.
type AppView struct {
	App App `json:"app"`
	// Current is the last successful deployment (what is live).
	Current *Run `json:"current"`
	// Last is the most recent deployment, whatever its outcome.
	Last *Run `json:"last"`
	// Active is set while a deployment runs.
	Active *DeployStart `json:"active"`
}

// SecretChange sets (Value) or removes a secret.
type SecretChange struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Remove bool   `json:"remove"`
}

// SaveAppInput is the editor's payload. Secret values and the token are
// write-only: they are stored in the OS keychain and never returned.
type SaveAppInput struct {
	App     App            `json:"app"`
	Secrets []SecretChange `json:"secrets"`
	// TokenAction: "" / "keep" | "set" | "remove".
	TokenAction string `json:"tokenAction"`
	Token       string `json:"token"`
}

// GitRef is a branch or tag of the remote repository.
type GitRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // branch | tag
	SHA  string `json:"sha"`
}

// ---- keychain ----

func secretKey(appID, name string) string { return "deploy:" + appID + ":secret:" + name }
func tokenKey(appID string) string        { return "deploy:" + appID + ":git-token" }

// ---- apps ----

func (s *DeployService) loadApp(connID, id string) (App, error) {
	if !reAppID.MatchString(id) {
		return App{}, apperr.New("deploy.appNotFound")
	}
	var a App
	if err := s.core.DB.Get(nsApp, id, &a); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return App{}, apperr.New("deploy.appNotFound")
		}
		return App{}, err
	}
	if a.Server != connID {
		return App{}, apperr.New("deploy.appNotFound")
	}
	a.HasToken = a.Type == TypeGit && store.Keychain(tokenKey(a.ID)) != ""
	if a.Vars == nil {
		a.Vars = []EnvVar{}
	}
	if a.Secrets == nil {
		a.Secrets = []string{}
	}
	if a.Image.Ports == nil {
		a.Image.Ports = []string{}
	}
	if a.Image.Volumes == nil {
		a.Image.Volumes = []string{}
	}
	return a, nil
}

func (s *DeployService) view(a App) AppView {
	v := AppView{App: a}
	if r, err := s.queryRuns(`ref=? AND server=? AND status='ok'`, 1, a.ID, a.Server); err == nil && len(r) > 0 {
		v.Current = &r[0]
	}
	if r, err := s.queryRuns(`ref=? AND server=?`, 1, a.ID, a.Server); err == nil && len(r) > 0 {
		v.Last = &r[0]
	}
	s.mu.Lock()
	if lr := s.active[a.ID]; lr != nil {
		v.Active = &DeployStart{JobID: lr.jobID, RunID: lr.currentRun(), AppID: a.ID}
	}
	s.mu.Unlock()
	return v
}

// Apps lists the server's applications with their deployment state.
func (s *DeployService) Apps(connID string) ([]AppView, error) {
	all, err := db.List[App](s.core.DB, nsApp)
	if err != nil {
		return nil, err
	}
	out := []AppView{}
	for _, a := range all {
		if a.Server != connID {
			continue
		}
		full, err := s.loadApp(connID, a.ID)
		if err != nil {
			continue
		}
		out = append(out, s.view(full))
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].App.Name) < strings.ToLower(out[j].App.Name)
	})
	return out, nil
}

// GetApp returns one application.
func (s *DeployService) GetApp(connID, appID string) (AppView, error) {
	a, err := s.loadApp(connID, appID)
	if err != nil {
		return AppView{}, err
	}
	return s.view(a), nil
}

// SaveApp creates or updates an application, its secrets and git token.
func (s *DeployService) SaveApp(connID string, in SaveAppInput) (AppView, error) {
	if err := s.core.Require(connID, core.PermDeploy); err != nil {
		return AppView{}, err
	}
	a := in.App
	creating := a.ID == ""
	var prev App
	if creating {
		a.ID = uuid.NewString()
		a.Created = time.Now().UnixMilli()
		a.Secrets = []string{}
	} else {
		p, err := s.loadApp(connID, a.ID)
		if err != nil {
			return AppView{}, err
		}
		prev = p
		a.Created = p.Created
		a.Secrets = append([]string{}, p.Secrets...)
	}
	a.Server = connID
	a.Updated = time.Now().UnixMilli()
	a.HasToken = false

	// Apply secret changes to the name list (values are validated here and
	// written to the keychain only once everything validated).
	set := map[string]string{}
	removed := map[string]bool{}
	for _, ch := range in.Secrets {
		ch.Name = strings.TrimSpace(ch.Name)
		if !ValidEnvName(ch.Name) {
			return AppView{}, apperr.New("deploy.invalidEnvName", "name", ch.Name)
		}
		if ch.Remove {
			removed[ch.Name] = true
			delete(set, ch.Name)
			continue
		}
		if len(ch.Value) < minSecretLen {
			return AppView{}, apperr.New("deploy.secretTooShort", "name", ch.Name, "min", itoa(minSecretLen))
		}
		if err := checkEnvValue(ch.Name, ch.Value); err != nil {
			return AppView{}, err
		}
		set[ch.Name] = ch.Value
		delete(removed, ch.Name)
	}
	names := []string{}
	for _, n := range a.Secrets {
		if !removed[n] {
			names = append(names, n)
		}
	}
	for n := range set {
		if !containsStr(names, n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	a.Secrets = names
	if err := a.normalize(); err != nil {
		return AppView{}, err
	}
	switch in.TokenAction {
	case "", "keep", "remove":
	case "set":
		if a.Type != TypeGit {
			return AppView{}, invalid("token")
		}
		if len(in.Token) < minSecretLen || len(in.Token) > 4096 || strings.ContainsAny(in.Token, "\r\n\x00 ") {
			return AppView{}, invalid("token")
		}
	default:
		return AppView{}, invalid("tokenAction")
	}

	// Keychain first: a failure leaves the app definition unchanged.
	for n, v := range set {
		if err := store.SetKeychain(secretKey(a.ID, n), v); err != nil {
			return AppView{}, err
		}
		s.core.Audit(connID, "deploy.secret.set", a.Name+"/"+n, "", nil)
	}
	for n := range removed {
		_ = store.SetKeychain(secretKey(a.ID, n), "")
		s.core.Audit(connID, "deploy.secret.remove", a.Name+"/"+n, "", nil)
	}
	switch {
	case in.TokenAction == "set":
		if err := store.SetKeychain(tokenKey(a.ID), in.Token); err != nil {
			return AppView{}, err
		}
		s.core.Audit(connID, "deploy.token.set", a.Name, "", nil)
	case in.TokenAction == "remove" || (a.Type != TypeGit && prev.HasToken):
		_ = store.SetKeychain(tokenKey(a.ID), "")
		s.core.Audit(connID, "deploy.token.remove", a.Name, "", nil)
	}
	stored := a
	stored.HasToken = false
	err := s.core.DB.Put(nsApp, a.ID, stored)
	action := "deploy.app.update"
	if creating {
		action = "deploy.app.create"
	}
	s.core.Audit(connID, action, a.Name, appSummary(&a), err)
	if err != nil {
		return AppView{}, err
	}
	full, err := s.loadApp(connID, a.ID)
	if err != nil {
		return AppView{}, err
	}
	return s.view(full), nil
}

// appSummary describes an app's configuration for the audit log (no secrets).
func appSummary(a *App) string {
	parts := []string{"type=" + a.Type, "stage=" + a.Stage, "dir=" + a.Dir}
	switch a.Type {
	case TypeGit:
		parts = append(parts, "repo="+a.Repo, "branch="+a.Branch)
	case TypeCompose:
		parts = append(parts, "compose="+a.ComposeFile, "tagVar="+a.TagVar)
	case TypeImage:
		parts = append(parts, "image="+a.Image.Image, "tagVar="+a.TagVar)
	}
	vars := []string{}
	for _, v := range a.Vars {
		vars = append(vars, v.Name)
	}
	if len(vars) > 0 {
		parts = append(parts, "vars="+strings.Join(vars, ","))
	}
	if len(a.Secrets) > 0 {
		parts = append(parts, "secrets="+strings.Join(a.Secrets, ","))
	}
	parts = append(parts, "health="+a.Health.Type)
	if a.AutoRollback {
		parts = append(parts, "autoRollback")
	}
	if a.Sudo {
		parts = append(parts, "sudo")
	}
	return strings.Join(parts, " ")
}

// DeleteApp removes an application, its secrets and history (files on the
// server are left alone).
func (s *DeployService) DeleteApp(connID, appID string) error {
	if err := s.core.Require(connID, core.PermDeploy); err != nil {
		return err
	}
	a, err := s.loadApp(connID, appID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	busy := s.active[appID] != nil
	s.mu.Unlock()
	if busy {
		return apperr.New("deploy.busy", "app", a.Name)
	}
	for _, n := range a.Secrets {
		_ = store.SetKeychain(secretKey(a.ID, n), "")
	}
	_ = store.SetKeychain(tokenKey(a.ID), "")
	err = s.core.DB.Delete(nsApp, a.ID)
	if err == nil {
		_, err = s.core.DB.Exec(`DELETE FROM runs WHERE kind='deploy' AND ref=?`, a.ID)
	}
	s.core.Audit(connID, "deploy.app.delete", a.Name, appSummary(&a), err)
	return err
}

// ---- history ----

func (s *DeployService) queryRuns(where string, limit int, args ...any) ([]Run, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.core.DB.Query(`SELECT id, ref, server, started, finished, status, actor, version, data FROM runs
		WHERE kind='deploy' AND `+where+` ORDER BY started DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		var data string
		if err := rows.Scan(&r.ID, &r.AppID, &r.Server, &r.Started, &r.Finished, &r.Status, &r.Actor, &r.Version, &data); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(data), &r.Data)
		if r.Data.Steps == nil {
			r.Data.Steps = []Step{}
		}
		s.overlayLive(&r)
		out = append(out, r)
	}
	return out, rows.Err()
}

// overlayLive replaces a running run's steps with the in-memory state.
func (s *DeployService) overlayLive(r *Run) {
	if r.Status != "running" {
		return
	}
	s.mu.Lock()
	lr := s.live[r.ID]
	s.mu.Unlock()
	if lr != nil {
		r.Data = lr.snapshot(r.ID, r.Data)
	}
}

// Runs lists an application's deployments, newest first.
func (s *DeployService) Runs(connID, appID string, limit int) ([]Run, error) {
	if _, err := s.loadApp(connID, appID); err != nil {
		return nil, err
	}
	return s.queryRuns(`ref=? AND server=?`, limit, appID, connID)
}

// RunDetail returns one deployment with its log.
func (s *DeployService) RunDetail(connID, runID string) (RunDetail, error) {
	if !reRunID.MatchString(runID) {
		return RunDetail{}, apperr.New("deploy.runNotFound")
	}
	var d RunDetail
	var data string
	err := s.core.DB.QueryRow(`SELECT id, ref, server, started, finished, status, actor, version, data, log FROM runs
		WHERE kind='deploy' AND id=? AND server=?`, runID, connID).
		Scan(&d.Run.ID, &d.Run.AppID, &d.Run.Server, &d.Run.Started, &d.Run.Finished, &d.Run.Status, &d.Run.Actor, &d.Run.Version, &data, &d.Log)
	if errors.Is(err, sql.ErrNoRows) {
		return RunDetail{}, apperr.New("deploy.runNotFound")
	}
	if err != nil {
		return RunDetail{}, err
	}
	_ = json.Unmarshal([]byte(data), &d.Run.Data)
	if d.Run.Data.Steps == nil {
		d.Run.Data.Steps = []Step{}
	}
	s.overlayLive(&d.Run)
	return d, nil
}

// Active lists the deployments running on this server.
func (s *DeployService) Active(connID string) []DeployStart {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []DeployStart{}
	for appID, lr := range s.active {
		if lr.server == connID {
			out = append(out, DeployStart{JobID: lr.jobID, RunID: lr.currentRun(), AppID: appID})
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
