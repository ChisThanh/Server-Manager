package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// ServerService manages saved profiles and connections.
type ServerService struct {
	core *Core
}

func NewServerService(core *Core) *ServerService { return &ServerService{core: core} }

func (s *ServerService) ServiceShutdown() error {
	s.core.Manager.CloseAll()
	return nil
}

type HostKeyInfo struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	KeyBase64   string `json:"keyBase64"`
	Mismatch    bool   `json:"mismatch"`
}

// ConnectResult tells the UI whether it needs to ask the user something
// (a secret, or confirmation of an unknown host key) before retrying.
type ConnectResult struct {
	Status  string       `json:"status"` // ok | need-secret | hostkey
	Home    string       `json:"home"`
	HostKey *HostKeyInfo `json:"hostKey"`
}

func (s *ServerService) List() []store.Server {
	return s.core.Store.List()
}

func (s *ServerService) Save(server store.Server, secret string, setSecret bool) (store.Server, error) {
	isNew := server.ID == ""
	var old store.Server
	if !isNew {
		old, _ = s.core.Store.Get(server.ID)
	}
	out, err := s.core.Store.Save(server, secret, setSecret)
	if err != nil {
		return out, err
	}
	action := "server.update"
	if isNew {
		action = "server.create"
	}
	s.core.Audit(out.ID, action, out.Name, profileDiff(old, out, setSecret), nil)
	// Background connections must pick up changed host/credentials.
	if !isNew && (old.Host != out.Host || old.Port != out.Port || old.User != out.User || old.AuthType != out.AuthType || old.KeyPath != out.KeyPath || setSecret) {
		s.core.Bg.Disconnect(out.ID)
	}
	return out, nil
}

func profileDiff(a, b store.Server, secret bool) string {
	var d []string
	add := func(k, x, y string) {
		if x != y {
			d = append(d, k+": "+x+" → "+y)
		}
	}
	add("host", a.Host, b.Host)
	add("port", strconv.Itoa(a.Port), strconv.Itoa(b.Port))
	add("user", a.User, b.User)
	add("auth", string(a.AuthType), string(b.AuthType))
	add("role", a.Role, b.Role)
	add("environment", a.Environment, b.Environment)
	add("monitor", strconv.FormatBool(a.Monitor), strconv.FormatBool(b.Monitor))
	if secret {
		d = append(d, "secret changed")
	}
	return strings.Join(d, "; ")
}

func (s *ServerService) Delete(id string) error {
	name := s.core.ServerName(id)
	s.core.Manager.Disconnect(id)
	s.core.Bg.Disconnect(id)
	err := s.core.Store.Delete(id)
	s.core.Audit("", "server.delete", name, "", err)
	return err
}

// SetSudoPassword stores (or with "" forgets) the sudo password in the
// keychain so background tasks can use it.
func (s *ServerService) SetSudoPassword(id, password string) error {
	err := s.core.Store.SetSudoSecret(id, password)
	action := "server.sudoSaved"
	if password == "" {
		action = "server.sudoForgotten"
	}
	s.core.Audit(id, action, s.core.ServerName(id), "", err)
	return err
}

// Connect connects to a saved server. secret overrides the stored secret;
// when saveSecret is true a provided secret is written to the keychain.
func (s *ServerService) Connect(id string, secret string, saveSecret bool) (ConnectResult, error) {
	sv, err := s.core.Store.Get(id)
	if err != nil {
		return ConnectResult{}, err
	}
	if secret == "" {
		secret = s.core.Store.Secret(id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := s.core.Manager.Connect(ctx, sv, secret)
	if res, handled := connectOutcome(err); handled {
		return res, nil
	}
	if err != nil {
		return ConnectResult{}, err
	}
	if saveSecret && secret != "" {
		if _, err := s.core.Store.Save(sv, secret, true); err != nil {
			application.Get().Logger.Warn("save secret", "error", err)
		}
	}
	s.core.Store.Touch(id)
	s.core.Audit(id, "server.connect", sv.User+"@"+sv.Host, string(sv.AuthType), nil)
	s.detectOS(conn)
	return ConnectResult{Status: "ok", Home: conn.Home()}, nil
}

// Test tries a connection with unsaved settings and closes it immediately.
func (s *ServerService) Test(server store.Server, secret string) (ConnectResult, error) {
	if secret == "" && server.ID != "" {
		secret = s.core.Store.Secret(server.ID)
	}
	if server.Port == 0 {
		server.Port = 22
	}
	server.ID = "test-" + time.Now().Format("150405.000000000")
	mgr := sshx.NewManager(s.core.Manager.HostKeys(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := mgr.Connect(ctx, server, secret)
	if res, handled := connectOutcome(err); handled {
		return res, nil
	}
	if err != nil {
		return ConnectResult{}, err
	}
	home := conn.Home()
	conn.Close()
	return ConnectResult{Status: "ok", Home: home}, nil
}

func connectOutcome(err error) (ConnectResult, bool) {
	if err == nil {
		return ConnectResult{}, false
	}
	if errors.Is(err, sshx.ErrNeedSecret) {
		return ConnectResult{Status: "need-secret"}, true
	}
	var hk *sshx.HostKeyError
	if errors.As(err, &hk) {
		return ConnectResult{Status: "hostkey", HostKey: &HostKeyInfo{
			Host: hk.Host, Port: hk.Port, KeyType: hk.KeyType,
			Fingerprint: hk.Fingerprint, KeyBase64: hk.KeyBase64, Mismatch: hk.Mismatch,
		}}, true
	}
	return ConnectResult{}, false
}

func (s *ServerService) TrustHostKey(host string, port int, keyBase64 string) error {
	err := s.core.Manager.HostKeys().Trust(host, port, keyBase64)
	s.core.Audit("", "hostkey.trust", host+":"+strconv.Itoa(port), sshx.Fingerprint(keyBase64), err)
	return err
}

// detectOS records the server's OS name for the fleet view.
func (s *ServerService) detectOS(conn *sshx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, `sh -c '( . /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" ) || uname -sr'`, nil)
	if err == nil && res.ExitCode == 0 {
		if os := strings.TrimSpace(res.Stdout); os != "" {
			s.core.Store.SetOSInfo(conn.ID, os)
		}
	}
}

func (s *ServerService) Disconnect(id string) {
	if conn, err := s.core.Manager.Get(id); err == nil && conn.Connected() {
		s.core.Audit(id, "server.disconnect", s.core.ServerName(id), "", nil)
	}
	s.core.Manager.Disconnect(id)
}

func (s *ServerService) Reconnect(id string) error {
	conn, err := s.core.Conn(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return conn.Reconnect(ctx)
}

func (s *ServerService) IsConnected(id string) bool {
	conn, err := s.core.Conn(id)
	return err == nil && conn.Connected()
}

// PickKeyFile opens a native file picker in ~/.ssh.
func (s *ServerService) PickKeyFile(title string) (string, error) {
	dlg := application.Get().Dialog.OpenFile().
		SetTitle(title).
		ShowHiddenFiles(true).
		CanChooseFiles(true)
	if home, err := os.UserHomeDir(); err == nil {
		dlg.SetDirectory(filepath.Join(home, ".ssh"))
	}
	return dlg.PromptForSingleSelection()
}
