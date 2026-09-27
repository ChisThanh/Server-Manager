package security

import (
	"strings"
	"testing"
)

func TestParseUFWAccess(t *testing.T) {
	cases := []struct {
		line       string
		ok         bool
		addr, kind string
		ports      string
		managed    bool
	}{
		{`ufw deny from 192.0.2.86 comment 'sm: brute force'`, true, "192.0.2.86", "block", "", true},
		{`ufw reject from 192.0.2.0/24`, true, "192.0.2.0/24", "block", "", false},
		{`ufw allow from 10.0.0.0/8 to any port 22 proto tcp comment 'sm: office'`, true, "10.0.0.0/8", "allow", "22/tcp", true},
		{`ufw deny from 2001:db8::1`, true, "2001:db8::1", "block", "", false},
		{`ufw allow 22/tcp`, false, "", "", "", false},
		{`ufw allow OpenSSH`, false, "", "", "", false},
		{`ufw deny out to 1.2.3.4`, false, "", "", "", false},
		{`ufw limit from 1.2.3.4 to any port 22`, false, "", "", "", false},
	}
	for _, c := range cases {
		e, ok := parseUFWAccess(c.line)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v", c.line, ok)
		}
		if !ok {
			continue
		}
		if e.Addr != c.addr || e.Kind != c.kind || e.Ports != c.ports || e.Managed != c.managed || e.Ref != c.line {
			t.Errorf("%s: got %+v", c.line, e)
		}
	}
}

func TestUFWOpensSSH(t *testing.T) {
	ports := []int{22}
	for line, want := range map[string]bool{
		"ufw allow 22/tcp":                              true,
		"ufw limit 22/tcp":                              true,
		"ufw allow OpenSSH":                             true,
		"ufw allow 22":                                  true,
		"ufw allow to any port 22 proto tcp":            true,
		"ufw allow 20:30/tcp":                           true,
		"ufw allow 80/tcp":                              false,
		"ufw allow 22/udp":                              false,
		"ufw allow from 1.2.3.4 to any port 22":         false,
		"ufw deny 22/tcp":                               false,
		"ufw allow in on eth0 to any port 22 proto tcp": true,
	} {
		if got := ufwOpensSSH(line, ports); got != want {
			t.Errorf("%s: got %v", line, got)
		}
	}
	if ufwOpensSSH("ufw allow OpenSSH", []int{2222}) {
		t.Error("OpenSSH profile is port 22 only")
	}
}

func TestParseRichAndIpt(t *testing.T) {
	e, ok := parseRichAccess(`rule family="ipv4" source address="1.2.3.0/24" drop`, "public")
	if !ok || e.Addr != "1.2.3.0/24" || e.Kind != "block" || e.Ref != `rich:public:rule family="ipv4" source address="1.2.3.0/24" drop` {
		t.Fatalf("rich drop: %+v", e)
	}
	e, ok = parseRichAccess(`rule family="ipv4" source address="10.1.1.1" port port="22" protocol="tcp" accept`, "public")
	if !ok || e.Kind != "allow" || e.Ports != "22/tcp" {
		t.Fatalf("rich accept: %+v", e)
	}
	if _, ok := parseRichAccess(`rule family="ipv4" source NOT address="1.2.3.4" drop`, "public"); ok {
		t.Fatal("NOT rule must be ignored")
	}
	e, ok = parseIptAccess(`-A INPUT -s 192.0.2.86/32 -m comment --comment "sm-block: brute" -j DROP`, "4")
	if !ok || e.Addr != "192.0.2.86" || e.Kind != "block" || !e.Managed || e.Comment != "sm-block: brute" {
		t.Fatalf("ipt drop: %+v", e)
	}
	e, ok = parseIptAccess(`-A INPUT -s 10.0.0.0/8 -p tcp -m multiport --dports 22,443 -j ACCEPT`, "4")
	if !ok || e.Kind != "allow" || e.Ports != "22,443/tcp" {
		t.Fatalf("ipt accept: %+v", e)
	}
	for _, l := range []string{`-A INPUT -j ufw-before-input`, `-A INPUT ! -s 1.2.3.4/32 -j DROP`, `-A INPUT -s 1.2.3.4/32 -j f2b-sshd`, `-P INPUT ACCEPT`} {
		if _, ok := parseIptAccess(l, "4"); ok {
			t.Errorf("%s: should be ignored", l)
		}
	}
}

func TestAccessAddr(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.86":    "192.0.2.86",
		" 192.0.2.0/24": "192.0.2.0/24",
		"192.0.2.7/24":  "192.0.2.0/24",
		"2001:db8::1":      "2001:db8::1",
	} {
		if got, why := accessAddr(in); got != want || why != "" {
			t.Errorf("%q: %q %q", in, got, why)
		}
	}
	for in, why := range map[string]string{
		"any": "invalid", "": "invalid", "1.2.3": "invalid", "0.0.0.0/0": "local",
		"127.0.0.1": "local", "10.0.0.0/7": "tooBroad", "2001::/8": "tooBroad", "::1": "local",
	} {
		if _, got := accessAddr(in); got != why {
			t.Errorf("%q: reason %q, want %q", in, got, why)
		}
	}
	if !addrIn("192.0.2.0/24", "192.0.2.86") || !addrIn("10.0.0.0/8", "10.1.0.0/16") || addrIn("10.1.0.0/16", "10.0.0.0/8") || addrIn("1.2.3.4", "1.2.3.5") {
		t.Fatal("addrIn")
	}
}

func TestBatchResult(t *testing.T) {
	out := "@@ok 0\n@@fail 1\nERROR: bad rule\nmore\n@@ok 2\n"
	r := batchResult(out)
	if r[0] != "" || r[1] != "ERROR: bad rule; more" || r[2] != "" || len(r) != 3 {
		t.Fatalf("%#v", r)
	}
	s := batchScript([]string{"ufw deny from '1.2.3.4'", "", "true"})
	if !strings.Contains(s, "t 0 ufw deny from '1.2.3.4'\n") || strings.Contains(s, "t 1 ") || !strings.Contains(s, "t 2 true") {
		t.Fatal(s)
	}
}

func TestFail2banFileRoundTrip(t *testing.T) {
	c := Fail2banConfig{MaxRetry: 4, FindTime: 900, BanTime: 3600, Increment: true, MaxTime: 604800, Recidive: true, Aggressive: true, IgnoreIP: []string{"203.0.113.7", "10.0.0.0/8"}}
	f := fail2banFile(c, []int{22, 2222}, true)
	for _, want := range []string{"ignoreip = 127.0.0.1/8 ::1 203.0.113.7 10.0.0.0/8", "port = 22,2222", "backend = systemd", "mode = aggressive", "bantime.maxtime = 604800", "[recidive]\nenabled = true"} {
		if !strings.Contains(f, want) {
			t.Errorf("missing %q in\n%s", want, f)
		}
	}
	back := parseF2BFile(f)
	if back.MaxRetry != 4 || back.FindTime != 900 || back.BanTime != 3600 || !back.Increment || back.MaxTime != 604800 || !back.Recidive || !back.Aggressive || strings.Join(back.IgnoreIP, " ") != "203.0.113.7 10.0.0.0/8" {
		t.Fatalf("%+v", back)
	}
	bad := Fail2banConfig{MaxRetry: 0, FindTime: 600, BanTime: 3600}
	if validF2B(&bad) == nil {
		t.Fatal("maxretry 0 accepted")
	}
	ok := Fail2banConfig{MaxRetry: 5, FindTime: 600, BanTime: -1, IgnoreIP: []string{"1.2.3.4", "1.2.3.4", "127.0.0.1/8"}}
	if err := validF2B(&ok); err != nil || len(ok.IgnoreIP) != 1 {
		t.Fatalf("%v %v", err, ok.IgnoreIP)
	}
}

func TestParseAccessUFW(t *testing.T) {
	out := `@@ufw
installed
Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
@@ufwadded
Added user rules (see 'ufw status' for running firewall):
ufw allow from 203.0.113.7 comment 'sm: my office'
ufw deny from 192.0.2.0/24 comment 'sm: scanners'
ufw allow 22/tcp
@@ufwprepend
1
@@firewalld
@@iptables
installed
@@ipt4
-P INPUT DROP
@@ipt6
@@persist
@@ssh
22
@@f2b
Fail2Ban v1.0.2
running
@@jail:sshd
Status for the jail: sshd
|- Filter
|  |- Currently failed:	3
|  ` + "`" + `- Total failed:	120
` + "`" + `- Actions
   |- Currently banned:	2
   |- Total banned:	9
   ` + "`" + `- Banned IP list:	192.0.2.63 45.1.2.3
@@f2bget
bantime=600
findtime=600
maxretry=5
@@f2bignore
These IP addresses/networks are ignored:
|- 127.0.0.1/8
|- ::1
` + "`" + `- 203.0.113.7
@@f2bfile
@@pkg
apt-get
@@logs
journal
@@docker
@@end`
	st := parseAccess(out)
	if st.Backend != "ufw" || !st.Active || !st.DefaultDeny || !st.ufwPrepend || !st.CanLock {
		t.Fatalf("backend: %+v", st)
	}
	if len(st.Entries) != 4 {
		t.Fatalf("entries: %+v", st.Entries)
	}
	f := st.Fail2ban
	if !f.Running || f.SSHJail != "sshd" || f.Version != "1.0.2" || f.Managed || f.Config.BanTime != 600 || !f.JournalOnly || f.PkgManager != "apt" || strings.Join(f.Config.IgnoreIP, ",") != "203.0.113.7" {
		t.Fatalf("f2b: %+v", f)
	}
	st.SSHPorts = []int{22}
	if st.sshLocked() {
		t.Fatal("22/tcp open to all: not locked")
	}
	st.ufwAdded = st.ufwAdded[:2]
	if !st.sshLocked() {
		t.Fatal("no public SSH rule: locked")
	}
	st.ClientIP = "203.0.113.7"
	if !st.otherAllowCovers(AccessEntry{}, st.ClientIP) {
		t.Fatal("client is allowlisted")
	}
	if st.covered("192.0.2.9", "block", "ufw") != true || st.covered("192.0.3.9", "block", "") {
		t.Fatal("covered")
	}
}
