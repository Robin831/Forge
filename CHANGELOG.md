# Changelog

All notable changes to The Forge will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Unreleased changes live as fragments in `changelog.d/` and are assembled at
release time by `scripts/assemble-changelog.sh`.

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
