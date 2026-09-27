//go:build integration

package deploy

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

// events collects deploy:step events (the emit hook is process-wide).
var (
	evMu   sync.Mutex
	events []StepEvent
)

func init() {
	core.SetEmitHook(func(name string, data any) {
		if name == EventStep {
			evMu.Lock()
			events = append(events, data.(StepEvent))
			evMu.Unlock()
		}
	})
}

func stepEvents(runID string) []StepEvent {
	evMu.Lock()
	defer evMu.Unlock()
	out := []StepEvent{}
	for _, e := range events {
		if e.RunID == runID {
			out = append(out, e)
		}
	}
	return out
}

func sh(t *testing.T, c *core.Core, id, cmd string) string {
	t.Helper()
	conn, err := c.Conn(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := c.RunOK(ctx, conn, cmd, false, "", "")
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return res.Stdout
}

func shSudo(t *testing.T, c *core.Core, id, cmd string) string {
	t.Helper()
	conn, _ := c.Conn(id)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := c.Run(ctx, conn, cmd, true, "", "")
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return res.Stdout
}

func wait(t *testing.T, c *core.Core, st DeployStart) core.JobInfo {
	t.Helper()
	j, ok := c.Jobs.Get(st.JobID)
	if !ok {
		t.Fatal("job not found")
	}
	done := make(chan core.JobInfo, 1)
	go func() { done <- j.Wait() }()
	select {
	case info := <-done:
		return info
	case <-time.After(5 * time.Minute):
		t.Fatal("deployment timed out")
	}
	return core.JobInfo{}
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return apperr.From(err).Code
}

// ok panics (failing the test) on error.
func ok[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// commit writes files in the work clone and pushes to the bare repo.
func commit(t *testing.T, c *core.Core, id, base, msg string, files map[string]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("set -e; cd " + core.Q(base+"/work") + "\n")
	for f, content := range files {
		b.WriteString("printf '%s\\n' " + core.Q(content) + " > " + core.Q(f) + "\n")
	}
	b.WriteString("git add -A && git commit -qm " + core.Q(msg) + " && git push -q origin HEAD:main\n")
	sh(t, c, id, b.String())
}

const apiKey = "sk-live-TESTSECRET-9f8e7d6c5b4a"

func TestGitDeployFlow(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	base := fmt.Sprintf("/tmp/smtest-deploy-git-%d", rand.IntN(1e9))
	port := 18000 + rand.IntN(900)
	t.Cleanup(func() {
		shSudo(t, c, id, "if [ -f "+base+"/app/.pid ]; then kill $(cat "+base+"/app/.pid) 2>/dev/null; fi; rm -rf "+core.Q(base)+" "+lockPathFor(base+"/app"))
	})
	sh(t, c, id, `set -e; B=`+core.Q(base)+`
mkdir -p "$B"; git init -q --bare "$B/repo.git"; git --git-dir="$B/repo.git" symbolic-ref HEAD refs/heads/main
git init -q "$B/work"; cd "$B/work"; git config user.email t@example.com; git config user.name "Test Author"
git checkout -q -b main; git remote add origin "$B/repo.git"`)
	commit(t, c, id, base, "first release", map[string]string{"VERSION": "v1", "HEALTH": "healthy"})

	build := "cp VERSION build.txt\ncp HEALTH health.txt\necho \"building with key $API_KEY\"\necho \"b64 $(printf %s \"$API_KEY\" | base64)\""
	restart := fmt.Sprintf(`if [ -f .pid ]; then kill "$(cat .pid)" 2>/dev/null || true; sleep 0.5; fi
nohup python3 -m http.server %d --bind 127.0.0.1 >server.log 2>&1 </dev/null &
echo $! > .pid`, port)
	in := SaveAppInput{
		App: App{
			Name: "smtest-deploy-web", Stage: "staging", Type: TypeGit, Dir: base + "/app", Repo: base + "/repo.git", Branch: "main",
			LoadEnv: true, RestartSudo: true, BuildCmd: build, TestCmd: "test -s build.txt", RestartCmd: restart,
			Vars:         []EnvVar{{Name: "PORT", Value: fmt.Sprint(port)}, {Name: "GREETING", Value: "it's alive"}},
			Health:       HealthCheck{Type: "http", URL: fmt.Sprintf("http://127.0.0.1:%d/health.txt", port), ExpectStatus: 200, BodyContains: "healthy", Retries: 8, Interval: 1, Timeout: 3, Delay: 1},
			AutoRollback: true,
		},
		Secrets:     []SecretChange{{Name: "API_KEY", Value: apiKey}},
		TokenAction: "set", Token: "ghp_FAKEtoken1234567890",
	}
	// Invalid input is rejected.
	bad := in
	bad.App.Dir = "/etc"
	if _, err := s.SaveApp(id, bad); code(err) != "deploy.invalid" {
		t.Fatalf("bad dir: %v", err)
	}
	bad = in
	bad.App.Branch = "main..x"
	if _, err := s.SaveApp(id, bad); code(err) != "deploy.invalid" {
		t.Fatalf("bad branch: %v", err)
	}
	bad = in
	bad.Secrets = []SecretChange{{Name: "X-Y", Value: "abcdef"}}
	if _, err := s.SaveApp(id, bad); code(err) != "deploy.invalidEnvName" {
		t.Fatalf("bad env name: %v", err)
	}

	v := ok(s.SaveApp(id, in))
	app := v.App
	if !app.HasToken || len(app.Secrets) != 1 || app.Secrets[0] != "API_KEY" {
		t.Fatalf("saved app: %+v", app)
	}
	if _, err := s.Deploy(id, app.ID, DeployOptions{Ref: "bad ref"}, ""); code(err) != "deploy.invalidRef" {
		t.Fatalf("bad ref: %v", err)
	}

	// 1st deploy.
	st := ok(s.Deploy(id, app.ID, DeployOptions{}, ""))
	info := wait(t, c, st)
	if info.State != "done" {
		t.Fatalf("deploy 1: %s %v\n%s", info.State, info.Error, info.Log)
	}
	if strings.Contains(info.Log, apiKey) || strings.Contains(info.Log, "ghp_FAKEtoken") {
		t.Fatalf("secret leaked into job log:\n%s", info.Log)
	}
	if !strings.Contains(info.Log, "building with key "+Mask) || !strings.Contains(info.Log, "b64 "+Mask) {
		t.Fatalf("expected masked secret in log:\n%s", info.Log)
	}
	evs := stepEvents(st.RunID)
	seen := map[string]string{}
	for _, e := range evs {
		seen[e.Step] = e.State
	}
	for _, sid := range []string{"preflight", "fetch", "env", "build", "test", "restart", "health", "done"} {
		if seen[sid] != StateOK {
			t.Errorf("step %s final event state = %q", sid, seen[sid])
		}
	}
	d := ok(s.RunDetail(id, st.RunID))
	if d.Run.Status != "ok" || d.Run.Data.Commit == nil || d.Run.Data.Commit.Subject != "first release" || d.Run.Data.Commit.Author != "Test Author" || d.Run.Data.Branch != "main" {
		t.Fatalf("run 1: %+v", d.Run)
	}
	if strings.Contains(d.Log, apiKey) || !strings.Contains(d.Log, Mask) {
		t.Fatal("stored log not redacted")
	}
	sha1 := d.Run.Data.Commit.SHA
	envInfo := shSudo(t, c, id, "stat -c %a "+core.Q(base+"/app/.env")+"; cat "+core.Q(base+"/app/.env"))
	if !strings.HasPrefix(envInfo, "600\n") || !strings.Contains(envInfo, "API_KEY='"+apiKey+"'") || !strings.Contains(envInfo, `GREETING="it's alive"`) {
		t.Fatalf(".env: %s", envInfo)
	}
	// The lock is released.
	if out := sh(t, c, id, "test -e "+lockPathFor(base+"/app")+" && echo held || echo free"); strings.TrimSpace(out) != "free" {
		t.Fatal("lock not released")
	}

	// 2nd deploy: new commit.
	commit(t, c, id, base, "second release", map[string]string{"VERSION": "v2"})
	st2 := ok(s.Deploy(id, app.ID, DeployOptions{Ref: "main"}, ""))
	if info := wait(t, c, st2); info.State != "done" {
		t.Fatalf("deploy 2: %v\n%s", info.Error, info.Log)
	}
	d2 := ok(s.RunDetail(id, st2.RunID))
	if d2.Run.Data.PreviousTarget != sha1 || d2.Run.Data.Commit.Subject != "second release" {
		t.Fatalf("run 2: %+v", d2.Run.Data)
	}
	sha2 := d2.Run.Data.Commit.SHA
	if out := sh(t, c, id, "curl -s http://127.0.0.1:"+fmt.Sprint(port)+"/build.txt"); strings.TrimSpace(out) != "v2" {
		t.Fatalf("served %q", out)
	}

	// 3rd deploy: health check fails -> automatic rollback to v2.
	commit(t, c, id, base, "broken release", map[string]string{"VERSION": "v3", "HEALTH": "broken"})
	st3 := ok(s.Deploy(id, app.ID, DeployOptions{}, ""))
	info3 := wait(t, c, st3)
	if info3.State != "error" || info3.Error == nil || info3.Error.Code != "deploy.rolledBack" {
		t.Fatalf("deploy 3: %s %+v\n%s", info3.State, info3.Error, info3.Log)
	}
	runs := ok(s.Runs(id, app.ID, 10))
	if len(runs) != 4 {
		t.Fatalf("want 4 runs, got %d", len(runs))
	}
	rb, failed := runs[0], runs[1]
	if failed.ID != st3.RunID || failed.Status != "failed" || failed.Data.RolledBackBy != rb.ID || failed.Data.Error == nil || failed.Data.Error.Code != "deploy.healthFailed" {
		t.Fatalf("failed run: %+v", failed)
	}
	if rb.Status != "ok" || rb.Data.Trigger != TriggerAutoRollback || rb.Data.Target != sha2 || rb.Data.RollbackOf != st3.RunID {
		t.Fatalf("rollback run: %+v", rb)
	}
	for _, sidState := range failed.Data.Steps {
		if sidState.ID == "health" && sidState.State != StateFailed {
			t.Fatalf("health step state %s", sidState.State)
		}
	}
	if out := sh(t, c, id, "curl -s http://127.0.0.1:"+fmt.Sprint(port)+"/build.txt"); strings.TrimSpace(out) != "v2" {
		t.Fatalf("after auto-rollback served %q", out)
	}
	view := ok(s.GetApp(id, app.ID))
	if view.Current == nil || view.Current.Data.Target != sha2 || view.Last == nil || view.Last.ID != rb.ID {
		t.Fatalf("view: %+v", view)
	}

	// Manual rollback to the previous version (v1), then redeploy of v2 from history.
	st4 := ok(s.Rollback(id, app.ID, "", ""))
	if info := wait(t, c, st4); info.State != "done" {
		t.Fatalf("rollback: %v\n%s", info.Error, info.Log)
	}
	d4 := ok(s.RunDetail(id, st4.RunID))
	if d4.Run.Data.Target != sha1 || d4.Run.Data.Trigger != TriggerRollback {
		t.Fatalf("rollback run: %+v", d4.Run.Data)
	}
	if out := sh(t, c, id, "curl -s http://127.0.0.1:"+fmt.Sprint(port)+"/build.txt"); strings.TrimSpace(out) != "v1" {
		t.Fatalf("after rollback served %q", out)
	}
	st5 := ok(s.Deploy(id, app.ID, DeployOptions{RedeployOf: st2.RunID}, ""))
	if info := wait(t, c, st5); info.State != "done" {
		t.Fatalf("redeploy: %v\n%s", info.Error, info.Log)
	}
	if d5 := ok(s.RunDetail(id, st5.RunID)); d5.Run.Data.Target != sha2 || d5.Run.Data.Trigger != TriggerRedeploy {
		t.Fatalf("redeploy run: %+v", d5.Run.Data)
	}

	// Remote refs.
	refs := ok(s.RemoteRefs(id, app.ID, ""))
	if len(refs) == 0 || refs[0].Name != "main" {
		t.Fatalf("refs: %+v", refs)
	}

	// Audit & timeline.
	audit := ok(c.AuditList(core.AuditQuery{Server: id, Action: "deploy."}))
	actions := map[string]int{}
	for _, e := range audit {
		actions[e.Action]++
		if strings.Contains(e.Detail, apiKey) {
			t.Fatal("secret in audit")
		}
	}
	if actions["deploy.run"] < 4 || actions["deploy.rollback"] < 2 || actions["deploy.app.create"] != 1 || actions["deploy.secret.set"] != 1 {
		t.Fatalf("audit: %v", actions)
	}
	evts := ok(c.Events(core.EventQuery{Server: id, Kinds: []string{"deploy"}}))
	codes := map[string]int{}
	for _, e := range evts {
		codes[e.Code]++
	}
	if codes["deploy.ok"] < 3 || codes["deploy.failed"] != 1 || codes["deploy.rollback"] != 2 {
		t.Fatalf("timeline: %v", codes)
	}

	// Secret removal, then delete.
	in2 := SaveAppInput{App: app, Secrets: []SecretChange{{Name: "API_KEY", Remove: true}}, TokenAction: "remove"}
	v2 := ok(s.SaveApp(id, in2))
	if len(v2.App.Secrets) != 0 || v2.App.HasToken {
		t.Fatalf("after removal: %+v", v2.App)
	}
	if err := s.DeleteApp(id, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(id, app.ID); code(err) != "deploy.appNotFound" {
		t.Fatal("app still there")
	}
}

func TestComposeImageDeploy(t *testing.T) {
	c, id := testutil.Connect(t, testutil.DindPort())
	s := New(c)
	base := fmt.Sprintf("/tmp/smtest-deploy-img-%d", rand.IntN(1e9))
	port := 19000 + rand.IntN(900)
	name := fmt.Sprintf("smtest-deploy-c%d", rand.IntN(1e6))
	project := strings.ReplaceAll(name, ".", "-")
	t.Cleanup(func() {
		sh(t, c, id, "cd "+core.Q(base)+" 2>/dev/null && docker compose -p "+core.Q(project)+" -f compose.yml --env-file .env down -v --remove-orphans >/dev/null 2>&1; docker rm -f "+core.Q(name)+" >/dev/null 2>&1; rm -rf "+core.Q(base)+" "+lockPathFor(base)+"; true")
	})
	// Tags to release: two busybox versions from Docker Hub, or local images
	// built from whatever busybox is available if pulls fail.
	image, tag1, tag2, skipPull := "busybox", "1.36", "1.37", false
	if _, err := c.RunOK(context.Background(), ok(c.Conn(id)), "docker pull -q busybox:1.36 && docker pull -q busybox:1.37", false, "", ""); err != nil {
		t.Logf("pull failed (%v); building local images", err)
		sh(t, c, id, `set -e; b=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -m1 busybox); d=$(mktemp -d)
printf 'FROM %s\n' "$b" > "$d/Dockerfile"
docker build -q -t smtest-deploy-img:1 "$d"; docker build -q -t smtest-deploy-img:2 "$d"; rm -rf "$d"`)
		image, tag1, tag2, skipPull = "smtest-deploy-img", "1", "2", true
		t.Cleanup(func() { sh(t, c, id, "docker rmi smtest-deploy-img:1 smtest-deploy-img:2 >/dev/null 2>&1; true") })
	}
	in := SaveAppInput{App: App{
		Name: name, Type: TypeImage, Dir: base, Project: project, TagVar: "APP_TAG", DefaultTag: tag1, SkipPull: skipPull,
		Image: ImageSpec{Image: image, ContainerName: name, Ports: []string{fmt.Sprintf("127.0.0.1:%d:8080", port)}, Restart: "no",
			Command: "mkdir -p /www && echo ok-$APP_TAG > /www/index.html && httpd -f -p 8080 -h /www"},
		Vars:   []EnvVar{{Name: "MODE", Value: "test"}},
		Health: HealthCheck{Type: "http", URL: fmt.Sprintf("http://127.0.0.1:%d/", port), BodyContains: "ok-", Retries: 10, Interval: 1, Timeout: 3},
	}, Secrets: []SecretChange{{Name: "DB_PASSWORD", Value: "pw-SECRET-123456"}}}
	app := ok(s.SaveApp(id, in)).App

	deploy := func(tag string) DeployStart {
		st := ok(s.Deploy(id, app.ID, DeployOptions{Ref: tag}, ""))
		if info := wait(t, c, st); info.State != "done" {
			t.Fatalf("deploy %s: %v\n%s", tag, info.Error, info.Log)
		} else if strings.Contains(info.Log, "pw-SECRET-123456") {
			t.Fatal("secret leaked")
		}
		return st
	}
	imageOf := func() string {
		return strings.TrimSpace(sh(t, c, id, "docker inspect -f '{{.Config.Image}}' "+core.Q(name)))
	}
	st1 := deploy(tag1)
	if got := imageOf(); got != image+":"+tag1 {
		t.Fatalf("image %s", got)
	}
	out := sh(t, c, id, "stat -c %a "+core.Q(base+"/.env")+"; cat "+core.Q(base+"/.env")+"; head -1 "+core.Q(base+"/compose.yml"))
	if !strings.HasPrefix(out, "600\n") || !strings.Contains(out, "APP_TAG='"+tag1+"'") || !strings.Contains(out, "# Managed by Server Manager") {
		t.Fatalf("files: %s", out)
	}
	// The container sees env vars and secrets.
	if env := sh(t, c, id, "docker exec "+core.Q(name)+" env"); !strings.Contains(env, "DB_PASSWORD=pw-SECRET-123456") || !strings.Contains(env, "MODE=test") {
		t.Fatalf("container env: %s", env)
	}
	st2 := deploy(tag2)
	if got := imageOf(); got != image+":"+tag2 {
		t.Fatalf("image %s", got)
	}
	d2 := ok(s.RunDetail(id, st2.RunID))
	if d2.Run.Version != tag2 || d2.Run.Data.PreviousVersion != tag1 {
		t.Fatalf("run 2: %+v", d2.Run)
	}
	// Rollback to the previous tag.
	st3 := ok(s.Rollback(id, app.ID, "", ""))
	if info := wait(t, c, st3); info.State != "done" {
		t.Fatalf("rollback: %v\n%s", info.Error, info.Log)
	}
	if got := imageOf(); got != image+":"+tag1 {
		t.Fatalf("after rollback image %s", got)
	}
	d3 := ok(s.RunDetail(id, st3.RunID))
	if d3.Run.Data.RollbackOf != st1.RunID || d3.Run.Version != tag1 {
		t.Fatalf("rollback run: %+v", d3.Run)
	}
	if _, err := s.Deploy(id, app.ID, DeployOptions{Ref: "bad:tag"}, ""); code(err) != "deploy.invalidTag" {
		t.Fatalf("bad tag: %v", err)
	}
}

func TestConcurrentDeployLock(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	base := fmt.Sprintf("/tmp/smtest-deploy-lock-%d", rand.IntN(1e9))
	dir := base + "/app"
	t.Cleanup(func() { sh(t, c, id, "rm -rf "+core.Q(base)+" "+lockPathFor(dir)) })
	sh(t, c, id, `set -e; B=`+core.Q(base)+`
mkdir -p "$B"; git init -q --bare "$B/repo.git"; git --git-dir="$B/repo.git" symbolic-ref HEAD refs/heads/main
git init -q "$B/work"; cd "$B/work"; git config user.email t@example.com; git config user.name T
git checkout -q -b main; git remote add origin "$B/repo.git"; echo 1 > f; git add f; git commit -qm one; git push -q origin main`)
	in := SaveAppInput{App: App{Name: "smtest-deploy-lock", Type: TypeGit, Dir: dir, Repo: base + "/repo.git", BuildCmd: "sleep 4"}}
	app := ok(s.SaveApp(id, in)).App

	st := ok(s.Deploy(id, app.ID, DeployOptions{}, ""))
	// Same app instance: rejected immediately.
	if _, err := s.Deploy(id, app.ID, DeployOptions{}, ""); code(err) != "deploy.busy" {
		t.Fatalf("second deploy: %v", err)
	}
	if act := s.Active(id); len(act) != 1 || act[0].RunID != st.RunID {
		t.Fatalf("active: %+v", act)
	}
	// Another app instance (separate core/DB) deploying the same directory:
	// rejected by the remote lock once the first holds it.
	c2, id2 := testutil.Connect(t, testutil.FullPort())
	s2 := New(c2)
	app2 := ok(s2.SaveApp(id2, in)).App
	deadline := time.Now().Add(20 * time.Second)
	for {
		out := sh(t, c, id, "test -d "+lockPathFor(dir)+" && echo held || echo free")
		if strings.TrimSpace(out) == "held" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock never taken")
		}
		time.Sleep(200 * time.Millisecond)
	}
	st2 := ok(s2.Deploy(id2, app2.ID, DeployOptions{}, ""))
	info2 := wait(t, c2, st2)
	if info2.State != "error" || info2.Error.Code != "deploy.locked" {
		t.Fatalf("remote lock not enforced: %s %+v", info2.State, info2.Error)
	}
	if info := wait(t, c, st); info.State != "done" {
		t.Fatalf("first deploy: %v\n%s", info.Error, info.Log)
	}
	// A stale lock (no heartbeat for > 10 min) is broken.
	sh(t, c, id, "L="+lockPathFor(dir)+`; mkdir "$L" && printf 'token=x\nactor=ghost\nhost=gone\n' > "$L/info" && touch -d '20 minutes ago' "$L"`)
	st3 := ok(s2.Deploy(id2, app2.ID, DeployOptions{}, ""))
	info3 := wait(t, c2, st3)
	if info3.State != "done" || !strings.Contains(info3.Log, "stale deploy lock") {
		t.Fatalf("stale lock: %s %+v\n%s", info3.State, info3.Error, info3.Log)
	}
	// A fresh foreign lock can be released explicitly.
	sh(t, c, id, "L="+lockPathFor(dir)+`; mkdir "$L" && printf 'token=x\nactor=ghost\nhost=gone\n' > "$L/info"`)
	if err := s.ReleaseLock(id, app.ID, ""); err != nil {
		t.Fatal(err)
	}
	if out := sh(t, c, id, "test -e "+lockPathFor(dir)+" && echo held || echo free"); strings.TrimSpace(out) != "free" {
		t.Fatal("lock not released")
	}
}

func TestViewerDenied(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	s := New(c)
	in := SaveAppInput{App: App{Name: "smtest-deploy-viewer", Type: TypeGit, Dir: "/tmp/smtest-deploy-viewer", Repo: "https://example.com/r.git"}}
	app := ok(s.SaveApp(id, in)).App
	testutil.SetRole(t, c, id, "viewer")
	if _, err := s.SaveApp(id, in); code(err) != "access.denied" {
		t.Fatalf("save: %v", err)
	}
	if _, err := s.Deploy(id, app.ID, DeployOptions{}, ""); code(err) != "access.denied" {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := s.Rollback(id, app.ID, "", ""); code(err) != "access.denied" {
		t.Fatalf("rollback: %v", err)
	}
	if err := s.ReleaseLock(id, app.ID, ""); code(err) != "access.denied" {
		t.Fatalf("release: %v", err)
	}
	if err := s.DeleteApp(id, app.ID); code(err) != "access.denied" {
		t.Fatalf("delete: %v", err)
	}
	// Reading is allowed.
	if apps := ok(s.Apps(id)); len(apps) != 1 {
		t.Fatalf("apps: %+v", apps)
	}
}
