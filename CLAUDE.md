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
forge preview list                    # List running Kiln preview environments
forge preview list --json             # Raw preview_list payload
forge preview stop <bead-id>          # Tear down a bead's preview environment
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
| `internal/assay` | AI pull-request review engine ("Assay") — multi-pass PR diff review (Triage + Logic/Security/Conventions/Tests/Repo passes) with deduped, idempotent findings; triggered by Bellows on open PRs |
| `internal/diff` | Shared unified-diff parsing/shaping primitives (size cap, generated-file filtering, changed-file extraction) used by both Warden and Assay |
| `internal/hooks` | Pipeline hook execution — shell commands before/after each stage |
| `internal/bellows` | Monitors open PRs for CI failures, review comments, and merge conflicts; gates Assay review runs. The CI gate that feeds Quench is head-scoped (`ci.go`): check results reported against a superseded commit are discarded, a head whose runs have not finished is `pending` (never `failed`), and a check queued past 30 minutes is `stuck` — surfaced as a Needs Attention note instead of dispatching a fix worker |
| `internal/crucible` | Orchestrates parent beads with children on feature branches — auto-detects, sequences, merges |
| `internal/depcheck` | Multi-language dependency update scanner (Go, .NET, Node) — creates beads for outdated deps |
| `internal/vulncheck` | Vulnerability scanning via `govulncheck` — creates prioritized beads |
| `internal/wicket` | GitHub issue triage monitor — polls repos for new issues, AI-classifies them, and creates beads or requests clarification |
| `internal/schematic` | Pre-analysis worker — decomposes complex beads or produces implementation plans |
| `internal/quench` | CI failure fix worker — spawns Smith with targeted fix prompt |
| `internal/burnish` | Review comment fix worker — addresses PR review feedback |
| `internal/rebase` | Conflict rebase handling for merge conflicts |
| `internal/poller` | Calls `bd ready` to get available beads from an anvil; detects Crucible candidates |
| `internal/anvilhealth` | Wedged-anvil detection — one `dolt_conflicts` query per anvil to spot a beads database left mid-merge with unresolved conflicts (every `bd` write against it fails). Detection only; resolution stays with the operator |
| `internal/worktree` | Creates/removes `git worktree` branches for each bead. Also materializes Kiln's detached preview checkouts under `<anvil>/.previews/<beadID>` (`CreateDetached`/`RemoveDetached`) — separate directory, detached HEAD, never touches the worker worktree lifecycle |
| `internal/state` | SQLite at `~/.forge/state.db` — workers, prs, events, retries, costs |
| `internal/cost` | Token usage and USD cost tracking per bead and per day |
| `internal/forge` | Core types and constants (version info) |
| `internal/ingot` | Data model and persistence for ingots (bead lifecycle snapshots) |
| `internal/ledger` | Interactive bead management TUI |
| `internal/ipc` | Named pipe (Windows) / Unix socket daemon↔CLI protocol; newline-delimited JSON |
| `internal/hearth` | **Hearth (TUI)** — Bubbletea terminal dashboard (`forge hearth`): three-column layout (Queue+Crucibles(when active)+ReadyToMerge+NeedsAttention / Workers / LiveActivity+Events) |
| `internal/web` | **Hearth 2.0 (web GUI)** — chi-based HTTP server run in-process inside the daemon (gated by `FORGE_WEB_ENABLED`). Serves the browser dashboard: bcrypt session login, JSON/SSE endpoints mirroring IPC (status, queue, workers, events, crucibles, ingots, costs, PRs), per-worker log tail/stream, per-bead log browsing, PR actions (merge/close/approve/fix), queue actions, worker steering/pause/resume, the Kiln preview routes (start/stop/list/log-tail, whose DTOs in `preview_handlers.go` are the frontend contract), and the Beads-Forge session pages |
| `internal/forgechat` | Backs the per-turn AI loop for the Hearth 2.0 "Beads-Forge" page — drafting → grilling → ready stages, one claude round-trip per turn via a pluggable `Runner` |
| `internal/queueactions` | Shared business logic behind the queue resolution verbs (clarify, unclarify, retry, clear, stop) — single source of truth for the state mutations/audit entries used by both the CLI verbs and the IPC handlers; enforces multi-forge safety |
| `internal/logsweep` | Daily retention sweep for preserved per-bead log directories under `~/.forge/logs/<beadID>/`; deletes stale dirs with no running worker |
| `internal/logrotate` | Minimal size-based `io.Writer` log rotator used as the sink for the daemon's slog handler so `~/.forge/logs/daemon.log` cannot grow unbounded |
| `internal/selfdeploy` | Rebuilds and restarts the Forge daemon from source after a merge lands on Forge's own repository (config-gated). Deploys that end anywhere other than "new binary live and restarting" — drain timeout, failed swap, failed restart, rollback — are escalated through an injected `Emitter` into Hearth's Needs Attention list, since a rollback is otherwise invisible |
| `internal/config` | Viper config loading — `forge.yaml` in cwd or `~/.forge/config.yaml` |
| `internal/prompt` | Builds the Smith prompt from bead metadata + AGENTS.md/CLAUDE.md/README.md |
| `internal/provider` | AI provider fallback chain (Claude, Gemini, Copilot) with rate limit handling |
| `internal/vcs` | VCS provider interface and GitHub implementation (`vcs/github`) |
| `internal/changelog` | Changelog fragment parsing and assembly |
| `internal/lifecycle` | Worker lifecycle management |
| `internal/retry` | Exponential backoff and retry logic |
| `internal/kiln` | **Kiln** — on-demand preview environments for worker branches. The declarative half: the `.forge/preview.yaml` manifest schema, loader (read from the anvil's MAIN checkout only) and template expansion (`{{.Port}}`, `{{.ServicePort "name"}}`, `{{.PreviewID}}`, `{{.Host}}`). The runtime half: collision-safe port allocation over `preview_port_range`, service supervision via `internal/executil` process groups (logs to `~/.forge/logs/<beadID>/preview-<service>.log`), per-service health checks (HTTP path or port-open) driving `starting → healthy \| failed`, and persistence of the whole thing in the `previews` table. The manager half (`kiln.Manager`): the bead→preview registry, the `preview_max_concurrent` cap (rejects rather than queues), the worktree + `setup`/`teardown` lifecycle around the runtime, `Touch` for the idle clock, and all-or-nothing unwinding of a failed start. The housekeeping half: `RunReaper` stops previews idle for longer than `preview_idle_timeout` (ticker derived from the timeout, injectable clock), and `Reconcile` clears previews left by a crashed daemon at startup — verified process-group kills (never a recycled PID), checkout removal, row deletion, and pruning of `<anvil>/.previews/` directories with no live preview, plus `StopAll` for shutdown. The daemon owns the manager (`internal/daemon/preview.go`): it is constructed only when `preview_enabled` is on globally AND at least one anvil has not opted out via its own tri-state, reconciles before IPC/web accept traffic, runs the reaper under the daemon waitgroup, stops every preview on shutdown, and tears a bead's preview down when its PR merges or closes. Previews are on-demand unless an anvil sets `preview_auto: ready_to_merge`, which starts one off Bellows' rising-edge `pr_ready_to_merge` event (`handlePreviewAutoStart`) — same cap, same idle reaper, silently skipped and logged when `preview_max_concurrent` is full, never for `ext-*` PRs. Previews disabled → the manager stays nil and every consumer goes through the nil-safe `Daemon.previews()`. See [docs/preview-manifest.md](docs/preview-manifest.md) for the schema and [docs/preview-manifests.md](docs/preview-manifests.md) for the worked example manifests (kept honest by the `testdata/manifests` fixtures) |
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
  → if approved: empty-branch guard — git rev-list --count <base>..<branch>
    → 0 commits (the work already landed on the base branch) → skip CreatePR,
      emit smith_empty_result, and resolve per settings.empty_diff_action
      (attention = Needs Attention entry, close = bd close with a note). Never
      retried and never counted against the dispatch circuit breaker: a
      re-dispatch would rebuild the identical empty branch. An unresolvable
      base ref or a failed rev-list falls through to the normal path.
  → if approved (branch has commits): vcs.Provider.CreatePR (gh pr create)
  → bellows monitors open PRs (CI fix, review fix, rebase)
    → Assay trigger gate → assay.Review (multi-pass AI PR review)
      → diff.* shapes the PR diff → Triage + parallel deep passes
      → dedupe/cap findings → post as PR review comments (idempotent per head SHA)
    → on merge: bd close with bounded retry (transient dolt errors — Error 1213
      serialization failures, i/o timeouts, lock timeouts — are retried)
      → still failing → persisted in `pending_bead_closes` + needs-attention
        ("merged but unclosed bead <ID> (PR #N) blocking M dependents")
      → every later bellows cycle re-derives the bead's status and re-attempts,
        clearing both once the close lands
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

logsweep.Monitor (background, daily by default)
  → deletes stale per-bead log dirs under ~/.forge/logs/<beadID>/
  → skips beads with a running worker; never touches the live daemon.log

anvilhealth check (once per FULL poll, per anvil; anvil_health_check)
  → SELECT `table`, num_conflicts FROM dolt_conflicts
  → non-empty → raise needs-attention (tables, count, ahead/behind), WARN, skip dispatch
  → empty     → clear the flag automatically (no operator action)
  → query error → state unknown: leave the previous flag untouched
```

### Two Front Ends: Hearth (TUI) vs Hearth 2.0 (web GUI)

Forge exposes the same daemon state through two independent front ends:

- **Hearth (TUI)** — `internal/hearth`, launched with `forge hearth`. A Bubbletea
  terminal dashboard that talks to the daemon over the IPC socket.
- **Hearth 2.0 (web GUI)** — `internal/web`, a chi HTTP server run **in-process**
  inside the daemon (no extra socket hop) and gated by `FORGE_WEB_ENABLED`. It
  dispatches to the same daemon command handlers the IPC layer uses, plus its own
  bcrypt-validated session login and SSE streams.

```
browser → internal/web (Hearth 2.0, in-process HTTP server, gated by FORGE_WEB_ENABLED)
  → session login (bcrypt) → chi router → in-process CommandHandler (daemon.handleIPC)
  → read views: status / queue / workers / events / crucibles / ingots / costs / PRs
  → live streams (SSE, capped per session): activity, worker-log tail/stream,
      PR findings, per-turn Beads-Forge stream
  → worker panels: per-worker log tail + kill
  → bead logs: browse/read preserved logs under ~/.forge/logs/<beadID>/
  → steering: POST /bead/{id}/steer          → steer_bead (inject a message mid-turn)
  → pause/resume: POST /bead/{id}/pause|resume|resume-with-message
                                              → pause_bead / resume_bead[_with_message]
  → PR + queue actions: merge/close/approve/fix-ci/fix-comments/fix-conflicts,
      queue retry/dispatch/clarify/stop, bead close/label/note/comment
  → Kiln previews: POST /bead/{id}/preview/start|stop → preview_start/preview_stop
      (queued, so both answer 202); GET /previews → previews, mapped to the
      PreviewSummary/PreviewServiceStatus DTOs in internal/web/preview_handlers.go
      (the frontend contract) with an entry URL built from preview_public_host
      falling back to the request's Host, plus `anvils` (the previewable
      anvils) so the SPA can gate its Preview button; GET
      /preview/{id}/log/{service} tails
      ~/.forge/logs/<beadID>/preview-<service>.log
      SPA side: src/api/previews.ts is the typed client, src/hooks/usePreview.ts
      the shared per-bead state machine plus usePreviewsList for the whole fleet
      (one polled previews snapshot for every consumer), <PreviewButton> the
      compact trigger + status chip mounted on worker cards and PR rows,
      <PreviewPanel> the full per-bead surface on the bead detail page
      (per-service port/health/uptime/log rows, Open preview, idle countdown,
      start/stop), and /previews (PreviewsPage, nav entry gated on Kiln being
      enabled) the fleet view of every running preview. <PreviewLogModal> tails
      one service log — plain monospace, not LogViewer, since preview output is
      raw process stdout rather than a claude stream-json transcript
  → async outcomes: an action the daemon runs in the background answers 202 with
      {request_id, poll_url}; the SPA polls GET /api/requests/{request_id}
      → request_status (pending → ok/error, unknown once evicted) so a failed
      write surfaces as an error toast instead of a phantom success

Beads-Forge sessions (session capture, forgechat):
  browser → POST /forge/sessions → state persists a Beads-Forge session
    → POST /forge/sessions/{id}/turn → forgechat.Runner (one claude round-trip)
      → drafting (markdown/plan) → grilling (structured questions) → ready
    → turn output streamed to the browser via SSE; turn snapshots persisted so a
      reconnecting client replays captured partial output before catching up live
```

### State Database

`~/.forge/state.db` (SQLite with WAL mode) tracks:
- **workers** — Smith process lifecycle with PID, status, log path
- **prs** — Pull requests created across anvils
- **events** — Timestamped event log (bead_claimed, smith_done, warden_pass, etc.)
- **retries** — Exponential backoff tracking; `needs_human=1` after exhausting retries
- **bead_costs / daily_costs** — Token usage and USD estimates per bead and per day
- **previews** — Kiln preview environments per bead: anvil, branch, status, worktree path, per-service JSON (name/port/health/pid/log), created/last-active timestamps
- **pending_bead_closes** — beads whose PR merged but whose `bd close` has not yet succeeded: PR number, close reason, cumulative attempts, last error, merge time. Re-attempted every Bellows cycle until the bead closes
- **deploy_failures** — self-deploys that did not go live, keyed by anvil + reason (`drain_timeout`, `swap_failed`, `restart_failed`, `rollback_failed`): attempted/restored build SHAs, rollback flag, detail, time. Surfaced as anvil-level Needs Attention entries and cleared by the deployer once a later deploy supersedes them

### IPC Protocol

The daemon exposes a named pipe (Windows: `\\.\pipe\forge`) or Unix socket. Messages are newline-delimited JSON `Command`/`Response` structs.

**Supported commands** (generated from the `handleIPC` switch in `internal/daemon/daemon.go` — enumerate every top-level `case` there by hand, or with `grep -nP '^\tcase ' internal/daemon/daemon.go`, when this list drifts). Grouped by functional area:

**Liveness & status (read-only queries)**
| Command | Description |
|---------|-------------|
| `ping` | Lightweight liveness probe; answers `pong` without touching the DB. |
| `status` | Daemon status: workers, queue size, open PRs, quotas, daily cost, pause state. |
| `subscribe` | Subscribe to the event stream. |
| `refresh` | Trigger an immediate poll + Bellows refresh. |
| `queue` | Return the cached queue of ready beads. |
| `workers` | List active workers with phase/kind/PR info. |
| `events` | Return recent events (default 50, max 500). |
| `crucibles` | List active crucible (epic) statuses. |
| `get_ingots` | List ingot records (bead lifecycle snapshots). |
| `get_ingot` | Fetch a single ingot with test results. |
| `wicket_status` | Wicket issue-triage monitor status and effective interval. |
| `request_status` | Resolve the `request_id` from a `queued` response to its outcome (`pending`/`ok`/`error`, or `unknown` when evicted). |

**Daemon control**
| Command | Description |
|---------|-------------|
| `shutdown` | Cancel the run context and shut the daemon down. |
| `pause_dispatch` | Pause auto-dispatch (idempotent); persists the pause + timestamp. |
| `resume_dispatch` | Resume auto-dispatch (idempotent) and kick a poll. |
| `reconcile_prs` | Reconcile open PR state from the VCS provider. |
| `wicket_scan` | Trigger an immediate Wicket issue scan. |

**Bead dispatch & lifecycle**
| Command | Description |
|---------|-------------|
| `run_bead` | Manually dispatch a bead. |
| `close_bead` | Close a bead via `bd close`. |
| `stop_bead` | Kill the worker and release the bd claim (shares impl with `queue_stop`). |
| `retry_bead` | Reset the circuit breaker and re-dispatch on next poll. |
| `clear_bead` | Clear needs-attention flags without re-dispatching. |
| `dismiss_bead` | Dismiss a bead/exhausted PR from the needs-attention list. |
| `force_smith` | Force a Smith run on a bead. |
| `approve_as_is` | Approve the current diff and proceed to PR without further review. |

**Bead metadata**
| Command | Description |
|---------|-------------|
| `set_clarification` | Mark a bead as needing clarification. |
| `clear_clarification` | Clear the clarification flag. |
| `append_notes` | Append notes to a bead. |
| `tag_bead` | Add a tag to a bead. |
| `update_label` | Add/update a label on a bead. |

**Queue actions** (delegated to `handleQueue*`)
| Command | Description |
|---------|-------------|
| `queue_clarify` | Mark a queued bead as needing clarification. |
| `queue_unclarify` | Clear the clarification flag on a queued bead. |
| `queue_retry` | Reset circuit breaker and re-dispatch a queued bead. |
| `queue_clear` | Clear needs-attention flags on a queued bead. |
| `queue_stop` | Kill the worker and prevent re-dispatch. |

**Steering** (delegated to `handle*Bead`)
| Command | Description |
|---------|-------------|
| `steer_bead` | Inject a steering message into a running worker. |
| `pause_bead` | Pause a running worker mid-turn. |
| `resume_bead` | Resume a paused worker. |
| `resume_bead_with_message` | Resume a paused worker with an accompanying message. |

**PR & review**
| Command | Description |
|---------|-------------|
| `create_pr` | Create a PR for a bead's branch. |
| `merge_pr` | Merge a PR (by PR id or number). |
| `pr_action` | Multiplexed PR action; `pa.Action` ∈ `close`, `discard`, `recover`, `open_browser`, `merge`, `quench`/`cifix`, `burnish`/`reviewfix`, `rebase`, `assign_bellows`, `unassign_bellows`, `approve`. |
| `warden_rerun` | Re-run Warden review on a bead. |
| `assay_rerun` | Re-run the assay (E2E) checks on a PR. |
| `resolve_orphan` | Resolve an orphaned bead/worktree via the given action. |

**Crucible**
| Command | Description |
|---------|-------------|
| `crucible_action` | Multiplexed crucible action; `ca.Action` ∈ `resume`, `stop`. |

**Worker & logs**
| Command | Description |
|---------|-------------|
| `kill_worker` | Kill a worker process (PID looked up from state.db). |
| `view_logs` | Return the log path/contents for a bead's worker. |

**Previews (Kiln)**
| Command | Description |
|---------|-------------|
| `preview_start` | Start a bead's preview environment (queued; anvil required, branch defaults to `forge/<beadID>`). |
| `preview_stop` | Tear a bead's preview down. Payload: `ipc.PreviewActionPayload` — `bead_id` required, anvil ignored. Answers `queued`; the tracked request completes with `ipc.PreviewStopResponse{stopped, bead_id, message}`. A bead with no live preview is rejected synchronously with `no preview running for bead <id>` (the automatic teardown paths call the manager directly, where stopping something already gone stays a no-op). |
| `previews` / `preview_list` | One command under two names — the dashboard's and the CLI's. No payload; answers `ipc.PreviewListResponse` (an alias of `ipc.PreviewsResponse`): live previews with per-service ports/health, each carrying `entry_url`, the entry `port`, `idle_remaining_seconds` (null when the reaper is disabled) and a `resource_note` summarising the services/ports it holds; plus `preview_public_host` and `preview_idle_timeout` so clients build links and deadlines themselves, and `anvils` — the anvils a preview can be started for (previews enabled AND a `.forge/preview.yaml` in their main checkout), which is how a client gates a per-bead Preview affordance without a probe per row. |

**Security**
| Command | Description |
|---------|-------------|
| `revoke_web_sessions` | Drop every web session so all users must re-authenticate. |

Unrecognized command types return an error (`unknown command: <type>`).

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
