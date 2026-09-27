# Contributing to Server Manager

Thanks for taking the time to help! This project was built quickly with a lot of AI assistance, so fresh human eyes are exactly what it needs. Every kind of contribution counts: bug reports, testing on your distro, code review, tests, translations, docs and features.

Tiếng Việt: bạn có thể viết issue và PR bằng tiếng Việt hoặc tiếng Anh đều được.

## Ways to help

- **Try it and report bugs.** Use a staging server, then [open an issue](../../issues/new/choose) with your OS, the server's distro and what happened. Screenshots and the output shown in the app help a lot.
- **Test on more platforms.** Development happens on macOS against Debian, Alpine and AlmaLinux servers. Reports from Windows/Linux desktops and from Ubuntu, Rocky, Fedora, Arch, openSUSE… servers are very valuable.
- **Add tests.** Especially integration tests for code that changes a server (firewall, sshd, users, web configs, backups).
- **Review code.** Security-sensitive areas are `internal/sshx`, `internal/core` and anything that builds shell commands in `services/`.
- **Translate.** The UI ships in 10 languages; most translations were machine-assisted and would benefit from native speakers.
- **Build features** from the roadmap in the README or from issues labelled `help wanted`. For anything big, open an issue first so we can agree on the approach.

## Development setup

Requirements: Go 1.26+, Node.js 20+, the [Wails v3 CLI](https://v3.wails.io/getting-started/installation/) and Docker (for the test servers).

```bash
go install github.com/wailsapp/wails/v3/cmd/wails3@latest
wails3 dev          # run the app with hot reload
```

Read [docs/DEV_GUIDE.md](docs/DEV_GUIDE.md) before writing code — it covers how a module is structured, how remote commands are run and quoted safely, permissions and audit, i18n and the performance rules the UI follows.

### Project layout

```
internal/core       shared state: connections, sudo, permissions, audit, events, jobs, settings
internal/db         SQLite (metrics, audit, events, alerts, run history, settings)
internal/sshx       SSH/SFTP, host keys, keepalive, reconnect, exec/stream
internal/store      server profiles (JSON) + OS keychain
services/           core services (servers, files, terminal, system, app)
services/<module>   monitor, docker, logs, security, web, database, backup, deploy, command
frontend/src/ui     shared UI components (Select, charts, virtual lists, job log, dialogs…)
frontend/src/components/<module>   one folder per module's UI
frontend/src/i18n   core + per-module dictionaries; Vietnamese and English are the source
testenv/            Dockerfiles of the test servers
```

## Tests and checks

Start the test servers (see [testenv/README.md](testenv/README.md) for the full list):

```bash
docker build -t sm-test-full testenv/full && docker run -d --name sm-test-full --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock -p 2225:22 sm-test-full
docker build -t sm-test-dind testenv/dind && docker run -d --name sm-test-dind --privileged -p 2224:22 sm-test-dind
```

Then run everything a PR should pass:

```bash
go test -race ./...                                              # unit tests
go test -race -tags integration ./internal/... ./services/...    # against the test servers
scripts/check.sh                                                 # build + bindings + TypeScript
scripts/i18n-check.py                                            # every key exists in every language
scripts/codes-check.py                                           # every error/audit code is translated
```

Never point the integration tests at a real server: they change firewalls, sshd settings and users.

## Pull requests

1. Fork, create a branch from `main` (`fix/…`, `feat/…`, `docs/…`).
2. Keep a PR focused on one thing; small PRs get reviewed faster.
3. Add or update tests for behaviour you change. For UI changes, add before/after screenshots.
4. New user-visible text goes through i18n: write Vietnamese and English, then run the translation workflow in `docs/DEV_GUIDE.md` (or leave a note in the PR and a maintainer will help with the other languages).
5. Make sure the checks above pass, then open the PR and describe what and why.

### Rules for code that touches servers

- Quote every interpolated value with `core.Q()` and validate identifiers with a strict pattern first.
- Secrets go through stdin, never the command line.
- Anything that changes a server needs a permission check (`core.Require`) and an audit entry (`core.Audit`).
- Anything that could lock the user out (firewall, sshd, keys, users) must detect it and refuse or ask for explicit confirmation.

## Reporting security issues

Please don't open public issues for vulnerabilities — see [SECURITY.md](SECURITY.md).

## Code of conduct

Be kind and constructive. Assume good intent, focus on the problem rather than the person, and help newcomers — many first-time contributors will arrive through this project.

By contributing you agree that your contributions are licensed under the [MIT License](LICENSE).
