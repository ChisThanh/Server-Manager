package security

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseUFW(t *testing.T) {
	verbose := "Status: active\nLogging: on (low)\nDefault: deny (incoming), allow (outgoing), disabled (routed)\nNew profiles: skip\n"
	numbered := `Status: active

     To                         Action      From
     --                         ------      ----
[ 1] 22/tcp                     ALLOW IN    Anywhere                   # SSH (Server Manager)
[ 2] 8000:8100/tcp              DENY IN     10.0.0.0/8
[ 3] Anywhere                   REJECT OUT  192.0.2.1 (log)
[ 4] OpenSSH                    LIMIT IN    Anywhere
[10] 22/tcp (v6)                ALLOW IN    Anywhere (v6)
`
	st := parseUFWStatus(verbose, numbered, "")
	if !st.Active || st.Logging != "on (low)" || st.Defaults != (UFWPolicy{"deny", "allow", "disabled"}) || len(st.Rules) != 5 {
		t.Fatalf("%+v", st)
	}
	r := st.Rules
	if r[0].To != "22/tcp" || r[0].Comment != "SSH (Server Manager)" || r[0].From != "Anywhere" || r[0].Direction != "IN" {
		t.Fatalf("rule 1: %+v", r[0])
	}
	if r[1].Action != "DENY" || r[1].From != "10.0.0.0/8" || r[2].Direction != "OUT" || r[2].From != "192.0.2.1" || r[4].Num != 10 || !r[4].V6 {
		t.Fatalf("rules: %+v", r)
	}
	if !ufwCoversPort(r[0].To, 22, "tcp") || ufwCoversPort(r[0].To, 22, "udp") || !ufwCoversPort(r[1].To, 8050, "tcp") ||
		!ufwCoversPort(r[2].To, 1, "tcp") || !ufwCoversPort(r[3].To, 22, "tcp") || ufwCoversPort("80,443/tcp", 22, "tcp") {
		t.Fatal("ufwCoversPort")
	}
}

func TestUFWCommand(t *testing.T) {
	cmd, err := ufwCommand(NewUFWRule{Action: "allow", Port: "8000-8100", Proto: "tcp", From: "10.1.2.3/24", Comment: "web'; rm -rf / #"})
	if err != nil {
		t.Fatal(err)
	}
	want := `ufw allow in proto tcp from '10.1.2.0/24' to 'any' port '8000:8100' comment 'web rm -rf / #'`
	if cmd != want {
		t.Fatalf("\n got %s\nwant %s", cmd, want)
	}
	for _, bad := range []NewUFWRule{
		{Action: "allow"},                                // too broad
		{Action: "allow", Port: "0"},                     // port
		{Action: "allow", Port: "80,443"},                // list without proto
		{Action: "allow", Port: "80", From: "1.2.3"},     // address
		{Action: "limit", Port: "22", Direction: "out"},  // limit out
		{Action: "allow", Port: "80", Interface: "e;th"}, // iface
	} {
		if _, err := ufwCommand(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestParseSockets(t *testing.T) {
	ss := `tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=85,fd=3))
tcp LISTEN 0 511 [::]:80 [::]:* users:(("nginx",pid=83,fd=6),("nginx",pid=82,fd=6))
tcp LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=9,fd=14))
udp UNCONN 0 0 *:5353 *:*
tcp ESTAB 0 0 10.0.0.1:22 10.0.0.2:5000 users:(("sshd",pid=99,fd=4))
tcp LISTEN 0 4096 192.168.1.10:8080 0.0.0.0:*`
	l := parseSS(ss)
	if len(l) != 5 || l[0].Process != "sshd" || !l[0].Public || l[0].Scope != "any" || !slices.Equal(l[1].PIDs, []int{83, 82}) || !l[1].V6 ||
		l[2].Scope != "loopback" || l[2].Public || l[3].Proto != "udp" || !l[3].Public || l[4].Scope != "specific" || !l[4].Public {
		t.Fatalf("%+v", l)
	}
	ns := `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN      12/sshd
tcp        0      0 :::22                   :::*                    LISTEN      12/sshd
tcp        0      0 127.0.0.1:6379          0.0.0.0:*               LISTEN      -
udp        0      0 0.0.0.0:68              0.0.0.0:*                           7/udhcpc`
	n := parseNetstat(ns)
	if len(n) != 4 || n[0].Process != "sshd" || n[1].Address != "::" || !n[1].V6 || n[2].Public || n[2].Process != "" || n[3].Proto != "udp" || n[3].Process != "udhcpc" {
		t.Fatalf("%+v", n)
	}
}

func TestParseLogs(t *testing.T) {
	now := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		line  string
		ts    int64
		ident string
	}{
		{"1790306964.239768 host sshd[85]: Server listening", 1790306964, "sshd"},
		{"2026-09-25T03:29:24.240207+00:00 host sshd-session[85]: x", time.Date(2026, 9, 25, 3, 29, 24, 0, time.UTC).Unix(), "sshd-session"},
		{"Jan  2 10:00:00 host sudo: x", time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).Unix() - 3600, "sudo"},
		{"Dec 31 10:00:00 host authpriv.info useradd[3]: new user", time.Date(2025, 12, 31, 10, 0, 0, 0, time.UTC).Unix() - 3600, "useradd"},
	} {
		l, ok := parseLogLine(c.line, 3600, now)
		if !ok || l.Time != c.ts || l.Ident != c.ident {
			t.Fatalf("%q: %+v %v", c.line, l, ok)
		}
	}
	p := newEventParser("tester")
	add := func(pid, msg string) { p.add(logLine{Time: 100, Ident: "sshd", PID: pid, Msg: msg}) }
	add("1", "Invalid user admin from 1.2.3.4 port 5000")
	add("1", "Failed password for invalid user admin from 1.2.3.4 port 5000 ssh2")
	add("2", "Invalid user oracle from 1.2.3.4 port 5001")
	add("3", "message repeated 2 times: [ Failed password for root from 5.6.7.8 port 22 ssh2]")
	add("4", "Accepted publickey for tester from 9.9.9.9 port 1 ssh2: ED25519 SHA256:abc")
	p.add(logLine{Time: 101, Ident: "sudo", Msg: "  tester : TTY=pts/0 ; PWD=/home/tester ; USER=root ; COMMAND=/usr/bin/sh -c echo a ; b"})
	p.add(logLine{Time: 102, Ident: "sudo", Msg: "bob : 3 incorrect password attempts ; TTY=pts/1 ; PWD=/ ; USER=root ; COMMAND=/bin/ls"})
	p.add(logLine{Time: 103, Ident: "sudo", Msg: "pam_unix(sudo:session): session opened for user root"})
	p.add(logLine{Time: 104, Ident: "usermod", Msg: "add 'bob' to group 'sudo'"})
	p.add(logLine{Time: 105, Ident: "passwd", Msg: "pam_unix(passwd:chauthtok): password changed for bob"})
	if len(p.failures) != 2 || len(p.softFails) != 2 || len(p.logins) != 1 || p.logins[0].Method != "publickey" {
		t.Fatalf("ssh: %+v %+v", p.failures, p.logins)
	}
	if len(p.sudo) != 2 || !p.sudo[0].App || p.sudo[0].Command != "/usr/bin/sh -c echo a ; b" || p.sudo[0].RunAs != "root" ||
		!p.sudo[1].Failed || p.sudo[1].Reason != "3 incorrect password attempts" || p.sudo[1].App {
		t.Fatalf("sudo: %+v", p.sudo)
	}
	if len(p.accounts) != 2 || p.accounts[1].Kind != "password" {
		t.Fatalf("accounts: %+v", p.accounts)
	}
}

func TestParseLast(t *testing.T) {
	out := `tester   pts/0        172.17.0.1       Wed Sep 24 03:29:24 2026 - Wed Sep 24 04:00:00 2026  (00:30)
root     pts/1        0.0.0.0          Thu Sep 25 03:29:24 2026   still logged in
reboot   system boot  0.0.0.0          Fri Sep 25 03:29:23 2026   still running
admin    ssh:notty    1.2.3.4          Thu Sep 25 03:30:00 2026 - Thu Sep 25 03:30:00 2026  (00:00)

wtmp begins Fri Sep 25 03:29:23 2026`
	l := parseLast(out, 0, time.Now())
	if len(l) != 3 || l[0].User != "tester" || l[0].Host != "172.17.0.1" || l[0].Detail != "00:30" || l[1].Detail != "still logged in" || l[2].TTY != "ssh:notty" {
		t.Fatalf("%+v", l)
	}
	if l[0].Time != time.Date(2026, 9, 24, 3, 29, 24, 0, time.UTC).Unix() {
		t.Fatal("time")
	}
}

func TestSSHPlanning(t *testing.T) {
	main := "# comment\nInclude /etc/ssh/sshd_config.d/*.conf\nPort 22\nPasswordAuthentication yes\nMatch User x\n  PasswordAuthentication no\n"
	if m, d := detectMode(main, "/etc/ssh"); m != "dropin" || d != "/etc/ssh/sshd_config.d" {
		t.Fatal(m, d)
	}
	if m, _ := detectMode("PermitRootLogin no\nInclude /etc/ssh/sshd_config.d/*.conf\n", "/etc/ssh"); m != "main" {
		t.Fatal(m)
	}
	st := &sshdState{Main: "/etc/ssh/sshd_config", Mode: "dropin", DropinDir: "/etc/ssh/sshd_config.d", Managed: map[string][]string{},
		Files: []sshFile{{Path: "/etc/ssh/sshd_config", Content: main}, {Path: "/etc/ssh/sshd_config.d/50-cloud.conf", Content: "PasswordAuthentication yes\n"}}}
	managed, edits, err := planSSH(st, map[string][]string{"port": {"22", "2222", "22"}, "permitrootlogin": {"without-password"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(managed["Port"], []string{"22", "2222"}) || managed["PermitRootLogin"][0] != "prohibit-password" || len(edits) != 2 {
		t.Fatalf("%v %+v", managed, edits)
	}
	if !edits[0].Create || !strings.Contains(edits[0].Content, "Port 2222\n") || !strings.Contains(edits[1].Content, disabledMark+"Port 22\n") ||
		!strings.Contains(edits[1].Content, "  PasswordAuthentication no") {
		t.Fatalf("%+v", edits)
	}
	// Back: removing Port restores the commented line and deletes the drop-in when empty.
	st.Files[0].Content = edits[1].Content
	st.Files = append(st.Files, sshFile{Path: st.dropinPath(), Content: edits[0].Content})
	st.Managed = managed
	_, edits, err = planSSH(st, map[string][]string{"Port": {}, "PermitRootLogin": nil})
	if err != nil || len(edits) != 2 || !edits[0].Remove || edits[1].Content != main {
		t.Fatalf("%v %+v", err, edits)
	}
	for _, bad := range []map[string][]string{{"Port": {"0"}}, {"MaxAuthTries": {"x"}}, {"PermitRootLogin": {"maybe"}}, {"AllowUsers": {"a;b"}}, {"Banner": {"x"}}} {
		if _, _, err := planSSH(st, bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestNormalizeKey(t *testing.T) {
	k := "no-pty,from=\"10.0.0.1\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl  me@host\u0007 "
	line, fp, err := normalizeKey(k)
	if err != nil || !strings.HasPrefix(fp, "SHA256:") || line != `no-pty,from="10.0.0.1" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@host` {
		t.Fatalf("%q %s %v", line, fp, err)
	}
	for _, bad := range []string{"", "ssh-ed25519 AAAA", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl a\nssh-rsa AAAA"} {
		if _, _, err := normalizeKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	keys := parseAuthorizedKeys("# c\n\n" + line + "\ngarbage line\n")
	if len(keys) != 2 || keys[0].Line != 3 || keys[0].Bits != 256 || keys[0].Comment != "me@host" || len(keys[0].Options) != 2 || !keys[1].Invalid {
		t.Fatalf("%+v", keys)
	}
}

func TestParseUsers(t *testing.T) {
	out := `@@passwd
root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
tester:x:1000:1000:Tester,,,:/home/tester:/bin/bash
bob:x:1001:1001::/home/bob:/bin/sh
toor:x:0:0::/root:/bin/sh
@@group
root:x:0:
sudo:x:27:tester
tester:x:1000:
bob:x:1001:
docker:x:998:bob,tester
@@shells
/bin/sh
/bin/bash
/usr/sbin/nologin
@@uidmin
1000
@@tools
shadow
@@shadow
root:L:
tester:P:
bob:NP:
daemon:NS:
@@today
20000
@@tz
+0000
@@lastlog
tester           pts/0    172.17.0.1       Wed Sep 24 03:29:24 +0000 2026
bob                                        **Never logged in**
@@last
@@end
`
	r := parseUsers(out, "tester")
	tester, bob, daemon := findUser(r, "tester"), findUser(r, "bob"), findUser(r, "daemon")
	if r.Busybox || r.Limited || !slices.Equal(r.AdminGroups, []string{"sudo"}) || len(r.Users) != 5 {
		t.Fatalf("%+v", r)
	}
	if !tester.Admin || !tester.IsLogin || tester.PwStatus != "P" || tester.LastFrom != "172.17.0.1" || tester.LastLogin == 0 || !slices.Equal(tester.Groups, []string{"docker", "sudo"}) {
		t.Fatalf("tester %+v", tester)
	}
	if bob.Admin || bob.PwStatus != "NP" || bob.LastLogin != 0 || !daemon.System || daemon.CanLogin || findUser(r, "toor").System || !findUser(r, "root").Locked {
		t.Fatalf("bob %+v daemon %+v", bob, daemon)
	}
}

func TestShellFields(t *testing.T) {
	w, ok := shellFields(`ufw allow from 10.0.0.0/8 to any port 22 proto tcp comment 'a "b" c' x\ y "d\"e"`)
	if !ok || !slices.Equal(w, []string{"ufw", "allow", "from", "10.0.0.0/8", "to", "any", "port", "22", "proto", "tcp", "comment", `a "b" c`, "x y", `d"e`}) {
		t.Fatalf("%q", w)
	}
	if _, ok := shellFields(`ufw allow 'unterminated`); ok {
		t.Fatal("accepted unterminated quote")
	}
}
