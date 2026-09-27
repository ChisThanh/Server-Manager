package command

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// Danger is one reason a command looks destructive. Code is translated by
// the frontend as "cmd.danger.<code>"; Match is the offending fragment.
type Danger struct {
	Code  string `json:"code"`
	Level string `json:"level"` // danger | warn
	Match string `json:"match"`
}

const (
	levelDanger = "danger"
	levelWarn   = "warn"
)

// Analyze inspects a shell snippet and returns why it looks dangerous
// (empty when nothing was found). It is a guard rail for the confirmation
// dialog, not a sandbox: obfuscated commands can always slip through.
func Analyze(script string) []Danger {
	found := map[string]Danger{}
	add := func(code, level, match string) {
		match = strings.TrimSpace(match)
		if len(match) > 120 {
			match = match[:120] + "…"
		}
		if _, ok := found[code]; !ok {
			found[code] = Danger{Code: code, Level: level, Match: match}
		}
	}

	// Raw-text rules (need the original punctuation).
	if m := forkBombRe.FindAllStringSubmatch(script, -1); m != nil {
		for _, g := range m {
			if g[1] == g[2] && g[2] == g[3] {
				add("forkBomb", levelDanger, g[0])
			}
		}
	}
	flat := stripQuotes(script)
	if m := blockDevRedirRe.FindString(flat); m != "" {
		add("diskOverwrite", levelDanger, m)
	}
	if m := etcTruncateRe.FindString(flat); m != "" {
		add("etcTruncate", levelDanger, m)
	} else if m := etcOverwriteRe.FindString(flat); m != "" {
		add("etcOverwrite", levelWarn, m)
	}
	if m := pipeShellRe.FindString(flat); m != "" {
		add("remoteScript", levelWarn, m)
	} else if m := procSubstShellRe.FindString(flat); m != "" {
		add("remoteScript", levelWarn, m)
	}

	// Per simple command rules.
	for _, words := range simpleCommands(flat) {
		checkCommand(words, add)
	}

	out := make([]Danger, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level == levelDanger
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// HasDanger reports whether any reason has the danger level.
func HasDanger(ds []Danger) bool {
	for _, d := range ds {
		if d.Level == levelDanger {
			return true
		}
	}
	return false
}

var (
	forkBombRe       = regexp.MustCompile(`([A-Za-z_:.][\w:.]*)\s*\(\s*\)\s*\{\s*([A-Za-z_:.][\w:.]*)\s*\|\s*([A-Za-z_:.][\w:.]*)\s*&\s*;?\s*\}`)
	blockDevRedirRe  = regexp.MustCompile(`(^|[^>&0-9])[0-9]?>\|?\s*/dev/(sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|dm-\d|mapper/|disk/|md\d)\S*`)
	etcTruncateRe    = regexp.MustCompile(`(^|[;&|(\n]|\bthen|\bdo|\belse)\s*(:|true)?\s*>\|?\s*/etc/\S*`)
	etcOverwriteRe   = regexp.MustCompile(`(^|[^>&0-9])[0-9]?>\|?\s*/etc/\S*`)
	pipeShellRe      = regexp.MustCompile(`\b(curl|wget|fetch)\b[^\n;]*\|\s*(sudo\s+(-\S+\s+)*)?(env\s+(\S+=\S*\s+)*)?(/\S*/)?(ba|da|z|k|a|c|tc)?sh\b`)
	procSubstShellRe = regexp.MustCompile(`\b(ba|da|z|k)?sh\s+(-c\s+)?(<\(|\$\()\s*(curl|wget)\b`)
)

// stripQuotes removes shell quoting so `rm -rf "/"` reads as `rm -rf /`.
func stripQuotes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'', '"':
			continue
		case '\\':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++ // line continuation
				b.WriteByte(' ')
				continue
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// simpleCommands splits a (quote-stripped) script into simple commands and
// their words, dropping wrappers like sudo/env/nohup and assignments.
func simpleCommands(s string) [][]string {
	var out [][]string
	var cur strings.Builder
	flush := func() {
		if words := strings.Fields(cur.String()); len(words) > 0 {
			if w := unwrap(words); len(w) > 0 {
				out = append(out, w)
			}
		}
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ';', '\n', '|', '&', '(', ')', '`', '{', '}':
			// Keep redirections like 2>&1 or &> intact enough: '&' right
			// after '>' is part of a redirection, not a separator.
			if c == '&' && i > 0 && s[i-1] == '>' {
				cur.WriteByte(c)
				continue
			}
			flush()
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				flush()
				i++
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// unwrap drops leading keywords, assignments and command wrappers.
func unwrap(w []string) []string {
	for len(w) > 0 {
		name := path.Base(w[0])
		switch {
		case assignRe.MatchString(w[0]):
			w = w[1:]
		case name == "sudo" || name == "doas":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				opt := w[0]
				w = w[1:]
				// Options that take a separate argument.
				if (opt == "-u" || opt == "-g" || opt == "-C" || opt == "-D" || opt == "-h" || opt == "-p" || opt == "-r" || opt == "-t" || opt == "-U") && len(w) > 0 {
					w = w[1:]
				}
				if opt == "--" {
					break
				}
			}
		case name == "env":
			w = w[1:]
			for len(w) > 0 && (strings.HasPrefix(w[0], "-") || assignRe.MatchString(w[0])) {
				w = w[1:]
			}
		case name == "nice" || name == "ionice" || name == "stdbuf" || name == "chrt" || name == "taskset":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
			if name == "nice" || name == "chrt" || name == "taskset" {
				// nice -n 10 cmd / chrt 10 cmd / taskset 0x1 cmd
				for len(w) > 1 && isNumberish(w[0]) {
					w = w[1:]
				}
			}
		case name == "timeout":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				opt := w[0]
				w = w[1:]
				if (opt == "-s" || opt == "-k" || opt == "--signal" || opt == "--kill-after") && len(w) > 0 {
					w = w[1:]
				}
			}
			if len(w) > 0 {
				w = w[1:] // duration
			}
		case name == "nohup" || name == "time" || name == "exec" || name == "command" || name == "builtin" ||
			name == "xargs" || name == "busybox" || name == "then" || name == "do" || name == "else" ||
			name == "elif" || name == "if" || name == "while" || name == "until" || name == "!" || name == "eval":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") && name != "!" {
				w = w[1:]
			}
		case name == "sh" || name == "bash" || name == "dash" || name == "zsh" || name == "ash":
			// sh -c 'rm -rf /' → analyse the inner script's words too.
			if len(w) > 2 && w[1] == "-c" {
				w = w[2:]
				continue
			}
			return w
		default:
			return w
		}
	}
	return w
}

func isNumberish(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != 'x' && c != '-' && c != '+' && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

var systemDirs = map[string]bool{
	"/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true, "/lib": true, "/lib32": true,
	"/lib64": true, "/opt": true, "/proc": true, "/root": true, "/sbin": true, "/srv": true, "/sys": true,
	"/usr": true, "/var": true, "/usr/bin": true, "/usr/lib": true, "/usr/sbin": true, "/usr/local": true,
	"/var/lib": true, "/var/log": true,
}

// targetKind classifies a path argument: "root" for / (and /*, /., //…),
// "system" for a top-level system directory, "home" for ~ / $HOME, "".
func targetKind(arg string) string {
	a := strings.TrimSpace(arg)
	if a == "" || strings.HasPrefix(a, "-") {
		return ""
	}
	switch a {
	case "~", "~/", "~/*", "$HOME", "$HOME/", "$HOME/*", "${HOME}", "${HOME}/", "${HOME}/*":
		return "home"
	}
	if !strings.HasPrefix(a, "/") {
		return ""
	}
	// Collapse "/*", "/.", "//", "/etc/", "/etc/*" to their directory.
	a = path.Clean(a)
	for _, suf := range []string{"/*", "/.*"} {
		a = strings.TrimSuffix(a, suf)
	}
	if a == "" {
		a = "/"
	}
	a = path.Clean(a)
	if a == "/" || a == "/*" || a == "/." || a == "/.." {
		return "root"
	}
	if systemDirs[a] {
		return "system"
	}
	return ""
}

func hasFlag(args []string, short string, long ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if strings.HasPrefix(a, "--") {
			for _, l := range long {
				if a == l || strings.HasPrefix(a, l+"=") {
					return true
				}
			}
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && strings.ContainsAny(a[1:], short) {
			return true
		}
	}
	return false
}

func checkCommand(w []string, add func(code, level, match string)) {
	name := path.Base(w[0])
	args := w[1:]
	text := strings.Join(w, " ")
	if strings.HasPrefix(name, "mkfs.") || strings.HasPrefix(name, "mke2fs.") {
		name = "mkfs" // mkfs.ext4, mkfs.xfs…
	}

	switch name {
	case "rm":
		recursive := hasFlag(args, "rR", "--recursive")
		if hasFlag(args, "", "--no-preserve-root") {
			add("rmRoot", levelDanger, text)
		}
		for _, a := range args {
			switch targetKind(a) {
			case "root":
				add("rmRoot", levelDanger, text)
			case "system":
				if recursive {
					add("rmSystem", levelDanger, text)
				}
			case "home":
				if recursive {
					add("rmHome", levelDanger, text)
				}
			}
		}
	case "find":
		if len(args) > 0 && targetKind(args[0]) != "" && contains(args, "-delete") {
			add("rmRoot", levelDanger, text)
		}
	case "shred", "wipefs", "blkdiscard", "sgdisk", "sfdisk", "parted", "fdisk":
		for _, a := range args {
			if strings.HasPrefix(a, "/dev/") && isBlockDev(a) {
				if name == "fdisk" && hasFlag(args, "l", "--list") {
					break
				}
				if name == "parted" && (contains(args, "print") || hasFlag(args, "l", "--list")) {
					break
				}
				add("diskFormat", levelDanger, text)
				break
			}
		}
	case "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=/dev/") && !harmlessDev(strings.TrimPrefix(a, "of=")) {
				add("ddDevice", levelDanger, text)
			}
		}
	case "shutdown", "reboot", "halt", "poweroff":
		if !hasFlag(args, "", "--help") && !contains(args, "-c") {
			add("power", levelDanger, text)
		}
	case "init", "telinit":
		if len(args) > 0 && (args[0] == "0" || args[0] == "6") {
			add("power", levelDanger, text)
		}
	case "systemctl":
		verb, units := systemctlVerb(args)
		switch verb {
		case "reboot", "poweroff", "halt", "kexec", "soft-reboot", "rescue", "emergency":
			add("power", levelDanger, text)
		case "stop", "disable", "mask", "kill", "restart":
			for _, u := range units {
				if isSSHUnit(u) {
					if verb == "restart" {
						continue // a restart keeps sessions and comes back
					}
					add("sshStop", levelDanger, text)
				}
			}
		case "isolate":
			for _, u := range units {
				if strings.HasPrefix(u, "rescue") || strings.HasPrefix(u, "emergency") || strings.HasPrefix(u, "poweroff") || strings.HasPrefix(u, "reboot") || strings.HasPrefix(u, "halt") {
					add("power", levelDanger, text)
				}
			}
		}
	case "service", "rc-service":
		if len(args) >= 2 && isSSHUnit(args[0]) && (args[1] == "stop" || args[1] == "disable") {
			add("sshStop", levelDanger, text)
		}
	case "rc-update", "update-rc.d", "chkconfig":
		for _, a := range args {
			if isSSHUnit(a) && (contains(args, "del") || contains(args, "remove") || contains(args, "disable") || contains(args, "off")) {
				add("sshStop", levelDanger, text)
				break
			}
		}
	case "killall", "pkill":
		for _, a := range args {
			if a == "sshd" || a == "ssh" {
				add("sshStop", levelDanger, text)
			}
		}
	case "mkfs", "mke2fs", "mkswap", "mkdosfs", "mkntfs":
		add("diskFormat", levelDanger, text)
	case "chmod", "chown", "chgrp":
		recursive := hasFlag(args, "R", "--recursive")
		for _, a := range args {
			k := targetKind(a)
			if k == "root" || (recursive && k == "system") {
				add("permRoot", levelDanger, text)
			}
		}
	case "iptables", "ip6tables", "iptables-legacy", "iptables-nft", "ip6tables-legacy", "ip6tables-nft":
		if hasFlag(args, "FX", "--flush", "--delete-chain") {
			add("firewallFlush", levelDanger, text)
		}
		if hasFlag(args, "P", "--policy") && (contains(args, "DROP") || contains(args, "REJECT")) {
			add("firewallPolicy", levelDanger, text)
		}
	case "nft":
		if len(args) >= 2 && args[0] == "flush" && args[1] == "ruleset" {
			add("firewallFlush", levelDanger, text)
		}
	case "ufw":
		for _, a := range args {
			if a == "disable" || a == "reset" {
				add("firewallFlush", levelDanger, text)
			}
			if a == "enable" {
				add("firewallEnable", levelWarn, text)
			}
		}
	case "firewall-cmd":
		if contains(args, "--panic-on") {
			add("firewallPolicy", levelDanger, text)
		}
	case "userdel", "deluser", "groupdel", "delgroup":
		add("userDelete", levelDanger, text)
	case "passwd", "usermod":
		if contains(args, "root") && hasFlag(args, "ldL", "--lock", "--delete", "--expire") {
			add("rootLock", levelDanger, text)
		}
	case "crontab":
		if hasFlag(args, "r", "--remove") {
			add("crontabRemove", levelWarn, text)
		}
	case "mv":
		for _, a := range args {
			if targetKind(a) == "root" || targetKind(a) == "system" {
				add("mvSystem", levelDanger, text)
				break
			}
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var blockDevRe = regexp.MustCompile(`^/dev/(sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|dm-\d|md\d|mapper/|disk/|loop\d)`)

func isBlockDev(p string) bool { return blockDevRe.MatchString(p) }

func harmlessDev(p string) bool {
	switch p {
	case "/dev/null", "/dev/zero", "/dev/stdout", "/dev/stderr", "/dev/tty", "/dev/full", "/dev/random", "/dev/urandom":
		return true
	}
	return strings.HasPrefix(p, "/dev/fd/") || strings.HasPrefix(p, "/dev/pts/")
}

func systemctlVerb(args []string) (string, []string) {
	verb := ""
	units := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// Options with a separate value.
			if (a == "-H" || a == "-M" || a == "-t" || a == "-p" || a == "-s" || a == "-n" || a == "-o" || a == "--signal" || a == "--kill-whom") && i+1 < len(args) {
				i++
			}
			continue
		}
		if verb == "" {
			verb = a
			continue
		}
		units = append(units, a)
	}
	return verb, units
}

func isSSHUnit(u string) bool {
	u = strings.TrimSuffix(strings.TrimSuffix(u, ".service"), ".socket")
	return u == "ssh" || u == "sshd" || u == "openssh" || u == "openssh-server" || u == "dropbear"
}
