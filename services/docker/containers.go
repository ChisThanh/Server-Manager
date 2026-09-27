package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
)

// Container is one row of `docker ps -a`, with its Compose labels.
type Container struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Command     string   `json:"command"`
	Created     int64    `json:"created"` // unix s
	State       string   `json:"state"`   // running | exited | paused | restarting | created | dead | removing
	Status      string   `json:"status"`  // "Up 3 minutes (healthy)"
	Health      string   `json:"health"`  // healthy | unhealthy | starting | ""
	Ports       string   `json:"ports"`
	Networks    []string `json:"networks"`
	Project     string   `json:"project"` // com.docker.compose.project
	Service     string   `json:"service"` // com.docker.compose.service
	WorkingDir  string   `json:"workingDir"`
	ConfigFiles []string `json:"configFiles"`
	EnvFiles    []string `json:"envFiles"`
}

// psFormat builds one JSON object per container. It avoids `{{json .}}`,
// which makes docker compute container sizes (slow on busy hosts), and reads
// the Compose labels individually (the Labels string is ambiguous because
// config_files itself contains commas).
const psFormat = `{"id":{{json .ID}},"names":{{json .Names}},"image":{{json .Image}},"command":{{json .Command}},` +
	`"created":{{json .CreatedAt}},"state":{{json .State}},"status":{{json .Status}},"ports":{{json .Ports}},` +
	`"networks":{{json .Networks}},"project":{{json (.Label "com.docker.compose.project")}},` +
	`"service":{{json (.Label "com.docker.compose.service")}},"workdir":{{json (.Label "com.docker.compose.project.working_dir")}},` +
	`"files":{{json (.Label "com.docker.compose.project.config_files")}},` +
	`"envfiles":{{json (.Label "com.docker.compose.project.environment_file")}}}`

type psRow struct {
	ID       string `json:"id"`
	Names    string `json:"names"`
	Image    string `json:"image"`
	Command  string `json:"command"`
	Created  string `json:"created"`
	State    string `json:"state"`
	Status   string `json:"status"`
	Ports    string `json:"ports"`
	Networks string `json:"networks"`
	Project  string `json:"project"`
	Service  string `json:"service"`
	Workdir  string `json:"workdir"`
	Files    string `json:"files"`
	EnvFiles string `json:"envfiles"`
}

func parsePS(out string) []Container {
	list := []Container{}
	for _, r := range jsonLines[psRow](out) {
		name, _, _ := strings.Cut(r.Names, ",")
		c := Container{
			ID: r.ID, Name: name, Image: r.Image, Command: strings.Trim(r.Command, `"`),
			Created: parseTime(r.Created), State: strings.ToLower(r.State), Status: r.Status, Health: healthOf(r.Status),
			Ports: r.Ports, Networks: splitList(r.Networks), Project: r.Project, Service: r.Service,
			WorkingDir: r.Workdir, ConfigFiles: splitList(r.Files), EnvFiles: splitList(r.EnvFiles),
		}
		if c.State == "" {
			c.State = stateFromStatus(r.Status)
		}
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// stateFromStatus covers old docker versions without {{.State}}.
func stateFromStatus(st string) string {
	switch {
	case strings.HasPrefix(st, "Up") && strings.Contains(st, "(Paused)"):
		return "paused"
	case strings.HasPrefix(st, "Up"):
		return "running"
	case strings.HasPrefix(st, "Restarting"):
		return "restarting"
	case strings.HasPrefix(st, "Created"):
		return "created"
	case strings.HasPrefix(st, "Dead"):
		return "dead"
	case strings.HasPrefix(st, "Removal"):
		return "removing"
	}
	return "exited"
}

func (x *sess) containers(ctx context.Context) ([]Container, error) {
	res, err := x.ok(ctx, "docker ps -a --no-trunc --format "+core.Q(psFormat), "")
	if err != nil {
		return nil, err
	}
	return parsePS(res.Stdout), nil
}

// Containers lists every container.
func (s *DockerService) Containers(connID, sudoPassword string) ([]Container, error) {
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	return x.containers(ctx)
}

// ContainerStats is one sample of `docker stats` (running containers only).
type ContainerStats struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CPU        float64 `json:"cpu"` // % of one core (can exceed 100)
	MemUsage   int64   `json:"memUsage"`
	MemLimit   int64   `json:"memLimit"`
	MemPct     float64 `json:"memPct"`
	NetRx      int64   `json:"netRx"`
	NetTx      int64   `json:"netTx"`
	BlockRead  int64   `json:"blockRead"`
	BlockWrite int64   `json:"blockWrite"`
	PIDs       int     `json:"pids"`
}

type statsRow struct {
	ID       string `json:"ID"`
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
	PIDs     string `json:"PIDs"`
}

func parseStats(out string) []ContainerStats {
	list := []ContainerStats{}
	for _, r := range jsonLines[statsRow](out) {
		st := ContainerStats{ID: r.ID, Name: r.Name, CPU: parsePct(r.CPUPerc), MemPct: parsePct(r.MemPerc)}
		st.MemUsage, st.MemLimit = parsePair(r.MemUsage)
		st.NetRx, st.NetTx = parsePair(r.NetIO)
		st.BlockRead, st.BlockWrite = parsePair(r.BlockIO)
		fmt.Sscan(r.PIDs, &st.PIDs)
		list = append(list, st)
	}
	return list
}

// Stats samples resource usage of running containers (takes ~2 s).
func (s *DockerService) Stats(connID, sudoPassword string) ([]ContainerStats, error) {
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	res, err := x.ok(ctx, "docker stats --no-stream --no-trunc --format '{{json .}}'", "")
	if err != nil {
		return nil, err
	}
	return parseStats(res.Stdout), nil
}

var containerActions = map[string]string{
	"start": "start", "stop": "stop", "restart": "restart", "pause": "pause", "unpause": "unpause", "kill": "kill",
}

// ContainerAction runs start | stop | restart | pause | unpause | kill.
func (s *DockerService) ContainerAction(connID, name, action, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return err
	}
	verb, ok := containerActions[action]
	if !ok {
		return apperr.New("docker.invalidAction", "action", action)
	}
	if err := checkObject("container", name); err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	err := func() error {
		x, err := s.open(ctx, connID, sudoPassword)
		if err != nil {
			return err
		}
		_, err = x.ok(ctx, "docker "+verb+" "+core.Q(name), "")
		return err
	}()
	s.core.Audit(connID, "docker.container."+action, name, "docker "+verb, err)
	return err
}

// RemoveContainer runs docker rm (force: also running ones; volumes: remove
// its anonymous volumes).
func (s *DockerService) RemoveContainer(connID, name string, force, volumes bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return err
	}
	if err := checkObject("container", name); err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	cmd := "docker rm"
	if force {
		cmd += " -f"
	}
	if volumes {
		cmd += " -v"
	}
	err := func() error {
		x, err := s.open(ctx, connID, sudoPassword)
		if err != nil {
			return err
		}
		_, err = x.ok(ctx, cmd+" "+core.Q(name), "")
		return err
	}()
	s.core.Audit(connID, "docker.container.remove", name, fmt.Sprintf("force=%t volumes=%t", force, volumes), err)
	return err
}

// ContainerLogs returns the last lines of a container's output (stdout and
// stderr merged). since: "15m", "2h", a unix time or a date; "" = all.
func (s *DockerService) ContainerLogs(connID, name string, tail int, since string, timestamps bool, sudoPassword string) (string, error) {
	if err := checkObject("container", name); err != nil {
		return "", err
	}
	if tail <= 0 || tail > 10000 {
		tail = 500
	}
	cmd := fmt.Sprintf("docker logs --tail %d", tail)
	if since = strings.TrimSpace(since); since != "" {
		if !sinceRe.MatchString(since) {
			return "", apperr.New("docker.invalidSince", "since", since)
		}
		cmd += " --since " + core.Q(since)
	}
	if timestamps {
		cmd += " --timestamps"
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return "", err
	}
	res, err := x.run(ctx, cmd+" "+core.Q(name)+" 2>&1", "")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", cmdErr(res)
	}
	out := res.Stdout
	const max = 4 << 20
	if len(out) > max {
		out = "…\n" + out[len(out)-max:]
	}
	return out, nil
}

// ---- inspect ----

type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"` // key looks like a secret; the UI masks it
}

type Mount struct {
	Type        string `json:"type"` // volume | bind | tmpfs
	Name        string `json:"name"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        string `json:"mode"`
	RW          bool   `json:"rw"`
}

type NetAttach struct {
	Network string   `json:"network"`
	IP      string   `json:"ip"`
	IPv6    string   `json:"ipv6"`
	Gateway string   `json:"gateway"`
	MAC     string   `json:"mac"`
	Aliases []string `json:"aliases"`
}

type PortMap struct {
	Container string `json:"container"` // "80/tcp"
	HostIP    string `json:"hostIp"`
	HostPort  string `json:"hostPort"` // "" = exposed only
}

type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type HealthEntry struct {
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
}

type ContainerDetail struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Image         string        `json:"image"`
	ImageID       string        `json:"imageId"`
	Created       int64         `json:"created"`
	State         string        `json:"state"`
	Health        string        `json:"health"`
	FailingStreak int           `json:"failingStreak"`
	StartedAt     int64         `json:"startedAt"`
	FinishedAt    int64         `json:"finishedAt"`
	ExitCode      int           `json:"exitCode"`
	OOMKilled     bool          `json:"oomKilled"`
	Error         string        `json:"error"`
	Pid           int           `json:"pid"`
	RestartCount  int           `json:"restartCount"`
	RestartPolicy string        `json:"restartPolicy"` // "unless-stopped", "on-failure:5"
	Entrypoint    []string      `json:"entrypoint"`
	Cmd           []string      `json:"cmd"`
	WorkingDir    string        `json:"workingDir"`
	User          string        `json:"user"`
	Hostname      string        `json:"hostname"`
	Privileged    bool          `json:"privileged"`
	NetworkMode   string        `json:"networkMode"`
	MemoryLimit   int64         `json:"memoryLimit"`
	CPULimit      float64       `json:"cpuLimit"` // cores
	HealthCheck   []string      `json:"healthCheck"`
	Env           []EnvVar      `json:"env"`
	Mounts        []Mount       `json:"mounts"`
	Networks      []NetAttach   `json:"networks"`
	Ports         []PortMap     `json:"ports"`
	Labels        []Label       `json:"labels"`
	HealthLog     []HealthEntry `json:"healthLog"`
	Project       string        `json:"project"`
	Service       string        `json:"service"`
	// Raw is the full `docker inspect` JSON, indented.
	Raw string `json:"raw"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type inspectJSON struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	Created      string `json:"Created"`
	Image        string `json:"Image"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		OOMKilled  bool   `json:"OOMKilled"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
			Log           []struct {
				Start    string `json:"Start"`
				End      string `json:"End"`
				ExitCode int    `json:"ExitCode"`
				Output   string `json:"Output"`
			} `json:"Log"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Hostname     string            `json:"Hostname"`
		User         string            `json:"User"`
		Env          []string          `json:"Env"`
		Cmd          []string          `json:"Cmd"`
		Entrypoint   []string          `json:"Entrypoint"`
		Image        string            `json:"Image"`
		WorkingDir   string            `json:"WorkingDir"`
		Labels       map[string]string `json:"Labels"`
		ExposedPorts map[string]any    `json:"ExposedPorts"`
		Healthcheck  *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		NetworkMode string `json:"NetworkMode"`
		Privileged  bool   `json:"Privileged"`
		Memory      int64  `json:"Memory"`
		NanoCpus    int64  `json:"NanoCpus"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Ports    map[string][]portBinding `json:"Ports"`
		Networks map[string]struct {
			IPAddress         string   `json:"IPAddress"`
			GlobalIPv6Address string   `json:"GlobalIPv6Address"`
			Gateway           string   `json:"Gateway"`
			MacAddress        string   `json:"MacAddress"`
			Aliases           []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ContainerInspect returns formatted details and the raw inspect JSON.
func (s *DockerService) ContainerInspect(connID, name, sudoPassword string) (ContainerDetail, error) {
	if err := checkObject("container", name); err != nil {
		return ContainerDetail{}, err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return ContainerDetail{}, err
	}
	res, err := x.run(ctx, "docker container inspect "+core.Q(name), "")
	if err != nil {
		return ContainerDetail{}, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(strings.ToLower(res.Stderr), "no such") {
			return ContainerDetail{}, apperr.New("docker.notFound", "name", name)
		}
		return ContainerDetail{}, cmdErr(res)
	}
	return parseInspect(res.Stdout)
}

var secretKeyRe = regexp.MustCompile(`(?i)pass|secret|token|key`)

func parseInspect(out string) (ContainerDetail, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) == 0 {
		return ContainerDetail{}, apperr.New("docker.parseFailed").WithDetail(firstLine(out))
	}
	var in inspectJSON
	if err := json.Unmarshal(raw[0], &in); err != nil {
		return ContainerDetail{}, apperr.Wrap(err, "docker.parseFailed")
	}
	d := ContainerDetail{
		ID: in.ID, Name: strings.TrimPrefix(in.Name, "/"), Image: in.Config.Image, ImageID: in.Image,
		Created: parseTime(in.Created), State: in.State.Status, StartedAt: parseTime(in.State.StartedAt),
		FinishedAt: parseTime(in.State.FinishedAt), ExitCode: in.State.ExitCode, OOMKilled: in.State.OOMKilled,
		Error: in.State.Error, Pid: in.State.Pid, RestartCount: in.RestartCount,
		Entrypoint: nonNil(in.Config.Entrypoint), Cmd: nonNil(in.Config.Cmd), WorkingDir: in.Config.WorkingDir,
		User: in.Config.User, Hostname: in.Config.Hostname, Privileged: in.HostConfig.Privileged,
		NetworkMode: in.HostConfig.NetworkMode, MemoryLimit: in.HostConfig.Memory,
		CPULimit:    float64(in.HostConfig.NanoCpus) / 1e9,
		HealthCheck: []string{}, Env: []EnvVar{}, Mounts: []Mount{}, Networks: []NetAttach{}, Ports: []PortMap{},
		Labels: []Label{}, HealthLog: []HealthEntry{},
		Project: in.Config.Labels["com.docker.compose.project"], Service: in.Config.Labels["com.docker.compose.service"],
	}
	rp := in.HostConfig.RestartPolicy
	d.RestartPolicy = rp.Name
	if rp.Name == "on-failure" && rp.MaximumRetryCount > 0 {
		d.RestartPolicy += ":" + itoa(rp.MaximumRetryCount)
	}
	if d.RestartPolicy == "" {
		d.RestartPolicy = "no"
	}
	if in.Config.Healthcheck != nil {
		d.HealthCheck = nonNil(in.Config.Healthcheck.Test)
	}
	if h := in.State.Health; h != nil {
		d.Health = h.Status
		d.FailingStreak = h.FailingStreak
		for _, l := range h.Log {
			d.HealthLog = append(d.HealthLog, HealthEntry{Start: parseTime(l.Start), End: parseTime(l.End), ExitCode: l.ExitCode, Output: strings.TrimSpace(l.Output)})
		}
	}
	for _, e := range in.Config.Env {
		k, v, _ := strings.Cut(e, "=")
		d.Env = append(d.Env, EnvVar{Key: k, Value: v, Secret: secretKeyRe.MatchString(k)})
	}
	for _, m := range in.Mounts {
		d.Mounts = append(d.Mounts, Mount{Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination, Mode: m.Mode, RW: m.RW})
	}
	for name, n := range in.NetworkSettings.Networks {
		d.Networks = append(d.Networks, NetAttach{Network: name, IP: n.IPAddress, IPv6: n.GlobalIPv6Address, Gateway: n.Gateway, MAC: n.MacAddress, Aliases: nonNil(n.Aliases)})
	}
	sort.Slice(d.Networks, func(i, j int) bool { return d.Networks[i].Network < d.Networks[j].Network })
	seen := map[string]bool{}
	for p, bs := range in.NetworkSettings.Ports {
		seen[p] = true
		if len(bs) == 0 {
			d.Ports = append(d.Ports, PortMap{Container: p})
		}
		for _, b := range bs {
			d.Ports = append(d.Ports, PortMap{Container: p, HostIP: b.HostIP, HostPort: b.HostPort})
		}
	}
	for p := range in.Config.ExposedPorts {
		if !seen[p] {
			d.Ports = append(d.Ports, PortMap{Container: p})
		}
	}
	sort.Slice(d.Ports, func(i, j int) bool {
		if d.Ports[i].Container != d.Ports[j].Container {
			return d.Ports[i].Container < d.Ports[j].Container
		}
		return d.Ports[i].HostIP < d.Ports[j].HostIP
	})
	for k, v := range in.Config.Labels {
		d.Labels = append(d.Labels, Label{Key: k, Value: v})
	}
	sort.Slice(d.Labels, func(i, j int) bool { return d.Labels[i].Key < d.Labels[j].Key })
	var pretty strings.Builder
	var obj any
	if json.Unmarshal(raw[0], &obj) == nil {
		enc := json.NewEncoder(&pretty)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(obj)
	}
	d.Raw = pretty.String()
	return d, nil
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ---- recreate (Compose) ----

// composeRef locates a Compose project on disk.
type composeRef struct {
	Project    string
	WorkingDir string
	Files      []string
	EnvFiles   []string
}

func (r composeRef) args(bin string) string {
	var b strings.Builder
	b.WriteString(bin)
	b.WriteString(" -p ")
	b.WriteString(core.Q(r.Project))
	if r.WorkingDir != "" {
		b.WriteString(" --project-directory " + core.Q(r.WorkingDir))
	}
	for _, f := range r.Files {
		b.WriteString(" -f " + core.Q(f))
	}
	for _, f := range r.EnvFiles {
		b.WriteString(" --env-file " + core.Q(f))
	}
	return b.String()
}

// cd returns "cd <dir> && " so relative paths in the files resolve.
func (r composeRef) cd() string {
	if r.WorkingDir == "" {
		return ""
	}
	return "cd " + core.Q(r.WorkingDir) + " && "
}

func (r composeRef) validate() (composeRef, error) {
	if err := checkProject(r.Project); err != nil {
		return r, err
	}
	var err error
	if r.WorkingDir != "" {
		if r.WorkingDir, err = checkPath(r.WorkingDir); err != nil {
			return r, err
		}
	}
	if r.Files, err = checkPaths(r.Files); err != nil {
		return r, err
	}
	if r.EnvFiles, err = checkPaths(r.EnvFiles); err != nil {
		return r, err
	}
	return r, nil
}

// RecreateContainer recreates a Compose-managed container from its project
// files (`up -d --force-recreate --no-deps <service>`) as a job.
func (s *DockerService) RecreateContainer(connID, name string, pull bool, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return "", err
	}
	if err := checkObject("container", name); err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	fail := func(err error) (string, error) {
		s.core.Audit(connID, "docker.container.recreate", name, "", err)
		return "", err
	}
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return fail(err)
	}
	bin, err := x.compose()
	if err != nil {
		return fail(err)
	}
	res, err := x.run(ctx, "docker container inspect --format '{{json .Config.Labels}}' "+core.Q(name), "")
	if err != nil {
		return fail(err)
	}
	if res.ExitCode != 0 {
		return fail(apperr.New("docker.notFound", "name", name))
	}
	var labels map[string]string
	_ = json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &labels)
	service := labels["com.docker.compose.service"]
	ref := composeRef{
		Project:    labels["com.docker.compose.project"],
		WorkingDir: labels["com.docker.compose.project.working_dir"],
		Files:      splitList(labels["com.docker.compose.project.config_files"]),
		EnvFiles:   splitList(labels["com.docker.compose.project.environment_file"]),
	}
	if ref.Project == "" || service == "" || len(ref.Files) == 0 {
		return fail(apperr.New("docker.notCompose", "name", name))
	}
	if ref, err = ref.validate(); err != nil {
		return fail(err)
	}
	if err := checkService(service); err != nil {
		return fail(err)
	}
	sudo, err := x.composeSudo(ctx, ref)
	if err != nil {
		return fail(err)
	}
	if err := x.checkSudo(ctx, sudo); err != nil {
		return fail(err)
	}
	base := ref.cd() + ref.args(bin)
	detail := fmt.Sprintf("project=%s service=%s files=%s pull=%t", ref.Project, service, strings.Join(ref.Files, ","), pull)
	id := x.job("docker.container.recreate", "compose recreate "+name, func(ctx context.Context, j *core.Job) error {
		err := func() error {
			if pull {
				if err := x.jobRun(ctx, j, base+" pull "+core.Q(service), sudo); err != nil {
					return err
				}
			}
			return x.jobRun(ctx, j, base+" up -d --force-recreate --no-deps "+core.Q(service), sudo)
		}()
		s.core.Audit(connID, "docker.container.recreate", name, detail, err)
		return err
	})
	return id, nil
}
