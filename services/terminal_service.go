package services

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// TerminalService runs interactive shells over SSH PTYs. Output is streamed
// to the frontend as base64 chunks so multi-byte characters split across
// reads survive intact (xterm.js decodes the byte stream itself).
type TerminalService struct {
	core *Core
	mu   sync.Mutex
	ts   map[string]*term
	// locales caches, per connection, the UTF-8 locale a shell must be given
	// ("" when the server's default is already UTF-8).
	locales map[string]string
}

type term struct {
	connID    string
	session   *ssh.Session
	stdin     io.WriteCloser
	once      sync.Once
	rec       *recorder
	recFile   string
	lastInput atomic.Int64 // unix ms
	idleOut   atomic.Bool
}

func NewTerminalService(core *Core) *TerminalService {
	s := &TerminalService{core: core, ts: map[string]*term{}, locales: map[string]string{}}
	go s.idleLoop()
	return s
}

func (s *TerminalService) ServiceShutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.ts {
		t.session.Close()
	}
	return nil
}

// Open starts a login shell, optionally in cwd. Returns the terminal id.
func (s *TerminalService) open(connID, cwd string, cols, rows int) (string, error) {
	return s.start(connID, cwd, "", cols, rows)
}

var execTarget = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}$`)

// execCommand builds the command for an interactive console of kind on
// target: "docker" (shell in a container), "psql" / "mysql" (database
// console, target = database or ""), "redis" (redis-cli). Consoles that
// need root fall back to sudo, which prompts inside the terminal.
func execCommand(kind, target string) (string, error) {
	if target != "" && !execTarget.MatchString(target) {
		return "", apperr.New("term.invalidTarget", "target", target)
	}
	q := sshx.ShellQuote(target)
	switch kind {
	case "docker":
		if target == "" {
			return "", apperr.New("term.invalidTarget", "target", target)
		}
		in := `if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi`
		ex := "docker exec -it -e TERM=xterm-256color " + q + " sh -c " + sshx.ShellQuote(in)
		return "if docker info >/dev/null 2>&1; then exec " + ex + "; else exec sudo " + ex + "; fi", nil
	case "psql":
		db := ""
		if target != "" {
			db = " " + q
		}
		return "if [ \"$(id -u)\" = 0 ]; then exec su postgres -c " + sshx.ShellQuote("psql"+db) + "; else exec sudo -u postgres psql" + db + "; fi", nil
	case "mysql":
		db := ""
		if target != "" {
			db = " " + q
		}
		cli := "$(command -v mariadb || command -v mysql)"
		return "if [ \"$(id -u)\" = 0 ]; then exec " + cli + db + "; else exec sudo " + cli + db + "; fi", nil
	case "redis":
		return "exec redis-cli", nil
	}
	return "", apperr.New("term.invalidTarget", "target", kind)
}

// OpenExec opens an interactive console (see execCommand) in a terminal.
func (s *TerminalService) OpenExec(connID, kind, target string, cols, rows int) (string, error) {
	if err := s.core.Require(connID, core.PermTerminal); err != nil {
		return "", err
	}
	cmd, err := execCommand(kind, target)
	if err != nil {
		return "", err
	}
	id, err := s.start(connID, "", cmd, cols, rows)
	detail := ""
	if f := s.RecordingOf(id); f != "" {
		detail = "recording: " + f
	}
	s.core.Audit(connID, "terminal.exec", kind+":"+target, detail, err)
	return id, err
}

// start opens a PTY session running the login shell (in cwd), or command
// when given.
func (s *TerminalService) start(connID, cwd, command string, cols, rows int) (string, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	client, err := conn.SSH()
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.IUTF8:         1, // kernel line editing erases whole UTF-8 characters
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return "", err
	}
	_ = sess.Setenv("COLORTERM", "truecolor")
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return "", err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return "", err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return "", err
	}
	// Without a UTF-8 locale the shell treats "ệ" as 3 separate bytes, so the
	// backspaces a Vietnamese IME sends leave broken characters behind.
	// OpenSSH forwards the client's LANG; we pick a UTF-8 locale the server has.
	locale := s.utf8Locale(conn)
	envRefused := locale != "" && (sess.Setenv("LANG", locale) != nil || sess.Setenv("LC_CTYPE", locale) != nil)
	if command != "" {
		prelude := ""
		if envRefused {
			prelude = "LANG=" + locale + " LC_CTYPE=" + locale + "; export LANG LC_CTYPE; "
		}
		err = sess.Start("/bin/sh -c " + sshx.ShellQuote(prelude+command))
	} else if envRefused {
		// sshd refused (no AcceptEnv): set it in a wrapper before the login shell.
		cmd := "LANG=" + locale + " LC_CTYPE=" + locale + "; export LANG LC_CTYPE; "
		err = sess.Start(shellCommand(cmd, cwd))
	} else if cwd != "" {
		err = sess.Start(shellCommand("", cwd))
	} else {
		err = sess.Shell()
	}
	if err != nil {
		sess.Close()
		return "", err
	}

	id := uuid.NewString()
	t := &term{connID: connID, session: sess, stdin: stdin}
	t.lastInput.Store(time.Now().UnixMilli())
	if s.core.Settings().RecordTerminal {
		title := conn.Server().Name
		if command != "" {
			title += " (console)"
		} else if cwd != "" {
			title += ":" + cwd
		}
		if rec, file, err := newRecorder(s.core.Dir, connID, id, title, cols, rows); err == nil {
			t.rec, t.recFile = rec, file
		}
	}
	s.mu.Lock()
	s.ts[id] = t
	s.mu.Unlock()

	var wg sync.WaitGroup
	pump := func(r io.Reader) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if t.rec != nil {
					t.rec.output(buf[:n])
				}
				emit(EventTermData, TermDataEvent{ID: id, Data: base64.StdEncoding.EncodeToString(buf[:n])})
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout)
	go pump(stderr)
	go func() {
		err := sess.Wait()
		wg.Wait()
		var exitErr *ssh.ExitError
		var ae *apperr.Error
		if err != nil && !errors.As(err, &exitErr) {
			ae = apperr.From(err)
		}
		if t.idleOut.Load() {
			ae = apperr.New("term.idle", "min", strconv.Itoa(s.core.Settings().TerminalIdleMinutes))
		}
		if t.rec != nil {
			t.rec.close()
		}
		s.remove(id)
		emit(EventTermExit, TermExitEvent{ID: id, Error: ae})
	}()
	return id, nil
}

// shellCommand runs prelude, cds into cwd and execs the user's login shell.
// It goes through /bin/sh so it works whatever the login shell is (fish, csh…).
func shellCommand(prelude, cwd string) string {
	script := prelude + `[ -n "$1" ] && cd "$1" 2>/dev/null; exec "${SHELL:-/bin/sh}" -l`
	return "/bin/sh -c " + sshx.ShellQuote(script) + " sh " + sshx.ShellQuote(cwd)
}

var localeName = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)

// localeProbe prints nothing when the default locale is already UTF-8 (or the
// system has no locale tool, e.g. musl where UTF-8 always works); otherwise
// it prints the best available UTF-8 locale.
const localeProbe = `case "$(locale charmap 2>/dev/null)" in UTF-8|utf8|UTF8|"") exit 0;; esac
l=$(locale -a 2>/dev/null | grep -iE '^(c|en_us)\.utf-?8$' | head -n 1)
[ -n "$l" ] || l=$(locale -a 2>/dev/null | grep -iE '\.utf-?8$' | head -n 1)
printf '%s' "$l"`

func (s *TerminalService) utf8Locale(conn *sshx.Conn) string {
	s.mu.Lock()
	l, ok := s.locales[conn.ID]
	s.mu.Unlock()
	if ok {
		return l
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, "sh -c "+sshx.ShellQuote(localeProbe), nil)
	if err != nil {
		return "" // don't cache: the connection may just be reconnecting
	}
	l = strings.TrimSpace(res.Stdout)
	if !localeName.MatchString(l) {
		l = ""
	}
	s.mu.Lock()
	s.locales[conn.ID] = l
	s.mu.Unlock()
	return l
}

func (s *TerminalService) get(id string) (*term, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ts[id]
	if !ok {
		return nil, apperr.New("term.closed")
	}
	return t, nil
}

func (s *TerminalService) remove(id string) {
	s.mu.Lock()
	t, ok := s.ts[id]
	delete(s.ts, id)
	s.mu.Unlock()
	if ok {
		t.once.Do(func() { t.session.Close() })
	}
}

func (s *TerminalService) Write(id, data string) error {
	t, err := s.get(id)
	if err != nil {
		return err
	}
	t.lastInput.Store(time.Now().UnixMilli())
	_, err = io.WriteString(t.stdin, data)
	return err
}

// idleLoop closes terminals without input for the configured time.
func (s *TerminalService) idleLoop() {
	for range time.Tick(30 * time.Second) {
		limit := s.core.Settings().TerminalIdleMinutes
		if limit <= 0 {
			continue
		}
		cutoff := time.Now().Add(-time.Duration(limit) * time.Minute).UnixMilli()
		s.mu.Lock()
		var idle []*term
		for _, t := range s.ts {
			if t.lastInput.Load() < cutoff {
				idle = append(idle, t)
			}
		}
		s.mu.Unlock()
		for _, t := range idle {
			t.idleOut.Store(true)
			t.session.Close()
		}
	}
}

// RecordingOf returns the recording file of an open terminal ("" if none).
func (s *TerminalService) RecordingOf(id string) string {
	if t, err := s.get(id); err == nil {
		return t.recFile
	}
	return ""
}

func (s *TerminalService) Resize(id string, cols, rows int) error {
	t, err := s.get(id)
	if err != nil {
		return err
	}
	if cols <= 0 || rows <= 0 {
		return nil
	}
	if t.rec != nil {
		t.rec.resize(cols, rows)
	}
	return t.session.WindowChange(rows, cols)
}

func (s *TerminalService) Close(id string) {
	s.remove(id)
}
