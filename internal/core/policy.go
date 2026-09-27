package core

import (
	"server-manager/internal/apperr"
	"server-manager/internal/db"
)

func errNotFound() error { return db.ErrNotFound }

// Perm is something a role may be allowed to do on a server. Reading
// (browsing files, metrics, logs, status) is always allowed.
type Perm string

const (
	PermFiles    Perm = "files"    // create/modify/delete files
	PermTerminal Perm = "terminal" // interactive shells
	PermExec     Perm = "exec"     // arbitrary commands (quick command, command center)
	PermServices Perm = "services" // start/stop services, kill processes
	PermDocker   Perm = "docker"   // container/compose changes
	PermDeploy   Perm = "deploy"   // run deployments / rollbacks
	PermDatabase Perm = "database" // run queries
	PermBackup   Perm = "backup"   // run/configure backups
	PermRestore  Perm = "restore"  // restore backups (destructive)
	PermWeb      Perm = "web"      // reverse proxy / certificates
	PermSecurity Perm = "security" // firewall, sshd config, authorized keys
	PermUsers    Perm = "users"    // Linux users and groups
)

// Roles and what they grant. The role is a per-server guard rail enforced by
// the backend; note that a role with terminal/exec can do anything the SSH
// account can, so give viewers read-only credentials as well.
var rolePerms = map[string]map[Perm]bool{
	"admin": {PermFiles: true, PermTerminal: true, PermExec: true, PermServices: true, PermDocker: true, PermDeploy: true,
		PermDatabase: true, PermBackup: true, PermRestore: true, PermWeb: true, PermSecurity: true, PermUsers: true},
	"operator": {PermFiles: true, PermTerminal: true, PermExec: true, PermServices: true, PermDocker: true, PermDeploy: true,
		PermDatabase: true, PermBackup: true, PermRestore: true, PermWeb: true},
	"developer": {PermFiles: true, PermTerminal: true, PermDeploy: true, PermDocker: true, PermDatabase: true},
	"viewer":    {},
}

// Allowed reports whether the server's role grants p.
func (c *Core) Allowed(serverID string, p Perm) bool {
	sv, err := c.Store.Get(serverID)
	if err != nil {
		return false
	}
	role := sv.Role
	if role == "" {
		role = "admin"
	}
	return rolePerms[role][p]
}

// Require returns access.denied unless the server's role grants p.
func (c *Core) Require(serverID string, p Perm) error {
	if c.Allowed(serverID, p) {
		return nil
	}
	role := "admin"
	if sv, err := c.Store.Get(serverID); err == nil && sv.Role != "" {
		role = sv.Role
	}
	return apperr.New("access.denied", "perm", string(p), "role", role)
}

// RolePerms lists the permissions of every role (for the UI).
func RolePerms() map[string][]string {
	out := map[string][]string{}
	for r, ps := range rolePerms {
		list := []string{}
		for p, ok := range ps {
			if ok {
				list = append(list, string(p))
			}
		}
		out[r] = list
	}
	return out
}
