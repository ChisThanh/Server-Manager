package logs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// QuerySpec selects log lines.
type QuerySpec struct {
	Kind          string `json:"kind"`   // journal | unit | kernel | auth | file | docker
	Target        string `json:"target"` // unit name, file path or container name/id
	Lines         int    `json:"lines"`  // tail N (1..10000); 0 = default (download: all)
	Since         int64  `json:"since"`  // unix seconds, 0 = unbounded
	Until         int64  `json:"until"`  // unix seconds, 0 = unbounded
	Level         string `json:"level"`  // "" or emerg|alert|crit|err|warning|notice|info|debug (this level and more severe)
	Search        string `json:"search"`
	Regex         bool   `json:"regex"`
	CaseSensitive bool   `json:"caseSensitive"`
}

// QueryResult is the outcome of Query, oldest line first.
type QueryResult struct {
	Lines []LogLine `json:"lines"`
	// LimitReached: as many lines as requested were returned; older ones may exist.
	LimitReached bool `json:"limitReached"`
	// Truncated: the output size cap was hit, some lines were dropped.
	Truncated bool `json:"truncated"`
	// Partial: a time-filtered file query only scanned the end of a large file.
	Partial bool  `json:"partial"`
	Sudo    bool  `json:"sudo"`
	TookMs  int64 `json:"tookMs"`
}

// DownloadResult describes a saved log file ("" path = cancelled).
type DownloadResult struct {
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

const (
	maxLines         = 10000
	defaultLines     = 500
	queryMaxBytes    = 64 << 20  // bytes read from the server per query
	downloadMaxBytes = 500 << 20 // bytes written per download
	fileScanBytes    = 32 << 20  // tail window scanned for time-filtered file queries
	followBacklogMax = 1000
)

var (
	reUnit      = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+$`)
	reContainer = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
)

// validUnit reports whether name is a safe systemd unit name.
func validUnit(name string) bool {
	return len(name) > 0 && len(name) <= 256 && name[0] != '-' && reUnit.MatchString(name)
}

func validPath(p string) bool {
	if p == "" || len(p) > 4096 || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
		return false
	}
	if path.Clean(p) != p || p == "/" {
		return false
	}
	for _, bad := range []string{"/proc/", "/sys/", "/dev/"} {
		if strings.HasPrefix(p+"/", bad) {
			return false
		}
	}
	return true
}

// normalize validates a spec and fills defaults.
func normalize(sp QuerySpec, download bool) (QuerySpec, error) {
	switch sp.Kind {
	case "journal", "kernel", "auth":
		sp.Target = ""
	case "unit":
		if !validUnit(sp.Target) {
			return sp, apperr.New("logs.invalidUnit", "name", sp.Target)
		}
	case "file":
		if !validPath(sp.Target) {
			return sp, apperr.New("logs.invalidPath", "path", sp.Target)
		}
	case "docker":
		if !reContainer.MatchString(sp.Target) {
			return sp, apperr.New("logs.invalidContainer", "name", sp.Target)
		}
	default:
		return sp, apperr.New("logs.invalidSource")
	}
	if sp.Lines < 0 || sp.Lines > maxLines {
		sp.Lines = maxLines
	}
	if sp.Lines == 0 && !download {
		sp.Lines = defaultLines
	}
	if sp.Level != "" && levelRank(sp.Level) < 0 {
		return sp, apperr.New("logs.invalidLevel")
	}
	if sp.Level == "debug" {
		sp.Level = "" // everything
	}
	if len(sp.Search) > 1000 || strings.ContainsAny(sp.Search, "\x00\n\r") {
		return sp, apperr.New("logs.invalidSearch")
	}
	if sp.Since < 0 || sp.Until < 0 || (sp.Since > 0 && sp.Until > 0 && sp.Since > sp.Until) {
		return sp, apperr.New("logs.invalidRange")
	}
	if sp.Search != "" && sp.Regex {
		if _, err := newMatcher(sp.Search, true, sp.CaseSensitive); err != nil {
			return sp, apperr.Wrap(err, "logs.invalidRegex")
		}
	}
	return sp, nil
}

// label names a source for audit records and file names.
func (sp QuerySpec) label() string {
	if sp.Target != "" {
		return sp.Kind + ":" + sp.Target
	}
	return sp.Kind
}

func isJournal(kind string) bool {
	return kind == "journal" || kind == "unit" || kind == "kernel" || kind == "auth"
}

// needSudo decides whether reading the source needs root, and checks that
// the source exists.
func (s *LogsService) needSudo(ctx context.Context, conn *sshx.Conn, c *caps, sp QuerySpec) (bool, error) {
	switch {
	case isJournal(sp.Kind):
		if !c.journalctl {
			return false, apperr.New("logs.noJournal")
		}
		return !c.journalRead, nil
	case sp.Kind == "file":
		if c.root {
			return false, nil
		}
		// r = readable, n = exists but unreadable, d = not a regular file,
		// x = missing, h = hidden behind an unreadable directory.
		script := `f=` + core.Q(sp.Target) + `
if [ -f "$f" ]; then if [ -r "$f" ]; then echo r; else echo n; fi
elif [ -e "$f" ]; then echo d
elif [ -x "$(dirname "$f")" ]; then echo x
else echo h; fi`
		res, err := s.core.Run(ctx, conn, script, false, "", "")
		if err != nil {
			return false, err
		}
		switch strings.TrimSpace(res.Stdout) {
		case "r":
			return false, nil
		case "d":
			return false, apperr.New("logs.notAFile", "path", sp.Target)
		case "x":
			return false, apperr.New("logs.fileNotFound", "path", sp.Target)
		}
		return true, nil
	case sp.Kind == "docker":
		if !c.docker {
			return false, apperr.New("logs.noDocker")
		}
		if c.root {
			return false, nil
		}
		res, err := s.core.Run(ctx, conn, "docker version --format '{{.Server.Version}}' 2>&1", false, "", "")
		if err != nil {
			return false, err
		}
		if res.ExitCode == 0 {
			return false, nil
		}
		if core.NeedsRoot(res) {
			return true, nil
		}
		return false, apperr.New("logs.dockerUnavailable").WithDetail(res.Stdout)
	}
	return false, apperr.New("logs.invalidSource")
}

type mode int

const (
	modeQuery mode = iota
	modeDownload
	modeFollow
)

// plan is a remote command and how to read its output.
type plan struct {
	cmd      string
	json     bool // journald JSON lines
	reverse  bool // output is newest first
	timeFilt bool // filter Since/Until in Go (text files)
	sizeHdr  bool // first line is "__SM_SIZE__ <bytes>"
	syslog   bool // parse "host ident[pid]: msg"
	source   string
	// input is the search pattern, fed on stdin (read into $SMQ) so it
	// shows neither in `ps` nor in sudo's log of the command line.
	input string
}

const readSMQ = `IFS= read -r SMQ || exit 2; `

const journalFields = "MESSAGE,PRIORITY,_SYSTEMD_UNIT,UNIT,SYSLOG_IDENTIFIER,_COMM,_PID"

// grepCmd greps for the search pattern held in $SMQ; it returns the
// pattern to put there.
func grepCmd(pattern string, regex, caseSensitive bool) (string, string) {
	args := []string{"grep", "-a"}
	if !caseSensitive {
		args = append(args, "-i")
	}
	if regex {
		args = append(args, "-E")
		pattern = toERE(pattern)
	} else {
		args = append(args, "-F")
	}
	args = append(args, "-e", `"$SMQ"`)
	return strings.Join(args, " "), pattern
}

// filters returns the grep stages for the search and level filters, and
// the value of $SMQ.
func filters(sp QuerySpec) ([]string, string) {
	var out []string
	input := ""
	if sp.Search != "" {
		g, pat := grepCmd(sp.Search, sp.Regex, sp.CaseSensitive)
		out = append(out, g)
		input = pat
	}
	if lp := levelPattern(sp.Level); lp != "" {
		out = append(out, "grep -a -i -E -e "+core.Q(lp))
	}
	return out, input
}

// build turns a spec into a remote command.
func build(sp QuerySpec, c *caps, m mode) plan {
	var p plan
	switch {
	case isJournal(sp.Kind):
		p = buildJournal(sp, c, m)
	case sp.Kind == "file":
		p = buildFile(sp, m)
	default:
		p = buildDocker(sp, m)
	}
	if p.input != "" {
		p.cmd = readSMQ + p.cmd
		p.input += "\n"
	}
	return p
}

func buildJournal(sp QuerySpec, c *caps, m mode) plan {
	p := plan{source: sp.Target}
	a := []string{"journalctl", "--no-pager", "-q"}
	if m == modeDownload {
		a = append(a, "-o", "short-iso")
	} else {
		p.json = true
		a = append(a, "-o", "json")
		if c.journalVer >= 236 {
			a = append(a, "--output-fields="+journalFields)
		}
	}
	grepServer := sp.Search != "" && c.pcre
	pipeGrep := sp.Search != "" && !c.pcre
	switch m {
	case modeQuery:
		p.reverse = true
		a = append(a, "-r")
		if sp.Search == "" {
			a = append(a, "-n", strconv.Itoa(sp.Lines))
		}
	case modeDownload:
		if sp.Lines > 0 && sp.Search == "" {
			a = append(a, "-n", strconv.Itoa(sp.Lines))
		}
	case modeFollow:
		a = append(a, "-f", "-n", strconv.Itoa(min(sp.Lines, followBacklogMax)))
	}
	if sp.Since > 0 && m != modeFollow {
		a = append(a, "--since", "@"+strconv.FormatInt(sp.Since, 10))
	}
	if sp.Until > 0 && m != modeFollow {
		a = append(a, "--until", "@"+strconv.FormatInt(sp.Until, 10))
	}
	if sp.Level != "" {
		a = append(a, "-p", sp.Level)
	}
	if grepServer {
		pat := sp.Search
		if !sp.Regex {
			pat = regexp.QuoteMeta(pat)
		}
		if c.journalVer >= 246 {
			a = append(a, "--case-sensitive="+strconv.FormatBool(sp.CaseSensitive))
		} else if !sp.CaseSensitive {
			pat = "(?i)" + pat
		}
		a = append(a, "-g", `"$SMQ"`)
		p.input = pat
	}
	switch sp.Kind {
	case "unit":
		a = append(a, "-u", core.Q(sp.Target))
	case "kernel":
		a = append(a, "_TRANSPORT=kernel")
	case "auth":
		a = append(a, "SYSLOG_FACILITY=4", "SYSLOG_FACILITY=10", "+",
			"SYSLOG_IDENTIFIER=sshd", "SYSLOG_IDENTIFIER=sudo", "SYSLOG_IDENTIFIER=su",
			"SYSLOG_IDENTIFIER=login", "SYSLOG_IDENTIFIER=systemd-logind", "SYSLOG_IDENTIFIER=polkitd")
	}
	cmd := strings.Join(a, " ")
	if pipeGrep && m != modeFollow {
		g, pat := grepCmd(sp.Search, sp.Regex, sp.CaseSensitive)
		cmd += " | " + g
		p.input = pat
	}
	if sp.Search != "" {
		switch {
		case m == modeQuery:
			cmd += " | head -n " + strconv.Itoa(sp.Lines)
		case m == modeDownload && sp.Lines > 0:
			cmd += " | tail -n " + strconv.Itoa(sp.Lines)
		}
	}
	p.cmd = cmd
	return p
}

func buildFile(sp QuerySpec, m mode) plan {
	p := plan{source: sp.Target, syslog: true}
	gz := strings.HasSuffix(sp.Target, ".gz")
	head := `f=` + core.Q(sp.Target) + `; [ -f "$f" ] || { echo __SM_NOFILE__ >&2; exit 3; }; `
	if m == modeFollow {
		p.cmd = head + "exec tail -n " + strconv.Itoa(min(sp.Lines, followBacklogMax)) + ` -F -- "$f"`
		return p
	}
	ranged := sp.Since > 0 || sp.Until > 0
	p.timeFilt = ranged
	stages, input := filters(sp)
	p.input = input
	var src string
	switch {
	case gz:
		src = `gzip -dc -- "$f"`
	case ranged && m == modeQuery:
		// Only the end of big files is scanned for time-filtered queries.
		p.sizeHdr = true
		head += `echo "__SM_SIZE__ $(wc -c < "$f")"; `
		src = "tail -c " + strconv.Itoa(fileScanBytes) + ` -- "$f"`
	case len(stages) == 0 && !ranged && sp.Lines > 0:
		p.cmd = head + "tail -n " + strconv.Itoa(sp.Lines) + ` -- "$f"`
		return p
	default:
		src = `cat -- "$f"`
	}
	parts := append([]string{src}, stages...)
	if sp.Lines > 0 && !ranged {
		parts = append(parts, "tail -n "+strconv.Itoa(sp.Lines))
	}
	p.cmd = head + strings.Join(parts, " | ")
	return p
}

func buildDocker(sp QuerySpec, m mode) plan {
	p := plan{source: sp.Target}
	head := `c=` + core.Q(sp.Target) + `; docker inspect --type container --format '{{.Id}}' "$c" >/dev/null 2>&1 || { echo __SM_NOCONTAINER__ >&2; exit 3; }; `
	a := []string{"docker", "logs", "--timestamps"}
	if m == modeFollow {
		a = append(a, "--follow", "--tail", strconv.Itoa(min(sp.Lines, followBacklogMax)))
		p.cmd = head + "exec " + strings.Join(a, " ") + ` "$c" 2>&1`
		return p
	}
	if sp.Since > 0 {
		a = append(a, "--since", strconv.FormatInt(sp.Since, 10))
	}
	if sp.Until > 0 {
		a = append(a, "--until", strconv.FormatInt(sp.Until, 10))
	}
	stages, input := filters(sp)
	p.input = input
	if len(stages) == 0 && sp.Lines > 0 {
		a = append(a, "--tail", strconv.Itoa(sp.Lines))
	}
	parts := append([]string{strings.Join(a, " ") + ` "$c" 2>&1`}, stages...)
	if len(stages) > 0 && sp.Lines > 0 {
		parts = append(parts, "tail -n "+strconv.Itoa(sp.Lines))
	}
	p.cmd = head + strings.Join(parts, " | ")
	return p
}

// toERE converts the Perl-style classes users commonly type (\d \s \w)
// into POSIX ERE for grep -E. Go re-checks every line afterwards, so the
// server-side grep only needs to return a superset.
func toERE(re string) string {
	var b strings.Builder
	inBr := false
	for i := 0; i < len(re); i++ {
		ch := re[i]
		if ch == '\\' && i+1 < len(re) {
			n := re[i+1]
			rep := ""
			switch n {
			case 'd':
				rep = map[bool]string{false: "[0-9]", true: "0-9"}[inBr]
			case 'D':
				if !inBr {
					rep = "[^0-9]"
				}
			case 's':
				rep = map[bool]string{false: "[[:space:]]", true: "[:space:]"}[inBr]
			case 'S':
				if !inBr {
					rep = "[^[:space:]]"
				}
			case 'w':
				rep = map[bool]string{false: "[[:alnum:]_]", true: "[:alnum:]_"}[inBr]
			case 'W':
				if !inBr {
					rep = "[^[:alnum:]_]"
				}
			}
			if rep != "" {
				b.WriteString(rep)
			} else {
				b.WriteByte(ch)
				b.WriteByte(n)
			}
			i++
			continue
		}
		switch {
		case ch == '[' && !inBr:
			inBr = true
		case ch == ']' && inBr:
			inBr = false
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// ---- running remote commands with streamed output ----

// prepare returns the full ssh command (through sudo when needed) and its
// stdin. extra, when non-nil, follows the sudo password on stdin.
func (s *LogsService) prepare(ctx context.Context, conn *sshx.Conn, cmd string, sudo bool, pw string, extra io.Reader) (string, io.Reader, error) {
	if !sudo || s.core.IsRoot(ctx, conn) {
		return "sh -c " + core.Q(cmd), extra, nil
	}
	probe, err := conn.Exec(ctx, "sudo -n true", nil)
	if err != nil {
		return "", nil, err
	}
	if probe.ExitCode == 0 {
		return "sudo -n -- sh -c " + core.Q(cmd), extra, nil
	}
	// Validates the password (coded sudo.* errors) and remembers it.
	if _, err := s.core.RunOK(ctx, conn, "true", true, pw, ""); err != nil {
		return "", nil, err
	}
	pwIn := strings.NewReader(s.core.SudoPassword(conn, pw) + "\n")
	if extra != nil {
		return "sudo -S -p '' -- sh -c " + core.Q(cmd), io.MultiReader(pwIn, extra), nil
	}
	return "sudo -S -p '' -- sh -c " + core.Q(cmd), pwIn, nil
}

// tailBuf keeps the last bytes written (stderr).
type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 16<<10 {
		t.b = t.b[len(t.b)-(16<<10):]
	}
	return len(p), nil
}

// lineWriter splits output into lines. fn returning false stops the command
// (via stop); so does exceeding max bytes.
type lineWriter struct {
	buf     []byte
	fn      func([]byte) bool
	max     int64
	n       int64
	stopped bool
	capped  bool
	stop    func()
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.stopped {
		return len(p), nil
	}
	w.n += int64(len(p))
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		if !w.fn(line) {
			w.halt(false)
			return len(p), nil
		}
	}
	if w.max > 0 && w.n > w.max {
		w.halt(true)
	}
	if len(w.buf) > 1<<20 { // absurdly long line: emit what we have
		if !w.fn(w.buf) {
			w.halt(false)
		}
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) halt(capped bool) {
	w.stopped = true
	w.capped = w.capped || capped
	w.buf = nil
	if w.stop != nil {
		w.stop()
	}
}

// flush emits a trailing line without newline.
func (w *lineWriter) flush() {
	if !w.stopped && len(w.buf) > 0 {
		w.fn(w.buf)
	}
	w.buf = nil
}

// run executes a prepared command, feeding stdout lines to fn. It returns
// whether output was cut short by the byte cap, and stderr.
func (s *LogsService) run(ctx context.Context, conn *sshx.Conn, p plan, sudo bool, pw string, maxBytes int64, fn func([]byte) bool) (capped bool, stderr string, err error) {
	var extra io.Reader
	if p.input != "" {
		extra = strings.NewReader(p.input)
	}
	full, stdin, err := s.prepare(ctx, conn, p.cmd, sudo, pw, extra)
	if err != nil {
		return false, "", err
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lw := &lineWriter{fn: fn, max: maxBytes, stop: cancel}
	var eb tailBuf
	code, err := conn.Stream(rctx, full, stdin, lw, &eb)
	lw.flush()
	stderr = strings.TrimSpace(string(eb.b))
	if lw.stopped && ctx.Err() == nil {
		// We stopped it ourselves: not an error.
		return lw.capped, stderr, nil
	}
	if err != nil {
		return lw.capped, stderr, err
	}
	if code != 0 && sudo {
		if strings.Contains(stderr, "incorrect password") || strings.Contains(stderr, "Sorry, try again") {
			return false, stderr, apperr.New("sudo.wrongPassword")
		}
	}
	if e := markerError(stderr); e != nil {
		return false, stderr, e
	}
	return lw.capped, stderr, nil
}

func markerError(stderr string) error {
	switch {
	case strings.Contains(stderr, "__SM_NOFILE__"):
		return apperr.New("logs.fileNotFound", "path", "")
	case strings.Contains(stderr, "__SM_NOCONTAINER__"):
		return apperr.New("logs.containerNotFound")
	}
	return nil
}

// ---- Query ----

// Query returns the log lines matching spec (oldest first).
func (s *LogsService) Query(connID string, spec QuerySpec, sudoPassword string) (QueryResult, error) {
	start := time.Now()
	sp, err := normalize(spec, false)
	if err != nil {
		return QueryResult{Lines: []LogLine{}}, err
	}
	conn, err := s.conn(connID)
	if err != nil {
		return QueryResult{Lines: []LogLine{}}, err
	}
	ctx, cancel := core.Timeout(90 * time.Second)
	defer cancel()
	res, err := s.query(ctx, conn, sp, sudoPassword)
	if err != nil {
		if e := apperr.From(err); e.Code == "logs.fileNotFound" {
			e.Params = map[string]string{"path": sp.Target}
		}
		return QueryResult{Lines: []LogLine{}}, err
	}
	s.auditSudoRead(connID, sp, res.Sudo)
	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
}

// auditSudoRead records root reads of files outside /var/log: the viewer
// can read any file with sudo, which deserves a trace.
func (s *LogsService) auditSudoRead(connID string, sp QuerySpec, sudo bool) {
	if sudo && sp.Kind == "file" && !strings.HasPrefix(sp.Target, "/var/log/") {
		s.core.Audit(connID, "logs.file.readSudo", sp.Target, "", nil)
	}
}

func (s *LogsService) query(ctx context.Context, conn *sshx.Conn, sp QuerySpec, pw string) (QueryResult, error) {
	out := QueryResult{Lines: []LogLine{}}
	c, err := s.getCaps(ctx, conn)
	if err != nil {
		return out, err
	}
	sudo, err := s.needSudo(ctx, conn, c, sp)
	if err != nil {
		return out, err
	}
	out.Sudo = sudo
	p := build(sp, c, modeQuery)
	m, _ := newMatcher(sp.Search, sp.Regex, sp.CaseSensitive)
	tp := &textParser{loc: c.loc, now: time.Now(), syslog: p.syslog}
	var since, until int64
	if p.timeFilt {
		since, until = sp.Since*1000, sp.Until*1000
		if until > 0 {
			until += 999
		}
	}
	// Ring buffer keeping the last N lines (first N when newest-first).
	ring := make([]LogLine, 0, min(sp.Lines, 1024))
	head := 0
	matched := 0
	var fileSize int64 = -1
	var firstTS int64
	first := true
	capped, stderr, err := s.run(ctx, conn, p, sudo, pw, queryMaxBytes, func(b []byte) bool {
		if first && p.sizeHdr {
			first = false
			if v, ok := strings.CutPrefix(string(b), "__SM_SIZE__ "); ok {
				fileSize, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				return true
			}
		}
		first = false
		if len(bytes.TrimSpace(b)) == 0 {
			return true
		}
		var l LogLine
		if p.json {
			var ok bool
			if l, ok = parseJournal(b, c.loc); !ok {
				return true
			}
			if !m.match(l.Message) {
				return true
			}
		} else {
			l = tp.parse(string(b))
			if firstTS == 0 {
				firstTS = l.TS
			}
			if !m.match(l.Raw) {
				return true
			}
			if p.timeFilt && (l.TS == 0 || (since > 0 && l.TS < since) || (until > 0 && l.TS > until)) {
				return true
			}
		}
		matched++
		if p.reverse {
			ring = append(ring, l)
			return len(ring) < sp.Lines
		}
		if len(ring) < sp.Lines {
			ring = append(ring, l)
		} else {
			ring[head] = l
			head = (head + 1) % sp.Lines
		}
		return true
	})
	if err != nil {
		return out, err
	}
	lines := append(ring[head:], ring[:head]...)
	if p.reverse {
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}
	if len(lines) == 0 && stderr != "" && !strings.HasPrefix(stderr, "tail:") {
		return out, apperr.New("logs.queryFailed").WithDetail(stderr)
	}
	out.Lines = lines
	out.Truncated = capped
	out.LimitReached = matched >= sp.Lines
	out.Partial = p.sizeHdr && fileSize > fileScanBytes && (since == 0 || firstTS == 0 || firstTS > since)
	return out, nil
}

// ---- Download ----

// Download saves the full output of a query (every matching line, up to
// 500 MB) to a local file chosen in a save dialog. Returns path "" when the
// user cancels.
func (s *LogsService) Download(connID string, spec QuerySpec, sudoPassword, title string) (DownloadResult, error) {
	sp, err := normalize(spec, true)
	if err != nil {
		return DownloadResult{}, err
	}
	conn, err := s.conn(connID)
	if err != nil {
		return DownloadResult{}, err
	}
	// Resolve sudo before the dialog so a password prompt doesn't follow it.
	ctx, cancel := core.Timeout(30 * time.Second)
	c, err := s.getCaps(ctx, conn)
	var sudo bool
	if err == nil {
		sudo, err = s.needSudo(ctx, conn, c, sp)
	}
	if err == nil && sudo {
		_, _, err = s.prepare(ctx, conn, "true", true, sudoPassword, nil)
	}
	cancel()
	if err != nil {
		return DownloadResult{}, err
	}
	name := fmt.Sprintf("%s-%s-%s.log", safeName(s.core.ServerName(connID)), safeName(sp.label()), time.Now().Format("20060102-150405"))
	dest, err := application.Get().Dialog.SaveFile().SetFilename(name).SetMessage(title).PromptForSingleSelection()
	if err != nil || dest == "" {
		return DownloadResult{}, err
	}
	r, err := s.downloadTo(conn, sp, sudoPassword, dest)
	detail := "→ " + dest
	if r.Truncated {
		detail += " (truncated)"
	}
	s.core.Audit(connID, "logs.download", sp.label(), detail, err)
	return r, err
}

var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = strings.Trim(reUnsafe.ReplaceAllString(s, "_"), "_.")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "logs"
	}
	return s
}

func (s *LogsService) downloadTo(conn *sshx.Conn, sp QuerySpec, pw, dest string) (DownloadResult, error) {
	out := DownloadResult{Path: dest}
	ctx, cancel := core.Timeout(30 * time.Minute)
	defer cancel()
	c, err := s.getCaps(ctx, conn)
	if err != nil {
		return out, err
	}
	sudo, err := s.needSudo(ctx, conn, c, sp)
	if err != nil {
		return out, err
	}
	p := build(sp, c, modeDownload)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return out, apperr.Wrap(err, "logs.writeFailed")
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	var werr error
	var since, until int64
	if p.timeFilt {
		since, until = sp.Since*1000, sp.Until*1000+999
		if sp.Until == 0 {
			until = 0
		}
	}
	tp := &textParser{loc: c.loc, now: time.Now()}
	capped, _, err := s.run(ctx, conn, p, sudo, pw, 0, func(b []byte) bool {
		if p.timeFilt {
			l := tp.parse(string(b))
			if l.TS == 0 || (since > 0 && l.TS < since) || (until > 0 && l.TS > until) {
				return true
			}
		}
		if out.Bytes+int64(len(b))+1 > downloadMaxBytes {
			out.Truncated = true
			return false
		}
		if _, werr = bw.Write(b); werr == nil {
			werr = bw.WriteByte('\n')
		}
		if werr != nil {
			return false
		}
		out.Bytes += int64(len(b)) + 1
		out.Lines++
		return true
	})
	if werr == nil {
		werr = bw.Flush()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if err == nil && werr != nil {
		err = apperr.Wrap(werr, "logs.writeFailed")
	}
	if err != nil {
		_ = os.Remove(dest)
		return DownloadResult{}, err
	}
	out.Truncated = out.Truncated || capped
	return out, nil
}
