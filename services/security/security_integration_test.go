//go:build integration

package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

// secPort is the dedicated security test server (SM_TEST_SEC_PORT, default 2226).
func secPort() int {
	if p, err := strconv.Atoi(os.Getenv("SM_TEST_SEC_PORT")); err == nil {
		return p
	}
	return 2226
}

func setup(t *testing.T) (*SecurityService, *core.Core, string) {
	t.Helper()
	c, id := testutil.Connect(t, secPort())
	return New(c), c, id
}

// sh runs a root command on the test server (test plumbing, not audited).
func sh(t *testing.T, s *SecurityService, id, cmd string) string {
	t.Helper()
	conn, err := s.core.Conn(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	res, err := s.root(ctx, conn, cmd, "", "")
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return res.Stdout
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !apperr.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sshLogin tries a password login; returns nil on success.
func sshLogin(user, password string) error {
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", secPort()), cfg)
	if err != nil {
		return err
	}
	return c.Close()
}

func TestReadOnly(t *testing.T) {
	s, _, id := setup(t)

	ov, err := s.Overview(id, "")
	must(t, err)
	if len(ov.Checks) < 10 || ov.Score <= 0 || ov.Score > 100 {
		t.Fatalf("overview: %+v", ov)
	}
	for _, c := range ov.Checks {
		t.Logf("check %-15s %-7s %-10s %v %v", c.ID, c.Status, c.Code, c.Params, c.Items)
	}
	t.Logf("score %d distro %q firewall %s", ov.Score, ov.Distro, ov.Firewall)

	cfg, err := s.SSHConfig(id, "")
	must(t, err)
	if !slices.Contains(cfg.Ports, 22) || cfg.Mode != "dropin" || cfg.SessionPort != 22 || cfg.PasswordAuthentication == "" {
		t.Fatalf("ssh config: %+v", cfg)
	}
	t.Logf("ssh: %+v", cfg)

	fw, err := s.FirewallDetect(id, "")
	must(t, err)
	if fw.Primary != "ufw" || !slices.Contains(fw.SSHPorts, 22) || fw.ClientIP == "" {
		t.Fatalf("detect: %+v", fw)
	}
	for _, b := range fw.Backends {
		if b.Name == "iptables" && b.Active {
			t.Fatal("empty ufw chains reported as an active iptables firewall")
		}
	}
	ipt, err := s.IptablesStatus(id, false, "")
	must(t, err)
	if len(ipt.Chains) != 3 || ipt.Chains[0].Policy == "" {
		t.Fatalf("iptables: %+v", ipt)
	}
	if _, err := s.NftRuleset(id, ""); err != nil {
		t.Fatal(err)
	}

	ports, err := s.OpenPorts(id, true, "")
	must(t, err)
	found := false
	for _, l := range ports.Listeners {
		if l.Port == 22 && l.Proto == "tcp" && l.Process == "sshd" && l.Public && len(l.PIDs) > 0 {
			found = true
		}
	}
	if !found || ports.Tool != "ss" || ports.Limited {
		t.Fatalf("ports: %+v", ports)
	}

	us, err := s.Users(id, "")
	must(t, err)
	tester := findUser(us, testutil.User)
	root := findUser(us, "root")
	if tester == nil || !tester.Admin || !tester.IsLogin || tester.PwStatus != "P" || root == nil || root.System || us.Limited {
		t.Fatalf("users: %+v %+v limited=%v", tester, root, us.Limited)
	}
	if findUser(us, "daemon") == nil || !findUser(us, "daemon").System || !slices.Contains(us.Shells, "/bin/bash") {
		t.Fatal("system accounts/shells not detected")
	}
}

func TestSSHConfigCandidate(t *testing.T) {
	s, _, id := setup(t)
	dir := "/tmp/smtest-sec-ssh-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { sh(t, s, id, "rm -rf "+core.Q(dir)) })
	// A copy of /etc/ssh whose Include points at the copy.
	sh(t, s, id, "cp -a /etc/ssh "+core.Q(dir)+" && sed -i 's#/etc/ssh/sshd_config.d#"+dir+"/sshd_config.d#' "+core.Q(dir+"/sshd_config")+
		" && printf 'PasswordAuthentication yes\\n' > "+core.Q(dir+"/sshd_config.d/50-cloud-init.conf"))
	main := dir + "/sshd_config"

	// Guards.
	_, err := s.updateSSH(id, SSHUpdate{Set: map[string][]string{"Port": {"2222"}}}, "", main, false)
	wantCode(t, err, "sec.ssh.removeCurrentPort")
	_, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"Port": {"22", "2222"}}}, "", main, false)
	wantCode(t, err, "sec.ssh.confirmPorts")
	_, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"PasswordAuthentication": {"no"}}}, "", main, false)
	wantCode(t, err, "sec.ssh.noKeys")
	_, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"AllowUsers": {"alice bob"}}}, "", main, false)
	wantCode(t, err, "sec.ssh.lockout")
	_, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"MaxAuthTries": {"0"}}}, "", main, false)
	wantCode(t, err, "sec.ssh.invalidValue")
	_, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"Banner": {"/etc/issue"}}}, "", main, false)
	wantCode(t, err, "sec.ssh.unsupportedKey")
	if strings.Contains(sh(t, s, id, "ls "+core.Q(dir+"/sshd_config.d")), dropinName) {
		t.Fatal("rejected candidate left in place")
	}

	// Drop-in mode: 00-server-manager.conf wins over 50-cloud-init.conf.
	res, err := s.updateSSH(id, SSHUpdate{
		Set: map[string][]string{"Port": {"22", "2222"}, "PasswordAuthentication": {"no"}, "PermitRootLogin": {"no"},
			"MaxAuthTries": {"4"}, "X11Forwarding": {"no"}, "ClientAliveInterval": {"300"}},
		Confirm: SSHConfirm{NewPorts: true, NoKeys: true},
	}, "", main, false)
	must(t, err)
	if len(res.Overridden) > 0 || !slices.Equal(res.Config.Ports, []int{22, 2222}) || res.Config.PasswordAuthentication != "no" ||
		res.Config.PermitRootLogin != "no" || res.Config.MaxAuthTries != 4 || res.Config.Mode != "dropin" {
		t.Fatalf("dropin result: %+v", res)
	}
	drop := sh(t, s, id, "cat "+core.Q(dir+"/sshd_config.d/"+dropinName))
	if !strings.Contains(drop, "Port 2222") || !strings.Contains(drop, "PermitRootLogin no") {
		t.Fatalf("dropin content: %s", drop)
	}
	// Removing a directive falls back to the other files.
	res, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"PasswordAuthentication": {}, "Port": {"22"}}}, "", main, false)
	must(t, err)
	if res.Config.PasswordAuthentication != "yes" || !slices.Equal(res.Config.Ports, []int{22}) {
		t.Fatalf("after removal: %+v", res.Config)
	}

	// An invalid candidate is rolled back.
	before := sh(t, s, id, "cat "+core.Q(dir+"/sshd_config.d/"+dropinName))
	conn, _ := s.core.Conn(id)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := s.root(ctx, conn, writeScript(main, []fileEdit{{Path: dir + "/sshd_config.d/" + dropinName, Content: "NoSuchDirective yes\n"}}, "t1"), "", "")
	must(t, err)
	if !strings.Contains(r.Stdout, "@@invalid") {
		t.Fatalf("bad config accepted: %s", r.Stdout)
	}
	if after := sh(t, s, id, "cat "+core.Q(dir+"/sshd_config.d/"+dropinName)); after != before {
		t.Fatalf("not restored: %q", after)
	}

	// Main mode (no Include): a managed block at the top; other Port lines
	// are disabled while the app manages Port and restored afterwards.
	sh(t, s, id, "rm -rf "+core.Q(dir+"/sshd_config.d")+" && sed -i '/^Include/d' "+core.Q(main)+" && printf 'Port 22\\n' >> "+core.Q(main))
	res, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"Port": {"22", "2200"}, "PermitRootLogin": {"prohibit-password"}},
		Confirm: SSHConfirm{NewPorts: true}}, "", main, false)
	must(t, err)
	content := sh(t, s, id, "cat "+core.Q(main))
	if res.Config.Mode != "main" || !strings.HasPrefix(content, blockBegin) || !strings.Contains(content, disabledMark+"Port 22") ||
		!slices.Equal(res.Config.Ports, []int{22, 2200}) {
		t.Fatalf("main mode: %+v\n%s", res.Config, content)
	}
	res, err = s.updateSSH(id, SSHUpdate{Set: map[string][]string{"Port": {}, "PermitRootLogin": {}}}, "", main, false)
	must(t, err)
	content = sh(t, s, id, "cat "+core.Q(main))
	if strings.Contains(content, blockBegin) || strings.Contains(content, disabledMark) || !slices.Equal(res.Config.Ports, []int{22}) {
		t.Fatalf("main mode cleanup: %+v\n%s", res.Config, content)
	}
	// The reload script is valid sh.
	r, err = s.root(ctx, conn, "sh -n", "", reloadScript(true))
	must(t, err)
	if r.ExitCode != 0 {
		t.Fatalf("reload script: %s", r.Stderr)
	}
	// The real config was never touched.
	if strings.Contains(sh(t, s, id, "ls /etc/ssh/sshd_config.d/"), dropinName) {
		t.Fatal("real sshd config modified")
	}
}

func genKey(t *testing.T, comment string) (string, string) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	must(t, err)
	sp, err := ssh.NewPublicKey(pub)
	must(t, err)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment, ssh.FingerprintSHA256(sp)
}

func TestUsersAndKeys(t *testing.T) {
	s, _, id := setup(t)
	name := "smtest-sec-u" + strconv.FormatInt(time.Now().Unix()%100000, 10)
	group := "smtest-sec-g" + strconv.FormatInt(time.Now().Unix()%100000, 10)
	t.Cleanup(func() {
		sh(t, s, id, "userdel -r "+core.Q(name)+" >/dev/null 2>&1; groupdel "+core.Q(group)+" >/dev/null 2>&1; true")
	})
	key1, fp1 := genKey(t, "smtest key one")

	// Validation.
	wantCode(t, s.AddUser(id, NewUser{Name: "Bad Name"}, ""), "sec.invalidUser")
	wantCode(t, s.AddUser(id, NewUser{Name: name, Shell: "/bin/nonexistent"}, ""), "sec.users.invalidShell")
	wantCode(t, s.AddUser(id, NewUser{Name: name, SSHKey: "ssh-rsa garbage"}, ""), "sec.keys.invalid")
	wantCode(t, s.AddUser(id, NewUser{Name: name, Password: "a\nb"}, ""), "sec.users.invalidPassword")

	must(t, s.AddUser(id, NewUser{Name: name, Gecos: "Smoke Test", Shell: "/bin/bash", CreateHome: true, Admin: true, Password: "Init-pass-1", SSHKey: key1}, ""))
	wantCode(t, s.AddUser(id, NewUser{Name: name}, ""), "sec.users.exists")
	us, err := s.Users(id, "")
	must(t, err)
	u := findUser(us, name)
	if u == nil || !u.Admin || u.PwStatus != "P" || u.Shell != "/bin/bash" || u.Home != "/home/"+name || !slices.Contains(u.Groups, "sudo") {
		t.Fatalf("new user: %+v", u)
	}
	must(t, sshLogin(name, "Init-pass-1"))

	// Keys: listed with fingerprint, perms 700/600 owned by the user.
	kl, err := s.ListKeys(id, name, "")
	must(t, err)
	if len(kl.Keys) != 1 || kl.Keys[0].Fingerprint != fp1 || kl.Keys[0].Type != "ssh-ed25519" || kl.Keys[0].Bits != 256 || kl.Keys[0].Comment != "smtest key one" || len(kl.Problems) != 0 {
		t.Fatalf("keys: %+v", kl)
	}
	if st := sh(t, s, id, "stat -c '%a %U' /home/"+name+"/.ssh /home/"+name+"/.ssh/authorized_keys"); st != "700 "+name+"\n600 "+name+"\n" {
		t.Fatalf("perms: %q", st)
	}
	key2, fp2 := genKey(t, "second")
	_, err = s.AddKey(id, name, key1, "")
	wantCode(t, err, "sec.keys.duplicate")
	_, err = s.AddKey(id, name, "ssh-ed25519 AAAA not-base64", "")
	wantCode(t, err, "sec.keys.invalid")
	k2, err := s.AddKey(id, name, "  "+key2+"  ", "")
	must(t, err)
	if k2.Fingerprint != fp2 {
		t.Fatalf("added: %+v", k2)
	}
	wantCode(t, s.RemoveKey(id, name, "SHA256:doesnotexist", false, ""), "sec.keys.notFound")
	must(t, s.RemoveKey(id, name, fp1, false, ""))
	kl, err = s.ListKeys(id, name, "")
	must(t, err)
	if len(kl.Keys) != 1 || kl.Keys[0].Fingerprint != fp2 {
		t.Fatalf("after remove: %+v", kl.Keys)
	}
	// Login user's own keys are readable without sudo.
	if _, err := s.ListKeys(id, "", ""); err != nil {
		t.Fatal(err)
	}

	// Lock / unlock / password.
	must(t, s.SetLocked(id, name, true, ""))
	us, _ = s.Users(id, "")
	if u := findUser(us, name); u == nil || !u.Locked || u.PwStatus != "L" {
		t.Fatalf("locked: %+v", u)
	}
	if sshLogin(name, "Init-pass-1") == nil {
		t.Fatal("locked account could log in with its password")
	}
	must(t, s.SetLocked(id, name, false, ""))
	must(t, s.SetPassword(id, name, "New-pass-2 with spaces:colon", ""))
	must(t, sshLogin(name, "New-pass-2 with spaces:colon"))
	wantCode(t, s.SetPassword(id, name, "", ""), "sec.users.invalidPassword")

	// Groups.
	must(t, s.AddGroup(id, group, ""))
	wantCode(t, s.AddGroup(id, group, ""), "sec.groups.exists")
	must(t, s.SetGroups(id, name, []string{group}, []string{"sudo"}, false, ""))
	us, _ = s.Users(id, "")
	if u := findUser(us, name); u == nil || u.Admin || !slices.Contains(u.Groups, group) {
		t.Fatalf("groups: %+v", u)
	}
	wantCode(t, s.SetGroups(id, name, []string{"no-such-group-x"}, nil, false, ""), "sec.users.groupNotFound")
	wantCode(t, s.SetGroups(id, testutil.User, nil, []string{"sudo"}, false, ""), "sec.users.selfAdmin")
	must(t, s.SetGroups(id, name, nil, []string{group}, false, ""))
	must(t, s.DeleteGroup(id, group, ""))
	wantCode(t, s.DeleteGroup(id, "sudo", ""), "sec.groups.refuseSystem")

	// Refusals.
	wantCode(t, s.DeleteUser(id, testutil.User, false, ""), "sec.users.refuseSelf")
	wantCode(t, s.DeleteUser(id, "root", false, ""), "sec.users.refuseRoot")
	wantCode(t, s.DeleteUser(id, "daemon", false, ""), "sec.users.refuseSystem")
	wantCode(t, s.SetLocked(id, testutil.User, true, ""), "sec.users.refuseSelf")
	wantCode(t, s.DeleteUser(id, "nosuchuser-smtest", false, ""), "sec.users.notFound")

	must(t, s.DeleteUser(id, name, true, ""))
	us, _ = s.Users(id, "")
	if findUser(us, name) != nil {
		t.Fatal("user not deleted")
	}
	if out := sh(t, s, id, "[ -d /home/"+name+" ] && echo exists; true"); strings.Contains(out, "exists") {
		t.Fatal("home not removed")
	}
}

func TestUFW(t *testing.T) {
	s, _, id := setup(t)
	bak := "/tmp/smtest-sec-ufw-bak"
	wasActive := strings.Contains(sh(t, s, id, "ufw status"), "Status: active")
	sh(t, s, id, "rm -rf "+bak+" && cp -a /etc/ufw "+bak)
	t.Cleanup(func() {
		cmd := "ufw --force disable >/dev/null; rm -rf /etc/ufw && cp -a " + bak + " /etc/ufw && rm -rf " + bak
		if wasActive {
			cmd += " && ufw allow 22/tcp >/dev/null && ufw --force enable >/dev/null"
		}
		sh(t, s, id, cmd)
	})
	if wasActive {
		must(t, s.UFWDisable(id, ""))
	}

	// Validation.
	wantCode(t, s.UFWAddRule(id, NewUFWRule{Action: "allow", Port: "70000", Proto: "tcp"}, ""), "sec.invalidPort")
	wantCode(t, s.UFWAddRule(id, NewUFWRule{Action: "allow", Port: "8000:8100", Proto: "any"}, ""), "sec.fw.rangeNeedsProto")
	wantCode(t, s.UFWAddRule(id, NewUFWRule{Action: "allow", Port: "80", From: "10.0.0.300/8"}, ""), "sec.fw.invalidAddr")
	wantCode(t, s.UFWAddRule(id, NewUFWRule{Action: "drop", Port: "80"}, ""), "sec.fw.invalidAction")
	wantCode(t, s.UFWAddRule(id, NewUFWRule{Action: "deny", Port: "22", Proto: "tcp"}, ""), "sec.fw.blocksSsh")

	must(t, s.UFWAddRule(id, NewUFWRule{Action: "allow", Direction: "in", Port: "8080", Proto: "tcp", From: "10.1.2.0/24", Comment: "smtest-sec 'quoted'; rm -rf"}, ""))
	st, err := s.UFWStatus(id, "")
	must(t, err)
	if st.Active || len(st.Added) == 0 || !strings.Contains(strings.Join(st.Added, "\n"), "8080") {
		t.Fatalf("inactive status: %+v", st)
	}

	// Rules of an inactive ufw are deleted by their `ufw show added` line.
	must(t, s.UFWAddRule(id, NewUFWRule{Action: "deny", Port: "5555", Proto: "udp", From: "192.0.2.7", Comment: "smtest-sec del"}, ""))
	st, _ = s.UFWStatus(id, "")
	var addedLine string
	for _, l := range st.Added {
		if strings.Contains(l, "smtest-sec del") {
			addedLine = l
		}
	}
	wantCode(t, s.UFWDeleteAdded(id, "ufw allow 1/tcp", ""), "sec.fw.ruleChanged")
	must(t, s.UFWDeleteAdded(id, addedLine, ""))
	st, _ = s.UFWStatus(id, "")
	if strings.Contains(strings.Join(st.Added, "\n"), "smtest-sec del") || addedLine == "" {
		t.Fatalf("added rule not deleted: %q %v", addedLine, st.Added)
	}

	allowed, err := s.UFWEnable(id, "")
	must(t, err)
	if !slices.Contains(allowed, "22/tcp") {
		t.Fatalf("enable allowed: %v", allowed)
	}
	st, err = s.UFWStatus(id, "")
	must(t, err)
	if !st.Active || st.Defaults.Incoming == "" || st.Logging == "" {
		t.Fatalf("active status: %+v", st)
	}
	var r8080, r22 *UFWRule
	for i, r := range st.Rules {
		t.Logf("rule %+v", r)
		if strings.HasPrefix(r.To, "8080/tcp") && !r.V6 {
			r8080 = &st.Rules[i]
		}
		if r.To == "22/tcp" && !r.V6 {
			r22 = &st.Rules[i]
		}
	}
	if r8080 == nil || r22 == nil || r8080.From != "10.1.2.0/24" || !strings.Contains(r8080.Comment, "smtest-sec") || r22.Action != "ALLOW" {
		t.Fatalf("rules: %+v", st.Rules)
	}
	// The SSH connection still works through the firewall.
	c2, id2 := testutil.Connect(t, secPort())
	if _, err := New(c2).FirewallDetect(id2, ""); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.UFWDeleteRule(id, r8080.Num, "something else", false, ""), "sec.fw.ruleChanged")
	if st.Defaults.Incoming != "allow" {
		wantCode(t, s.UFWDeleteRule(id, r22.Num, r22.Raw, false, ""), "sec.fw.sshRule")
	}
	must(t, s.UFWDeleteRule(id, r8080.Num, r8080.Raw, false, ""))
	must(t, s.UFWReload(id, ""))
	must(t, s.UFWSetLogging(id, "low", ""))
	wantCode(t, s.UFWSetLogging(id, "loud", ""), "sec.fw.invalidValue")
	pc, err := s.CheckPort(id, 22, "tcp", "")
	must(t, err)
	if pc.Backend != "ufw" || !pc.Active || pc.Allowed != "yes" {
		t.Fatalf("check 22: %+v", pc)
	}
	pc, err = s.CheckPort(id, 65010, "tcp", "")
	must(t, err)
	if pc.Allowed != "no" {
		t.Fatalf("check 65010: %+v", pc)
	}
	b, err := s.AllowPort(id, 65010, "tcp", "smtest-sec", "")
	must(t, err)
	pc, _ = s.CheckPort(id, 65010, "tcp", "")
	if b != "ufw" || pc.Allowed != "yes" {
		t.Fatalf("allow port: %s %+v", b, pc)
	}
	must(t, s.UFWDisable(id, ""))
}

func TestIptables(t *testing.T) {
	s, _, id := setup(t)
	t.Cleanup(func() {
		sh(t, s, id, "while iptables -S INPUT | grep -q smtest-sec; do n=$(iptables -L INPUT --line-numbers -n | awk '/smtest-sec/{print $1; exit}'); iptables -D INPUT $n; done; true")
	})
	wantCode(t, s.IptablesAddRule(id, NewIptRule{Chain: "INPUT", Target: "DROP", Proto: "tcp", Port: "22"}, ""), "sec.fw.blocksSsh")
	wantCode(t, s.IptablesAddRule(id, NewIptRule{Chain: "BOGUS", Target: "DROP"}, ""), "sec.fw.invalidValue")
	wantCode(t, s.IptablesAddRule(id, NewIptRule{Chain: "INPUT", Target: "ACCEPT", Proto: "icmp", Port: "80"}, ""), "sec.fw.rangeNeedsProto")
	must(t, s.IptablesAddRule(id, NewIptRule{Chain: "INPUT", Target: "ACCEPT", Proto: "tcp", Port: "65020", Source: "192.0.2.0/24", Comment: "smtest-sec", Top: true}, ""))
	st, err := s.IptablesStatus(id, false, "")
	must(t, err)
	var rule *IptRule
	for i, r := range st.Chains[0].Rules {
		if r.Comment == "smtest-sec" {
			rule = &st.Chains[0].Rules[i]
		}
	}
	if rule == nil || rule.Target != "ACCEPT" || rule.Source != "192.0.2.0/24" || !strings.Contains(rule.Spec, "--dport 65020") {
		t.Fatalf("iptables rule: %+v", st.Chains[0])
	}
	wantCode(t, s.IptablesDeleteRule(id, false, "INPUT", rule.Num, "-A INPUT -j ACCEPT", false, ""), "sec.fw.ruleChanged")
	must(t, s.IptablesDeleteRule(id, false, "INPUT", rule.Num, rule.Spec, false, ""))
	if strings.Contains(sh(t, s, id, "iptables -S INPUT"), "smtest-sec") {
		t.Fatal("rule not deleted")
	}
	_, err = s.IptablesSave(id, "")
	wantCode(t, err, "sec.fw.noPersist")
}

func TestEvents(t *testing.T) {
	s, _, id := setup(t)
	bogus := "smtest-sec-nouser"
	for i := 0; i < 2; i++ {
		if sshLogin(bogus, "wrong") == nil || sshLogin(testutil.User, "wrong-password") == nil {
			t.Fatal("login with a wrong password succeeded")
		}
	}
	must(t, sshLogin(testutil.User, testutil.Password))
	time.Sleep(1500 * time.Millisecond)
	ev, err := s.Events(id, 1, "")
	must(t, err)
	t.Logf("source=%s failed=%d byIP=%+v", ev.Source, ev.FailedTotal, ev.FailedByIP)
	var inv, tst *FailedByUser
	for i, u := range ev.FailedByUser {
		switch u.User {
		case bogus:
			inv = &ev.FailedByUser[i]
		case testutil.User:
			tst = &ev.FailedByUser[i]
		}
	}
	if inv == nil || !inv.Invalid || inv.Count < 2 || tst == nil || tst.Invalid || tst.Count < 2 || len(ev.FailedByIP) == 0 {
		t.Fatalf("failures: %+v", ev.FailedByUser)
	}
	ok := false
	for _, l := range ev.Logins {
		if l.User == testutil.User && l.Method == "password" && l.IP != "" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("logins: %+v", ev.Logins)
	}
	app := false
	for _, e := range ev.Sudo {
		app = app || (e.App && e.User == testutil.User && e.RunAs == "root")
	}
	if !app || !ev.Fail2ban.Installed || ev.Fail2ban.Running || len(ev.Lastb) == 0 {
		t.Fatalf("sudo/f2b/lastb: app=%v f2b=%+v lastb=%d", app, ev.Fail2ban, len(ev.Lastb))
	}
	wantCode(t, s.Fail2banSetBan(id, "bad jail", "1.2.3.4", false, ""), "sec.f2b.invalidJail")
	wantCode(t, s.Fail2banSetBan(id, "sshd", "1.2.3", false, ""), "sec.fw.invalidAddr")
}

func TestViewerDenied(t *testing.T) {
	s, c, id := setup(t)
	testutil.SetRole(t, c, id, "viewer")
	denied := func(err error) {
		t.Helper()
		wantCode(t, err, "access.denied")
	}
	_, err := s.UpdateSSH(id, SSHUpdate{Set: map[string][]string{"X11Forwarding": {"no"}}}, "")
	denied(err)
	_, err = s.AddKey(id, "", "ssh-ed25519 AAAA", "")
	denied(err)
	denied(s.RemoveKey(id, "", "SHA256:x", false, ""))
	denied(s.UFWAddRule(id, NewUFWRule{Action: "allow", Port: "80"}, ""))
	_, err = s.UFWEnable(id, "")
	denied(err)
	denied(s.UFWDisable(id, ""))
	denied(s.IptablesAddRule(id, NewIptRule{Chain: "INPUT", Target: "ACCEPT"}, ""))
	denied(s.FirewalldChange(id, "public", "port", "80/tcp", true, false, ""))
	_, err = s.AllowPort(id, 80, "tcp", "", "")
	denied(err)
	denied(s.Fail2banSetBan(id, "sshd", "1.2.3.4", false, ""))
	denied(s.AddUser(id, NewUser{Name: "smtest-sec-x"}, ""))
	denied(s.DeleteUser(id, "smtest-sec-x", false, ""))
	denied(s.SetPassword(id, "smtest-sec-x", "x", ""))
	denied(s.SetLocked(id, "smtest-sec-x", true, ""))
	denied(s.SetGroups(id, "smtest-sec-x", []string{"sudo"}, nil, false, ""))
	// Reading stays allowed.
	if _, err := s.SSHConfig(id, ""); err != nil {
		t.Fatal(err)
	}
}

// TestAlpineReadOnly checks the BusyBox/Alpine code paths read-only against
// the shared Alpine server (opt-in: SM_TEST_SEC_ALPINE=1).
func TestAlpineReadOnly(t *testing.T) {
	if os.Getenv("SM_TEST_SEC_ALPINE") == "" {
		t.Skip("set SM_TEST_SEC_ALPINE=1 to run against testutil.DindPort()")
	}
	c, id := testutil.Connect(t, testutil.DindPort())
	s := New(c)
	us, err := s.Users(id, "")
	must(t, err)
	if findUser(us, testutil.User) == nil || findUser(us, "root") == nil || len(us.Shells) == 0 {
		t.Fatalf("alpine users: %+v", us)
	}
	t.Logf("alpine users: busybox tools=%v limited=%v admin groups=%v", us.Busybox, us.Limited, us.AdminGroups)
	ports, err := s.OpenPorts(id, true, "")
	must(t, err)
	t.Logf("ports via %s: %d listeners", ports.Tool, len(ports.Listeners))
	found := false
	for _, l := range ports.Listeners {
		found = found || (l.Port == 22 && l.Proto == "tcp")
	}
	if !found {
		t.Fatalf("alpine ports: %+v", ports)
	}
	ov, err := s.Overview(id, "")
	must(t, err)
	for _, ch := range ov.Checks {
		t.Logf("check %-15s %-7s %s %v", ch.ID, ch.Status, ch.Code, ch.Params)
	}
	cfg, err := s.SSHConfig(id, "")
	must(t, err)
	t.Logf("alpine ssh: mode=%s ports=%v path=%s", cfg.Mode, cfg.Ports, cfg.ConfigPath)
	ev, err := s.Events(id, 24, "")
	must(t, err)
	t.Logf("alpine events: source=%s failed=%d logins=%d sudo=%d", ev.Source, ev.FailedTotal, len(ev.Logins), len(ev.Sudo))
	fw, err := s.FirewallDetect(id, "")
	must(t, err)
	t.Logf("alpine firewall: %+v", fw)
}
