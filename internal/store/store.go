// Package store persists server profiles on the local machine. Profiles live in
// a JSON file under the user config dir; secrets (passwords, key passphrases)
// are kept in the OS keychain and never written to disk by this package.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"server-manager/internal/apperr"
)

const keyringService = "server-manager"

type AuthType string

const (
	AuthPassword AuthType = "password"
	AuthKey      AuthType = "key"
	AuthAgent    AuthType = "agent"
)

// Server is a saved connection profile.
type Server struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Group       string   `json:"group"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	User        string   `json:"user"`
	AuthType    AuthType `json:"authType"`
	KeyPath     string   `json:"keyPath"`
	DefaultPath string   `json:"defaultPath"`
	Color       string   `json:"color"`
	// HasSecret reports whether a password/passphrase is stored in the keychain.
	HasSecret bool  `json:"hasSecret"`
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
	LastUsed  int64 `json:"lastUsed"`

	// Organisation.
	Environment string   `json:"environment"` // production | staging | development | ""
	Tags        []string `json:"tags"`
	Region      string   `json:"region"`
	Provider    string   `json:"provider"`
	Notes       string   `json:"notes"`
	// Role limits what this app may do on the server (admin, operator,
	// developer, viewer); see core.Require. Empty means admin.
	Role string `json:"role"`

	// Monitoring: when Monitor is set the app keeps its own connection to
	// collect metrics, evaluate health checks and alerts in the background.
	Monitor       bool        `json:"monitor"`
	WatchServices []string    `json:"watchServices"`
	HTTPChecks    []HTTPCheck `json:"httpChecks"`

	// OSInfo is detected on connect ("Ubuntu 24.04 LTS").
	OSInfo string `json:"osInfo"`
	// SudoSaved reports whether a sudo password is stored in the keychain.
	SudoSaved bool `json:"sudoSaved"`
}

// HTTPCheck is an HTTP(S) health check attached to a server.
type HTTPCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Expect is the expected status code (0 = any 2xx/3xx).
	Expect int `json:"expect"`
	// Contains, when set, must appear in the response body.
	Contains string `json:"contains"`
	// ViaServer runs the request on the server itself (curl/wget), for
	// endpoints only reachable from localhost.
	ViaServer bool `json:"viaServer"`
}

func (s *Server) normalize() {
	if s.Tags == nil {
		s.Tags = []string{}
	}
	if s.WatchServices == nil {
		s.WatchServices = []string{}
	}
	if s.HTTPChecks == nil {
		s.HTTPChecks = []HTTPCheck{}
	}
}

type Store struct {
	mu      sync.Mutex
	dir     string
	file    string
	servers []Server
}

// Dir returns the application's config directory, creating it if needed.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "server-manager")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func New(dir string) (*Store, error) {
	s := &Store{dir: dir, file: filepath.Join(dir, "servers.json")}
	data, err := os.ReadFile(s.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.servers); err != nil {
			// Keep the broken file around instead of silently overwriting it.
			_ = os.Rename(s.file, s.file+".broken-"+time.Now().Format("20060102-150405"))
			s.servers = nil
		}
	}
	return s, nil
}

func (s *Store) List() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Server, len(s.servers))
	copy(out, s.servers)
	for i := range out {
		out[i].normalize()
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return strings.ToLower(out[i].Group) < strings.ToLower(out[j].Group)
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (s *Store) Get(id string) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.servers {
		if sv.ID == id {
			sv.normalize()
			return sv, nil
		}
	}
	return Server{}, apperr.New("server.notFound", "id", id)
}

// Save creates or updates a profile. When setSecret is true the secret is
// stored in (or, if empty, removed from) the keychain.
func (s *Store) Save(in Server, secret string, setSecret bool) (Server, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.User = strings.TrimSpace(in.User)
	in.KeyPath = strings.TrimSpace(in.KeyPath)
	in.Group = strings.TrimSpace(in.Group)
	if in.Host == "" {
		return Server{}, apperr.New("server.hostRequired")
	}
	if in.User == "" {
		return Server{}, apperr.New("server.userRequired")
	}
	if in.Port <= 0 || in.Port > 65535 {
		in.Port = 22
	}
	if in.Name == "" {
		in.Name = in.User + "@" + in.Host
	}
	switch in.AuthType {
	case AuthPassword, AuthKey, AuthAgent:
	default:
		in.AuthType = AuthPassword
	}
	switch in.Environment {
	case "production", "staging", "development", "":
	default:
		in.Environment = ""
	}
	switch in.Role {
	case "admin", "operator", "developer", "viewer":
	default:
		in.Role = "admin"
	}
	in.Tags = cleanList(in.Tags)
	in.WatchServices = cleanList(in.WatchServices)
	checks := in.HTTPChecks[:0:0]
	for _, c := range in.HTTPChecks {
		c.URL = strings.TrimSpace(c.URL)
		c.Name = strings.TrimSpace(c.Name)
		if c.URL != "" {
			checks = append(checks, c)
		}
	}
	in.HTTPChecks = checks
	in.normalize()

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	idx := -1
	if in.ID != "" {
		for i := range s.servers {
			if s.servers[i].ID == in.ID {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		in.ID = uuid.NewString()
		in.CreatedAt = now
	} else {
		in.CreatedAt = s.servers[idx].CreatedAt
		in.LastUsed = s.servers[idx].LastUsed
		in.HasSecret = s.servers[idx].HasSecret
		in.SudoSaved = s.servers[idx].SudoSaved
		if in.OSInfo == "" {
			in.OSInfo = s.servers[idx].OSInfo
		}
	}
	in.UpdatedAt = now

	if setSecret {
		if secret == "" {
			_ = keyring.Delete(keyringService, in.ID)
			in.HasSecret = false
		} else {
			if err := keyring.Set(keyringService, in.ID, secret); err != nil {
				return Server{}, apperr.Wrap(err, "keychain.saveFailed")
			}
			in.HasSecret = true
		}
	}

	if idx == -1 {
		s.servers = append(s.servers, in)
	} else {
		s.servers[idx] = in
	}
	return in, s.persist()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.servers[:0]
	for _, sv := range s.servers {
		if sv.ID != id {
			out = append(out, sv)
		}
	}
	s.servers = out
	_ = keyring.Delete(keyringService, id)
	_ = keyring.Delete(keyringService, "sudo:"+id)
	return s.persist()
}

// SetOSInfo records the detected operating system.
func (s *Store) SetOSInfo(id, os string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.servers {
		if s.servers[i].ID == id && s.servers[i].OSInfo != os {
			s.servers[i].OSInfo = os
			_ = s.persist()
		}
	}
}

// SudoSecret returns the stored sudo password, or "".
func (s *Store) SudoSecret(id string) string {
	v, err := keyring.Get(keyringService, "sudo:"+id)
	if err != nil {
		return ""
	}
	return v
}

// SetSudoSecret stores (or with "" removes) the sudo password.
func (s *Store) SetSudoSecret(id, pw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.servers {
		if s.servers[i].ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return apperr.New("server.notFound", "id", id)
	}
	if pw == "" {
		_ = keyring.Delete(keyringService, "sudo:"+id)
		s.servers[idx].SudoSaved = false
	} else {
		if err := keyring.Set(keyringService, "sudo:"+id, pw); err != nil {
			return apperr.Wrap(err, "keychain.saveFailed")
		}
		s.servers[idx].SudoSaved = true
	}
	return s.persist()
}

// SetKeychain stores an arbitrary secret (notification tokens, backup
// passphrases…) under name; "" deletes it.
func SetKeychain(name, value string) error {
	if value == "" {
		err := keyring.Delete(keyringService, name)
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := keyring.Set(keyringService, name, value); err != nil {
		return apperr.Wrap(err, "keychain.saveFailed")
	}
	return nil
}

// Keychain returns the secret stored under name, or "".
func Keychain(name string) string {
	v, err := keyring.Get(keyringService, name)
	if err != nil {
		return ""
	}
	return v
}

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func (s *Store) Touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.servers {
		if s.servers[i].ID == id {
			s.servers[i].LastUsed = time.Now().Unix()
		}
	}
	_ = s.persist()
}

// Secret returns the stored password/passphrase, or "" if none.
func (s *Store) Secret(id string) string {
	v, err := keyring.Get(keyringService, id)
	if err != nil {
		return ""
	}
	return v
}

// persist writes atomically so a crash never leaves a half-written file.
func (s *Store) persist() error {
	data, err := json.MarshalIndent(s.servers, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}
