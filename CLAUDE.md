# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build
go build -o forge ./cmd/forge

# Build and run
go run ./cmd/forge

# Run tests
go test ./...

# Run a single package's tests
go test ./internal/pipeline/...

# Run with verbose output
go test -v ./internal/state/...

# Vet
go vet ./...
```

## CLI Quick Reference

```bash
forge up                              # Start the daemon
forge down                            # Stop the daemon
forge status                          # Show daemon status (via IPC)
forge hearth                          # Open TUI dashboard
forge anvil add <name> <path>         # Register a repository
forge anvil list                      # List registered anvils
forge anvil remove <name>             # Deregister an anvil
forge queue list                      # Show queued beads
forge queue run <id>                  # Manually dispatch a bead
forge queue clarify <id>              # Mark bead as needing clarification
forge queue unclarify <id>            # Clear clarification flag
forge queue retry <id>                # Reset dispatch circuit breaker
forge history                         # Show recent worker history
forge history events                  # Show event log
forge scan                            # Run govulncheck on Go anvils
forge autostart install               # Enable auto-start via Windows Task Scheduler
forge doctor                          # Check dependencies (bd, claude, gh, git)
forge changelog assemble              # Assemble changelog.d into CHANGELOG.md
forge changelog validate <bead-ids>   # Check fragments exist for beads
```

## Architecture

Forge is a **Go orchestrator daemon** that autonomously drives Claude Code agents across multiple git repositories. It uses a blacksmith metaphor throughout.

### Component Map

| Package | Role |
|---------|------|
| `internal/daemon` | Main background process. Runs the poll loop, manages IPC server, hot-reloads config |
| `internal/pipeline` | Orchestrates one bead through Schematic → Smith → Temper → Warden |
| `internal/smith` | Spawns `claude` CLI as a subprocess in a worktree |
| `internal/temper` | Runs build/lint/test checks; auto-detects Go, .NET, Node |
| `internal/warden` | Spawns a second Claude session to review Smith's diff |
| `internal/bellows` | Monitors open PRs for CI failures, review comments, and merge conflicts |
| `internal/schematic` | Pre-analysis worker — decomposes complex beads or produces implementation plans |
| `internal/poller` | Calls `bd ready` to get available beads from an anvil |
| `internal/worktree` | Creates/removes `git worktree` branches for each bead |
| `internal/state` | SQLite at `~/.forge/state.db` — workers, prs, events, retries, costs |
| `internal/ipc` | Named pipe (Windows) / Unix socket daemon↔CLI protocol; newline-delimited JSON |
| `internal/hearth` | Bubbletea TUI: three-column layout (Queue+ReadyToMerge+NeedsAttention / Workers / LiveActivity+Events) |
| `internal/config` | Viper config loading — `forge.yaml` in cwd or `~/.forge/config.yaml` |
| `internal/prompt` | Builds the Smith prompt from bead metadata + AGENTS.md/CLAUDE.md/README.md |
| `internal/hotreload` | fsnotify watcher — reloads `forge.yaml` without restart |
| `internal/notify` | MS Teams Adaptive Card webhooks |
| `internal/shutdown` | Graceful shutdown: SIGINT drain, orphan worktree cleanup |
| `internal/autostart` | Windows Task Scheduler integration |
| `cmd/forge` | Cobra CLI — subcommands wired to daemon/state/ipc |

### Data Flow

```
bd ready (poller) → pipeline.Run()
  → worktree.Create (git worktree add)
  → schematic.Analyze (optional pre-analysis: plan, decompose, or skip)
  → smith.Spawn (claude CLI subprocess, reads prompt from prompt.Builder)
  → temper.Run (go build/vet/test or dotnet or npm)
  → warden.Review (second claude session, reviews diff)
  → if request_changes: loop back to Smith (max max_review_attempts iterations)
  → if approved: ghpr.Create (gh pr create)
  → bellows monitors open PRs (CI fix, review fix, rebase)
  → worktree.Remove
```

### State Database

`~/.forge/state.db` (SQLite with WAL mode) tracks:
- **workers** — Smith process lifecycle with PID, status, log path
- **prs** — Pull requests created across anvils
- **events** — Timestamped event log (bead_claimed, smith_done, warden_pass, etc.)
- **retries** — Exponential backoff tracking; `needs_human=1` after exhausting retries
- **bead_costs / daily_costs** — Token usage and USD estimates per bead and per day

### IPC Protocol

The daemon exposes a named pipe (Windows: `\\.\pipe\forge`) or Unix socket. Messages are newline-delimited JSON `Command`/`Response` structs. Supported commands: `status`, `kill_worker`, `refresh`, `queue`, `subscribe` (event stream).

### Configuration

Config resolution order: `--config` flag → `./forge.yaml` → `~/.forge/config.yaml`. Environment variables override with `FORGE_` prefix (e.g. `FORGE_SETTINGS_MAX_TOTAL_SMITHS=4`). The daemon hot-reloads the config file on change via fsnotify. See [docs/configuration.md](docs/configuration.md) for the full settings reference including `daily_cost_limit`, `max_ci_fix_attempts`, `max_review_fix_attempts`, `max_rebase_attempts`, `smith_providers`, `merge_strategy`, `schematic_enabled`, and more.

### Per-Anvil Smith Prompt Customization

Place a template file at `<anvil-path>/.forge/prompt.tmpl` or `.forge/smith-prompt.tmpl` to override the default Smith prompt for that repo. The template receives `{{.Bead}}`, `{{.AgentsMD}}`, `{{.ClaudeMD}}`, `{{.ReadmeMD}}`.

## Beads Database — kubectl port-forward ONLY

Forge's beads DB connects via kubectl port-forward to the AKS pod `tn-heimdall/dolt-beads` on **port 3306**.

- ❌ Never run `dolt sql-server` locally on port 3306
- ❌ Never run `start-dolt-server.ps1` (offline fallback only, uses port 3307)
- ❌ A local dolt on port 3306 will conflict with the port-forward and break `bd` with "Access denied"

**Auto-start is permanently disabled** via `.beads/config.yaml` (`dolt.auto-start: false`).
Without this, beads auto-starts a local dolt when the port-forward drops and spawns an
idle-monitor watchdog that restarts it even after manual kills. Do not remove that setting.

- ✅ If `bd` returns "Access denied", restart the port-forward:
  ```powershell
  kubectl port-forward -n tn-heimdall svc/dolt-beads 3306:3306
  ```

## Issue Tracking

All task tracking uses **bd (beads)**. See `AGENTS.md` for the full workflow. Key commands:

```bash
bd ready            # Find available work
bd show <id>        # View issue details
bd update <id> --status=in_progress
bd close <id>
```

## Shell Safety (on Windows)

Always use non-interactive flags to avoid hanging on prompts:
```bash
cp -f source dest    # NOT: cp source dest
rm -f file           # NOT: rm file
rm -rf dir           # NOT: rm -r dir
```
