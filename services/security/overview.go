package security

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/core"
)

// Check is one item of the security checklist. The UI renders the title
// from `sec.chk.<id>`, the finding from `sec.chk.<id>.<code>` (with Params)
// and the recommendation from `sec.fix.<id>`.
type Check struct {
	ID     string            `json:"id"`
	Status string            `json:"status"` // ok | warn | crit | info | unknown
	Code   string            `json:"code"`
	Params map[string]string `json:"params"`
	Items  []string          `json:"items"`
	Weight int               `json:"weight"`
	// Tab is the panel tab where the issue can be fixed.
	Tab string `json:"tab"`
}

type Overview struct {
	Score    int         `json:"score"`
	Checks   []Check     `json:"checks"`
	Logins   []LastEntry `json:"logins"`
	Limited  bool        `json:"limited"` // no root: some checks unknown
	Distro   string      `json:"distro"`
	Firewall string      `json:"firewall"`
	Time     int64       `json:"time"`
}

const overviewScript = `T=""; command -v timeout >/dev/null 2>&1 && T="timeout 40"
. /etc/os-release 2>/dev/null; echo @@distro; echo "$PRETTY_NAME"
echo @@family; echo "$ID $ID_LIKE"
echo @@sshd; sshd -T 2>/dev/null | grep -Ei '^(port|permitrootlogin|passwordauthentication|kbdinteractiveauthentication|challengeresponseauthentication|usepam|pubkeyauthentication) '
` + detectScript + `
echo @@f2b
command -v fail2ban-client >/dev/null 2>&1 && echo installed
if command -v systemctl >/dev/null 2>&1; then
  for u in fail2ban crowdsec sshguard; do [ "$(systemctl is-active $u 2>/dev/null)" = active ] && echo "active=$u"; done
else
  for u in fail2ban-server crowdsec sshguard; do pgrep -x $u >/dev/null 2>&1 && echo "active=$u"; done
fi
echo @@updates
if command -v apt-get >/dev/null 2>&1; then
  echo mgr=apt
  echo "total=$(apt list --upgradable 2>/dev/null | grep -c 'upgradable from')"
  echo "security=$($T apt-get -s -o Debug::NoLocking=1 dist-upgrade 2>/dev/null | grep '^Inst ' | grep -ci 'securi')"
  echo "age=$(( $(date +%s) - $(stat -c %Y /var/lib/apt/lists 2>/dev/null || date +%s) ))"
elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
  M=$(command -v dnf >/dev/null 2>&1 && echo dnf || echo yum); echo "mgr=$M"
  O=$($T $M -q -C check-update 2>/dev/null); rc=$?
  if [ $rc = 0 ] || [ $rc = 100 ]; then
    echo "total=$(printf '%s\n' "$O" | awk 'NF==3 && $1 ~ /\./' | wc -l)"
    echo "security=$($T $M -q -C updateinfo list --security 2>/dev/null | awk 'NF>=3' | wc -l)"
  fi
elif command -v apk >/dev/null 2>&1; then
  echo mgr=apk; echo "total=$(apk list -u 2>/dev/null | wc -l)"
elif command -v zypper >/dev/null 2>&1; then
  echo mgr=zypper; echo "total=$($T zypper -q lu 2>/dev/null | grep -c '^v ')"
  echo "security=$($T zypper -q lp -g security 2>/dev/null | grep -c '|')"
fi
echo @@auto
if command -v dpkg-query >/dev/null 2>&1; then
  dpkg-query -W -f='${Status}\n' unattended-upgrades 2>/dev/null | grep -q 'ok installed' && echo installed=unattended-upgrades
  apt-config dump 2>/dev/null | grep -E '^APT::Periodic::(Unattended-Upgrade|Update-Package-Lists) '
fi
if command -v systemctl >/dev/null 2>&1; then
  for t in dnf-automatic.timer dnf-automatic-install.timer dnf5-automatic.timer yum-cron.service; do [ "$(systemctl is-enabled $t 2>/dev/null)" = enabled ] && echo "enabled=$t"; done
fi
echo @@ports; ss -tulpnH 2>/dev/null || { echo @@netstat; netstat -tulpn 2>/dev/null; }
echo @@uid0; awk -F: '$3==0{print $1}' /etc/passwd
echo @@emptypw; [ -r /etc/shadow ] && { echo readable; awk -F: '$2==""{print $1}' /etc/shadow; }
echo @@ww; find /etc -xdev -maxdepth 4 -type f -perm -0002 2>/dev/null | head -n 20
echo @@tz; date +%z
echo @@last; last -F -w -i -n 15 2>/dev/null || last -n 15 2>/dev/null
echo @@done`

func kv(sec string) map[string]string {
	m := map[string]string{}
	for _, l := range lines(sec) {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func buildOverview(out string, limited bool) Overview {
	sec := sections(out)
	ov := Overview{Checks: []Check{}, Logins: []LastEntry{}, Limited: limited, Time: time.Now().Unix()}
	ov.Distro = strings.TrimSpace(sec["distro"])
	add := func(c Check) {
		if c.Params == nil {
			c.Params = map[string]string{}
		}
		if c.Items == nil {
			c.Items = []string{}
		}
		ov.Checks = append(ov.Checks, c)
	}
	sshd := parseSSHDT(sec["sshd"])
	one := func(k string) string {
		if v := sshd[k]; len(v) > 0 {
			return strings.ToLower(v[0])
		}
		return ""
	}
	haveSSHD := len(sshd) > 0

	// PermitRootLogin.
	c := Check{ID: "rootLogin", Weight: 15, Tab: "ssh"}
	switch v := one("permitrootlogin"); {
	case !haveSSHD:
		c.Status, c.Code = "unknown", "unknown"
	case v == "yes":
		c.Status, c.Code = "crit", "yes"
	case v == "no":
		c.Status, c.Code = "ok", "no"
	case v == "forced-commands-only":
		c.Status, c.Code = "ok", "forced"
	default:
		c.Status, c.Code = "ok", "keyOnly"
	}
	add(c)

	// PasswordAuthentication.
	pwdOn := one("passwordauthentication") == "yes"
	kbdOn := core.FirstNonEmpty(one("kbdinteractiveauthentication"), one("challengeresponseauthentication")) == "yes" && one("usepam") == "yes"
	c = Check{ID: "passwordAuth", Weight: 10, Tab: "ssh"}
	switch {
	case !haveSSHD:
		c.Status, c.Code = "unknown", "unknown"
	case pwdOn:
		c.Status, c.Code = "warn", "enabled"
	case kbdOn:
		c.Status, c.Code = "warn", "kbd"
	default:
		c.Status, c.Code = "ok", "disabled"
	}
	add(c)

	// Listening services (needed by the firewall check).
	var listeners []Listener
	if raw, ok := sec["netstat"]; ok {
		listeners = parseNetstat(raw)
	} else {
		listeners = parseSS(sec["ports"])
	}
	sshPorts := []int{}
	for _, p := range sshd["port"] {
		sshPorts = append(sshPorts, atoi(p))
	}
	if len(sshPorts) == 0 {
		sshPorts = []int{22}
	}
	public := []string{}
	seen := map[string]bool{}
	for _, l := range listeners {
		if !l.Public || (l.Proto == "tcp" && slices.Contains(sshPorts, l.Port)) || (l.Proto == "udp" && (l.Port == 68 || l.Port == 546)) {
			continue
		}
		label := fmt.Sprintf("%d/%s", l.Port, l.Proto)
		if l.Process != "" {
			label += " (" + l.Process + ")"
		}
		if !seen[label] {
			seen[label] = true
			public = append(public, label)
		}
	}

	// Firewall.
	fw := parseDetect(out)
	ov.Firewall = fw.Primary
	active := ""
	installed := false
	for _, b := range fw.Backends {
		installed = installed || b.Installed
		if b.Active && active == "" && b.Name != "nftables" {
			active = b.Name
		}
	}
	if active == "" {
		for _, b := range fw.Backends {
			if b.Active {
				active = b.Name
			}
		}
	}
	c = Check{ID: "firewall", Weight: 15, Tab: "firewall"}
	switch {
	case active != "":
		c.Status, c.Code, c.Params = "ok", "active", map[string]string{"backend": active}
	case limited:
		c.Status, c.Code = "unknown", "needRoot"
	case installed && len(public) > 0:
		c.Status, c.Code, c.Params = "crit", "inactive", map[string]string{"backend": fw.Primary}
	case installed:
		c.Status, c.Code, c.Params = "warn", "inactive", map[string]string{"backend": fw.Primary}
	default:
		c.Status, c.Code = "warn", "none"
	}
	add(c)

	// Brute-force protection.
	f2b := sec["f2b"]
	c = Check{ID: "bruteforce", Weight: 5, Tab: "access"}
	if m := kv(f2b); m["active"] != "" {
		c.Status, c.Code, c.Params = "ok", "active", map[string]string{"name": m["active"]}
	} else if !pwdOn && !kbdOn && haveSSHD {
		c.Status, c.Code = "info", "keysOnly"
	} else if strings.Contains(f2b, "installed") {
		c.Status, c.Code = "warn", "inactive"
	} else {
		c.Status, c.Code = "warn", "missing"
	}
	add(c)

	// Pending updates.
	up := kv(sec["updates"])
	c = Check{ID: "updates", Weight: 10, Params: map[string]string{"mgr": up["mgr"]}}
	total, hasTotal := up["total"]
	nTotal, nSec := atoi(total), atoi(up["security"])
	if age := atoi(up["age"]); age > 7*86400 {
		c.Params["age"] = strconv.Itoa(age / 86400)
	}
	c.Params["n"], c.Params["security"] = strconv.Itoa(nTotal), strconv.Itoa(nSec)
	switch {
	case !hasTotal:
		c.Status, c.Code = "unknown", "unknown"
	case nSec > 0:
		c.Status, c.Code = "crit", "security"
	case nTotal > 0:
		c.Status, c.Code = "warn", "pending"
	default:
		c.Status, c.Code = "ok", "none"
	}
	add(c)

	// Automatic updates.
	au := sec["auto"]
	c = Check{ID: "autoUpdates", Weight: 5}
	family := strings.ToLower(sec["family"])
	switch {
	case strings.Contains(au, "enabled="):
		c.Status, c.Code, c.Params = "ok", "enabled", map[string]string{"name": kv(au)["enabled"]}
	case strings.Contains(au, "installed=unattended-upgrades"):
		if strings.Contains(au, `Unattended-Upgrade "1"`) {
			c.Status, c.Code, c.Params = "ok", "enabled", map[string]string{"name": "unattended-upgrades"}
		} else {
			c.Status, c.Code = "warn", "disabled"
		}
	case strings.Contains(family, "debian") || strings.Contains(family, "ubuntu"):
		c.Status, c.Code, c.Params = "warn", "missing", map[string]string{"name": "unattended-upgrades"}
	case strings.Contains(family, "rhel") || strings.Contains(family, "fedora") || strings.Contains(family, "centos"):
		c.Status, c.Code, c.Params = "warn", "missing", map[string]string{"name": "dnf-automatic"}
	default:
		c.Status, c.Code = "info", "unsupported"
	}
	add(c)

	// Public services.
	c = Check{ID: "publicServices", Weight: 10, Tab: "ports", Items: public, Params: map[string]string{"n": strconv.Itoa(len(public))}}
	switch {
	case len(public) == 0:
		c.Status, c.Code = "ok", "none"
	case active != "":
		c.Status, c.Code = "info", "filtered"
	default:
		c.Status, c.Code = "warn", "exposed"
	}
	add(c)

	// Extra UID 0 accounts.
	extra := []string{}
	for _, u := range lines(sec["uid0"]) {
		if u = strings.TrimSpace(u); u != "root" {
			extra = append(extra, u)
		}
	}
	c = Check{ID: "uid0", Weight: 15, Tab: "users", Items: extra}
	if len(extra) > 0 {
		c.Status, c.Code = "crit", "extra"
	} else {
		c.Status, c.Code = "ok", "none"
	}
	add(c)

	// Empty passwords.
	ep := lines(sec["emptypw"])
	c = Check{ID: "emptyPasswords", Weight: 15, Tab: "users"}
	switch {
	case len(ep) == 0 || ep[0] != "readable":
		c.Status, c.Code = "unknown", "needRoot"
	case len(ep) > 1:
		c.Status, c.Code, c.Items = "crit", "found", ep[1:]
	default:
		c.Status, c.Code = "ok", "none"
	}
	add(c)

	// World-writable files in /etc.
	ww := lines(sec["ww"])
	c = Check{ID: "worldWritable", Weight: 5, Items: ww}
	if len(ww) > 0 {
		c.Status, c.Code = "warn", "found"
	} else {
		c.Status, c.Code = "ok", "none"
	}
	add(c)

	// SSH port (information only).
	ps := []string{}
	for _, p := range sshPorts {
		ps = append(ps, strconv.Itoa(p))
	}
	c = Check{ID: "sshPort", Tab: "ssh", Params: map[string]string{"ports": strings.Join(ps, ", ")}}
	if slices.Contains(sshPorts, 22) {
		c.Status, c.Code = "info", "default"
	} else {
		c.Status, c.Code = "ok", "custom"
	}
	add(c)

	// Last logins.
	tz := parseTZ(strings.TrimSpace(sec["tz"]))
	ov.Logins = capSlice(parseLast(sec["last"], tz, time.Now()), 10)
	c = Check{ID: "lastLogins", Status: "info", Code: "summary", Tab: "events", Params: map[string]string{"n": strconv.Itoa(len(ov.Logins))}}
	add(c)

	// Score: ok = full weight, warn = half, crit = none; info/unknown ignored.
	var got, max float64
	for _, c := range ov.Checks {
		w := float64(c.Weight)
		switch c.Status {
		case "ok":
			got += w
			max += w
		case "warn":
			got += w / 2
			max += w
		case "crit":
			max += w
		}
	}
	if max > 0 {
		ov.Score = int(math.Round(got / max * 100))
	}
	return ov
}

// Overview runs the security checklist and computes a score.
func (s *SecurityService) Overview(connID, sudoPassword string) (Overview, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return Overview{}, err
	}
	ctx, cancel := core.Timeout(120 * time.Second)
	defer cancel()
	res, limited, err := s.rootOrUser(ctx, conn, overviewScript, sudoPassword, "")
	if err != nil {
		return Overview{}, err
	}
	return buildOverview(res.Stdout, limited), nil
}
