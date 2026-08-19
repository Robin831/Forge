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
forge assay rerun <pr> --anvil <name> # Re-run Assay over a PR's head, addressed by
                                      # its GitHub PR number (scoped by the anvil)
forge assay run --pr <id> --anvil <name>  # Same, addressed by the state.db PR row id
forge bellows stop <pr> --anvil <name>   # Mute Bellows for one PR: no automatic CI/review/
                                      # rebase/Assay work, and its fix workers are killed
forge bellows resume <pr> --anvil <name> # Unmute it; problems that outlived the mute are
                                      # re-detected as fresh transitions
forge wicket status                   # Show Wicket issue triage status
forge preview list                    # List running Kiln preview environments
forge preview list --json             # Raw preview_list payload
forge preview start <bead-id> --anvil <name> [--branch <branch>]
                                      # Start a preview; --branch defaults to forge/<bead-id>.
                                      # The bead id is a registry key — it need not exist as a
                                      # bd issue, so ad-hoc smoke previews use ids like kiln-smoke-1
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
| `internal/assay` | AI pull-request review engine ("Assay") — multi-pass PR diff review (Triage + Logic/Security/Conventions/Tests/Repo passes) with deduped, idempotent findings; triggered by Bellows on open PRs. A run where some deep passes reviewed the head and others did not is **partial**, not a failure and not a success: `DeriveStatus` (`status.go`) makes that three-way call once, and the result carries the pass tally plus a `PassFailure{Name, Reason}` per pass that never ran — the reason being the provider result subtype (`error_max_turns`) where there is one, which `PassError` keeps structured instead of leaving it to be re-parsed out of a message. Everything downstream reads that one computation: `assay_runs.status/completed_passes/total_passes/failed_passes` persist it, `RenderStatusText` renders `partial: 3 of 5 passes completed (failed: logic, repo-specific — error_max_turns)` into the daemon log, the `assay_partial` event message and the PR findings panel — the worker row itself carries only the status (the TUI's `◐` glyph, the web row's `partial` chip), not the tally — and `PartialCoverageNote` puts the same passes at the top of the PR summary comment — above the severity table, since a caveat read after the findings arrives too late. A partial run always posts a summary, findings or none: silence there is what would read as a clean review. A pass that ends on `error_max_turns` is re-run **once** in a fresh session with identical inputs before it counts as failed (`maxTurnsRetries`, `runDeepPass` owns the loop and `runPassAttempt` knows nothing of it, so a retry cannot itself retry): that subtype means the model spent its budget exploring and never emitted JSON, not that it read the diff and found nothing, so only the final attempt's outcome is recorded — a pass that recovers is an ordinary success, never a residue in `PassErrors`. What the retry is tuned against is per-pass telemetry: `smith.Result.NumTurns` (the provider's `num_turns`) rides through `PassOutput`/`PassError` onto `PassReport{Turns, TerminationReason, Attempts, Retried}`, and `RenderPassTelemetry` puts `pass=logic turns=12 term=error_max_turns retry=1` in the daemon's Assay log line as its own field — additive, so the coverage status text is untouched. Cost is counted the same way at both levels, because a failure is not a refund: `PassError.CostUSD` carries what a failed session was billed, and a run that returns no result at all — triage failed, or every deep pass did — leaves its spend on a `*RunError` wrapping the cause, which `assay.RunCost` reads back so the daemon records `assay_runs.cost_usd` on the error path exactly as on the success path. Every run closes itself out in the activity feed with **exactly one** terminal event — `assay_completed`, `assay_partial` or `assay_failed` — emitted from the one place a run ends (`emitAssayTerminalEvent`, `internal/daemon/assayevent.go`), so the `pr_review_needed` that opened the review always has a matching resolution: `Assay PR #347: complete — 5/5 passes, 7 findings ($2.80, 152s)`, rendered by `assay.RunEvent.Message` from the run record rather than the review result, since a run that died before the engine returned has no result but still has a record. A shadow-mode run appends `(shadow — findings in panel only)`: it posts nothing on the PR by design, so without the clause it was invisible end to end. A failed run names its cause (trimmed to one bounded line) instead of a findings count, which previously surfaced only as the worker row's state |
| `internal/diff` | Shared unified-diff parsing/shaping primitives (size cap, generated-file filtering, changed-file extraction) used by both Warden and Assay |
| `internal/termtext` | The one stripper for text Forge did not write but renders into a terminal — `Line` removes escape sequences whole (CSI including the private-parameter bytes `<`/`=`/`>`, OSC/DCS/PM/APC/SOS, the bare two-byte forms), spaces line breaks and tabs, and drops every remaining non-printable rune. It is one package rather than one per surface because it was two: Hearth's bead-title stripper handled only CSI while Assay's feed-message stripper handled the string sequences too, so whichever surface a hostile string reached decided how much of it survived |
| `internal/textfmt` | Small shared rendering helpers with no home of their own. `Count(n, noun)` ("1 quest" / "3 quests") and `Suffix` (the bare ""/"s" for `%d worker%s` format strings) are one package rather than six copies: pluralization had been re-derived in `cmd/forge`, `questgiver`, `web` and `daemon` (three times, two of them open-coded), in two shapes and under two names — none of them disagreeing, which is why every surface that wanted it wrote its own instead of finding one of the others |
| `internal/hooks` | Pipeline hook execution — shell commands before/after each stage |
| `internal/bellows` | Monitors open PRs for CI failures, review comments, and merge conflicts; gates Assay review runs. The CI gate that feeds Quench is head-scoped (`ci.go`): check results reported against a superseded commit are discarded, a head whose runs have not finished is `pending` (never `failed`), and a check queued past 30 minutes is `stuck` — surfaced as a Needs Attention note instead of dispatching a fix worker. A PR carrying `prs.bellows_detached` is **muted**: `checkPR` returns before the first emit, which is the only placement that also closes the steady-state re-emit branches ("still failing CI", "still conflicting", "still unresolved threads") — those fire on *unchanged* state, so a per-emit check would have left a muted PR re-announcing its problem every poll. What still runs is the mergeability and terminal-state persistence, because a muted PR is unwatched, not unknown: the PR panel keeps showing the truth, and a detached PR that merges is recorded as merged rather than polled forever. Its `bellows-<anvil>-<n>` row keeps its place in the Workers panel (a row that silently vanishes reads as a bug) carrying `state.WorkerDetached` — non-terminal, since the flag is reversible, and `SetBellowsWorkerDetached` moves an existing row in either direction without resurrecting a terminal one. Resuming clears the cached snapshot (`wasDetached`, the same trick the ext-PR assignment path uses), so problems that outlived the mute are re-detected as fresh transitions instead of being swallowed as state bellows has already seen |
| `internal/crucible` | Orchestrates parent beads with children on feature branches — sequences and merges them. Candidacy is **opt-in** (`IsCrucibleCandidate` = children AND `epic.IsOrchestrated`): having children is not enough, since parent/child relations are used for plain grouping far more often than for a shared branch. The opt-out is per child: `excludeIndependent` drops every `independent`-labeled child in the **one** place children enter a run — after `fetchChildren`, before the topological sort — so the same bead cannot be dispatched by one path while being counted by another. That single point is also the completeness decision: an excluded child is not a *skipped* one (which pauses the run, because skipped work never reached the branch it was meant to reach) — its work could never have reached this branch, so it is left out of `TotalChildren` and out of the set that holds the final PR. `FetchChildren` applies the same filter at the source and does not enqueue an excluded child's subtree either |
| `internal/epic` | The one place the epic opt-in and the epic branch name are decided. `IsOrchestrated(labels)` is true only for the `crucible` label or an explicit `epic-branch:<name>` — a bead's issue type is deliberately not an input, so `issue_type: epic` alone no longer routes its children anywhere; both forms are whitespace-trimmed the same way, so a padded label cannot opt in through one and not the other. `BranchName(id, labels)` is the single source of truth the poller (which stamps children with it) and the Crucible (which creates it) both call: they used to derive `epic/<id>` and `feature/<id>` independently, so a child dispatched outside the Crucible hard-failed on "base branch not found on origin" and burned the dispatch circuit breaker. Centralising it is also the one place a label-supplied name can be screened, which `ValidBranchName` does — the value is stamped onto children as a PR base, handed to `git worktree add` and folded into a worktree path, so a name git would reject or read as a flag (`epic-branch:--force`, `../../x`) never leaves `ExplicitBranch`: the label still counts as an opt-in, the branch falls back to the derived name. `IsIndependent(labels)` is the mirror image — the `independent` label a single child carries to be left out of a parent that *did* opt in — and it wins over both opt-in forms on a bead carrying them together, since a bead read as orchestrating while its own dispatch path treats it as independent would run to main while its children were still stamped with a branch nothing then creates. The two labels do describe different relationships (`crucible` points at a bead's children, `independent` at its parent) and could in principle compose on a sub-epic, but nothing below this package is scoped per edge — a bead is orchestrated or it is not — so the pair is read as a mistake and `HasConflictingLabels` is what lets the poller WARN about it by name once per bead, rather than the opt-in going inert in silence. Adding `independent` to a parent **mid-flight** is the one unsafe use: the demotion unwinds nothing, so a parent whose Crucible already merged child PRs onto `feature/<parent-id>` starts dispatching to main and leaves that branch orphaned with no final PR |
| `internal/depcheck` | Multi-language dependency update scanner (Go, .NET, Node) — creates beads for outdated deps. The npm half syncs `node_modules` with `npm ci --ignore-scripts` before reading `npm outdated`, so it is gated on a `PreviewLivenessFunc` the daemon injects (`SetPreviewLiveness`, nil = never blocked): an anvil with a live Kiln preview linked into its `node_modules` is skipped for the whole cycle, naming the holding bead in the log, rather than having `npm ci` delete that tree through the link. Go and .NET scanning are never gated |
| `internal/vulncheck` | Vulnerability scanning via `govulncheck` — creates prioritized beads |
| `internal/wicket` | GitHub issue triage monitor — polls repos for new issues, AI-classifies them, and creates beads or requests clarification |
| `internal/schematic` | Pre-analysis worker — decomposes complex beads or produces implementation plans |
| `internal/quench` | CI failure fix worker — spawns Smith with targeted fix prompt |
| `internal/burnish` | Review comment fix worker — addresses PR review feedback. Its verification (Temper between Smith's commit and the push) is **advisory**, unlike the pipeline's: every burnish output lands on an open PR that humans, Copilot and Assay review again. So a verification that never completes is re-run (`burnish_verify_retries`, default 1) and then, still timing out, the commit is **pushed marked unverified** (`review_fix_unverified_push` + Needs Attention) rather than discarded — and burnish never loops back to Smith on a timeout, since a timeout says nothing about the diff. A fix commit that reaches neither verification nor the remote sets `FixResult.UnpushedHead`, which is what stops the worktree being torn down over it (`review_fix_work_preserved`). The old path did the opposite of all three: WARN, skip the push, delete the worktree, report success — a finished fix left as an unreferenced object while the loop re-derived it every cycle (Forge-xl50) |
| `internal/rebase` | Conflict rebase handling for merge conflicts |
| `internal/poller` | Calls `bd ready` to get available beads from an anvil; detects Crucible candidates. `ResolveEpicBranches` stamps a child with its parent's branch **only** when the parent opted in, so by default `EpicBranch` stays empty and the child flows through the normal pipeline to main. It finds the parent through the child's own `Parent` field or a `blocks`/`parent-child` dependency entry (`ParentCandidates`) — never through `Blocks`, which `pollAnvil` has already overwritten with the inverted meaning "my children" — and records **which** candidate resolved on `EpicParent`, since `ParentCandidates` is ordered by edge, not by opt-in: a `bd dep add` sequencing edge is `blocks`-typed and routinely precedes the labeled parent, so re-deriving the answer later names an unrelated bead. `OpenChildren` is the wider question `Blocks` cannot answer — `Blocks` is reconstructed from the current poll batch, so "no children are ready" and "no children exist" are the same empty set there, and only the second means an epic has nothing left to orchestrate. The per-child opt-out is read through `IsIndependentBead` (the `independent` label **or** the manual `ForceIndependent` flag — neither implies the other, and the flag is `json:"-"`, so a bead restored from the queue cache carries only the label): an opted-out child is not stamped by `ResolveEpicBranches`, not registered in its parent's reconstructed `Blocks`, and not counted by `OpenChildren`, which is what keeps a parent whose only ready child is independent from starting a Crucible with nothing to put on its branch. The two gates that read a `bd show` **dependents** array (`OpenChildren` and `lookupBlocks`) cannot read the label off it: a dependents entry is an edge summary — id, title, status, priority, issue_type, dependency_type — and labels live only on a bead's own record, so `dep.Labels` would have been permanently nil and the opt-out inert on exactly the two paths that decide whether an epic still has work. Both resolve it through `dropIndependent` instead: one `bd show <ids...> --json` for the whole child list, and a child whose labels cannot be read is **kept**, which is the conservative direction at both sites (it holds the parent for an operator rather than closing an epic that still has children, and leaves a parent a Crucible candidate rather than dropping a child that never opted out). `isClosedStatus` and `isChildDependency` are shared by the same two readers, so neither bd's terminal status nor the edge types that make a dependent a child can be read one way by one gate and another by the other: `lookupBlocks` used to count only `blocks` edges while `OpenChildren` counted `blocks` and `parent-child`, so a family linked purely by `parent-child` edges was held open by one while the other saw no children at all — the parent escalated and `IsCrucibleCandidate` never fired, leaving no way out of the hold |
| `internal/anvilhealth` | Wedged-anvil detection — one `dolt_conflicts` query per anvil to spot a beads database left mid-merge with unresolved conflicts (every `bd` write against it fails). Detection only; resolution stays with the operator |
| `internal/worktree` | Creates/removes `git worktree` branches for each bead. `RemoveIfPushed` (`unpushed.go`) is `Remove` plus one invariant — a worktree whose HEAD is not *provably* on the remote (an ancestor of `origin/<branch>`, or contained in some other remote-tracking branch) is never deleted; it returns `*UnpushedHeadError` naming the SHA and the checkout so recovery is a cherry-pick rather than an excavation of `git fsck --lost-found`. Anything the check cannot prove counts as unpushed: a preserved worktree costs one directory, a wrong removal costs the work. The daemon routes the **review-fix** teardown through it (`removeLifecycleWorktree`); quench and rebase still remove unconditionally, since their push semantics are their own decision. Also materializes Kiln's detached preview checkouts under `<anvil>/.previews/<beadID>` (`CreateDetached`/`RemoveDetached`) — separate directory, detached HEAD, never touches the worker worktree lifecycle |
| `internal/state` | SQLite at `~/.forge/state.db` — workers, prs, events, retries, costs |
| `internal/cost` | Token usage and USD cost tracking per bead and per day |
| `internal/forge` | Core types and constants (version info) |
| `internal/ingot` | Data model and persistence for ingots (bead lifecycle snapshots) |
| `internal/ledger` | Interactive bead management TUI |
| `internal/ipc` | Named pipe (Windows) / Unix socket daemon↔CLI protocol; newline-delimited JSON |
| `internal/hearth` | **Hearth (TUI)** — Bubbletea terminal dashboard (`forge hearth`): three-column layout (Queue+Crucibles(when active)+ReadyToMerge+NeedsAttention / Workers / LiveActivity+Events) |
| `internal/web` | **Hearth 2.0 (web GUI)** — chi-based HTTP server run in-process inside the daemon (gated by `FORGE_WEB_ENABLED`). Serves the browser dashboard: bcrypt session login, JSON/SSE endpoints mirroring IPC (status, queue, workers, events, crucibles, ingots, costs, PRs), per-worker log tail/stream, per-bead log browsing, PR actions (merge/close/approve/fix), queue actions, worker steering/pause/resume, the Kiln preview routes (start/stop/list/log-tail, whose DTOs in `preview_handlers.go` are the frontend contract), and the Beads-Forge session pages. It also fronts previews by hostname when `settings.preview_proxy_base` is set (`preview_proxy.go`): `PreviewProxyMiddleware` sits ahead of routing, and a request whose Host `kiln.ParsePreviewHost` accepts is resolved through the daemon's `preview_resolve` and forwarded to the preview's loopback port by one process-wide `httputil.ReverseProxy` — `FlushInterval = -1` so SSE/HMR stream, a Director that rewrites only scheme and host (path, query and the Host header arrive byte-for-byte), compression and HTTP/2 off so websocket upgrades pass through, and generous transport timeouts because a preview's traffic holds connections open for minutes. Everything else — every host the parse rejects, the apex included — falls through untouched, which is what keeps the dashboard's own traffic out of the proxy path. Proxied previews are gated by `preview_auth.go` unless `settings.preview_proxy_auth` is `none`: `sharedCookieDomain` widens the Hearth session cookie to the parent the dashboard host and the proxy base share (refusing IPs, `localhost` and anything that would cross a public suffix), and where no such parent exists the entry URL carries a short-lived `?_forge_token=` HMAC that the middleware exchanges for an HttpOnly `forge_preview_<label>` cookie and redirects off the URL. A request proving neither is 401 (302 to the apex login for a navigation) *before* `preview_resolve` runs, and Hearth's own cookies are stripped from everything forwarded upstream. That login redirect carries the preview URL as `next`, and `loginnext.go` is the only thing that consumes it: both `/login` handlers send the browser on to it once a session exists, but only after re-validating it against the live `preview_proxy_base` (a preview hostname, no embedded credentials, http/https, not the apex) and only where the session cookie can reach preview hosts at all — anything else silently falls back to the dashboard, and the SPA follows the URL the server returns rather than the one in its own address bar |
| `internal/forgechat` | Backs the per-turn AI loop for the Hearth 2.0 "Beads-Forge" page — drafting → grilling → ready stages, one claude round-trip per turn via a pluggable `Runner` |
| `internal/queueactions` | Shared business logic behind the queue resolution verbs (clarify, unclarify, retry, clear, stop) — single source of truth for the state mutations/audit entries used by both the CLI verbs and the IPC handlers; enforces multi-forge safety |
| `internal/logsweep` | Daily retention sweep for preserved per-bead log directories under `~/.forge/logs/<beadID>/`; deletes stale dirs with no running worker |
| `internal/logrotate` | Minimal size-based `io.Writer` log rotator used as the sink for the daemon's slog handler so `~/.forge/logs/daemon.log` cannot grow unbounded |
| `internal/selfdeploy` | Rebuilds and restarts the Forge daemon from source after a merge lands on Forge's own repository (config-gated). Deploys that end anywhere other than "new binary live and restarting" — drain timeout, failed swap, failed restart, rollback — are escalated through an injected `Emitter` into Hearth's Needs Attention list, since a rollback is otherwise invisible. The set the drain waits on is the daemon's `activeWorkerIDs`, which is work that owns a live process or pipeline goroutine (Smith, the quench/burnish/rebase/Assay fix workers) plus operator-paused workers — never a bellows per-PR monitor row: those are non-terminal, so `ActiveWorkers` returns them, but they hold no PID and outlive any restart, and counting them meant one PR parked in the fix loop deferred every deploy for the full `max_drain_wait`. The test is a monitor-only status (`state.WorkerStatus.IsMonitorOnly`, covering both `monitoring` and `detached`) **and** a bead with no `activeBeads` reservation, because the status alone does not mean idle: a pipeline flips its own row to `monitoring` at warden approval, before the push and before `finalizePipeline` creates the PR and drops the worktree, and holds the reservation across that whole window — a bellows row never takes one. `beadInFlight` is the one reader of that map, shared with bellows' in-flight checker so "busy" means one thing. `forge status` reads the same function for its "waiting on N workers" detail, so the two never disagree |
| `internal/config` | Viper config loading — `forge.yaml` in cwd or `~/.forge/config.yaml` |
| `internal/prompt` | Builds the Smith prompt from bead metadata + AGENTS.md/CLAUDE.md/README.md |
| `internal/provider` | AI provider fallback chain (Claude, Gemini, Copilot) with rate limit handling |
| `internal/vcs` | VCS provider interface and GitHub implementation (`vcs/github`) |
| `internal/changelog` | Changelog fragment parsing and assembly |
| `internal/lifecycle` | Worker lifecycle management |
| `internal/retry` | Exponential backoff and retry logic |
| `internal/kiln` | **Kiln** — on-demand preview environments for worker branches. The declarative half: the `.forge/preview.yaml` manifest schema, loader (read from the anvil's MAIN checkout only) and template expansion (`{{.Port}}`, `{{.ServicePort "name"}}`, `{{.PreviewID}}`, `{{.Host}}` — the public name for URLs — and `{{.BindHost}}` — `preview_bind_host`, the address a service must be told to listen on, so a manifest never hardcodes one that disagrees with the setting). The runtime half: collision-safe port allocation over `preview_port_range` (whose default, `24000-24999`, sits below every common ephemeral port floor — a range inside the kernel's ephemeral range can have an allocated port taken by an outbound connection in the minutes between the allocator's bind-test and the service actually binding it, which `ephemeral.go` detects from `/proc/sys/net/ipv4/ip_local_port_range` and the daemon reports as a WARN at start, never a rejection), service supervision via `internal/executil` process groups (logs to `~/.forge/logs/<beadID>/preview-<service>.log`), per-service health checks (HTTP path or port-open) driving `starting → healthy \| failed`, and persistence of the whole thing in the `previews` table. Health is not one-shot readiness: the supervisor has always observed a service's death (`reap`), and `watchServiceExits`/`handleServiceExit` are what read it back, so `healthy → exited` carries the exit code and freezes the service's uptime at `ExitedAt` (`state.PreviewService.Lifetime` is the one place the interval becomes a duration) rather than leaving every surface reporting a growing uptime over a dead process. The watchers start only after `waitHealthy` returns, so they cannot race a readiness check for the same service; a death during teardown or shutdown (`p.stopped`, or the process context already cancelled) is recorded but never demoted, since that death *is* the stop working. `deriveStatus` is re-applied after each exit and counts `exited` as not-serving, so the fold that decided the status at startup is the one that decides it at minute seven. `RuntimeConfig.OnServiceExit` carries the death out to the daemon, which is what emits `preview_service_exited`: Kiln owns the transition, the daemon owns telling anybody. `FormatServiceExit` (`exit.go`) is the one renderer of `exited (exit 1, lived 7m31s)` that the log, the event, the CLI and the withheld-link note all share. Recovering from that death is opt-in per service (`restart: on-failure` + `max_restarts`, default 3, capped at 10; `restart.go`), because the default has to stay "a service that dies for a real reason is not hidden behind a restart loop": `claimRestartLocked` decides inside the same hold of `p.mu` that demoted the service — so a clean exit, a spent budget and a teardown are all refusals, and the demotion is recorded either way, since the window before a working relaunch is a window in which nothing is serving. `restartService` then re-spawns the stored `ServiceSpec` verbatim on the **same** allocated port (the allocator holds it for the preview's life), re-runs the readiness check and re-derives the status through the same fold, so a recovered service returns to `running` by the path a fresh start uses; a relaunch that cannot spawn or never passes readiness is terminal. Its visibility is the condition on the whole feature — `ServiceExit` carries `Restarting`/`Restarts`/`MaxRestarts` so the exit event reads `restarting (attempt 1 of 3)` or `restart attempts exhausted`, `OnServiceRestart` emits `preview_service_restarted` per attempt, and `state.PreviewService.Restarts` rides to every surface so `healthy` after three restarts never reads like `healthy` all along. The manager half (`kiln.Manager`): the bead→preview registry, the `preview_max_concurrent` cap — rejects rather than queues, and the refusal (`*TooManyPreviewsError`, wrapping `ErrTooManyPreviews`) names the limit *and* the beads holding the slots so the answer says what to stop; `preview_evict_lru` (default off) turns that refusal into stopping the least recently used preview instead, never one whose start is still in flight and never on behalf of an automatic start (`StartOptions.NoEvict`) — the worktree + `setup`/`teardown` lifecycle around the runtime, `Touch` for the idle clock, and all-or-nothing unwinding of a failed start. The housekeeping half: `RunReaper` stops previews idle for longer than `preview_idle_timeout` (ticker derived from the timeout, injectable clock), and `Reconcile` clears previews left by a crashed daemon at startup — verified process-group kills (never a recycled PID), checkout removal, row deletion, and pruning of `<anvil>/.previews/` directories with no live preview, plus `StopAll` for shutdown. The daemon owns the manager (`internal/daemon/preview.go`): it is constructed only when `preview_enabled` is on globally AND at least one anvil has not opted out via its own tri-state, reconciles before IPC/web accept traffic, runs the reaper under the daemon waitgroup, stops every preview on shutdown, and tears a bead's preview down when its PR merges or closes. Previews are on-demand unless an anvil sets `preview_auto: ready_to_merge`, which starts one off Bellows' rising-edge `pr_ready_to_merge` event (`handlePreviewAutoStart`) — same cap, same idle reaper, silently skipped and logged when `preview_max_concurrent` is full (never evicting, even with `preview_evict_lru` on), never for `ext-*` PRs. The addressing half (`label.go`): `PreviewLabel` folds a bead id to a DNS label (`SanitizePreviewID` with `_`→`-`, since `_` is the one character that alphabet carries which a hostname may not) and `ParsePreviewHost` inverts it — `<label>.<base>` and `<label>--<service>.<base>`, case- and port-insensitive, rejecting the apex itself and any host with extra labels. The fold is not injective, so `CheckPreviewLabelCollisions` (same shape as the manifest's `FORGE_PREVIEW_PORT_<NAME>` check) refuses the *second* colliding preview at start rather than letting two share one address — gated on `ManagerConfig.ProxyBase`, i.e. only when `settings.preview_proxy_base` is set and something actually routes by hostname. `ServiceLabel` is the same fold for a manifest service name (`.`/`_`→`-`), which is what the `--<service>` half of both the parse and the build uses. `EntryURL` (`url.go`) is the one place either form of a preview address is assembled — the host-based `<scheme>://<label>[--<service>].<base>[:port]/[?<token>]` when a proxy base is given (it wins outright: where routing is by hostname the loopback port is often unreachable, so no label means no link rather than a port link), else the direct `http://<host>:<port>/`. Everything that shows a preview link goes through it: `kiln.Preview.EntryURL` (the direct address, which is what the quest browser and the daemon logs want), the daemon's `previewEntryURL` (the operator-facing link the previews payload carries and `forge preview list` prints — settings only, no request, no token), and `internal/web`'s `previewEntryURL` (the same builder plus the browser's own scheme/port and a minted access token). All three withhold the link when the entry service is not serving (failed or exited) and never fall back to a healthy sibling's port — a link to a dead process answers with a browser error that reads as a broken tunnel, and a link to the wrong service is worse than none; the daemon's `previewEntryNote` is the sentence that replaces it, and `preview_resolve` answers `not_serving` rather than forwarding. Previews disabled → the manager stays nil and every consumer goes through the nil-safe `Daemon.previews()`. See [docs/preview-manifest.md](docs/preview-manifest.md) for the schema and [docs/preview-manifests.md](docs/preview-manifests.md) for the worked example manifests (kept honest by the `testdata/manifests` fixtures) |
| `internal/questgiver` | E2E quest discovery and execution. Quest steps expand `{{.BaseURL}}` (`Expand`) against the quest file's own `url` on the scheduled scan, or against a Kiln preview's entry URL via `Monitor.RunQuestsForPreview(ctx, anvil, headSHA, baseURL)` — gated on the anvil's `preview_quests` opt-in and on the preview being healthy (`state.PreviewRunning`), and returning a `QuestRunResult` tagged with the preview ID and head SHA so downstream reporting can dedupe. Preview runs create no beads (the caller reports them) and never run the anvil's questgiver setup/teardown commands: the preview *is* the environment. The daemon supplies the opt-in anvil set and the `PreviewLookup` (`internal/daemon/preview.go`), which matches a live preview by anvil plus the commit its detached checkout has at HEAD. `RunStore` (`runs.go`) holds the on-demand runs of one daemon lifetime — `running \| passed \| failed \| skipped \| error`, per-quest outcomes and screenshot paths — in memory and nowhere else: nothing in the pipeline, Bellows or the merge path reads a preview quest result, and keeping it out of `state.db` is what keeps that true. `Reporter` (`report.go`) publishes a finished run onto the bead's open PR as a single comment, upserted against a hidden `<!-- forge-preview-quest: <headSHA> -->` marker (the same idempotency trick as Assay's `assay-hash` marker) so re-running a commit edits its comment while a new head gets a fresh one — a pass/fail table plus screenshots, embedded when a `ScreenshotUploader` is wired and named by path when it is not or an upload fails. It creates **no** check run and **no** commit status, and the daemon (`reportPreviewQuestRun` in `internal/daemon/preview_quests.go`) only logs its error: a preview quest result must never gate a merge. Skipped and errored runs are not reported at all |
| `internal/adventurer` | Headless browser quest executor (drives quest steps via rod) |
| `internal/smelter` | Batches pending warden rules into PRs |
| `internal/watchdog` | Stale worker detection |
| `internal/hotreload` | fsnotify watcher — reloads `forge.yaml` without restart. `applyChanges` is both the "did anything reloadable move" gate and the log lines; anything it does not detect is never swapped in. The per-anvil Kiln tri-states (`preview_enabled`, `preview_auto`, `preview_quests`) are on it, since they resolve per request. What it cannot apply it reports rather than drops: `restartOnlyKeys` names the settings captured once at startup (the Kiln globals — manager, allocator and reaper are all built from them), and `reportIgnored` falls back to a generic WARN when the config differs but nothing reloadable moved, so an ignored edit never passes in silence |
| `internal/notify` | MS Teams Adaptive Card webhooks |
| `internal/shutdown` | Graceful shutdown: SIGINT drain, orphan worktree cleanup |
| `internal/autostart` | Windows Task Scheduler integration |
| `internal/executil` | Platform-specific process execution. Owns the single `bd` entry point — `BdCommand`/`BdCommandTimeout` bound every bd subprocess by `settings.bd_timeout` (`SetBdTimeout`, default 5m) and report a deadline kill as a `*BdTimeoutError` naming the command, the elapsed time and the limit that fired, instead of the bare `signal: killed` exec.CommandContext produces; a caller with a tighter deadline of its own still wins and is what the message reports. Also owns `CleanGitEnv`/`StripGitEnv`/`IsGitRepoEnvVar` — the single strip set for git's repo-location env vars (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_NAMESPACE`, `GIT_GRAFT_FILE`, `GIT_SHALLOW_FILE`, `GIT_CEILING_DIRECTORIES`, `GIT_DISCOVERY_ACROSS_FILESYSTEM`), so every `git -C <path>` Forge runs is answered by that path and not by the repository an ambient GIT_DIR points at. `BdShowDependents` (`bdshow.go`) is the single builder of `bd show <ids...> --json --include-dependents`, because the `dependents` array is opt-in on bd (verified against 1.1.2) and its absence is *silent* — unflagged, bd reports a bare `dependent_count` and omits the array, so a call site that forgets the flag decodes an empty slice and reads it as "this bead has no children": the answer that keeps a Crucible from ever finding work, holds an epic open forever, and auto-closes a decomposed parent its children are still blocked on. There is no unflagged fallback, since retrying without the flag would hand the caller back exactly the empty array the flag exists to fix: a bd that rejects it is classified by `ClassifyBdShowError` into `ErrIncludeDependentsUnsupported`, which names the upgrade, and `forge doctor`'s `bd dependents support` check probes `bd show --help` up front rather than comparing version strings. The flag is documented as slow on hub beads (it streams each dependent's record), and `poller.ResolveBlocks` is its widest fan-out — one goroutine per ready bead with no `Blocks` — which is accepted rather than optimised: reading `dependent_count` first would double the round trips for exactly the parents that matter |
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
      → close landed → maybeCloseGroupingParent: resolve the child's parent
        (poller.ParentCandidates) and close it when it is a plain grouping
        parent whose every child is now closed. Independent mode's counterpart
        to the Crucible closing an orchestrated parent after its final PR — so
        it is a no-op for a parent epic.IsOrchestrated says the Crucible owns,
        for an already-closed parent, and for one bd reports no children for at
        all. At most one parent per child close (no cascade to grandparents),
        and every bd failure leaves the parent open: it never changes what the
        merge close reports. Candidates are walked in order, but only until one
        of them turns out to BE this child's parent — it lists the child among
        its own children — after which the walk ends whether or not the close
        happened here: already closed, a sibling closing it right now (the
        parentAutoCloseInFlight guard's loser), an open sibling, a bd refusal,
        a candidate bd could not answer for at all. Only "this is somebody
        else's parent" continues, because ParentCandidates' trailing entries
        are `blocks` sequencing edges that merely look like parents from the
        child's side, and falling through to one closes a bead outside the
        relationship that justified acting. The one exception is an
        orchestrated candidate, which is skipped rather than terminal: the
        Crucible owning that bead says nothing about the next candidate. Child
        IDs reach a persisted close reason and the rendered activity feed, so
        they go through termtext.Line first
  → worktree.Remove

Crucible path (parent beads with children AND the `crucible` opt-in label):
  bd ready (poller) → detect bead.Blocks (children) + epic.IsOrchestrated(labels)
    → a child labeled `independent` is carved out of all of this: not stamped
      with the epic branch, not in the parent's Blocks, not withheld from the
      dispatch loop, not claimed by the Crucible, and not part of the epic's
      completeness set — its work reaches main through its own PR, so an open
      one never holds up the epic's final PR
    → children of an orchestrating parent are withheld from the dispatch loop
      (crucibleOwnedChildren) so one poll cycle never runs a child twice —
      the opt-in label is what makes a parent an owner, not crucible_enabled,
      so with the Crucible off the children are held back and the PARENT is
      escalated to Needs Attention rather than each child hard-failing on
      "base branch not found on origin" and burning its circuit breaker
    → everything that would dispatch the opted-in PARENT alone escalates it
      instead, because the label is what routes the children and a standalone
      dispatch cannot un-route them: the Crucible disabled, open-but-unready
      children (poller.OpenChildren — only all-closed/no children runs the
      ordinary pipeline; a bd error defers instead, so a flaky beads database
      never burns the dispatch circuit breaker), and a schematic crucible check
      that declines — the last one sanitized through termtext.Line, since the
      model's own text now reaches a persisted, rendered Needs Attention row
    → the first two describe a CONDITION, so they carry the `epic on hold: `
      prefix and clearResolvedEpicHold withdraws them the moment the parent is
      orchestrable again (Crucible on + a child ready). needs_human is sticky
      and those conditions are not, so without it a transiently-blocked child
      deadlocked the whole epic until an operator ran `forge queue clear`. A
      schematic decline carries no prefix: a label contradicting its own check
      is resolved by an operator, not by time
    → crucible.Run()
      → worktree.CreateEpicBranch (epic.BranchName: feature/<parent-id>, or epic-branch:<name>)
      → fetch children via bd show, topological sort
      → for each child: pipeline.Run() → vcs.CreatePR(base=feature branch) → vcs.MergePR
      → vcs.CreatePR(feature branch → main) — final PR
      → bellows monitors final PR (CI fix, review, merge → close parent)

depcheck.Monitor (background, weekly by default)
  → scans each anvil for outdated dependencies (Go, .NET, Node)
  → npm half only: anvil has a live Kiln preview? → skip it this cycle (INFO,
    names the bead) — `npm ci` would delete the checkout's node_modules through
    the link the preview holds
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
  → Bellows mute: POST /prs/{id}/bellows/detach|resume → detach_bellows /
      reattach_bellows, the browser half of `forge bellows stop|resume`. Both
      resolve the row through requirePR, so the payload carries the row id and
      the number+anvil pair together and an ext-* PR mutes exactly like a
      forge-authored one; the flag rides back out on the PR payload's
      `bellows_detached`, which is what lets a muted PR read as muted rather
      than as `monitoring`
  → Kiln previews: POST /bead/{id}/preview/start|stop → preview_start/preview_stop
      (queued, so both answer 202; start forwards the body's optional `branch`
      verbatim and omits it when blank, leaving the daemon's forge/<bead-id>
      default in place); GET /previews → previews, mapped to the
      PreviewSummary/PreviewServiceStatus DTOs in internal/web/preview_handlers.go
      (the frontend contract) with an entry URL built by kiln.EntryURL — the
      preview hostname when preview_proxy_base is set (carrying the request's
      own scheme/port and a minted access token when the gate needs one), else
      preview_public_host falling back to the request's Host — plus `anvils` (the previewable
      anvils) so the SPA can gate its Preview button; `idle_remaining_seconds`
      and `resource_note` are forwarded verbatim from the preview manager's
      payload — same field names `preview_list` reports over IPC, computed once
      in the manager and never per transport — alongside `idle_deadline`, the
      absolute form of the same countdown; GET
      /preview/{id}/log/{service} tails
      ~/.forge/logs/<beadID>/preview-<service>.log
      SPA side: src/api/previews.ts is the typed client, src/hooks/usePreview.ts
      the shared per-bead state machine plus usePreviewsList for the whole fleet
      (one polled previews snapshot for every consumer, stamped with the
      `fetchedAt` the idle countdown ages from), <PreviewButton> the
      compact trigger + status chip mounted on worker cards and PR rows,
      <PreviewPanel> the full per-bead surface on the bead detail page
      (per-service port/health/uptime/log rows, Open preview, idle countdown,
      resource note, start/stop). A service that died after readiness reads
      `exited` on both surfaces — amber, its own badge — with the lifetime it
      had rather than a running clock, and where the Open button would be, the
      payload's `entry_note` says why the link was withheld: a control that
      silently vanishes reads as a UI bug. /previews (PreviewsPage, nav entry gated
      on Kiln being enabled) the fleet view of every running preview, which also
      mounts <AdHocPreviewForm> — the browser half of `forge preview start`:
      preview id + anvil (from the payload's `anvils`) + optional branch, run
      through the same usePreview start rather than a second polling path, so a
      branch with no bead can be previewed under a made-up registry key like
      kiln-smoke-1.
      lib/previewFormat.previewIdleCountdown is the one countdown renderer both
      surfaces call: it prefers the relative `idle_remaining_seconds` (immune to
      a browser clock that disagrees with the daemon's) and falls back to
      `idle_deadline`. <PreviewLogModal> tails
      one service log — plain monospace, not LogViewer, since preview output is
      raw process stdout rather than a claude stream-json transcript
  → Preview quest runs (QuestGiver): POST /bead/{id}/quests → preview_quest_run
      (202 + run id; the two gates — the anvil's preview_quests opt-in and a
      healthy preview — answer 403), GET /bead/{id}/quests[/{run_id}] →
      preview_quest_status, and GET
      /bead/{id}/quests/{run_id}/screenshot/{index} streaming one captured
      image (addressed by position in the run, never by path; image extensions
      only). The previews payload carries `quest_anvils` so the SPA gates the
      action without a probe per bead. SPA side: usePreviewQuests is the
      per-bead dispatch+poll state machine and <PreviewQuestResults> the status
      badge, per-quest rows and screenshot thumbnails mounted under
      <PreviewPanel>. Results are informational — no pipeline, Bellows or merge
      gate reads them, and the panel says so next to a failure
  → Host-based preview routing (settings.preview_proxy_base, preview_proxy.go):
      PreviewProxyMiddleware runs ahead of routing. A Host of <label>.<base> or
      <label>--<service>.<base> is authorised (preview_auth.go), resolved
      through preview_resolve (which also bumps the idle clock) and forwarded to
      127.0.0.1:<port> by one shared ReverseProxy — streaming, websockets and the
      request itself untouched apart from Hearth's own cookies, which are
      stripped. A host that names no live preview answers 404 in one request
      naming the state; every other host, apex included, falls through to the
      router
  → Preview auth gate (settings.preview_proxy_auth, preview_auth.go): unless the
      mode is `none`, a proxied request must present a Hearth session cookie
      (widened to the shared parent domain by sharedCookieDomain when the Hearth
      host and the proxy base have one) or a preview grant — a `?_forge_token=`
      HMAC over label+expiry, minted into the entry URL and exchanged on first
      contact for an HttpOnly `forge_preview_<label>` cookie. Refusals are 401
      (302 to the apex login for navigations), never a pass-through, and happen
      before preview_resolve so nothing unauthenticated probes the registry
  → Returning from that login (loginnext.go): the `next` the redirect carries is
      consumed by both /login handlers — the authenticated GET 303s to it, the
      POST answers {"redirect": ...} for the SPA to follow — but only after
      re-validating it against the live preview_proxy_base, and only where a
      session issued on this host would reach preview hostnames. Everything else
      falls back to the dashboard; a rejected next never fails the sign-in
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
- **prs** — Pull requests created across anvils. Three independent Bellows flags live on the row: `bellows_managed` (does Bellows run lifecycle workers for this PR at all — defaulted to 0 for `ext-*` PRs), `bellows_manually_assigned` (an operator pinned that answer, so the reconcile loop's defensive `ext-*` clobber leaves the row alone) and `bellows_detached` (managed, but **muted**: no events, no automatic quench/burnish/rebase/Assay dispatch, while mergeability and terminal state keep being refreshed). The detach flag is deliberately not folded into the other two — a mute is a different question from "is this PR ours" — and reconcile never touches it, so a detach survives the managed-flag rewrites. `UpdatePRBellowsDetached` is its only writer, behind the `detach_bellows` / `reattach_bellows` PR actions
- **events** — Timestamped event log (bead_claimed, smith_done, warden_pass, etc.)
- **retries** — Exponential backoff tracking; `needs_human=1` after exhausting retries
- **bead_costs / daily_costs** — Token usage and USD estimates per bead and per day
- **previews** — Kiln preview environments per bead: anvil, branch, status, worktree path, per-service JSON (name/port/health/pid/log, plus the started/exited timestamps and exit code a service that died after readiness carries), created/last-active timestamps
- **pending_bead_closes** — beads whose PR merged but whose `bd close` has not yet succeeded: PR number, close reason, cumulative attempts, last error, merge time. Re-attempted every Bellows cycle until the bead closes
- **review_fix_dispatches** — burnish dispatches per PR, keyed to the head SHA they ran against: head, attempts, last outcome (`pushed`/`unverified_push`/`preserved`/`failed`). The counter resets the moment the head moves, which is what separates a PR that is progressing from one rebuilding the identical diff every cycle; past `max_same_head_review_fixes` (default 2) the dispatch is refused with a Needs Attention entry instead. Fails open — an unreadable head SHA dispatches unchecked — and is cleared when the PR merges/closes or an operator runs `forge queue retry`
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
| `pr_action` | Multiplexed PR action; `pa.Action` ∈ `close`, `discard`, `recover`, `open_browser`, `merge`, `quench`/`cifix`, `burnish`/`reviewfix`, `rebase`, `assign_bellows`, `unassign_bellows`, `detach_bellows`, `reattach_bellows`, `approve`. The two bellows pairs are independent: assignment decides whether an external PR is managed at all, **detach** mutes one that is. `detach_bellows` persists `prs.bellows_detached` — refusing a PR it cannot resolve, since reporting success without writing the flag would leave the operator watching Forge keep working the PR — and then stops the in-flight quench/burnish/rebase workers for it through `killWorkerProcess`, off the response, because a fix session mid-run would otherwise push one more commit to the branch that was just taken off the loop. That kill is only real because the three fix packages record their Smith PID on the worker row next to the log path — without it `killWorkerProcess` takes its `pid <= 0` branch and marks the row failed while the session keeps running, which is a mute that stops nothing. `reattach_bellows` clears the flag and drops bellows' cached snapshot (`ResetPRState`) so the problems that outlived the mute are re-detected as fresh transitions; it restarts nothing. Neither verb bricks the PR: `actionBlockedByDetach` (`internal/daemon/detach.go`) is the one check behind both dispatch sites — `handleLifecycleAction` and `drainPendingAction`, so an action parked while attached cannot slip through by being drained after — and it lets `IsManual` requests past, which is what keeps `forge assay run` / `forge queue run` working on a muted PR. It refuses only the worker-spawning actions (CI fix, review fix, rebase, Assay), never `ActionCloseBead`/`ActionCleanup`: a detached PR still merges, and a merged bead left open blocks its dependents. Both verbs are named once, as `ipc.PRActionDetachBellows`/`ipc.PRActionReattachBellows`, which the daemon's own `case` labels read too: more than one front end sends them — `forge bellows stop|resume <pr> --anvil <name>` (`cmd/forge/bellows.go`) and the dashboard's `POST /api/prs/{id}/bellows/detach|resume` (`internal/web/pr_actions.go`) — and a second spelling is a caller that reports success while the daemon answers `unknown pr_action`. The CLI addresses the PR the way an operator reads it off a PR page (GitHub number + `--anvil`, `ext-*` PRs included) and sends neither a row id nor a bead id, leaving the resolution to `resolvePRTargetPreferID`. |
| `warden_rerun` | Re-run Warden review on a bead. |
| `assay_rerun` | Re-run the assay (E2E) checks on a PR. Payload: `ipc.AssayRerunPayload` — `anvil` required, plus **exactly one** of `pr` (the state.db row id the dashboard holds) or `pr_number` (the GitHub number, scoped by the anvil, which `forge assay rerun <pr> --anvil <a>` sends). Both forms resolve through the daemon's `resolvePRTarget` — the one lookup and one error set it shares with `pr_action`'s **rebase** branch, the other handler that takes either form (the row-id-only handlers still look up directly) — so neither form supplied, or both, is a clear refusal rather than a guess at which PR was meant. `pr_action`'s rebase resolves through the same helper and **refuses** on any failure — DB error, missing row, or an id owned by another anvil — rather than dispatching with an empty base: `rebase.Rebase` reads an empty `BaseBranch` as `main` and force-pushes, which for a crucible child based on `feature/<parent-id>` would rewrite the branch onto main and destroy its head. |
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
| `preview_start` | Start a bead's preview environment. Payload: `ipc.PreviewActionPayload` — `bead_id` and `anvil` required, `branch` optional (defaults to `forge/<beadID>`). The bead id is a registry key: it is never looked up in bd, which is what lets `forge preview start kiln-smoke-1 --anvil <name> --branch main` preview a branch that has no bead. Answers `queued`; the tracked request completes with `ipc.PreviewStartResponse{bead_id, status, message, entry_url}`, of which `request_status` surfaces only `message` — a client wanting the URL reads it back from `preview_list`. |
| `preview_stop` | Tear a bead's preview down. Payload: `ipc.PreviewActionPayload` — `bead_id` required, anvil ignored. Answers `queued`; the tracked request completes with `ipc.PreviewStopResponse{stopped, bead_id, message}`. A bead with no live preview is rejected synchronously with `no preview running for bead <id>` (the automatic teardown paths call the manager directly, where stopping something already gone stays a no-op). |
| `previews` / `preview_list` | One command under two names — the dashboard's and the CLI's. No payload; answers `ipc.PreviewListResponse` (an alias of `ipc.PreviewsResponse`): live previews with per-service ports/health, each carrying `entry_url` (the operator-facing link, built by `kiln.EntryURL` from the settings: `https://<label>.<base>/` when `preview_proxy_base` routes by hostname, else the entry `port` on `preview_public_host` — which is what makes `forge preview list` print the same link the dashboard shows), the entry `port`, `idle_remaining_seconds` (null when the reaper is disabled) and a `resource_note` summarising the services/ports it holds; plus `preview_public_host` and `preview_idle_timeout` so clients build links and deadlines themselves, and `anvils` — the anvils a preview can be started for (previews enabled AND a `.forge/preview.yaml` in their main checkout), which is how a client gates a per-bead Preview affordance without a probe per row, plus `quest_anvils` — the anvils that also opted into `preview_quests`, which gates the "Run quests" action the same way. |
| `preview_resolve` | Resolve a preview host label to the address serving it, for `internal/web`'s host-based proxy. Payload: `ipc.PreviewResolvePayload` — `label` (required, `kiln.PreviewLabel` of a bead id) and an optional `service` from the `--<service>` form. Answers `ipc.PreviewResolveResponse`: `found` with `host`/`port` (the preview bind host, wildcard reported as loopback), or `found=false` with a reason from the `ipc.PreviewResolve*` set (`previews_disabled`, `no_preview`, `stopped`, `no_service`, `no_port`, `not_serving`) — a refusal is an `ok` response, since "that preview is not running" is an answer. `not_serving` is the one that is not about the preview's existence: it is up, and the process behind that hostname failed or has exited, so forwarding would produce a connection error the browser reports as a network fault. A successful resolve **touches** the preview: unlike `previews` (which the dashboard polls, so counting it would disable the reaper), a proxied request is somebody using the preview. |
| `preview_quest_run` | Run the anvil's E2E quests against a bead's live preview. Payload: `ipc.PreviewQuestRunPayload` — `bead_id` only (the preview names its own anvil). Answers synchronously with `ipc.PreviewQuestRunResponse`: `started` plus a `run_id` to poll, or `started=false` with a `reason` from the `ipc.PreviewQuestReject*` set (`not_enabled`, `not_healthy`, `no_preview`, `previews_disabled`, `already_running`, `no_entry_url`, `unavailable`). A refusal is an `ok` response, not an IPC error, so the web layer can map the two gate reasons onto `403`. The browser work runs on its own goroutine. |
| `preview_quest_status` | Read one quest run. Payload: `ipc.PreviewQuestStatusPayload` — `run_id` for a specific run, else `bead_id` for that bead's most recent one. Answers `ipc.PreviewQuestStatusResponse`; a bead that never ran quests is `found=false`, not an error. |

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
