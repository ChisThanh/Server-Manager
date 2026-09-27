package database

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// markerPrefix starts every marker line the runners print.
const markerPrefix = "@@SM"

// exitMarker precedes the client's exit status on stderr (piped runs).
const exitMarker = "@@SMEXIT "

// takeExitMarker moves the client's exit status from stderr to ExitCode.
func takeExitMarker(res sshx.ExecResult) sshx.ExecResult {
	i := strings.LastIndex(res.Stderr, exitMarker)
	if i < 0 || (i > 0 && res.Stderr[i-1] != '\n') {
		return res
	}
	line := res.Stderr[i+len(exitMarker):]
	line, _, _ = strings.Cut(line, "\n")
	if code, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && res.ExitCode == 0 {
		res.ExitCode = code
	}
	res.Stderr = res.Stderr[:i]
	return res
}

// traceHook, when set (tests), receives every remote command line the
// package runs, so tests can assert that no secret appears in it.
var traceHook func(cmd string)

// sm_as runs a command as another OS user (runuser, sudo, or su).
const asUserFn = `sm_as() { u=$1; shift
if [ "$(id -un)" = "$u" ]; then "$@"
elif command -v runuser >/dev/null 2>&1; then runuser -u "$u" -- "$@"
elif command -v sudo >/dev/null 2>&1; then sudo -n -u "$u" -- "$@"
else su -s /bin/sh "$u" -c 'exec "$0" "$@"' "$@"; fi; }
`

const mysqlClient = `c=$(command -v mariadb || command -v mysql) || { echo "sm: mysql client not found" >&2; exit 127; }`

func secretVar(engine string) string {
	switch engine {
	case EnginePostgres:
		return "PGPASSWORD"
	case EngineMySQL:
		return "MYSQL_PWD"
	}
	return "REDISCLI_AUTH"
}

type envVar struct{ k, v string }

func quoteArgs(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = core.Q(a)
	}
	return strings.Join(q, " ")
}

// clientCommand builds the remote shell script running the engine's client
// with args for target t. The password (if any) is the first line of the
// returned stdin, read into the client's environment variable by the shell;
// input follows it. With capBytes > 0 the output is cut after that many
// bytes (the client is then killed by SIGPIPE).
func (s *DatabaseService) clientCommand(ctx context.Context, conn *sshx.Conn, t Target, args []string, env []envVar, input string, capBytes int) (script, stdin string, sudo bool, err error) {
	var pw string
	if t.Auth == AuthPassword {
		pw = store.Keychain(secretName(t.Server, t.ID))
		if pw == "" {
			return "", "", false, apperr.New("db.noPassword", "name", t.Name)
		}
	}
	sv := secretVar(t.Engine)
	envs := ""
	for _, e := range env {
		envs += " " + e.k + "=" + core.Q(e.v)
	}
	qa := quoteArgs(args)
	var prelude, final string
	switch t.Mode {
	case ModeLocal:
		switch {
		case t.Auth == AuthPeer && t.Engine == EnginePostgres:
			sudo = true
			prelude = asUserFn + "cd / 2>/dev/null"
			final = "sm_as postgres env" + envs + " psql " + qa
		case t.Engine == EngineMySQL:
			sudo = t.Auth == AuthPeer
			prelude = mysqlClient
			final = "env" + envs + ` "$c" ` + qa
		case t.Engine == EnginePostgres:
			prelude = "cd / 2>/dev/null"
			final = "env" + envs + " psql " + qa
		default:
			prelude = ":"
			final = "env" + envs + " redis-cli " + qa
		}
	case ModeDocker:
		if !reContainer.MatchString(t.Container) {
			return "", "", false, apperr.New("db.invalidContainer", "name", t.Container)
		}
		sudo = s.dockerNeedsSudo(ctx, conn)
		inner := dockerInner(t, qa, envs)
		e := ""
		if pw != "" {
			e = " -e " + sv
		}
		prelude = ":"
		final = "docker exec -i" + e + " " + core.Q(t.Container) + " sh -c " + core.Q(inner)
	default:
		return "", "", false, apperr.New("db.invalidMode", "mode", t.Mode)
	}
	if pw != "" {
		script = "IFS= read -r " + sv + "; export " + sv + "\n"
		stdin = pw + "\n"
	}
	script += prelude + "\n"
	if capBytes > 0 {
		// The pipeline's status is head's: report the client's on stderr.
		script += "{ " + final + "; echo \"" + exitMarker + "$?\" >&2; } | head -c " + strconv.Itoa(capBytes)
	} else if strings.HasPrefix(final, "sm_as ") {
		script += final // a shell function can't be exec'd
	} else {
		script += "exec " + final
	}
	return script, stdin + input, sudo, nil
}

// dockerInner is the script run by `sh -c` inside the container. With peer
// auth it uses the credentials of the container's own environment (the
// official images' variables); values never leave the container.
func dockerInner(t Target, qa, envs string) string {
	exports := ""
	if envs != "" {
		exports = "export" + envs + "; "
	}
	peer := t.Auth == AuthPeer
	switch t.Engine {
	case EnginePostgres:
		if peer {
			return exports + `u="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-postgres}}"; p="${POSTGRES_PASSWORD:-${POSTGRESQL_PASSWORD:-}}"; ` +
				`[ -n "$p" ] && { PGPASSWORD="$p"; export PGPASSWORD; }; exec psql -U "$u" ` + qa
		}
		return exports + "exec psql " + qa
	case EngineMySQL:
		if peer {
			return exports + `p="${MYSQL_ROOT_PASSWORD:-${MARIADB_ROOT_PASSWORD:-}}"; [ -n "$p" ] && { MYSQL_PWD="$p"; export MYSQL_PWD; }; ` +
				mysqlClient + `; exec "$c" -uroot ` + qa
		}
		return exports + mysqlClient + `; exec "$c" ` + qa
	default:
		if peer {
			return exports + `[ -n "${REDIS_PASSWORD:-}" ] && { REDISCLI_AUTH="$REDIS_PASSWORD"; export REDISCLI_AUTH; }; exec redis-cli ` + qa
		}
		return exports + "exec redis-cli " + qa
	}
}

// runClient runs the client of t and classifies connection-level failures
// (client missing, cannot connect, authentication, container).
func (s *DatabaseService) runClient(ctx context.Context, conn *sshx.Conn, t Target, args []string, env []envVar, input string, capBytes int, sudoPW string) (sshx.ExecResult, error) {
	script, stdin, sudo, err := s.clientCommand(ctx, conn, t, args, env, input, capBytes)
	if err != nil {
		return sshx.ExecResult{}, err
	}
	if traceHook != nil {
		traceHook(script)
	}
	res, err := s.core.Run(ctx, conn, script, sudo, sudoPW, stdin)
	if capBytes > 0 {
		res = takeExitMarker(res)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, apperr.New("db.timeout")
		}
		return res, err
	}
	if e := clientError(t, res); e != nil {
		return res, e
	}
	return res, nil
}

var (
	reAuthFail = regexp.MustCompile(`(?i)password authentication failed|peer authentication failed|no password supplied|fe_sendauth|ERROR 1045|ERROR 1698|access denied for user|NOAUTH|WRONGPASS|invalid password|invalid username-password|Authentication required`)
	reConnFail = regexp.MustCompile(`(?i)could not connect to server|connection to server .* failed|ERROR 200[2356]|can't connect to|could not connect to redis|connection refused|is the server running|no such file or directory.*socket|unknown mysql server host|could not translate host name`)
)

func clientError(t Target, res sshx.ExecResult) *apperr.Error {
	if res.ExitCode == 0 {
		return nil
	}
	out := strings.TrimSpace(res.Stderr)
	all := out + "\n" + res.Stdout
	switch {
	case strings.Contains(all, "No such container"):
		return apperr.New("db.containerNotFound", "name", t.Container)
	case strings.Contains(all, "is not running") && t.Mode == ModeDocker:
		return apperr.New("db.containerStopped", "name", t.Container)
	case strings.Contains(all, "sm: mysql client not found"),
		res.ExitCode == 127 && !strings.Contains(res.Stdout, markerPrefix):
		return apperr.New("db.clientMissing", "engine", engineLabel(t.Engine)).WithDetail(firstLines(out, 3))
	case regexp.MustCompile(`user .?postgres.? does not exist|unknown user:? postgres|user postgres not found`).MatchString(all):
		return apperr.New("db.noPostgresUser")
	}
	// Connection problems are reported before any statement ran (the
	// runners print a marker once connected).
	if strings.Contains(res.Stdout, markerPrefix) {
		return nil
	}
	head := firstLines(out, 4)
	if reAuthFail.MatchString(head) {
		return apperr.New("db.authFailed").WithDetail(head)
	}
	if reConnFail.MatchString(head) {
		return apperr.New("db.connectFailed").WithDetail(head)
	}
	return nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// connTarget resolves the connection and target of a call.
func (s *DatabaseService) connTarget(connID, targetID string) (*sshx.Conn, Target, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, Target{}, err
	}
	t, err := s.target(connID, targetID)
	if err != nil {
		return nil, Target{}, err
	}
	return conn, t, nil
}
