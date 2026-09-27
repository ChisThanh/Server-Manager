package security

import (
	"context"
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

type LinuxUser struct {
	Name         string   `json:"name"`
	UID          int      `json:"uid"`
	GID          int      `json:"gid"`
	Gecos        string   `json:"gecos"`
	Home         string   `json:"home"`
	Shell        string   `json:"shell"`
	PrimaryGroup string   `json:"primaryGroup"`
	Groups       []string `json:"groups"` // supplementary groups
	Admin        bool     `json:"admin"`  // uid 0 or member of sudo/wheel/admin
	// PwStatus: P (usable password), L (locked), NP (empty password!),
	// NS (no password set: key-only), "" (unknown without root).
	PwStatus  string `json:"pwStatus"`
	Locked    bool   `json:"locked"`
	Expired   bool   `json:"expired"`
	System    bool   `json:"system"`
	CanLogin  bool   `json:"canLogin"` // shell is not nologin/false
	LastLogin int64  `json:"lastLogin"`
	LastFrom  string `json:"lastFrom"`
	IsLogin   bool   `json:"isLogin"`
}

type LinuxGroup struct {
	Name    string   `json:"name"`
	GID     int      `json:"gid"`
	Members []string `json:"members"`
	System  bool     `json:"system"`
}

type UsersResult struct {
	Users       []LinuxUser  `json:"users"`
	Groups      []LinuxGroup `json:"groups"`
	Shells      []string     `json:"shells"`
	AdminGroups []string     `json:"adminGroups"`
	LoginUser   string       `json:"loginUser"`
	Busybox     bool         `json:"busybox"`
	Limited     bool         `json:"limited"` // no root: password status unknown
	UIDMin      int          `json:"uidMin"`
}

type NewUser struct {
	Name       string `json:"name"`
	Gecos      string `json:"gecos"`
	Shell      string `json:"shell"`
	CreateHome bool   `json:"createHome"`
	Admin      bool   `json:"admin"`
	Password   string `json:"password"` // optional; fed on stdin to chpasswd
	SSHKey     string `json:"sshKey"`   // optional public key
}

var adminGroupNames = []string{"sudo", "wheel", "admin"}

// usersScript never prints password hashes: /etc/shadow is reduced to a
// status on the server.
const usersScript = `echo @@passwd; cat /etc/passwd
echo @@group; cat /etc/group
echo @@shells; cat /etc/shells 2>/dev/null; for s in /usr/sbin/nologin /sbin/nologin /bin/false /usr/bin/false; do [ -x "$s" ] && echo "$s"; done
echo @@uidmin; awk '$1=="UID_MIN"{print $2}' /etc/login.defs 2>/dev/null
echo @@tools; command -v useradd >/dev/null 2>&1 && echo shadow
echo @@shadow; awk -F: '{s=$2; st="P"; if (s=="") st="NP"; else if (s ~ /^!+$/ || s=="!*" || s ~ /^\*/) st="NS"; else if (substr(s,1,1)=="!") st="L"; print $1 ":" st ":" $8}' /etc/shadow 2>/dev/null
echo @@today; echo $(( $(date +%s) / 86400 ))
echo @@tz; date +%z
echo @@lastlog; lastlog 2>/dev/null | tail -n +2
echo @@last; last -F -w -i -n 3000 2>/dev/null || last -n 500 2>/dev/null
echo @@end`

var reLastlogTime = regexp.MustCompile(`(Mon|Tue|Wed|Thu|Fri|Sat|Sun) (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) +(\d{1,2}) (\d\d:\d\d:\d\d) ([+-]\d{4}) (\d{4})`)

func parseUsers(out, login string) UsersResult {
	sec := sections(out)
	r := UsersResult{Users: []LinuxUser{}, Groups: []LinuxGroup{}, Shells: []string{}, AdminGroups: []string{}, LoginUser: login, UIDMin: 1000}
	if n := atoi(sec["uidmin"]); n > 0 {
		r.UIDMin = n
	}
	r.Busybox = !strings.Contains(sec["tools"], "shadow")
	for _, s := range lines(sec["shells"]) {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "/") && !slices.Contains(r.Shells, s) {
			r.Shells = append(r.Shells, s)
		}
	}
	gidName := map[int]string{}
	members := map[string][]string{}
	for _, l := range lines(sec["group"]) {
		f := strings.Split(l, ":")
		if len(f) < 4 {
			continue
		}
		g := LinuxGroup{Name: f[0], GID: atoi(f[2]), Members: []string{}}
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				g.Members = append(g.Members, m)
				members[m] = append(members[m], g.Name)
			}
		}
		g.System = g.GID < r.UIDMin || g.GID == 65534
		gidName[g.GID] = g.Name
		r.Groups = append(r.Groups, g)
		if slices.Contains(adminGroupNames, g.Name) {
			r.AdminGroups = append(r.AdminGroups, g.Name)
		}
	}
	type shadowInfo struct {
		status  string
		expired bool
	}
	today := atoi(sec["today"])
	shadow := map[string]shadowInfo{}
	for _, l := range lines(sec["shadow"]) {
		f := strings.Split(l, ":")
		if len(f) < 3 {
			continue
		}
		exp := atoi(f[2])
		shadow[f[0]] = shadowInfo{status: f[1], expired: f[2] != "" && exp > 0 && today > 0 && exp <= today}
	}
	r.Limited = len(shadow) == 0
	tz := parseTZ(strings.TrimSpace(sec["tz"]))
	lastSeen := map[string]LastEntry{}
	for _, l := range lines(sec["lastlog"]) {
		if strings.Contains(l, "**Never logged in**") {
			continue
		}
		m := reLastlogTime.FindStringSubmatchIndex(l)
		if m == nil {
			continue
		}
		t, err := time.Parse("Mon Jan 2 15:04:05 -0700 2006", strings.Join(strings.Fields(l[m[0]:m[1]]), " "))
		if err != nil {
			continue
		}
		f := strings.Fields(l[:m[0]])
		if len(f) == 0 {
			continue
		}
		e := LastEntry{User: f[0], Time: t.Unix()}
		for _, tok := range f[1:] {
			if strings.Contains(tok, "/") || strings.HasPrefix(tok, "tty") || strings.HasPrefix(tok, "pts") {
				continue
			}
			e.Host = tok
		}
		lastSeen[e.User] = e
	}
	for _, e := range parseLast(sec["last"], tz, time.Now()) {
		if cur, ok := lastSeen[e.User]; !ok || e.Time > cur.Time {
			lastSeen[e.User] = e
		}
	}
	for _, l := range lines(sec["passwd"]) {
		f := strings.Split(l, ":")
		if len(f) < 7 {
			continue
		}
		u := LinuxUser{Name: f[0], UID: atoi(f[2]), GID: atoi(f[3]), Gecos: f[4], Home: f[5], Shell: f[6], Groups: []string{}}
		u.PrimaryGroup = gidName[u.GID]
		for _, g := range members[u.Name] {
			if g != u.PrimaryGroup && !slices.Contains(u.Groups, g) {
				u.Groups = append(u.Groups, g)
			}
		}
		sort.Strings(u.Groups)
		u.Admin = u.UID == 0
		for _, g := range append(slices.Clone(u.Groups), u.PrimaryGroup) {
			if slices.Contains(adminGroupNames, g) {
				u.Admin = true
			}
		}
		if si, ok := shadow[u.Name]; ok {
			u.PwStatus = si.status
			u.Locked = si.status == "L"
			u.Expired = si.expired
		}
		u.System = (u.UID != 0 && u.UID < r.UIDMin) || u.UID == 65534
		sh := u.Shell
		u.CanLogin = sh != "" && !strings.HasSuffix(sh, "/nologin") && !strings.HasSuffix(sh, "/false") && !strings.HasSuffix(sh, "/sync")
		if e, ok := lastSeen[u.Name]; ok {
			u.LastLogin, u.LastFrom = e.Time, e.Host
		}
		u.IsLogin = u.Name == login
		r.Users = append(r.Users, u)
	}
	return r
}

func (s *SecurityService) listUsers(ctx context.Context, conn *sshx.Conn, pw string) (UsersResult, error) {
	res, limited, err := s.rootOrUser(ctx, conn, usersScript, pw, "")
	if err != nil {
		return UsersResult{}, err
	}
	r := parseUsers(res.Stdout, loginUser(conn))
	r.Limited = r.Limited || limited
	return r, nil
}

// Users lists Linux accounts and groups.
func (s *SecurityService) Users(connID, sudoPassword string) (UsersResult, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return UsersResult{}, err
	}
	ctx, cancel := core.Timeout(defaultTimeout)
	defer cancel()
	return s.listUsers(ctx, conn, sudoPassword)
}

func findUser(r UsersResult, name string) *LinuxUser {
	for i := range r.Users {
		if r.Users[i].Name == name {
			return &r.Users[i]
		}
	}
	return nil
}

func findGroup(r UsersResult, name string) *LinuxGroup {
	for i := range r.Groups {
		if r.Groups[i].Name == name {
			return &r.Groups[i]
		}
	}
	return nil
}

func validPassword(p string) error {
	if p == "" || len(p) > 512 || strings.ContainsAny(p, "\r\n\x00") {
		return apperr.New("sec.users.invalidPassword")
	}
	return nil
}

// userOp runs a PermUsers mutation with audit.
func (s *SecurityService) userOp(connID, action, target string, fn func(ctx context.Context, conn *sshx.Conn) (string, error)) error {
	if err := s.core.Require(connID, core.PermUsers); err != nil {
		return err
	}
	detail := ""
	err := func() error {
		conn, err := s.core.Conn(connID)
		if err != nil {
			return err
		}
		ctx, cancel := core.Timeout(90 * time.Second)
		defer cancel()
		detail, err = fn(ctx, conn)
		return err
	}()
	s.core.Audit(connID, action, target, detail, err)
	return err
}

func cmdErr(code string, res sshx.ExecResult, params ...string) error {
	return apperr.New(code, params...).WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
}

// AddUser creates an account; on a failure after creation it is removed again.
func (s *SecurityService) AddUser(connID string, req NewUser, sudoPassword string) error {
	return s.userOp(connID, "sec.users.add", req.Name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		return s.addUser(ctx, conn, req, sudoPassword)
	})
}

func (s *SecurityService) addUser(ctx context.Context, conn *sshx.Conn, req NewUser, pw string) (string, error) {
	if !reUser.MatchString(req.Name) {
		return "", apperr.New("sec.invalidUser", "name", req.Name)
	}
	gecos := strings.ReplaceAll(sanitizeComment(req.Gecos, 64), ":", "")
	keyLine, keyFP := "", ""
	if strings.TrimSpace(req.SSHKey) != "" {
		var err error
		if keyLine, keyFP, err = normalizeKey(req.SSHKey); err != nil {
			return "", err
		}
	}
	if req.Password != "" {
		if err := validPassword(req.Password); err != nil {
			return "", err
		}
	}
	cur, err := s.listUsers(ctx, conn, pw)
	if err != nil {
		return "", err
	}
	if findUser(cur, req.Name) != nil {
		return "", apperr.New("sec.users.exists", "name", req.Name)
	}
	if findGroup(cur, req.Name) != nil && !cur.Busybox {
		// useradd would fail creating the user group.
		return "", apperr.New("sec.users.groupExists", "name", req.Name)
	}
	shell := req.Shell
	if shell == "" {
		shell = "/bin/sh"
		if slices.Contains(cur.Shells, "/bin/bash") {
			shell = "/bin/bash"
		}
	}
	if !slices.Contains(cur.Shells, shell) {
		return "", apperr.New("sec.users.invalidShell", "shell", shell)
	}
	adminGroup := ""
	if req.Admin {
		if len(cur.AdminGroups) == 0 {
			return "", apperr.New("sec.users.noAdminGroup")
		}
		adminGroup = cur.AdminGroups[0]
	}
	detail := "shell=" + shell + " home=" + strconv.FormatBool(req.CreateHome)
	var cmd string
	if cur.Busybox {
		cmd = "adduser -D -s " + core.Q(shell)
		if !req.CreateHome {
			cmd += " -H"
		}
		if gecos != "" {
			cmd += " -g " + core.Q(gecos)
		}
		cmd += " " + core.Q(req.Name)
	} else {
		cmd = "useradd -s " + core.Q(shell)
		if req.CreateHome {
			cmd += " -m"
		} else {
			cmd += " -M"
		}
		if gecos != "" {
			cmd += " -c " + core.Q(gecos)
		}
		cmd += " -- " + core.Q(req.Name)
	}
	res, err := s.root(ctx, conn, cmd, pw, "")
	if err != nil {
		return detail, err
	}
	if res.ExitCode != 0 {
		return detail, cmdErr("sec.users.addFailed", res)
	}
	rollback := func(e error) (string, error) {
		del := "userdel -r -- " + core.Q(req.Name)
		if cur.Busybox {
			del = "deluser --remove-home " + core.Q(req.Name)
		}
		_, _ = s.root(ctx, conn, del+" >/dev/null 2>&1", pw, "")
		return detail + " (rolled back)", e
	}
	if adminGroup != "" {
		detail += " admin=" + adminGroup
		g := "usermod -aG " + core.Q(adminGroup) + " -- " + core.Q(req.Name)
		if cur.Busybox {
			g = "addgroup " + core.Q(req.Name) + " " + core.Q(adminGroup)
		}
		if r, err := s.root(ctx, conn, g, pw, ""); err != nil || r.ExitCode != 0 {
			if err == nil {
				err = cmdErr("sec.users.groupFailed", r)
			}
			return rollback(err)
		}
	}
	if req.Password != "" {
		detail += " password=set"
		if err := s.chpasswd(ctx, conn, req.Name, req.Password, pw); err != nil {
			return rollback(err)
		}
	}
	if keyLine != "" {
		detail += " key=" + keyFP
		t, err := s.keyTarget(ctx, conn, req.Name)
		if err == nil {
			err = s.writeKeys(ctx, conn, t, keyLine+"\n", pw)
		}
		if err != nil {
			return rollback(err)
		}
	}
	return detail, nil
}

func (s *SecurityService) chpasswd(ctx context.Context, conn *sshx.Conn, name, password, pw string) error {
	res, err := s.root(ctx, conn, "chpasswd", pw, name+":"+password+"\n")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return cmdErr("sec.users.passwordFailed", res)
	}
	return nil
}

// checkTarget loads the users and validates an existing, changeable account.
func (s *SecurityService) checkTarget(ctx context.Context, conn *sshx.Conn, name, pw string) (UsersResult, *LinuxUser, error) {
	if err := existingUser(name); err != nil {
		return UsersResult{}, nil, err
	}
	cur, err := s.listUsers(ctx, conn, pw)
	if err != nil {
		return cur, nil, err
	}
	u := findUser(cur, name)
	if u == nil {
		return cur, nil, apperr.New("sec.users.notFound", "name", name)
	}
	return cur, u, nil
}

// DeleteUser removes an account (optionally its home). Refuses root, UID 0
// accounts, system accounts and the account the app is connected as.
func (s *SecurityService) DeleteUser(connID, name string, removeHome bool, sudoPassword string) error {
	return s.userOp(connID, "sec.users.delete", name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		detail := "removeHome=" + strconv.FormatBool(removeHome)
		cur, u, err := s.checkTarget(ctx, conn, name, sudoPassword)
		if err != nil {
			return detail, err
		}
		switch {
		case u.UID == 0:
			return detail, apperr.New("sec.users.refuseRoot")
		case u.IsLogin:
			return detail, apperr.New("sec.users.refuseSelf")
		case u.System:
			return detail, apperr.New("sec.users.refuseSystem", "name", name)
		}
		cmd := "userdel "
		if removeHome {
			cmd += "-r "
		}
		cmd += "-- " + core.Q(name)
		if cur.Busybox {
			cmd = "deluser "
			if removeHome {
				cmd += "--remove-home "
			}
			cmd += core.Q(name)
		}
		res, err := s.root(ctx, conn, cmd, sudoPassword, "")
		if err != nil {
			return detail, err
		}
		// userdel -r exits 12 when only the mail spool is missing.
		if res.ExitCode != 0 && !(res.ExitCode == 12 && strings.Contains(res.Stderr, "mail spool")) {
			return detail, cmdErr("sec.users.deleteFailed", res)
		}
		return detail, nil
	})
}

// SetLocked locks or unlocks an account's password (usermod -L/-U). Key
// logins keep working while locked.
func (s *SecurityService) SetLocked(connID, name string, locked bool, sudoPassword string) error {
	action := "sec.users.unlock"
	if locked {
		action = "sec.users.lock"
	}
	return s.userOp(connID, action, name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		cur, u, err := s.checkTarget(ctx, conn, name, sudoPassword)
		if err != nil {
			return "", err
		}
		if locked && u.IsLogin {
			return "", apperr.New("sec.users.refuseSelf")
		}
		flag := "-U"
		if locked {
			flag = "-L"
		}
		cmd := "usermod " + flag + " -- " + core.Q(name)
		if cur.Busybox {
			cmd = "passwd " + strings.ToLower(flag) + " " + core.Q(name)
		}
		res, err := s.root(ctx, conn, cmd, sudoPassword, "")
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", cmdErr("sec.users.lockFailed", res)
		}
		return "", nil
	})
}

// SetPassword sets an account's password (fed on stdin to chpasswd).
func (s *SecurityService) SetPassword(connID, name, password, sudoPassword string) error {
	return s.userOp(connID, "sec.users.password", name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		if err := validPassword(password); err != nil {
			return "", err
		}
		if _, _, err := s.checkTarget(ctx, conn, name, sudoPassword); err != nil {
			return "", err
		}
		return "", s.chpasswd(ctx, conn, name, password, sudoPassword)
	})
}

// SetGroups adds/removes supplementary groups. Removing the login user from
// its last admin group requires force.
func (s *SecurityService) SetGroups(connID, name string, add, remove []string, force bool, sudoPassword string) error {
	detail := "+" + strings.Join(add, ",") + " -" + strings.Join(remove, ",")
	return s.userOp(connID, "sec.users.groups", name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		cur, u, err := s.checkTarget(ctx, conn, name, sudoPassword)
		if err != nil {
			return detail, err
		}
		for _, g := range append(slices.Clone(add), remove...) {
			if !reGroup.MatchString(g) || findGroup(cur, g) == nil {
				return detail, apperr.New("sec.users.groupNotFound", "name", g)
			}
		}
		if u.IsLogin && !force && u.UID != 0 {
			admins := 0
			for _, g := range u.Groups {
				if slices.Contains(adminGroupNames, g) && !slices.Contains(remove, g) {
					admins++
				}
			}
			for _, g := range remove {
				if slices.Contains(adminGroupNames, g) && slices.Contains(u.Groups, g) && admins == 0 {
					return detail, apperr.New("sec.users.selfAdmin", "group", g)
				}
			}
		}
		cmds := []string{}
		for _, g := range add {
			if slices.Contains(u.Groups, g) {
				continue
			}
			if cur.Busybox {
				cmds = append(cmds, "addgroup "+core.Q(name)+" "+core.Q(g))
			} else {
				cmds = append(cmds, "usermod -aG "+core.Q(g)+" -- "+core.Q(name))
			}
		}
		for _, g := range remove {
			if !slices.Contains(u.Groups, g) {
				continue
			}
			if cur.Busybox {
				cmds = append(cmds, "delgroup "+core.Q(name)+" "+core.Q(g))
			} else {
				cmds = append(cmds, "gpasswd -d "+core.Q(name)+" "+core.Q(g)+" >/dev/null")
			}
		}
		if len(cmds) == 0 {
			return detail, nil
		}
		res, err := s.root(ctx, conn, "set -e; "+strings.Join(cmds, "; "), sudoPassword, "")
		if err != nil {
			return detail, err
		}
		if res.ExitCode != 0 {
			return detail, cmdErr("sec.users.groupFailed", res)
		}
		return detail, nil
	})
}

// AddGroup creates a group.
func (s *SecurityService) AddGroup(connID, name, sudoPassword string) error {
	return s.userOp(connID, "sec.groups.add", name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		if !reUser.MatchString(name) {
			return "", apperr.New("sec.invalidGroup", "name", name)
		}
		cur, err := s.listUsers(ctx, conn, sudoPassword)
		if err != nil {
			return "", err
		}
		if findGroup(cur, name) != nil {
			return "", apperr.New("sec.groups.exists", "name", name)
		}
		cmd := "groupadd -- " + core.Q(name)
		if cur.Busybox {
			cmd = "addgroup " + core.Q(name)
		}
		res, err := s.root(ctx, conn, cmd, sudoPassword, "")
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", cmdErr("sec.users.groupFailed", res)
		}
		return "", nil
	})
}

// DeleteGroup deletes a non-system group that is nobody's primary group.
func (s *SecurityService) DeleteGroup(connID, name, sudoPassword string) error {
	return s.userOp(connID, "sec.groups.delete", name, func(ctx context.Context, conn *sshx.Conn) (string, error) {
		if !reGroup.MatchString(name) {
			return "", apperr.New("sec.invalidGroup", "name", name)
		}
		cur, err := s.listUsers(ctx, conn, sudoPassword)
		if err != nil {
			return "", err
		}
		g := findGroup(cur, name)
		if g == nil {
			return "", apperr.New("sec.users.groupNotFound", "name", name)
		}
		if g.System || slices.Contains(adminGroupNames, name) {
			return "", apperr.New("sec.groups.refuseSystem", "name", name)
		}
		for _, u := range cur.Users {
			if u.GID == g.GID {
				return "", apperr.New("sec.groups.primary", "name", name, "user", u.Name)
			}
		}
		cmd := "groupdel -- " + core.Q(name)
		if cur.Busybox {
			cmd = "delgroup " + core.Q(name)
		}
		res, err := s.root(ctx, conn, cmd, sudoPassword, "")
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", cmdErr("sec.users.groupFailed", res)
		}
		return "", nil
	})
}
