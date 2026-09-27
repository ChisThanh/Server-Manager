package logs

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// LogLine is one structured log entry.
type LogLine struct {
	TS      int64  `json:"ts"`      // unix ms, 0 when unknown
	Level   string `json:"level"`   // emerg|alert|crit|err|warning|notice|info|debug, "" when unknown
	Source  string `json:"source"`  // unit / syslog identifier / container / file
	Message string `json:"message"` // the text without timestamp/prefix
	Raw     string `json:"raw"`     // the full line as a human would read it
}

// Syslog priorities, most severe first.
var levels = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

func levelRank(l string) int {
	for i, v := range levels {
		if v == l {
			return i
		}
	}
	return -1
}

// normLevel maps the many spellings of a level onto the syslog names.
func normLevel(s string) string {
	switch strings.ToLower(s) {
	case "emerg", "emergency", "panic":
		return "emerg"
	case "alert":
		return "alert"
	case "crit", "critical", "fatal", "severe":
		return "crit"
	case "err", "error", "errors", "eror":
		return "err"
	case "warn", "warning":
		return "warning"
	case "notice":
		return "notice"
	case "info", "information", "informational":
		return "info"
	case "debug", "trace", "dbug", "verbose":
		return "debug"
	}
	return ""
}

const maxLineBytes = 16 << 10

// clip bounds a line's size and makes it valid UTF-8.
func clip(s string) string {
	if len(s) > maxLineBytes {
		s = strings.ToValidUTF8(s[:maxLineBytes], "") + " …"
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return s
}

// ---- level detection in free text ----

var (
	reLvlBracket = regexp.MustCompile(`(?i)[\[<(](?:[a-z0-9_.-]+:)?(emerg|emergency|alert|crit|critical|fatal|panic|err|error|warn|warning|notice|info|debug|trace)[\]>)]`)
	reLvlKV      = regexp.MustCompile(`(?i)\b(?:level|lvl|severity|loglevel)"?\s*[=:]\s*"?([a-z]+)`)
	reLvlUpper   = regexp.MustCompile(`\b(EMERG|EMERGENCY|ALERT|CRIT|CRITICAL|FATAL|PANIC|ERR|ERROR|SEVERE|WARN|WARNING|NOTICE|INFO|DEBUG|TRACE)\b`)
	reLvlLower   = regexp.MustCompile(`(?i)\b(fatal|panic|critical|error|failed|failure|warning)\b`)
)

// detectLevel guesses the level of a text log line.
func detectLevel(msg string) string {
	if len(msg) > 400 {
		msg = msg[:400]
	}
	if m := reLvlBracket.FindStringSubmatch(msg); m != nil {
		return normLevel(m[1])
	}
	if m := reLvlKV.FindStringSubmatch(msg); m != nil {
		if l := normLevel(m[1]); l != "" {
			return l
		}
	}
	if m := reLvlUpper.FindStringSubmatch(msg); m != nil {
		return normLevel(m[1])
	}
	if m := reLvlLower.FindStringSubmatch(msg); m != nil {
		switch strings.ToLower(m[1]) {
		case "fatal", "panic", "critical":
			return "crit"
		case "warning":
			return "warning"
		default:
			return "err"
		}
	}
	return ""
}

// levelWords are the words matched (case-insensitively) by the level filter
// for text logs, cumulative from the most severe.
var levelWords = [][]string{
	{"EMERG", "EMERGENCY", "PANIC"},
	{"ALERT"},
	{"CRIT", "CRITICAL", "FATAL", "SEVERE"},
	{"ERR", "ERROR", "ERRORS"},
	{"WARN", "WARNING"},
	{"NOTICE"},
	{"INFO"},
}

// levelPattern is the POSIX ERE used with `grep -i -E` to keep lines at
// least as severe as level ("" when no filtering is needed).
func levelPattern(level string) string {
	r := levelRank(level)
	if r < 0 || r >= len(levelWords) {
		return ""
	}
	var words []string
	for i := 0; i <= r; i++ {
		words = append(words, levelWords[i]...)
	}
	return `(^|[^[:alnum:]_])(` + strings.Join(words, "|") + `)([^[:alnum:]_]|$)`
}

// ---- timestamps in text logs ----

var (
	reISO     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2}):(\d{2})(?:[.,](\d{1,9}))?(Z|[+-]\d{2}:?\d{2})?\s?`)
	reSlash   = regexp.MustCompile(`^(\d{4})/(\d{2})/(\d{2}) (\d{2}):(\d{2}):(\d{2})\s?`)
	reSyslog  = regexp.MustCompile(`^([A-Z][a-z]{2}) {1,2}(\d{1,2}) (\d{2}):(\d{2}):(\d{2})\s?`)
	reApache  = regexp.MustCompile(`^\[[A-Z][a-z]{2} ([A-Z][a-z]{2}) (\d{2}) (\d{2}):(\d{2}):(\d{2})(?:\.(\d+))? (\d{4})\]\s?`)
	reAccess  = regexp.MustCompile(`\[(\d{2})/([A-Z][a-z]{2})/(\d{4}):(\d{2}):(\d{2}):(\d{2}) ([+-]\d{4})\]`)
	reSysTail = regexp.MustCompile(`^(\S+) ([^\s:\[]+)(?:\[(\d+)\])?: `)
)

var months = map[string]time.Month{"Jan": 1, "Feb": 2, "Mar": 3, "Apr": 4, "May": 5, "Jun": 6, "Jul": 7, "Aug": 8, "Sep": 9, "Oct": 10, "Nov": 11, "Dec": 12}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func nanos(frac string) int {
	if frac == "" {
		return 0
	}
	for len(frac) < 9 {
		frac += "0"
	}
	return atoi(frac[:9])
}

func zoneOf(z string, loc *time.Location) *time.Location {
	switch {
	case z == "":
		return loc
	case z == "Z":
		return time.UTC
	}
	return parseTZ(strings.Replace(z, ":", "", 1))
}

// Timestamp styles recognised by parseTS.
const (
	tsNone = iota
	tsISO
	tsSlash
	tsSyslog
	tsApache
	tsAccess
)

// parseTS finds the timestamp of a text log line. It returns unix ms (0 if
// none), the rest of the line after a leading timestamp, and the style.
func parseTS(line string, loc *time.Location, now time.Time) (int64, string, int) {
	if m := reISO.FindStringSubmatch(line); m != nil {
		t := time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]), nanos(m[7]), zoneOf(m[8], loc))
		return t.UnixMilli(), line[len(m[0]):], tsISO
	}
	if m := reSlash.FindStringSubmatch(line); m != nil {
		t := time.Date(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]), atoi(m[4]), atoi(m[5]), atoi(m[6]), 0, loc)
		return t.UnixMilli(), line[len(m[0]):], tsSlash
	}
	if m := reSyslog.FindStringSubmatch(line); m != nil {
		mon, ok := months[m[1]]
		if ok {
			nl := now.In(loc)
			t := time.Date(nl.Year(), mon, atoi(m[2]), atoi(m[3]), atoi(m[4]), atoi(m[5]), 0, loc)
			// Syslog has no year: a date in the future belongs to last year.
			if t.After(nl.Add(48 * time.Hour)) {
				t = t.AddDate(-1, 0, 0)
			}
			return t.UnixMilli(), line[len(m[0]):], tsSyslog
		}
	}
	if m := reApache.FindStringSubmatch(line); m != nil {
		if mon, ok := months[m[1]]; ok {
			t := time.Date(atoi(m[7]), mon, atoi(m[2]), atoi(m[3]), atoi(m[4]), atoi(m[5]), nanos(m[6]), loc)
			return t.UnixMilli(), line[len(m[0]):], tsApache
		}
	}
	head := line
	if len(head) > 300 {
		head = head[:300]
	}
	if m := reAccess.FindStringSubmatch(head); m != nil {
		if mon, ok := months[m[2]]; ok {
			t := time.Date(atoi(m[3]), mon, atoi(m[1]), atoi(m[4]), atoi(m[5]), atoi(m[6]), 0, parseTZ(m[7]))
			return t.UnixMilli(), line, tsAccess
		}
	}
	return 0, line, tsNone
}

// textParser turns lines of a text log (file or docker) into LogLines.
// Lines without a timestamp (stack traces…) inherit the previous one.
type textParser struct {
	loc    *time.Location
	now    time.Time
	source string
	syslog bool // try "host ident[pid]: msg" after the timestamp
	lastTS int64
}

func (p *textParser) parse(raw string) LogLine {
	raw = clip(strings.TrimRight(raw, "\r"))
	ts, rest, style := parseTS(raw, p.loc, p.now)
	l := LogLine{Raw: raw, Source: p.source, Message: rest}
	if ts == 0 {
		ts = p.lastTS
	} else {
		p.lastTS = ts
	}
	l.TS = ts
	if p.syslog && (style == tsISO || style == tsSyslog) {
		if m := reSysTail.FindStringSubmatch(rest); m != nil && normLevel(m[1]) == "" && normLevel(m[2]) == "" {
			l.Source = m[2]
			l.Message = rest[len(m[0]):]
		}
	}
	l.Level = detectLevel(l.Message)
	return l
}

// ---- journald JSON ----

type journalEntry map[string]json.RawMessage

func (e journalEntry) str(k string) string {
	v, ok := e[k]
	if !ok || len(v) == 0 || string(v) == "null" {
		return ""
	}
	if v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	if v[0] == '[' {
		// Binary-safe fields are exported as arrays of bytes; fields with
		// several values as arrays of strings.
		var b []byte
		var nums []int
		if json.Unmarshal(v, &nums) == nil {
			b = make([]byte, 0, len(nums))
			for _, n := range nums {
				b = append(b, byte(n))
			}
			return string(b)
		}
		var strs []string
		if json.Unmarshal(v, &strs) == nil && len(strs) > 0 {
			return strs[len(strs)-1]
		}
	}
	return strings.Trim(string(v), `"`)
}

// parseJournal parses one `journalctl -o json` line.
func parseJournal(line []byte, loc *time.Location) (LogLine, bool) {
	var e journalEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return LogLine{}, false
	}
	l := LogLine{}
	if us, err := strconv.ParseInt(e.str("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		l.TS = us / 1000
	}
	if p, err := strconv.Atoi(e.str("PRIORITY")); err == nil && p >= 0 && p < len(levels) {
		l.Level = levels[p]
	}
	msg := strings.TrimRight(e.str("MESSAGE"), "\n")
	l.Message = clip(msg)
	ident := e.str("SYSLOG_IDENTIFIER")
	if ident == "" {
		ident = e.str("_COMM")
	}
	unit := e.str("_SYSTEMD_UNIT")
	if unit == "" {
		unit = e.str("UNIT") // messages systemd writes about a unit
	}
	// Like journalctl's own output, name the program; fall back to the unit.
	l.Source = ident
	if l.Source == "" {
		l.Source = unit
	}
	var b strings.Builder
	if l.TS > 0 {
		b.WriteString(time.UnixMilli(l.TS).In(loc).Format("2006-01-02T15:04:05-0700"))
		b.WriteByte(' ')
	}
	if ident != "" {
		b.WriteString(ident)
		if pid := e.str("_PID"); pid != "" {
			b.WriteString("[" + pid + "]")
		}
		b.WriteString(": ")
	}
	b.WriteString(msg)
	l.Raw = clip(b.String())
	return l, true
}

// ---- search ----

// matcher applies the text search in Go (after the server-side grep), so
// results are exact whatever the server's grep/journalctl support.
type matcher struct {
	re    *regexp.Regexp
	fixed string
	fold  bool
}

func newMatcher(q string, isRegex, caseSensitive bool) (*matcher, error) {
	if q == "" {
		return nil, nil
	}
	if isRegex {
		expr := q
		if !caseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		return &matcher{re: re}, nil
	}
	m := &matcher{fixed: q, fold: !caseSensitive}
	if m.fold {
		m.fixed = strings.ToLower(q)
	}
	return m, nil
}

func (m *matcher) match(s string) bool {
	if m == nil {
		return true
	}
	if m.re != nil {
		return m.re.MatchString(s)
	}
	if m.fold {
		return strings.Contains(strings.ToLower(s), m.fixed)
	}
	return strings.Contains(s, m.fixed)
}
