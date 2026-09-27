package backup

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
	"golang.org/x/text/unicode/norm"

	"server-manager/internal/apperr"
)

// kv namespaces.
const (
	nsDest  = "backup.dest"
	nsJob   = "backup.job"
	nsState = "backup.state"
)

// Job types.
const (
	TypeFiles    = "files"
	TypePostgres = "postgres"
	TypeMySQL    = "mysql"
	TypeVolume   = "volume"
	TypeRedis    = "redis"
	TypeCustom   = "custom"
)

// Destination types.
const (
	DestLocal  = "local"
	DestServer = "server"
	DestS3     = "s3"
)

// Destination is where backups are stored. Destinations are global: any
// server's jobs may use them. The S3 secret key lives in the keychain.
type Destination struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // local | server | s3
	// Path is the folder on this computer (local) or on the backed-up
	// server (server).
	Path string `json:"path"`
	// S3-compatible storage.
	Endpoint  string `json:"endpoint"` // host[:port], no scheme
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"accessKey"`
	PathStyle bool   `json:"pathStyle"`
	UseTLS    bool   `json:"useTls"`
	// HasSecret reports whether the S3 secret key is stored in the keychain.
	HasSecret bool  `json:"hasSecret"`
	Created   int64 `json:"created"`
	Updated   int64 `json:"updated"`
}

// Schedule is the user-facing schedule; Cron is derived from it.
type Schedule struct {
	Mode    string `json:"mode"`    // manual | hourly | daily | weekly | cron
	Minute  int    `json:"minute"`  // hourly: minute past the hour
	Time    string `json:"time"`    // daily/weekly: "HH:MM"
	Weekday int    `json:"weekday"` // weekly: 0=Sunday … 6=Saturday
	Cron    string `json:"cron"`    // cron mode: 5-field expression; otherwise derived
}

// Retention limits how many backups a job keeps at its destination.
type Retention struct {
	KeepLast   int `json:"keepLast"`   // 0 = unlimited
	MaxAgeDays int `json:"maxAgeDays"` // 0 = unlimited
}

// Job is a backup definition for one server.
type Job struct {
	ID      string `json:"id"`
	Server  string `json:"server"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	// Folder is "<server-slug>/<job-slug>", fixed when the job is created so
	// renaming a job doesn't orphan its backups.
	Folder string `json:"folder"`

	// files
	Paths    []string `json:"paths"`
	Excludes []string `json:"excludes"`
	// Sudo runs the backup command as root (files, custom, password-auth
	// databases). Peer/socket database auth, volumes and redis always do.
	Sudo bool `json:"sudo"`

	// postgres / mysql / redis
	Database string `json:"database"` // "" = all databases
	AuthMode string `json:"authMode"` // peer (postgres via sudo -u postgres, mysql root socket) | password
	DBUser   string `json:"dbUser"`
	DBHost   string `json:"dbHost"`
	DBPort   int    `json:"dbPort"`
	// HasDBPassword reports whether a database/redis password is stored.
	HasDBPassword bool `json:"hasDbPassword"`

	// docker volume
	Volume         string `json:"volume"`
	StopContainers bool   `json:"stopContainers"`

	// redis
	RedisDumpPath string `json:"redisDumpPath"` // "" = ask redis (CONFIG GET dir/dbfilename)

	// custom
	Command        string `json:"command"`
	Ext            string `json:"ext"`
	RestoreCommand string `json:"restoreCommand"`

	Compression string `json:"compression"` // none | gzip | zstd
	Encrypt     bool   `json:"encrypt"`
	// HasPassphrase reports whether the encryption passphrase is stored.
	HasPassphrase bool `json:"hasPassphrase"`
	// VerifyAfter re-reads the uploaded backup and checks its hash.
	VerifyAfter bool `json:"verifyAfter"`

	Schedule    Schedule  `json:"schedule"`
	Retention   Retention `json:"retention"`
	Destination string    `json:"destination"`
	PreHook     string    `json:"preHook"`
	PostHook    string    `json:"postHook"`
	TimeoutMin  int       `json:"timeoutMin"`

	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// runsAsRoot reports whether the backup command needs root on the server.
func (j *Job) runsAsRoot() bool {
	switch j.Type {
	case TypeVolume, TypeRedis:
		return true
	case TypePostgres, TypeMySQL:
		return j.AuthMode != "password" || j.Sudo
	}
	return j.Sudo
}

// ---- validation ----

var (
	reDBName  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_$.-]{0,62}$`)
	reDBUser  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]{0,62}$`)
	reHost    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	reVolume  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
	reExt     = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	reTime    = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
	reBucket  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	reRegion  = regexp.MustCompile(`^[A-Za-z0-9-]{0,64}$`)
	reID      = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	reAccess  = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	reSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

func invalid(field string) error { return apperr.New("backup.invalid", "field", field) }

func hasCtl(s string) bool {
	for _, r := range s {
		if r == 0 || r == '\n' || r == '\r' || (unicode.IsControl(r) && r != '\t') {
			return true
		}
	}
	return false
}

func validName(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && utf8.RuneCountInString(s) <= 64 && !hasCtl(s)
}

// validServerPath accepts a clean absolute POSIX path.
func validServerPath(p string) bool {
	return p != "" && strings.HasPrefix(p, "/") && !hasCtl(p) && path.Clean(p) == p && len(p) < 4096
}

func validHost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return reHost.MatchString(h)
}

// normalizeJob validates j in place and derives Schedule.Cron.
func normalizeJob(j *Job) error {
	j.Name = strings.TrimSpace(j.Name)
	if !validName(j.Name) {
		return invalid("name")
	}
	if j.Paths == nil {
		j.Paths = []string{}
	}
	if j.Excludes == nil {
		j.Excludes = []string{}
	}
	switch j.Type {
	case TypeFiles:
		var paths []string
		for _, p := range j.Paths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !strings.HasPrefix(p, "/") || hasCtl(p) {
				return apperr.New("backup.invalidPath", "path", p)
			}
			paths = append(paths, path.Clean(p))
		}
		if len(paths) == 0 {
			return invalid("paths")
		}
		j.Paths = paths
		ex := []string{}
		for _, e := range j.Excludes {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if hasCtl(e) || len(e) > 1024 {
				return apperr.New("backup.invalidPath", "path", e)
			}
			ex = append(ex, e)
		}
		j.Excludes = ex
	case TypePostgres, TypeMySQL:
		if j.Database != "" && !reDBName.MatchString(j.Database) {
			return invalid("database")
		}
		if j.AuthMode != "password" {
			j.AuthMode = "peer"
		} else {
			if !reDBUser.MatchString(j.DBUser) {
				return invalid("dbUser")
			}
		}
		if j.DBHost != "" && !validHost(j.DBHost) {
			return invalid("dbHost")
		}
		if j.DBPort < 0 || j.DBPort > 65535 {
			return invalid("dbPort")
		}
	case TypeVolume:
		if !reVolume.MatchString(j.Volume) {
			return invalid("volume")
		}
	case TypeRedis:
		if j.DBHost != "" && !validHost(j.DBHost) {
			return invalid("dbHost")
		}
		if j.DBPort < 0 || j.DBPort > 65535 {
			return invalid("dbPort")
		}
		if j.RedisDumpPath != "" && !validServerPath(j.RedisDumpPath) {
			return invalid("redisDumpPath")
		}
		if j.AuthMode != "password" {
			j.AuthMode = "none"
		}
	case TypeCustom:
		j.Command = strings.TrimSpace(j.Command)
		if j.Command == "" || strings.ContainsRune(j.Command, 0) || len(j.Command) > 8192 {
			return invalid("command")
		}
		if j.Ext == "" {
			j.Ext = "bin"
		}
		if !reExt.MatchString(j.Ext) {
			return invalid("ext")
		}
		if strings.ContainsRune(j.RestoreCommand, 0) || len(j.RestoreCommand) > 8192 {
			return invalid("restoreCommand")
		}
	default:
		return invalid("type")
	}
	switch j.Compression {
	case "", "none":
		j.Compression = "none"
	case "gzip", "zstd":
	default:
		return invalid("compression")
	}
	for _, h := range []string{j.PreHook, j.PostHook} {
		if strings.ContainsRune(h, 0) || len(h) > 8192 {
			return invalid("hook")
		}
	}
	j.PreHook = strings.TrimSpace(j.PreHook)
	j.PostHook = strings.TrimSpace(j.PostHook)
	if j.TimeoutMin == 0 {
		j.TimeoutMin = 120
	}
	if j.TimeoutMin < 1 || j.TimeoutMin > 7*24*60 {
		return invalid("timeoutMin")
	}
	if j.Retention.KeepLast < 0 || j.Retention.KeepLast > 100000 {
		return invalid("keepLast")
	}
	if j.Retention.MaxAgeDays < 0 || j.Retention.MaxAgeDays > 36500 {
		return invalid("maxAgeDays")
	}
	if !reID.MatchString(j.Destination) {
		return apperr.New("backup.noDestination")
	}
	expr, err := cronOf(j.Schedule)
	if err != nil {
		return err
	}
	j.Schedule.Cron = expr
	return nil
}

// cronOf turns a schedule into a cron expression ("" = manual only).
func cronOf(s Schedule) (string, error) {
	hm := func() (int, int, error) {
		m := reTime.FindStringSubmatch(strings.TrimSpace(s.Time))
		if m == nil {
			return 0, 0, invalid("time")
		}
		var h, mi int
		fmt.Sscan(m[1], &h)
		fmt.Sscan(m[2], &mi)
		return h, mi, nil
	}
	switch s.Mode {
	case "", "manual":
		return "", nil
	case "hourly":
		if s.Minute < 0 || s.Minute > 59 {
			return "", invalid("minute")
		}
		return fmt.Sprintf("%d * * * *", s.Minute), nil
	case "daily":
		h, m, err := hm()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %d * * *", m, h), nil
	case "weekly":
		h, m, err := hm()
		if err != nil {
			return "", err
		}
		if s.Weekday < 0 || s.Weekday > 6 {
			return "", invalid("weekday")
		}
		return fmt.Sprintf("%d %d * * %d", m, h, s.Weekday), nil
	case "cron":
		expr := strings.TrimSpace(s.Cron)
		if _, err := parseCron(expr); err != nil {
			return "", err
		}
		return expr, nil
	}
	return "", invalid("schedule")
}

// parseCron parses a standard 5-field expression (or @daily etc.).
func parseCron(expr string) (cron.Schedule, error) {
	if expr == "" || hasCtl(expr) || len(expr) > 200 {
		return nil, apperr.New("backup.invalidCron")
	}
	sc, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, apperr.New("backup.invalidCron").WithDetail(err.Error())
	}
	if sc.Next(time.Now()).IsZero() {
		return nil, apperr.New("backup.invalidCron").WithDetail("never fires")
	}
	return sc, nil
}

// nextRuns returns the next n activation times after from.
func nextRuns(expr string, from time.Time, n int) ([]time.Time, error) {
	sc, err := parseCron(expr)
	if err != nil {
		return nil, err
	}
	out := []time.Time{}
	t := from
	for i := 0; i < n; i++ {
		t = sc.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

// normalizeDest validates a destination in place.
func normalizeDest(d *Destination) error {
	d.Name = strings.TrimSpace(d.Name)
	if !validName(d.Name) {
		return invalid("name")
	}
	switch d.Type {
	case DestLocal:
		d.Path = strings.TrimSpace(d.Path)
		if d.Path == "" || hasCtl(d.Path) || !isAbsLocal(d.Path) {
			return invalid("path")
		}
	case DestServer:
		d.Path = strings.TrimRight(strings.TrimSpace(d.Path), "/")
		if !validServerPath(d.Path) || d.Path == "/" {
			return invalid("path")
		}
	case DestS3:
		ep := strings.TrimSpace(d.Endpoint)
		switch {
		case strings.HasPrefix(ep, "https://"):
			ep, d.UseTLS = strings.TrimPrefix(ep, "https://"), true
		case strings.HasPrefix(ep, "http://"):
			ep, d.UseTLS = strings.TrimPrefix(ep, "http://"), false
		}
		ep = strings.TrimRight(ep, "/")
		host := ep
		if h, p, err := net.SplitHostPort(ep); err == nil {
			host = h
			var port int
			if _, err := fmt.Sscan(p, &port); err != nil || port < 1 || port > 65535 {
				return invalid("endpoint")
			}
		}
		if !validHost(host) {
			return invalid("endpoint")
		}
		d.Endpoint = ep
		d.Bucket = strings.TrimSpace(d.Bucket)
		if !reBucket.MatchString(d.Bucket) {
			return invalid("bucket")
		}
		d.Region = strings.TrimSpace(d.Region)
		if !reRegion.MatchString(d.Region) {
			return invalid("region")
		}
		d.Prefix = strings.Trim(strings.TrimSpace(d.Prefix), "/")
		if hasCtl(d.Prefix) || strings.Contains(d.Prefix, "//") || len(d.Prefix) > 512 {
			return invalid("prefix")
		}
		for _, seg := range strings.Split(d.Prefix, "/") {
			if seg == "." || seg == ".." {
				return invalid("prefix")
			}
		}
		d.AccessKey = strings.TrimSpace(d.AccessKey)
		if !reAccess.MatchString(d.AccessKey) {
			return invalid("accessKey")
		}
	default:
		return invalid("type")
	}
	return nil
}

// ---- naming ----

// slug makes a lowercase ASCII path segment ("Sao lưu DB" → "sao-luu-db").
func slug(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "đ", "d"), "Đ", "D")
	var b strings.Builder
	dash := false
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "-")
	}
	if out == "" {
		out = "x"
	}
	return out
}

const tsLayout = "20060102-150405"

// dumpExt is the extension of the raw dump produced by a job.
func dumpExt(j *Job) string {
	switch j.Type {
	case TypeFiles, TypeVolume:
		return "tar"
	case TypePostgres:
		if j.Database == "" {
			return "sql"
		}
		return "dump"
	case TypeMySQL:
		return "sql"
	case TypeRedis:
		return "rdb"
	case TypeCustom:
		return j.Ext
	}
	return "bin"
}

// objectName is "<YYYYMMDD-HHMMSS>.<ext>[.gz|.zst][.age]" (UTC).
func objectName(t time.Time, ext, compression string, encrypted bool) string {
	n := t.UTC().Format(tsLayout) + "." + ext
	switch compression {
	case "gzip":
		n += ".gz"
	case "zstd":
		n += ".zst"
	}
	if encrypted {
		n += ".age"
	}
	return n
}

const manifestSuffix = ".manifest.json"

var reObject = regexp.MustCompile(`^(\d{8}-\d{6})\.[a-z0-9]{1,10}(\.gz|\.zst)?(\.age)?$`)

// parseObjectName returns the backup time encoded in a data object name.
func parseObjectName(name string) (time.Time, bool) {
	m := reObject.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(tsLayout, m[1], time.UTC)
	return t, err == nil
}

// validFolder checks "<seg>/<seg>".
func validFolder(f string) bool {
	parts := strings.Split(f, "/")
	return len(parts) == 2 && reSegment.MatchString(parts[0]) && reSegment.MatchString(parts[1])
}

// checkKey verifies that key names a data object of the job's folder.
func checkKey(j *Job, key string) (string, error) {
	dir, name := path.Split(key)
	if strings.TrimSuffix(dir, "/") != j.Folder || !validFolder(j.Folder) {
		return "", apperr.New("backup.invalidKey")
	}
	if _, ok := parseObjectName(name); !ok {
		return "", apperr.New("backup.invalidKey")
	}
	return name, nil
}
