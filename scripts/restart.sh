#!/usr/bin/env bash
# restart.sh — Source-of-truth for the Forge daemon rebuild/restart script.
#
# Deployed to ~/.forge/restart.sh on the production host. Hytte's Mezzanine
# "Rebuild & Restart" button POSTs to /api/forge/restart, which shells out to
# this script via RestartForgeHandler.
#
# The running daemon continuously rewrites .beads/issues.jsonl in its working
# tree (bd auto-export), so a plain `git pull` always trips
# "local changes would be overwritten by merge" and aborts. We stash only
# that file before pulling and drop the specific stash ref after; any other
# dirty tracked files are unexpected and we abort to avoid data loss.

set -euo pipefail

echo "==> Pulling latest..."
cd ~/source/Forge

# Self-heal: the running daemon re-sets core.worktree on every worker spawn,
# leaving the main repo in a "fatal: this operation must be run in a work tree"
# state. Unset it before pulling. Once the new binary is live (Forge-d6j5 fix),
# this is redundant but harmless.
git config --unset core.worktree 2>/dev/null || true

# Fail fast if any tracked dirty files exist beyond .beads/issues.jsonl.
if git diff --name-only | grep -qv '^\.beads/issues\.jsonl$' 2>/dev/null \
|| git diff --cached --name-only | grep -qv '^\.beads/issues\.jsonl$' 2>/dev/null; then
  echo "ERROR: Unexpected dirty files — aborting to prevent data loss:" >&2
  { git diff --name-only; git diff --cached --name-only; } | sort -u \
    | grep -v '^\.beads/issues\.jsonl$' >&2 || true
  exit 1
fi

STASH_REF=""
if ! git diff --quiet -- .beads/issues.jsonl \
|| ! git diff --cached --quiet -- .beads/issues.jsonl; then
  STASH_MSG="restart-auto-stash-$(date +%s)"
  git stash push -m "$STASH_MSG" -- .beads/issues.jsonl
  # Record the exact ref so we don't accidentally drop an unrelated stash.
  STASH_REF=$(git stash list \
    | awk -v msg="$STASH_MSG" 'index($0, msg) {sub(/:.*/, "", $1); print $1; exit}')
fi
git pull --ff-only origin main
if [ -n "$STASH_REF" ]; then
  git stash drop "$STASH_REF" || true
fi

echo "==> Building..."
go build -o ~/bin/forge ./cmd/forge

echo "==> Restarting daemon..."
sudo systemctl restart forge

echo "==> Done. Status:"
sudo systemctl status forge --no-pager -l | head -10
