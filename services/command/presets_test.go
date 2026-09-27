package command

import (
	"strings"
	"testing"
	"time"

	"server-manager/internal/apperr"
)

func TestRenderPresetsAll(t *testing.T) {
	for _, p := range presets {
		params := map[string]string{}
		for _, def := range p.Params {
			if def.Kind == "unit" {
				params[def.Name] = "nginx"
			}
		}
		r, err := RenderPreset(p.ID, params)
		if err != nil {
			t.Errorf("%s: %v", p.ID, err)
			continue
		}
		if strings.TrimSpace(r.Command) == "" {
			t.Errorf("%s: empty command", p.ID)
		}
		if r.Sudo != p.Sudo && p.ID != "updates" {
			t.Errorf("%s: sudo %v, want %v", p.ID, r.Sudo, p.Sudo)
		}
	}
}

func TestRenderServices(t *testing.T) {
	r, err := RenderPreset("service.restart", map[string]string{"unit": "php8.2-fpm.service"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Command, "systemctl restart 'php8.2-fpm.service'") || !r.Sudo {
		t.Errorf("unexpected: %+v", r)
	}
	if !strings.Contains(r.Command, "rc-service 'php8.2-fpm.service' restart") {
		t.Errorf("OpenRC fallback missing: %s", r.Command)
	}
	r, _ = RenderPreset("service.status", map[string]string{"unit": "getty@tty1.service"})
	if !strings.Contains(r.Command, "systemctl status --no-pager -l 'getty@tty1.service'") || r.Sudo {
		t.Errorf("unexpected: %+v", r)
	}
	// Stopping ssh through a preset is flagged by the danger analysis.
	r, _ = RenderPreset("service.stop", map[string]string{"unit": "ssh"})
	if !hasCode(Analyze(r.Command), "sshStop") {
		t.Errorf("stopping ssh via preset not flagged: %v", codes(Analyze(r.Command)))
	}
}

func TestRenderInvalid(t *testing.T) {
	bad := []struct {
		id     string
		params map[string]string
		code   string
	}{
		{"service.restart", map[string]string{}, "cmd.paramRequired"},
		{"service.restart", map[string]string{"unit": "nginx; rm -rf /"}, "cmd.invalidUnit"},
		{"service.restart", map[string]string{"unit": "-nginx"}, "cmd.invalidUnit"},
		{"service.restart", map[string]string{"unit": "$(id)"}, "cmd.invalidUnit"},
		{"service.restart", map[string]string{"unit": "a b"}, "cmd.invalidUnit"},
		{"service.restart", map[string]string{"unit": strings.Repeat("a", 200)}, "cmd.invalidUnit"},
		{"service.restart", map[string]string{"unit": "nginx", "extra": "1"}, "cmd.invalidParam"},
		{"top", map[string]string{"count": "0"}, "cmd.invalidParam"},
		{"top", map[string]string{"count": "101"}, "cmd.invalidParam"},
		{"top", map[string]string{"count": "5;id"}, "cmd.invalidParam"},
		{"top", map[string]string{"sort": "disk"}, "cmd.invalidParam"},
		{"docker.ps", map[string]string{"all": "maybe"}, "cmd.invalidParam"},
		{"nope", nil, "cmd.unknownPreset"},
	}
	for _, b := range bad {
		_, err := RenderPreset(b.id, b.params)
		if !apperr.HasCode(err, b.code) {
			t.Errorf("RenderPreset(%s, %v) = %v, want %s", b.id, b.params, err, b.code)
		}
	}
}

func TestRenderParams(t *testing.T) {
	r, err := RenderPreset("top", map[string]string{"sort": "mem", "count": "5"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Command, "--sort=-%mem | head -n 6") {
		t.Errorf("top: %s", r.Command)
	}
	r, _ = RenderPreset("docker.ps", map[string]string{"all": "true"})
	if !strings.Contains(r.Command, "docker ps -a --format") {
		t.Errorf("docker.ps: %s", r.Command)
	}
	r, _ = RenderPreset("updates", map[string]string{"refresh": "true"})
	if !r.Sudo || !strings.Contains(r.Command, "apt-get update") {
		t.Errorf("updates refresh: %+v", r)
	}
	r, _ = RenderPreset("updates", nil)
	if r.Sudo || strings.Contains(r.Command, "apt-get update") {
		t.Errorf("updates without refresh: %+v", r)
	}
	r, _ = RenderPreset("reboot", nil)
	if !r.Danger || !r.Sudo || !hasCode(Analyze(r.Command), "power") {
		t.Errorf("reboot: %+v %v", r, codes(Analyze(r.Command)))
	}
	r, _ = RenderPreset("disk", nil)
	if r.Command != "df -hP" {
		t.Errorf("disk: %q", r.Command)
	}
	r, _ = RenderPreset("memory", nil)
	if r.Command != "free -m" {
		t.Errorf("memory: %q", r.Command)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	out, tr := truncateUTF8(s, 5)
	if !tr || out != "éé" {
		t.Errorf("got %q %v", out, tr)
	}
	out, tr = truncateUTF8("abc", 5)
	if tr || out != "abc" {
		t.Errorf("got %q %v", out, tr)
	}
}

func TestWrapScript(t *testing.T) {
	w := wrapScript("echo 'hi'", 30, "smcmd-abc")
	if !strings.Contains(w, `setsid sh -c 'echo '\''hi'\''' 'smcmd-abc' &`) || !strings.Contains(w, "-lt 30 ]") {
		t.Errorf("wrap: %s", w)
	}
	k := killScript("smcmd-abc")
	if !strings.Contains(k, "grep -q -- 'smcm[d]-abc'") || strings.Contains(k, "smcmd-abc") {
		t.Errorf("kill script must not match itself: %s", k)
	}
}

func TestRemoteTimedOut(t *testing.T) {
	if !remoteTimedOut(124, 10*time.Second, 10*time.Second) {
		t.Error("124 at the deadline is a timeout")
	}
	if remoteTimedOut(124, 2*time.Second, 10*time.Second) {
		t.Error("124 before the deadline is the command's own exit code")
	}
	if remoteTimedOut(1, 10*time.Second, 10*time.Second) {
		t.Error("exit 1 is not a timeout")
	}
}

func TestSummarize(t *testing.T) {
	s := summarize([]ServerResult{{State: StateDone}, {State: StateFailed}, {State: StateTimeout}, {State: StateRunning}, {State: StateConnecting}, {State: StateSkipped}, {State: StateQueued}, {State: StateCancelled}})
	if s.Total != 8 || s.OK != 1 || s.Failed != 1 || s.Timeout != 1 || s.Running != 2 || s.Skipped != 1 || s.Queued != 1 || s.Cancelled != 1 {
		t.Errorf("summary: %+v", s)
	}
}
