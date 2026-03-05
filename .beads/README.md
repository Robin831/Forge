# Beads Issue Tracker Setup (Windows)

This project uses [beads](https://github.com/steveyegge/beads) for issue tracking, backed by a shared Dolt SQL server running in AKS (`tn-heimdall/dolt-beads`), database `forge`.

> **Note:** The Dolt CLI is only needed for the one-time identity configuration in step 1 below. It is not required for day-to-day `bd` usage.

## Prerequisites

**Install Go** (to build `bd` from source):

1. Download and install Go from https://go.dev/dl/ (1.21+)
2. Verify: `go version`

**Install bd (beads CLI):**

Since the official release doesn't include the server mode fix for pure-Go builds, build from source:

```powershell
git clone https://github.com/steveyegge/beads.git
cd beads

$env:CGO_ENABLED=0
go build -o $env:USERPROFILE\go\bin\bd.exe .\cmd\bd\

# Verify
bd version
```

**Install Dolt** (CLI — needed for the one-time identity setup in step 1):

1. `winget install DoltHub.Dolt` or download from https://github.com/dolthub/dolt/releases
2. Verify: `dolt version`

**Install kubectl** (to access the shared Dolt server):

- Part of the standard FHI developer setup. Verify: `kubectl version --client`

## First-Time Setup

### 1. Configure Dolt identity (one-time)

`bd` uses your git identity for Dolt commits. Set it once:

```powershell
dolt config --global --add user.name (git config user.name)
dolt config --global --add user.email (git config user.email)
```

> Note: `dolt` only needs to be installed for this one-time step. It is not needed for normal `bd` usage.

### 2. Set the server password

The shared Dolt server requires a password. Set it as a persistent user environment variable:

```powershell
[System.Environment]::SetEnvironmentVariable('BEADS_DOLT_PASSWORD', '<password>', 'User')
```

Ask a team member for the password. Then open a new terminal so the variable is loaded.

### 3. Start the port-forward

`bd` connects to the shared server via `kubectl port-forward`. Run this in a background job or dedicated terminal:

```powershell
kubectl port-forward svc/dolt-beads 3306:3306 -n tn-heimdall
```

**To start automatically on every terminal session**, add to `$PROFILE`:

```powershell
$env:BEADS_DOLT_PASSWORD = [System.Environment]::GetEnvironmentVariable('BEADS_DOLT_PASSWORD', 'User')

if (-not (Get-Job | Where-Object { $_.Command -like '*dolt-beads*' -and $_.State -eq 'Running' })) {
    Start-Job -ScriptBlock { kubectl port-forward svc/dolt-beads 3306:3306 -n tn-heimdall } | Out-Null
}
```

### 4. Clone and initialize

```powershell
git clone https://github.com/folkehelseinstituttet/Forge.git
cd Forge

# Install git hooks for automatic JSONL sync
bd hooks install
```

> **Note:** With Dolt server mode, sync is automatic. No manual `bd sync` needed after clone.

### 5. Verify

```powershell
bd list   # Should show Forge issues
```

---

## Daily Usage

The port-forward must be running before using `bd` commands. If you added it to `$PROFILE` (step 3), it starts automatically.

### Check for ready work

```powershell
bd ready
```

### Create and manage issues

```powershell
bd create "Issue title" -t bug -p 2 --description "Context"
bd update <id> --status in_progress
bd close <id> --reason "Done"
```

### Common commands

```powershell
bd list                    # Show all issues
bd ready                   # Show unblocked work
bd show <id>               # Show issue details
bd --help                  # Full command reference
```

### Sync

Sync is **automatic** via git hooks:

- **pre-commit**: Exports Dolt → `issues.jsonl`
- **post-merge**: Imports `issues.jsonl` → Dolt after `git pull`
- **post-checkout**: Imports JSONL after branch switches

Run `bd dolt pull` to pull remote changes, or `bd dolt push` to push local changes, if something seems out of sync.

### Parallel development with worktrees

Use `bd worktree` to work on multiple feature branches at once while sharing this Dolt database:

```powershell
bd worktree create .worktrees/<name> --branch feature/<name>
bd worktree list
bd worktree remove .worktrees/<name>
```

Each worktree gets a `.beads/redirect` file that points back to this directory, so both checkouts connect to the same `tn-heimdall/dolt-beads` server automatically.

**Always use `bd worktree`**, not `git worktree add` directly — the redirect file is only created by `bd worktree create`.

---

## Troubleshooting

### "Dolt server unreachable at 127.0.0.1:3306"

The port-forward isn't running. Start it:

```powershell
kubectl port-forward svc/dolt-beads 3306:3306 -n tn-heimdall
```

### Authentication errors

Check that `BEADS_DOLT_PASSWORD` is set in the current session:

```powershell
$env:BEADS_DOLT_PASSWORD   # Should print the password
```

If empty, re-open the terminal (if set via `$PROFILE`) or set it manually:

```powershell
$env:BEADS_DOLT_PASSWORD = "<password>"
```

### Git hooks not working

```powershell
bd hooks install --force
```

### Issues out of sync

```powershell
bd dolt pull
bd doctor --fix
```

---

## Architecture Notes

- **Database**: Shared Dolt SQL server in AKS (`tn-heimdall/dolt-beads`), database `forge`
- **JSONL Export**: `.beads/issues.jsonl` (version controlled, synced via hooks)
- **Access**: `kubectl port-forward svc/dolt-beads 3306:3306 -n tn-heimdall`
- **No CGO**: The `bd` binary is pure Go — server mode doesn't require CGO
- **Shared server**: All developers connect to the same Dolt instance. Multiple concurrent connections are supported.

## References

- beads: https://github.com/steveyegge/beads
- Dolt: https://github.com/dolthub/dolt
- Full bd CLI reference: `bd --help` or docs in beads repo
