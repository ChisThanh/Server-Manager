// Package database: PostgreSQL, MySQL/MariaDB and Redis status, queries and
// maintenance, agentlessly through the clients installed on the server
// (psql, mysql/mariadb, redis-cli), locally or inside Docker containers.
//
// Security rules followed throughout the package:
//   - secrets never appear on a command line: passwords are read from stdin
//     into an environment variable (PGPASSWORD, MYSQL_PWD, REDISCLI_AUTH)
//     by the remote shell, and passed to docker exec by name only;
//   - identifiers are validated with strict patterns and quoted, never
//     escaped;
//   - user SQL is lexed before it is sent: client meta-commands are
//     rejected, and read-only mode only lets through reading statements
//     (on top of a read-only transaction enforced by the server).
package database

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/store"
)

type DatabaseService struct {
	core *core.Core

	mu       sync.Mutex
	detected map[string]detCache // connID → last detection
	flavors  map[string]string   // connID/targetID → "mariadb" | "mysql"
	docker   map[string]bool     // connID → docker needs sudo
}

type detCache struct {
	at      time.Time
	targets []Target
}

func New(c *core.Core) *DatabaseService {
	return &DatabaseService{core: c, detected: map[string]detCache{}, flavors: map[string]string{}, docker: map[string]bool{}}
}

// Engines, modes and auth methods.
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
	EngineRedis    = "redis"

	ModeLocal  = "local"
	ModeDocker = "docker"

	// AuthPeer: local admin access — `sudo -u postgres psql`, `sudo mysql`
	// (unix_socket), plain redis-cli; in a container, the credentials of
	// the container's own environment (POSTGRES_USER, MYSQL_ROOT_PASSWORD).
	AuthPeer = "peer"
	// AuthPassword: user + password (stored in the OS keychain).
	AuthPassword = "password"
)

const (
	nsTarget  = "db.target"
	nsHistory = "db.history"
	nsSnippet = "db.snippet"
)

// Target is a database server the app can talk to: detected on the host or
// in a container, or added by the user.
type Target struct {
	ID        string `json:"id"`
	Server    string `json:"server"`
	Engine    string `json:"engine"` // postgres | mysql | redis
	Name      string `json:"name"`
	Mode      string `json:"mode"`      // local | docker
	Container string `json:"container"` // docker mode
	Auth      string `json:"auth"`      // peer | password
	Host      string `json:"host"`      // "" = local socket / default
	Port      int    `json:"port"`      // 0 = default
	User      string `json:"user"`
	Database  string `json:"database"` // default database ("" = engine default)
	// Detected targets come from Detect; Saved ones have a stored record
	// (a user-defined target, or edited settings of a detected one).
	Detected    bool   `json:"detected"`
	Saved       bool   `json:"saved"`
	HasPassword bool   `json:"hasPassword"`
	Version     string `json:"version"` // from detection, when known
	Running     bool   `json:"running"`
	Image       string `json:"image"` // docker image
}

// TargetInput is what the UI sends to create or edit a target.
type TargetInput struct {
	ID        string `json:"id"` // "" = new
	Engine    string `json:"engine"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	Container string `json:"container"`
	Auth      string `json:"auth"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	User      string `json:"user"`
	Database  string `json:"database"`
}

// savedTarget is the kv record (never contains the password).
type savedTarget struct {
	Target
	Updated int64 `json:"updated"`
}

// ---- validation ----

var (
	reIdent     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
	reDBName    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)
	reUser      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@$-]{0,62}$`)
	reHost      = regexp.MustCompile(`^[A-Za-z0-9_\[][A-Za-z0-9_.:\[\]-]{0,252}$`)
	reSocket    = regexp.MustCompile(`^/[A-Za-z0-9_./-]{1,200}$`)
	reContainer = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	reTargetID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,160}$`)
	reMyHost    = regexp.MustCompile(`^[A-Za-z0-9_.%:-]{1,255}$`)
)

func validEngine(e string) bool { return e == EnginePostgres || e == EngineMySQL || e == EngineRedis }

// checkDBName validates a database name used in commands and statements.
func checkDBName(n string) error {
	if !reDBName.MatchString(n) {
		return apperr.New("db.invalidName", "name", n)
	}
	return nil
}

// checkIdent validates a name we create (database, role): the strict
// identifier subset that needs no escaping in any engine.
func checkIdent(n string) error {
	if !reIdent.MatchString(n) {
		return apperr.New("db.invalidName", "name", n)
	}
	return nil
}

func validSecret(pw string) error {
	if strings.ContainsAny(pw, "\n\r\x00") || len(pw) > 1024 {
		return apperr.New("db.invalidPassword")
	}
	return nil
}

func (in TargetInput) validate() error {
	if !validEngine(in.Engine) {
		return apperr.New("db.invalidEngine", "engine", in.Engine)
	}
	switch in.Mode {
	case ModeLocal:
	case ModeDocker:
		if !reContainer.MatchString(in.Container) {
			return apperr.New("db.invalidContainer", "name", in.Container)
		}
	default:
		return apperr.New("db.invalidMode", "mode", in.Mode)
	}
	if in.Auth != AuthPeer && in.Auth != AuthPassword {
		return apperr.New("db.invalidAuth", "auth", in.Auth)
	}
	if in.Host != "" && !reHost.MatchString(in.Host) && !reSocket.MatchString(in.Host) {
		return apperr.New("db.invalidHost", "host", in.Host)
	}
	if in.Port < 0 || in.Port > 65535 {
		return apperr.New("db.invalidPort")
	}
	if in.User != "" && !reUser.MatchString(in.User) {
		return apperr.New("db.invalidUser", "user", in.User)
	}
	if in.Auth == AuthPassword && in.User == "" && in.Engine != EngineRedis {
		return apperr.New("db.invalidUser", "user", "")
	}
	if in.Database != "" {
		if in.Engine == EngineRedis {
			if !regexp.MustCompile(`^[0-9]{1,4}$`).MatchString(in.Database) {
				return apperr.New("db.invalidName", "name", in.Database)
			}
		} else if err := checkDBName(in.Database); err != nil {
			return err
		}
	}
	if len(in.Name) > 80 {
		return apperr.New("db.invalidName", "name", in.Name[:80])
	}
	return nil
}

// ---- targets ----

func secretName(server, id string) string { return "db:" + server + ":" + id }

func (s *DatabaseService) saved(connID string) []Target {
	list, _ := db.List[savedTarget](s.core.DB, nsTarget)
	out := []Target{}
	for _, t := range list {
		if t.Server == connID {
			t.Saved = true
			t.HasPassword = t.Auth == AuthPassword && store.Keychain(secretName(connID, t.ID)) != ""
			out = append(out, t.Target)
		}
	}
	return out
}

// merge overlays saved records on detected targets.
func merge(detected, saved []Target) []Target {
	byID := map[string]int{}
	out := []Target{}
	for _, t := range detected {
		byID[t.ID] = len(out)
		out = append(out, t)
	}
	for _, sv := range saved {
		if i, ok := byID[sv.ID]; ok {
			d := out[i]
			sv.Detected = true
			sv.Running = d.Running
			sv.Version = d.Version
			sv.Image = d.Image
			out[i] = sv
			continue
		}
		if sv.Detected {
			sv.Running = false // detected before, not found now
		}
		out = append(out, sv)
	}
	rank := map[string]int{EnginePostgres: 0, EngineMySQL: 1, EngineRedis: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Mode != out[j].Mode {
			return out[i].Mode == ModeLocal
		}
		if rank[out[i].Engine] != rank[out[j].Engine] {
			return rank[out[i].Engine] < rank[out[j].Engine]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Targets lists the detected and saved targets of a server. Detection runs
// when it hasn't yet this session (or is older than 10 minutes).
func (s *DatabaseService) Targets(connID string) ([]Target, error) {
	s.mu.Lock()
	c, ok := s.detected[connID]
	s.mu.Unlock()
	if !ok || time.Since(c.at) > 10*time.Minute {
		d, err := s.Detect(connID, "")
		if err != nil {
			return nil, err
		}
		return d.Targets, nil
	}
	return merge(c.targets, s.saved(connID)), nil
}

// target resolves a target id (saved record, else detected).
func (s *DatabaseService) target(connID, id string) (Target, error) {
	if !reTargetID.MatchString(id) {
		return Target{}, apperr.New("db.targetNotFound", "id", id)
	}
	for _, t := range s.saved(connID) {
		if t.ID == id {
			return t, nil
		}
	}
	s.mu.Lock()
	c, ok := s.detected[connID]
	s.mu.Unlock()
	if !ok {
		if _, err := s.Detect(connID, ""); err != nil {
			return Target{}, err
		}
		s.mu.Lock()
		c = s.detected[connID]
		s.mu.Unlock()
	}
	for _, t := range c.targets {
		if t.ID == id {
			return t, nil
		}
	}
	return Target{}, apperr.New("db.targetNotFound", "id", id)
}

// SaveTarget creates or updates a target. password is stored in the OS
// keychain when setPassword is true ("" removes it).
func (s *DatabaseService) SaveTarget(connID string, in TargetInput, password string, setPassword bool) (Target, error) {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return Target{}, err
	}
	t, err := s.saveTarget(connID, in, password, setPassword)
	s.core.Audit(connID, "db.target.save", core.FirstNonEmpty(t.Name, in.Name, in.ID), in.Engine+" "+in.Mode+" auth="+in.Auth, err)
	return t, err
}

func (s *DatabaseService) saveTarget(connID string, in TargetInput, password string, setPassword bool) (Target, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	in.User = strings.TrimSpace(in.User)
	in.Database = strings.TrimSpace(in.Database)
	if err := in.validate(); err != nil {
		return Target{}, err
	}
	if setPassword {
		if err := validSecret(password); err != nil {
			return Target{}, err
		}
	}
	var prev Target
	if in.ID == "" {
		in.ID = "t-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	} else {
		if !reTargetID.MatchString(in.ID) {
			return Target{}, apperr.New("db.targetNotFound", "id", in.ID)
		}
		if p, err := s.target(connID, in.ID); err == nil {
			prev = p
		}
	}
	if in.Name == "" {
		in.Name = core.FirstNonEmpty(prev.Name, engineLabel(in.Engine))
	}
	t := Target{
		ID: in.ID, Server: connID, Engine: in.Engine, Name: in.Name, Mode: in.Mode, Container: in.Container,
		Auth: in.Auth, Host: in.Host, Port: in.Port, User: in.User, Database: in.Database,
		Detected: prev.Detected, Version: prev.Version, Running: prev.Running, Image: prev.Image,
	}
	if in.Mode == ModeLocal {
		t.Container = ""
	}
	if setPassword {
		if err := store.SetKeychain(secretName(connID, t.ID), password); err != nil {
			return Target{}, err
		}
	}
	if t.Auth != AuthPassword {
		_ = store.SetKeychain(secretName(connID, t.ID), "")
	}
	if err := s.core.DB.Put(nsTarget, connID+"/"+t.ID, savedTarget{Target: t, Updated: time.Now().Unix()}); err != nil {
		return Target{}, err
	}
	s.forgetFlavor(connID, t.ID)
	t.Saved = true
	t.HasPassword = t.Auth == AuthPassword && store.Keychain(secretName(connID, t.ID)) != ""
	return t, nil
}

// DeleteTarget removes a saved target (a detected one reverts to its
// detected settings) and its stored password.
func (s *DatabaseService) DeleteTarget(connID, id string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	var err error
	if !reTargetID.MatchString(id) {
		err = apperr.New("db.targetNotFound", "id", id)
	} else {
		_ = store.SetKeychain(secretName(connID, id), "")
		err = s.core.DB.Delete(nsTarget, connID+"/"+id)
		_ = s.core.DB.Delete(nsHistory, connID+"/"+id)
		s.forgetFlavor(connID, id)
	}
	s.core.Audit(connID, "db.target.delete", id, "", err)
	return err
}

func (s *DatabaseService) forgetFlavor(connID, id string) {
	s.mu.Lock()
	delete(s.flavors, connID+"/"+id)
	s.mu.Unlock()
}

func engineLabel(e string) string {
	switch e {
	case EnginePostgres:
		return "PostgreSQL"
	case EngineMySQL:
		return "MySQL"
	case EngineRedis:
		return "Redis"
	}
	return e
}
