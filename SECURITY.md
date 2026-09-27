# Security Policy

Server Manager holds SSH credentials and runs commands as root on your servers, so security reports are taken seriously.

## Reporting a vulnerability

**Please do not open a public issue.** Use GitHub's private reporting instead: go to the repository's **Security** tab → **Report a vulnerability**.

Include what you can:

- what an attacker could do and under which conditions,
- steps to reproduce or a proof of concept,
- the version / commit and your OS.

You should get a first reply within a few days. Once a fix is ready, it will be released and you will be credited in the release notes (unless you prefer not to be).

## Scope

In scope, for example:

- command injection through any value sent to a server,
- secrets leaking to disk, logs, the audit log or the process list,
- host-key verification bypasses,
- permission (role) checks that can be bypassed in the backend,
- guard rails that fail to stop an action which locks the user out of a server.

Out of scope: vulnerabilities in the servers you manage, in third-party tools the app drives (OpenSSH, ufw, Docker…), or issues that require an attacker who already controls your computer.

## Supported versions

The project is in alpha; only the latest `main` branch receives fixes.
