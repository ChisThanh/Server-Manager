// Package sshx manages SSH/SFTP connections. Everything the app does on a
// server goes through the standard sshd (exec, pty, sftp subsystem), so
// nothing needs to be installed remotely.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"server-manager/internal/apperr"
	"server-manager/internal/store"
)

// ErrNeedSecret means the profile needs a password/passphrase that isn't stored.
var ErrNeedSecret = apperr.New("auth.needSecret")

const (
	dialTimeout       = 15 * time.Second
	keepaliveInterval = 20 * time.Second
	keepaliveTimeout  = 10 * time.Second
)

type Status string

const (
	StatusConnecting   Status = "connecting"
	StatusConnected    Status = "connected"
	StatusDisconnected Status = "disconnected"
)

// StatusFunc is notified whenever a connection changes state.
type StatusFunc func(id string, status Status, err *apperr.Error)

type Manager struct {
	mu       sync.Mutex
	conns    map[string]*Conn
	hostKeys *HostKeys
	onStatus StatusFunc
}

func NewManager(hostKeys *HostKeys, onStatus StatusFunc) *Manager {
	return &Manager{conns: map[string]*Conn{}, hostKeys: hostKeys, onStatus: onStatus}
}

func (m *Manager) HostKeys() *HostKeys { return m.hostKeys }

// Connect opens (or reuses) the connection for a profile.
func (m *Manager) Connect(ctx context.Context, cfg store.Server, secret string) (*Conn, error) {
	m.mu.Lock()
	c, ok := m.conns[cfg.ID]
	if !ok {
		c = &Conn{ID: cfg.ID, hostKeys: m.hostKeys, onStatus: m.onStatus}
		m.conns[cfg.ID] = c
	}
	m.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.secret = secret
	c.userClosed = false
	if c.client != nil {
		return c, nil
	}
	if err := c.dialLocked(ctx); err != nil {
		m.mu.Lock()
		if m.conns[cfg.ID] == c {
			delete(m.conns, cfg.ID)
		}
		m.mu.Unlock()
		return nil, err
	}
	return c, nil
}

func (m *Manager) Get(id string) (*Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, apperr.New("conn.notConnected")
	}
	return c, nil
}

// IDs lists the servers with a connection object.
func (m *Manager) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.conns))
	for id := range m.conns {
		out = append(out, id)
	}
	return out
}

func (m *Manager) Disconnect(id string) {
	m.mu.Lock()
	c, ok := m.conns[id]
	delete(m.conns, id)
	m.mu.Unlock()
	if ok {
		c.Close()
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	conns := m.conns
	m.conns = map[string]*Conn{}
	m.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// Conn is one logical connection to a server. It transparently re-dials
// when the underlying TCP connection drops.
type Conn struct {
	ID       string
	hostKeys *HostKeys
	onStatus StatusFunc

	mu         sync.Mutex
	cfg        store.Server
	secret     string
	client     *ssh.Client
	sftp       *sftp.Client
	home       string
	gen        int
	userClosed bool

	active   atomic.Int32 // commands/transfers in flight
	lastUsed atomic.Int64 // unix ms
}

func (c *Conn) use() func() {
	c.active.Add(1)
	c.lastUsed.Store(time.Now().UnixMilli())
	return func() {
		c.lastUsed.Store(time.Now().UnixMilli())
		c.active.Add(-1)
	}
}

// Idle reports how long the connection has had no command or transfer in
// flight (0 while busy).
func (c *Conn) Idle() time.Duration {
	if c.active.Load() > 0 {
		return 0
	}
	last := c.lastUsed.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.UnixMilli(last))
}

func (c *Conn) Server() store.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func (c *Conn) Home() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.home
}

func (c *Conn) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client != nil
}

func (c *Conn) status(s Status, err *apperr.Error) {
	if c.onStatus != nil {
		c.onStatus(c.ID, s, err)
	}
}

func (c *Conn) dialLocked(ctx context.Context) error {
	c.status(StatusConnecting, nil)
	client, err := dial(ctx, c.cfg, c.secret, c.hostKeys)
	if err != nil {
		c.status(StatusDisconnected, apperr.From(err))
		return err
	}
	sc, err := sftp.NewClient(client,
		sftp.UseConcurrentWrites(true),
		sftp.UseConcurrentReads(true),
		sftp.MaxConcurrentRequestsPerFile(64),
	)
	if err != nil {
		client.Close()
		err = apperr.Wrap(err, "conn.sftpUnsupported")
		c.status(StatusDisconnected, apperr.From(err))
		return err
	}
	c.client = client
	c.sftp = sc
	c.gen++
	if wd, err := sc.Getwd(); err == nil {
		c.home = wd
	} else {
		c.home = "/"
	}
	go c.monitor(client, c.gen)
	c.status(StatusConnected, nil)
	return nil
}

// monitor sends keepalives and detects dead connections.
func (c *Conn) monitor(client *ssh.Client, gen int) {
	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			c.markDown(gen, apperr.New("conn.lost"))
			return
		case <-t.C:
			if err := ping(client); err != nil {
				client.Close()
				c.markDown(gen, apperr.Wrap(err, "conn.keepaliveFailed"))
				return
			}
		}
	}
}

func ping(client *ssh.Client) error {
	errc := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-time.After(keepaliveTimeout):
		return errors.New("timeout")
	}
}

func (c *Conn) markDown(gen int, reason *apperr.Error) {
	c.mu.Lock()
	if c.gen != gen || c.client == nil {
		c.mu.Unlock()
		return
	}
	c.closeLocked()
	closed := c.userClosed
	c.mu.Unlock()
	if !closed {
		c.status(StatusDisconnected, reason)
	}
}

func (c *Conn) closeLocked() {
	if c.sftp != nil {
		c.sftp.Close()
		c.sftp = nil
	}
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
}

func (c *Conn) Close() {
	c.mu.Lock()
	c.userClosed = true
	c.closeLocked()
	c.mu.Unlock()
	c.status(StatusDisconnected, nil)
}

// Reconnect forces a fresh connection.
func (c *Conn) Reconnect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
	c.userClosed = false
	return c.dialLocked(ctx)
}

func (c *Conn) ensure() (*ssh.Client, *sftp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.userClosed {
		return nil, nil, apperr.New("conn.closed")
	}
	if c.client == nil {
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+5*time.Second)
		defer cancel()
		if err := c.dialLocked(ctx); err != nil {
			return nil, nil, err
		}
	}
	return c.client, c.sftp, nil
}

// SSH returns a live ssh client, reconnecting if needed.
func (c *Conn) SSH() (*ssh.Client, error) {
	cl, _, err := c.ensure()
	return cl, err
}

// SFTP runs fn with a live sftp client. If the call fails because the
// connection died, it reconnects and retries once.
func (c *Conn) SFTP(fn func(*sftp.Client) error) error {
	defer c.use()()
	_, sc, err := c.ensure()
	if err != nil {
		return err
	}
	err = fn(sc)
	if err == nil || !isConnErr(err) {
		return err
	}
	c.mu.Lock()
	cl, gen, same := c.client, c.gen, c.sftp == sc
	c.mu.Unlock()
	if same && cl != nil {
		if ping(cl) == nil {
			return err // connection is healthy; the error is genuine
		}
		cl.Close()
		c.markDown(gen, apperr.New("conn.reconnecting"))
	}
	_, sc, err2 := c.ensure()
	if err2 != nil {
		return err
	}
	return fn(sc)
}

func isConnErr(err error) bool {
	if errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection lost", "broken pipe", "connection reset", "use of closed network connection"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ExecResult is the outcome of a non-interactive command.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// Exec runs a command and collects its output. Output is capped to keep
// runaway commands from exhausting memory.
func (c *Conn) Exec(ctx context.Context, cmd string, stdin io.Reader) (ExecResult, error) {
	defer c.use()()
	client, err := c.SSH()
	if err != nil {
		return ExecResult{}, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()
	const limit = 8 << 20
	var stdout, stderr capBuffer
	stdout.max, stderr.max = limit, limit
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if stdin != nil {
		sess.Stdin = stdin
	}
	if err := sess.Start(cmd); err != nil {
		return ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		// Wait for the output copiers to stop before touching the buffers.
		select {
		case <-done:
			return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}, ctx.Err()
		case <-time.After(5 * time.Second):
			return ExecResult{ExitCode: -1}, ctx.Err()
		}
	}
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		var missing *ssh.ExitMissingError
		if errors.As(err, &missing) {
			res.ExitCode = -1
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// Stream runs cmd, copying its output to stdout/stderr as it arrives (either
// may be nil to discard). It returns the exit code, or -1 when the command
// was killed or the server reported none. Cancelling ctx kills the command.
func (c *Conn) Stream(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	defer c.use()()
	client, err := c.SSH()
	if err != nil {
		return -1, err
	}
	sess, err := client.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	sess.Stdout = stdout
	sess.Stderr = stderr
	if stdin != nil {
		sess.Stdin = stdin
	}
	if err := sess.Start(cmd); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return -1, ctx.Err()
	}
	if err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitStatus(), nil
		}
		var missing *ssh.ExitMissingError
		if errors.As(err, &missing) {
			return -1, nil
		}
		return -1, err
	}
	return 0, nil
}

type capBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// ShellQuote quotes s for POSIX sh.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func dial(ctx context.Context, cfg store.Server, secret string, hk *HostKeys) (*ssh.Client, error) {
	auth, cleanup, err := authMethods(cfg, secret)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	addr := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	tcp, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, apperr.Wrap(err, "conn.dialFailed", "addr", addr)
	}
	conf := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              auth,
		HostKeyCallback:   hk.Callback(cfg.Host, cfg.Port),
		HostKeyAlgorithms: hk.KnownAlgorithms(addr, tcp.RemoteAddr()),
		Timeout:           dialTimeout,
		ClientVersion:     "SSH-2.0-ServerManager",
	}
	_ = tcp.SetDeadline(time.Now().Add(dialTimeout + 10*time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(tcp, addr, conf)
	if err != nil {
		tcp.Close()
		var hkErr *HostKeyError
		if errors.As(err, &hkErr) {
			return nil, hkErr
		}
		// x/crypto reports a rejected password followed by keyboard-interactive
		// as "unexpected message type 51"; both mean the credentials were wrong.
		if msg := err.Error(); strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "unexpected message type 51") {
			return nil, apperr.Wrap(err, "auth.failed")
		}
		return nil, err
	}
	_ = tcp.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

func authMethods(cfg store.Server, secret string) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	switch cfg.AuthType {
	case store.AuthPassword:
		if secret == "" {
			return nil, noop, ErrNeedSecret
		}
		return []ssh.AuthMethod{
			ssh.Password(secret),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					if !echos[i] {
						answers[i] = secret
					}
				}
				return answers, nil
			}),
		}, noop, nil

	case store.AuthKey:
		paths := []string{}
		if cfg.KeyPath != "" {
			paths = append(paths, expandHome(cfg.KeyPath))
		} else if home, err := os.UserHomeDir(); err == nil {
			for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
				paths = append(paths, filepath.Join(home, ".ssh", n))
			}
		}
		var signers []ssh.Signer
		var lastErr error
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				lastErr = err
				continue
			}
			signer, err := ssh.ParsePrivateKey(data)
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				if secret == "" {
					return nil, noop, ErrNeedSecret
				}
				signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(secret))
			}
			if err != nil {
				lastErr = apperr.Wrap(err, "key.readFailed", "path", p)
				continue
			}
			signers = append(signers, signer)
		}
		if len(signers) == 0 {
			if lastErr == nil {
				lastErr = apperr.New("key.notFound")
			}
			return nil, noop, lastErr
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, noop, nil

	case store.AuthAgent:
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, noop, apperr.New("agent.notFound")
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, noop, apperr.Wrap(err, "agent.dialFailed")
		}
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(conn).Signers)}, func() { conn.Close() }, nil
	}
	return nil, noop, apperr.New("auth.invalidType", "type", string(cfg.AuthType))
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
