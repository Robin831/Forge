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
forge queue list                      # Show queued beads
forge history                         # Show recent worker history
forge autostart enable                # Enable auto-start via Windows Task Scheduler
forge doctor                          # Check dependencies (bd, claude, gh, git)
```

## Architecture

Forge is a **Go orchestrator daemon** that autonomously drives Claude Code agents across multiple git repositories. It uses a blacksmith metaphor throughout.

### Component Map

| Package | Role |
|---------|------|
| `internal/daemon` | Main background process. Runs the poll loop, manages IPC server, hot-reloads config |
| `internal/pipeline` | Orchestrates one bead through Smith → Temper → Warden, up to 3 iterations |
| `internal/smith` | Spawns `claude` CLI as a subprocess in a worktree |
| `internal/temper` | Runs build/lint/test checks; auto-detects Go, .NET, Node |
| `internal/warden` | Spawns a second Claude session to review Smith's diff |
| `internal/bellows` | Monitors open PRs for CI failures and review comments |
| `internal/poller` | Calls `bd ready` to get available beads from an anvil |
| `internal/worktree` | Creates/removes `git worktree` branches for each bead |
| `internal/state` | SQLite at `~/.forge/state.db` — workers, prs, events, retries, costs |
| `internal/ipc` | Named pipe (Windows) / Unix socket daemon↔CLI protocol; newline-delimited JSON |
| `internal/hearth` | Bubbletea TUI: three-panel (Queue / Workers / Events) |
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
  → smith.Spawn (claude CLI subprocess, reads prompt from prompt.Builder)
  → temper.Run (go build/vet/test or dotnet or npm)
  → warden.Review (second claude session, reviews diff)
  → if request_changes: loop back to Smith (max 3 iterations)
  → if approved: ghpr.Create (gh pr create)
  → bellows monitors open PRs
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

Config resolution order: `--config` flag → `./forge.yaml` → `~/.forge/config.yaml`. Environment variables override with `FORGE_` prefix (e.g. `FORGE_SETTINGS_MAX_TOTAL_SMITHS=4`). The daemon hot-reloads the config file on change via fsnotify.

### Per-Anvil Smith Prompt Customization

Place a template file at `<anvil-path>/.forge/prompt.tmpl` or `.forge/smith-prompt.tmpl` to override the default Smith prompt for that repo. The template receives `{{.Bead}}`, `{{.AgentsMD}}`, `{{.ClaudeMD}}`, `{{.ReadmeMD}}`.

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
