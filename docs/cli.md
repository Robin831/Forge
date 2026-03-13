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

## Repository Management

### `forge anvil add <name> <path>`

Register a repository as an anvil. The path must contain a `.beads/` directory.

```bash
forge anvil add heimdall C:\source\fhigit\Heimdall
forge anvil add metadata C:\source\fhigit\Fhi.Metadata
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
forge queue clarify BD-42 --anvil heimdall --reason "Which auth library should be used?"
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |
| `-r, --reason` | Explanation for why clarification is needed (required) |

### `forge queue unclarify <id>`

Clear the clarification flag so a bead can proceed.

```bash
forge queue unclarify BD-42 --anvil heimdall
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge queue retry <id>`

Reset the dispatch circuit breaker for a bead so it can be retried.

```bash
forge queue retry BD-42 --anvil heimdall
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

## Scanning

### `forge scan`

Run `govulncheck` on registered Go anvils to check for known vulnerabilities.

```bash
forge scan                    # Scan all anvils
forge scan --anvil heimdall   # Scan a specific anvil
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Scan only this anvil (optional; default: all) |
| `--create-beads` | Automatically create beads for discovered vulnerabilities (default: true) |

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
forge warden learn --anvil heimdall
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge warden list`

List all learned review rules for an anvil.

```bash
forge warden list --anvil heimdall
```

| Flag | Description |
|------|-------------|
| `-a, --anvil` | Anvil name (required) |

### `forge warden forget`

Remove a learned rule by ID.

```bash
forge warden forget <rule-id> --anvil heimdall
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

Generic-JSON webhooks additionally receive payloads from `notifications.webhooks[]` entries (filtered by the `release` event) and from `notifications.release_webhook_urls` in config. The `FORGE_RELEASE_WEBHOOK_URL` environment variable adds one more generic target.
