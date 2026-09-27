package web

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// A transaction changes several configuration files, validates the result
// with the engine's own checker and restores every touched path if the
// check (or any step) fails:
//
//  1. candidate contents are written (via sudo, fed on stdin) into a private
//     directory next to the configuration (same filesystem → atomic mv);
//  2. one script backs up each target, moves the candidates into place,
//     creates/removes links, and runs the test command;
//  3. on failure it undoes the applied steps in reverse order and prints
//     the checker's output.

type txOp struct {
	Kind         string // write | delete | link | rename
	Path         string
	Content      string // write
	Mode         string // write: octal mode ("" = keep existing, else 0644)
	Group        string // write: chgrp
	Target       string // link target / rename destination
	MustNotExist bool   // write/link: fail if Path already exists
}

type txResult struct {
	TestOutput string
}

var txnDirRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+/\.sm-txn\.[A-Za-z0-9]+$`)
var modeRe = regexp.MustCompile(`^0?[0-7]{3}$`)
var groupRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

const (
	exitTestFailed  = 3
	exitApplyFailed = 4
	exitExists      = 5
)

// transact runs ops under base (a directory on the same filesystem, e.g.
// /etc/nginx). pre are shell commands run first (mkdir…; not rolled back).
func (s *WebService) transact(ctx context.Context, conn *sshx.Conn, pw, base string, pre []string, ops []txOp, testCmd string) (txResult, error) {
	for _, op := range ops {
		if err := validAbsPath(op.Path); err != nil {
			return txResult{}, err
		}
		if op.Target != "" {
			if err := validAbsPath(op.Target); err != nil {
				return txResult{}, err
			}
		}
		if op.Mode != "" && !modeRe.MatchString(op.Mode) {
			return txResult{}, invalid("mode", op.Mode)
		}
		if op.Group != "" && !groupRe.MatchString(op.Group) {
			return txResult{}, invalid("group", op.Group)
		}
	}
	// 1. private directory (stale ones from interrupted runs are removed)
	res, err := s.sudo(ctx, conn, pw, fmt.Sprintf(
		`find %[1]s -maxdepth 1 -name '.sm-txn.*' -mmin +720 -exec rm -rf {} + 2>/dev/null; T=$(mktemp -d %[1]s/.sm-txn.XXXXXX) && chmod 700 "$T" && echo "$T"`, core.Q(base)), "")
	if err != nil {
		return txResult{}, err
	}
	tdir := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || !txnDirRe.MatchString(tdir) {
		return txResult{}, apperr.New("web.applyFailed").WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	cleanup := func() { _, _ = s.sudo(context.Background(), conn, pw, "rm -rf "+core.Q(tdir), "") }
	// 2. candidates
	for i, op := range ops {
		if op.Kind != "write" {
			continue
		}
		r, err := s.sudo(ctx, conn, pw, fmt.Sprintf("umask 077; cat > %s", core.Q(fmt.Sprintf("%s/c%d", tdir, i))), op.Content)
		if err == nil && r.ExitCode != 0 {
			err = apperr.New("web.applyFailed").WithDetail(core.FirstNonEmpty(r.Stderr, r.Stdout))
		}
		if err != nil {
			cleanup()
			return txResult{}, err
		}
	}
	// 3. commit + test
	script := buildTxnScript(tdir, pre, ops, testCmd)
	res, err = s.sudo(ctx, conn, pw, script, "")
	if err != nil {
		cleanup()
		return txResult{}, err
	}
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	switch res.ExitCode {
	case 0:
		return txResult{TestOutput: tail(out, 60)}, nil
	case exitTestFailed:
		return txResult{TestOutput: out}, apperr.New("web.testFailed").WithDetail(tail(out, 60))
	case exitExists:
		return txResult{}, apperr.New("web.siteExists").WithDetail(tail(out, 5))
	default:
		cleanup()
		return txResult{}, apperr.New("web.applyFailed").WithDetail(tail(out, 30))
	}
}

func buildTxnScript(tdir string, pre []string, ops []txOp, testCmd string) string {
	var b strings.Builder
	T := core.Q(tdir)
	b.WriteString(pathPrefix + "\n")
	b.WriteString("A=-1\n")
	// rollback, newest first
	b.WriteString("rb() {\n")
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		P := core.Q(op.Path)
		bk := core.Q(fmt.Sprintf("%s/b%d", tdir, i))
		fmt.Fprintf(&b, "  if [ $A -ge %d ]; then ", i)
		switch op.Kind {
		case "rename":
			fmt.Fprintf(&b, "mv -f %s %s", core.Q(op.Target), P)
		default:
			fmt.Fprintf(&b, "rm -f %s; if [ -e %s ] || [ -L %s ]; then mv -f %s %s; fi", P, bk, bk, bk, P)
		}
		b.WriteString("; fi\n")
	}
	b.WriteString("  :\n}\n")
	fmt.Fprintf(&b, "fail() { echo \"step failed: $1\" >&2; rb; rm -rf %s; exit %d; }\n", T, exitApplyFailed)
	for _, c := range pre {
		fmt.Fprintf(&b, "{ %s; } || fail pre\n", c)
	}
	for i, op := range ops {
		P := core.Q(op.Path)
		bk := core.Q(fmt.Sprintf("%s/b%d", tdir, i))
		cand := core.Q(fmt.Sprintf("%s/c%d", tdir, i))
		if op.MustNotExist {
			fmt.Fprintf(&b, "if [ -e %s ] || [ -L %s ]; then echo %s >&2; rb; rm -rf %s; exit %d; fi\n", P, P, core.Q("exists: "+op.Path), T, exitExists)
		}
		if op.Kind != "rename" {
			fmt.Fprintf(&b, "if [ -e %s ] || [ -L %s ]; then cp -a %s %s || fail backup; fi\n", P, P, P, bk)
		}
		switch op.Kind {
		case "write":
			if op.Mode != "" {
				fmt.Fprintf(&b, "chmod %s %s || fail chmod\n", op.Mode, cand)
			} else {
				fmt.Fprintf(&b, "if [ -f %s ] && [ ! -L %s ]; then chmod \"$(stat -c %%a %s)\" %s; chown \"$(stat -c %%u:%%g %s)\" %s; else chmod 644 %s; fi || fail chmod\n",
					P, P, P, cand, P, cand, cand)
			}
			if op.Group != "" {
				fmt.Fprintf(&b, "chgrp %s %s || fail chgrp\n", core.Q(op.Group), cand)
			}
			fmt.Fprintf(&b, "mv -f %s %s || fail write\n", cand, P)
		case "delete":
			fmt.Fprintf(&b, "rm -f %s || fail delete\n", P)
		case "link":
			fmt.Fprintf(&b, "if [ -e %s ] && [ ! -L %s ]; then fail notalink; fi\n", P, P)
			fmt.Fprintf(&b, "ln -sfn %s %s || fail link\n", core.Q(op.Target), P)
		case "rename":
			fmt.Fprintf(&b, "if [ -e %s ]; then fail exists; fi\n", core.Q(op.Target))
			fmt.Fprintf(&b, "mv %s %s || fail rename\n", P, core.Q(op.Target))
		}
		fmt.Fprintf(&b, "A=%d\n", i)
	}
	if testCmd != "" {
		fmt.Fprintf(&b, "OUT=$( { %s; } 2>&1 ); RC=$?\n", testCmd)
		fmt.Fprintf(&b, "if [ $RC -ne 0 ]; then printf '%%s\\n' \"$OUT\"; rb; rm -rf %s; exit %d; fi\n", T, exitTestFailed)
		b.WriteString("printf '%s\\n' \"$OUT\"\n")
	}
	fmt.Fprintf(&b, "rm -rf %s\nexit 0\n", T)
	return b.String()
}

// reloadNginx reloads a running nginx. notRunning = nothing to reload.
func (s *WebService) reloadNginx(ctx context.Context, conn *sshx.Conn, pw string) (reloaded, notRunning bool, err error) {
	return s.reload(ctx, conn, pw, "nginx", "nginx -s reload")
}

func (s *WebService) reloadCaddy(ctx context.Context, conn *sshx.Conn, pw string) (reloaded, notRunning bool, err error) {
	return s.reload(ctx, conn, pw, "caddy", "caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile")
}

func (s *WebService) reload(ctx context.Context, conn *sshx.Conn, pw, unit, direct string) (bool, bool, error) {
	cmd := fmt.Sprintf(`if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet %[1]s; then systemctl reload %[1]s 2>&1; elif pgrep -x %[1]s >/dev/null 2>&1; then %[2]s 2>&1; else echo @@NOTRUNNING; fi`, unit, direct)
	res, err := s.sudo(ctx, conn, pw, cmd, "")
	if err != nil {
		return false, false, err
	}
	if strings.Contains(res.Stdout, "@@NOTRUNNING") {
		return false, true, nil
	}
	if res.ExitCode != 0 {
		return false, false, apperr.New("web.reloadFailed", "engine", unit).WithDetail(core.FirstNonEmpty(res.Stderr, res.Stdout))
	}
	return true, false, nil
}
