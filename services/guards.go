package services

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Exported entry points for changing operations: each checks the server's
// role, then records the outcome in the audit log. The implementations are
// the lower-case methods.

func (s *FileService) guard(connID string) error { return s.core.Require(connID, core.PermFiles) }

func (s *FileService) WriteFile(connID, p, content string, expectedModTime int64, force bool) (SaveResult, error) {
	if err := s.guard(connID); err != nil {
		return SaveResult{}, err
	}
	r, err := s.writeFile(connID, p, content, expectedModTime, force)
	s.core.Audit(connID, "file.write", p, fmt.Sprintf("%d bytes", len(content)), err)
	return r, err
}

func (s *FileService) WriteFileSudo(connID, p, content, password string, expectedModTime int64, force bool) (SaveResult, error) {
	if err := s.guard(connID); err != nil {
		return SaveResult{}, err
	}
	r, err := s.writeFileSudo(connID, p, content, password, expectedModTime, force)
	s.core.Audit(connID, "file.write", p, fmt.Sprintf("sudo, %d bytes", len(content)), err)
	return r, err
}

func (s *FileService) ReadFileSudo(connID, p, password string) (FileContent, error) {
	r, err := s.readFileSudo(connID, p, password)
	s.core.Audit(connID, "file.readSudo", p, "", err)
	return r, err
}

func (s *FileService) ListDirSudo(connID, dir, password string) ([]FileEntry, error) {
	return s.listDirSudo(connID, dir, password)
}

func (s *FileService) CreateFile(connID, p string) (FileEntry, error) {
	if err := s.guard(connID); err != nil {
		return FileEntry{}, err
	}
	r, err := s.createFile(connID, p)
	s.core.Audit(connID, "file.create", p, "", err)
	return r, err
}

func (s *FileService) CreateDir(connID, p string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.createDir(connID, p)
	s.core.Audit(connID, "file.mkdir", p, "", err)
	return err
}

func (s *FileService) Rename(connID, from, to string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.rename(connID, from, to)
	s.core.Audit(connID, "file.rename", from, "→ "+to, err)
	return err
}

func (s *FileService) Move(connID string, paths []string, destDir string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.move(connID, paths, destDir)
	s.core.Audit(connID, "file.move", strings.Join(paths, ", "), "→ "+destDir, err)
	return err
}

func (s *FileService) Copy(connID string, paths []string, destDir string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.copy(connID, paths, destDir)
	s.core.Audit(connID, "file.copy", strings.Join(paths, ", "), "→ "+destDir, err)
	return err
}

func (s *FileService) Delete(connID string, paths []string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.delete(connID, paths)
	s.core.Audit(connID, "file.delete", strings.Join(paths, ", "), "", err)
	return err
}

func (s *FileService) Chmod(connID, p string, mode uint32, recursive bool) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.chmod(connID, p, mode, recursive)
	s.core.Audit(connID, "file.chmod", p, fmt.Sprintf("%04o recursive=%t", mode, recursive), err)
	return err
}

func (s *FileService) Compress(connID, dir string, names []string, archiveName string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.compress(connID, dir, names, archiveName)
	s.core.Audit(connID, "file.compress", dir+"/"+archiveName, strings.Join(names, ", "), err)
	return err
}

func (s *FileService) Extract(connID, archive string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.extract(connID, archive)
	s.core.Audit(connID, "file.extract", archive, "", err)
	return err
}

func (s *FileService) SudoOp(connID, op string, paths []string, dest string, mode uint32, recursive bool, password string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.sudoOp(connID, op, paths, dest, mode, recursive, password)
	detail := "sudo"
	if dest != "" {
		detail += ", → " + dest
	}
	if op == "chmod" {
		detail += fmt.Sprintf(", %04o recursive=%t", mode, recursive)
	}
	s.core.Audit(connID, "file."+op, strings.Join(paths, ", "), detail, err)
	return err
}

func (s *FileService) Upload(connID string, localPaths []string, remoteDir string) (string, error) {
	if err := s.guard(connID); err != nil {
		return "", err
	}
	id, err := s.upload(connID, localPaths, remoteDir)
	s.core.Audit(connID, "file.upload", remoteDir, strings.Join(localPaths, ", "), err)
	return id, err
}

func (s *FileService) Download(connID, remotePath, localPath string) (string, error) {
	id, err := s.download(connID, remotePath, localPath)
	s.core.Audit(connID, "file.download", remotePath, "→ "+localPath, err)
	return id, err
}

func (s *SystemService) Kill(connID string, pid int, signal string, useSudo bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermServices); err != nil {
		return err
	}
	err := s.kill(connID, pid, signal, useSudo, sudoPassword)
	s.core.Audit(connID, "process.kill", fmt.Sprint(pid), "SIG"+signal, err)
	return err
}

func (s *SystemService) ServiceAction(connID, name, action string, useSudo bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermServices); err != nil {
		return err
	}
	err := s.serviceAction(connID, name, action, useSudo, sudoPassword)
	s.core.Audit(connID, "service."+action, name, "", err)
	return err
}

func (s *SystemService) Exec(connID, command string, useSudo bool, sudoPassword string) (sshx.ExecResult, error) {
	if err := s.core.Require(connID, core.PermExec); err != nil {
		return sshx.ExecResult{}, err
	}
	res, err := s.exec(connID, command, useSudo, sudoPassword)
	outcome := err
	if err == nil && res.ExitCode != 0 {
		outcome = core.CmdError(res)
	}
	detail := ""
	if useSudo {
		detail = "sudo"
	}
	s.core.Audit(connID, "command.run", command, detail, outcome)
	return res, err
}

func (s *TerminalService) Open(connID, cwd string, cols, rows int) (string, error) {
	if err := s.core.Require(connID, core.PermTerminal); err != nil {
		return "", err
	}
	id, err := s.open(connID, cwd, cols, rows)
	detail := ""
	if f := s.RecordingOf(id); f != "" {
		detail = "recording: " + f
	}
	s.core.Audit(connID, "terminal.open", cwd, detail, err)
	return id, err
}

var ownerName = regexp.MustCompile(`^([a-z_][a-z0-9_.-]{0,31}\$?|[0-9]{1,10})$`)

// Chown changes owner and/or group (empty = unchanged), through sudo when
// the login user may not.
func (s *FileService) Chown(connID, p, owner, group string, recursive bool, sudoPassword string) error {
	if err := s.guard(connID); err != nil {
		return err
	}
	err := s.chown(connID, p, owner, group, recursive, sudoPassword)
	s.core.Audit(connID, "file.chown", p, fmt.Sprintf("%s:%s recursive=%t", owner, group, recursive), err)
	return err
}

func (s *FileService) chown(connID, p, owner, group string, recursive bool, sudoPassword string) error {
	if (owner != "" && !ownerName.MatchString(owner)) || (group != "" && !ownerName.MatchString(group)) || (owner == "" && group == "") {
		return apperr.New("fs.invalidOwner")
	}
	if p == "" || p == "/" {
		return apperr.New("fs.invalidOp")
	}
	spec := owner
	if group != "" {
		spec += ":" + group
	}
	cmd := "chown "
	if recursive {
		cmd += "-R "
	}
	cmd += "-- " + core.Q(spec) + " " + core.Q(p)
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := s.core.RunAuto(ctx, conn, cmd, sudoPassword, "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return core.CmdError(res)
	}
	return nil
}

// ConfigResult is the outcome of ApplyConfig.
type ConfigResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

// ApplyConfig validates and reloads a service after its config was edited:
// kind = nginx | caddy | apache | sshd | systemd | service (unit = name).
// The reload only happens when validation passes.
func (s *SystemService) ApplyConfig(connID, kind, unit, sudoPassword string) (ConfigResult, error) {
	if err := s.core.Require(connID, core.PermServices); err != nil {
		return ConfigResult{}, err
	}
	var cmd string
	switch kind {
	case "nginx":
		cmd = `nginx -t 2>&1 && { systemctl reload nginx 2>&1 || nginx -s reload 2>&1; }`
	case "caddy":
		cmd = `caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1 && { systemctl reload caddy 2>&1 || caddy reload --config /etc/caddy/Caddyfile 2>&1; }`
	case "apache":
		cmd = `if command -v apache2ctl >/dev/null 2>&1; then apache2ctl configtest 2>&1 && systemctl reload apache2 2>&1; else apachectl configtest 2>&1 && systemctl reload httpd 2>&1; fi`
	case "sshd":
		cmd = `sshd -t 2>&1 && { systemctl reload ssh 2>&1 || systemctl reload sshd 2>&1; }`
	case "systemd":
		cmd = `systemctl daemon-reload 2>&1`
		if unit != "" {
			if !unitName.MatchString(unit) {
				return ConfigResult{}, apperr.New("sys.invalidUnit")
			}
			cmd += " && systemctl try-restart " + core.Q(unit) + " 2>&1"
		}
	case "service":
		if !unitName.MatchString(unit) {
			return ConfigResult{}, apperr.New("sys.invalidUnit")
		}
		cmd = "systemctl restart " + core.Q(unit) + " 2>&1"
	default:
		return ConfigResult{}, apperr.New("sys.invalidAction")
	}
	conn, err := s.core.Conn(connID)
	if err != nil {
		return ConfigResult{}, err
	}
	ctx, cancel := core.Timeout(2 * time.Minute)
	defer cancel()
	res, err := s.core.Run(ctx, conn, cmd, true, sudoPassword, "")
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	r := ConfigResult{OK: err == nil && res.ExitCode == 0, Output: out}
	var auditErr error = err
	if err == nil && !r.OK {
		auditErr = core.CmdError(res)
	}
	s.core.Audit(connID, "config.apply", kind+" "+unit, "", auditErr)
	if err != nil {
		return r, err
	}
	return r, nil
}
