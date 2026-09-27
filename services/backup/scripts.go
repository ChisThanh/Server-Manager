package backup

import (
	"fmt"
	"strconv"
	"strings"

	"server-manager/internal/core"
)

// stage is one command of a remote pipeline.
type stage struct {
	name string
	cmd  string // sh snippet, run in its own subshell
}

// pipelineScript builds a POSIX sh script that runs stages connected by
// pipes and exits non-zero when ANY stage fails. dash (Debian's sh) has no
// `set -o pipefail`, so every stage appends its exit status to a temp file
// which is checked afterwards; a stage killed before reporting counts as a
// failure too. This is what keeps a failed pg_dump piped into gzip from
// looking like a successful (empty) backup.
//
// pre runs first in the main shell (exiting there aborts the script);
// redirect is an optional target for the last stage's stdout; onFail runs
// when a stage failed, onOK after all stages succeeded (its failure fails
// the script).
func pipelineScript(pre string, stages []stage, redirect, onFail, onOK string) string {
	var b strings.Builder
	b.WriteString("umask 077\n")
	b.WriteString(`__st=$(mktemp "${TMPDIR:-/tmp}/smbk.XXXXXX") || { echo "backup: mktemp failed" >&2; exit 97; }` + "\n")
	b.WriteString(`trap 'rm -f "$__st"' EXIT` + "\n")
	b.WriteString("trap 'exit 143' HUP INT TERM\n")
	if pre != "" {
		b.WriteString(pre)
		b.WriteString("\n")
	}
	for i, s := range stages {
		if i > 0 {
			b.WriteString(" | ")
		}
		fmt.Fprintf(&b, "{ ( %s\n); echo \"%s $?\" >>\"$__st\"; }", s.cmd, s.name)
	}
	if redirect != "" {
		b.WriteString(" > " + redirect)
	}
	b.WriteString("\n__rc=0\n")
	names := make([]string, len(stages))
	for i, s := range stages {
		names[i] = s.name
	}
	fmt.Fprintf(&b, "for __s in %s; do __v=$(sed -n \"s/^$__s //p\" \"$__st\"); "+
		"if [ \"$__v\" != 0 ]; then echo \"backup: stage '$__s' failed (exit ${__v:-killed})\" >&2; __rc=1; fi; done\n",
		strings.Join(names, " "))
	if onFail != "" {
		b.WriteString("if [ $__rc != 0 ]; then\n" + onFail + "\nexit 1\nfi\n")
	} else {
		b.WriteString("[ $__rc = 0 ] || exit 1\n")
	}
	if onOK != "" {
		b.WriteString(onOK + "\n")
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// asUserFn runs a command as another local user (root → runuser/su,
// otherwise sudo -n -u).
const asUserFn = `__asuser() { __u=$1; shift
if [ "$(id -u)" = 0 ]; then
  if command -v runuser >/dev/null 2>&1; then runuser -u "$__u" -- "$@"; else su -s /bin/sh "$__u" -c 'exec "$0" "$@"' -- "$@"; fi
else sudo -n -u "$__u" -- "$@"; fi; }`

// tarPre detects GNU tar (for --one-file-system and its "file changed" exit 1).
const tarPre = `__gnu=; tar --version 2>/dev/null | grep -q 'GNU tar' && __gnu=1
__o=; [ -n "$__gnu" ] && __o='--one-file-system'`

// tarTolerant wraps a tar command so GNU tar's exit 1 ("some files differ /
// changed while being read") is a warning, not a failure.
func tarTolerant(cmd string) string {
	return cmd + `; __r=$?; if [ "$__r" = 1 ] && [ -n "$__gnu" ]; then echo "tar: warning: some files changed while being read" >&2; __r=0; fi; exit $__r`
}

const readPwPG = `IFS= read -r PGPASSWORD || true; export PGPASSWORD`
const readPwMy = `IFS= read -r MYSQL_PWD || true; export MYSQL_PWD`
const readPwRedis = `IFS= read -r REDISCLI_AUTH || true; export REDISCLI_AUTH`

const mysqlDumpFind = `__d=$(command -v mariadb-dump || command -v mysqldump) || { echo "backup: mysqldump/mariadb-dump not found" >&2; exit 127; }`
const mysqlCliFind = `__m=$(command -v mariadb || command -v mysql) || { echo "backup: mysql/mariadb client not found" >&2; exit 127; }`

func pgConnArgs(j *Job) string {
	if j.AuthMode != "password" {
		if j.DBPort > 0 {
			return " -p " + strconv.Itoa(j.DBPort)
		}
		return ""
	}
	host := j.DBHost
	if host == "" {
		host = "127.0.0.1"
	}
	port := j.DBPort
	if port == 0 {
		port = 5432
	}
	return " -w -h " + core.Q(host) + " -p " + strconv.Itoa(port) + " -U " + core.Q(j.DBUser)
}

func pgCmd(j *Job, tool string) string {
	if j.AuthMode == "password" {
		return tool + pgConnArgs(j)
	}
	return "cd / && __asuser postgres " + tool + pgConnArgs(j)
}

func mysqlConnArgs(j *Job) string {
	var a string
	if j.DBHost != "" {
		a += " -h " + core.Q(j.DBHost)
	}
	if j.DBPort > 0 {
		a += " -P " + strconv.Itoa(j.DBPort)
		if j.DBHost == "" {
			a += " --protocol=TCP"
		}
	}
	if j.AuthMode == "password" {
		a += " -u " + core.Q(j.DBUser)
	}
	return a
}

func redisCliFn(j *Job) string {
	args := ""
	if j.DBHost != "" {
		args += " -h " + core.Q(j.DBHost)
	}
	if j.DBPort > 0 {
		args += " -p " + strconv.Itoa(j.DBPort)
	}
	return `command -v redis-cli >/dev/null 2>&1 || { echo "backup: redis-cli not found" >&2; exit 127; }
__rc() { redis-cli` + args + ` "$@" | tr -d '\r'; }`
}

// redisDumpPathSh sets $__f to the RDB file.
func redisDumpPathSh(j *Job) string {
	if j.RedisDumpPath != "" {
		return "__f=" + core.Q(j.RedisDumpPath)
	}
	return `__dir=$(__rc CONFIG GET dir | sed -n 2p); __fn=$(__rc CONFIG GET dbfilename | sed -n 2p)
[ -n "$__dir" ] && [ -n "$__fn" ] || { echo "backup: cannot read the dump location (CONFIG GET dir/dbfilename); set the dump path in the job" >&2; exit 1; }
__f="$__dir/$__fn"`
}

// producer returns the setup code and the command whose stdout is the
// backup payload.
func producer(j *Job) (pre, cmd string) {
	switch j.Type {
	case TypeFiles:
		var args []string
		for _, e := range j.Excludes {
			if strings.HasPrefix(e, "/") {
				e = "." + e
			}
			args = append(args, "--exclude="+core.Q(e))
		}
		args = append(args, "-C", "/")
		for _, p := range j.Paths {
			rel := "." + p
			if p == "/" {
				rel = "."
			}
			args = append(args, core.Q(rel))
		}
		return tarPre, tarTolerant("tar -cpf - $__o " + strings.Join(args, " "))
	case TypePostgres:
		pre = asUserFn
		if j.AuthMode == "password" {
			pre += "\n" + readPwPG
		}
		if j.Database == "" {
			return pre, pgCmd(j, "pg_dumpall")
		}
		return pre, pgCmd(j, "pg_dump -Fc") + " " + core.Q(j.Database)
	case TypeMySQL:
		pre = mysqlDumpFind
		if j.AuthMode == "password" {
			pre += "\n" + readPwMy
		}
		c := `"$__d"` + mysqlConnArgs(j) + " --single-transaction --quick --routines --triggers --events --hex-blob --default-character-set=utf8mb4"
		if j.Database == "" {
			return pre, c + " --all-databases"
		}
		return pre, c + " " + core.Q(j.Database)
	case TypeVolume:
		pre = tarPre + "\n" +
			`command -v docker >/dev/null 2>&1 || { echo "backup: docker not found" >&2; exit 127; }
__mp=$(docker volume inspect -f '{{.Mountpoint}}' ` + core.Q(j.Volume) + `) || { echo "backup: docker volume inspect failed" >&2; exit 1; }
[ -d "$__mp" ] || { echo "backup: volume directory $__mp not found on this host" >&2; exit 1; }`
		return pre, tarTolerant(`tar -cpf - -C "$__mp" .`)
	case TypeRedis:
		if j.AuthMode == "password" {
			pre = readPwRedis + "\n"
		}
		pre += redisCliFn(j) + `
__t0=$(__rc LASTSAVE)
case "$__t0" in ''|*[!0-9]*) echo "backup: redis LASTSAVE failed: $__t0" >&2; exit 1;; esac
__now=$(__rc TIME | head -n 1)
[ "$__now" = "$__t0" ] && sleep 1
__o=$(__rc BGSAVE 2>&1)
case "$__o" in *"in progress"*|*scheduled*|*started*) ;; *) echo "backup: redis BGSAVE failed: $__o" >&2; exit 1;; esac
echo "redis: $__o" >&2
__i=0
while :; do __t=$(__rc LASTSAVE); [ -n "$__t" ] && [ "$__t" != "$__t0" ] && break
  __i=$((__i+1)); [ $__i -ge 3600 ] && { echo "backup: timed out waiting for redis BGSAVE" >&2; exit 1; }; sleep 1; done
__bs=$(__rc INFO persistence | sed -n 's/^rdb_last_bgsave_status://p')
[ "$__bs" = ok ] || { echo "backup: redis BGSAVE status: $__bs" >&2; exit 1; }
` + redisDumpPathSh(j) + `
[ -f "$__f" ] || { echo "backup: $__f not found" >&2; exit 1; }
echo "redis: copying $__f" >&2`
		return pre, `cat -- "$__f"`
	case TypeCustom:
		return "", "sh -c " + core.Q(j.Command)
	}
	return "", "false"
}

func compressStage(compression string) (stage, bool) {
	switch compression {
	case "gzip":
		return stage{"compress", "gzip -c"}, true
	case "zstd":
		return stage{"compress", "zstd -q -c"}, true
	}
	return stage{}, false
}

func decompressCmd(compression, file string) string {
	switch compression {
	case "gzip":
		return "gzip -dc < " + file
	case "zstd":
		return "zstd -q -dc < " + file
	}
	return "cat < " + file
}

// backupStages returns the setup and stages producing the (compressed)
// payload on stdout.
func backupStages(j *Job, compression string) (string, []stage) {
	pre, cmd := producer(j)
	stages := []stage{{"dump", cmd}}
	if c, ok := compressStage(compression); ok {
		stages = append(stages, c)
	}
	return pre, stages
}

// streamScript: the payload is written to stdout for the app to store.
func streamScript(j *Job, compression string) string {
	pre, stages := backupStages(j, compression)
	return pipelineScript(pre, stages, "", "", "")
}

// directScript writes the payload to dst on the server itself and prints
// "SMBK <sha256> <size>" on success. Nothing is left under dst on failure.
func directScript(j *Job, compression, dst string) string {
	pre, stages := backupStages(j, compression)
	dir := dst[:strings.LastIndex(dst, "/")]
	pre += "\nmkdir -p -- " + core.Q(dir) + " || exit 1\n__out=" + core.Q(dst+".part")
	onOK := `[ -s "$__out" ] || { echo "backup: the backup command produced no output" >&2; rm -f -- "$__out"; exit 1; }
__sum=$(sha256sum < "$__out" | cut -d ' ' -f 1) || { rm -f -- "$__out"; exit 1; }
__sz=$(wc -c < "$__out" | tr -d ' ') || { rm -f -- "$__out"; exit 1; }
mv -f -- "$__out" ` + core.Q(dst) + ` || { rm -f -- "$__out"; exit 1; }
printf 'SMBK %s %s\n' "$__sum" "$__sz"`
	return pipelineScript(pre, stages, `"$__out"`, `rm -f -- "$__out"`, onOK)
}

// toolsScript prints the versions of the tools a job uses ("name=version").
func toolsScript(j *Job) string {
	tools := [][2]string{}
	switch j.Type {
	case TypeFiles:
		tools = append(tools, [2]string{"tar", "tar --version"})
	case TypePostgres:
		if j.Database == "" {
			tools = append(tools, [2]string{"pg_dumpall", "pg_dumpall --version"})
		} else {
			tools = append(tools, [2]string{"pg_dump", "pg_dump --version"})
		}
	case TypeMySQL:
		tools = append(tools, [2]string{"mysqldump", `"$(command -v mariadb-dump || command -v mysqldump)" --version`})
	case TypeVolume:
		tools = append(tools, [2]string{"docker", "docker --version"}, [2]string{"tar", "tar --version"})
	case TypeRedis:
		tools = append(tools, [2]string{"redis-cli", "redis-cli --version"})
	}
	var b strings.Builder
	b.WriteString("for __t in gzip zstd sha256sum; do command -v $__t >/dev/null 2>&1 && echo \"have=$__t\"; done\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "__v=$(%s 2>/dev/null | head -n 1) && [ -n \"$__v\" ] && echo \"%s=$__v\"\n", t[1], t[0])
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// ---- restore ----

// restoreStage returns setup code and the command that consumes the payload
// on stdin. opts are validated.
func restoreStage(j *Job, m *Manifest, o *RestoreOptions) (pre, cmd string) {
	switch m.Type {
	case TypeFiles:
		if o.Target == "original" {
			return tarPre, "tar -xpf - -C /"
		}
		return tarPre + "\nmkdir -p -- " + core.Q(o.Dir) + " || exit 1", "tar -xpf - -C " + core.Q(o.Dir)
	case TypePostgres:
		pre = asUserFn
		if j.AuthMode == "password" {
			pre += "\n" + readPwPG
		}
		if m.Database == "" || m.DumpFormat == "sql" {
			return pre, pgCmd(j, "psql -X -q -d postgres")
		}
		db := core.Q(o.Database)
		lit := strings.ReplaceAll(o.Database, "'", "''")
		if o.CreateDB {
			pre += "\n__ex=$(" + pgCmd(j, "psql -X -tAc "+core.Q("SELECT 1 FROM pg_database WHERE datname='"+lit+"'")+" -d postgres") + ") || exit 1\n" +
				"if [ \"$__ex\" != 1 ]; then echo \"restore: creating database " + o.Database + "\" >&2; " + pgCmd(j, "createdb") + " " + db + " || exit 1; fi"
		}
		return pre, pgCmd(j, "pg_restore --clean --if-exists --no-owner") + " -d " + db
	case TypeMySQL:
		pre = mysqlCliFind
		if j.AuthMode == "password" {
			pre += "\n" + readPwMy
		}
		cli := `"$__m"` + mysqlConnArgs(j)
		if m.Database == "" {
			return pre, cli
		}
		if o.CreateDB {
			pre += "\n" + cli + " -e " + core.Q("CREATE DATABASE IF NOT EXISTS `"+o.Database+"`") + " || exit 1"
		}
		return pre, cli + " " + core.Q(o.Database)
	case TypeVolume:
		pre = tarPre + "\n" + `command -v docker >/dev/null 2>&1 || { echo "restore: docker not found" >&2; exit 127; }
docker volume inspect ` + core.Q(o.Volume) + ` >/dev/null 2>&1 || { echo "restore: creating volume ` + o.Volume + `" >&2; docker volume create ` + core.Q(o.Volume) + ` >/dev/null || exit 1; }
__mp=$(docker volume inspect -f '{{.Mountpoint}}' ` + core.Q(o.Volume) + `) || exit 1
[ -d "$__mp" ] || { echo "restore: volume directory $__mp not found on this host" >&2; exit 1; }`
		if o.ClearVolume {
			pre += "\n" + `echo "restore: clearing $__mp" >&2; rm -rf -- "$__mp"/* "$__mp"/.[!.]* "$__mp"/..?* 2>/dev/null; true`
		}
		return pre, `tar -xpf - -C "$__mp"`
	case TypeRedis:
		if o.Target == "replace" {
			if j.AuthMode == "password" {
				pre = readPwRedis + "\n"
			}
			pre += redisCliFn(j) + `
__aof=$(__rc CONFIG GET appendonly | sed -n 2p)
[ "$__aof" = yes ] && { echo "restore: redis uses AOF (appendonly yes); the RDB file would be ignored. Disable AOF or restore manually." >&2; exit 3; }
` + redisDumpPathSh(j) + `
__svc=; for __n in redis-server redis valkey-server valkey; do systemctl cat "$__n.service" >/dev/null 2>&1 && { __svc=$__n; break; }; done
[ -n "$__svc" ] || { echo "restore: redis systemd service not found" >&2; exit 1; }
__own=$(stat -c %U:%G -- "$__f" 2>/dev/null || echo redis:redis)
echo "restore: stopping $__svc" >&2; systemctl stop "$__svc" || exit 1
[ -f "$__f" ] && cp -p -- "$__f" "$__f.before-restore" && echo "restore: previous file kept as $__f.before-restore" >&2`
			return pre, `cat > "$__f.restore" && chown "$__own" "$__f.restore" && chmod 640 "$__f.restore" && mv -f -- "$__f.restore" "$__f" && echo "restore: starting $__svc" >&2 && systemctl start "$__svc"`
		}
		return "mkdir -p -- " + core.Q(o.Dir) + " || exit 1", "cat > " + core.Q(o.Dir+"/dump.rdb")
	case TypeCustom:
		if o.Target == "command" && j.RestoreCommand != "" {
			return "", "sh -c " + core.Q(j.RestoreCommand)
		}
		return "mkdir -p -- " + core.Q(o.Dir) + " || exit 1", "cat > " + core.Q(o.Dir+"/"+restoreFileName(m))
	}
	return "", "false"
}

// restoreFileName is the plain payload name ("20260101-000000.sql").
func restoreFileName(m *Manifest) string {
	n := m.Object
	n = strings.TrimSuffix(n, ".age")
	n = strings.TrimSuffix(n, ".gz")
	n = strings.TrimSuffix(n, ".zst")
	return n
}

// restoreScript applies the payload stored in file (a quoted shell word).
func restoreScript(j *Job, m *Manifest, o *RestoreOptions, file string) string {
	pre, cmd := restoreStage(j, m, o)
	return pipelineScript(pre, []stage{{"read", decompressCmd(m.Compression, file)}, {"restore", cmd}}, "", "", "")
}
