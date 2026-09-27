package database

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// UnitInfo is a systemd unit of a database engine.
type UnitInfo struct {
	Name   string `json:"name"`
	Active string `json:"active"`
	Sub    string `json:"sub"`
}

// EngineInfo describes what was found on the host for one engine.
type EngineInfo struct {
	Engine        string     `json:"engine"`
	Client        string     `json:"client"` // path of psql / mysql / redis-cli ("" = missing)
	ClientVersion string     `json:"clientVersion"`
	ServerVersion string     `json:"serverVersion"`
	Flavor        string     `json:"flavor"` // mariadb | mysql (MySQL engine)
	Units         []UnitInfo `json:"units"`
	Ports         []int      `json:"ports"`
	Running       bool       `json:"running"`
	Installed     bool       `json:"installed"` // a server is installed on the host
}

// Detection is the result of scanning a server for database engines.
type Detection struct {
	Engines []EngineInfo `json:"engines"`
	Targets []Target     `json:"targets"`
	// Docker: none (not installed) | ok | sudo (usable through sudo) |
	// denied (no access: sudo password needed) | down (daemon not running).
	Docker     string `json:"docker"`
	Containers int    `json:"containers"`
}

const detectScript = `
for b in psql mysql mariadb redis-cli docker; do p=$(command -v "$b" 2>/dev/null) && echo "@@bin $b $p"; done
v=$(psql --version 2>/dev/null | head -n 1); [ -n "$v" ] && echo "@@ver psql $v"
v=$( (mariadb --version || mysql --version) 2>/dev/null | head -n 1); [ -n "$v" ] && echo "@@ver mysql $v"
v=$(redis-cli --version 2>/dev/null | head -n 1); [ -n "$v" ] && echo "@@ver redis-cli $v"
for p in $(command -v postgres 2>/dev/null) /usr/lib/postgresql/*/bin/postgres /usr/pgsql-*/bin/postgres /usr/local/pgsql/bin/postgres /usr/libexec/postgresql*/postgres /usr/bin/postgres; do
  [ -x "$p" ] && { v=$("$p" --version 2>/dev/null | head -n 1); [ -n "$v" ] && { echo "@@ver postgres $v"; break; }; }
done
for p in $(command -v mariadbd mysqld 2>/dev/null) /usr/sbin/mariadbd /usr/sbin/mysqld /usr/libexec/mysqld /usr/local/mysql/bin/mysqld; do
  [ -x "$p" ] && { v=$("$p" --version 2>/dev/null | head -n 1); [ -n "$v" ] && { echo "@@ver mysqld $v"; break; }; }
done
for p in $(command -v redis-server valkey-server 2>/dev/null) /usr/bin/redis-server /usr/local/bin/redis-server; do
  [ -x "$p" ] && { v=$("$p" --version 2>/dev/null | head -n 1); [ -n "$v" ] && { echo "@@ver redis-server $v"; break; }; }
done
if command -v systemctl >/dev/null 2>&1; then
  systemctl list-units --type=service --all --no-legend --plain --no-pager 'postgresql*' 'mysql*' 'mariadb*' 'redis*' 'valkey*' 2>/dev/null | sed 's/^/@@unit /'
fi
if command -v ss >/dev/null 2>&1; then ss -ltnH 2>/dev/null | awk '{print "@@listen " $4}'
elif command -v netstat >/dev/null 2>&1; then netstat -ltn 2>/dev/null | awk '/LISTEN/ {print "@@listen " $4}'
fi
(ps -eo comm= 2>/dev/null || ps -o comm 2>/dev/null) | sort -u | grep -E '^(postgres|postmaster|mysqld|mariadbd|redis-server|valkey-server)$' | sed 's/^/@@proc /'
if command -v docker >/dev/null 2>&1; then
  if out=$(docker ps --no-trunc --format '{{.ID}}	{{.Names}}	{{.Image}}	{{.Status}}	{{.Ports}}' 2>&1); then
    echo "@@docker ok"; printf '%s\n' "$out" | sed '/^$/d; s/^/@@ctr /'
  else case "$out" in *ermission*) echo "@@docker denied";; *) echo "@@docker down";; esac; fi
else echo "@@docker none"; fi
echo @@end
`

// rePublished finds host ports a container publishes ("0.0.0.0:3306->3306/tcp").
var rePublished = regexp.MustCompile(`:(\d+)->\d+/tcp`)

const dockerPS = `docker ps --no-trunc --format '{{.ID}}	{{.Names}}	{{.Image}}	{{.Status}}'`

// Detect scans the server for database engines (client binaries, server
// binaries, systemd units, listening ports, processes) and for running
// Docker containers of postgres/mysql/mariadb/redis images. When Docker
// needs root, sudo is used if it works without prompting (NOPASSWD, or a
// known password); otherwise Docker is reported as "denied".
func (s *DatabaseService) Detect(connID, sudoPassword string) (Detection, error) {
	return s.detect(connID, sudoPassword, false)
}

// DetectSudo is Detect that reports sudo errors (sudo.required,
// sudo.wrongPassword) when Docker needs root, so the UI can ask for the
// password and scan the containers.
func (s *DatabaseService) DetectSudo(connID, sudoPassword string) (Detection, error) {
	return s.detect(connID, sudoPassword, true)
}

func (s *DatabaseService) detect(connID, sudoPassword string, strict bool) (Detection, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return Detection{}, err
	}
	ctx, cancel := core.Timeout(40 * time.Second)
	defer cancel()
	res, err := s.core.Run(ctx, conn, detectScript, false, "", "")
	if err != nil {
		return Detection{}, err
	}
	d := parseDetect(res.Stdout)
	if d.Docker == "denied" {
		r, err := s.core.Run(ctx, conn, dockerPS, true, sudoPassword, "")
		if err == nil && r.ExitCode == 0 {
			d.Docker = "sudo"
			addContainers(&d, r.Stdout)
		} else if err != nil && strict {
			return Detection{}, err
		}
	}
	s.setDockerSudo(connID, d.Docker)
	for i := range d.Targets {
		d.Targets[i].Server = connID
	}
	s.mu.Lock()
	s.detected[connID] = detCache{at: time.Now(), targets: d.Targets}
	s.mu.Unlock()
	d.Targets = merge(d.Targets, s.saved(connID))
	return d, nil
}

var reVersion = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func parseDetect(out string) Detection {
	eng := map[string]*EngineInfo{
		EnginePostgres: {Engine: EnginePostgres, Units: []UnitInfo{}, Ports: []int{}},
		EngineMySQL:    {Engine: EngineMySQL, Units: []UnitInfo{}, Ports: []int{}},
		EngineRedis:    {Engine: EngineRedis, Units: []UnitInfo{}, Ports: []int{}},
	}
	procs := map[string]bool{}
	d := Detection{Docker: "none", Targets: []Target{}}
	var ctrLines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		kind, rest, _ := strings.Cut(line[2:], " ")
		switch kind {
		case "bin":
			name, path, _ := strings.Cut(rest, " ")
			switch name {
			case "psql":
				eng[EnginePostgres].Client = path
			case "mysql", "mariadb":
				if eng[EngineMySQL].Client == "" || name == "mariadb" {
					eng[EngineMySQL].Client = path
				}
			case "redis-cli":
				eng[EngineRedis].Client = path
			}
		case "ver":
			tool, v, _ := strings.Cut(rest, " ")
			switch tool {
			case "psql":
				eng[EnginePostgres].ClientVersion = reVersion.FindString(v)
			case "mysql":
				eng[EngineMySQL].ClientVersion = mysqlVersion(v)
			case "redis-cli":
				eng[EngineRedis].ClientVersion = reVersion.FindString(v)
			case "postgres":
				eng[EnginePostgres].ServerVersion = reVersion.FindString(v)
				eng[EnginePostgres].Installed = true
			case "mysqld":
				e := eng[EngineMySQL]
				e.ServerVersion = mysqlVersion(v)
				e.Installed = true
				if strings.Contains(strings.ToLower(v), "mariadb") {
					e.Flavor = "mariadb"
				} else {
					e.Flavor = "mysql"
				}
			case "redis-server":
				if m := regexp.MustCompile(`v=(\d+\.\d+\.\d+)`).FindStringSubmatch(v); m != nil {
					eng[EngineRedis].ServerVersion = m[1]
				} else {
					eng[EngineRedis].ServerVersion = reVersion.FindString(v)
				}
				eng[EngineRedis].Installed = true
			}
		case "unit":
			f := strings.Fields(rest)
			if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
				continue
			}
			u := UnitInfo{Name: f[0], Active: f[2], Sub: f[3]}
			n := strings.ToLower(f[0])
			var e *EngineInfo
			switch {
			case strings.HasPrefix(n, "postgresql"):
				e = eng[EnginePostgres]
				// Debian's postgresql.service is an umbrella (active/exited).
				if n == "postgresql.service" && u.Sub == "exited" {
					continue
				}
			case strings.HasPrefix(n, "mysql"), strings.HasPrefix(n, "mariadb"):
				e = eng[EngineMySQL]
			case strings.HasPrefix(n, "redis"), strings.HasPrefix(n, "valkey"):
				e = eng[EngineRedis]
				if strings.Contains(n, "sentinel") {
					continue
				}
			}
			if e == nil || f[1] == "not-found" {
				continue
			}
			e.Units = append(e.Units, u)
			e.Installed = true
			if u.Active == "active" {
				e.Running = true
			}
		case "listen":
			i := strings.LastIndex(rest, ":")
			if i < 0 {
				continue
			}
			p, err := strconv.Atoi(rest[i+1:])
			if err != nil {
				continue
			}
			var e *EngineInfo
			switch p {
			case 5432:
				e = eng[EnginePostgres]
			case 3306:
				e = eng[EngineMySQL]
			case 6379:
				e = eng[EngineRedis]
			}
			if e != nil && !containsInt(e.Ports, p) {
				e.Ports = append(e.Ports, p)
			}
		case "proc":
			switch rest {
			case "postgres", "postmaster":
				procs[EnginePostgres] = true
			case "mysqld", "mariadbd":
				procs[EngineMySQL] = true
			case "redis-server", "valkey-server":
				procs[EngineRedis] = true
			}
		case "docker":
			d.Docker = rest
		case "ctr":
			ctrLines = append(ctrLines, rest)
		}
	}
	// Engines running in containers: ports they publish (docker-proxy
	// listeners) and their processes (visible in the host's ps) must not
	// look like a host installation.
	var ctrs Detection
	addContainers(&ctrs, strings.Join(ctrLines, "\n"))
	inDocker := map[string]bool{}
	for _, t := range ctrs.Targets {
		inDocker[t.Engine] = true
	}
	published := map[int]bool{}
	for _, l := range ctrLines {
		for _, m := range rePublished.FindAllStringSubmatch(l, -1) {
			if p, err := strconv.Atoi(m[1]); err == nil {
				published[p] = true
			}
		}
	}
	for _, k := range []string{EnginePostgres, EngineMySQL, EngineRedis} {
		e := eng[k]
		ports := []int{}
		for _, p := range e.Ports {
			if !published[p] {
				ports = append(ports, p)
			}
		}
		e.Ports = ports
		hostEvidence := e.ServerVersion != "" || len(e.Units) > 0
		if procs[k] && (hostEvidence || !inDocker[k]) {
			e.Running, e.Installed = true, true
		}
		if e.Engine == EngineMySQL && e.Flavor == "" && e.ClientVersion != "" {
			e.Flavor = "mysql"
			if strings.Contains(strings.ToLower(e.Client), "mariadb") {
				e.Flavor = "mariadb"
			}
		}
		if len(e.Ports) > 0 && !e.Running {
			// A listener on the default port without a visible process
			// (another user's process list may be hidden).
			e.Running = true
		}
		d.Engines = append(d.Engines, *e)
		if !e.Installed && len(e.Ports) == 0 {
			continue
		}
		t := Target{ID: "local-" + k, Engine: k, Mode: ModeLocal, Auth: AuthPeer, Detected: true,
			Running: e.Running, Version: core.FirstNonEmpty(e.ServerVersion, e.ClientVersion)}
		t.Name = engineLabel(k)
		if k == EngineMySQL && e.Flavor == "mariadb" {
			t.Name = "MariaDB"
		}
		d.Targets = append(d.Targets, t)
	}
	addContainers(&d, strings.Join(ctrLines, "\n"))
	return d
}

func addContainers(d *Detection, out string) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 3 || !reContainer.MatchString(f[1]) {
			continue
		}
		engine := imageEngine(f[2])
		if engine == "" {
			continue
		}
		d.Containers++
		name := f[1]
		t := Target{ID: "docker-" + name, Engine: engine, Name: name, Mode: ModeDocker, Container: name,
			Auth: AuthPeer, Detected: true, Running: true, Image: f[2]}
		if _, tag, ok := strings.Cut(imageBase(f[2]), ":"); ok {
			t.Version = tag
		}
		d.Targets = append(d.Targets, t)
	}
}

// imageBase is the last path element of an image reference ("postgres:16").
func imageBase(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return image
}

// imageEngine maps a Docker image to an engine ("" = not a database).
func imageEngine(image string) string {
	name, _, _ := strings.Cut(strings.ToLower(imageBase(image)), ":")
	switch {
	case strings.Contains(name, "postgres"), strings.Contains(name, "postgis"), strings.Contains(name, "timescale"), name == "pgvector":
		if strings.Contains(name, "exporter") || strings.Contains(name, "pgadmin") || strings.Contains(name, "backup") {
			return ""
		}
		return EnginePostgres
	case name == "mysql", name == "mariadb", strings.HasPrefix(name, "percona-server"), name == "mysql-server":
		return EngineMySQL
	case name == "redis", strings.HasPrefix(name, "redis-stack"), name == "valkey", name == "keydb":
		return EngineRedis
	}
	return ""
}

func mysqlVersion(v string) string {
	// "mysql  Ver 15.1 Distrib 10.11.6-MariaDB, for debian-linux-gnu" / "mysql  Ver 8.0.36 for Linux"
	if m := regexp.MustCompile(`Distrib (\d+\.\d+\.\d+)`).FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile(`Ver (\d+\.\d+\.\d+)`).FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return reVersion.FindString(v)
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---- docker access ----

func (s *DatabaseService) setDockerSudo(connID, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case "ok":
		s.docker[connID] = false
	case "sudo", "denied":
		s.docker[connID] = true
	}
}

// dockerNeedsSudo reports whether docker must run through sudo (cached).
func (s *DatabaseService) dockerNeedsSudo(ctx context.Context, conn *sshx.Conn) bool {
	s.mu.Lock()
	v, ok := s.docker[conn.ID]
	s.mu.Unlock()
	if ok {
		return v
	}
	res, err := s.core.Run(ctx, conn, "docker ps -q >/dev/null 2>&1 && echo ok", false, "", "")
	need := (err != nil || strings.TrimSpace(res.Stdout) != "ok") && !s.core.IsRoot(ctx, conn)
	s.mu.Lock()
	s.docker[conn.ID] = need
	s.mu.Unlock()
	return need
}
