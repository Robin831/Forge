# Configuration Reference

Forge loads configuration from YAML files in this resolution order:

1. `--config` flag (explicit path)
2. `./forge.yaml` (working directory)
3. `~/.forge/config.yaml` (user home)

If no file is found, built-in defaults are used. The daemon hot-reloads the config file on change via fsnotify — no restart required.

## Full Example

```yaml
anvils:
  heimdall:
    path: C:\source\fhigit\Heimdall
    max_smiths: 2
    auto_dispatch: all
    schematic_enabled: false

  metadata:
    path: C:\source\fhigit\Fhi.Metadata
    max_smiths: 3
    auto_dispatch: tagged
    auto_dispatch_tag: forge-auto

  legacy-repo:
    path: C:\source\fhigit\Legacy
    max_smiths: 1
    auto_dispatch: priority
    auto_dispatch_min_priority: 0

settings:
  poll_interval: 5m
  smith_timeout: 30m
  max_total_smiths: 4
  max_review_attempts: 2
  max_ci_fix_attempts: 5
  max_review_fix_attempts: 5
  max_rebase_attempts: 3
  merge_strategy: squash
  daily_cost_limit: 50.00
  bellows_interval: 2m
  stale_interval: 5m
  claude_flags:
    - --dangerously-skip-permissions
    - --max-turns
    - "50"
  providers:
    - claude
    - gemini/gemini-2.5-pro
    - gemini/gemini-2.5-flash
  smith_providers:
    - claude/claude-opus-4-6
  rate_limit_backoff: 5m
  schematic_enabled: true
  schematic_word_threshold: 150
  depcheck_interval: 168h
  depcheck_timeout: 5m
  vulncheck_enabled: true
  vulncheck_interval: 24h
  vulncheck_timeout: 10m
  auto_learn_rules: true
  crucible_enabled: true
  auto_merge_crucible_children: true

notifications:
  enabled: true
  teams_webhook_url: https://outlook.webhook.office.com/webhookb2/...
  events:
    - pr_created
    - bead_failed
    - daily_cost
```

## Anvils

Each key under `anvils` is the anvil name. The name is used in CLI output, logs, and state tracking.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | string | **required** | Filesystem path to the repository root. Must contain a `.beads/` directory. |
| `max_smiths` | int | 1 | Maximum concurrent workers for this anvil. Values <= 0 are treated as 1. Overall concurrency is still limited by `max_total_smiths`. |
| `auto_dispatch` | string | `"all"` | Dispatch mode — see below. |
| `auto_dispatch_tag` | string | | Required when `auto_dispatch: tagged`. Beads must have this tag (case-insensitive) to be dispatched. |
| `auto_dispatch_min_priority` | int | 0 | Required when `auto_dispatch: priority`. Only beads with priority <= this value are dispatched. Range: 0-4. |
| `schematic_enabled` | bool\|null | null (use global) | Per-anvil override for `settings.schematic_enabled`. When set, takes precedence over the global setting. |
| `golangci_lint` | bool\|null | null (auto-detect) | Per-anvil override for golangci-lint in Temper. When null, golangci-lint runs if the binary is found on PATH. Set to `false` to disable. |

### Auto-Dispatch Modes

| Mode | Description |
|------|-------------|
| `all` | Dispatch all ready beads found in the anvil (default). |
| `tagged` | Only dispatch beads where a tag exactly matches `auto_dispatch_tag`. |
| `priority` | Only dispatch beads with priority <= `auto_dispatch_min_priority`. |
| `off` | Never auto-dispatch. Beads must be started manually via `forge queue run`. |

## Settings

| Field | Type | Default | Min | Description |
|-------|------|---------|-----|-------------|
| `poll_interval` | duration | `5m` | `10s` | How often the poller checks for ready beads. |
| `smith_timeout` | duration | `30m` | `1m` | Maximum time a Smith worker can run before being killed. |
| `max_total_smiths` | int | `4` | `1` | Global limit on concurrent Smith workers across all anvils. |
| `max_review_attempts` | int | `2` | `1` | Maximum Smith-Warden review iterations per bead. |
| `claude_flags` | []string | `[]` | | Additional flags passed to the Claude CLI (or translated for other providers). |
| `providers` | []string | `["claude", "gemini"]` | | Ordered provider fallback chain. See [Providers](providers.md). |
| `smith_providers` | []string | `[]` (uses `providers`) | | Provider chain for Smith/Warden/Schematic only. Lets dispatch use a more capable model while lifecycle workers (cifix, reviewfix) use `providers`. Same syntax as `providers`. |
| `rate_limit_backoff` | duration | `5m` | | How long to wait before retrying when all providers are rate-limited. |
| `schematic_enabled` | bool | `false` | | Enable Schematic pre-worker globally for complex beads. |
| `schematic_word_threshold` | int | `100` | | Minimum word count in bead description to trigger Schematic analysis. |
| `bellows_interval` | duration | `2m` | `30s` | How often Bellows polls GitHub for PR status changes. |
| `daily_cost_limit` | float | `0` (no limit) | | Maximum estimated USD spend per calendar day. When exceeded, auto-dispatch pauses until the next day. |
| `max_ci_fix_attempts` | int | `5` | `1` | Maximum CI fix cycles per PR before marking as exhausted. |
| `max_review_fix_attempts` | int | `5` | `1` | Maximum review fix cycles per PR before marking as exhausted. |
| `max_rebase_attempts` | int | `3` | `1` | Maximum conflict rebase attempts per PR before marking as exhausted. |
| `merge_strategy` | string | `"squash"` | | How PRs are merged from Hearth TUI. Valid: `squash`, `merge`, `rebase`. |
| `stale_interval` | duration | `5m` | `30s` or `0` | How long a worker's log can go without modification before marking as stalled. `0` disables stale detection. |
| `depcheck_interval` | duration | `168h` | `1h` or `0` | How often the dependency checker runs `go list -m -u all` on Go anvils. `0` disables. |
| `depcheck_timeout` | duration | `5m` | | Maximum time for a single depcheck invocation per anvil. |
| `vulncheck_enabled` | bool | `true` | | Enable/disable vulnerability scanning entirely. When `false`, scheduled scanning and `forge scan` are disabled. |
| `vulncheck_interval` | duration | `24h` | `0` | How often `govulncheck` runs on registered Go anvils. `0` disables. |
| `vulncheck_timeout` | duration | `10m` | | Maximum time for a single govulncheck invocation per anvil. |
| `auto_learn_rules` | bool | `false` | | Automatically learn Warden review rules from Copilot comments when a PR is merged. Rules are saved to each anvil's `.forge/warden-rules.yaml`. |
| `crucible_enabled` | bool | `false` | | Enable Crucible auto-orchestration for parent beads with children. When a ready bead blocks other beads, the Crucible creates a feature branch, dispatches children in topological order, merges each child PR, then creates a final PR to main. |
| `auto_merge_crucible_children` | bool | `true` | | Auto-merge child PRs targeting a Crucible feature branch after the pipeline succeeds. Set to `false` to require manual merge of child PRs. |

Duration values use Go syntax: `30s`, `5m`, `1h30m`, `168h`, etc.

## Notifications

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable MS Teams webhook notifications. |
| `teams_webhook_url` | string | | Teams incoming webhook URL (HTTPS). |
| `events` | []string | `[]` (all) | Event filter. Empty means all events. |

### Supported Events

| Event | Description |
|-------|-------------|
| `pr_created` | A pull request was created. |
| `bead_failed` | A bead exhausted retries and needs human intervention. |
| `daily_cost` | Daily token usage and cost summary. |
| `worker_done` | A worker successfully completed its pipeline. |
| `bead_decomposed` | Schematic split a bead into sub-beads; the parent is now blocked. |

Notifications are sent as MS Teams Adaptive Cards with color-coded severity.

## Environment Variable Overrides

Environment variables with the `FORGE_` prefix override YAML values. Nested keys use underscores as separators.

| Variable | Overrides |
|----------|-----------|
| `FORGE_SETTINGS_POLL_INTERVAL` | `settings.poll_interval` |
| `FORGE_SETTINGS_SMITH_TIMEOUT` | `settings.smith_timeout` |
| `FORGE_SETTINGS_MAX_TOTAL_SMITHS` | `settings.max_total_smiths` |
| `FORGE_SETTINGS_MAX_REVIEW_ATTEMPTS` | `settings.max_review_attempts` |
| `FORGE_SETTINGS_RATE_LIMIT_BACKOFF` | `settings.rate_limit_backoff` |
| `FORGE_SETTINGS_SCHEMATIC_ENABLED` | `settings.schematic_enabled` |
| `FORGE_SETTINGS_SCHEMATIC_WORD_THRESHOLD` | `settings.schematic_word_threshold` |
| `FORGE_SETTINGS_BELLOWS_INTERVAL` | `settings.bellows_interval` |
| `FORGE_SETTINGS_DAILY_COST_LIMIT` | `settings.daily_cost_limit` |
| `FORGE_SETTINGS_MAX_CI_FIX_ATTEMPTS` | `settings.max_ci_fix_attempts` |
| `FORGE_SETTINGS_MAX_REVIEW_FIX_ATTEMPTS` | `settings.max_review_fix_attempts` |
| `FORGE_SETTINGS_MAX_REBASE_ATTEMPTS` | `settings.max_rebase_attempts` |
| `FORGE_SETTINGS_MERGE_STRATEGY` | `settings.merge_strategy` |
| `FORGE_SETTINGS_STALE_INTERVAL` | `settings.stale_interval` |
| `FORGE_SETTINGS_DEPCHECK_INTERVAL` | `settings.depcheck_interval` |
| `FORGE_SETTINGS_DEPCHECK_TIMEOUT` | `settings.depcheck_timeout` |
| `FORGE_SETTINGS_VULNCHECK_ENABLED` | `settings.vulncheck_enabled` |
| `FORGE_SETTINGS_VULNCHECK_INTERVAL` | `settings.vulncheck_interval` |
| `FORGE_SETTINGS_VULNCHECK_TIMEOUT` | `settings.vulncheck_timeout` |
| `FORGE_SETTINGS_AUTO_LEARN_RULES` | `settings.auto_learn_rules` |
| `FORGE_NOTIFICATIONS_ENABLED` | `notifications.enabled` |
| `FORGE_NOTIFICATIONS_TEAMS_WEBHOOK_URL` | `notifications.teams_webhook_url` |

Duration values from environment variables are parsed as Go duration strings (e.g., `"5m"`, `"30s"`).

Per-anvil configuration is best managed in the YAML file, as the flat environment variable namespace doesn't map cleanly to the nested `anvils` map.

## Validation Rules

The config is validated at load time. Errors are reported as a list:

- `max_total_smiths` must be >= 1
- `max_review_attempts` must be >= 1
- `max_ci_fix_attempts` must be >= 1
- `max_review_fix_attempts` must be >= 1
- `max_rebase_attempts` must be >= 1
- `poll_interval` must be >= 10s
- `smith_timeout` must be >= 1m
- `bellows_interval` must be >= 30s
- `daily_cost_limit` must be a non-negative finite number
- `stale_interval` must be >= 30s when enabled, or 0 to disable
- `depcheck_interval` must be >= 1h when enabled, or 0 to disable
- Each anvil `path` must be non-empty
- Each anvil `max_smiths` must be >= 0
- `auto_dispatch` must be one of: `all`, `tagged`, `priority`, `off`
- If `auto_dispatch: tagged`, then `auto_dispatch_tag` must be non-empty
- If `auto_dispatch: priority`, then `auto_dispatch_min_priority` must be 0-4

## Hot Reload

The daemon watches `forge.yaml` via fsnotify. When the file changes, **only a subset of settings are hot-reloaded**:

- `poll_interval` is re-read and the new value takes effect on the next cycle
- `smith_timeout` is re-read and used for newly started smiths
- `max_total_smiths` is re-read and applied to subsequent scheduling decisions
- `claude_flags` are re-read and used for newly started smiths
- `smith_providers` are re-read and used for newly dispatched beads
- `notifications.*` (webhook URL, enabled, events, etc.) are re-read and applied immediately
- In-flight workers are **not** interrupted

All other configuration changes (including `anvils.*`, `providers`, `rate_limit_backoff`, `daily_cost_limit`, `merge_strategy`, and scheduling fields not listed above) **require a daemon restart** to take effect.
