//go:build integration

package security

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"server-manager/internal/testutil"
)

func findEntry(st AccessState, addr, kind, source string) (AccessEntry, bool) {
	for _, e := range st.Entries {
		if e.Addr == addr && e.Kind == kind && (source == "" || e.Source == source) {
			return e, true
		}
	}
	return AccessEntry{}, false
}

func skipReason(r AccessResult, addr string) string {
	for _, s := range r.Skipped {
		if s.Addr == addr {
			return s.Reason
		}
	}
	return ""
}

func TestAccessUFW(t *testing.T) {
	s, _, id := setup(t)
	bak := "/tmp/smtest-sec-acc-bak"
	wasActive := strings.Contains(sh(t, s, id, "ufw status"), "Status: active")
	sh(t, s, id, "rm -rf "+bak+" && cp -a /etc/ufw "+bak)
	t.Cleanup(func() {
		cmd := "ufw --force disable >/dev/null; rm -rf /etc/ufw && cp -a " + bak + " /etc/ufw && rm -rf " + bak
		if wasActive {
			cmd += " && ufw allow 22/tcp >/dev/null && ufw --force enable >/dev/null"
		}
		sh(t, s, id, cmd)
	})
	_, err := s.UFWEnable(id, "")
	must(t, err)

	st, err := s.AccessList(id, "")
	must(t, err)
	if st.Backend != "ufw" || !st.Active || !st.DefaultDeny || st.ClientIP == "" || st.SSHLocked {
		t.Fatalf("state: %+v", st)
	}
	client := st.ClientIP

	// Block: bad input, own address and localhost are refused per address.
	r, err := s.AccessBlock(id, AccessRequest{Addrs: []string{"198.51.100.7", "198.51.100.0/24", "2001:db8::66", client, "127.0.0.1", "nonsense", "0.0.0.0/0", "198.51.100.7"}, Comment: "smtest 'x'; rm"}, "")
	must(t, err)
	if len(r.Applied) != 3 || skipReason(r, client) != "self" || skipReason(r, "127.0.0.1") != "local" || skipReason(r, "nonsense") != "invalid" {
		t.Fatalf("block result: %+v", r)
	}
	// Deny rules go before the SSH allow rule (first in their family).
	num := sh(t, s, id, "ufw status numbered")
	if i, j := strings.Index(num, "198.51.100.0/24"), strings.Index(num, "22/tcp "); i < 0 || j < 0 || i > j {
		t.Fatalf("order:\n%s", num)
	}
	if err := sshLogin("tester", "secret123"); err != nil {
		t.Fatalf("SSH must still work: %v", err)
	}
	st, _ = s.AccessList(id, "")
	e, ok := findEntry(st, "198.51.100.7", "block", "ufw")
	if !ok || !e.Managed || !strings.HasPrefix(e.Comment, "sm: smtest") {
		t.Fatalf("entry: %+v", st.Entries)
	}
	r, _ = s.AccessBlock(id, AccessRequest{Addrs: []string{"198.51.100.7"}}, "")
	if skipReason(r, "198.51.100.7") != "already" {
		t.Fatalf("already: %+v", r)
	}

	// Allow: SSH only; a later block of it is refused.
	r, err = s.AccessAllow(id, AccessRequest{Addrs: []string{"203.0.113.9"}, Ports: "ssh", Comment: "smtest office"}, "")
	must(t, err)
	st, _ = s.AccessList(id, "")
	if e, ok := findEntry(st, "203.0.113.9", "allow", "ufw"); !ok || e.Ports != "22/tcp" {
		t.Fatalf("allow: %+v", st.Entries)
	}
	r, _ = s.AccessBlock(id, AccessRequest{Addrs: []string{"203.0.113.9"}}, "")
	if skipReason(r, "203.0.113.9") != "allowlisted" {
		t.Fatalf("allowlisted: %+v", r)
	}

	// SSH lock needs this session's address allowlisted first.
	wantCode(t, s.AccessSSHLock(id, true, ""), "sec.acc.lockNeedsSelf")
	_, err = s.AccessAllow(id, AccessRequest{Addrs: []string{client}, Comment: "smtest me"}, "")
	must(t, err)
	must(t, s.AccessSSHLock(id, true, ""))
	st, _ = s.AccessList(id, "")
	if !st.SSHLocked {
		t.Fatalf("not locked: %s", strings.Join(st.ufwAdded, "\n"))
	}
	if err := sshLogin("tester", "secret123"); err != nil {
		t.Fatalf("allowlisted client must still log in: %v", err)
	}
	me, _ := findEntry(st, client, "allow", "ufw")
	_, err = s.AccessRemove(id, []AccessEntry{me}, false, "")
	wantCode(t, err, "sec.acc.removeSelf")
	must(t, s.AccessSSHLock(id, false, ""))
	st, _ = s.AccessList(id, "")
	if st.SSHLocked {
		t.Fatal("still locked")
	}

	// Remove everything the test added; a stale entry is reported as gone.
	rm := []AccessEntry{}
	for _, e := range st.Entries {
		if e.Source == "ufw" && strings.Contains(e.Comment, "smtest") {
			rm = append(rm, e)
		}
	}
	r, err = s.AccessRemove(id, rm, false, "")
	must(t, err)
	if len(r.Applied) != len(rm) || len(rm) != 5 {
		t.Fatalf("remove: %d entries, %+v", len(rm), r)
	}
	r, _ = s.AccessRemove(id, rm[:1], false, "")
	if len(r.Skipped) != 1 || r.Skipped[0].Reason != "gone" {
		t.Fatalf("gone: %+v", r)
	}
	st, _ = s.AccessList(id, "")
	for _, e := range st.Entries {
		if strings.Contains(e.Comment, "smtest") {
			t.Fatalf("left over: %+v", e)
		}
	}
}

func TestAccessFail2ban(t *testing.T) {
	s, _, id := setup(t)
	st, err := s.AccessList(id, "")
	must(t, err)
	if !st.Fail2ban.Installed {
		t.Skip("fail2ban not installed on the test server")
	}
	wasRunning := st.Fail2ban.Running
	t.Cleanup(func() {
		cmd := "rm -f " + f2bFile + " " + f2bFile + ".bak"
		if wasRunning {
			cmd += "; fail2ban-client reload >/dev/null 2>&1"
		} else {
			cmd += "; systemctl disable --now fail2ban >/dev/null 2>&1; true"
		}
		sh(t, s, id, cmd)
	})
	wantCode(t, s.Fail2banApply(id, Fail2banConfig{MaxRetry: 0, FindTime: 600, BanTime: 600}, ""), "sec.f2b.invalidSetting")
	cfg := Fail2banConfig{MaxRetry: 6, FindTime: 600, BanTime: 900, Increment: true, MaxTime: 86400, Recidive: true, IgnoreIP: []string{"203.0.113.50"}}
	must(t, s.Fail2banApply(id, cfg, ""))
	st, err = s.AccessList(id, "")
	must(t, err)
	f := st.Fail2ban
	if !f.Running || !f.Managed || f.SSHJail != "sshd" || f.Config.MaxRetry != 6 || f.Config.BanTime != 900 || !slices.Contains(f.Config.IgnoreIP, "203.0.113.50") || !slices.Contains(f.Config.IgnoreIP, st.ClientIP) {
		t.Fatalf("fail2ban: %+v", f)
	}
	if got := sh(t, s, id, "fail2ban-client get sshd maxretry"); strings.TrimSpace(got) != "6" {
		t.Fatalf("runtime maxretry %q", got)
	}

	// Temporary block = fail2ban ban; CIDRs and the session address refused.
	r, err := s.AccessBlock(id, AccessRequest{Addrs: []string{"198.51.100.99", "198.51.100.0/24", st.ClientIP}, Temporary: true}, "")
	must(t, err)
	if len(r.Applied) != 1 || skipReason(r, "198.51.100.0/24") != "cidrTemp" || skipReason(r, st.ClientIP) != "self" {
		t.Fatalf("temp block: %+v", r)
	}
	st, _ = s.AccessList(id, "")
	e, ok := findEntry(st, "198.51.100.99", "block", "fail2ban")
	if !ok || e.Jail != "sshd" {
		t.Fatalf("banned entry: %+v", st.Entries)
	}
	_, err = s.AccessRemove(id, []AccessEntry{e}, false, "")
	must(t, err)
	st, _ = s.AccessList(id, "")
	if _, ok := findEntry(st, "198.51.100.99", "block", "fail2ban"); ok {
		t.Fatal("still banned")
	}

	// Allowlisting adds to the ignore list (file + running jails).
	_, err = s.AccessAllow(id, AccessRequest{Addrs: []string{"203.0.113.51"}}, "")
	must(t, err)
	if !strings.Contains(sh(t, s, id, "cat "+f2bFile), "203.0.113.51") || !strings.Contains(sh(t, s, id, "fail2ban-client get sshd ignoreip"), "203.0.113.51") {
		t.Fatal("ignoreip not updated")
	}
	st, _ = s.AccessList(id, "")
	for _, e := range st.Entries {
		if e.Addr == "203.0.113.51" && e.Kind == "allow" {
			_, err = s.AccessRemove(id, []AccessEntry{e}, false, "")
			must(t, err)
		}
	}
	if strings.Contains(sh(t, s, id, "cat "+f2bFile), "203.0.113.51") {
		t.Fatal("ignoreip not cleaned")
	}

	must(t, s.Fail2banSetRunning(id, false, ""))
	st, _ = s.AccessList(id, "")
	if st.Fail2ban.Running {
		t.Fatal("still running")
	}
}

// TestAccessIptables runs on the Alpine server (iptables only, no ufw).
func TestAccessIptables(t *testing.T) {
	c, id := testutil.Connect(t, 2224)
	s := New(c)
	st, err := s.AccessList(id, "")
	must(t, err)
	if st.Backend != "iptables" {
		t.Skipf("backend %s", st.Backend)
	}
	r, err := s.AccessBlock(id, AccessRequest{Addrs: []string{"198.51.100.8", "2001:db8::77", st.ClientIP}, Comment: "smtest"}, "")
	must(t, err)
	if len(r.Applied) != 2 || skipReason(r, st.ClientIP) != "self" {
		t.Fatalf("block: %+v", r)
	}
	_, err = s.AccessAllow(id, AccessRequest{Addrs: []string{"203.0.113.10"}, Ports: "22,443", Comment: "smtest"}, "")
	must(t, err)
	st, _ = s.AccessList(id, "")
	b4, ok4 := findEntry(st, "198.51.100.8", "block", "iptables")
	b6, ok6 := findEntry(st, "2001:db8::77", "block", "iptables")
	al, oka := findEntry(st, "203.0.113.10", "allow", "iptables")
	if !ok4 || !ok6 || !oka || !b4.Managed || al.Ports != "22,443/tcp" {
		t.Fatalf("entries: %+v", st.Entries)
	}
	r, err = s.AccessRemove(id, []AccessEntry{b4, b6, al}, false, "")
	must(t, err)
	if len(r.Applied) != 3 {
		t.Fatalf("remove: %+v", r)
	}
	st, _ = s.AccessList(id, "")
	for _, e := range st.Entries {
		if strings.Contains(e.Comment, "smtest") {
			t.Fatalf("left over: %+v", e)
		}
	}
}

// TestAccessFirewalld needs the AlmaLinux + firewalld server (port 2227).
func TestAccessFirewalld(t *testing.T) {
	const port = 2227
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second); err != nil {
		t.Skip("no firewalld test server")
	} else {
		c.Close()
	}
	login := func() error {
		cfg := &ssh.ClientConfig{User: "tester", Auth: []ssh.AuthMethod{ssh.Password("secret123")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
		c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), cfg)
		if err != nil {
			return err
		}
		return c.Close()
	}
	c, id := testutil.Connect(t, port)
	s := New(c)
	t.Cleanup(func() {
		sh(t, s, id, `for z in drop trusted; do for a in $(firewall-cmd --permanent --zone=$z --list-sources); do firewall-cmd --permanent --zone=$z --remove-source=$a; done; done
firewall-cmd --permanent --list-rich-rules | while read -r r; do firewall-cmd --permanent --remove-rich-rule="$r"; done
firewall-cmd --permanent --add-service=ssh; firewall-cmd --reload`)
	})
	st, err := s.AccessList(id, "")
	must(t, err)
	if st.Backend != "firewalld" || !st.Active || !st.CanLock || st.SSHLocked || st.ClientIP == "" {
		t.Fatalf("state: %+v", st)
	}
	client := st.ClientIP
	r, err := s.AccessBlock(id, AccessRequest{Addrs: []string{"198.51.100.20", "198.51.100.32/28", client}}, "")
	must(t, err)
	if len(r.Applied) != 2 || skipReason(r, client) != "self" {
		t.Fatalf("block: %+v", r)
	}
	_, err = s.AccessAllow(id, AccessRequest{Addrs: []string{"203.0.113.20"}, Ports: "ssh"}, "")
	must(t, err)
	st, _ = s.AccessList(id, "")
	b, okb := findEntry(st, "198.51.100.32/28", "block", "firewalld")
	a, oka := findEntry(st, "203.0.113.20", "allow", "firewalld")
	if !okb || !oka || a.Ports != "22/tcp" || !strings.HasPrefix(b.Ref, "zone:drop:") {
		t.Fatalf("entries: %+v", st.Entries)
	}
	// Allowing a blocked source moves it out of the drop zone.
	_, err = s.AccessAllow(id, AccessRequest{Addrs: []string{"198.51.100.20"}}, "")
	must(t, err)
	st, _ = s.AccessList(id, "")
	if _, ok := findEntry(st, "198.51.100.20", "block", ""); ok {
		t.Fatal("still in drop zone")
	}
	if _, ok := findEntry(st, "198.51.100.20", "allow", "firewalld"); !ok {
		t.Fatal("not trusted")
	}

	wantCode(t, s.AccessSSHLock(id, true, ""), "sec.acc.lockNeedsSelf")
	_, err = s.AccessAllow(id, AccessRequest{Addrs: []string{client}}, "")
	must(t, err)
	must(t, s.AccessSSHLock(id, true, ""))
	st, _ = s.AccessList(id, "")
	if !st.SSHLocked || slices.Contains(st.fwdSvc, "ssh") {
		t.Fatalf("not locked: %v", st.fwdSvc)
	}
	if err := login(); err != nil {
		t.Fatalf("trusted client must still log in: %v", err)
	}
	must(t, s.AccessSSHLock(id, false, ""))
	st, _ = s.AccessList(id, "")
	if st.SSHLocked {
		t.Fatal("still locked")
	}
	rm := []AccessEntry{}
	for _, e := range st.Entries {
		if e.Source == "firewalld" {
			rm = append(rm, e)
		}
	}
	r, err = s.AccessRemove(id, rm, false, "")
	must(t, err)
	if len(r.Applied) != len(rm) || len(rm) != 4 {
		t.Fatalf("remove %d: %+v", len(rm), r)
	}
}
