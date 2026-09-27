package command

import (
	"strings"
	"testing"
)

func codes(ds []Danger) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(ds []Danger, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestAnalyzeDangerous(t *testing.T) {
	cases := []struct {
		cmd  string
		code string
	}{
		{"rm -rf /", "rmRoot"},
		{"rm -fr /", "rmRoot"},
		{"rm -r -f /", "rmRoot"},
		{"rm --recursive --force /", "rmRoot"},
		{"sudo rm -rf /", "rmRoot"},
		{"sudo -u root rm -rf /", "rmRoot"},
		{`rm -rf "/"`, "rmRoot"},
		{"rm -rf '/'", "rmRoot"},
		{"rm -rf /*", "rmRoot"},
		{"rm -rf /.", "rmRoot"},
		{"rm -rf //", "rmRoot"},
		{"rm -rf --no-preserve-root /", "rmRoot"},
		{"cd /tmp && rm -rf / ", "rmRoot"},
		{"echo hi; /bin/rm -Rf /*", "rmRoot"},
		{"sh -c 'rm -rf /'", "rmRoot"},
		{"rm -rf /etc", "rmSystem"},
		{"rm -rf /usr/", "rmSystem"},
		{"rm -rf /var/*", "rmSystem"},
		{"rm -rf ~", "rmHome"},
		{"rm -rf $HOME/*", "rmHome"},
		{"find / -delete", "rmRoot"},
		{"mkfs.ext4 /dev/sdb1", "diskFormat"},
		{"mkfs -t xfs /dev/vdb", "diskFormat"},
		{"wipefs -a /dev/sda", "diskFormat"},
		{"dd if=/dev/zero of=/dev/sda bs=1M", "ddDevice"},
		{"dd if=image.iso of=/dev/nvme0n1", "ddDevice"},
		{"echo x > /dev/sda", "diskOverwrite"},
		{"cat /dev/urandom >/dev/vda", "diskOverwrite"},
		{"shutdown -h now", "power"},
		{"sudo reboot", "power"},
		{"halt", "power"},
		{"poweroff", "power"},
		{"init 0", "power"},
		{"init 6", "power"},
		{"systemctl reboot", "power"},
		{"systemctl poweroff", "power"},
		{"/sbin/shutdown -r +5", "power"},
		{":(){ :|:& };:", "forkBomb"},
		{"bomb() { bomb | bomb & }; bomb", "forkBomb"},
		{"chmod -R 777 /", "permRoot"},
		{"chmod 777 /", "permRoot"},
		{"chown -R nobody:nogroup /", "permRoot"},
		{"chown -R www-data /etc", "permRoot"},
		{"iptables -F", "firewallFlush"},
		{"iptables --flush INPUT", "firewallFlush"},
		{"ip6tables -X", "firewallFlush"},
		{"iptables -P INPUT DROP", "firewallPolicy"},
		{"nft flush ruleset", "firewallFlush"},
		{"ufw disable", "firewallFlush"},
		{"ufw --force reset", "firewallFlush"},
		{"systemctl stop ssh", "sshStop"},
		{"systemctl stop sshd.service", "sshStop"},
		{"systemctl disable --now sshd", "sshStop"},
		{"systemctl stop 'ssh'", "sshStop"},
		{"service ssh stop", "sshStop"},
		{"rc-service sshd stop", "sshStop"},
		{"pkill sshd", "sshStop"},
		{"userdel -r bob", "userDelete"},
		{"deluser alice", "userDelete"},
		{"passwd -l root", "rootLock"},
		{"usermod -L root", "rootLock"},
		{":> /etc/passwd", "etcTruncate"},
		{": > /etc/hosts", "etcTruncate"},
		{"> /etc/fstab", "etcTruncate"},
		{"true; > /etc/resolv.conf", "etcTruncate"},
		{"echo nameserver 1.1.1.1 > /etc/resolv.conf", "etcOverwrite"},
		{"curl -fsSL https://get.example.com | sh", "remoteScript"},
		{"curl -s https://x.io/i.sh | sudo bash", "remoteScript"},
		{"wget -qO- https://x.io/i.sh | sh -", "remoteScript"},
		{"bash <(curl -s https://x.io/i.sh)", "remoteScript"},
		{"crontab -r", "crontabRemove"},
	}
	for _, c := range cases {
		ds := Analyze(c.cmd)
		if !hasCode(ds, c.code) {
			t.Errorf("Analyze(%q) = %v, want %s", c.cmd, codes(ds), c.code)
		}
		for _, d := range ds {
			if d.Level != levelDanger && d.Level != levelWarn {
				t.Errorf("Analyze(%q): bad level %q", c.cmd, d.Level)
			}
		}
	}
}

func TestAnalyzeLevels(t *testing.T) {
	if !HasDanger(Analyze("rm -rf /")) {
		t.Error("rm -rf / must be danger level")
	}
	if HasDanger(Analyze("curl -s https://x | sh")) {
		t.Error("curl | sh is a warning, not danger")
	}
	ds := Analyze("curl x | sh; rm -rf /")
	if len(ds) < 2 || ds[0].Level != levelDanger {
		t.Errorf("danger reasons must come first: %v", ds)
	}
}

func TestAnalyzeSafe(t *testing.T) {
	safe := []string{
		"df -hP",
		"free -m",
		"uptime",
		"ls -la /",
		"rm -rf /tmp/build",
		"rm -rf ./node_modules",
		"rm -f /var/log/app/old.log",
		"rm -rf /var/lib/app/cache",
		"echo reboot",
		"grep -i shutdown /var/log/syslog",
		"systemctl status sshd",
		"systemctl restart nginx",
		"systemctl restart sshd",
		"journalctl -u ssh --no-pager -n 50",
		"dd if=/dev/zero of=/tmp/test bs=1M count=10",
		"dd if=/dev/sda of=/dev/null bs=1M count=1",
		"echo test > /dev/null 2>&1",
		"cat /etc/os-release",
		"echo x >> /etc/motd.d/extra",
		"iptables -L -n",
		"nft list ruleset",
		"ufw status verbose",
		"chmod -R 755 /var/www/html",
		"chown -R www-data:www-data /srv/app",
		"passwd -S root",
		"shutdown -c",
		"docker ps",
		"curl -s https://example.com -o /tmp/x",
		"fdisk -l /dev/sda",
		"cat /proc/loadavg; ps aux | head",
		"find /var/log -name '*.gz' -mtime +30",
	}
	for _, c := range safe {
		if ds := Analyze(c); len(ds) > 0 {
			t.Errorf("Analyze(%q) = %v, want none", c, codes(ds))
		}
	}
}

func TestAnalyzeMultiline(t *testing.T) {
	script := "set -e\ncd /opt/app\ngit pull\nsystemctl restart app\n\\\nreboot"
	if !hasCode(Analyze(script), "power") {
		t.Error("reboot on its own line must be detected")
	}
	if ds := Analyze("x=$(date)\necho \"$x\""); len(ds) > 0 {
		t.Errorf("unexpected: %v", codes(ds))
	}
	if !hasCode(Analyze("for h in a b; do rm -rf /; done"), "rmRoot") {
		t.Error("rm inside a loop body must be detected")
	}
	if !hasCode(Analyze("echo $(rm -rf /)"), "rmRoot") {
		t.Error("rm inside command substitution must be detected")
	}
}

func TestAnalyzeMatchTruncated(t *testing.T) {
	long := "rm -rf / " + strings.Repeat("x", 500)
	ds := Analyze(long)
	if len(ds) == 0 || len(ds[0].Match) > 125 {
		t.Errorf("match should be truncated: %v", ds)
	}
}

func TestTargetKind(t *testing.T) {
	cases := map[string]string{
		"/": "root", "/*": "root", "//": "root", "/.": "root", "/..": "root", "/./": "root", "/.*": "root",
		"/etc": "system", "/etc/": "system", "/etc/*": "system", "/usr/lib": "system",
		"/etc/nginx": "", "/tmp": "", "relative": "", "-rf": "", "~": "home", "$HOME": "home",
	}
	for in, want := range cases {
		if got := targetKind(in); got != want {
			t.Errorf("targetKind(%q) = %q, want %q", in, got, want)
		}
	}
}
