// Package logs: Log viewing and live streaming (journald, files, Docker) and systemd service details.
package logs

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// EventLines carries batches of streamed log lines to the frontend.
const EventLines = "logs:lines"

// LinesEvent is one batch of a live stream. The last event of a stream has
// Done set (and Error when it ended abnormally).
type LinesEvent struct {
	StreamID string        `json:"streamId"`
	Lines    []LogLine     `json:"lines"`
	Dropped  int           `json:"dropped"` // lines skipped because the UI could not keep up
	Done     bool          `json:"done"`
	Error    *apperr.Error `json:"error"`
}

func init() {
	application.RegisterEvent[LinesEvent](EventLines)
}

type LogsService struct {
	core *core.Core

	capsMu sync.Mutex
	caps   map[*sshx.Conn]*caps

	streamsMu sync.Mutex
	streams   map[string]*stream
}

func New(c *core.Core) *LogsService {
	return &LogsService{core: c, caps: map[*sshx.Conn]*caps{}, streams: map[string]*stream{}}
}

// ServiceShutdown stops every live stream.
func (s *LogsService) ServiceShutdown() error {
	s.stopAll()
	return nil
}

// caps describes what a server offers, probed once per connection.
type caps struct {
	at          time.Time
	root        bool
	groups      []string
	journalctl  bool
	journalVer  int
	pcre        bool // journalctl -g
	systemd     bool
	docker      bool
	journalRead bool // may read the system journal without sudo
	loc         *time.Location
}

const capsTTL = 5 * time.Minute

const capsScript = `
echo "@@uid $(id -u)"
echo "@@groups $(id -Gn 2>/dev/null)"
if command -v journalctl >/dev/null 2>&1; then echo "@@journal $(journalctl --version 2>/dev/null | tr '\n' ' ')"; fi
[ -d /run/systemd/system ] && echo "@@systemd"
command -v docker >/dev/null 2>&1 && echo "@@docker"
echo "@@tz $(date +%z 2>/dev/null)"
`

func (s *LogsService) getCaps(ctx context.Context, conn *sshx.Conn) (*caps, error) {
	s.capsMu.Lock()
	c := s.caps[conn]
	s.capsMu.Unlock()
	if c != nil && time.Since(c.at) < capsTTL {
		return c, nil
	}
	res, err := s.core.Run(ctx, conn, capsScript, false, "", "")
	if err != nil {
		return nil, err
	}
	c = parseCaps(res.Stdout)
	s.capsMu.Lock()
	// Forget closed connections so the map doesn't grow.
	for k := range s.caps {
		if !k.Connected() {
			delete(s.caps, k)
		}
	}
	s.caps[conn] = c
	s.capsMu.Unlock()
	return c, nil
}

func parseCaps(out string) *caps {
	c := &caps{at: time.Now(), loc: time.UTC}
	for _, l := range strings.Split(out, "\n") {
		key, val, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch key {
		case "@@uid":
			c.root = strings.TrimSpace(val) == "0"
		case "@@groups":
			c.groups = strings.Fields(val)
		case "@@journal":
			c.journalctl = true
			f := strings.Fields(val)
			if len(f) >= 2 && f[0] == "systemd" {
				c.journalVer, _ = strconv.Atoi(f[1])
			}
			c.pcre = strings.Contains(val, "+PCRE2")
		case "@@systemd":
			c.systemd = true
		case "@@docker":
			c.docker = true
		case "@@tz":
			c.loc = parseTZ(strings.TrimSpace(val))
		}
	}
	c.journalRead = c.root
	for _, g := range c.groups {
		if g == "systemd-journal" || g == "adm" || g == "wheel" || g == "root" {
			c.journalRead = true
		}
	}
	return c
}

// parseTZ turns "+0700" into a fixed zone.
func parseTZ(v string) *time.Location {
	if len(v) != 5 || (v[0] != '+' && v[0] != '-') {
		return time.UTC
	}
	h, err1 := strconv.Atoi(v[1:3])
	m, err2 := strconv.Atoi(v[3:5])
	if err1 != nil || err2 != nil {
		return time.UTC
	}
	off := h*3600 + m*60
	if v[0] == '-' {
		off = -off
	}
	return time.FixedZone(v, off)
}

func (s *LogsService) conn(connID string) (*sshx.Conn, error) {
	return s.core.Conn(connID)
}
