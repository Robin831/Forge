# Changelog

All notable changes to The Forge will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Unreleased changes live as fragments in `changelog.d/` and are assembled at
release time by `scripts/assemble-changelog.sh`.

## [0.14.0] - 2026-04-15

### Added

- **Async ack IPC protocol** - Added "queued" response type with request_id correlation and RequestTracker for async command handling in the IPC layer. (Forge-7v4u)
- **Auto-link node_modules in worktrees** - After creating a git worktree, Forge now automatically symlinks (or creates junctions on Windows) node_modules directories from the main checkout into the worktree. This eliminates the need for npm ci during temper for Node.js projects. (Forge-jqyw)
- **Configurable lint step severity** - Added `temper.lint_required` option to make the lint step fail the temper run instead of only warning. Default preserves existing advisory-only behavior. (Forge-lsj4)
- **Custom temper steps** - Support arbitrary named steps in per-anvil temper config via `temper.steps`, enabling pipelines with more than three steps, per-step working directories, timeouts, and required/optional control. The existing `build`/`test`/`lint` shorthand remains supported. (Forge-mycv)
- **Last-chance external_ref lookup before PR creation** - Pipeline now fetches the latest external_ref from bd right before creating a PR, ensuring Closes #N is included even when the reference was unavailable at dispatch time. (Forge-xz3d)
- **Needs Attention shows title, failure count, and recovery errors** - The TUI Needs Attention panel now displays bead titles, failure counts from dispatch and recovery circuit breakers, and properly classifies recovery failures as a distinct attention category. (Forge-idle)
- **Orphan recovery failure notifications** - Emit webhook notifications when a bead is flagged as needs-human due to repeated orphan recovery failures, including bead ID, title, failure count, and error details. (Forge-z2i9)
- **Recovery failure tracking** - Track consecutive orphan recovery failures and flag beads as needs-human after 3 consecutive failures or 30 minutes of failing, preventing infinite recovery loops. (Forge-y04i)
- **Temper path-based step filtering** - Temper steps can now include an optional `paths` glob filter. When set, the step is skipped if no changed files in the diff match the patterns, saving time on multi-stack repos where not all steps are relevant to every change. (Forge-p0sa)

### Changed

- **Async IPC command handling** - Daemon handlers that shell out to bd or gh (tag_bead, close_bead, stop_bead, retry_bead, pr_action close/merge, append_notes) now return a queued response immediately and execute the subprocess in a background goroutine, preventing IPC blocking during slow remote operations. (Forge-r3ks)
- **TUI async IPC correlation** - Hearth TUI now handles async "queued" IPC responses by subscribing to daemon completion events, correlating by request_id, and updating the status line with success or error messages. IPC read deadline reduced from 10s to 3s since only the fast initial ack needs to arrive. (Forge-pkkq)

### Fixed

- **Include external_ref in Smith prompt** - Pass the external reference (`external_ref`) from bead metadata through to the Smith prompt so generated PRs can include closing references without Smith needing to run extra commands. (Forge-pqrw)
- **Ledger labels column populated** - Join the labels table in bd sql queries so the Labels column in the Ledger list and kanban views displays bead labels instead of always being empty. (Forge-myij)
- **Orphan recovery bd stderr logging** - Capture and log bd stderr and exit status separately at WARN level when orphan recovery fails, aiding diagnosis of bd command failures. (Forge-jul4)
- **Poller bd ready limit** - Pass --limit=100 (configurable via settings.bd_ready_limit) to 'bd ready' so labeled lower-priority beads beyond the default top 10 are visible to the poller and dispatched. (Forge-4kx4)
- **Skip node_modules junction for dependency-update beads** - Depcheck beads now get a `deps-update` label, and worktree setup skips the node_modules junction for these beads so npm install writes to a fresh local directory instead of corrupting the main checkout. (Forge-0e0v)
- **Temper blocks npm ci when node_modules is a junction** - Detect when node_modules is a symlink/junction (from worktree linking) and skip destructive install commands like `npm ci` that would wipe the shared main checkout's dependencies. The step is skipped with a clear explanation instead of failing with EPERM. (Forge-bdqz)
- **Temper hooks fire in burnish and quench** - Pipeline stage hooks (`before_temper`, `after_temper`) now fire during burnish (review-fix) and quench (CI-fix) temper runs, not just during the initial pipeline. Setup commands like `npm ci` now apply uniformly across all temper invocations. (Forge-w8rb)
- **Worktree corruption guard** - Hardened worktree validation to check for .git file presence and gitdir integrity, preventing Smith from accidentally editing the main checkout when a worktree directory exists but lacks proper git linkage. Added retry-with-backoff for directory removal on Windows, post-creation verification, and a Smith-side pre-flight check that refuses to run in an invalid worktree. (Forge-cn6x)
- **Worktree removal unlinks junctions before recursive delete** - On Windows, os.RemoveAll follows reparse points (junctions/symlinks) into the target directory, causing failures when locked files are encountered. Worktree removal now walks the tree and unlinks all reparse points first, preventing recursive deletion from entering junctioned directories like node_modules. (Forge-17gt)

## [0.13.0] - 2026-04-11

### Added

- **Custom Temper commands per anvil** - Allow per-anvil override of build/test/lint commands in forge.yaml via the `temper` config block, enabling support for Python, Rust, and repos with non-standard build tooling without modifying Forge internals. (Forge-pjnr)
- **Per-stage provider configuration** - New `stage_providers` config map allows separate model/provider chains for each pipeline stage (smith, warden, schematic, cifix, reviewfix), enabling cost optimization by using cheaper models for simpler stages. Falls back to `smith_providers` then `providers` when a stage key is not set. (Forge-tq8g)
- **Pipeline hooks** - Configurable shell commands that run before/after each pipeline stage (schematic, smith, temper, warden). Hooks receive context via environment variables (FORGE_BEAD_ID, FORGE_WORKTREE_PATH, FORGE_BRANCH, FORGE_ANVIL_NAME, FORGE_ANVIL_PATH, FORGE_STAGE, FORGE_ITERATION). Before-hooks abort the pipeline on failure; after-hooks are best-effort. Configure per-anvil under `hooks` in forge.yaml. (Forge-esii)
- **Smith deny patterns** - Per-anvil file and command deny lists prevent Smith from modifying sensitive files or running dangerous commands, enforced via post-Smith diff validation in the pipeline. (Forge-21j8)

### Changed

- **Dependency updates** - Updated go-runewidth v0.0.21→v0.0.22 and modernc.org/sqlite v1.48.0→v1.48.1. (Forge-yjcn)
- **Rename cifix/reviewfix packages to quench/burnish** - Renamed internal/cifix to internal/quench and internal/reviewfix to internal/burnish, aligning package names with the blacksmith metaphor used throughout the codebase. Config keys and DB columns remain unchanged for backward compatibility. (Forge-8tex)

### Fixed

- **Bump bd subprocess timeout to 5 minutes** - Increase `executil.DefaultBdTimeout` from 60 seconds to 5 minutes to prevent premature kills on anvils with remote Dolt, kubectl port-forward, or GitHub auto-sync under concurrent load. (Forge-apqe)
- **Bump bd subprocess timeouts to 60s** - All bd subprocess invocations now use a centralized 60-second timeout (up from 10-30s) via `executil.DefaultBdTimeout`, preventing premature kills on anvils with remote Dolt or GitHub auto-sync where bd writes routinely take 20-30 seconds. (Forge-u21i)
- **Burnish temper verification gate** - Burnish now runs Temper verification before pushing review-fix commits, breaking the burnish-quench infinite loop on PRs with automated review comments. Previously, burnish trusted Smith to only push working code, but Smith often pushed broken builds which triggered quench, which then triggered another Copilot review, restarting the cycle. (Forge-syrn)
- **Include bead notes in Smith prompt** - The poller's `Bead` struct now captures the `notes` field from `bd ready --json`, and the prompt template renders it as a "Notes" section after the description so detailed implementation instructions are visible to the Smith. (Forge-wr3d)
- **Schematic tolerates bd dep add non-zero exit** - When `bd dep add` exits non-zero but stdout confirms the dependency was added, treat it as success instead of marking the bead as needing clarification. (Forge-30a5)
- **Schematic tolerates trailing noise in bd output** - Use streaming JSON decoder instead of strict `json.Unmarshal` so that trailing diagnostics from `bd create --json` (e.g. orphan detection warnings) no longer break sub-bead ID parsing. Applied the same fix to depcheck and ledger packages. (Forge-byca)
- **Windows build restored** - `internal/daemon/daemon.go` was calling the Unix-only `syscall.Kill` directly in `killWorkerProcess`, breaking every Windows build since Forge#497. The 2-phase interrupt → grace → kill logic is now routed through `signalInterrupt` / `signalKill` / `processAlive` helpers defined in the existing `killgroup_unix.go` / `killgroup_windows.go` platform split. No behavior change on Unix; Windows gains a best-effort `os.Interrupt` phase (CTRL_BREAK_EVENT for processes started with CREATE_NEW_PROCESS_GROUP) with `TerminateProcess` as the force-kill fallback. (Forge-81ks)
- **Worker kill: 2-phase SIGINT→SIGKILL with process group support** - Kill worker via IPC now sends SIGINT to the entire process group, waits up to 5 seconds, then escalates to SIGKILL. This ensures child processes (git, node, etc.) are also terminated. (Forge-yx7k)

## [0.12.0] - 2026-04-04

### Added

- **Hearth 'w' shortcut for manual Wicket scan** - Press `w` in the Hearth TUI to immediately trigger a Wicket issue triage scan instead of waiting for the next scheduled interval. A 30-second cooldown prevents accidental double-triggers. (Forge-gkdl)
- **Ledger external_ref display** - Show the GitHub issue link (external_ref) in the Ledger detail panel as a clickable OSC 8 hyperlink (e.g. "#42") and in the beads table as a compact "Ext" column. The column and link are populated automatically when beads are synced from GitHub via `bd github sync` or Wicket. (Forge-mopo)
- **Ledger update panel: dispatch to pipeline** - Added a "Dispatch to pipeline (via Forge daemon)" option to the Ledger dependency update overlay. When selected, Forge finds or creates today's consolidated dep bead for each anvil (using `depcheck.FindOrCreateBeadID`) and dispatches it through the full Smith → Temper → Warden pipeline via IPC, rather than applying updates directly. Shows a clear error when the daemon is not running. (Forge-lr1w)

### Changed

- **Consolidated depcheck bead per anvil** - Replaced per-package bead creation in `depcheck.Scanner` (specifically in `Scanner.scanAnvil`) with a find-or-create pattern that produces one date-stamped bead per anvil (`Package updates starting DD.MM.YYYY`). All npm, NuGet, and Go packages for the same anvil are grouped into this single bead. If the bead already exists the description is updated with any new packages; if not, a new bead is created and tagged with the anvil's configured `auto_dispatch_tag` for auto-dispatch. (Forge-8y9k)
- **Dependency update: modernc.org/sqlite v1.48.0** - Updated modernc.org/sqlite from v1.47.0 to v1.48.0. (Forge-zzup)
- **Ledger bead ID column shows short suffix** - The ID column now displays only the suffix part of a bead ID (e.g. `jxl2` instead of `Forge-jxl2`) so long prefixes like `Fhi.Metadata` no longer cause truncation. Press `y` on any selected bead to copy the full ID to the clipboard. (Forge-jxl2)
- **Lifecycle worker stale detection** - Lifecycle workers (CI fix, review fix, rebase) are now registered with a per-worker stale timeout (half of `smith_timeout`) so they can be detected as stalled even though they are excluded from the global background-phase stale check. Adds `stale_timeout` column to the workers table and `StaleTimeout` field on `state.Worker`. (Forge-erze)
- **Smelter startup-skip pattern** - The Smelter now skips its startup flush if a full cycle completed within the configured `smelter_interval`, preventing redundant PRs on daemon restarts. Logs `smelter_cycle_done` events after each flush cycle to track recency. For low-volume setups, `smelter_interval: 48h` or `72h` is a reasonable alternative to the default `8h`. (Forge-w9kj)

### Removed

- **Legacy update-deps command and raw command flow removed** - Deleted the `forge update-deps` CLI command, the `internal/depupdate` package (which ran raw `npm install`, `dotnet add package`, and `go get` commands directly), and the Hearth/Ledger TUI update overlay. Dependency updates are now handled exclusively through the bead-based flow created by the depcheck scanner. (Forge-8dy9)

### Fixed

- **Bellows PR merge no longer fails on active worktree branch** - Pass `--delete-branch=false` to `gh pr merge` so local branch cleanup is left to the worktree teardown, preventing a false failure when the branch is still in use. (Forge-x8g6)
- **Bellows: PR title not stored in prs table** - Bellows now persists the PR title fetched from the VCS API on each status check, backfilling any empty titles from previous runs. The `CreatePR` path in the GitHub provider also now stores the title at insert time so ready-to-merge notifications show the actual PR title instead of "PR #N". (Forge-p9nu)
- **Burnish worker no longer stalls on slow Smith exit** - Added `WaitWithExitTimeout` to `smith.Process` that signals I/O completion independently of process exit, then kills the subprocess after a 30-second grace period if it hasn't exited. The burnish (reviewfix) worker now uses this so thread resolution and the completion event are never blocked by a subprocess that is slow to terminate after pushing fixes. (Forge-m2es)
- **Copilot quota exhaustion now returns rate-limit error** - When the GitHub Copilot CLI's premium request quota is exhausted it may output plain-text (non-JSON) error messages to stdout instead of structured stream events. These lines are now scanned for rate-limit indicators so the provider fallback chain triggers correctly instead of treating the session as a generic failure. The `IsRateLimitError` phrase list also gains Copilot-specific patterns (`"premium request"`, `"request quota"`) to handle Copilot's quota error wording. (Forge-8fru)
- **Decrypt enc: webhook URLs from config** - Forge now decrypts AES-256-GCM encrypted webhook URLs (enc: prefix written by Hytte) when loading config, eliminating 'unsupported protocol scheme "enc"' errors on every webhook delivery. (Forge-t7qu)
- **Depcheck consolidated bead no longer auto-tagged with the configured auto-dispatch label (for example, forgeReady)** - Consolidated dependency update beads are now created without the auto-dispatch label. The user must manually apply the configured auto-dispatch label (for example, forgeReady) when ready to dispatch, matching the pattern used by feature bead chains. (Forge-pq0a)
- **Depcheck duplicate bead creation** - Fixed an issue where depcheck created a new "Package updates" bead each day instead of reusing the existing open one. The bead lookup now matches any open bead with the "Package updates" title prefix rather than requiring an exact date-specific title match. (Forge-sgi4)
- **Hearth poll health badges missing when config not in working directory** - When `forge hearth` is run from a directory without `forge.yaml` (e.g. SSH sessions on remote hosts), the anvil list was empty so poll health badges were never shown. Hearth now falls back to querying all known anvils from the events table when no config-derived anvil list is available. (Forge-h0sw)
- **Ledger Ext column compact display** - Parse full GitHub issue URLs (e.g. `https://github.com/org/repo/issues/42`) and display only the issue number (`#42`) in the Ext column instead of the full URL. (Forge-saxr)
- **Ledger bd close false error** - `bd close --json` sometimes exits with status 1 even when the close succeeded. Ledger now inspects the JSON output and treats the operation as a success when the returned bead has `status=closed` with a valid `closed_at` timestamp, preventing spurious error messages in the activity log. (Forge-hbxz)
- **Lifecycle worker timeout** - Burnish (review fix), quench (CI fix), and rebase workers now run under a deadline derived from `smith_timeout`, preventing indefinite hangs when a worker stalls. (Forge-hvta)
- **PR Ready to Merge notification includes PR title** - The push notification body now shows the PR title instead of the URL, making it readable at a glance on mobile (e.g. "PR #481 — Fix crucible branch targeting (Forge-t6y9, forge)"). (Forge-hdn1)
- **Pipeline PR base branch for dependency chains** - Regular blocked-by dependencies (task A blocks task B) no longer incorrectly route A's PR to `feature/B`. The epic branch lookup now only returns a feature branch for beads that are explicitly typed as `epic` or have an `epic-branch:` label; having "blocks"-type dependents alone is not sufficient. (Forge-t6y9)
- **Worktree cleanup no longer deletes remote branch** - Removed `git push origin --delete` from `(*worktree.Manager).Remove` which was running after PR creation and deleting the remote branch the new PR depended on, causing all PRs to fail with "Head sha can't be blank". Remote branch cleanup is now delegated to GitHub's auto-delete-branch setting or Bellows after merge. (Forge-0mmb)

## [0.11.2] - 2026-03-27

### Fixed

- **Windows Defender no longer flags Adventurer/Rod** - Disabled the leakless helper binary (`launcher.Leakless(false)`) that Rod extracts at runtime, which Windows Defender flagged as suspicious. Process cleanup is handled by context cancellation instead. (Forge-leakless)

## [0.11.1] - 2026-03-26

### Changed

- **Ledger immediate pending toast on action** - Show a "Closing Forge-xxxx..." / "Updating Forge-xxxx..." toast instantly when a form is submitted, so there is no silent gap between form dismissal and the bd command completing. (Forge-6frk)
- **vulncheck: modernize string/slice patterns** - Replace `bytes.Split`+range with `bytes.SplitSeq` iterator and manual duplicate-check loop with `slices.Contains` in the govulncheck JSON parser. (Forge-q66l)

### Fixed

- **Auto-create PR for orphaned branch on NO_CHANGES_NEEDED** - When Smith reports NO_CHANGES_NEEDED but a forge branch with commits ahead of main has no open PR, the daemon now automatically creates the PR instead of flagging needs_human. Only if PR creation itself fails does the bead escalate to needs_human, eliminating the most common "last mile" stuck scenario. (Forge-ueyj)
- **Hearth Wicket panel Tab navigation** - Include the Wicket issues panel in the Tab/Shift+Tab cycle so it can be focused like other panels. The panel highlights with the accent border when focused, and is skipped automatically when Wicket is disabled or has no data. Mouse click detection in the center column also now correctly targets the Wicket panel. (Forge-smxa)
- **Ledger close bead fails with exit status 1** - Added `--json` flag to all `bd close` calls in the Ledger (actions, bulk, update_overlay, and kanban lane moves) so they run non-interactively (matching how the daemon closes beads). Also improved `bdExec` error reporting to trim whitespace and include stdout content when stderr is empty, surfacing the real failure reason. (Forge-cspl)
- **Vulncheck skips redundant startup scan** - Vulncheck no longer re-scans on every daemon restart; if a scan already completed today (recorded in state.db), the startup scan is skipped and the next run follows the normal interval schedule. (Forge-nmsw)
- **Wicket bead creation fails with "unknown flag: --tag"** - The `bd` CLI renamed `--tag` to `--labels` (for create) and `--add-label` (for update), but Wicket still used the old flag names. Updated all references. (Forge-wicket-tag-fix)
- **Wicket pending issues no longer stuck after bead creation failure** - `shouldSkip` now treats a `pending` issue with no `bead_id` as retryable instead of permanently skipping it. The triage loop also handles the retry case when the pending row already exists in state.db, allowing the next scan cycle to reattempt bead creation without manual DB intervention. (Forge-bzii)
- **forge update binary not found** - Fixed `forge update` failing with "no release binary found" by matching the GoReleaser archive naming convention (`forge_{version}_{os}_{arch}.zip|tar.gz`) and extracting the binary from the downloaded archive. (Forge-qepp)

## [0.11.0] - 2026-03-25

### Added

- **Anvil context in triage prompt for external repos** - When a triaged issue originates from an external monitored repo (different from the anvil's own git-remote repo), prepend the anvil's README.md and AGENTS.md as an `<anvil_context>` section so the AI can contextualise the foreign issue against the implementing codebase's domain. (Forge-9lta)
- **Cross-anvil duplicate detection via Source URL** - Wicket triage now checks all configured anvil bead databases for a matching Source URL before calling the AI. If the incoming GitHub issue was already triaged in a different anvil, it is immediately returned as a duplicate without incurring an AI call. (Forge-2ftz)
- **Wicket GitHubClient interface and implementation** - Add `GitHubClient` interface with `ListIssues`, `GetIssue`, `CommentOnIssue`, `AddLabels`, `RemoveLabel`, and `CloseIssue` methods, backed by the `gh` CLI. Includes `MockGitHubClient` for use in tests and `ghclient_test.go` with JSON parse tests. (Forge-plcz)
- **Wicket Phase 2: non-trusted user triage, label filters, and bot ignore list** - Non-trusted contributors now receive a generic acknowledgement comment and are flagged for human review instead of running full AI triage. Obvious spam is silently discarded. A trigger label (`wicket_trigger_label`) enables pull-model processing where only labeled issues are triaged. Issue label filtering (`wicket_issue_labels`) now enforces ALL-label semantics in the poll loop. Known bot accounts (dependabot, renovate, github-actions, etc.) and custom `wicket_ignore_users` lists are silently skipped. Wicket events (bead created, flagged for review, rejected, error) now surface as toast notifications in the Hearth TUI. (Forge-9xqj)
- **Wicket Phase 3: Dispatch Confirmation + Follow-Up** - Adds dispatch confirmation via rocket reaction or "dispatch" comment, clarification re-triage when authors reply, PR linking and auto-close on merge, stale issue detection with configurable `wicket_stale_days`, and a new `label <tag>` comment handler for tagging beads from GitHub. (Forge-s96n)
- **Wicket `wicket_repos` config + repo resolution** - Added `RepoResolver` type in `internal/wicket/repos.go` that resolves the GitHub repository list for each anvil: uses explicit `wicket_repos` when configured, otherwise derives the repo from `git remote get-url origin`. The Monitor now maintains a `repo→anvil` mapping derived from resolved repositories so downstream dispatch and clarification code can look up anvil ownership without redundantly resolving configuration in many cases. (Forge-r8gs)
- **Wicket auth error handling** - Wrap GitHub API calls in the poll loop with auth-failure detection (HTTP 401/403, SAML SSO, bad credentials); log a clear actionable message including the repo name and `gh auth status` hint without crashing the loop. (Forge-9lta)
- **Wicket bead creation and comment templates** - Added `CreateBead` function that shells out to `bd create` with wicket/github-issue tags and stores the issue→bead mapping, plus Go text/template definitions for BeadCreated, ClarificationNeeded, and FlaggedForHuman comment types. (Forge-xi4r)
- **Wicket bead creation: anvil_name metadata and Source URL** - When Wicket creates a bead from a GitHub issue, the monitoring anvil name is now embedded in the bead's metadata (`anvil_name` field) and a `Source:` URL pointing to the originating GitHub issue is injected into the bead description. (Forge-1nda)
- **Wicket daemon wiring and CLI status command** - Wire the Wicket issue triage monitor into the daemon lifecycle (startup, hot-reload, shutdown) and add `forge wicket status` command that displays enabled state, monitored repos, issue counts by triage state, and poll interval via IPC. (Forge-qcae)
- **Wicket documentation** - Added a dedicated Wicket section to `docs/configuration.md` covering all global and per-anvil settings with defaults, `wicket_repos` multi-repo scanning, push vs pull (trigger_label) model explanation, and three example configurations (minimal, multi-repo, trigger-label). Added Wicket component entries to the Architecture tables in `README.md` and `CLAUDE.md`. (Forge-2p2e)
- **Wicket foundation layer** - Add config fields, shared types, state DB table, and event constants for the Wicket GitHub issue triage monitor. Includes `wicket_issues` SQLite table with full CRUD, `AnvilWicketConfig`/`TriageDecision`/`Issue` types, global and per-anvil config knobs (`wicket_enabled`, `wicket_interval`, `wicket_batch_size`, label defaults), and `EventWicket*` event constants. (Forge-apol)
- **Wicket integration tests** - Comprehensive mock-based integration tests covering the full lifecycle (issue triage → bead created → dispatch → PR linked → merged → issue closed), duplicate detection, already-fixed detection, out-of-scope with custom prompt, and rate limiting behavior. (Forge-bg5r)
- **Wicket monitor core** - Add `internal/wicket/wicket.go` with `Monitor` struct, `New`, `Run`, `UpdateConfig`, and `Stop` methods implementing the GitHub issue triage poll loop. The monitor iterates enabled anvils on a configurable interval, lists open issues, filters already-processed and already-tracked issues, checks the trusted-users list (bypassing AI triage for trusted authors), invokes AI triage for others, and dispatches the result (create bead / ask clarification / flag for human). Unit tests cover filtering logic, trusted-user detection, and the full triage dispatch flow. (Forge-t75t)
- **Wicket panel in Hearth TUI** - Adds a compact Wicket summary panel to the center column of the Hearth dashboard, positioned between the Workers panel and the Usage panel. Shows per-repo open issue counts and needs-human counts (⚠ highlighted in yellow when non-zero). The panel is shown only when `wicket_enabled: true` in config and there are repos with open issues. (Forge-jr41)
- **Wicket rate limiting and backoff** - Added `rateLimiter` type to track GitHub API quota and apply exponential backoff when rate limits are hit. The Wicket monitor now detects rate-limit errors (HTTP 403, secondary rate limit) from gh CLI stderr, doubles the poll interval when quota drops below 100, and backs off exponentially (1m → 2m → 4m, capped at 60m) on repeated failures. Logs `EventWicketError` when rate limited and skips remaining repos for the cycle. Also adds `FetchRateLimitRemaining` to query the GitHub rate_limit API endpoint. (Forge-yuag)
- **Wicket smart triage: duplicate, already-fixed, out-of-scope detection** - Triage AI now fetches open and recently-closed beads before evaluating each issue, enabling it to return `duplicate`, `already_fixed`, or `out_of_scope` verdicts with appropriate GitHub comments and per-anvil `wicket_triage_prompt` context. (Forge-svt8)
- **Wicket triage logic and AI prompt** - Implement `RunTriage` in the wicket package to call the AI provider with a formatted issue prompt, parse the JSON response into a `TriageDecision`, retry once on parse failure, and fall back to `ActionFlagHuman` on persistent failure. (Forge-7wwh)

### Changed

- **First-anvil-wins dedup at DB layer** - Wicket now explicitly detects UNIQUE constraint violations on `wicket_issues` INSERT and logs a clear "issue already tracked by another anvil, skipping" warning, implementing first-anvil-wins semantics to prevent double-processing across multiple anvils. (Forge-i2qo)
- **Wicket triage prompt includes multi-repo bead context** - The AI triage prompt now aggregates open and closed beads from all anvil paths mapped to the current anvil via the `wicket_repos` (5a) configuration, giving the AI visibility into work tracked across all related repositories rather than only the triggering anvil. (Forge-blay)

### Fixed

- **Adventurer test timeout** - Apply executor timeout to click/fill/assert element lookups and add t.Parallel() so tests run concurrently; prevents 300s suite timeout caused by rod's default 30s per-element wait. (Forge-b1h4)
- **Bellows ready-to-merge with CI in progress** - The Ready to Merge panel no longer shows PRs while CI checks are still running. When bellows preserves the last completed CI passing state for transition detection, it now correctly stores `ci_passing=false` in the database until all checks complete, and excludes in-progress polls from the ready-to-merge event trigger. (Forge-i3fz)
- **CLI update-deps worktree isolation** - The `forge update-deps --create-pr` command now creates an isolated git worktree for each anvil (matching the Hearth/Ledger overlay pattern), so dep commits land on the batch-update branch instead of main, multi-anvil runs no longer reset each other's branch, and the main anvil directory stays on main throughout. (Forge-n3m4)
- **Ledger forms (close, edit, etc.) now respond to Enter and Tab** - Huh form internal messages (focus/blur from Init) were not being forwarded to the active form, causing Enter (submit) and Tab (next field) to silently do nothing. (Forge-q06p)
- **Manual retry reset now clears warden-rejection needs_human flag** - The manual retry handler was calling `ResetDispatchFailures` (which only matches circuit-breaker rows) when `dispatch_failures > 0`, silently leaving `needs_human=1` for warden rejections. Replaced with `ResetRetry` so all needs_human states are cleared on manual reset. (Forge-5h0n)
- **NO_CHANGES_NEEDED safety check for un-PR'd branch work** - Before auto-closing a bead on NO_CHANGES_NEEDED, the daemon now checks whether a forge branch with commits ahead of main exists on origin without an open PR. When detected (caused by a prior dispatch that pushed commits but failed before PR creation), the bead is escalated to needs_human instead of being silently closed. (Forge-jvpg)
- **Schematic auto-closes parent bead after decomposition** - After decomposing a parent bead into sub-beads, the parent is now closed with `--force` instead of being left open. This prevents Forge from re-dispatching the parent once all sub-beads complete. (Forge-x75y)
- **Schematic chain transfer uses live dependency data** - The chain-aware decomposition now re-fetches the parent bead's dependencies via `bd show --json` before transferring, fixing a bug where the `blocks` and `depends_on` fields were empty due to JSON field name mismatch between bd output and the internal struct. (Forge-29ub)
- **Schematic decomposition now preserves dependency chain** - When a bead in the middle of a dependency chain (A → B → C) is decomposed into sub-beads (B1, B2, B3), the original `DependsOn` relationships are now transferred to B1 (first sub-bead) and `Blocks` relationships are transferred to B3 (last sub-bead). Previously, decomposing B would leave B1 unblocked by A and allow C to start before B3 completed. (Forge-29ub)
- **update-deps scan worktree isolation** - Create a temporary git worktree for the scan phase (including `--dry-run`) so that `npm ci` runs in an isolated directory rather than the main repo. This avoids EPERM errors on Windows when `.node` binaries in the main repo's `node_modules` are locked by editors or antivirus. Falls back to scanning the main directory if worktree creation fails. (Forge-xdjr)

## [0.10.0] - 2026-03-23

### Added

- **Auto-close matching dep beads after PR creation** - After `forge update-deps` creates a batch update PR, any open depcheck beads covering the same packages are automatically closed with the reason "Updated via forge update-deps", preventing the queue from filling with resolved work. (Forge-3cx3)
- **Data layer testability via bdExecFunc injection** - Introduced `bdExecFunc` type and `fetchAllBeadsWithExec` internal function to allow dependency injection of the bd CLI executor, enabling unit tests for `FetchAllBeads` without spawning real processes. (Forge-lmlh)
- **Dep update worker visibility** - When applying dependency updates from the Hearth U panel, a synthetic worker entry now appears in the Workers panel and log output streams into the Live Activity panel. Start/completion/failure events are also recorded in the event log. (Forge-j6v6)
- **Dependency batch PR creation** - Added `depupdate.CreatePR` to push a `deps/batch-update-<date>` branch and open a single grouped pull request per anvil after changelog commits complete. (Forge-z8sd)
- **Dependency update changelog generation** - Implement `GenerateChangelog` in `internal/depupdate/changelog.go` to produce changelog fragments for dependency batch updates, supporting both monolingual (`.md`) and bilingual (`.en.md`/`.nb.md`) projects. Fragments are git-added and committed automatically after group updates. (Forge-cvh8)
- **Dependency update command** - New `forge update-deps` command scans anvils for outdated dependencies and displays a dry-run summary. Supports `--anvil`, `--patch-only`, `--no-major`, and `--dry-run` flags. (Forge-6ilz)
- **Dependency update executors** - Add npm, dotnet, and Go install executors with temper verification, git rollback on failure, and per-group commit support for the direct dependency update pipeline. (Forge-fgr7)
- **Description viewer with glamour markdown rendering** - Press `d` on a bead in the Queue or Needs Attention panel to open a scrollable overlay that renders the bead's description field as formatted markdown using glamour. Dismiss with `Esc` or `q`. (Forge-1ndf)
- **Hearth 'U' keybinding for dependency updates** - Press `U` in the Hearth TUI to open the dependency update overlay, which scans all configured anvils for outdated packages, shows a grouped view (by anvil and dep group with patch/minor/major colour coding), and lets you apply all updates, patch+minor only, or select individual groups; emits toast notifications on start and completion. (Forge-ndn6)
- **Ledger TUI anvil filter and closed bead toggle** - Press `f` to cycle the anvil filter (All → anvil1 → anvil2 → … → All) and `s` to show/hide closed beads. The active filter is shown in the header (e.g. `[forge]  +12 closed`). Filter state persists across view switches and applies to all three views (list, kanban, hierarchy). (Forge-015x)
- **Ledger TUI bead CRUD operations** - Add create (n), edit (e), close (x), and reopen (r) key bindings to the Ledger TUI with huh form overlays and toast notifications for action feedback. (Forge-c64f)
- **Ledger TUI bulk operations** - Add multi-select and bulk close/label/priority to the Ledger TUI. Space toggles selection on the focused bead, Ctrl+A selects all visible beads, Ctrl+X bulk-closes all selected beads (with a summary toast), Ctrl+L sets a label on all selected beads, and Ctrl+P sets the priority on all selected beads. A selection count with bulk keybinding hints is shown in the footer when any beads are selected; Esc clears the selection. (Forge-hobz)
- **Ledger TUI dependency management** - Added `d` key to add a dependency (bead picker filtered to exclude self and existing deps, runs `bd dep add`), and `b` key to view/remove dependencies (shows blocks and depends-on relationships with optional removal). Both keys refresh the hierarchy view after changes. (Forge-crvm)
- **Ledger TUI help overlay and context-sensitive footer** - Press `?` in the Ledger TUI to open a scrollable help overlay listing all keybindings grouped by category (Navigation, CRUD, Metadata, Dependencies, Bulk, AI, Filters, General). The footer now uses `bubbles/help.Model` to render context-sensitive shortcuts that adapt to the current view (list, kanban, hierarchy), active overlays, and bulk selection mode. (Forge-hedw)
- **Ledger TUI kanban view** - Added kanban board view with four lanes (Open, In Progress, In Review, Closed) to the Ledger TUI. Navigate with h/l between lanes, j/k within lanes, and Shift+H/L to move beads. Toggle between list and kanban views with Tab. (Forge-995y)
- **Ledger TUI list view** - Table rendering with color-coded status rows, j/k navigation, scrolling viewport, and S key sort selector (Priority/Status/Updated). (Forge-k226)
- **Ledger TUI metadata operations** - Add label management (l), priority change (p), comment (c), notes edit (N), and assignee (a) operations to the Ledger TUI with huh form overlays and toast feedback. (Forge-b05i)
- **Ledger TUI** - New `forge ledger` command provides an interactive bead management TUI that fetches beads from all anvils, shows status counts, and enriches with PR data from the state DB. (Forge-n30e)
- **Ledger TUI: AI bead improvement** - Press `i` on any bead to invoke an AI provider (respecting `smith_providers` / `providers` config) that investigates the codebase and proposes an improved title, description, complexity estimate, and AI effort estimate. A spinner overlay shows while the AI runs; an approval overlay lets you accept or reject the changes before they are written back via `bd update`. (Forge-mcmw)
- **Ledger event/error panel** - Added a persistent activity panel (toggle with `E`) to the Ledger TUI showing recent operations and errors. Errors from bd commands, failed refreshes, and action results are now logged with timestamps and severity indicators, replacing transient toasts as the only visibility mechanism. The footer shows an `⚠ errors — E: show` hint when errors are logged but the panel is hidden. (Forge-xo0p)
- **Ledger hierarchy tree view** - New Hierarchy tab in the Ledger TUI showing epic/parent-child bead relationships as an indented tree with expand/collapse, status icons, progress badges, and dependency arrows. Accessible via Tab cycling (list → kanban → hierarchy → list). (Forge-vs9f)
- **Ledger mouse scroll support** - Scroll wheel events now navigate the bead list, kanban lane, and hierarchy view in the Ledger TUI, and add mouse-enabled startup to the Ledger interface. Pass `--no-mouse` to disable and restore normal terminal text selection. (Forge-wxb9)
- **Ledger multi-panel layout** - The Ledger TUI now renders a persistent bead detail panel on the right side (on terminals ≥120 columns wide), showing the selected bead's ID, title, status, priority, type, anvil, assignee, labels, dependencies, and description. Toggle visibility with `\`. This brings the Ledger in line with Hearth's first-class TUI experience. (Forge-lbin)
- **Ledger view switching via Tab/V** - Added `tab` and `v` keybindings to cycle through List → Kanban → Hierarchy views. View switching is now handled globally, and the List header shows "List" consistently with the Kanban and Hierarchy headers. (Forge-imxd)
- **Ledger: U keybinding for dependency updates** - Press `U` in the Ledger TUI to scan and apply dependency updates across all anvils using the same overlay as Hearth. After a successful update, Ledger offers to close any open dep-update beads that are now resolved. (Forge-d6lu)
- **Package grouper for dependency updates** - Groups related outdated packages using npm peer-dep analysis and scope-based grouping so they can be installed atomically. (Forge-qgfr)
- **Pending warden rules table** - Add pending_warden_rules table to state.db for staging learned warden rules before human approval, with CRUD operations. (Forge-q8no)
- **Periodic refresh with cursor state preservation** - The TUI now refreshes every 5 seconds with a concurrent-fetch guard (`refreshing` flag) to prevent overlapping refresh cycles, and restores the queue cursor to the previously focused bead by ID after each refresh so navigation is not disrupted by list updates. (Forge-w0u9)
- **Programmatic depupdate API** - Expose `Scan`, `Apply`, and `Preview` functions in `internal/depupdate` so Hearth and Ledger can query and apply dependency updates without going through the CLI. Introduces `Anvil`, `AnvilReport`, and `Result` types for structured, testable integration. (Forge-815c)
- **Responsive Ledger TUI layout** - Enforce 80×24 minimum terminal size with a clear "too small" message; kanban view degrades to 2 visible lanes on terminals narrower than 100 columns; list view hides Labels/Assignee columns on narrow terminals to keep the title readable; empty-state messages added for "No beads found", "No beads match the current filter", "No epics with children", and empty kanban lanes. (Forge-rlju)
- **Smelter config settings** - Add `smelter_enabled` (default true) and `smelter_interval` (default 8h) settings with hot-reload support and validation. (Forge-oixi)
- **Smelter daemon integration** - The smelter now runs as a background goroutine in the daemon on a configurable schedule (`smelter_interval`), with hot-reload support for enable/disable toggling and interval changes. (Forge-eyju)
- **Smelter flush logic** - Batch pending warden rules into PRs per anvil, with dedup, force-push to forge/warden-learn-batch branch, and automatic PR creation/update. (Forge-zp70)
- **update-deps full pipeline** - Wire interactive group selection, executor loop (install→verify→commit/rollback), changelog generation, PR creation, and dep-bead auto-close into a single `forge update-deps --create-pr` command. Add `--yes` flag to skip per-group prompts. (Forge-caoo)

### Changed

- **Ledger: anvil-first navigation** - The Ledger now opens an anvil picker instead of loading all beads at startup. Top-level shows registered anvils; selecting one fetches that anvil's beads. Closed beads are hidden by default; press `c` to toggle visibility. Press `esc` or `f` to return to the anvil picker from any bead view. The comment key has moved from `c` to `C` to free up `c` for toggling closed beads. (Forge-hyu7)
- **Warden learn routes to pending table when smelter enabled** - When `smelter_enabled` is true, learned warden rules from both auto-learn (Copilot comments) and CI fix learning are inserted into the `pending_warden_rules` table instead of creating immediate PRs or saving directly to the rules file. This allows the Smelter to batch-process rule additions. (Forge-aqqw)

### Fixed

- **Depcheck scan stdout suppression** - `runNpmInstall` in the depcheck scan phase now discards stdout (`io.Discard`) so npm ci output no longer leaks to the terminal and changes the tab title. (Forge-bjxm)
- **Depupdate bead auto-close** - Close matching depcheck beads after PR creation in both Hearth and Ledger update overlays. (Forge-9f1g)
- **Depupdate changelog format** - Generate changelog fragments in bold-title format with traceability tags, with proper Norwegian translations for bilingual repos. (Forge-9f1g)
- **Depupdate gh CLI compatibility** - Remove unsupported --json flag from gh pr create for older gh versions. (Forge-9f1g)
- **Depupdate remote branch cleanup** - Delete remote dep branch before pushing to avoid stale-info rejection on same-day re-runs. (Forge-9f1g)
- **Depupdate stdout suppression** - Add io.Discard to all git and bd subprocess calls in depupdate that were missing stdout capture, preventing terminal tab title corruption and TUI display artifacts during dependency updates. (Forge-h6k5)
- **Depupdate worktree isolation** - Dependency update runs (Hearth/Ledger update panel) now execute in an isolated git worktree instead of checking out a branch directly in the main anvil directory, keeping the anvil on `main` and preventing conflicts with concurrent Smith/Bellows operations. (Forge-4upd)
- **Hearth U key dependency update** - Wire `UpdateAnvils` from config in `cmd/forge/hearth.go` so pressing U in Hearth actually scans configured anvils instead of always showing 'No anvils configured'. (Forge-gtw9)
- **Hearth U panel stdout suppression** - Suppress subprocess stdout in depupdate install functions (npm, go, dotnet) so command output no longer leaks into the TUI terminal and corrupts the alt-screen display. (Forge-8cb2)
- **Hearth retry/dismiss for non-bead PRs** - Retry and dismiss actions in the Needs Attention panel now work for PRs that have no associated bead (e.g. warden-learn PRs). Previously the daemon rejected these with "bead_id and anvil are required"; now `bead_id` is optional when a `pr_id` is provided. The action menu also filters out bead-only actions (warden rerun, approve as-is, force smith) for non-bead PRs. (Forge-6v7m)
- **Hearth update scan title reset** - Reset terminal window title after dependency scan completes, not just after apply, preventing npm from permanently corrupting the tab title. (Forge-bjxm)
- **Hearth window title after update** - The `updateApplyDoneMsg` handler now resets the window title to "The Forge — Hearth" after dependency updates complete, restoring the correct tab title. (Forge-bjxm)
- **Kanban move error surfacing** - `moveBeadMsg` errors in the Ledger TUI now surface as a toast notification and trigger a data refresh, instead of silently setting the error field without feedback. (Forge-1lj7)
- **Ledger always-visible activity panel** - Activity panel is now permanently anchored at the bottom of the main panel instead of a toggleable overlay. (Forge-ryoz)
- **Ledger anvil mouse scroll** - Mouse wheel scrolling now works on the anvil selection screen. (Forge-ryoz)
- **Ledger anvil path inline display** - Selected anvil path no longer wraps to the next line. (Forge-ryoz)
- **Ledger bd list timeout** - Increase bd command timeout from 30 seconds to 3 minutes for slow Dolt connections. (Forge-g9h2)
- **Ledger bead filter always empty** - Fixed 'No beads match the current filter' in the Ledger by replacing single `bd list --status=open --status=in_progress` calls (unsupported by bd) with separate per-status calls in `FetchAnvilBeads` and `FetchAllBeads`. (Forge-g9h2)
- **Ledger closed-bead fetch timeout** - Cap `bd list --status=closed` at 50 results (instead of unlimited) and increase fetch timeout to 60s to prevent timeouts on remote Dolt anvils over slow connections. (Forge-jej7)
- **Ledger crash on out-of-range timestamps** - Sanitise `closed_at`/`updated_at` timestamps from `bd list --json` whose year is outside the JSON-safe range [0,9999], preventing a panic when the Ledger re-marshals them for the detail panel. (Forge-50yc)
- **Ledger detail panel height overflow** - Clip the detail panel content to exactly the terminal height before rendering so that selecting a bead with a long description no longer causes the bead list to scroll upward unexpectedly. (Forge-xfxj)
- **Ledger hierarchy detail panel width** - Hierarchy view now uses mainPanelWidth for title truncation, preventing the detail panel from being squeezed to ~6 chars. (Forge-ryoz)
- **Ledger loading progress** - Show anvil name and fetch steps during loading instead of generic message. (Forge-ryoz)
- **Ledger panel borders** - Added rounded borders to list, detail, and main panels matching Hearth TUI style. (Forge-ryoz)
- **Ledger scroll alignment** - Fixed viewport height calculations across all views (list, kanban, hierarchy) to account for borders and activity panel. (Forge-ryoz)
- **Ledger terminal tab title** - Set window title to "The Forge — Ledger" on init, matching the Hearth TUI pattern. (Forge-5np8)
- **Live Activity plain-text log lines** - The log tailer now surfaces plain-text lines (e.g. from the synthetic depupdate worker) in the Live Activity panel instead of silently dropping them. (Forge-bjxm)
- **Schematic generates detailed sub-bead descriptions** - When decomposing complex beads, the schematic now instructs the AI to produce detailed descriptions for each sub-bead including files to modify, function signatures, implementation approach, and connections to sibling sub-beads, instead of the generic 'Sub-task decomposed from <parent>' placeholder. (Forge-4i3r)
- **Smelter force-push on fresh worktree** - Fetch the batch branch from origin before pushing so `--force-with-lease` has a remote-tracking ref to compare against. Without this fetch, a freshly created worktree had no tracking ref and the push was rejected when the branch already existed on origin from a prior smelter run. (Forge-oc7l)
- **Smelter stale tracking ref** - Fixed `(stale info)` push failure after GitHub auto-deletes a merged batch warden-learn PR branch. The smelter now always creates batch worktrees from main (not the stale remote ref), and clears stale tracking refs before pushing. (Forge-owzy)
- **Subprocess console isolation** - Use CREATE_NO_WINDOW flag on Windows to fully prevent subprocesses from corrupting the terminal tab title via Console API calls. (Forge-bjxm)
- **TUI log output suppression** - Redirect Go's default logger to io.Discard while Hearth/Ledger TUI is running to prevent background goroutine log output from corrupting the alt-screen. (Forge-bjxm)
- **TUI update panel now applies deps via dedicated branch and PR** - The Hearth and Ledger U panel now creates a dedicated `deps/batch-update-<date>` branch, applies updates there, generates a changelog fragment, and opens a GitHub PR. Previously, updates were committed directly to the current branch (typically main). (Forge-9f1g)
- **depcheck dedup timeouts for remote Dolt anvils** - Increased `bd list` timeout from 15s to 60s and `bd show` timeout from 10s to 30s so anvils using a remote Dolt server (e.g. via kubectl port-forward) no longer skip bead creation. (Forge-6h1q)
- **npm install runs in correct directory for multi-package.json repos** - `InstallNpmGroup` now uses the `SourceDir` recorded on each `ModuleUpdate` (the directory where the `package.json` was found) instead of always running from the anvil root. This prevents spurious root `node_modules/` creation and ensures the right `package-lock.json` is updated when a repo has multiple `package.json` files (e.g. root and `web/`). (Forge-k04c)

## [0.9.0] - 2026-03-20

### Added

- **Adventurer browser executor** - New `internal/adventurer` package using Rod (go-rod/rod) for headless Chrome automation. Executes quest steps (navigate, fill, click, wait, assert, screenshot) with per-step timing and graceful error handling. (Forge-i0fr)
- **Batch CI fix and review fix modes for Copilot** - New `copilot_batch_ci_fixes` and `copilot_batch_review_fixes` settings combine multiple CI failures or review comments into a single Smith invocation, saving 1-3 premium requests per PR when using Copilot. Both default to false (opt-in). (Forge-99pk)
- **Changelog fragment format validation** - `forge changelog validate` (no args) now reports all malformed fragments instead of stopping at the first error. The release formula runs this check before assembly so bad fragments are caught early with clear error messages. (Forge-42fk)
- **Combined Smith+Warden prompt for Copilot single-request mode** - When `copilot_combined_smith_warden` is enabled and the primary provider is Copilot, Warden review criteria are embedded into the Smith prompt so Smith self-reviews its own diff. A real Warden still runs for P0-P1 beads, when concerns are flagged, or via configurable random sampling (`copilot_warden_sample_rate`, default 10%). Saves 1+ premium requests per bead. Config defaults to opt-in false with 10% sample rate. (Forge-d4ey)
- **Config file and disk space doctor checks** - `forge doctor` now warns about missing or unreadable config files, world-readable permissions (Unix), and low disk space (<1 GiB) on the forge directory and anvil volumes. Added `--strict` flag to treat warnings as failures. The release formula now runs `forge doctor --strict` as a pre-release gate. (Forge-cxwi)
- **Ingot lifecycle tracking in pipeline** - Every bead processed through pipeline.Run() now gets a corresponding ingot record tracking its journey through init, smith, temper, warden, approved, pr_open, and failed stages. Temper step results are recorded as structured test results with rune-based output truncation. PR creation updates ingot records with PR number and URL. All ingot writes are best-effort and never fail the pipeline. (Forge-y41p)
- **Ingot status counts in Hearth dashboard** - The Usage panel now shows a summary of ingot counts by pipeline status (smith, temper, warden, pr_open, merged, failed, etc.), giving operators a quick view of ingot pipeline health. (Forge-dw3n)
- **Ingot status updates on PR lifecycle events** - Bellows now updates ingot status to "pr_merged" when a PR is merged and "failed" when a PR is closed without merge, completing the ingot lifecycle tracking. (Forge-4eia)
- **New `internal/ingot` package with schema, types, and CRUD operations** - Introduces the `Ingot` data model that bundles a bead, PR, worker lifecycle, and structured test results into a single queryable record. Adds `ingots` and `ingot_test_results` tables via a new state migration, along with full CRUD operations (`InsertIngot`, `UpdateIngotStatus`, `UpdateIngotTemperResults`, `UpdateIngotPR`, `GetIngot`, `GetIngotsByStatus`, `InsertTestResult`, `GetTestResults`). (Forge-czem)
- **New forge doctor checks for git, provider CLIs, and provider auth** - Doctor now verifies git is in PATH (with version), checks that each configured provider's CLI binary is available, and validates per-provider authentication (API keys, OAuth, GitHub auth). (Forge-9507)
- **Per-anvil VCS provider support** - Each anvil now uses its own VCS provider based on its `platform` config setting instead of hardcoding GitHub for all anvils. Supports GitHub, GitLab, and Gitea. Providers are rebuilt on config hot-reload when anvil platforms change. (Forge-cdp4)
- **Quest CLI commands** - Added `forge quest list` and `forge quest run` subcommands for discovering and manually executing E2E quests across anvils. (Forge-9c3i)
- **Quest types and YAML parsing for QuestGiver** - Added `internal/questgiver` package with Quest and Step structs, YAML parsing via `ParseQuest`, and directory-based discovery via `DiscoverQuests`. (Forge-oihe)
- **QuestGiver configuration and daemon integration** - Added `questgiver_enabled`, `questgiver_interval`, and `adventurer_timeout` settings, per-anvil QuestGiver fields (`questgiver_enabled`, `questgiver_base_url`, `questgiver_setup_cmd`, `questgiver_teardown_cmd`), and wired the QuestGiver monitor into the daemon lifecycle with hot-reload support. (Forge-91p4)
- **QuestGiver monitor for E2E quest execution** - Adds a background monitor that polls anvils for quest definitions, executes them via the Adventurer browser executor, and automatically creates bug beads on failure with deduplication to prevent duplicates. (Forge-ghtw)
- **Skip Warden review for small Copilot diffs** - New opt-in setting `copilot_skip_warden_small_diffs` auto-approves small, low-risk diffs when the primary provider is Copilot, saving one premium request per skipped review. Applies when all criteria are met: ≤100 lines changed, docs/tests-only or ≤2 files, no security-sensitive paths, and P3+ priority. (Forge-mvqd)
- **Staggered anvil poll timers** - Anvil polls are now evenly distributed across the poll interval instead of all firing simultaneously, reducing burst load on Dolt and git. (Forge-w5x0)
- **Warden focused re-review on subsequent iterations** - When the Warden requests changes, subsequent reviews now only check whether the previously raised issues were addressed, instead of doing a full independent review. This prevents the 'whack-a-mole' pattern where each fix triggers new unrelated feedback and burns through iterations without converging. A new `warden_full_rereview` config toggle (default: false) reverts to the previous full-review-every-iteration behavior. (Forge-fwp7)
- **`forge ingots list` and `forge ingots show` CLI commands** - Query ingot records (bead lifecycle snapshots) from the daemon. List supports `--anvil` and `--status` filters; show displays full detail with temper test results. (Forge-wfc8)
- **`forge update` command for self-updating the binary** - Downloads the latest release from GitHub, verifies the checksum if a checksums file is present, gracefully stops the daemon, replaces the binary (with a `.bak` rollback on failure), and restarts the daemon. Works without Go installed. `forge status` now hints when a newer release is available. (Forge-1xir)
- **`warden_model_override` and `schematic_model_override` config settings** - Route Warden review and Schematic pre-analysis to a cheaper Copilot model (e.g. `claude-haiku-4-5` at 0.33× premium) while keeping Smith on a stronger model (`claude-sonnet-4-6` at 1×). Only Copilot provider entries are affected; non-Copilot providers are unchanged. (Forge-mtfa)
- **forge doctor: Dolt connectivity, depcheck tooling, and changelog fragment checks** - Added three new health checks: beads database connectivity verification via `bd list`, ecosystem-aware depcheck tooling detection (Go/npm/.NET), and changelog fragment validation for parse errors. (Forge-5rt6)

### Changed

- **Platform-aware `gh` check in `forge doctor`** - The GitHub CLI check now only runs when at least one anvil uses the GitHub platform. Non-GitHub setups (GitLab, Gitea, Bitbucket, Azure DevOps) no longer report a false failure for missing `gh`. (Forge-khg5)
- **Skip Schematic pre-analysis when primary provider is Copilot** - Copilot charges per-request rather than per-token, so Schematic pre-analysis would consume an extra premium request. The phase is now skipped automatically when the first provider in the chain is Copilot, unless the bead is tagged "decompose". (Forge-qkn6)
- **Update modernc.org/sqlite v1.46.1 → v1.47.0** - Routine minor version update of the SQLite driver dependency. (Forge-neab)

### Fixed

- **Allow all lifecycle actions on non-bead PRs** - The BeadID guard was too broad, blocking burnish/quench/rebase on PRs with no associated bead (e.g. warden-learn PRs). Added IsManual flag so both automatic and manual actions work on non-bead PRs. (Forge-3l3w)
- **Bellows no longer flags CI as failed while checks are still in progress** - Added proper handling for GitHub StatusContext items (legacy commit status API) alongside CheckRun items in CI status evaluation. Fixed edge cases where COMPLETED checks with empty conclusions (transient state) and REQUESTED status were not detected as in-progress. (Forge-d793)
- **Bellows no longer flags CI as failed while checks are still running** - Previously, bellows would emit `ci_failed` events when any CI check had a non-success conclusion, including checks that were still in progress. Now bellows waits until all checks have completed before evaluating CI status, preventing false failure events and unnecessary cifix attempts. (Forge-68vu)
- **Depcheck npm scanner now syncs node_modules before scanning** - Runs `npm install --ignore-scripts` before `npm outdated` so reported versions match the lock file instead of potentially stale installed versions. (Forge-cehs)
- **Handle PR creation failures and duplicate PRs in finalizePipeline** - PR creation errors now log a DB event, mark the bead as needs_human, and update worker status to failed. Duplicate PR detection uses a VCS-layer sentinel error (`ErrPRAlreadyExists`) instead of fragile string matching, and marks the worker as done instead of leaving it stranded in monitoring state. All DB write errors (UpdateWorkerStatus, ClearRetry) are logged instead of silently discarded. (Forge-yvu6)
- **close bead immediately when PR merged via IPC (Forge-xxn4)** (Forge-xxn4)
- **depcheck aborts scan when git pull fails on anvil (Forge-5qce)** (Forge-5qce)
- **log dispatch failure to event log on every attempt (Forge-eyds)** (Forge-eyds)
- **run bead close in background after IPC merge to avoid timeout (Forge-kox3)** (Forge-kox3)

## [0.8.0] - 2026-03-16

### Added

- **Anvil platform configuration** - Added `platform` field to anvil config (`github|gitlab|gitea|bitbucket|azuredevops`, default: `github`) to specify which VCS provider each repository uses. (Forge-9lor)
- **Auto-close decomposed parent beads with no dependents** - When schematic decomposes a bead into children and the parent has no dependents, the parent is now automatically closed since the children represent the actual work. Parents with dependents remain open to preserve blocking relationships. (Forge-qir6)
- **Filterable event log in Hearth TUI** - Press `/` in the Events panel to open a text filter. Events are matched by substring against timestamp, type, bead ID, and message. Shows filtered/total count (e.g. "12/89"). Esc clears the filter. (Forge-lm7w)
- **GitHub VCS provider** - Wraps the existing `ghpr` package as a `vcs.Provider` implementation, so `vcs.ForPlatform("")` and `vcs.ForPlatform("github")` return a working provider. (Forge-1363)
- **GitLab VCS provider** - Implements the VCS provider interface for GitLab using the `glab` CLI. Supports merge request creation, merging (with strategy validation), status checks, approval fetching, pending review request detection, unresolved thread counting, and open MR listing. (Forge-1363)
- **Gitea/Forgejo VCS provider** - Added VCS provider implementation for Gitea and Forgejo instances using the REST API v1. Configure with `platform: gitea` in anvil config. Authentication via `GITEA_TOKEN` or `FORGEJO_TOKEN` environment variable. Set `GITEA_URL` or `FORGEJO_URL` to override the API base URL (useful when git remotes use SSH but the API is served over HTTP). (Forge-ksia)
- **Local model support via Ollama backend** - Added `claude:ollama` provider syntax that redirects the Claude CLI to a local Ollama instance by setting `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` environment variables on the Smith subprocess. Use `claude:ollama/qwen2.5-coder:32b` to specify a model. Works as a fallback in the provider chain when cloud APIs are rate-limited. (Forge-d3l)
- **OpenAI/GPT provider support** - Added `openai` as a first-class provider kind in Forge's provider fallback chain. Configure with `openai`, `openai/gpt-5.1-codex`, or `openai:codex/o3` in your `providers` list. Uses the OpenAI Codex CLI (`codex`) with `--full-auto` mode and stream-json output parsing. Includes rate-limit detection and cost estimation (estimated from token counts when `total_cost_usd` is absent). (Forge-0m4)
- **Per-anvil auto-merge for ready PRs** - Added `auto_merge` config option for anvils that automatically merges PRs when they reach the ready-to-merge state (CI passing, no conflicts, no unresolved threads, no pending reviews). External PRs are never auto-merged. The Ready to Merge panel in Hearth shows an `[auto]` tag for PRs that will be auto-merged. (Forge-e827)
- **Sequential dependencies between decomposed children** - When Schematic decomposes a bead into sub-tasks, each child now depends on the previous one, ensuring they are dispatched in the logical order the AI specified rather than randomly. (Forge-cxjw)
- **Support 'no changes needed' as valid Smith outcome** - When Smith determines no code changes are required (e.g. the fix is already implemented or resolved upstream), it can now signal this via the NO_CHANGES_NEEDED: marker. The pipeline skips Warden and Temper, closes the bead with the reason, and logs a dedicated no_changes_needed event. (Forge-33zf)
- **Tool result correlation in Live Activity panel** - Tool invocations now show their outcome inline (e.g., `→ ✓`, `→ 3 matches`, `→ 2 files`, `→ ✗ error`). Bash, Grep, Glob, Edit, Write, and Agent results are enriched with exit status, match counts, and file counts. Both Claude and Gemini log formats are supported. (Forge-6nrm)
- **VCS provider interface** - Added `internal/vcs` package defining a platform-agnostic `Provider` interface for PR operations (create, merge, status, list). This enables future support for GitLab, Gitea, Bitbucket, and Azure DevOps alongside the existing GitHub integration. (Forge-9lor)
- **VCS provider interface** - Introduces `internal/vcs` with a platform-agnostic `Provider` interface, `Platform` parsing, and a `ForPlatform()` factory. This is the foundation layer; existing operations (daemon/bellows/crucible) still call `internal/ghpr` directly and will be migrated in follow-up work. (Forge-1363)
- **Warden re-review, Approve as-is, and Force Smith actions for Needs Attention beads** - Three new resolution actions in the Hearth TUI action menu: re-run warden on the existing branch (useful when rules changed or warden was too strict), approve as-is to bypass warden and create a PR directly, and force smith to push another implementation iteration with existing warden feedback attached. Each action is backed by a new IPC command and daemon handler. (Forge-wnlx)
- **X-Forge-Event HTTP header on webhook notifications** - Generic JSON webhook requests now include an `X-Forge-Event` header set to the event type (e.g. `pr_created`, `bead_failed`, `release_published`). This allows consumers like Hytte to identify and filter Forge-originated webhooks without parsing the JSON body. (Forge-9k1g)

### Changed

- **All VCS consumers use the Provider interface** - Crucible, CI fix, review fix, bellows, and warden now use the `vcs.Provider` interface instead of direct `gh` CLI calls. GitLab `ResolveThread` is fully implemented. Unsafe GitHub-only fallbacks removed. (Forge-oxlb)
- **Force Smith note pre-filled with warden rejection** - The note input shown when hitting "Force Smith" in the Needs Attention panel is now pre-filled with the most recent warden reject reason for that bead. The user can edit or augment the text before re-dispatching Smith. The description changes to reflect that content is present. (Forge-q2tf)
- **Improved text and thinking block rendering in Live Activity** - Text and thinking blocks now show up to 20 lines when expanded (up from 3), support markdown-lite inline styling (bold and code spans), and thinking blocks are visually dimmed to distinguish them from normal text output. (Forge-dw67)
- **Incremental log reading in Hearth** - Worker log files are now tailed incrementally instead of re-reading the entire file every 2-second tick, significantly reducing I/O for long-running workers. (Forge-80ro)
- **Live Activity panel now flows like Claude CLI terminal output** - Removed collapsible group headers (▸/▾) and [text]/[think] prefixes. Activity lines now flow continuously with blank-line separators between logical blocks. Tool entries keep their [tool] prefix. Thinking lines remain visually dimmed. Fixed an overflow bug where expanded groups could grow past the panel boundary. (Forge-hy0b)
- **Reduced smith re-exploration on warden/temper feedback iterations** - On iteration 2+, the prompt now includes the previous iteration's diff at the top with a directive to skip codebase re-exploration, significantly reducing wasted tool calls. (Forge-zfok)
- **Refactored GitHub PR operations behind VCS provider interface** - The `ghpr` package has been removed and its functionality replaced by the `vcs.Provider` interface with a GitHub implementation in `internal/vcs/github`. All callers (daemon, bellows, crucible, reviewfix) now use the `vcs.Provider` abstraction, enabling future support for GitLab, Forgejo, Bitbucket, and Azure DevOps. ReviewFix now safely handles nil VCS providers by falling back to the default GitHub provider. (Forge-co9n)
- **Rich tool call formatting in Live Activity panel** - Tool invocations now show contextual input details instead of raw JSON: Read shows filename+line range, Edit shows filename+changed text snippet, Bash shows the command, Grep shows the search pattern+glob, Write shows the filename, Glob shows the pattern, and Agent shows its description. Unknown tools fall back to a truncated JSON dump of their parameters. (Forge-3iim)

### Fixed

- **Bellows-managed PRs now appear as workers in the Hearth Workers panel** - Each PR actively managed by bellows (Forge-created PRs and external PRs explicitly assigned to bellows) creates a worker entry with phase "bellows" and status "monitoring", providing visibility into which PRs are being watched. Unmanaged external PRs remain display-only in the PR panel. Workers are automatically cleaned up when PRs are merged or closed. (Forge-p2y0)
- **Close beads for PRs merged during daemon downtime** - On startup, the daemon now runs a reconciliation pass that detects merged PRs in state.db whose beads are still open, and closes them. This prevents beads from staying in_progress indefinitely when a PR merges during a restart window. (Forge-q0qp)
- **Fix false empty-diff detection after smith pushes** - The hasEmptyDiff check now compares against the pre-smith HEAD SHA instead of @{upstream}...HEAD, preventing false needs_human escalation when smith commits and pushes successfully. (Forge-z9h6)
- **Force Smith no longer races with normal pipeline** - Force smith now claims the `activeBeads` slot before launching, preventing the poller from dispatching a normal pipeline run on the same bead concurrently. If the bead is already in flight when force smith is triggered, the IPC call returns an error instead of spawning a duplicate worker. (Forge-1lqk)
- **Force Smith note input now works correctly in Hearth** - Selecting "Force Smith" in the Needs Attention action menu now shows a note input overlay, accepts fast typing, and removes the bead from Needs Attention immediately when the smith starts. Four bugs fixed: missing `Init()` (form never started), missing `View()` (form was invisible), blocking tick commands in `driveHuhSync` (typing at ~1 char/sec), and `needs_human` only cleared after smith completed. (Forge-wnlx)
- **Force smith now continues with temper/warden/PR instead of re-dispatching** - After force smith completes, the pipeline proceeds directly to temper verification, warden review, and PR creation on the same branch. The force smith worktree is properly cleaned up before the pipeline creates its own, and PR creation uses the shared finalize path to avoid duplication. The bead stays in_progress throughout. (Forge-nixc)
- **Live Activity panel flows like a terminal** - New output now arrives at the bottom (oldest at top, newest at bottom) with auto-follow, matching Claude Code's terminal behaviour. The `[tool]` label prefix is removed — tool lines are rendered in a distinct muted blue colour instead. Blank lines now separate every tool call individually, not just on type transitions. Persistent `[result] success` line at startup is suppressed (session-end marker removed from the stream). (Forge-vk89)
- **Orphan recovery no longer races with post-merge bead close** - Extended HasOpenPRForBead to also protect beads with recently-merged PRs (within a 10-minute grace window), preventing orphan recovery from falsely resetting beads to open before the async bd close completes. (Forge-hp1h)
- **Orphan recovery no longer resets beads parked for human attention** - Beads with `needs_human=1` or `clarification_needed=1` in the retries table are now skipped during orphan recovery, preventing them from being re-dispatched without the human intervention they require. (Forge-nbee)
- **Prevent duplicate bellows workers in Hearth** - Remove stale pipeline worker row before bellows inserts its own, avoiding duplicate worker entries for the same bead. (Forge-uq7c)
- **Prevent re-dispatch of decomposed parent beads** - When a decomposed parent bead has dependents it stays open until they complete, then becomes unblocked and re-dispatched. The daemon now tags the parent with `forge-decomposed` so schematic detects it, returns `ActionAlreadyDecomposed`, and the pipeline closes the bead without spawning a new Smith session. (Forge-74a0)
- **Prevent smith from misusing NO_CHANGES_NEEDED in warden review iterations** - `NO_CHANGES_NEEDED` is now hidden from the smith prompt on warden review iterations (iteration 2+). If smith emits it anyway or produces no diff during a review iteration, the pipeline escalates to `needs_human` instead of looping indefinitely. (Forge-6gas)
- **Rebase/cifix/burnish skip non-bead PRs** - Lifecycle workers (rebase, CI-fix, review-fix) no longer fire for warden-learn PRs that have no associated bead ID. Previously, these PRs would trigger a rebase worker that ran in the `.workers/` directory itself (due to empty bead ID collapsing the worktree path), causing Smith to operate in the wrong directory. (Forge-6sed)
- **Schematic no longer exhausts turns without emitting verdict** - Strengthened prompt to require JSON output in the first response without tool use, and reduced default MaxTurns from 10 to 5 to prevent runaway investigation sessions for providers that support --max-turns (e.g. Claude CLI). (Forge-tapr)
- **Warden rules YAML quoting** - Auto-learned rules containing colon-space (`: `) in values are now double-quoted when saved, preventing YAML parse errors on reload that previously broke all rule loading for the anvil. (Forge-dkty)
- **reviewfix and cifix now run tests before pushing** - Both workers previously told smith to "ensure tests pass" without requiring it to actually run them. The prompts now explicitly instruct smith to run the test suite (go test, dotnet test, npm test, etc.) and fix any failures before committing or pushing, breaking the fix-comments→CI-fails→fix-CI→new-comments loop.
- Beads with deferred close (due to dependents) are now automatically closed when their PR merges, instead of staying in_progress indefinitely
- Crucible no longer misidentifies children as parents when both are in the poll batch — raw `blocks` field (child→parent direction) is now cleared before reconstruction, preventing the inverted parent-child relationship
- Removed ResolveBlocks entirely — bd show's dependents array lists beads that depend on me (parents I block), not children, causing systematic crucible inversion
- Smith no longer closes beads — the orchestrator now explicitly instructs agents not to call `bd close`, preventing dependent beads from being unblocked before the PR merges
- Warden hitting max review iterations now immediately surfaces the bead in Needs Attention — previously it cycled through the circuit breaker (3 more full dispatch attempts) before marking needs_human

## [0.7.1] - 2026-03-14

### Fixed

- **Depcheck scanners no longer inflate counts from .worktrees copies** - Added `.worktrees` to the skip list in `findNpmProjects` and `findDotnetProjects` so that worktree copies of a repo are not scanned as separate projects. (The Go scanner already avoids this — it checks only the root `go.mod` without walking subdirectories.) Also added `bin` and `obj` to the npm skip list for consistency, and added cross-project deduplication to `scanNpm` (matching the NuGet fix from c995ee4) to prevent the same package from appearing multiple times when scanned across several package.json files; the most severe update kind (major > minor > patch) is kept when conflicts arise. (Forge-tikw)
- **Fix install.sh checksum grep matching `.sbom.json` sidecar files** - Anchored the grep pattern to `" ${ASSET_NAME}$"` so it matches only the exact archive filename at end of line, preventing multi-line `EXPECTED_HASH` and false SHA256 mismatch errors. (Forge-dras)
- Assigning bellows to an external PR no longer requires a daemon restart — the snapshot cache is cleared on managed transition so seeding runs immediately
- Bellows now detects pre-existing issues (unresolved threads, merge conflicts) on newly assigned external PRs by seeding snapshot state to force transition detection on first poll
- Changelog assembler now inserts new versions before the first existing section instead of after the preamble, fixing out-of-order sections since v0.4.0
- Depcheck dedup cache now fetches all beads (`--limit 0`) instead of defaulting to 50, preventing duplicates when more than 50 open beads exist
- Depcheck no longer creates duplicate beads when the beads database is unreachable — the dedup cache now tracks validity and skips bead creation when `bd list` fails, instead of silently treating failures as "no beads exist"
- Depcheck now runs `git pull --ff-only` before scanning each anvil, preventing duplicate beads for dependencies that were already updated on main but not yet pulled locally
- Fixed v0.5.0 version heading typo (was incorrectly labeled as v1.5.0)
- NuGet depcheck deduplicates packages across multiple .sln/.csproj files in the same anvil, reducing false "outdated" counts
- PR reconciliation now correctly scopes lookups by anvil — previously a PR number from one repo could shadow the same number in another, preventing external PRs from appearing in Hearth
- Pre-existing external PRs (`ext-*`) no longer incorrectly receive bellows lifecycle management (cifix, reviewfix, rebase) after upgrade — a data fixup now resets `bellows_managed=0` for all `ext-*` PRs on startup
- Reconstructed missing v0.5.1 changelog section

## [0.7.0] - 2026-03-14

### Added

- External PRs (created outside Forge) now appear in the Hearth PR panel with `[ext]` tag
- New "Assign bellows" action on external PRs enables auto-monitoring for CI failures, review comments, and merge conflicts
- PR reconciliation with GitHub runs periodically and on PR panel open (`p` key)
- PRs table stores title and bellows_managed flag for external PR lifecycle control

### Changed

- **Documentation updated to reflect all current features** - Added `forge notify release` CLI reference, corrected `bellows_interval` default from 5m to 2m in reviewfix docs, and expanded the full forge.yaml example to include all available options (`go_race_detection`, per-anvil `golangci_lint`/`go_race_detection`/`depcheck_enabled`, and updated notifications to use the current nested `teams:`/`webhooks:` format). (Forge-o1i6)

### Fixed

- **Rate-limit backoff now visible in event log** - When all providers are rate limited, a `rate_limited` event is logged to the event store with the expected retry time (e.g. "Hytte-toa rate limited, will retry at 22:47"), making the backoff visible in `forge history events` and the hearth TUI. (Forge-sgzu)
- **Schematic and crucible workers now record PID and log path** - When the schematic or crucible spawns a claude subprocess, the worker DB record is updated with the process PID and log file path immediately after the process starts. This allows hearth's Live Activity panel to tail logs and show progress during the schematic phase. Previously the worker showed PID=0 and an empty log path for the duration of schematic analysis. (Forge-6j7q)
- Beads with warden hard-reject (no diff) now appear in Needs Attention immediately instead of requiring multiple failures to trip the circuit breaker
- Crucible: children with `blocks=[parentID]` no longer misidentified as crucible parents — the poller now filters `Blocks` to only include bead IDs present in the current poll batch

## [0.6.0] - 2026-03-13

### Added

- **Crucible action menu in Hearth TUI** - Paused Crucibles now have an action menu (press Enter) with Resume and Stop options. Resume retries the parent bead to re-enter the crucible loop; Stop closes the parent bead. (Forge-83ie)
- **Force-run bead independently** - Added `--force` flag to `forge queue run` and "Run independently" action to the Hearth queue menu. Force-run fetches the bead via `bd show` (bypassing `bd ready`), skips crucible detection and parent/blocker checks, and dispatches it straight through the pipeline as a standalone bead. Requires `--anvil` flag. (Forge-qxec)
- **Native Linux packages (deb, rpm, apk) via GoReleaser nfpms** - Releases now produce deb, rpm, and apk packages enabling installation via `apt install forge` or `yum install forge`. Includes a systemd unit file (`forge.service`) that is enabled on install, starting the daemon automatically after installation. (Forge-x0rf)

### Changed

- **Hearth action menus now have better visual spacing** - Added blank lines between header, options, and footer sections. Titles use bold accent styling and subtitles are dimmed for clearer visual hierarchy. (Forge-nteu)
- Rename display labels for CI fix and review fix workers to "quench" and "burnish" respectively, aligning with the forge/blacksmith naming theme throughout the TUI, logs, and IPC actions (Forge-quench-burnish)

### Fixed

- Change Workers panel selected row background from bright orange to subtle gray for readability (Forge-naby)
- Defer bead close when downstream beads depend on it (depends_on) — previously only blocks-type children triggered deferred close, so depends_on dependents could start before the PR was merged (Forge-crucible-blocks)
- Fix CI failure never detected on daemon restart — when CI was already failing with no fix attempted, bellows seeded the snapshot with ci_passing=false from the DB, matching the polled state and producing no transition. Now seeds ci_passing=true in this case to force detection (Forge-pr-ready-notify)
- Fix CI failure not detected after review fix — bellows snapshot cache was not reset after review fix completion, so CI failing (false→false) was never a transition and no quench worker spawned (Forge-pr-ready-notify)
- Fix PR ready-to-merge webhook notifications never firing — the event condition required `HasApproval` but Copilot only submits COMMENTED reviews (never APPROVED), so the condition was never satisfied. Removed `HasApproval` from the ready-to-merge check to match the Ready to Merge panel query (Forge-pr-ready-notify)
- Fix Workers panel header wrapping — MarginBottom padding from the title joined with the header's first line, creating an oversized line that pushed "Time" column to a new row (Forge-naby)
- Fix Workers panel table corruption caused by ANSI escape sequences in cell values — Bubbles table internally calls runewidth.Truncate which does not handle ANSI, breaking row alignment (Forge-naby)
- Fix notification context cancellation race in handleBellowsNotifications — the goroutine used the bellows polling context which could be cancelled before webhook HTTP calls completed, now uses a detached context with 30s timeout (Forge-pr-ready-notify)
- Fix poller Blocks reconstruction treating depends_on relationships as parent-child edges — only blocks and parent-child dependency types should be used, preventing the crucible from incorrectly adopting downstream beads as children (Forge-crucible-blocks)
- Strip forgeReady label during orphan recovery — prevents recovered beads from being immediately re-dispatched by the poller before human review (Forge-crucible-blocks)

## [0.5.1] - 2026-03-13

### Fixed

- Bellows skips lifecycle actions (cifix, reviewfix, rebase) for external PRs with ext- bead IDs (Forge-4h4i)
- Retry reset now resets bead status to open for poller visibility (Forge-4zea)

## [0.5.0] - 2026-03-13

### Added

- **Added UPX binary compression** - Compresses release binaries to reduce download size by 50-70% with no performance impact. (Forge-q72n)
- **View Smith log from the Workers panel** - Press `o` on a selected worker in the Workers panel to open that worker's log file in the full-screen viewport overlay. Previously log viewing was only accessible via the Needs Attention action menu. (Forge-x1bs)
- Documentation for remote Hearth access via VS Code Remote Tunnels (docs/remote-access.md). Covers setup, authentication, running alongside the Forge daemon, Claude Code remote sessions, and troubleshooting.
- Hearth TUI: "Stop" action in the Queue panel action menu (Enter on a queue item).
- Hearth TUI: press `S` on a worker in the Workers panel to stop its bead entirely.
- Hearth: dedicated PR panel overlay (press `p`) listing all open PRs with status indicators (CI, conflicts, reviews, approval) and action menu (Open in browser, Fix comments, Resolve conflicts, Close PR)
- New `OpenPRsWithDetail()` database query for PR panel data with title resolution
- New `pr_action` IPC command for triggering reviewfix, rebase, close, and open-in-browser on any open PR
- `forge queue stop <id> --anvil <name>` command to fully stop a bead: kills the running worker, sets clarification_needed (preventing re-dispatch), and releases the bead back to open. Use `forge queue unclarify` to resume.

### Changed

- **Hearth: use Bubbles table component for Workers panel** - Replaced the hand-rolled worker list rendering with the charmbracelet/bubbles table component, providing column resizing, better styling, and robust scrolling. (Forge-e50w)
- Added `state.LastPollPerAnvil()` query for efficient per-anvil health status lookup.
- Hearth Events panel: poll/poll_error events are no longer shown in the event log since anvil health is now visible in the Queue panel.
- Hearth Queue panel: anvil headers now show health badges (● green = last poll OK, ⊘ red = poll error) with time since last poll. For single-anvil setups, the badge appears in the panel title.

### Fixed

- **Fix auto-learn distillation errors** - Improved warden rule learning by increasing AI turns, adding provider fallback, and making JSON extraction more robust against code snippets and braces in strings. (Forge-k0nj)
- **Improve PR descriptions with detailed change summaries** - The Forge now extracts change bullets from Smith's changelog fragment and includes them in the PR description, providing much better context than the Warden's fallback verdict summary. (Forge-jhif)
- Pipeline retry now resets the worktree branch to the base ref (origin/main) instead of reusing commits from a failed run, preventing cascading junk commits and wasted API spend on hopeless retries.

### Security

- **Enable SBOM generation for releases** - Added GoReleaser v2's `sboms` section and installed Syft in the release workflow to generate Software Bill of Materials for supply chain security. (Forge-ztgx)

## [0.4.0] - 2026-03-12

### Added

- **Context-sensitive keybinding hints in Hearth footer** - The Hearth TUI dashboard now uses the `charmbracelet/bubbles` help component to show relevant keybinding hints for the focused panel (e.g. `K kill` in Workers, `enter expand/collapse` in Live Activity, `enter merge` in Ready to Merge). Hints update automatically as you switch panels with Tab. (Forge-ohpq)
- **Docker images published to ghcr.io/robin831/forge** - Multi-arch (amd64/arm64) Alpine-based container images are now built and pushed via GoReleaser on each release. Useful for devcontainers, CI pipelines, and the ForgeDevContainerTemplate project. Pull with `docker pull ghcr.io/robin831/forge:latest`. (Forge-0nyl)
- **Hearth: glamour-rendered bead descriptions** - When a bead is selected in the Queue or Needs Attention panel, press `d` to open a detail overlay that renders the bead's description as formatted markdown using charmbracelet/glamour. Code blocks, bold, links, and lists are all rendered properly. The overlay is scrollable and dismissible with `Esc` or `q`. (Forge-o0d4)
- **Hot-reload notifications config without daemon restart** - Changes to `notifications.enabled`, `notifications.teams_webhook_url`, and `notifications.events` in `forge.yaml` are now applied immediately via the hot-reload watcher. The notifier is atomically recreated so in-flight workers are unaffected. An invalid webhook URL during hot-reload falls back to the raw URL rather than disabling notifications. Previously a full `forge down` / `forge up` cycle was required to pick up notification setting changes. (Forge-0ld8)
- **Inline bead notes from the Hearth TUI** - Press `n` on a selected bead in the Queue or Needs Attention panel to open a textarea overlay. Type your notes and press `Ctrl+D` to append them to the bead. Press `Esc` to cancel. (Forge-x7j0)
- **Mouse support in Hearth TUI** - Enable mouse interactions in the Hearth dashboard: left-click to focus a panel, scroll wheel to navigate within the panel under the cursor. Mouse support is enabled via `tea.WithMouseCellMotion()`. Clicking while an overlay (action menu, merge menu, log viewer) is open dismisses it. (Forge-ecue)
- **Multiple generic webhook targets in notifications** - Added `notifications.webhooks[]` config to send a uniform JSON payload (`event_type`, `bead_id`, `anvil`, `message`, `timestamp`) to any HTTP endpoint. Each target can filter events independently. The Teams webhook keeps its Adaptive Card format; generic targets receive the simpler payload. Added `release` event type for generic webhooks alongside the existing `release_published` Teams event. Generic targets now correctly honour the global `notifications.enabled` flag, and receive `bead_decomposed` and `daily_cost` events (dispatched when a bead is decomposed by Schematic or when the daily cost limit is reached). Fixed a context cancellation race where webhook goroutines spawned by `Dispatch` would silently fail, and ensured the CLI command waits for HTTP requests to complete before exiting. (Forge-9fpc)
- **Release notifications via webhooks** - New `forge notify release` command sends release announcements to configured webhooks when a new Forge version is published. When a Teams webhook URL is configured, the CLI command delivers a rich Adaptive Card; all other endpoints receive a generic JSON payload (`event`, `version`, `tag`, `release_url`, `changelog_summary`). The release GitHub Actions workflow also posts a generic JSON payload via `curl` to any webhook URLs configured in the `RELEASE_WEBHOOK_URL` and `TEAMS_RELEASE_WEBHOOK_URL` secrets on tag push (no Adaptive Card formatting in this path). Configure additional generic webhook URLs via `notifications.release_webhook_urls` in `forge.yaml`. (Forge-284t)
- **Toast notifications in Hearth TUI** - Transient toast messages now appear at the bottom of the dashboard for key events: PR created, PR merged, bead closed, warden review passed, smith failure, PR merge failure, lifecycle exhausted, and crucible complete. Toasts auto-dismiss after 4 seconds using `tea.Tick` and stack up to 3 at once. (Forge-xp95)
- **Warden learns from CI fix patterns** - After a successful `cifix`, Forge now extracts ESLint rule IDs from the failing CI logs, distills them into warden rules (via Claude), and stores them in `.forge/warden-rules.yaml`. This allows the Warden to flag the same anti-pattern during code review before it ever hits CI, reducing cifix cycles from 5-9 down to 0-1. (Forge-fx7q)
- **Warden validates diff against bead description to catch scope drift** - The Warden review prompt now includes the bead title and description, enabling a 6th check: whether the diff actually implements what the bead requested. This catches partial implementations, scope drift, and cases where the Smith went off on a tangent. (Forge-95yi)
- **`pr_ready_to_merge` webhook notification** - Forge now sends a notification when a PR passes CI and warden approval and enters the Ready to Merge state. Sends a Teams Adaptive Card to the configured `teams_webhook_url` and exposes a `SendGenericPRReadyToMerge` helper for generic JSON webhooks. Add `pr_ready_to_merge` to the `notifications.events` filter to subscribe selectively. (Forge-2fzv)

### Changed

- **Hearth TUI now adapts colors for light and dark terminal backgrounds** - All color definitions in the Hearth dashboard have been migrated to `lipgloss.AdaptiveColor` pairs, replacing hardcoded dark-terminal-only color codes. Users with light terminal backgrounds now get proper contrast for all UI elements including panel borders, status indicators, priority labels, phase tags, and event types. (Forge-l0a8)
- **Hearth crucible panel uses bubbles progress bar** - Replaced the manual ASCII block progress bar in the Crucibles panel with a `charmbracelet/bubbles` progress component. The bar is color-coded: green when the crucible is complete, red when paused due to a child failure, and yellow while in progress. (Forge-3hjr)
- **Hearth log viewer now uses charmbracelet/bubbles viewport for scrolling** - Replaced the manual scroll state and line slicing in the log viewer overlay with the `charmbracelet/bubbles/viewport` component, gaining built-in page-up/page-down support and mouse wheel scrolling. The scroll indicator now shows a percentage instead of a line number. (Forge-qian)
- **Hearth now prompts before recovering orphan beads** - When the daemon detects an orphaned in-progress bead and Hearth is connected, it defers recovery to a dialog asking the user whether to Recover (reopen and re-queue), Close (mark work as completed), or Discard (close without retry). In headless/CI mode with no Hearth client, the existing auto-recovery behaviour is preserved. (Forge-zp4z)
- **Rich webhook payloads with pre-formatted summary and structured metadata** - Generic webhook POSTs now use a unified `WebhookPayload` schema with `source` (always `"forge"`), `summary` (human-readable one-liner), `event`, `detail`, `url`, `repo`, `version`, `tag`, `bead`, and `pr` fields. The `tag` field preserves the git tag exactly as passed via `--tag` (which may differ from `--version`, e.g. `"2.0.0"` vs `"v2.0.0"`). Receivers such as Hytte can display rich notifications without guessing field meanings. (Forge-si43)
- **Use huh for action menus** - Replaced hand-rolled action menus (Needs Attention, queue label, merge menu, and orphan dialog) with charmbracelet/huh form components for a cleaner and more standardized UI. (Forge-dntc)

### Fixed

- **Distinguish warden hard-reject from request-changes in event log** - Added a new `warden_hard_reject` event type for terminal warden rejections, separate from `warden_reject` which now exclusively represents request-changes verdicts. This makes it clear in the event log why a bead stopped early instead of iterating up to `max_pipeline_iterations`. (Forge-erdg)
- **Fixed main branch hijacking by worktree feature branches** - Moved the branch recovery logic to the worktree package with unit tests and added checks in the daemon to verify the anvil root is on main/master to prevent working environment corruption. (Forge-gll5)
- **Hearth: press `m` to toggle mouse capture on/off** - Mouse reporting (click-to-focus, wheel scroll) can now be toggled at runtime with the `m` key. Disabling mouse restores normal terminal text selection so bead IDs, error messages, and PR URLs can be copied. The footer hint updates to reflect the current state. Start with mouse disabled by passing `--no-mouse` to `forge hearth`. (Forge-yt7c)
- **Preserve smith logs after worktree cleanup** - Smith log files from `.forge-logs/` are now copied to `~/.forge/logs/<bead-id>/` before the worktree is removed, making post-mortem debugging possible after pipeline completion or failure. The worker's `log_path` in the state DB is updated to point to the persistent location. (Forge-6153)

## [0.3.1] - 2026-03-11

### Added

- **Homebrew tap support for macOS installation** - Configure GoReleaser to publish a Homebrew formula to `Robin831/homebrew-forge`, enabling `brew install Robin831/forge/forge` on macOS. Formula is auto-published on stable releases. (Forge-mx3g)
- **Scoop manifest for Windows installation** - GoReleaser now publishes a Scoop manifest to the `Robin831/scoop-forge` bucket, enabling `scoop install forge` as an alternative to the PowerShell install script. (Forge-dzzm)
- **tar.gz archives for Linux and macOS releases** - GoReleaser now produces `.tar.gz` archives for Linux and macOS targets in addition to `.zip` for Windows. The `install.sh` script has been updated to use `tar` (universally available) instead of `unzip`, removing the need to install an extra package on minimal containers. (Forge-3cjf)

## [0.3.0] - 2026-03-11

### Added

- **Auto-learn skip events in Hearth event log** - When bellows auto-learn finds no Copilot comments or no new rules on a merged PR, an `auto_learn_skipped` event is now logged to the Hearth event log so operators can confirm the feature is running. (Forge-7spt)
- **One-liner install script for Linux and macOS** - Added `install.sh` at the repo root that detects OS/arch, fetches the latest (or a pinned) release from GitHub, verifies the SHA256 checksum, and installs the `forge` binary to `~/bin`. The GoReleaser release body now includes the install command so it appears on every GitHub release page. (Forge-t3ba)
- **Spinner animations for active workers and crucible phases** - The Hearth TUI now shows animated braille dot spinners (⣾⣽⣻⢿⡿⣟⣯⣷) next to running and reviewing workers, and next to active crucible phases (dispatching, started, final_pr), making it immediately obvious which workers are actively processing versus stalled. The spinner updates at 100ms intervals independently of the 2-second data refresh cycle. (Forge-3gjk)
- **`max_pipeline_iterations` config setting** - The pipeline's Smith-Warden loop now reads its iteration cap from `settings.max_pipeline_iterations` in `forge.yaml` (default: 5) instead of a hardcoded constant. Previously this was always 5 with no way to tune it. The existing `max_review_attempts` setting remains unchanged and continues to control the Bellows review-fix cycles after PR creation. (Forge-ga7l)

### Changed

- **Hearth Ready to Merge action menu now shows PR title** - The merge action menu displays the bead title (resolved from queue cache or worker history) below the PR number header, matching the Queue action menu style. The menu width is also increased from 52 to 68 to match Queue action menu dimensions. (Forge-cqc6)

### Fixed

- **Bellows reliably retries CI fixes after failed attempts** - Bellows now directly detects when CI is still failing after a completed cifix attempt and re-emits EventCIFailed to trigger retries, rather than relying solely on snapshot cache resets which had timing issues with pending CI checks. (Forge-bzk6)
- **Crucible child failure no longer causes orphan recovery loop** - When a crucible child fails, the child bead is now reset to open and marked needs_human so orphan recovery won't pick it up and dispatch it as a standalone bead outside crucible context. (Forge-flf0)
- **Crucible child failures now correctly prevent standalone re-dispatch** - Failed crucible children use the "circuit breaker:" LastError prefix and the dispatch filter now checks all needs_human=1 rows, preventing re-dispatch outside crucible context. Also preserves existing retry counters and logs UpsertRetry errors. (Forge-roki)
- **Decomposed child beads now inherit the parent's auto_dispatch tag** - When Schematic decomposes a bead into children, the daemon now copies the `forgeReady` label (or whatever `auto_dispatch_tag` is configured) from the parent to each child, so they are picked up by the poller immediately instead of sitting in the queue forever. (Forge-fk5f)
- **Decomposed flag no longer clears retry record when no children created** - When schematic ran with `ActionDecompose` but produced zero sub-beads, the daemon incorrectly cleared the retry record, causing the bead to silently disappear instead of surfacing in Needs Attention. Now the retry record is only cleared when actual child beads were created. (Forge-0qj7)
- **Failed pipeline now clears bead assignee on release** - When a pipeline fails and the bead is released back to open status, the assignee is now also cleared so the poller can re-dispatch the bead. Previously, the assignee set during claim was never cleared on failure, causing the bead to remain permanently invisible to the poller. `releaseBead` (pipeline.go), `resetBead` (shutdown.go), and the `forge queue retry` IPC handler (daemon.go) now include `--assignee=` in the `bd update` call. Note: other paths that reset bead status (e.g. Crucible and Schematic parent resets) are not changed by this fix. (Forge-3kdt)
- **Orphan recovery no longer resets active Crucible parent beads** - The periodic orphan recovery scan previously could reset a Crucible parent bead back to `open` when its pending worker row was absent or terminal, even though the Crucible goroutine was still running. The recovery now consults the in-process `crucibleStatuses` map and skips any bead that has an active Crucible run. (Forge-epfe)
- **PR title now reflects bead intent instead of incidental commit messages** - `ghpr.selectTitle` previously derived the PR title from the Smith's most recent commit subject, which could describe a secondary fix discovered during implementation rather than the bead's primary goal. The PR title is now anchored to the bead title (with bead ID suffix) when available, falling back to the commit subject only when no structured bead title is provided. (Forge-l1x5)
- **PR titles and descriptions now prefer English when available** - When beads have non-English titles or descriptions (e.g. Norwegian), the PR title attempts to use Smith's English commit subject when available instead of the raw bead title, and the PR body leads with the English change summary from Warden review when present. The original bead description is preserved under an "Original Issue" section for context. (Forge-aaxy)

## [0.2.0] - 2026-03-10

### Added

- **Copilot premium request quota in Usage panel** - The `copilot_daily_request_limit` config setting is now documented in the configuration reference and example configs. Set to 300 (Pro) or 1500 (Pro+) to see a progress indicator like `5/300 premium req` in the Hearth Usage panel. (Forge-3s5)
- **Copilot premium request tracking and daily limit** - Tracks weighted premium requests per Copilot model (e.g. opus 4.6 = 3x, haiku 4.5 = 0.33x) and enforces a configurable daily limit via `copilot_daily_request_limit`. When exceeded, the Copilot provider is skipped in the fallback chain while other providers remain available. (Forge-dq2)
- **Daemon health indicator in Hearth TUI** - The header now shows a live connection indicator (● Connected / ○ Disconnected) with last poll time, so you can tell at a glance if the daemon is alive. (Forge-dl56)
- **Depcheck dedup event logging** - When depcheck skips creating a bead due to deduplication (existing open, in-progress, or recently closed bead), a `depcheck_dedup` event is now logged to the event table so operators can see why an update wasn't created in Hearth's event log. (Forge-vgv)
- **Hearth usage panel** - Added a compact Usage panel below Workers in the Hearth TUI showing today's per-provider cost/token breakdown, Copilot premium request usage, and total cost vs daily limit. Per-provider daily costs are now tracked in the state database. (Forge-bo0)
- **Live last-poll and queue-size in `forge status`** - The daemon now tracks actual last poll time and queue size instead of showing "n/a" and 0. (Forge-dl56)
- **Warden provider-specific verdict parsing** - The Warden now uses provider-aware fallback heuristics when parsing review verdicts. Claude, Gemini, and Copilot each have tailored parsing strategies: Copilot/Haiku outputs are parsed for natural language approval/rejection signals, Gemini outputs check for key-value verdict lines and markdown formatting, and Claude retains its existing JSON-first approach. This eliminates false "Could not parse structured verdict" fallbacks when non-Claude providers produce the review. (Forge-wrj)
- **`forge status --brief` flag** - One-line output suitable for shell prompts and status bars (e.g. `⚒ 2 smiths | 5 queued | 3 PRs | $1.23 | polled 30s ago`). (Forge-dl56)

### Changed

- **Copilot CLI now uses structured JSONL output** - Switched Copilot provider from PlainText (--silent) to StreamJSON (--output-format json), enabling token counting and cost estimation for Copilot runs. (Forge-6g6)
- **Hearth Live Activity: grouped events and expanded text preview** - Consecutive events of the same type (tool, text, think, etc.) are now collapsed into summary headers (e.g. "▸ [tool] x5 — Read, Edit, Grep") with the most recent group expanded. Text and thinking blocks now show up to 3 lines instead of 1, giving operators better visibility into what smiths are doing. (Forge-8m8)
- **Improved PR descriptions with bead context** - PR bodies now include the bead title, description, type, and a change summary from the Warden review instead of generic boilerplate. (Forge-gvnz)
- **Increased queue action popup dimensions** - The popup now shows up to 5 lines of description text (up from 3) and is 8 columns wider for better readability. (Forge-rk38)
- **Release notes include install instructions and categorized changelog** - GitHub releases now show a one-line install command, followed by changelog entries grouped into Features, Bug Fixes, and Other Changes instead of a raw PR list. (Forge-8j1)

### Fixed

- **Hearth Queue shows all registered anvils even when empty** - Anvils with no open beads now appear in the Queue panel with a (0) count instead of being hidden, so operators can see all registered repos at a glance. (Forge-0g1)
- **Improved queue action popup sizing and title display** - Widened the popup from 52 to 60 columns so menu options no longer wrap awkwardly, and bead titles now word-wrap up to two lines instead of being truncated to a single line. (Forge-uyd2)
- **Live Activity panel scrolling and group expansion** - Replaced broken scroll offset with the shared scrollViewport component used by other panels. Groups can now be expanded (Enter) and collapsed (Esc). Newest activity renders at the top without overlap. (Forge-0z2)
- **Orphan recovery no longer resets non-Forge beads** - Removed fallback in `RecoverOrphanedBeads()` that treated beads without a Forge worker record as orphan candidates after 15 minutes. Beads set to in_progress by humans or external tools are now always left untouched. (Forge-10c)
- **PRs no longer flash in Ready to Merge while Copilot review is pending** - New PRs now default to has_pending_reviews=1 (pending) and only appear in Ready to Merge after bellows confirms no reviews are outstanding. The merge handler also now checks for pending review requests in its live readiness gate. (Forge-6sc)
- **Track provider quota from all claude sessions** - Warden, cifix, reviewfix, and schematic now persist rate-limit quota data to state.db via UpsertProviderQuota, matching the existing smith behavior. Previously only smith sessions reported quota, causing the dashboard to undercount actual provider usage. (Forge-g5m)
- **depcheck_dedup events now include anvil name** - Event messages for skipped duplicate dependency updates now include the anvil name in the message text, making them unambiguous in the Events panel when multiple anvils are monitored. (Forge-3s8)

