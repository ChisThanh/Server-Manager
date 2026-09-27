## What and why

<!-- What does this change, and why is it needed? Link the issue: Fixes #123 -->

## How it was tested

<!-- Unit / integration tests, distro(s) tried, screenshots for UI changes -->

## Checklist

- [ ] `go test -race ./...` passes
- [ ] Integration tests for server-changing code (`-tags integration`)
- [ ] `scripts/check.sh` passes (build, bindings, TypeScript)
- [ ] New text goes through i18n (`scripts/i18n-check.py`)
- [ ] Remote commands quote values with `core.Q()`; changes are permission-checked and audited
