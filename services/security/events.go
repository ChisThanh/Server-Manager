package security

import (
	"context"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

type FailedByIP struct {
	IP      string   `json:"ip"`
	Count   int      `json:"count"`
	Users   []string `json:"users"`
	Invalid int      `json:"invalid"` // attempts with non-existent users
	First   int64    `json:"first"`
	Last    int64    `json:"last"`
}

type FailedByUser struct {
	User    string `json:"user"`
	Count   int    `json:"count"`
	IPs     int    `json:"ips"`
	Invalid bool   `json:"invalid"`
	First   int64  `json:"first"`
	Last    int64  `json:"last"`
}

type LoginEvent struct {
	Time   int64  `json:"time"`
	User   string `json:"user"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	// New: no earlier successful login from this IP (window + wtmp history).
	New bool `json:"new"`
}

type SudoEvent struct {
	Time    int64  `json:"time"`
	User    string `json:"user"`
	RunAs   string `json:"runAs"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	TTY     string `json:"tty"`
	Failed  bool   `json:"failed"`
	Reason  string `json:"reason"`
	// App: most likely run by Server Manager itself (sh -c by the login user).
	App bool `json:"app"`
}

type AccountEvent struct {
	Time    int64  `json:"time"`
	Tool    string `json:"tool"`
	Kind    string `json:"kind"` // user | group | password
	Message string `json:"message"`
}

type LastEntry struct {
	User   string `json:"user"`
	TTY    string `json:"tty"`
	Host   string `json:"host"`
	Time   int64  `json:"time"`
	Detail string `json:"detail"`
}

type Fail2banJail struct {
	Name            string   `json:"name"`
	CurrentlyFailed int      `json:"currentlyFailed"`
	TotalFailed     int      `json:"totalFailed"`
	CurrentlyBanned int      `json:"currentlyBanned"`
	TotalBanned     int      `json:"totalBanned"`
	Banned          []string `json:"banned"`
}

type Fail2banStatus struct {
	Installed bool           `json:"installed"`
	Running   bool           `json:"running"`
	Jails     []Fail2banJail `json:"jails"`
}

type EventsResult struct {
	Source       string         `json:"source"` // journal | file | none
	SourcePath   string         `json:"sourcePath"`
	Since        int64          `json:"since"`
	Now          int64          `json:"now"`
	FailedTotal  int            `json:"failedTotal"`
	FailedByIP   []FailedByIP   `json:"failedByIp"`
	FailedByUser []FailedByUser `json:"failedByUser"`
	Logins       []LoginEvent   `json:"logins"`
	Sudo         []SudoEvent    `json:"sudo"`
	Accounts     []AccountEvent `json:"accounts"`
	Last         []LastEntry    `json:"last"`
	Lastb        []LastEntry    `json:"lastb"`
	Fail2ban     Fail2banStatus `json:"fail2ban"`
	Limited      bool           `json:"limited"`
	Truncated    bool           `json:"truncated"`
	LoginUser    string         `json:"loginUser"`
}

const eventComms = "sshd sshd-session sudo useradd userdel usermod groupadd groupdel groupmod passwd chpasswd gpasswd adduser deluser addgroup delgroup"

const maxLogLines = 30000

func eventsScript(since int64) string {
	matches := ""
	for _, c := range strings.Fields(eventComms) {
		matches += " _COMM=" + c
	}
	idents := strings.ReplaceAll(eventComms, " ", "|")
	return `S=` + strconv.FormatInt(since, 10) + `
echo @@tz; date +%z
echo @@now; date +%s
if command -v journalctl >/dev/null 2>&1 && [ -n "$(journalctl -q -n 1 --no-pager _COMM=sshd _COMM=sshd-session 2>/dev/null)" ]; then
  echo @@journal
  journalctl -q --no-pager -o short-unix --since "@$S" -n ` + strconv.Itoa(maxLogLines) + matches + ` 2>/dev/null
else
  for f in /var/log/auth.log /var/log/secure /var/log/messages; do
    [ -f "$f" ] || continue
    echo @@file; echo "$f"
    echo @@log
    for g in "$f.1" "$f"; do [ -f "$g" ] && cat "$g"; done | grep -aE ' (` + idents + `)(\[[0-9]+\])?: ' | tail -n ` + strconv.Itoa(maxLogLines) + `
    break
  done
fi
echo @@last; last -F -w -i -n 2000 2>/dev/null || last -n 500 2>/dev/null
echo @@lastb; lastb -F -w -i -n 50 2>/dev/null || lastb -n 50 2>/dev/null
if command -v fail2ban-client >/dev/null 2>&1; then
  echo @@f2b
  if fail2ban-client ping >/dev/null 2>&1; then
    echo running
    for j in $(fail2ban-client status 2>/dev/null | sed -n 's/.*Jail list:[[:space:]]*//p' | tr ',' ' '); do echo "@@jail:$j"; fail2ban-client status "$j" 2>/dev/null; done
  else echo stopped; fi
fi
echo @@end`
}

// logLine is one parsed syslog/journal line.
type logLine struct {
	Time  int64
	Ident string
	PID   string
	Msg   string
}

var (
	reIdent    = regexp.MustCompile(`^([A-Za-z0-9_.-]+)(?:\[(\d+)\])?:$`)
	reUnixTime = regexp.MustCompile(`^\d{9,11}(\.\d+)? `)
	reBSDTime  = regexp.MustCompile(`^([A-Z][a-z]{2}) +(\d{1,2}) (\d\d:\d\d:\d\d) `)
)

// parseLogLine understands journalctl short-unix, RFC3339 and BSD syslog
// timestamps. tz is the server's UTC offset (for BSD times), now its clock.
func parseLogLine(l string, tz int, now time.Time) (logLine, bool) {
	var ts int64
	var rest string
	loc := time.FixedZone("srv", tz)
	switch {
	case reUnixTime.MatchString(l):
		sp := strings.IndexByte(l, ' ')
		if sp < 0 {
			return logLine{}, false
		}
		f, err := strconv.ParseFloat(l[:sp], 64)
		if err != nil {
			return logLine{}, false
		}
		ts, rest = int64(f), l[sp+1:]
	case len(l) > 20 && l[4] == '-' && l[10] == 'T':
		sp := strings.IndexByte(l, ' ')
		if sp < 0 {
			return logLine{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, l[:sp])
		if err != nil {
			return logLine{}, false
		}
		ts, rest = t.Unix(), l[sp+1:]
	default:
		m := reBSDTime.FindStringSubmatch(l)
		if m == nil {
			return logLine{}, false
		}
		t, err := time.ParseInLocation("Jan 2 15:04:05 2006", m[1]+" "+m[2]+" "+m[3]+" "+strconv.Itoa(now.In(loc).Year()), loc)
		if err != nil {
			return logLine{}, false
		}
		if t.After(now.Add(48 * time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		ts, rest = t.Unix(), l[len(m[0]):]
	}
	// rest: "host [facility.level] ident[pid]: message"
	f := strings.SplitN(rest, " ", 4)
	for i := 1; i < len(f) && i < 3; i++ {
		if m := reIdent.FindStringSubmatch(f[i]); m != nil {
			msg := strings.Join(f[i+1:], " ")
			return logLine{Time: ts, Ident: m[1], PID: m[2], Msg: msg}, true
		}
	}
	return logLine{}, false
}

var (
	reFailed    = regexp.MustCompile(`^Failed (\S+) for (invalid user )?(.*?) from (\S+) port \d+`)
	reInvalid   = regexp.MustCompile(`^Invalid user (.*?) from (\S+)(?: port \d+)?`)
	reAuthClose = regexp.MustCompile(`^(?:Connection closed|Disconnected) (?:by|from) authenticating user (\S+) (\S+) port \d+ \[preauth\]`)
	reAccepted  = regexp.MustCompile(`^Accepted (\S+) for (\S+) from (\S+) port \d+`)
	reRepeated  = regexp.MustCompile(`^message repeated (\d+) times: \[ ?(.*?) ?\]$`)
	reAppSudo   = regexp.MustCompile(`^(/usr)?/bin/(sh|true)( |$)`)
)

type failure struct {
	Time    int64
	User    string
	IP      string
	Invalid bool
	Count   int
}

// eventParser accumulates parsed log lines.
type eventParser struct {
	loginUser  string
	failures   []failure
	softFails  map[string]failure // pid → invalid-user/preauth close, counted if no Failed line
	failedPIDs map[string]bool
	logins     []LoginEvent
	sudo       []SudoEvent
	accounts   []AccountEvent
}

func newEventParser(login string) *eventParser {
	return &eventParser{loginUser: login, softFails: map[string]failure{}, failedPIDs: map[string]bool{}}
}

func (p *eventParser) add(l logLine) {
	msg := strings.TrimSpace(l.Msg)
	count := 1
	if m := reRepeated.FindStringSubmatch(msg); m != nil {
		count, msg = atoi(m[1]), m[2]
	}
	switch l.Ident {
	case "sshd", "sshd-session":
		if m := reFailed.FindStringSubmatch(msg); m != nil {
			p.failures = append(p.failures, failure{Time: l.Time, User: m[3], IP: m[4], Invalid: m[2] != "", Count: count})
			p.failedPIDs[l.PID] = true
			return
		}
		if m := reInvalid.FindStringSubmatch(msg); m != nil {
			key := l.PID
			if key == "" {
				key = strconv.FormatInt(l.Time, 10) + m[2]
			}
			p.softFails[key] = failure{Time: l.Time, User: m[1], IP: m[2], Invalid: true, Count: count}
			return
		}
		if m := reAuthClose.FindStringSubmatch(msg); m != nil && l.PID != "" {
			if _, ok := p.softFails[l.PID]; !ok {
				p.softFails[l.PID] = failure{Time: l.Time, User: m[1], IP: m[2], Count: 1}
			}
			return
		}
		if m := reAccepted.FindStringSubmatch(msg); m != nil {
			p.logins = append(p.logins, LoginEvent{Time: l.Time, Method: m[1], User: m[2], IP: m[3]})
		}
	case "sudo":
		if e, ok := parseSudo(msg); ok {
			e.Time = l.Time
			e.App = e.User == p.loginUser && reAppSudo.MatchString(e.Command)
			p.sudo = append(p.sudo, e)
		}
	default:
		kind := "user"
		switch l.Ident {
		case "groupadd", "groupdel", "groupmod", "gpasswd", "addgroup", "delgroup":
			kind = "group"
		case "passwd", "chpasswd":
			kind = "password"
			if !strings.Contains(msg, "password changed") && !strings.Contains(msg, "chauthtok") {
				return
			}
		}
		if strings.Contains(msg, ":session)") {
			return
		}
		p.accounts = append(p.accounts, AccountEvent{Time: l.Time, Tool: l.Ident, Kind: kind, Message: msg})
	}
}

// parseSudo parses "user : [reason ; ]TTY=x ; PWD=y ; USER=z ; COMMAND=cmd".
func parseSudo(msg string) (SudoEvent, bool) {
	user, rest, ok := strings.Cut(msg, " : ")
	if !ok || strings.Contains(user, " ") || strings.HasPrefix(msg, "pam_") {
		return SudoEvent{}, false
	}
	e := SudoEvent{User: strings.TrimSpace(user)}
	if i := strings.Index(rest, "COMMAND="); i >= 0 {
		e.Command = strings.TrimSpace(rest[i+len("COMMAND="):])
		rest = rest[:i]
	}
	for _, part := range strings.Split(rest, " ; ") {
		part = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), ";"))
		k, v, ok := strings.Cut(part, "=")
		switch {
		case ok && k == "TTY":
			e.TTY = v
		case ok && k == "PWD":
			e.Cwd = v
		case ok && k == "USER":
			e.RunAs = v
		case ok && (k == "ENV" || k == "TSID" || k == "GROUP"):
		case part != "" && !ok:
			e.Failed = true
			e.Reason = part
		}
	}
	if e.Command == "" && !e.Failed {
		return SudoEvent{}, false
	}
	return e, true
}

var reLastTime = regexp.MustCompile(`(Mon|Tue|Wed|Thu|Fri|Sat|Sun) (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) +(\d{1,2}) (\d\d:\d\d)(:\d\d)?(?: (\d{4}))?`)

// parseLast parses `last`/`lastb` output (with or without -F -i).
func parseLast(out string, tz int, now time.Time) []LastEntry {
	res := []LastEntry{}
	loc := time.FixedZone("srv", tz)
	for _, l := range lines(out) {
		f := strings.Fields(l)
		if len(f) < 3 || f[0] == "reboot" || f[0] == "shutdown" || strings.HasPrefix(l, "wtmp ") || strings.HasPrefix(l, "btmp ") {
			continue
		}
		loc0 := reLastTime.FindStringSubmatchIndex(l)
		if loc0 == nil {
			continue
		}
		m := reLastTime.FindStringSubmatch(l)
		year := m[6]
		if year == "" {
			year = strconv.Itoa(now.In(loc).Year())
		}
		secs := m[5]
		if secs == "" {
			secs = ":00"
		}
		t, err := time.ParseInLocation("Jan 2 15:04:05 2006", m[2]+" "+m[3]+" "+m[4]+secs+" "+year, loc)
		if err != nil {
			continue
		}
		if m[6] == "" && t.After(now.Add(48*time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		before := strings.Fields(l[:loc0[0]])
		e := LastEntry{User: before[0], Time: t.Unix()}
		if len(before) > 1 {
			e.TTY = before[1]
		}
		if len(before) > 2 {
			e.Host = before[2]
		}
		after := strings.TrimSpace(l[loc0[1]:])
		after = strings.TrimSpace(strings.TrimPrefix(after, "-"))
		if i := strings.LastIndex(after, "("); i >= 0 {
			e.Detail = strings.Trim(after[i:], "()")
		} else {
			e.Detail = after
		}
		if strings.Contains(after, "still logged in") {
			e.Detail = "still logged in"
		}
		res = append(res, e)
	}
	return res
}

func parseFail2ban(sec map[string]string) Fail2banStatus {
	st := Fail2banStatus{Jails: []Fail2banJail{}}
	v, ok := sec["f2b"]
	if !ok {
		return st
	}
	st.Installed = true
	st.Running = strings.TrimSpace(v) == "running"
	names := []string{}
	for k := range sec {
		if n, ok := strings.CutPrefix(k, "jail:"); ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		j := Fail2banJail{Name: n, Banned: []string{}}
		for _, l := range lines(sec["jail:"+n]) {
			l = strings.TrimLeft(l, "|`- \t")
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "Currently failed":
				j.CurrentlyFailed = atoi(v)
			case "Total failed":
				j.TotalFailed = atoi(v)
			case "Currently banned":
				j.CurrentlyBanned = atoi(v)
			case "Total banned":
				j.TotalBanned = atoi(v)
			case "Banned IP list":
				if f := strings.Fields(v); f != nil {
					j.Banned = f
				}
			}
		}
		st.Jails = append(st.Jails, j)
	}
	return st
}

// buildEvents turns the raw script output into the result.
func buildEvents(out string, since int64, login string) EventsResult {
	sec := sections(out)
	tz := parseTZ(strings.TrimSpace(sec["tz"]))
	nowUnix := int64(atoi(strings.TrimSpace(sec["now"])))
	if nowUnix == 0 {
		nowUnix = time.Now().Unix()
	}
	now := time.Unix(nowUnix, 0)
	r := EventsResult{Since: since, Now: nowUnix, Source: "none", LoginUser: login,
		FailedByIP: []FailedByIP{}, FailedByUser: []FailedByUser{}, Logins: []LoginEvent{}, Sudo: []SudoEvent{},
		Accounts: []AccountEvent{}, Last: []LastEntry{}, Lastb: []LastEntry{}}
	var raw string
	if v, ok := sec["journal"]; ok {
		r.Source, r.SourcePath, raw = "journal", "journalctl", v
	} else if v, ok := sec["log"]; ok {
		r.Source, r.SourcePath, raw = "file", strings.TrimSpace(sec["file"]), v
	}
	p := newEventParser(login)
	n := 0
	for _, l := range lines(raw) {
		n++
		ll, ok := parseLogLine(l, tz, now)
		if !ok || ll.Time < since {
			continue
		}
		p.add(ll)
	}
	r.Truncated = n >= maxLogLines
	for pid, f := range p.softFails {
		if !p.failedPIDs[pid] {
			p.failures = append(p.failures, f)
		}
	}
	byIP := map[string]*FailedByIP{}
	byUser := map[string]*FailedByUser{}
	userIPs := map[string]map[string]bool{}
	for _, f := range p.failures {
		r.FailedTotal += f.Count
		a := byIP[f.IP]
		if a == nil {
			a = &FailedByIP{IP: f.IP, Users: []string{}, First: f.Time}
			byIP[f.IP] = a
		}
		a.Count += f.Count
		if f.Invalid {
			a.Invalid += f.Count
		}
		if !slices.Contains(a.Users, f.User) && len(a.Users) < 20 {
			a.Users = append(a.Users, f.User)
		}
		a.First, a.Last = min(a.First, f.Time), max(a.Last, f.Time)
		u := byUser[f.User]
		if u == nil {
			u = &FailedByUser{User: f.User, First: f.Time}
			byUser[f.User] = u
			userIPs[f.User] = map[string]bool{}
		}
		u.Count += f.Count
		u.Invalid = u.Invalid || f.Invalid
		u.First, u.Last = min(u.First, f.Time), max(u.Last, f.Time)
		userIPs[f.User][f.IP] = true
	}
	for _, a := range byIP {
		r.FailedByIP = append(r.FailedByIP, *a)
	}
	for k, u := range byUser {
		u.IPs = len(userIPs[k])
		r.FailedByUser = append(r.FailedByUser, *u)
	}
	sort.Slice(r.FailedByIP, func(i, j int) bool { return r.FailedByIP[i].Count > r.FailedByIP[j].Count })
	sort.Slice(r.FailedByUser, func(i, j int) bool { return r.FailedByUser[i].Count > r.FailedByUser[j].Count })
	if len(r.FailedByIP) > 500 {
		r.FailedByIP = r.FailedByIP[:500]
	}
	if len(r.FailedByUser) > 500 {
		r.FailedByUser = r.FailedByUser[:500]
	}

	last := parseLast(sec["last"], tz, now)
	// Baseline of known IPs: wtmp history before the window.
	known := map[string]bool{}
	for _, e := range last {
		if e.Time < since {
			known[e.Host] = true
		}
	}
	sort.Slice(p.logins, func(i, j int) bool { return p.logins[i].Time < p.logins[j].Time })
	for i := range p.logins {
		ip := p.logins[i].IP
		if !known[ip] && !isLocalIP(ip) {
			p.logins[i].New = true
		}
		known[ip] = true
	}
	slices.Reverse(p.logins)
	r.Logins = capSlice(p.logins, 500)
	slices.Reverse(p.sudo)
	r.Sudo = capSlice(p.sudo, 1000)
	slices.Reverse(p.accounts)
	r.Accounts = capSlice(p.accounts, 500)
	r.Last = capSlice(last, 50)
	r.Lastb = capSlice(parseLast(sec["lastb"], tz, now), 50)
	r.Fail2ban = parseFail2ban(sec)
	return r
}

func capSlice[T any](s []T, n int) []T {
	if s == nil {
		return []T{}
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

func isLocalIP(s string) bool {
	ip := net.ParseIP(s)
	return ip == nil || ip.IsLoopback() || ip.IsUnspecified()
}

// Events collects security events of the last `hours` hours (1..720).
func (s *SecurityService) Events(connID string, hours int, sudoPassword string) (EventsResult, error) {
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return EventsResult{}, err
	}
	ctx, cancel := core.Timeout(90 * time.Second)
	defer cancel()
	return s.events(ctx, conn, hours, sudoPassword)
}

func (s *SecurityService) events(ctx context.Context, conn *sshx.Conn, hours int, pw string) (EventsResult, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	res, limited, err := s.rootOrUser(ctx, conn, eventsScript(since), pw, "")
	if err != nil {
		return EventsResult{}, err
	}
	r := buildEvents(res.Stdout, since, loginUser(conn))
	r.Limited = limited
	return r, nil
}

var reJail = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)

// Fail2banSetBan bans or unbans an IP in a fail2ban jail. Banning the
// address of this session is refused.
func (s *SecurityService) Fail2banSetBan(connID, jail, ip string, ban bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermSecurity); err != nil {
		return err
	}
	action := "sec.f2b.unban"
	if ban {
		action = "sec.f2b.ban"
	}
	err := func() error {
		if !reJail.MatchString(jail) {
			return apperr.New("sec.f2b.invalidJail", "jail", jail)
		}
		addr := net.ParseIP(strings.TrimSpace(ip))
		if addr == nil {
			return apperr.New("sec.fw.invalidAddr", "addr", ip)
		}
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(defaultTimeout)
		defer cancel()
		if ban {
			if client, _ := s.sessionClient(ctx, conn); client != "" && net.ParseIP(client).Equal(addr) {
				return apperr.New("sec.f2b.self")
			}
		}
		verb := "unbanip"
		if ban {
			verb = "banip"
		}
		res, err := s.root(ctx, conn, "fail2ban-client set "+core.Q(jail)+" "+verb+" "+core.Q(addr.String()), sudoPassword, "")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return apperr.New("sec.f2b.failed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
		}
		return nil
	}()
	s.core.Audit(connID, action, jail, ip, err)
	return err
}
