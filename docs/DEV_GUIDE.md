# Developer guide — feature modules

Server Manager is a Wails v3 desktop app (Go backend + React frontend) that
manages Linux servers **agentlessly over SSH**: nothing is installed on the
server by the app itself; every feature uses tools that already exist there
(`/proc`, `systemctl`, `journalctl`, `docker`, `nginx`, `psql`…) through SSH
exec / SFTP. If a tool is missing, detect it and tell the user (optionally
offer to install it with the distro package manager as an explicit,
audited action).

## Layout

```
main.go                      registers every service (already done for all modules)
internal/core                shared state + helpers (DO NOT EDIT — ask the lead)
internal/db                  SQLite (kv store, metrics, events, audit, alerts, runs)
internal/sshx                SSH/SFTP connections (Conn.Exec, Conn.Stream, Conn.SFTP)
internal/apperr              coded errors (translated by the frontend)
internal/testutil            integration test helpers
services/                    core services: Server, File, Terminal, System, App
services/<module>/           one Go package per feature module (you own yours)
frontend/src/components/<module>/   your UI; index.tsx default-exports the panel
frontend/src/i18n/modules/<module>.ts  your strings
frontend/src/ui/             shared UI kit (DO NOT EDIT — ask the lead)
```

Module ownership: `docker`, `logs` (+ `services/system_service.go` service
parts and `components/system/Services.tsx`), `security`, `web`, `database`,
`backup`, `deploy`, `command` (+ `components/pages/CommandCenter.tsx`).
`monitor`, `overview`, `monitoring`, global pages and everything shared
belong to the lead. **Only edit files you own.** If you need a change in a
shared file, write it down in your final report instead.

## Backend conventions

Your package already contains `type XService struct{ core *core.Core }` and
`New(c *core.Core)`; `main.go` registers it. Every exported method becomes a
TypeScript function in `frontend/bindings/server-manager/services/<module>/`.

- **Signatures**: first parameter is the server id `connID string`. Methods
  that may need root take a trailing `sudoPassword string` (the frontend
  passes "" first and prompts only if the backend returns `sudo.required` /
  `sudo.wrongPassword`). Return plain structs (JSON tags in camelCase) and
  `error`. Never return nil slices meant to be arrays — use `[]T{}`.
- **Connection**: `conn, err := s.core.Conn(connID)` (UI connection). Code that
  runs without the UI (schedulers, command center) uses `s.core.AnyConn(ctx, id)`.
- **Commands** (always pass a context with timeout, `core.Timeout(30*time.Second)`):
  - `s.core.Run(ctx, conn, cmd, sudo, pw, stdin)` → `sshx.ExecResult{Stdout, Stderr, ExitCode}`;
    non-zero exit is NOT an error. `cmd` is a sh snippet (wrapped in `sh -c`).
  - `s.core.RunOK(...)` same but non-zero exit → `cmd.failed` error with output.
  - `s.core.RunAuto(ctx, conn, cmd, pw, stdin)` tries without sudo, retries with
    sudo on a permission error. Only for read-only or all-or-nothing commands.
  - `s.core.IsRoot(ctx, conn)`; sudo password resolution/caching is automatic.
- **Blocking / allowing IPs**: other modules call `SecurityService.AccessBlock` /
  `AccessAllow` (`services/security/access.go`) instead of writing firewall
  rules themselves: they pick the right tool (firewalld › ufw › iptables, or a
  fail2ban ban when `Temporary`), put block/allow rules first, never block the
  session's own address or an allowlisted one, and mark their rules with an
  `sm:` / `sm-block:` / `sm-allow:` comment so the UI shows them as the app's.
- **Quoting / injection**: every value interpolated into a shell command
  goes through `core.Q(v)` (POSIX single-quote). Validate identifiers
  (container ids, unit names, db names, domains, ports…) with a strict regexp
  *before* use and return a coded error when invalid. Never pass secrets on
  the command line (visible in `ps`) — feed them on stdin, e.g.
  `IFS= read -r PGPASSWORD; export PGPASSWORD; psql …` with `stdin = pw+"\n"`.
- **Writing root-owned files**: write via `cat > tmp` through `s.core.Run(..., sudo=true, stdin=content)`,
  validate (e.g. `nginx -t`), then `mv` into place; keep a backup and restore
  it if validation fails. Preserve owner/mode where it matters.
- **Permissions**: before any change call
  `if err := s.core.Require(connID, core.PermDocker); err != nil { return …, err }`
  (see `internal/core/policy.go` for the perms). Reading never needs a perm.
- **Audit**: after every change, `s.core.Audit(connID, "docker.container.stop", target, detail, err)`
  (action = `<module>.<object>.<verb>`; detail = what changed, never secrets).
  This also adds an entry to the server's event timeline.
- **Timeline**: noteworthy non-user events (deploy finished, backup failed,
  certificate renewed) → `s.core.AddEvent(core.Event{Server, Kind, Severity, Code, Params})`;
  Kind ∈ alert|conn|service|container|deploy|backup|action|security|health,
  Severity ∈ info|ok|warn|crit, Code is translated by the frontend as `ev.<code>`
  (add `"ev.<code>"` keys to your i18n module; params are `{name}` placeholders).
- **Long-running work** (compose up, deploy, backup, certbot, apt): run it as
  a job so output streams live and the user can cancel:
  ```go
  j := s.core.Jobs.Start(connID, "docker.compose.up", title, func(ctx context.Context, j *core.Job) error {
      j.Step("Pulling images")
      return s.core.JobRun(ctx, j, conn, "docker compose -f "+core.Q(file)+" up -d", sudo, pw)
  })
  return j.ID(), nil   // frontend: runJob(title, () => Service.X(...))
  ```
  `JobExec` returns the exit code; `JobRun` errors on non-zero. `j.Logf`,
  `j.Step`, `j.SetResult`. Job ctx is cancelled by the user.
- **Errors**: return `apperr.New("docker.notInstalled")`,
  `apperr.New("docker.invalidName", "name", n)`, `apperr.Wrap(err, "...")`,
  `.WithDetail(stderr)`. Codes are `<module>.<name>`; add `"err.<module>.<name>"`
  to your i18n module (vi + en). Never put user-facing text in Go.
- **Persistence**: config objects (apps, jobs, destinations, saved queries…)
  go in the kv store: `s.core.DB.Put("<module>.<kind>", id, v)`, `Get`,
  `Delete`, `db.List[T](s.core.DB, ns)`. Run history uses the `runs` table
  (`id, kind, server, ref, started, finished, status, actor, version, data, log`).
  Secrets (tokens, passwords, passphrases) go in the OS keychain with
  `store.SetKeychain("<module>:<id>:<name>", v)` / `store.Keychain(name)`,
  never in the DB; expose only a `hasX bool` to the UI.
- **Background work**: implement `ServiceStartup(ctx context.Context, _ application.ServiceOptions) error`
  / `ServiceShutdown() error` on your service if you need goroutines
  (e.g. a scheduler). Stop them on shutdown.
- **Events to the frontend**: `core.Emit("<module>:<name>", payload)`; register
  the payload type in your package `init()` with
  `application.RegisterEvent[T]("<module>:<name>")` so it is typed in TS.
- **Detect, don't assume**: check tool availability (`command -v docker`),
  handle distro differences (Debian/Ubuntu, RHEL/Rocky/Alma, Alpine/BusyBox,
  systemd vs not), and return a clear coded error for unsupported setups.

## Frontend conventions

- Panel: `export default function XPanel({ connId, visible, connected, navigate, arg }: PanelProps)`
  (`ui/types.ts`). Poll only while `visible && connected`.
- Bindings: `import { DockerService } from "../../../bindings/server-manager/services/docker";`
  and types from the same place (`models.ts`) or `.../internal/core`.
- UI kit (`src/ui/`): `Page`, `PageHeader`, `Section`, `Loading`, `ErrorBox`,
  `Empty`, `KV` (Page.tsx); `SubTabs` (Tabs.tsx); `StateBadge`, `Dot`, `Bar`,
  `toneOf` (Status.tsx); `TimeChart` (uPlot); `CodeEditor` (Monaco, for YAML/
  nginx/SQL); `JobLog`; `runJob(title, start)` / `watchJob` (jobs.tsx: modal
  with live output + cancel); `confirmDanger({serverId, title, message, confirmText})`
  (typed confirmation on production); `confirmMany`; hooks `useRemote(load, deps, {enabled, poll})`,
  `useInterval`, `useBusy` (hooks.ts); `useCan(connId, "docker")`, `can()`,
  `useServer(connId, visible)` (perm.ts).
- Dropdowns: always `Select` from `ui/Select.tsx`, never a native `<select>`:
  `<Select value={v} onChange={setV} options={["a", "b", { value: "c", label: t("…"), hint, icon, group, disabled }]} />`.
  Values may be strings or numbers; `onChange` receives the value itself.
  A search box appears past 8 options (`searchable` forces it on/off; search
  ignores Vietnamese accents); `group` adds headers; `size="sm"` (26px),
  `block` (full width, automatic inside `.field`), `ghost` (status bars),
  `mono`, `invalid`, `placeholder`. Arrow keys / type-ahead work on the
  closed trigger; Escape inside a `Modal` closes only the dropdown.
- Store helpers: `withSudo(connId, pw => Service.X(connId, …, pw))` prompts
  for the sudo password when needed (`store/sudo.ts`); `toast(msg, "success"|"error")`,
  `confirmDialog`, `promptDialog`, `useUI().openDialog({fields…})` (`store/ui.ts`);
  `Modal` (`components/Overlays.tsx`); `errMsg(e)` (`lib/api.ts`);
  `formatBytes`, `formatDuration`, `formatDate` (`lib/format.ts`).
- Existing CSS classes to reuse: `btn` (`primary`, `danger`, `ghost`, `sm`),
  `icon-btn`, `input`, `input-sm`, `field` + `label`, `row`, `form-grid`,
  `toolbar`, `table-wrap` + `table.grid` (`th.num`, `td.num`, `td.cmd`, `th.sortable`),
  `cards` + `card` (`.k .v .s`), `panel-box` + `box-title`, `dash-cols`,
  `chart-grid` + `chart-card`, `list-rows` + `list-row` (`clickable`), `chips` + `chip`,
  `segmented`, `badge`, `ui-badge`, `muted`, `mono`, `log-view`, `hint`, `hint-box`,
  `spinner`, `center-msg`, `check`. Dark theme variables: `--bg*`, `--fg*`,
  `--accent`, `--ok`, `--warn`, `--err`, `--border`, `--radius`, `--mono`.
  Put module-specific CSS in `components/<module>/<module>.css` (import it from
  your index.tsx) and prefix its classes with the module name.
- i18n: all visible text via `const t = useT(); t("docker.title")`. Your keys
  live in `i18n/modules/<module>.ts` (`defineModule({ vi: {...}, en: {...} })`),
  must start with `<module>.` (or `err.<module>.` / `ev.<code>` / `audit.<module>.`),
  Vietnamese is the default language — write natural Vietnamese first, then English.
  The other 8 languages live in `i18n/lang/<code>.ts` (one file per language,
  loaded only when that language is selected); TypeScript fails the build until
  every new key is translated there. Workflow: `scripts/i18n-tr.py export` →
  translate `_tr/source.json` into `_tr/<code>.json` → `scripts/i18n-tr.py import <code>`,
  then `scripts/i18n-check.py` and `scripts/codes-check.py`.
- Performance: long lists (logs, result grids, >200 rows) render through
  `ui/VirtualRows` (only visible rows are in the DOM). Heavy libraries load
  lazily: use `ui/CodeEditor` (Monaco loads on first use) and `JobLog`/`runJob`
  (xterm); don't import `monaco-editor` or `@xterm/xterm` from eagerly loaded
  code. Panels are kept mounted when hidden, so gate polling on `visible` and
  use `useSample(id, visible)` / `useServer(id, visible)` / `useFrozen(value, visible)`
  so store updates don't re-render a hidden panel. Workspace wraps each panel
  in `memo`, so keep props passed to panels stable. Tables that refresh on a
  timer render rows through a `memo` row component with primitive props and one
  stable callback (see `ServiceRow` in `system/Services.tsx`), so a poll only
  re-renders the rows whose values changed.
- Audit action labels: add `"audit.<action>"` keys (e.g. `"audit.docker.container.stop": "Dừng container"`)
  so the audit log shows readable text.
- Dangerous actions (stop, delete, restore, firewall changes…) must ask with
  `confirmDanger`. Hide/disable change buttons when `!useCan(connId, perm)`.
- Long lists: filter box, sortable columns, empty state, loading state,
  error state with retry. Never leave the user without feedback.

## Build, check, test

```bash
scripts/check.sh                                   # go build + regenerate bindings + tsc (locked; safe in parallel)
go vet ./services/<module>/
go test -tags integration -race -count=1 ./services/<module>/ -v
```

`tsc` reports errors from other modules too — only fix yours. If binding
generation fails because another package doesn't compile, wait a minute and
rerun. Don't run `wails3 build`/`wails3 dev` and don't edit `main.go`.

Test servers (Docker, already running; user `tester` / `secret123`, sudo with
password): `testutil.FullPort()` = Debian 12 + systemd with nginx, caddy
(disabled), ufw, iptables, nftables, postgresql, mariadb, redis, certbot, git,
fail2ban (disabled), rsyslog; `testutil.DindPort()` = Alpine with Docker 27 +
Compose (tester is in the docker group). `c, id := testutil.Connect(t, port)`
gives an isolated Core connected to it. **The servers are shared with other
developers working in parallel**: name everything you create `smtest-<module>-*`
and remove it at the end; never stop sshd, never enable a firewall without
allowing 22/tcp first and restoring the previous state, never change sshd
settings for real (test config generation + `sshd -t` on a copy), don't
reboot/stop the containers.

Write integration tests (`//go:build integration`) for your backend logic:
parsing of real command output, the happy path of every action, permission
denial (`testutil.SetRole(t, c, id, "viewer")`), and invalid-input rejection.
