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
  claude_flags:
    - --dangerously-skip-permissions
    - --max-turns
    - "50"
  providers:
    - claude
    - gemini/gemini-2.5-pro
    - gemini/gemini-2.5-flash
  rate_limit_backoff: 5m
  schematic_enabled: true
  schematic_word_threshold: 150

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
| `rate_limit_backoff` | duration | `5m` | | How long to wait before retrying when all providers are rate-limited. |
| `schematic_enabled` | bool | `false` | | Enable Schematic pre-worker globally for complex beads. |
| `schematic_word_threshold` | int | `100` | | Minimum word count in bead description to trigger Schematic analysis. |

Duration values use Go syntax: `30s`, `5m`, `1h30m`, etc.

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
| `FORGE_NOTIFICATIONS_ENABLED` | `notifications.enabled` |
| `FORGE_NOTIFICATIONS_TEAMS_WEBHOOK_URL` | `notifications.teams_webhook_url` |

Duration values from environment variables are parsed as Go duration strings (e.g., `"5m"`, `"30s"`).

Per-anvil configuration is best managed in the YAML file, as the flat environment variable namespace doesn't map cleanly to the nested `anvils` map.

## Validation Rules

The config is validated at load time. Errors are reported as a list:

- `max_total_smiths` must be >= 1
- `max_review_attempts` must be >= 1
- `poll_interval` must be >= 10s
- `smith_timeout` must be >= 1m
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
- `notifications.*` (webhook URL, enabled, events, etc.) are re-read and applied immediately
- In-flight workers are **not** interrupted

All other configuration changes (including `anvils.*`, `providers`, `rate_limit_backoff`, and any fields not listed above) **require a daemon restart** to take effect.
