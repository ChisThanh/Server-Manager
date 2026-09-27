//go:build integration

// Integration tests against the Docker-in-Docker test server
// (testutil.DindPort). Run: go test -tags integration -race -count=1 ./services/docker/ -v
//
// The daemon has no network access to registries, so a tiny image is built
// with `docker import` from the server's own busybox + musl loader. Every
// object created is named smtest-docker-* and removed at the end.
package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

const testImage = "smtest-docker-base:1"

type env struct {
	t  *testing.T
	s  *DockerService
	c  *core.Core
	id string
}

// sh runs a setup command on the server (sudo with the login password).
func (e *env) sh(cmd string, sudo bool) string {
	e.t.Helper()
	conn, err := e.c.Conn(e.id)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := e.c.Run(ctx, conn, cmd, sudo, testutil.Password, "")
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	if res.ExitCode != 0 {
		e.t.Fatalf("%s: exit %d: %s%s", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res.Stdout
}

func (e *env) wait(jobID string, err error) core.JobInfo {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("start job: %v", err)
	}
	j, ok := e.c.Jobs.Get(jobID)
	if !ok {
		e.t.Fatalf("job %s not found", jobID)
	}
	done := make(chan core.JobInfo, 1)
	go func() { done <- j.Wait() }()
	select {
	case info := <-done:
		return info
	case <-time.After(3 * time.Minute):
		e.t.Fatalf("job %s timed out", jobID)
	}
	return core.JobInfo{}
}

func (e *env) mustDone(jobID string, err error) core.JobInfo {
	e.t.Helper()
	info := e.wait(jobID, err)
	if info.State != "done" {
		e.t.Fatalf("job %s: state %s err %v\n%s", info.Kind, info.State, info.Error, info.Log)
	}
	return info
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return apperr.From(err).Code
}

func expectCode(t *testing.T, err error, want string) {
	t.Helper()
	if code(err) != want {
		t.Fatalf("want error %s, got %v", want, err)
	}
}

func (e *env) container(name string) *Container {
	e.t.Helper()
	list, err := e.s.Containers(e.id, "")
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

func (e *env) state(name string) string {
	e.t.Helper()
	if c := e.container(name); c != nil {
		return c.State
	}
	return "absent"
}

const buildImage = `set -e
d=$(mktemp -d /tmp/smtest-docker-rootfs.XXXXXX)
mkdir -p "$d/bin" "$d/lib" "$d/tmp" "$d/etc" "$d/data"
cp /bin/busybox "$d/bin/"; cp /lib/ld-musl-*.so.1 "$d/lib/"
for a in sh sleep echo cat ls env true; do ln -s busybox "$d/bin/$a"; done
echo "$1" > "$d/etc/smtest"
tar -C "$d" -c . | docker import -c 'CMD ["/bin/sh"]' - "$2" >/dev/null
rm -rf "$d"`

func (e *env) cleanup() {
	conn, err := e.c.Conn(e.id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	script := `
for p in $(docker compose ls -a -q 2>/dev/null | grep '^smtest-docker'); do docker compose -p "$p" down -v --remove-orphans >/dev/null 2>&1; done
c=$(docker ps -aq --filter name=smtest-docker); [ -n "$c" ] && docker rm -f -v $c >/dev/null
v=$(docker volume ls -q | grep '^smtest-docker'); [ -n "$v" ] && docker volume rm $v >/dev/null
n=$(docker network ls --format '{{.Name}}' | grep '^smtest-docker'); [ -n "$n" ] && docker network rm $n >/dev/null
i=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^smtest-docker'); [ -n "$i" ] && docker rmi -f $i >/dev/null
docker image prune -f >/dev/null
rm -rf /tmp/smtest-docker-*
true`
	_, _ = e.c.Run(ctx, conn, script, false, "", "")
	_, _ = e.c.Run(ctx, conn, "rm -rf /tmp/smtest-docker-*", true, testutil.Password, "")
}

func TestDocker(t *testing.T) {
	c, id := testutil.Connect(t, testutil.DindPort())
	e := &env{t: t, s: New(c), c: c, id: id}
	e.cleanup()
	t.Cleanup(e.cleanup)
	e.sh(`set -- v1 `+testImage+"\n"+buildImage, false)

	t.Run("status", func(t *testing.T) {
		st, err := e.s.Status(id, "")
		if err != nil {
			t.Fatal(err)
		}
		if !st.Installed || !st.DaemonRunning || st.NeedSudo || st.Compose != "plugin" || st.ServerVersion == "" || st.ComposeVersion == "" {
			t.Fatalf("unexpected status %+v", st)
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		for _, n := range []string{"-rf", "a;b", "a b", "$(id)", "", "../x"} {
			expectCode(t, e.s.ContainerAction(id, n, "stop", ""), "docker.invalidName")
			expectCode(t, e.s.RemoveVolume(id, n, ""), "docker.invalidName")
			expectCode(t, e.s.RemoveNetwork(id, n, ""), "docker.invalidName")
		}
		expectCode(t, e.s.ContainerAction(id, "smtest-docker-x", "rm -rf", ""), "docker.invalidAction")
		_, err := e.s.PullImage(id, "Bad Ref!", "")
		expectCode(t, err, "docker.invalidName")
		_, err = e.s.PullImage(id, "--help", "")
		expectCode(t, err, "docker.invalidName")
		_, err = e.s.ContainerLogs(id, "smtest-docker-x", 10, "1 day", false, "")
		expectCode(t, err, "docker.invalidSince")
		for _, p := range []string{"relative/compose.yaml", "/tmp/../etc/passwd", "/tmp/a,b.yml", "/tmp/x'y", "/", "/tmp/-x"} {
			_, err = e.s.ReadComposeFile(id, p, "")
			expectCode(t, err, "docker.invalidPath")
		}
		_, err = e.s.ComposeAction(id, "Bad Project", "up", ComposeActionOptions{}, "")
		expectCode(t, err, "docker.invalidName")
		_, err = e.s.ComposeAction(id, "smtest-docker-x", "rm", ComposeActionOptions{}, "")
		expectCode(t, err, "docker.invalidAction")
		expectCode(t, e.s.RemoveNetwork(id, "bridge", ""), "docker.builtinNetwork")
		_, err = e.s.DeployCompose(id, DeployRequest{Spec: ComposeSpec{Project: "UPPER", Path: "/tmp/smtest-docker-x/compose.yaml"}, Content: "services: {}", Kind: "new"}, "")
		expectCode(t, err, "docker.invalidName")
	})

	t.Run("container lifecycle", func(t *testing.T) {
		name := "smtest-docker-c1"
		e.sh(fmt.Sprintf(`docker run -d --name %s --label smtest=1 -e API_TOKEN=s3cr3t -e PLAIN=x -p 18081:80 --restart unless-stopped %s sh -c 'echo hello-from-c1; exec sleep 3600' >/dev/null`, name, testImage), false)
		c1 := e.container(name)
		if c1 == nil || c1.State != "running" || c1.Image != testImage || len(c1.ID) != 64 || c1.Created == 0 || !strings.Contains(c1.Ports, "18081") {
			t.Fatalf("container row %+v", c1)
		}
		stats, err := e.s.Stats(id, "")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, st := range stats {
			if st.Name == name {
				found = true
				if st.MemUsage <= 0 || st.MemLimit <= 0 || st.PIDs < 1 || st.ID != c1.ID {
					t.Fatalf("stats %+v", st)
				}
			}
		}
		if !found {
			t.Fatalf("no stats for %s: %+v", name, stats)
		}

		logs, err := e.s.ContainerLogs(id, name, 50, "1h", true, "")
		if err != nil || !strings.Contains(logs, "hello-from-c1") {
			t.Fatalf("logs %q %v", logs, err)
		}

		d, err := e.s.ContainerInspect(id, name, "")
		if err != nil {
			t.Fatal(err)
		}
		if d.Name != name || d.RestartPolicy != "unless-stopped" || d.State != "running" || !strings.Contains(d.Raw, `"Id"`) {
			t.Fatalf("inspect %+v", d)
		}
		var tok, plain *EnvVar
		for i := range d.Env {
			switch d.Env[i].Key {
			case "API_TOKEN":
				tok = &d.Env[i]
			case "PLAIN":
				plain = &d.Env[i]
			}
		}
		if tok == nil || !tok.Secret || tok.Value != "s3cr3t" || plain == nil || plain.Secret {
			t.Fatalf("env %+v", d.Env)
		}
		okPort := false
		for _, p := range d.Ports {
			if p.Container == "80/tcp" && p.HostPort == "18081" {
				okPort = true
			}
		}
		if !okPort || len(d.Networks) == 0 || d.Networks[0].IP == "" {
			t.Fatalf("ports %+v networks %+v", d.Ports, d.Networks)
		}
		_, err = e.s.ContainerInspect(id, "smtest-docker-missing", "")
		expectCode(t, err, "docker.notFound")

		steps := []struct{ action, want string }{
			{"stop", "exited"}, {"start", "running"}, {"pause", "paused"}, {"unpause", "running"}, {"restart", "running"}, {"kill", "exited"},
		}
		for _, st := range steps {
			if err := e.s.ContainerAction(id, name, st.action, ""); err != nil {
				t.Fatalf("%s: %v", st.action, err)
			}
			if got := e.state(name); got != st.want {
				t.Fatalf("after %s: state %s, want %s", st.action, got, st.want)
			}
		}
		// Recreate refuses a container that isn't Compose-managed.
		_, err = e.s.RecreateContainer(id, name, false, "")
		expectCode(t, err, "docker.notCompose")
		// Errors of the CLI come back coded.
		expectCode(t, e.s.ContainerAction(id, "smtest-docker-missing", "start", ""), "docker.cmdFailed")

		if err := e.s.ContainerAction(id, name, "start", ""); err != nil {
			t.Fatal(err)
		}
		if err := e.s.RemoveContainer(id, name, true, true, ""); err != nil {
			t.Fatal(err)
		}
		if got := e.state(name); got != "absent" {
			t.Fatalf("after remove: %s", got)
		}
		audits, _ := c.AuditList(core.AuditQuery{Server: id, Action: "docker.container."})
		seen := map[string]bool{}
		for _, a := range audits {
			seen[a.Action] = true
		}
		for _, a := range []string{"docker.container.stop", "docker.container.kill", "docker.container.remove", "docker.container.pause"} {
			if !seen[a] {
				t.Fatalf("missing audit %s (have %v)", a, seen)
			}
		}
	})

	t.Run("images", func(t *testing.T) {
		e.sh(fmt.Sprintf("docker run -d --name smtest-docker-img-user %s sleep 3600 >/dev/null", testImage), false)
		list, err := e.s.Images(id, "")
		if err != nil {
			t.Fatal(err)
		}
		var base *Image
		for i := range list {
			if list[i].Repository == "smtest-docker-base" && list[i].Tag == "1" {
				base = &list[i]
			}
		}
		if base == nil || base.Size <= 0 || base.Created == 0 || !strings.HasPrefix(base.ID, "sha256:") || len(base.UsedBy) != 1 || base.UsedBy[0] != "smtest-docker-img-user" {
			t.Fatalf("image %+v", base)
		}
		expectCode(t, e.s.RemoveImage(id, testImage, false, ""), "docker.imageInUse")
		expectCode(t, e.s.RemoveImage(id, "smtest-docker-nope:1", false, ""), "docker.notFound")

		// A second tag can be removed while the first stays.
		e.sh("docker tag "+testImage+" smtest-docker-base:tmp", false)
		if err := e.s.RemoveImage(id, "smtest-docker-base:tmp", false, ""); err != nil {
			t.Fatal(err)
		}
		// Dangling image: re-import the same tag with other content.
		e.sh(`set -- d1 smtest-docker-dangle:1`+"\n"+buildImage, false)
		e.sh(`set -- d2 smtest-docker-dangle:1`+"\n"+buildImage, false)
		list, _ = e.s.Images(id, "")
		dangling := 0
		for _, im := range list {
			if im.Dangling {
				dangling++
			}
		}
		if dangling == 0 {
			t.Fatalf("expected a dangling image: %+v", list)
		}
		r, err := e.s.PruneImages(id, false, "")
		if err != nil || r.Reclaimed <= 0 {
			t.Fatalf("prune %+v %v", r, err)
		}
		if err := e.s.RemoveImage(id, "smtest-docker-dangle:1", false, ""); err != nil {
			t.Fatal(err)
		}
		// Pull runs as a job; the test daemon may have no registry access,
		// so only check that it ends and is audited.
		info := e.wait(e.s.PullImage(id, "busybox:1.36", ""))
		if info.State != "done" && info.State != "error" {
			t.Fatalf("pull state %s", info.State)
		}
		if info.State == "done" {
			e.sh("docker rmi busybox:1.36 >/dev/null", false)
		}
		e.sh("docker rm -f smtest-docker-img-user >/dev/null", false)
	})

	t.Run("volumes", func(t *testing.T) {
		e.sh("docker volume create smtest-docker-v1 >/dev/null && docker volume create smtest-docker-v2 >/dev/null", false)
		e.sh(fmt.Sprintf("docker run -d --name smtest-docker-vol-user -v smtest-docker-v2:/data %s sh -c 'echo x > /data/f; exec sleep 3600' >/dev/null", testImage), false)
		list, err := e.s.Volumes(id, "")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Volume{}
		for _, v := range list {
			got[v.Name] = v
		}
		v1, v2 := got["smtest-docker-v1"], got["smtest-docker-v2"]
		if v1.Driver != "local" || v1.Mountpoint == "" || len(v1.UsedBy) != 0 || v1.Size != -1 || len(v2.UsedBy) != 1 {
			t.Fatalf("volumes %+v %+v", v1, v2)
		}
		sizes, err := e.s.VolumeSizes(id, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := sizes["smtest-docker-v2"]; !ok {
			t.Fatalf("sizes %+v", sizes)
		}
		expectCode(t, e.s.RemoveVolume(id, "smtest-docker-v2", ""), "docker.volumeInUse")
		if err := e.s.RemoveVolume(id, "smtest-docker-v1", ""); err != nil {
			t.Fatal(err)
		}
		e.sh("docker rm -f smtest-docker-vol-user >/dev/null", false)
		if _, err := e.s.PruneVolumes(id, false, ""); err != nil {
			t.Fatal(err)
		}
		if err := e.s.RemoveVolume(id, "smtest-docker-v2", ""); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("networks and disk usage", func(t *testing.T) {
		e.sh("docker network create --subnet 10.231.0.0/24 smtest-docker-n1 >/dev/null", false)
		e.sh(fmt.Sprintf("docker run -d --name smtest-docker-net-user --network smtest-docker-n1 %s sleep 3600 >/dev/null", testImage), false)
		list, err := e.s.Networks(id, "")
		if err != nil {
			t.Fatal(err)
		}
		var n1, bridge *Network
		for i := range list {
			switch list[i].Name {
			case "smtest-docker-n1":
				n1 = &list[i]
			case "bridge":
				bridge = &list[i]
			}
		}
		if n1 == nil || n1.Driver != "bridge" || len(n1.Subnets) != 1 || n1.Subnets[0] != "10.231.0.0/24" || len(n1.Containers) != 1 || n1.Builtin {
			t.Fatalf("network %+v", n1)
		}
		if bridge == nil || !bridge.Builtin {
			t.Fatalf("bridge %+v", bridge)
		}
		expectCode(t, e.s.RemoveNetwork(id, "smtest-docker-n1", ""), "docker.networkInUse")
		e.sh("docker rm -f smtest-docker-net-user >/dev/null", false)
		if err := e.s.RemoveNetwork(id, "smtest-docker-n1", ""); err != nil {
			t.Fatal(err)
		}
		df, err := e.s.DiskUsage(id, "")
		if err != nil || len(df) < 3 || df[0].Type != "Images" || df[0].Total < 1 || df[0].Size <= 0 {
			t.Fatalf("df %+v %v", df, err)
		}
	})

	t.Run("compose", func(t *testing.T) {
		dir := "/tmp/smtest-docker-proj"
		file := dir + "/compose.yaml"
		project := "smtest-docker-proj"
		spec := ComposeSpec{Project: project, Path: file}
		v1 := fmt.Sprintf(`services:
  web:
    image: %s
    command: ["sleep", "3600"]
    env_file: ./app.env
    healthcheck:
      test: ["CMD", "true"]
      interval: 2s
`, testImage)
		v2 := v1 + fmt.Sprintf(`  worker:
    image: %s
    command: ["sleep", "3600"]
`, testImage)

		// Validation happens in the target directory (relative env_file).
		e.sh("mkdir -p "+dir+" && echo APP_SECRET=1 > "+dir+"/app.env", false)
		r, err := e.s.ValidateCompose(id, spec, v1, "")
		if err != nil || !r.OK {
			t.Fatalf("validate v1: %+v %v", r, err)
		}
		r, err = e.s.ValidateCompose(id, spec, "services:\n  web:\n    image: [\n", "")
		if err != nil || r.OK || r.Output == "" {
			t.Fatalf("validate bad yaml: %+v %v", r, err)
		}
		r, err = e.s.ValidateCompose(id, spec, strings.Replace(v1, "./app.env", "./missing.env", 1), "")
		if err != nil || r.OK || !strings.Contains(r.Output, "missing.env") {
			t.Fatalf("validate missing env file: %+v %v", r, err)
		}
		if out := e.sh("ls -a "+dir, false); strings.Contains(out, ".sm-compose") {
			t.Fatalf("temp file left behind: %s", out)
		}
		// Validation of a project whose directory doesn't exist yet.
		r, err = e.s.ValidateCompose(id, ComposeSpec{Project: "smtest-docker-new", Path: "/tmp/smtest-docker-new/compose.yaml"}, "services:\n  a:\n    image: x\n", "")
		if err != nil || !r.OK {
			t.Fatalf("validate new dir: %+v %v", r, err)
		}

		// Create.
		e.mustDone(e.s.DeployCompose(id, DeployRequest{Spec: spec, Content: v1, Kind: "new"}, ""))
		_, err = e.s.DeployCompose(id, DeployRequest{Spec: spec, Content: v1, Kind: "new"}, "")
		expectCode(t, err, "docker.fileExists")
		f, err := e.s.ReadComposeFile(id, file, "")
		if err != nil || !f.Exists || f.Content != v1 || f.Sha != sha(v1) || !f.Writable || f.Mode != "644" {
			t.Fatalf("read %+v %v", f, err)
		}
		e.sh("chmod 600 "+file, false)

		projects, err := e.s.ComposeProjects(id, "")
		if err != nil {
			t.Fatal(err)
		}
		var p *ComposeProject
		for i := range projects {
			if projects[i].Name == project {
				p = &projects[i]
			}
		}
		if p == nil || p.Running != 1 || p.Total != 1 || len(p.ConfigFiles) != 1 || p.ConfigFiles[0] != file || p.WorkingDir != dir {
			t.Fatalf("project %+v", p)
		}
		det, err := e.s.ComposeProjectDetail(id, project, "")
		if err != nil || len(det.Containers) != 1 || det.Containers[0].Service != "web" || len(det.Defined) != 1 || det.ConfigError != "" {
			t.Fatalf("detail %+v %v", det, err)
		}

		// Save a new version (stale sha refused, invalid content refused).
		_, err = e.s.DeployCompose(id, DeployRequest{Spec: spec, Content: v2, ExpectedSha: sha("other")}, "")
		expectCode(t, err, "docker.fileChanged")
		info := e.wait(e.s.DeployCompose(id, DeployRequest{Spec: spec, Content: "services: [oops", ExpectedSha: f.Sha}, ""))
		if info.State != "error" || info.Error == nil || info.Error.Code != "docker.validateFailed" {
			t.Fatalf("invalid deploy: %+v", info)
		}
		if f2, _ := e.s.ReadComposeFile(id, file, ""); f2.Content != v1 {
			t.Fatalf("file changed by an invalid deploy")
		}
		info = e.mustDone(e.s.DeployCompose(id, DeployRequest{Spec: spec, Content: v2, ExpectedSha: f.Sha}, ""))
		if info.Result != sha(v2) {
			t.Fatalf("result %q", info.Result)
		}
		f, _ = e.s.ReadComposeFile(id, file, "")
		if f.Content != v2 || f.Mode != "600" {
			t.Fatalf("after save: mode %s content %q", f.Mode, f.Content)
		}
		if det, _ := e.s.ComposeProjectDetail(id, project, ""); len(det.Containers) != 2 {
			t.Fatalf("after save: %+v", det.Containers)
		}
		hist, err := e.s.ComposeHistory(id, file)
		if err != nil || len(hist) != 1 || hist[0].Sha != sha(v1) || hist[0].Content != "" || hist[0].Actor == "" {
			t.Fatalf("history %+v %v", hist, err)
		}
		content, err := e.s.ComposeVersionContent(id, file, hist[0].ID)
		if err != nil || content != v1 {
			t.Fatalf("version content %q %v", content, err)
		}
		_, err = e.s.ComposeVersionContent(id, file, "nope")
		expectCode(t, err, "docker.versionNotFound")

		// Rollback to v1: the worker is removed as an orphan.
		e.mustDone(e.s.DeployCompose(id, DeployRequest{Spec: spec, Kind: "rollback", VersionID: hist[0].ID, Content: v1, ExpectedSha: f.Sha}, ""))
		f, _ = e.s.ReadComposeFile(id, file, "")
		if f.Content != v1 {
			t.Fatalf("rollback content %q", f.Content)
		}
		if det, _ := e.s.ComposeProjectDetail(id, project, ""); len(det.Containers) != 1 {
			t.Fatalf("after rollback: %+v", det.Containers)
		}
		if hist, _ := e.s.ComposeHistory(id, file); len(hist) != 2 || hist[0].Sha != sha(v2) {
			t.Fatalf("history after rollback %+v", hist)
		}

		// Recreate the web container from its labels.
		web := project + "-web-1"
		before := e.container(web)
		e.mustDone(e.s.RecreateContainer(id, web, false, ""))
		after := e.container(web)
		if before == nil || after == nil || before.ID == after.ID || after.State != "running" {
			t.Fatalf("recreate: before %+v after %+v", before, after)
		}

		// Project actions.
		for _, a := range []string{"restart", "stop", "start", "pull", "up"} {
			info := e.wait(e.s.ComposeAction(id, project, a, ComposeActionOptions{}, ""))
			if a == "pull" {
				continue // no registry access; local-only image
			}
			if info.State != "done" {
				t.Fatalf("%s: %+v", a, info)
			}
		}
		if got := e.state(web); got != "running" {
			t.Fatalf("after up: %s", got)
		}
		e.mustDone(e.s.ComposeAction(id, project, "down", ComposeActionOptions{RemoveVolumes: true}, ""))
		if got := e.state(web); got != "absent" {
			t.Fatalf("after down: %s", got)
		}
		_, err = e.s.ComposeAction(id, project, "up", ComposeActionOptions{}, "")
		expectCode(t, err, "docker.projectNotFound")

		audits, _ := c.AuditList(core.AuditQuery{Server: id, Action: "docker.compose."})
		seen := map[string]bool{}
		for _, a := range audits {
			seen[a.Action] = true
		}
		for _, a := range []string{"docker.compose.create", "docker.compose.deploy", "docker.compose.rollback", "docker.compose.down", "docker.compose.restart"} {
			if !seen[a] {
				t.Fatalf("missing audit %s (%v)", a, seen)
			}
		}
	})

	t.Run("root-owned compose file uses sudo", func(t *testing.T) {
		dir := "/tmp/smtest-docker-rootproj"
		file := dir + "/compose.yaml"
		content := fmt.Sprintf("services:\n  app:\n    image: %s\n    command: [\"sleep\", \"3600\"]\n", testImage)
		e.sh("mkdir -p "+dir+" && printf '%s' "+core.Q(content)+" > "+file+" && chmod 600 "+file+" && chmod 755 "+dir, true)
		f, err := e.s.ReadComposeFile(id, file, "")
		if err != nil || !f.Exists || f.Content != content || f.Writable || f.Owner != "root:root" {
			t.Fatalf("read root file %+v %v", f, err)
		}
		spec := ComposeSpec{Project: "smtest-docker-rootproj", Path: file}
		if r, err := e.s.ValidateCompose(id, spec, content, ""); err != nil || !r.OK {
			t.Fatalf("validate %+v %v", r, err)
		}
		next := content + "    restart: unless-stopped\n"
		e.mustDone(e.s.DeployCompose(id, DeployRequest{Spec: spec, Content: next, ExpectedSha: f.Sha}, ""))
		f, err = e.s.ReadComposeFile(id, file, "")
		if err != nil || f.Content != next || f.Owner != "root:root" || f.Mode != "600" {
			t.Fatalf("after save %+v %v", f, err)
		}
		e.mustDone(e.s.ComposeAction(id, "smtest-docker-rootproj", "down", ComposeActionOptions{}, ""))
	})

	t.Run("viewer is denied", func(t *testing.T) {
		testutil.SetRole(t, c, id, "viewer")
		defer testutil.SetRole(t, c, id, "admin")
		expectCode(t, e.s.ContainerAction(id, "smtest-docker-x", "stop", ""), "access.denied")
		expectCode(t, e.s.RemoveContainer(id, "smtest-docker-x", true, false, ""), "access.denied")
		expectCode(t, e.s.RemoveImage(id, testImage, false, ""), "access.denied")
		expectCode(t, e.s.RemoveVolume(id, "smtest-docker-x", ""), "access.denied")
		expectCode(t, e.s.RemoveNetwork(id, "smtest-docker-x", ""), "access.denied")
		_, err := e.s.PullImage(id, "busybox", "")
		expectCode(t, err, "access.denied")
		_, err = e.s.PruneImages(id, false, "")
		expectCode(t, err, "access.denied")
		_, err = e.s.ComposeAction(id, "smtest-docker-x", "down", ComposeActionOptions{}, "")
		expectCode(t, err, "access.denied")
		_, err = e.s.ValidateCompose(id, ComposeSpec{Project: "smtest-docker-x", Path: "/tmp/smtest-docker-x/compose.yaml"}, "services: {}", "")
		expectCode(t, err, "access.denied")
		_, err = e.s.DeployCompose(id, DeployRequest{Spec: ComposeSpec{Project: "smtest-docker-x", Path: "/tmp/smtest-docker-x/compose.yaml"}, Content: "services: {}", Kind: "new"}, "")
		expectCode(t, err, "access.denied")
		_, err = e.s.RecreateContainer(id, "smtest-docker-x", false, "")
		expectCode(t, err, "access.denied")
		// Reading stays allowed.
		if _, err := e.s.Containers(id, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.Images(id, ""); err != nil {
			t.Fatal(err)
		}
	})
}
