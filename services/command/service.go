// Package command: Command Center: run a command or action on many servers with limits, confirmation and audit.
package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/crypto/ssh"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/store"
)

// Event names emitted to the frontend.
const (
	EventUpdate   = "command:update"
	EventFinished = "command:finished"
)

func init() {
	application.RegisterEvent[ServerResult](EventUpdate)
	application.RegisterEvent[FinishedEvent](EventFinished)
}

// Limits.
const (
	DefaultTimeoutSec = 60
	MinTimeoutSec     = 5
	MaxTimeoutSec     = 3600
	MaxConcurrency    = 50
	MaxServers        = 1000
	MaxCommandBytes   = 64 << 10
	liveOutputMax     = 256 << 10 // per stream, sent to the UI
	storedOutputMax   = 16 << 10  // per stream, kept in history
	connectTimeout    = 30 * time.Second
	bgIdleClose       = 2 * time.Minute
	snippetNS         = "cmd.snippet"
	runKind           = "command"
)

// Per-server states.
const (
	StateQueued     = "queued"
	StateConnecting = "connecting"
	StateRunning    = "running"
	StateDone       = "done"
	StateFailed     = "failed"
	StateTimeout    = "timeout"
	StateCancelled  = "cancelled"
	StateSkipped    = "skipped"
)

type CommandService struct {
	core *core.Core

	mu   sync.Mutex
	runs map[string]*run // active runs

	connMu sync.Mutex
	conns  map[string]*connRef
}

func New(c *core.Core) *CommandService {
	return &CommandService{core: c, runs: map[string]*run{}, conns: map[string]*connRef{}}
}

// ServiceStartup marks runs left "running" by a crash as interrupted.
func (s *CommandService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	s.markInterrupted()
	return nil
}

// ServiceShutdown cancels active runs.
func (s *CommandService) ServiceShutdown() error {
	s.mu.Lock()
	active := make([]*run, 0, len(s.runs))
	for _, r := range s.runs {
		active = append(active, r)
	}
	s.mu.Unlock()
	for _, r := range active {
		r.cancel()
	}
	for _, r := range active {
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	}
	s.connMu.Lock()
	for _, ref := range s.conns {
		if ref.timer != nil {
			ref.timer.Stop()
		}
	}
	s.connMu.Unlock()
	return nil
}

func (s *CommandService) markInterrupted() {
	if _, err := s.core.DB.Exec(`UPDATE runs SET status='interrupted', finished=? WHERE kind=? AND status='running'`,
		time.Now().UnixMilli(), runKind); err != nil {
		return
	}
}

// ---- types ----

// Spec describes what to run and where.
type Spec struct {
	Servers []string `json:"servers"`
	// Kind is "command" (free script) or "preset".
	Kind    string            `json:"kind"`
	Command string            `json:"command"`
	Preset  string            `json:"preset"`
	Params  map[string]string `json:"params"`
	// Snippet is the name of the saved snippet the command came from (display only).
	Snippet       string `json:"snippet"`
	Sudo          bool   `json:"sudo"`
	TimeoutSec    int    `json:"timeoutSec"`
	Concurrency   int    `json:"concurrency"`
	StopOnFailure bool   `json:"stopOnFailure"`
}

// ServerResult is the state of one server in a run; it is also the payload
// of the command:update event.
type ServerResult struct {
	RunID       string        `json:"runId"`
	ServerID    string        `json:"serverId"`
	Name        string        `json:"name"`
	Environment string        `json:"environment"`
	State       string        `json:"state"`
	ExitCode    int           `json:"exitCode"`
	Stdout      string        `json:"stdout"`
	Stderr      string        `json:"stderr"`
	Truncated   bool          `json:"truncated"`
	Error       *apperr.Error `json:"error"`
	StartedAt   int64         `json:"startedAt"`  // unix ms
	FinishedAt  int64         `json:"finishedAt"` // unix ms
}

// Summary counts servers by outcome.
type Summary struct {
	Total     int `json:"total"`
	Queued    int `json:"queued"`
	Running   int `json:"running"` // connecting or running
	OK        int `json:"ok"`
	Failed    int `json:"failed"`
	Timeout   int `json:"timeout"`
	Cancelled int `json:"cancelled"`
	Skipped   int `json:"skipped"`
}

// RunInfo is a whole run (live or from history).
type RunInfo struct {
	ID       string         `json:"id"`
	Spec     Spec           `json:"spec"`
	Command  string         `json:"command"` // resolved script
	Sudo     bool           `json:"sudo"`    // effective
	Actor    string         `json:"actor"`
	Started  int64          `json:"started"`  // unix ms
	Finished int64          `json:"finished"` // 0 while running
	Status   string         `json:"status"`   // running | done | failed | cancelled | interrupted
	Summary  Summary        `json:"summary"`
	Results  []ServerResult `json:"results"`
	Dangers  []Danger       `json:"dangers"`
}

// FinishedEvent is the payload of command:finished.
type FinishedEvent struct {
	RunID    string  `json:"runId"`
	Status   string  `json:"status"`
	Summary  Summary `json:"summary"`
	Finished int64   `json:"finished"`
}

// HistoryItem is a past run in the history list.
type HistoryItem struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"` // command (first 200 chars) or preset id
	Kind     string  `json:"kind"`
	Preset   string  `json:"preset"`
	Snippet  string  `json:"snippet"`
	Actor    string  `json:"actor"`
	Started  int64   `json:"started"`
	Finished int64   `json:"finished"`
	Status   string  `json:"status"`
	Summary  Summary `json:"summary"`
}

// Target is a saved server with its readiness for the command center.
type Target struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Group       string   `json:"group"`
	Host        string   `json:"host"`
	User        string   `json:"user"`
	Port        int      `json:"port"`
	Color       string   `json:"color"`
	Environment string   `json:"environment"`
	Tags        []string `json:"tags"`
	Role        string   `json:"role"`
	// Readiness: connected | auto | needSecret | unavailable.
	Readiness string `json:"readiness"`
	// Via explains how it connects: ui | background | password | key | keyPassphrase | agent.
	Via string `json:"via"`
	// Reason is an error code when Readiness is unavailable.
	Reason string `json:"reason"`
	// CanExec is false when the role forbids running commands.
	CanExec bool `json:"canExec"`
	// Sudo: root | stored | unknown (sudo password not stored; works only with NOPASSWD).
	Sudo string `json:"sudo"`
}

// Preview is the dry-run view of a spec.
type Preview struct {
	Command     string          `json:"command"`
	Sudo        bool            `json:"sudo"`
	TimeoutSec  int             `json:"timeoutSec"`
	Concurrency int             `json:"concurrency"`
	Dangers     []Danger        `json:"dangers"`
	Danger      bool            `json:"danger"` // needs a typed confirmation
	Servers     []PreviewServer `json:"servers"`
}

// PreviewServer is what would happen on one server.
type PreviewServer struct {
	ServerID    string `json:"serverId"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	// Action: run | skip.
	Action  string `json:"action"`
	Reason  string `json:"reason"` // error code when skipped
	Via     string `json:"via"`
	Sudo    string `json:"sudo"`
	Command string `json:"command"` // exactly what is executed
}

// Snippet is a saved command.
type Snippet struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Command     string `json:"command"`
	Sudo        bool   `json:"sudo"`
	Description string `json:"description"`
	Created     int64  `json:"created"`
	Updated     int64  `json:"updated"`
}

// ---- catalog ----

// Presets lists the predefined actions.
func (s *CommandService) Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	for i := range out {
		if out[i].Params == nil {
			out[i].Params = []PresetParam{}
		}
		for j := range out[i].Params {
			if out[i].Params[j].Options == nil {
				out[i].Params[j].Options = []string{}
			}
		}
	}
	return out
}

// Render returns the script a preset runs with the given parameters.
func (s *CommandService) Render(preset string, params map[string]string) (Rendered, error) {
	return RenderPreset(preset, params)
}

// Check returns why a script looks dangerous (empty when it doesn't).
func (s *CommandService) Check(script string) []Danger {
	return Analyze(script)
}

// Targets lists saved servers with their readiness.
func (s *CommandService) Targets() []Target {
	list := s.core.Store.List()
	out := make([]Target, 0, len(list))
	for _, sv := range list {
		readiness, via, reason := s.readiness(sv)
		role := sv.Role
		if role == "" {
			role = "admin"
		}
		out = append(out, Target{
			ID: sv.ID, Name: sv.Name, Group: sv.Group, Host: sv.Host, User: sv.User, Port: sv.Port, Color: sv.Color,
			Environment: sv.Environment, Tags: nonNil(sv.Tags), Role: role,
			Readiness: readiness, Via: via, Reason: reason,
			CanExec: s.core.Allowed(sv.ID, core.PermExec),
			Sudo:    sudoReadiness(sv),
		})
	}
	return out
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func sudoReadiness(sv store.Server) string {
	switch {
	case sv.User == "root":
		return "root"
	case sv.SudoSaved, sv.AuthType == store.AuthPassword && sv.HasSecret:
		return "stored"
	}
	return "unknown"
}

// readiness tells whether the app can open a connection on its own.
func (s *CommandService) readiness(sv store.Server) (readiness, via, reason string) {
	if c, err := s.core.Manager.Get(sv.ID); err == nil && c.Connected() {
		return "connected", "ui", ""
	}
	if c, err := s.core.Bg.Get(sv.ID); err == nil && c.Connected() {
		return "connected", "background", ""
	}
	switch sv.AuthType {
	case store.AuthPassword:
		if sv.HasSecret {
			return "auto", "password", ""
		}
		return "needSecret", "password", "auth.needSecret"
	case store.AuthAgent:
		if os.Getenv("SSH_AUTH_SOCK") == "" {
			return "unavailable", "agent", "agent.notFound"
		}
		return "auto", "agent", ""
	case store.AuthKey:
		switch keyState(sv.KeyPath) {
		case "plain":
			return "auto", "key", ""
		case "encrypted":
			if sv.HasSecret {
				return "auto", "keyPassphrase", ""
			}
			return "needSecret", "keyPassphrase", "auth.needSecret"
		default:
			return "unavailable", "key", "key.notFound"
		}
	}
	return "unavailable", "", "auth.invalidType"
}

// keyState inspects the private key(s) a key profile would use: "plain"
// (usable without passphrase), "encrypted" or "missing".
func keyState(keyPath string) string {
	paths := []string{}
	if keyPath != "" {
		p := keyPath
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~"))
			}
		}
		paths = append(paths, p)
	} else if home, err := os.UserHomeDir(); err == nil {
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			paths = append(paths, filepath.Join(home, ".ssh", n))
		}
	}
	state := "missing"
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		_, err = ssh.ParsePrivateKey(data)
		if err == nil {
			return "plain"
		}
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			state = "encrypted"
		}
	}
	return state
}

// ---- snippets ----

// Snippets lists saved commands, by name.
func (s *CommandService) Snippets() ([]Snippet, error) {
	list, err := db.List[Snippet](s.core.DB, snippetNS)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return list, nil
}

// SaveSnippet creates (empty id) or updates a snippet.
func (s *CommandService) SaveSnippet(in Snippet) (Snippet, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" || len(in.Name) > 100 {
		return Snippet{}, apperr.New("cmd.snippetName")
	}
	if len(in.Description) > 1000 {
		in.Description = in.Description[:1000]
	}
	if err := validateScript(in.Command); err != nil {
		return Snippet{}, err
	}
	now := time.Now().UnixMilli()
	if in.ID == "" {
		in.ID = uuid.NewString()
		in.Created = now
	} else {
		var old Snippet
		if err := s.core.DB.Get(snippetNS, in.ID, &old); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return Snippet{}, apperr.New("cmd.snippetNotFound")
			}
			return Snippet{}, err
		}
		in.Created = old.Created
	}
	in.Updated = now
	err := s.core.DB.Put(snippetNS, in.ID, in)
	s.core.Audit("", "cmd.snippet.save", in.Name, "", err)
	return in, err
}

// DeleteSnippet removes a snippet.
func (s *CommandService) DeleteSnippet(id string) error {
	var old Snippet
	if err := s.core.DB.Get(snippetNS, id, &old); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return err
	}
	err := s.core.DB.Delete(snippetNS, id)
	s.core.Audit("", "cmd.snippet.delete", old.Name, "", err)
	return err
}

// ---- validation ----

func validateScript(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return apperr.New("cmd.empty")
	}
	if len(cmd) > MaxCommandBytes {
		return apperr.New("cmd.tooLong", "max", "64 KB")
	}
	if strings.ContainsRune(cmd, 0) {
		return apperr.New("cmd.invalidChars")
	}
	return nil
}

type prepared struct {
	spec    Spec
	command string
	sudo    bool
	danger  bool
	dangers []Danger
	servers []store.Server
}

// prepare validates a spec and resolves the command and target servers.
func (s *CommandService) prepare(spec Spec) (prepared, error) {
	p := prepared{}
	switch spec.Kind {
	case "", "command":
		spec.Kind = "command"
		spec.Preset = ""
		spec.Params = map[string]string{}
		spec.Command = strings.ReplaceAll(spec.Command, "\r\n", "\n")
		if err := validateScript(spec.Command); err != nil {
			return p, err
		}
		p.command = spec.Command
		p.sudo = spec.Sudo
	case "preset":
		r, err := RenderPreset(spec.Preset, spec.Params)
		if err != nil {
			return p, err
		}
		spec.Command = ""
		spec.Snippet = ""
		p.command = r.Command
		p.sudo = spec.Sudo || r.Sudo
		p.danger = r.Danger
	default:
		return p, apperr.New("cmd.invalidKind", "kind", spec.Kind)
	}
	if spec.Params == nil {
		spec.Params = map[string]string{}
	}
	spec.Sudo = p.sudo

	switch {
	case spec.TimeoutSec == 0:
		spec.TimeoutSec = DefaultTimeoutSec
	case spec.TimeoutSec < MinTimeoutSec || spec.TimeoutSec > MaxTimeoutSec:
		return p, apperr.New("cmd.invalidTimeout", "min", "5", "max", "3600")
	}
	switch {
	case spec.Concurrency == 0:
		spec.Concurrency = s.core.Settings().CommandConcurrency
	case spec.Concurrency < 1 || spec.Concurrency > MaxConcurrency:
		return p, apperr.New("cmd.invalidConcurrency", "max", "50")
	}

	seen := map[string]bool{}
	ids := []string{}
	for _, id := range spec.Servers {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		sv, err := s.core.Store.Get(id)
		if err != nil {
			return p, err
		}
		ids = append(ids, id)
		p.servers = append(p.servers, sv)
	}
	if len(ids) == 0 {
		return p, apperr.New("cmd.noServers")
	}
	if len(ids) > MaxServers {
		return p, apperr.New("cmd.tooManyServers", "max", "1000")
	}
	spec.Servers = ids
	p.dangers = Analyze(p.command)
	p.danger = p.danger || len(p.dangers) > 0
	p.spec = spec
	return p, nil
}

// wrapScript runs script in its own session (process group) tagged with
// marker, and makes the server enforce the timeout itself: after timeoutSec
// the whole group gets TERM, then KILL 5 s later. Closing the SSH channel
// alone would leave the command running, so cancellation uses killScript.
// The script's stdin is /dev/null (background job) so nothing waits for input.
func wrapScript(script string, timeoutSec int, marker string) string {
	q := core.Q(script)
	m := core.Q(marker)
	n := strconv.Itoa(timeoutSec)
	return "if command -v setsid >/dev/null 2>&1; then setsid sh -c " + q + " " + m + " & else sh -c " + q + " " + m + " & fi\n" +
		"p=$!\n" +
		"(i=0; while [ $i -lt " + n + " ]; do sleep 1; kill -0 $p 2>/dev/null || exit 0; i=$((i+1)); done\n" +
		" kill -TERM -$p 2>/dev/null || kill -TERM $p 2>/dev/null\n" +
		" i=0; while [ $i -lt 5 ]; do sleep 1; kill -0 $p 2>/dev/null || exit 0; i=$((i+1)); done\n" +
		" kill -KILL -$p 2>/dev/null || kill -KILL $p 2>/dev/null) >/dev/null 2>&1 &\n" +
		"wait $p"
}

// killScript terminates every process group started by wrapScript with
// marker (found through /proc, so it needs neither pgrep nor ps). The
// pattern is written so that it doesn't match its own command line.
func killScript(marker string) string {
	pat := core.Q(marker[:4] + "[" + marker[4:5] + "]" + marker[5:])
	return "pids=$(for d in /proc/[0-9]*; do if tr '\\0' ' ' < \"$d/cmdline\" 2>/dev/null | grep -q -- " + pat + "; then echo \"${d#/proc/}\"; fi; done)\n" +
		"[ -z \"$pids\" ] && exit 0\n" +
		"for p in $pids; do kill -TERM -$p 2>/dev/null; kill -TERM $p 2>/dev/null; done\n" +
		"sleep 2\n" +
		"for p in $pids; do kill -KILL -$p 2>/dev/null; kill -KILL $p 2>/dev/null; done\n" +
		"exit 0"
}

// marker tags the processes started by one run.
func marker(runID string) string { return "smcmd-" + runID }

// Preview resolves a spec without running anything (dry run).
func (s *CommandService) Preview(spec Spec) (Preview, error) {
	p, err := s.prepare(spec)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Command: p.command, Sudo: p.sudo, TimeoutSec: p.spec.TimeoutSec, Concurrency: p.spec.Concurrency,
		Dangers: p.dangers, Danger: p.danger, Servers: []PreviewServer{}}
	for _, sv := range p.servers {
		ps := PreviewServer{ServerID: sv.ID, Name: sv.Name, Environment: sv.Environment, Action: "run", Sudo: sudoReadiness(sv)}
		readiness, via, reason := s.readiness(sv)
		ps.Via = via
		switch {
		case !s.core.Allowed(sv.ID, core.PermExec):
			ps.Action, ps.Reason = "skip", "access.denied"
		case readiness == "needSecret" || readiness == "unavailable":
			ps.Action, ps.Reason = "skip", reason
		}
		full := wrapScript(p.command, p.spec.TimeoutSec, marker("<run-id>"))
		if p.sudo && sv.User != "root" {
			ps.Command = "sudo -- sh -c " + core.Q(full)
		} else {
			ps.Command = "sh -c " + core.Q(full)
		}
		out.Servers = append(out.Servers, ps)
	}
	return out, nil
}
