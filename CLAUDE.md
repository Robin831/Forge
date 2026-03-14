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
forge scan --anvil <name>             # Scan a specific anvil
forge autostart install               # Enable auto-start via Windows Task Scheduler
forge autostart remove                # Remove autostart task
forge autostart status                # Check autostart registration
forge autostart generate              # Generate Task Scheduler XML
forge doctor                          # Check dependencies (bd, claude, gh, git)
forge changelog assemble              # Assemble changelog.d into CHANGELOG.md
forge changelog validate <bead-ids>   # Check fragments exist for beads
forge warden learn --anvil <name>     # Learn review rules from Copilot comments
forge warden list --anvil <name>      # List learned review rules
forge warden forget <id> --anvil <name>  # Remove a learned rule
forge notify release --version v1.2.3  # Send release notification to configured webhooks
forge notify release \
  --version v1.2.3 \
  --tag v1.2.3 \
  --release-url https://github.com/org/forge/releases/tag/v1.2.3 \
  --changelog "- Added X\n- Fixed Y" \
  --webhook-url https://... \
  --extra-url https://...    # --version required; other flags (--tag/--release-url/--changelog/--webhook-url/--extra-url) optional
forge version                         # Print version information
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
| `internal/warden` | Code review agent — validates Smith's diff, learns rules from Copilot comments |
| `internal/bellows` | Monitors open PRs for CI failures, review comments, and merge conflicts |
| `internal/crucible` | Orchestrates parent beads with children on feature branches — auto-detects, sequences, merges |
| `internal/depcheck` | Multi-language dependency update scanner (Go, .NET, Node) — creates beads for outdated deps |
| `internal/vulncheck` | Vulnerability scanning via `govulncheck` — creates prioritized beads |
| `internal/schematic` | Pre-analysis worker — decomposes complex beads or produces implementation plans |
| `internal/cifix` | CI failure fix worker — spawns Smith with targeted fix prompt |
| `internal/reviewfix` | Review comment fix worker — addresses PR review feedback |
| `internal/rebase` | Conflict rebase handling for merge conflicts |
| `internal/poller` | Calls `bd ready` to get available beads from an anvil; detects Crucible candidates |
| `internal/worktree` | Creates/removes `git worktree` branches for each bead |
| `internal/state` | SQLite at `~/.forge/state.db` — workers, prs, events, retries, costs |
| `internal/cost` | Token usage and USD cost tracking per bead and per day |
| `internal/ipc` | Named pipe (Windows) / Unix socket daemon↔CLI protocol; newline-delimited JSON |
| `internal/hearth` | Bubbletea TUI: three-column layout (Queue+Crucibles(when active)+ReadyToMerge+NeedsAttention / Workers / LiveActivity+Events) |
| `internal/config` | Viper config loading — `forge.yaml` in cwd or `~/.forge/config.yaml` |
| `internal/prompt` | Builds the Smith prompt from bead metadata + AGENTS.md/CLAUDE.md/README.md |
| `internal/provider` | AI provider fallback chain (Claude, Gemini, Copilot) with rate limit handling |
| `internal/ghpr` | GitHub PR creation and management via `gh` CLI |
| `internal/changelog` | Changelog fragment parsing and assembly |
| `internal/lifecycle` | Worker lifecycle management |
| `internal/retry` | Exponential backoff and retry logic |
| `internal/watchdog` | Stale worker detection |
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

Crucible path (parent beads with children):
  bd ready (poller) → detect bead.Blocks (children)
    → crucible.Run()
      → worktree.CreateEpicBranch (feature/<parent-id>)
      → fetch children via bd show, topological sort
      → for each child: pipeline.Run() → ghpr.Create(base=feature branch) → gh pr merge
      → ghpr.Create(feature branch → main) — final PR
      → bellows monitors final PR (CI fix, review, merge → close parent)

depcheck.Monitor (background, weekly by default)
  → scans each anvil for outdated dependencies (Go, .NET, Node)
  → creates beads for outdated dependencies (patch/minor auto-dispatch, major needs attention)

vulncheck.Monitor (background, daily by default)
  → runs govulncheck on Go anvils
  → creates prioritized beads for discovered vulnerabilities
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

Config resolution order: `--config` flag → `./forge.yaml` → `~/.forge/config.yaml`. Environment variables override with `FORGE_` prefix (e.g. `FORGE_SETTINGS_MAX_TOTAL_SMITHS=4`). The daemon hot-reloads the config file on change via fsnotify. See [docs/configuration.md](docs/configuration.md) for the full settings reference including `daily_cost_limit`, `max_ci_fix_attempts`, `max_review_fix_attempts`, `max_rebase_attempts`, `smith_providers`, `merge_strategy`, `schematic_enabled`, `depcheck_interval`, and more.

### Per-Anvil Smith Prompt Customization

Place a template file at `<anvil-path>/.forge/prompt.tmpl` or `.forge/smith-prompt.tmpl` to override the default Smith prompt for that repo. The template receives `{{.Bead}}`, `{{.AgentsMD}}`, `{{.ClaudeMD}}`, `{{.ReadmeMD}}`.

## Beads Database

Forge uses `bd` (beads) backed by a Dolt database for issue tracking. The database connection is configured in `.beads/config.yaml`. If `bd` returns connection errors, check your Dolt server or port-forward configuration.

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
