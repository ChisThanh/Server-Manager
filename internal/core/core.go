// Package core holds the state and helpers shared by every Wails service:
// connections, the local database, settings, the audit log, the event
// timeline, background jobs, privilege escalation and access policy.
//
// Module services (services/*) receive a *Core and should:
//   - check permissions with Require before changing anything,
//   - run commands through Run / RunAuto (sudo handled here),
//   - record every change with Audit,
//   - use Jobs for anything long-running or streaming.
package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/db"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// Event names emitted to the frontend by core.
const (
	EventConnStatus = "conn:status"
	EventJobOutput  = "job:output"
	EventJobDone    = "job:done"
	EventTimeline   = "timeline:new"
)

type ConnStatusEvent struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Error  *apperr.Error `json:"error"`
}

func init() {
	application.RegisterEvent[ConnStatusEvent](EventConnStatus)
	application.RegisterEvent[JobOutputEvent](EventJobOutput)
	application.RegisterEvent[JobInfo](EventJobDone)
	application.RegisterEvent[Event](EventTimeline)
}

// Core is the shared state behind all services.
type Core struct {
	Dir   string
	Store *store.Store
	DB    *db.DB
	// Manager holds the connections opened from the UI.
	Manager *sshx.Manager
	// Bg holds connections the app opens on its own (monitoring, scheduled
	// backups, command center) so closing a workspace doesn't break them.
	Bg   *sshx.Manager
	Jobs *Jobs

	settingsMu sync.RWMutex
	settings   Settings
	onSettings []func(Settings)

	auditMu sync.Mutex

	privMu sync.Mutex
	sudoPW map[string]string // sudo passwords that worked this session
	isRoot map[*sshx.Conn]bool
}

func New() (*Core, error) {
	dir, err := store.Dir()
	if err != nil {
		return nil, err
	}
	st, err := store.New(dir)
	if err != nil {
		return nil, err
	}
	d, err := db.Open(dir)
	if err != nil {
		return nil, err
	}
	return newCore(dir, st, d), nil
}

// NewForTest builds a Core around an existing store and an in-memory DB.
func NewForTest(dir string, st *store.Store) (*Core, error) {
	d, err := db.OpenMemory()
	if err != nil {
		return nil, err
	}
	return newCore(dir, st, d), nil
}

func newCore(dir string, st *store.Store, d *db.DB) *Core {
	c := &Core{Dir: dir, Store: st, DB: d, sudoPW: map[string]string{}, isRoot: map[*sshx.Conn]bool{}}
	hk := sshx.NewHostKeys(dir)
	var lostMu sync.Mutex
	lost := map[string]bool{} // dropped unexpectedly, not yet back
	up := map[string]bool{}   // has been connected
	c.Manager = sshx.NewManager(hk, func(id string, s sshx.Status, err *apperr.Error) {
		Emit(EventConnStatus, ConnStatusEvent{ID: id, Status: string(s), Error: err})
		// Unexpected drops and recoveries go on the timeline.
		lostMu.Lock()
		was := lost[id]
		if s == sshx.StatusConnected && !was {
			up[id] = true
		}
		switch {
		case s == sshx.StatusDisconnected && err != nil && !was && up[id]:
			lost[id] = true
			lostMu.Unlock()
			c.AddEvent(Event{Server: id, Kind: "conn", Severity: "warn", Code: "conn.lost", Detail: err.Error()})
			return
		case s == sshx.StatusConnected && was:
			delete(lost, id)
			lostMu.Unlock()
			c.AddEvent(Event{Server: id, Kind: "conn", Severity: "ok", Code: "conn.restored"})
			return
		case s == sshx.StatusDisconnected && err == nil:
			delete(lost, id) // closed by the user
			delete(up, id)
		}
		lostMu.Unlock()
	})
	c.Bg = sshx.NewManager(hk, nil)
	c.Jobs = newJobs()
	c.loadSettings()
	go c.reapBg()
	return c
}

// reapBg closes background connections of unmonitored servers once nothing
// has used them for 5 minutes (commands and transfers in flight keep them).
func (c *Core) reapBg() {
	for range time.Tick(time.Minute) {
		for _, id := range c.Bg.IDs() {
			if sv, err := c.Store.Get(id); err == nil && sv.Monitor {
				continue
			}
			if conn, err := c.Bg.Get(id); err == nil && conn.Idle() > 5*time.Minute {
				c.Bg.Disconnect(id)
			}
		}
	}
}

// Close releases all connections and the database.
func (c *Core) Close() {
	c.Jobs.CancelAll()
	c.Manager.CloseAll()
	c.Bg.CloseAll()
	c.DB.Close()
}

// Conn returns the UI connection for a server.
func (c *Core) Conn(id string) (*sshx.Conn, error) {
	return c.Manager.Get(id)
}

// AnyConn returns a live connection for a server: the UI one if it is
// connected, otherwise a background connection using the stored secret.
// Servers whose secret isn't stored fail with auth.needSecret.
func (c *Core) AnyConn(ctx context.Context, id string) (*sshx.Conn, error) {
	if conn, err := c.Manager.Get(id); err == nil && conn.Connected() {
		return conn, nil
	}
	if conn, err := c.Bg.Get(id); err == nil && conn.Connected() {
		return conn, nil
	}
	sv, err := c.Store.Get(id)
	if err != nil {
		return nil, err
	}
	return c.Bg.Connect(ctx, sv, c.Store.Secret(id))
}

// ServerName returns the display name of a server (or its id).
func (c *Core) ServerName(id string) string {
	if sv, err := c.Store.Get(id); err == nil {
		return sv.Name
	}
	return id
}

// ---- events to the frontend ----

var emitHook atomic.Pointer[func(name string, data any)]

// SetEmitHook redirects every emitted event to h (tests). nil restores.
func SetEmitHook(h func(name string, data any)) {
	if h == nil {
		emitHook.Store(nil)
		return
	}
	emitHook.Store(&h)
}

// Emit sends an event to the frontend.
func Emit(name string, data any) {
	if h := emitHook.Load(); h != nil {
		(*h)(name, data)
		return
	}
	if app := application.Get(); app != nil {
		app.Event.Emit(name, data)
	}
}

// ---- running commands ----

// IsRoot reports whether the connection's user is root (cached).
func (c *Core) IsRoot(ctx context.Context, conn *sshx.Conn) bool {
	c.privMu.Lock()
	v, ok := c.isRoot[conn]
	c.privMu.Unlock()
	if ok {
		return v
	}
	res, err := conn.Exec(ctx, "id -u", nil)
	if err != nil {
		return false
	}
	v = strings.TrimSpace(res.Stdout) == "0"
	c.privMu.Lock()
	c.isRoot[conn] = v
	c.privMu.Unlock()
	return v
}

// SudoPassword returns the password to feed sudo: the explicit one, one that
// worked earlier this session, the stored sudo password, or the stored login
// password for password-auth profiles.
func (c *Core) SudoPassword(conn *sshx.Conn, explicit string) string {
	if explicit != "" {
		return explicit
	}
	sv := conn.Server()
	c.privMu.Lock()
	pw := c.sudoPW[sv.ID]
	c.privMu.Unlock()
	if pw != "" {
		return pw
	}
	if sv.SudoSaved {
		if pw := c.Store.SudoSecret(sv.ID); pw != "" {
			return pw
		}
	}
	if sv.AuthType == store.AuthPassword {
		return c.Store.Secret(sv.ID)
	}
	return ""
}

func (c *Core) rememberSudo(conn *sshx.Conn, pw string) {
	if pw == "" {
		return
	}
	c.privMu.Lock()
	c.sudoPW[conn.Server().ID] = pw
	c.privMu.Unlock()
}

// Run executes cmd (a shell snippet) on conn, through sudo when sudo is set
// and the user isn't root. A non-zero exit status is not an error; check
// res.ExitCode. Sudo failures return sudo.required / sudo.wrongPassword.
func (c *Core) Run(ctx context.Context, conn *sshx.Conn, cmd string, sudo bool, password, stdin string) (sshx.ExecResult, error) {
	if !sudo || c.IsRoot(ctx, conn) {
		var in *strings.Reader
		if stdin != "" {
			in = strings.NewReader(stdin)
		}
		if in == nil {
			return conn.Exec(ctx, "sh -c "+sshx.ShellQuote(cmd), nil)
		}
		return conn.Exec(ctx, "sh -c "+sshx.ShellQuote(cmd), in)
	}
	pw := c.SudoPassword(conn, password)
	res, err := sudoExec(ctx, conn, pw, cmd, stdin)
	if err == nil {
		c.rememberSudo(conn, pw)
	}
	return res, err
}

var permDenied = regexp.MustCompile(`(?i)permission denied|operation not permitted|must be (run as )?root|are you root|access denied|requires? (superuser|root)|only root|interactive authentication required|polkit|not in the sudoers|you need to be root|insufficient privileges`)

// NeedsRoot reports whether a command's output looks like a permission error.
func NeedsRoot(res sshx.ExecResult) bool {
	return res.ExitCode != 0 && permDenied.MatchString(res.Stderr+"\n"+res.Stdout)
}

// RunAuto runs cmd as the login user and, if it fails (non-zero exit) with
// a permission error, again through sudo. Only use it for read-only or
// all-or-nothing commands, since a failed first attempt is simply re-run.
func (c *Core) RunAuto(ctx context.Context, conn *sshx.Conn, cmd, password, stdin string) (sshx.ExecResult, error) {
	res, err := c.Run(ctx, conn, cmd, false, "", stdin)
	if err != nil || !NeedsRoot(res) || c.IsRoot(ctx, conn) {
		return res, err
	}
	return c.Run(ctx, conn, cmd, true, password, stdin)
}

// RunOK is Run that turns a non-zero exit status into a cmd.failed error.
func (c *Core) RunOK(ctx context.Context, conn *sshx.Conn, cmd string, sudo bool, password, stdin string) (sshx.ExecResult, error) {
	res, err := c.Run(ctx, conn, cmd, sudo, password, stdin)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, CmdError(res)
	}
	return res, nil
}

// sudoExec runs cmd through sudo. If sudo needs no password it runs with -n
// so stdin reaches the command untouched; otherwise the password is fed as
// the first line of stdin (sudo reads it byte by byte, leaving the rest).
func sudoExec(ctx context.Context, conn *sshx.Conn, password, cmd, stdin string) (sshx.ExecResult, error) {
	full, input, err := sudoCommand(ctx, conn, password, cmd, stdin)
	if err != nil {
		return sshx.ExecResult{}, err
	}
	res, err := conn.Exec(ctx, full, strings.NewReader(input))
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		if e := sudoError(res.Stderr); e != nil {
			return res, e
		}
	}
	return res, nil
}

// SudoRun is the legacy helper used by the file/system services: like Run
// with sudo, but a non-zero exit becomes sudo.failed with the output.
func SudoRun(ctx context.Context, conn *sshx.Conn, password, cmd, stdin string) (sshx.ExecResult, error) {
	res, err := sudoExec(ctx, conn, password, cmd, stdin)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return res, apperr.New("sudo.failed").WithDetail(msg)
	}
	return res, nil
}

func sudoCommand(ctx context.Context, conn *sshx.Conn, password, cmd, stdin string) (full, input string, err error) {
	probe, err := conn.Exec(ctx, "sudo -n true", nil)
	if err != nil {
		return "", "", err
	}
	if probe.ExitCode == 0 {
		return "sudo -n -- sh -c " + sshx.ShellQuote(cmd), stdin, nil
	}
	if strings.Contains(probe.Stderr, "not found") && strings.Contains(probe.Stderr, "sudo") {
		return "", "", apperr.New("sudo.missing")
	}
	if password == "" {
		return "", "", apperr.New("sudo.required")
	}
	return "sudo -S -p '' -- sh -c " + sshx.ShellQuote(cmd), password + "\n" + stdin, nil
}

func sudoError(stderr string) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "incorrect password") || strings.Contains(msg, "Sorry, try again"):
		return apperr.New("sudo.wrongPassword")
	case strings.Contains(msg, "is not in the sudoers") || strings.Contains(msg, "not allowed to execute"):
		return apperr.New("sudo.notAllowed").WithDetail(msg)
	}
	return nil
}

// CmdError describes a failed command by its exit code and output.
func CmdError(res sshx.ExecResult) error {
	return apperr.New("cmd.failed", "code", fmt.Sprint(res.ExitCode)).
		WithDetail(FirstNonEmpty(strings.TrimSpace(res.Stderr), strings.TrimSpace(res.Stdout)))
}

func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Timeout returns a context for a remote operation.
func Timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// Q quotes s for POSIX sh.
func Q(s string) string { return sshx.ShellQuote(s) }
