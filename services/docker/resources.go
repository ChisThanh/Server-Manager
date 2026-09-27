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

// ---- inventory: which containers use which image / volume ----

type invEntry struct {
	ID      string
	Name    string
	ImageID string
	Volumes []string
}

const inventoryScript = `ids=$(docker ps -aq --no-trunc) || exit 1
[ -z "$ids" ] && exit 0
docker inspect --format '{{.Id}}|{{.Name}}|{{.Image}}|{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' $ids 2>/dev/null
exit 0`

func (x *sess) inventory(ctx context.Context) ([]invEntry, error) {
	res, err := x.ok(ctx, inventoryScript, "")
	if err != nil {
		return nil, err
	}
	out := []invEntry{}
	for _, l := range strings.Split(res.Stdout, "\n") {
		f := strings.SplitN(strings.TrimSpace(l), "|", 4)
		if len(f) < 4 {
			continue
		}
		out = append(out, invEntry{ID: f[0], Name: strings.TrimPrefix(f[1], "/"), ImageID: f[2], Volumes: strings.Fields(f[3])})
	}
	return out, nil
}

// ---- images ----

type Image struct {
	ID         string   `json:"id"` // sha256:…
	Repository string   `json:"repository"`
	Tag        string   `json:"tag"`
	Digest     string   `json:"digest"`
	Created    int64    `json:"created"`
	Size       int64    `json:"size"`
	Dangling   bool     `json:"dangling"`
	UsedBy     []string `json:"usedBy"` // container names
}

// Ref is how the image is addressed for removal ("repo:tag", or the id).
func (i Image) ref() string {
	if i.Repository != "" && i.Repository != "<none>" && i.Tag != "" && i.Tag != "<none>" {
		return i.Repository + ":" + i.Tag
	}
	return i.ID
}

type imageRow struct {
	ID         string `json:"ID"`
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	Digest     string `json:"Digest"`
	CreatedAt  string `json:"CreatedAt"`
	Size       string `json:"Size"`
}

func parseImages(out string, inv []invEntry) []Image {
	users := map[string][]string{}
	for _, c := range inv {
		users[c.ImageID] = append(users[c.ImageID], c.Name)
	}
	list := []Image{}
	for _, r := range jsonLines[imageRow](out) {
		id := r.ID
		if !strings.HasPrefix(id, "sha256:") && len(id) == 64 {
			id = "sha256:" + id
		}
		im := Image{ID: id, Repository: r.Repository, Tag: r.Tag, Digest: r.Digest, Created: parseTime(r.CreatedAt), Size: parseSize(r.Size), UsedBy: []string{}}
		if im.Digest == "<none>" {
			im.Digest = ""
		}
		im.Dangling = (r.Repository == "<none>" || r.Repository == "") && (r.Tag == "<none>" || r.Tag == "")
		if u := users[id]; u != nil {
			im.UsedBy = append(im.UsedBy, u...)
			sort.Strings(im.UsedBy)
		}
		list = append(list, im)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Repository != list[j].Repository {
			return list[i].Repository < list[j].Repository
		}
		return list[i].Tag < list[j].Tag
	})
	return list
}

// Images lists images with the containers that use them.
func (s *DockerService) Images(connID, sudoPassword string) ([]Image, error) {
	ctx, cancel := core.Timeout(45 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	res, err := x.ok(ctx, "docker images --no-trunc --format '{{json .}}'", "")
	if err != nil {
		return nil, err
	}
	inv, err := x.inventory(ctx)
	if err != nil {
		return nil, err
	}
	return parseImages(res.Stdout, inv), nil
}

// PullImage pulls an image as a job.
func (s *DockerService) PullImage(connID, ref, sudoPassword string) (string, error) {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return "", err
	}
	ref = strings.TrimSpace(ref)
	if err := checkImage(ref); err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err == nil {
		err = x.checkSudo(ctx, x.a.sudo)
	}
	if err != nil {
		s.core.Audit(connID, "docker.image.pull", ref, "", err)
		return "", err
	}
	return x.job("docker.image.pull", "docker pull "+ref, func(ctx context.Context, j *core.Job) error {
		err := x.jobRun(ctx, j, "docker pull "+core.Q(ref), x.a.sudo)
		s.core.Audit(connID, "docker.image.pull", ref, "", err)
		return err
	}), nil
}

// RemoveImage removes an image (or one of its tags). An image used by
// containers is refused with docker.imageInUse unless force is set.
func (s *DockerService) RemoveImage(connID, ref string, force bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return err
	}
	if err := checkImage(ref); err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	err := func() error {
		x, err := s.open(ctx, connID, sudoPassword)
		if err != nil {
			return err
		}
		res, err := x.run(ctx, "docker image inspect --format '{{.Id}} {{len .RepoTags}}' "+core.Q(ref), "")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return apperr.New("docker.notFound", "name", ref)
		}
		f := strings.Fields(res.Stdout)
		if len(f) < 2 {
			return apperr.New("docker.parseFailed").WithDetail(res.Stdout)
		}
		id, tags := f[0], f[1]
		// Removing one of several tags only untags; the image stays.
		untag := !imageIDRe.MatchString(ref) && tags != "0" && tags != "1"
		if !force && !untag {
			inv, err := x.inventory(ctx)
			if err != nil {
				return err
			}
			var users []string
			for _, c := range inv {
				if c.ImageID == id {
					users = append(users, c.Name)
				}
			}
			if len(users) > 0 {
				sort.Strings(users)
				return apperr.New("docker.imageInUse", "name", ref, "containers", strings.Join(users, ", "))
			}
		}
		cmd := "docker rmi "
		if force {
			cmd += "-f "
		}
		_, err = x.ok(ctx, cmd+core.Q(ref), "")
		return err
	}()
	s.core.Audit(connID, "docker.image.remove", ref, fmt.Sprintf("force=%t", force), err)
	return err
}

type PruneResult struct {
	Reclaimed int64  `json:"reclaimed"` // bytes
	Output    string `json:"output"`
}

var reclaimedRe = regexp.MustCompile(`(?i)total reclaimed space:\s*(\S+)`)

func pruneResult(out string) PruneResult {
	r := PruneResult{Output: strings.TrimSpace(out)}
	if m := reclaimedRe.FindStringSubmatch(out); m != nil {
		r.Reclaimed = parseSize(m[1])
	}
	return r
}

// PruneImages removes dangling images (all: every image without a container).
func (s *DockerService) PruneImages(connID string, all bool, sudoPassword string) (PruneResult, error) {
	return s.prune(connID, "docker.image.prune", "docker image prune -f", all, sudoPassword)
}

// PruneVolumes removes unused anonymous volumes (all: named ones too).
func (s *DockerService) PruneVolumes(connID string, all bool, sudoPassword string) (PruneResult, error) {
	return s.prune(connID, "docker.volume.prune", "docker volume prune -f", all, sudoPassword)
}

func (s *DockerService) prune(connID, action, cmd string, all bool, sudoPassword string) (PruneResult, error) {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return PruneResult{}, err
	}
	if all {
		cmd += " -a"
	}
	ctx, cancel := core.Timeout(10 * time.Minute)
	defer cancel()
	var r PruneResult
	err := func() error {
		x, err := s.open(ctx, connID, sudoPassword)
		if err != nil {
			return err
		}
		res, err := x.ok(ctx, cmd, "")
		if err != nil {
			return err
		}
		r = pruneResult(res.Stdout)
		return nil
	}()
	s.core.Audit(connID, action, "", fmt.Sprintf("all=%t reclaimed=%d bytes", all, r.Reclaimed), err)
	return r, err
}

// ---- volumes ----

type Volume struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Mountpoint string   `json:"mountpoint"`
	Scope      string   `json:"scope"`
	Created    int64    `json:"created"`
	Project    string   `json:"project"`   // com.docker.compose.project
	Anonymous  bool     `json:"anonymous"` // 64-hex name
	UsedBy     []string `json:"usedBy"`
	Size       int64    `json:"size"` // -1 = unknown (see VolumeSizes)
}

type volumeJSON struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	Scope      string            `json:"Scope"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
}

var hex64Re = regexp.MustCompile(`^[a-f0-9]{64}$`)

func parseVolumes(out string, inv []invEntry) []Volume {
	users := map[string][]string{}
	for _, c := range inv {
		for _, v := range c.Volumes {
			users[v] = append(users[v], c.Name)
		}
	}
	var raw []volumeJSON
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &raw)
	list := []Volume{}
	for _, v := range raw {
		vol := Volume{Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint, Scope: v.Scope, Created: parseTime(v.CreatedAt),
			Project: v.Labels["com.docker.compose.project"], Anonymous: hex64Re.MatchString(v.Name), UsedBy: []string{}, Size: -1}
		if u := users[v.Name]; u != nil {
			vol.UsedBy = append(vol.UsedBy, u...)
			sort.Strings(vol.UsedBy)
		}
		list = append(list, vol)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Volumes lists volumes with the containers that mount them.
func (s *DockerService) Volumes(connID, sudoPassword string) ([]Volume, error) {
	ctx, cancel := core.Timeout(45 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	res, err := x.ok(ctx, `v=$(docker volume ls -q) || exit 1; [ -z "$v" ] && { echo '[]'; exit 0; }; docker volume inspect $v`, "")
	if err != nil {
		return nil, err
	}
	inv, err := x.inventory(ctx)
	if err != nil {
		return nil, err
	}
	return parseVolumes(res.Stdout, inv), nil
}

// VolumeSizes returns volume sizes in bytes from `docker system df -v`
// (can take a while on large hosts).
func (s *DockerService) VolumeSizes(connID, sudoPassword string) (map[string]int64, error) {
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	res, err := x.ok(ctx, "docker system df -v --format '{{json .}}'", "")
	if err != nil {
		return nil, err
	}
	var df struct {
		Volumes []struct {
			Name string `json:"Name"`
			Size string `json:"Size"`
		} `json:"Volumes"`
	}
	out := map[string]int64{}
	if json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &df) != nil {
		return out, nil // older docker without JSON support for -v
	}
	for _, v := range df.Volumes {
		out[v.Name] = parseSize(v.Size)
	}
	return out, nil
}

// RemoveVolume deletes a volume (docker refuses volumes in use).
func (s *DockerService) RemoveVolume(connID, name, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return err
	}
	if err := checkObject("volume", name); err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	err := func() error {
		x, err := s.open(ctx, connID, sudoPassword)
		if err != nil {
			return err
		}
		res, err := x.run(ctx, "docker volume rm "+core.Q(name), "")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			if strings.Contains(res.Stderr, "in use") {
				return apperr.New("docker.volumeInUse", "name", name).WithDetail(res.Stderr)
			}
			return cmdErr(res)
		}
		return nil
	}()
	s.core.Audit(connID, "docker.volume.remove", name, "", err)
	return err
}

// ---- networks ----

type NetContainer struct {
	Name string `json:"name"`
	IPv4 string `json:"ipv4"`
}

type Network struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Driver     string         `json:"driver"`
	Scope      string         `json:"scope"`
	Internal   bool           `json:"internal"`
	Created    int64          `json:"created"`
	Subnets    []string       `json:"subnets"`
	Gateways   []string       `json:"gateways"`
	Containers []NetContainer `json:"containers"`
	Project    string         `json:"project"`
	Builtin    bool           `json:"builtin"` // bridge / host / none: can't be removed
}

type networkJSON struct {
	ID       string `json:"Id"`
	Name     string `json:"Name"`
	Driver   string `json:"Driver"`
	Scope    string `json:"Scope"`
	Internal bool   `json:"Internal"`
	Created  string `json:"Created"`
	IPAM     struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
	Labels map[string]string `json:"Labels"`
}

var builtinNetworks = map[string]bool{"bridge": true, "host": true, "none": true}

func parseNetworks(out string) []Network {
	var raw []networkJSON
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &raw)
	list := []Network{}
	for _, n := range raw {
		nw := Network{ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Internal: n.Internal, Created: parseTime(n.Created),
			Subnets: []string{}, Gateways: []string{}, Containers: []NetContainer{}, Project: n.Labels["com.docker.compose.project"],
			Builtin: builtinNetworks[n.Name]}
		for _, c := range n.IPAM.Config {
			if c.Subnet != "" {
				nw.Subnets = append(nw.Subnets, c.Subnet)
			}
			if c.Gateway != "" {
				nw.Gateways = append(nw.Gateways, c.Gateway)
			}
		}
		for _, c := range n.Containers {
			nw.Containers = append(nw.Containers, NetContainer{Name: c.Name, IPv4: c.IPv4Address})
		}
		sort.Slice(nw.Containers, func(i, j int) bool { return nw.Containers[i].Name < nw.Containers[j].Name })
		list = append(list, nw)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Networks lists networks with their subnets and attached (running) containers.
func (s *DockerService) Networks(connID, sudoPassword string) ([]Network, error) {
	ctx, cancel := core.Timeout(45 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	res, err := x.ok(ctx, `n=$(docker network ls -q --no-trunc) || exit 1; [ -z "$n" ] && { echo '[]'; exit 0; }; docker network inspect $n`, "")
	if err != nil {
		return nil, err
	}
	return parseNetworks(res.Stdout), nil
}

// RemoveNetwork deletes a user-defined network.
func (s *DockerService) RemoveNetwork(connID, name, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDocker); err != nil {
		return err
	}
	if err := checkObject("network", name); err != nil {
		return err
	}
	if builtinNetworks[name] {
		return apperr.New("docker.builtinNetwork", "name", name)
	}
	ctx, cancel := core.Timeout(time.Minute)
	defer cancel()
	err := func() error {
		x, err := s.open(ctx, connID, sudoPassword)
		if err != nil {
			return err
		}
		res, err := x.run(ctx, "docker network rm "+core.Q(name), "")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			if strings.Contains(res.Stderr, "active endpoints") {
				return apperr.New("docker.networkInUse", "name", name).WithDetail(res.Stderr)
			}
			if strings.Contains(res.Stderr, "pre-defined") {
				return apperr.New("docker.builtinNetwork", "name", name)
			}
			return cmdErr(res)
		}
		return nil
	}()
	s.core.Audit(connID, "docker.network.remove", name, "", err)
	return err
}

// ---- disk usage ----

type DiskUsage struct {
	Type        string `json:"type"` // Images | Containers | Local Volumes | Build Cache
	Total       int    `json:"total"`
	Active      int    `json:"active"`
	Size        int64  `json:"size"`
	Reclaimable int64  `json:"reclaimable"`
}

type dfRow struct {
	Type        string `json:"Type"`
	TotalCount  string `json:"TotalCount"`
	Active      string `json:"Active"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
}

func parseDF(out string) []DiskUsage {
	list := []DiskUsage{}
	for _, r := range jsonLines[dfRow](out) {
		d := DiskUsage{Type: r.Type, Size: parseSize(r.Size), Reclaimable: parseSize(r.Reclaimable)}
		fmt.Sscan(r.TotalCount, &d.Total)
		fmt.Sscan(r.Active, &d.Active)
		list = append(list, d)
	}
	return list
}

// DiskUsage summarizes `docker system df`.
func (s *DockerService) DiskUsage(connID, sudoPassword string) ([]DiskUsage, error) {
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	x, err := s.open(ctx, connID, sudoPassword)
	if err != nil {
		return nil, err
	}
	res, err := x.ok(ctx, "docker system df --format '{{json .}}'", "")
	if err != nil {
		return nil, err
	}
	return parseDF(res.Stdout), nil
}
