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
forge pause                           # Pause auto-dispatch (running workers finish; no new dispatch)
forge resume                          # Resume auto-dispatch
forge status                          # Show daemon status (via IPC)
forge hearth                          # Open TUI dashboard
forge anvil add <name> <path>         # Register a repository
forge anvil list                      # List registered anvils
forge anvil remove <name>             # Deregister an anvil
forge queue list                      # Show queued beads
forge queue run <id>                  # Manually dispatch a bead
forge queue stop <id> --anvil <name>  # Kill worker, prevent re-dispatch
forge queue clarify <id>              # Mark bead as needing clarification
forge queue unclarify <id>            # Clear clarification flag
forge queue retry <id>                # Reset circuit breaker AND re-dispatch on next poll
forge queue clear <id>                # Clear needs-attention flags WITHOUT re-dispatching
forge history                         # Show recent worker history
forge history events                  # Show event log
forge ingots list                     # List ingot records (bead lifecycle)
forge ingots show <id>                # Show ingot details with test results
forge ledger                          # Open interactive bead management TUI
forge quest list                      # List discovered E2E quests
forge quest run <quest> --anvil <name>  # Execute a quest and report results
forge wicket status                   # Show Wicket issue triage status
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
forge update                          # Download and install the latest Forge release
forge update --check                  # Check for updates without installing
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
| `internal/hooks` | Pipeline hook execution — shell commands before/after each stage |
| `internal/bellows` | Monitors open PRs for CI failures, review comments, and merge conflicts |
| `internal/crucible` | Orchestrates parent beads with children on feature branches — auto-detects, sequences, merges |
| `internal/depcheck` | Multi-language dependency update scanner (Go, .NET, Node) — creates beads for outdated deps |
| `internal/vulncheck` | Vulnerability scanning via `govulncheck` — creates prioritized beads |
| `internal/wicket` | GitHub issue triage monitor — polls repos for new issues, AI-classifies them, and creates beads or requests clarification |
| `internal/schematic` | Pre-analysis worker — decomposes complex beads or produces implementation plans |
| `internal/quench` | CI failure fix worker — spawns Smith with targeted fix prompt |
| `internal/burnish` | Review comment fix worker — addresses PR review feedback |
| `internal/rebase` | Conflict rebase handling for merge conflicts |
| `internal/poller` | Calls `bd ready` to get available beads from an anvil; detects Crucible candidates |
| `internal/worktree` | Creates/removes `git worktree` branches for each bead |
| `internal/state` | SQLite at `~/.forge/state.db` — workers, prs, events, retries, costs |
| `internal/cost` | Token usage and USD cost tracking per bead and per day |
| `internal/forge` | Core types and constants (version info) |
| `internal/ingot` | Data model and persistence for ingots (bead lifecycle snapshots) |
| `internal/ledger` | Interactive bead management TUI |
| `internal/ipc` | Named pipe (Windows) / Unix socket daemon↔CLI protocol; newline-delimited JSON |
| `internal/hearth` | Bubbletea TUI: three-column layout (Queue+Crucibles(when active)+ReadyToMerge+NeedsAttention / Workers / LiveActivity+Events) |
| `internal/config` | Viper config loading — `forge.yaml` in cwd or `~/.forge/config.yaml` |
| `internal/prompt` | Builds the Smith prompt from bead metadata + AGENTS.md/CLAUDE.md/README.md |
| `internal/provider` | AI provider fallback chain (Claude, Gemini, Copilot) with rate limit handling |
| `internal/vcs` | VCS provider interface and GitHub implementation (`vcs/github`) |
| `internal/changelog` | Changelog fragment parsing and assembly |
| `internal/lifecycle` | Worker lifecycle management |
| `internal/retry` | Exponential backoff and retry logic |
| `internal/questgiver` | E2E quest discovery and execution |
| `internal/adventurer` | Headless browser quest executor (drives quest steps via rod) |
| `internal/smelter` | Batches pending warden rules into PRs |
| `internal/watchdog` | Stale worker detection |
| `internal/hotreload` | fsnotify watcher — reloads `forge.yaml` without restart |
| `internal/notify` | MS Teams Adaptive Card webhooks |
| `internal/shutdown` | Graceful shutdown: SIGINT drain, orphan worktree cleanup |
| `internal/autostart` | Windows Task Scheduler integration |
| `internal/executil` | Platform-specific process execution |
| `internal/worker` | Worker process abstraction |
| `cmd/forge` | Cobra CLI — subcommands wired to daemon/state/ipc |

### Changelog Fragments

Every PR must include a changelog fragment in `changelog.d/`. The file name should be `<bead-id>.md` (e.g. `Forge-abc1.md`).

**Required format:**
```
category: Added
- **Short title** - Description of the change. (Forge-abc1)
```

**Rules:**
- **Line 1 MUST be `category: <Category>`** — one of: `Added`, `Changed`, `Fixed`, `Removed`, `Deprecated`, `Security`
- **Line 2+ are markdown bullet points** describing the change
- **Include the bead ID** in parentheses at the end of each bullet
- **Do NOT use commit-message style** (e.g. `fix: description`) — that format will break the changelog assembler

**Examples:**
```
category: Added
- **Ingot lifecycle tracking** - Track bead→PR→merge journey with structured test results in SQLite. (Forge-czem)
```

```
category: Fixed
- **Bellows CI status timing** - Only flag CI as failed when all checks are completed, not while still in progress. (Forge-68vu)
```

### Data Flow

```
bd ready (poller) → pipeline.Run()
  → worktree.Create (git worktree add)
  → [before_schematic hook] → schematic.Analyze (optional) → [after_schematic hook]
  → [before_smith hook] → smith.Spawn (claude CLI) → [after_smith hook]
  → deny_patterns validation (file globs + command globs, resets on violation)
  → [before_temper hook] → temper.Run (build/test/lint) → [after_temper hook]
  → [before_warden hook] → warden.Review (second claude session) → [after_warden hook]
  → if request_changes: loop back to Smith (max max_review_attempts iterations)
  → if approved: vcs.Provider.CreatePR (gh pr create)
  → bellows monitors open PRs (CI fix, review fix, rebase)
  → worktree.Remove

Crucible path (parent beads with children):
  bd ready (poller) → detect bead.Blocks (children)
    → crucible.Run()
      → worktree.CreateEpicBranch (feature/<parent-id>)
      → fetch children via bd show, topological sort
      → for each child: pipeline.Run() → vcs.CreatePR(base=feature branch) → vcs.MergePR
      → vcs.CreatePR(feature branch → main) — final PR
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

Config resolution order: `--config` flag → `./forge.yaml` → `~/.forge/config.yaml`. Environment variables override with `FORGE_` prefix (e.g. `FORGE_SETTINGS_MAX_TOTAL_SMITHS=4`). The daemon hot-reloads the config file on change via fsnotify. See [docs/configuration.md](docs/configuration.md) for the full settings reference including `daily_cost_limit`, `max_ci_fix_attempts`, `max_review_fix_attempts`, `max_rebase_attempts`, `stage_providers`, `merge_strategy`, `schematic_enabled`, `depcheck_interval`, per-anvil `hooks`, `smith.deny_patterns`, `temper` commands/steps, and more.

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


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:7510c1e2 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->

<!-- bd-doctor-divergence: ok -->
