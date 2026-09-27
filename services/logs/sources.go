package logs

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Source is a log source the server offers.
type Source struct {
	Kind        string `json:"kind"`   // journal | kernel | auth | unit | file | docker
	Target      string `json:"target"` // unit, path or container ("" for journal/kernel/auth)
	Label       string `json:"label"`
	NeedsSudo   bool   `json:"needsSudo"`
	Common      bool   `json:"common"`  // well-known file (syslog, auth.log, nginx…)
	Rotated     bool   `json:"rotated"` // rotated/compressed file (.1, .gz, -20240101)
	Size        int64  `json:"size"`
	MTime       int64  `json:"mtime"` // unix seconds
	State       string `json:"state"` // unit active state / container state
	Description string `json:"description"`
}

// SourceList is what Sources discovered.
type SourceList struct {
	Sources []Source `json:"sources"`
	Journal bool     `json:"journal"` // journald available
	Systemd bool     `json:"systemd"`
	Docker  bool     `json:"docker"`
	// JournalSudo: reading the system journal needs sudo.
	JournalSudo bool `json:"journalSudo"`
	// DockerError explains why containers could not be listed.
	DockerError string `json:"dockerError"`
}

var commonFiles = map[string]bool{
	"/var/log/syslog": true, "/var/log/messages": true, "/var/log/auth.log": true, "/var/log/secure": true,
	"/var/log/kern.log": true, "/var/log/daemon.log": true, "/var/log/cron.log": true, "/var/log/cron": true,
	"/var/log/mail.log": true, "/var/log/maillog": true, "/var/log/ufw.log": true, "/var/log/fail2ban.log": true,
	"/var/log/nginx/access.log": true, "/var/log/nginx/error.log": true,
	"/var/log/apache2/access.log": true, "/var/log/apache2/error.log": true,
	"/var/log/httpd/access_log": true, "/var/log/httpd/error_log": true,
	"/var/log/dpkg.log": true, "/var/log/apt/history.log": true, "/var/log/dnf.log": true, "/var/log/yum.log": true,
	"/var/log/mysql/error.log": true, "/var/log/redis/redis-server.log": true, "/var/log/caddy/access.log": true,
}

// Binary or noisy files that aren't text logs.
var skipFiles = map[string]bool{"wtmp": true, "btmp": true, "lastlog": true, "faillog": true, "utmp": true, "README": true}

const maxFileSources = 1000

// fileList prints "size|mtime|path" for the text files under /var/log.
const fileList = `smlist() { find /var/log -maxdepth 3 -type f ! -name '*.journal' ! -name '*.journal~' "$@" 2>/dev/null | head -n 3000; }
if find /var/log -maxdepth 0 -printf '' >/dev/null 2>&1; then smlist -printf '%s|%T@|%p\n'; else smlist -exec stat -c '%s|%Y|%n' {} +; fi |
while IFS='|' read -r s m f; do
  if [ -r "$f" ]; then r=r; else r=n; fi
  printf '%s|%s|%s|%s\n' "$r" "$s" "${m%%.*}" "$f"
done`

const sourcesScript = `
echo @@files
` + fileList + `
if [ -d /run/systemd/system ]; then
  echo @@units
  systemctl list-units --type=service --all --no-legend --no-pager --plain 2>/dev/null
fi
if command -v docker >/dev/null 2>&1; then
  echo @@docker
  docker ps -a --format '{{.Names}}|{{.State}}|{{.Image}}' 2>&1
  echo "@@dockerexit $?"
fi
echo @@end
`

// Sources discovers the logs available on the server. Files and containers
// hidden from the login user are listed through sudo when it works without
// prompting (NOPASSWD, cached or saved password); otherwise they're missing.
func (s *LogsService) Sources(connID string) (SourceList, error) {
	out := SourceList{Sources: []Source{}}
	conn, err := s.conn(connID)
	if err != nil {
		return out, err
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	c, err := s.getCaps(ctx, conn)
	if err != nil {
		return out, err
	}
	res, err := s.core.Run(ctx, conn, sourcesScript, false, "", "")
	if err != nil {
		return out, err
	}
	sec := sections(res.Stdout)
	out.Journal = c.journalctl
	out.Systemd = c.systemd
	out.Docker = c.docker
	out.JournalSudo = c.journalctl && !c.journalRead

	if c.journalctl {
		out.Sources = append(out.Sources,
			Source{Kind: "journal", Label: "journal", NeedsSudo: out.JournalSudo},
			Source{Kind: "kernel", Label: "kernel", NeedsSudo: out.JournalSudo},
			Source{Kind: "auth", Label: "auth", NeedsSudo: out.JournalSudo})
	}

	// Files: as the login user, then (silently) through sudo for the rest.
	files := map[string]Source{}
	parseFiles(sec["files"], files, false)
	if !c.root && s.silentSudo(ctx, conn) {
		if r, err := s.core.Run(ctx, conn, fileList, true, "", ""); err == nil {
			more := map[string]Source{}
			parseFiles(strings.Split(r.Stdout, "\n"), more, true)
			for p, f := range more {
				if _, ok := files[p]; !ok {
					files[p] = f
				}
			}
		}
	}
	fl := make([]Source, 0, len(files))
	for _, f := range files {
		fl = append(fl, f)
	}
	sort.Slice(fl, func(i, j int) bool {
		if fl[i].Common != fl[j].Common {
			return fl[i].Common
		}
		if fl[i].Rotated != fl[j].Rotated {
			return !fl[i].Rotated
		}
		return fl[i].Target < fl[j].Target
	})
	if len(fl) > maxFileSources {
		fl = fl[:maxFileSources]
	}
	out.Sources = append(out.Sources, fl...)

	for _, l := range sec["units"] {
		f := strings.Fields(strings.TrimLeft(l, "●* "))
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") || !validUnit(f[0]) {
			continue
		}
		out.Sources = append(out.Sources, Source{Kind: "unit", Target: f[0], Label: f[0], State: f[2],
			Description: strings.Join(f[4:], " "), NeedsSudo: out.JournalSudo})
	}

	if c.docker {
		dl := sec["docker"]
		exit := sec["dockerexit"]
		ok := len(exit) > 0 && strings.TrimSpace(exit[0]) == "0"
		sudo := false
		if !ok && !c.root && core.NeedsRoot(sshx.ExecResult{ExitCode: 1, Stdout: strings.Join(dl, "\n")}) && s.silentSudo(ctx, conn) {
			if r, err := s.core.Run(ctx, conn, "docker ps -a --format '{{.Names}}|{{.State}}|{{.Image}}'", true, "", ""); err == nil && r.ExitCode == 0 {
				dl, ok, sudo = strings.Split(r.Stdout, "\n"), true, true
			}
		}
		if ok {
			for _, l := range dl {
				p := strings.SplitN(strings.TrimSpace(l), "|", 3)
				if len(p) < 3 || !reContainer.MatchString(p[0]) {
					continue
				}
				out.Sources = append(out.Sources, Source{Kind: "docker", Target: p[0], Label: p[0], State: p[1], Description: p[2], NeedsSudo: sudo})
			}
		} else {
			out.DockerError = strings.TrimSpace(strings.Join(dl, "\n"))
			if len(out.DockerError) > 500 {
				out.DockerError = out.DockerError[:500]
			}
		}
	}
	return out, nil
}

// silentSudo reports whether sudo works without asking the user: NOPASSWD,
// or a password cached this session / saved with the server.
func (s *LogsService) silentSudo(ctx context.Context, conn *sshx.Conn) bool {
	_, err := s.core.RunOK(ctx, conn, "true", true, "", "")
	return err == nil
}

func sections(out string) map[string][]string {
	sec := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@@") {
			name, rest, _ := strings.Cut(strings.TrimPrefix(line, "@@"), " ")
			cur = name
			if rest != "" {
				sec[cur] = append(sec[cur], rest)
			} else if _, ok := sec[cur]; !ok {
				sec[cur] = []string{}
			}
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			sec[cur] = append(sec[cur], line)
		}
	}
	return sec
}

func isRotated(p string) bool {
	b := path.Base(p)
	if strings.HasSuffix(b, ".gz") || strings.HasSuffix(b, ".xz") || strings.HasSuffix(b, ".bz2") || strings.HasSuffix(b, ".zst") {
		return true
	}
	ext := path.Ext(b)
	if len(ext) > 1 {
		if _, err := strconv.Atoi(ext[1:]); err == nil {
			return true // syslog.1
		}
	}
	// secure-20240101, messages-20240101
	if i := strings.LastIndexByte(b, '-'); i > 0 && len(b)-i-1 == 8 {
		if _, err := strconv.Atoi(b[i+1:]); err == nil {
			return true
		}
	}
	return false
}

func parseFiles(lines []string, into map[string]Source, viaSudo bool) {
	for _, l := range lines {
		p := strings.SplitN(strings.TrimSpace(l), "|", 4)
		if len(p) < 4 || !validPath(p[3]) {
			continue
		}
		b := path.Base(p[3])
		if skipFiles[b] {
			continue
		}
		// Compressed formats other than gzip can't be read.
		if strings.HasSuffix(b, ".xz") || strings.HasSuffix(b, ".bz2") || strings.HasSuffix(b, ".zst") {
			continue
		}
		size, _ := strconv.ParseInt(p[1], 10, 64)
		mt, _ := strconv.ParseInt(p[2], 10, 64)
		into[p[3]] = Source{Kind: "file", Target: p[3], Label: p[3], Size: size, MTime: mt,
			NeedsSudo: viaSudo || p[0] != "r", Common: commonFiles[p[3]], Rotated: isRotated(p[3])}
	}
}
