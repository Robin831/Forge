# CLI Reference

## Global Flags

| Flag | Description |
|------|-------------|
| `--config <path>` | Path to config file (default: `forge.yaml` in cwd or `~/.forge/config.yaml`) |
| `--json` | Output in JSON format |
| `-v, --verbose` | Enable verbose output |
| `-V, --version` | Print version and exit |

## Daemon & Monitoring

### `forge up`

Start the Forge daemon.

```bash
forge up              # Start as background process
forge up --foreground # Run in foreground (for debugging)
```

### `forge down`

Stop the daemon gracefully.

```bash
forge down
```

### `forge pause`

Pause daemon-wide auto-dispatch. Forge stops claiming and dispatching **new**
beads, but all currently-running workers are left untouched and finish normally.
Manual `forge queue run <id>` still works while paused.

```bash
forge pause
```

Use this to drain the active worker set to zero before rebuilding or restarting
the daemon mid-day without trampling in-flight work: pause, wait for the active
workers to finish, then restart cleanly.

Note: pausing does **not** make a restart free — workers still running at restart
are killed and their beads reset to open (orphan recovery). The value of pausing
is letting the active set drain to empty first.

The pause is in-memory only and **resets on daemon restart** — a restart resumes
dispatch by default (resuming dispatch is the whole point of restarting), so
there is no persisted pause flag to clear afterward.

### `forge resume`

Resume auto-dispatch after a `forge pause`. New beads are dispatched again on the
next poll (a poll is triggered immediately).

```bash
forge resume
```

### `forge status`

Show daemon status, active workers, provider quotas, and recent events.

```bash
forge status
forge status --json
```

Output includes:
- Daemon PID and uptime
- Active worker count and queue size
- Open PR count
- Provider quota information (requests/tokens remaining, reset times)
- Dispatch pause indicator (shown when paused via `forge pause`)
- Active workers table (ID, bead, anvil, status, running time)
- Recent events

### `forge hearth`

Open the TUI dashboard. Requires the daemon to be running.

```bash
forge hearth
```

Three-column layout with up to seven panels:
- **Left column**: Queue (ready beads), Crucibles (active epic orchestrations, shown when present), Ready to Merge (PRs passing CI and approved), and Needs Attention (beads requiring human intervention)
- **Center column**: Workers (active Smith, Temper, Warden, CIFix, ReviewFix processes)
- **Right column**: Live Activity (streaming worker log) and Events (timestamped event log)

### `forge doctor`

Run health checks on the Forge installation.

```bash
forge doctor
```

Checks:
- `bd` (beads) installed
- `gh` (GitHub CLI) installed and authenticated
- `claude` (Claude CLI) installed
- State database accessible
- Daemon running
- IPC socket available
- `~/.forge` directory exists
- Anvils configured
- Autostart registration (Windows)

### `forge version`

Print version information.

```bash
forge version
# Output: forge v0.1.0 (build abc1234)
```

### `forge web revoke-sessions`

Revoke every active Hearth web-UI session, forcing all signed-in browsers to
re-authenticate. Use this as an incident-response escape hatch when a session
cookie may have been compromised. Talks to the running daemon over IPC; the
web server does not need to be enabled for the revocation to succeed.

```bash
forge web revoke-sessions
# Output: revoked 3 web session(s)
```

The Hearth web UI itself is gated by these environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `FORGE_WEB_ENABLED` | off | Enable the web UI when truthy (`1`/`true`/`yes`/`on`). |
| `FORGE_WEB_ADDR` | `:8080` | TCP listen address. |
| `FORGE_USERS` | — | `user:bcrypt-hash` pairs, comma-separated. |
| `FORGE_WEB_COOKIE_SECURE` | off | Force the `Secure` cookie attribute (set behind HTTPS). |
| `FORGE_WEB_SESSION_TTL` | `720h` (30d) | Sliding session lifetime (Go duration). |
| `FORGE_WEB_SESSION_ABSOLUTE_TTL` | `168h` (7d) | Absolute session lifetime cap, measured from creation, regardless of activity. |
| `FORGE_WEB_TRUSTED_PROXIES` | — | Comma-separated IPs/CIDRs of reverse proxies (e.g. Caddy/Cloudflare) allowed to set a trusted `X-Forwarded-For`. Unset means audit logs use the direct peer and ignore forwarding headers. |

The audit log's `remote` field is derived from the direct peer address unless
that peer is listed in `FORGE_WEB_TRUSTED_PROXIES`, in which case the rightmost
untrusted hop in `X-Forwarded-For` is used. Each web session may hold at most
20 concurrent Server-Sent Events streams; further stream opens receive a `429`
until a slot frees up.

Failed logins are progressively throttled per username and per client IP
(five free attempts, then 1s, 2s, 4s … up to 60s), and the session token is
rotated on every successful login.

## Repository Management

### `forge anvil add <name> <path>`

Register a repository as an anvil. The path must contain a `.beads/` directory.

```bash
forge anvil add my-api /path/to/repos/my-api
forge anvil add my-frontend /path/to/repos/my-frontend
```

Creates the anvil entry with defaults: `max_smiths=1`, `auto_dispatch=all`.

### `forge anvil remove <name>`

Deregister an anvil.

```bash
forge anvil remove legacy-repo
```

### `forge anvil list`

List all registered anvils with their configuration and status.

```bash
forge anvil list
```

Output columns: NAME, PATH, MAX SMITHS, AUTO-DISPATCH, STATUS (ok/missing/no .beads/).

## Work & Scheduling

### `forge queue`

Show ready beads across all anvils (alias for `forge queue list`).

```bash
forge queue
forge queue list
forge queue --json
```

Output columns: PRIORITY, ANVIL, ID, TITLE.

### `forge queue run <id>`

Manually dispatch a specific bead for execution.

```bash
forge queue run BD-42
forge queue run BD-42 --anvil metadata  # Disambiguate across anvils
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required if bead ID exists in multiple anvils) |

### `forge queue clarify <id>`

Mark a bead as needing human clarification before work can start.

```bash
forge queue clarify BD-42 --anvil my-api --reason "Which auth library should be used?"
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |
| `-r, --reason` | Explanation for why clarification is needed (required) |

### `forge queue unclarify <id>`

Clear the clarification flag so a bead can proceed.

```bash
forge queue unclarify BD-42 --anvil my-api
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge queue retry <id>`

Reset the circuit breaker and re-dispatch the bead on the next poll. Use this
when the previous attempt failed for a transient reason and you want Forge to
try again.

```bash
forge queue retry BD-42 --anvil my-api
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge queue clear <id>`

Clear the needs-attention flags from a bead without triggering a re-dispatch.
Use this when the underlying work is already done (PR merged, bead closed) and
you only want the bead to stop showing up in the needs-attention list. Unlike
`retry`, it does not schedule the bead for the next poll. Unlike `stop`, it
does not mark the bead as needing clarification. Idempotent — safe to run on
an already-clean bead.

```bash
forge queue clear BD-42 --anvil my-api
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

## PR Monitoring (Bellows)

Bellows watches every open PR Forge manages and dispatches the automatic
follow-up work — CI fixes (Quench), review fixes (Burnish), conflict rebases and
Assay review runs. These two verbs mute and unmute it for **one** PR.

A muted ("detached") PR is still watched: its mergeability and terminal state
keep being refreshed, so the PR panel goes on telling the truth, and a detached
PR that merges is still recorded as merged and its bead closed. What stops is
the automatic work — no events, no fix workers, no Assay runs — and its worker
row stays in place marked detached rather than vanishing.

Both verbs address the PR by its **GitHub PR number**, scoped by `--anvil`,
since PR numbers are per-repository. Externally-opened (`ext-*`) PRs are
addressed exactly the same way. A PR that cannot be resolved is refused, never
reported as muted.

### `forge bellows stop <pr-number>`

Detach a PR from Bellows. Also kills the CI-fix, review-fix and rebase workers
already running for it, so nothing pushes one more commit to the branch after
the mute.

```bash
forge bellows stop 431 --anvil heimdall
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil the PR belongs to (required) |

The mute never bricks the PR: manual verbs (`forge assay run`, `forge queue
run`, the dashboard's fix buttons) still run a single pass by hand.

### `forge bellows resume <pr-number>`

Reattach a PR to Bellows. Resuming drops the snapshot Bellows cached while the
PR was muted, so problems that outlived the mute — failing CI, a conflict,
unresolved threads — are re-detected as fresh transitions instead of being
swallowed as state it has already seen. Nothing that was skipped is replayed:
automatic work resumes from the next cycle onwards.

```bash
forge bellows resume 431 --anvil heimdall
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil the PR belongs to (required) |

## Preview Environments (Kiln)

Preview environments require `settings.preview_enabled` and a
`.forge/preview.yaml` manifest in the anvil's main checkout — see
[preview-manifest.md](preview-manifest.md) for the schema and the
`preview_*` settings in [configuration.md](configuration.md).

### `forge preview list`

List every running preview with its status, entry URL, idle countdown and the
resources (services and ports) it is holding.

```bash
forge preview list
forge preview list --json     # raw preview_list payload
```

| Flag | Description |
|------|-------------|
| `--json` | Emit the daemon's payload verbatim (adds per-service health and the previewable anvils) |

### `forge preview start <bead-id>`

Start a preview environment: check the branch out into its own detached
preview checkout, run the manifest's setup command and supervise the declared
services. Waits for the daemon's outcome and prints the entry URL.

```bash
forge preview start Forge-abc1 --anvil forge
forge preview start kiln-smoke-1 --anvil my-api --branch main
```

The bead id is a **registry key, not a lookup**. It names the preview, keys its
logs under `~/.forge/logs/<bead-id>/` and derives its hostname label, but it
does not have to exist as a bd issue — which is what makes this usable for
ad-hoc work: smoke-testing a new manifest, verifying a deployment, or
previewing a branch that has no bead yet. Such previews conventionally use ids
like `kiln-smoke-1`.

Without `--branch`, the bead's canonical `forge/<bead-id>` branch is previewed.

A refusal — previews disabled globally or for the anvil, no manifest, the
`preview_max_concurrent` cap already full — is printed as the daemon phrased it
and exits non-zero. `forge preview stop <bead-id>` is the inverse.

Hearth 2.0 offers the same thing in the browser: the **Ad-hoc preview** form at
the top of its `/previews` page takes the same id, anvil and optional branch.

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil the branch lives in (required) |
| `-b, --branch` | Branch to preview (default: `forge/<bead-id>`) |

### `forge preview stop <bead-id>`

Tear a preview down: kill its supervised services, run the manifest's teardown
command and remove the preview checkout. A bead with no running preview is an
error, not a silent success.

```bash
forge preview stop Forge-abc1
```

## Scanning

### `forge scan`

Run `govulncheck` on registered Go anvils to check for known vulnerabilities.

```bash
forge scan                    # Scan all anvils
forge scan --anvil my-api   # Scan a specific anvil
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Scan only this anvil (optional; default: all) |
| `--create-beads` | Automatically create beads for discovered vulnerabilities (default: true) |

## AI PR Review (Assay)

### `forge assay stats`

Aggregate the recorded Assay runs into ISO weeks and print what a review costs
lately: run count, mean cost, mean duration, and the split by coverage outcome
(complete vs partial).

A single run's cost only reads as wrong against the runs around it, so this is
what catches a step change in the week it happens rather than in a month-end
total. The complete/partial split is the point of it: a partial run that costs
*more* than a complete one is invisible in one blended mean and obvious with the
two printed side by side.

```bash
forge assay stats                  # The current ISO week plus the four behind it
forge assay stats --weeks 12       # A longer window
forge assay stats --json           # Sums and means per week, for further analysis
```

```
Assay runs by ISO week (last 5 weeks, 640 runs):

  2026-W30: 128 runs, mean $0.184, mean 88s | complete 120 runs $0.180/86s | partial 8 runs $0.240/120s
  ...
  2026-W34: 128 runs, mean $0.412, mean 94s | complete 101 runs $0.395/88s | partial 27 runs $0.475/116s

WARNING cost drift: 2026-W34 mean $0.412/run over 128 runs is 2.24x the trailing 4-week mean $0.184/run (512 runs)
```

| Flag | Description |
|------|-------------|
| `--weeks` | ISO weeks to report, ending with the current one (default 5, max 104) |
| `--json` | Emit the per-week sums, means and any drift flag as JSON |

The daemon writes the same report to its own log once a day (`Assay weekly
cost`), plus a WARN (`Assay weekly cost drift`) when the current week's mean
cost per run exceeds the trailing four weeks' by more than 1.5x — so the signal
reaches an operator who never runs the command.

Runs that never reviewed a diff (skipped by the Bellows trigger gate, a failed
diff fetch) are excluded. Runs that failed *after* spending are included: a
failure is not a refund, and spend shifting into failed runs is exactly what the
report exists to expose.

### `forge assay run` / `forge assay rerun`

Force a fresh Assay review over a PR's current head, bypassing the trigger
gate's head-SHA debounce. `run --pr <id>` addresses the PR by its `state.db` row
id (what the dashboard holds); `rerun <pr>` addresses it by GitHub PR number,
scoped by `--anvil`.

```bash
forge assay run --pr 12 --anvil my-api
forge assay rerun 431 --anvil my-api
```

## History

### `forge history`

Show completed workers (default view).

```bash
forge history
forge history -n 50
```

| Flag | Description |
|------|-------------|
| `-n, --limit` | Number of entries to show (default: 20) |

### `forge history workers`

Show completed worker history.

```bash
forge history workers
forge history workers -n 10
```

Output columns: ID, BEAD, ANVIL, STATUS, DURATION, COMPLETED.

### `forge history events`

Show the event log.

```bash
forge history events
forge history events -n 100
```

Output columns: TIME, TYPE, MESSAGE, BEAD, ANVIL.

## Cost Reporting

### `forge cost assay`

Report what Assay spent over a window, split by first-review vs re-review of a PR
and by prompt-cache token class. See
[assay-cost-attribution.md](assay-cost-attribution.md) for the methodology and
the baseline reconciliation.

```bash
forge cost assay
forge cost assay --since 2026-06-01 --until 2026-07-01
forge cost assay --format json --out before.json
forge cost assay --expect-repeat-cost 2326.54 --expect-repeat-runs 780
```

| Flag | Description |
|------|-------------|
| `--since` | Start of the window, inclusive (`YYYY-MM-DD` or RFC3339; default: no lower bound) |
| `--until` | End of the window, exclusive (`YYYY-MM-DD` or RFC3339; default: no upper bound) |
| `--format` | `table` (default), `json` or `csv` |
| `--out` | Write the report to this file instead of stdout |
| `--anvil` | Restrict the report to one anvil |
| `--include-skipped` | Count runs that dispatched no passes (default: excluded, matching the per-PR run cap) |
| `--model-tier` | Pricing row for token classes: `haiku`, `sonnet` (default), `opus`, `fable` |
| `--expect-repeat-cost` | Reconcile the repeat-run total against a published baseline figure (USD) |
| `--expect-repeat-runs` | Reconcile the repeat-run count against a published baseline figure |

Recorded spend (the provider's own `cost_usd`) and priced cache attribution are
reported separately and never summed: `assay_runs` stores no plain input/output
token counts, so the cache classes are a subset of the recorded total. Runs
predating cache instrumentation report token class `unknown` rather than a
misleading zero. `forge cost` with no subcommand runs this report.

## Configuration (Windows)

### `forge autostart install`

Register Forge as a Windows Task Scheduler logon task for automatic `forge up` at login.

```bash
forge autostart install
```

### `forge autostart remove`

Remove the autostart task.

```bash
forge autostart remove
```

### `forge autostart status`

Check autostart registration status.

```bash
forge autostart status
```

### `forge autostart generate`

Generate the Task Scheduler XML without registering it.

```bash
forge autostart generate
```

## Changelog Fragments

### `forge changelog assemble`

Assemble `changelog.d/` fragments into `CHANGELOG.md`.

```bash
forge changelog assemble
forge changelog assemble --dir . --output CHANGELOG.md
forge changelog assemble --dry-run
```

| Flag | Description |
|------|-------------|
| `--dir` | Directory containing `changelog.d/` (default: `.`) |
| `--output` | Output file path (default: `CHANGELOG.md`) |
| `--dry-run` | Print assembled output without writing |

### `forge changelog validate`

Check that changelog fragments exist for the specified bead IDs.

```bash
forge changelog validate Forge-abc Forge-xyz
```

| Flag | Description |
|------|-------------|
| `--dir` | Root directory containing `changelog.d/` (default: `.`) |

Exits non-zero if any bead is missing a fragment.

## Warden Rule Management

### `forge warden learn`

Learn review rules from GitHub Copilot comments on recently merged PRs for an anvil. Rules are saved to `<anvil-path>/.forge/warden-rules.yaml`.

```bash
forge warden learn --anvil my-api
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge warden list`

List all learned review rules for an anvil.

```bash
forge warden list --anvil my-api
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge warden forget`

Remove a learned rule by ID.

```bash
forge warden forget <rule-id> --anvil my-api
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

## Notifications

### `forge notify release`

Send a release notification to configured webhook endpoints (MS Teams Adaptive Card and/or generic JSON webhooks).

```bash
forge notify release --version v1.2.3
forge notify release \
  --version v1.2.3 \
  --tag v1.2.3 \
  --release-url https://github.com/org/forge/releases/tag/v1.2.3 \
  --changelog "- Added X\n- Fixed Y" \
  --webhook-url https://outlook.webhook.office.com/webhookb2/... \
  --extra-url https://example.com/api/webhooks/forge
```

| Flag | Description |
|------|-------------|
| `--version` | Release version string, e.g. `v1.2.3` (required) |
| `--tag` | Git tag (defaults to `--version` if omitted) |
| `--release-url` | URL to the GitHub release page |
| `--changelog` | Short changelog summary to include in the notification |
| `--webhook-url` | Teams webhook URL — overrides `notifications.teams.webhook_url` in config |
| `--extra-url` | Additional generic-JSON webhook URL(s) to notify (repeatable) |

Webhook URL resolution order for Teams notifications:
1. `--webhook-url` flag
2. `FORGE_NOTIFICATIONS_TEAMS_WEBHOOK_URL` environment variable
3. `notifications.teams.webhook_url` (or legacy `notifications.teams_webhook_url`) in `forge.yaml`

> **Note:** Config-based resolution (steps 2–3 above, plus the generic-webhook config paths below) only applies when `notifications.enabled: true` is set in `forge.yaml`. CLI flags and environment variables work regardless of `notifications.enabled`.

`forge notify release` sends to two categories of generic-JSON webhook targets, each with a **different payload schema**:

- **`notifications.webhooks[]`** entries with `events: [release]` and any `--extra-url` flags receive the uniform event payload:
  ```json
  { "event_type": "release", "bead_id": "", "anvil": "", "message": "...", "timestamp": "..." }
  ```

- **`notifications.release_webhook_urls`** (config), `FORGE_RELEASE_WEBHOOK_URL` (env var), and `--extra-url` flags receive the richer release-published payload:
  ```json
  { "source": "forge", "summary": "...", "event": "release_published", "detail": "...", "url": "...", "repo": "...", "version": "...", "tag": "..." }
  ```

Integrators should use `notifications.webhooks[]` for systems expecting the standard event schema, and `notifications.release_webhook_urls` / `--extra-url` for systems expecting the richer release payload.
