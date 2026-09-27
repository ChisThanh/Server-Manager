package deploy

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"testing"
)

const secret = "s3cr3t-P@ss/w0rd+x"

func TestRedactVariants(t *testing.T) {
	r := NewRedactor(secret, "tok_ABCDEFGH12345")
	cases := []string{
		secret,
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
		url.QueryEscape(secret),
		hex.EncodeToString([]byte(secret)),
		`s3cr3t-P@ss\/w0rd+x`, // not JSON-escaped by Go, but the raw form is inside
		"tok_ABCDEFGH12345",
	}
	for _, c := range cases {
		in := "before " + c + " after"
		out := r.Redact(in)
		if strings.Contains(out, c) && c != `s3cr3t-P@ss\/w0rd+x` {
			t.Errorf("not redacted: %q -> %q", in, out)
		}
		if !strings.HasPrefix(out, "before ") || !strings.HasSuffix(out, " after") {
			t.Errorf("surroundings damaged: %q", out)
		}
	}
	// Secret inside a larger base64 blob (Basic auth "user:secret"), at any alignment.
	for _, prefix := range []string{"u:", "us:", "use:", "user:"} {
		blob := base64.StdEncoding.EncodeToString([]byte(prefix + secret + "!"))
		out := r.Redact("Authorization: Basic " + blob)
		if strings.Contains(out, blob) {
			t.Errorf("embedded base64 not redacted (prefix %q): %s", prefix, out)
		}
	}
	// Short values are not patterns; unrelated text untouched.
	if got := NewRedactor("ab").Redact("abcabc"); got != "abcabc" {
		t.Errorf("short secret redacted: %q", got)
	}
	if got := r.Redact("nothing to see"); got != "nothing to see" {
		t.Errorf("clean text changed: %q", got)
	}
}

func TestRedactWriterChunks(t *testing.T) {
	r := NewRedactor(secret)
	text := "line1\npassword=" + secret + "\nend " + secret + secret + "\ntail"
	// Every possible split into two writes, and byte-by-byte.
	for cut := 0; cut <= len(text); cut++ {
		var buf bytes.Buffer
		w := r.Writer(&buf)
		w.Write([]byte(text[:cut]))
		w.Write([]byte(text[cut:]))
		w.Flush()
		if strings.Contains(buf.String(), secret) || strings.Contains(buf.String(), "s3cr3t-P@") {
			t.Fatalf("cut %d leaked: %q", cut, buf.String())
		}
		want := r.Redact(text)
		if buf.String() != want {
			t.Fatalf("cut %d: got %q want %q", cut, buf.String(), want)
		}
	}
	var buf bytes.Buffer
	w := r.Writer(&buf)
	for i := 0; i < len(text); i++ {
		w.Write([]byte{text[i]})
	}
	w.Flush()
	if buf.String() != r.Redact(text) {
		t.Fatalf("byte-by-byte: %q", buf.String())
	}
	// A dangling prefix at the end of the stream is masked on flush.
	buf.Reset()
	w = r.Writer(&buf)
	w.Write([]byte("x " + secret[:8]))
	w.Flush()
	if strings.Contains(buf.String(), secret[:8]) {
		t.Fatalf("dangling prefix leaked: %q", buf.String())
	}
}

func TestRedactWriterConcurrent(t *testing.T) {
	r := NewRedactor(secret)
	var mu sync.Mutex
	var buf bytes.Buffer
	sink := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) })
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.Writer(sink)
			for j := 0; j < 200; j++ {
				w.Write([]byte("x" + secret + "\n"))
			}
			w.Flush()
		}()
	}
	wg.Wait()
	if strings.Contains(buf.String(), secret) {
		t.Fatal("leak under concurrency")
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestRenderEnv(t *testing.T) {
	out, err := renderEnv(
		[]EnvVar{{Name: "PORT", Value: "8080"}, {Name: "GREETING", Value: "it's $HOME"}, {Name: "EMPTY", Value: ""}},
		[]EnvVar{{Name: "DB_PASS", Value: `p"a$s\w`}},
		[]EnvVar{{Name: "APP_TAG", Value: "1.2.3"}},
	)
	if err == nil {
		t.Fatal("value with ' and $ must be rejected")
	}
	out, err = renderEnv(
		[]EnvVar{{Name: "PORT", Value: "8080"}, {Name: "GREETING", Value: "it's fine"}, {Name: "EMPTY", Value: ""}, {Name: "SP", Value: " a b # c "}},
		[]EnvVar{{Name: "DB_PASS", Value: `p"a$s\w`}},
		[]EnvVar{{Name: "APP_TAG", Value: "1.2.3"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PORT='8080'\n",
		`GREETING="it's fine"` + "\n",
		"EMPTY=''\n",
		"SP=' a b # c '\n",
		`DB_PASS='p"a$s\w'` + "\n",
		"APP_TAG='1.2.3'\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "# Managed by Server Manager") {
		t.Error("missing header")
	}
	for _, bad := range []string{"a\nb", "x\x00", "'\"", "it's $X", "it's `x`", `it's \n`} {
		if _, ok := quoteEnv(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := renderEnv([]EnvVar{{Name: "1BAD", Value: "x"}}, nil, nil); err == nil {
		t.Error("bad name accepted")
	}
	if v := parseEnvValue(out, "APP_TAG"); v != "1.2.3" {
		t.Errorf("parseEnvValue = %q", v)
	}
	if v := parseEnvValue("export APP_TAG=v2 # comment\n", "APP_TAG"); v != "v2" {
		t.Errorf("parseEnvValue unquoted = %q", v)
	}
}

func TestValidRef(t *testing.T) {
	good := []string{"main", "release/1.2", "v1.0.0", "feature/x-y_z", "abc1234", "HEAD"}
	bad := []string{"", "-rf", "/main", "main/", "a..b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`, "a.lock", "x/.hidden", "a@{1}", "@", "a//b", "main.", "ref\n"}
	for _, r := range good {
		if !ValidRef(r) {
			t.Errorf("rejected %q", r)
		}
	}
	for _, r := range bad {
		if ValidRef(r) {
			t.Errorf("accepted %q", r)
		}
	}
}

func TestValidators(t *testing.T) {
	repos := map[string]bool{
		"https://github.com/org/repo.git":         true,
		"ssh://git@github.com:22/org/repo.git":    true,
		"git@github.com:org/repo.git":             true,
		"/srv/git/repo.git":                       true,
		"file:///srv/git/repo.git":                true,
		"https://user:pass@github.com/org/r.git":  false,
		"-uhttps://x":                             false,
		"https://github.com/org/repo.git; rm -rf": false,
		"ftp://x/y":                    false,
		"https://x/'y":                 false,
		"ext::sh -c touch% /tmp/pwned": false,
		"ext::sh":                      false,
		"a@-oProxyCommand=x:y":         false,
		"a@-oProxyCommand:y":           false,
		"git@host:-u/x":                false,
	}
	for u, want := range repos {
		if ValidRepoURL(u) != want {
			t.Errorf("ValidRepoURL(%q) = %v", u, !want)
		}
	}
	images := map[string]bool{"nginx": true, "ghcr.io/org/app": true, "localhost:5000/a/b": true, "Nginx": false, "nginx:1.2": false, "a b": false, "": false}
	for s, want := range images {
		if ValidImage(s) != want {
			t.Errorf("ValidImage(%q) = %v", s, !want)
		}
	}
	tags := map[string]bool{"1.2.3": true, "latest": true, "v1_rc-2": true, ".x": false, "a:b": false, strings.Repeat("a", 129): false}
	for s, want := range tags {
		if ValidTag(s) != want {
			t.Errorf("ValidTag(%q) = %v", s, !want)
		}
	}
	envs := map[string]bool{"A": true, "_X1": true, "DB_URL": true, "1A": false, "A-B": false, "A B": false, "": false}
	for s, want := range envs {
		if ValidEnvName(s) != want {
			t.Errorf("ValidEnvName(%q) = %v", s, !want)
		}
	}
	dirs := map[string]bool{"/srv/app": true, "/opt/my-app": true, "/": false, "/etc": false, "/srv": false, "srv/app": false, "/srv/../etc": false, "/srv/a b": false, "/srv/$(x)": false}
	for s, want := range dirs {
		if validDir(s) != want {
			t.Errorf("validDir(%q) = %v", s, !want)
		}
	}
	ports := map[string]bool{"8080:80": true, "127.0.0.1:8080:80/tcp": true, "80": true, "8000-8010:8000-8010": true, "a:80": false, "80:80/icmp": false}
	for s, want := range ports {
		if rePort.MatchString(s) != want {
			t.Errorf("port %q = %v", s, !want)
		}
	}
	vols := map[string]bool{"./data:/data": true, "data:/var/lib/x:ro": true, "/srv/x:/x": true, "../x:/x": false, "x:/y:bad": false, "x": false}
	for s, want := range vols {
		if validVolume(s) != want {
			t.Errorf("volume %q = %v", s, !want)
		}
	}
}

func TestNormalizeApp(t *testing.T) {
	a := App{Name: " web ", Type: TypeGit, Dir: "/srv/web/", Repo: "https://example.com/r.git",
		Vars: []EnvVar{{Name: "A", Value: "1"}, {Name: "", Value: ""}}, Secrets: []string{"B"}}
	if err := a.normalize(); err != nil {
		t.Fatal(err)
	}
	if a.Name != "web" || a.Dir != "/srv/web" || a.Branch != "main" || a.Stage != "production" || len(a.Vars) != 1 || a.Health.Type != "none" {
		t.Fatalf("unexpected normalization: %+v", a)
	}
	if a.envPath() != "/srv/web/.env" {
		t.Fatal(a.envPath())
	}
	dup := a
	dup.Vars = []EnvVar{{Name: "B", Value: "x"}}
	if err := dup.normalize(); err == nil {
		t.Fatal("var/secret name clash accepted")
	}
	img := App{Name: "x", Type: TypeImage, Dir: "/srv/x", Image: ImageSpec{Image: "nginx", Ports: []string{"8080:80"}}}
	if err := img.normalize(); err != nil {
		t.Fatal(err)
	}
	if img.TagVar != "APP_TAG" || img.ComposeFile != "compose.yml" || img.Image.Restart != "unless-stopped" {
		t.Fatalf("%+v", img)
	}
	clash := img
	clash.Vars = []EnvVar{{Name: "APP_TAG", Value: "x"}}
	if err := clash.normalize(); err == nil {
		t.Fatal("tag var clash accepted")
	}
	for _, envFile := range []string{"/etc/passwd", "/etc/ssh/sshd_config", "/root/.ssh/authorized_keys", "/home/u/.ssh/x", "/usr/bin/env", "/etc/foo"} {
		e := App{Name: "x", Type: TypeGit, Dir: "/srv/x", Repo: "/srv/r.git", EnvFile: envFile}
		if err := e.normalize(); err == nil {
			t.Errorf("env file %s accepted", envFile)
		}
	}
	for _, envFile := range []string{".env", "config/app.env", "/etc/myapp/env", "/etc/default/myapp", "/etc/myapp.env"} {
		e := App{Name: "x", Type: TypeGit, Dir: "/srv/x", Repo: "/srv/r.git", EnvFile: envFile}
		if err := e.normalize(); err != nil {
			t.Errorf("env file %s rejected: %v", envFile, err)
		}
	}
	h := App{Name: "x", Type: TypeGit, Dir: "/srv/x", Repo: "/srv/r.git", Health: HealthCheck{Type: "http", URL: "ftp://x"}}
	if err := h.normalize(); err == nil {
		t.Fatal("bad health url accepted")
	}
}

func TestRenderCompose(t *testing.T) {
	a := App{Name: "x", Type: TypeImage, Dir: "/srv/x", Image: ImageSpec{
		Image: "ghcr.io/org/app", ContainerName: "smx", Ports: []string{"8080:80"}, Volumes: []string{"data:/data", "./conf:/conf:ro"},
		Command: "echo $HOME && sleep 1", HealthCmd: "wget -q -O- localhost || exit 1", HealthInterval: 10, HealthRetries: 3,
	}}
	if err := a.normalize(); err != nil {
		t.Fatal(err)
	}
	out := renderCompose(&a)
	for _, want := range []string{
		`image: "ghcr.io/org/app:${APP_TAG:?APP_TAG is not set}"`,
		`container_name: "smx"`,
		`restart: "unless-stopped"`,
		`- "/srv/x/.env"`,
		`command: ["sh", "-c", "echo $$HOME \u0026\u0026 sleep 1"]`,
		`- "8080:80"`,
		`test: ["CMD-SHELL", "wget -q -O- localhost || exit 1"]`,
		"interval: 10s",
		`"data": {}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
}

func TestParseLsRemote(t *testing.T) {
	out := "aaa\trefs/heads/main\nbbb\trefs/heads/dev\nccc\trefs/tags/v1\nddd\trefs/tags/v1^{}\neee\trefs/pull/1/head\n"
	refs := parseLsRemote(out)
	if len(refs) != 3 || refs[0].Name != "dev" || refs[2].Name != "v1" || refs[2].SHA != "ddd" {
		t.Fatalf("%+v", refs)
	}
}
