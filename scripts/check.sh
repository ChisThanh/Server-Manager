#!/bin/sh
# Regenerates the frontend bindings and type-checks the frontend, holding a
# lock so parallel runs don't clobber each other. Usage: scripts/check.sh
cd "$(dirname "$0")/.." || exit 1
LOCK=/tmp/sm-bindings.lock
i=0
until mkdir "$LOCK" 2>/dev/null; do
  i=$((i+1))
  if [ $i -gt 300 ]; then echo "lock timeout; removing stale lock"; rm -rf "$LOCK"; fi
  sleep 1
done
trap 'rm -rf "$LOCK"' EXIT INT TERM
echo "== go build"; go build . ./internal/... ./services/... 2>&1 | grep -v "ld: warning" 
echo "== bindings"; wails3 generate bindings -clean=false -ts -i 2>&1 | grep -iE "error|warn|Processed" | sed 's/\x1b\[[0-9;]*m//g'
echo "== tsc"; (cd frontend && npx tsc --noEmit 2>&1 | head -80)
echo "== done"
