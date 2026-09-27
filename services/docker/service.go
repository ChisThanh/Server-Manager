// Package docker: Docker containers, images, volumes, networks and Compose
// projects, managed agentlessly through the docker CLI over SSH.
//
// Every docker command runs as the login user when it can reach the daemon
// socket, otherwise through sudo (detected once per connection and cached,
// see access.go). Reading never needs a permission; every change requires
// core.PermDocker and is written to the audit log.
package docker

import (
	"sync"

	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

type DockerService struct {
	core *core.Core

	mu    sync.Mutex
	cache map[*sshx.Conn]*access
}

func New(c *core.Core) *DockerService {
	return &DockerService{core: c, cache: map[*sshx.Conn]*access{}}
}
