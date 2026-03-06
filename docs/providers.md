# Providers

Forge supports multiple AI providers as a fallback chain. When a provider hits rate limits, the next provider in the list is tried automatically.

## Configuration

```yaml
settings:
  providers:
    - claude
    - gemini/gemini-2.5-pro
    - gemini/gemini-2.5-flash
  rate_limit_backoff: 5m
```

Default: `["claude", "gemini"]` when unset.

## Provider String Syntax

Each entry in the `providers` list uses this format:

```
kind[:command][/model]
```

| Component | Description | Example |
|-----------|-------------|---------|
| `kind` | Provider type: `claude`, `gemini`, or `copilot` | `gemini` |
| `:command` | Optional custom binary path | `gemini:mybin` |
| `/model` | Optional model override | `gemini/gemini-2.5-pro` |

### Examples

```yaml
settings:
  providers:
    # Claude with default settings
    - claude

    # Gemini with default model
    - gemini

    # Gemini with specific model
    - gemini/gemini-2.5-pro

    # Gemini with custom binary and model
    - gemini:mybin/gemini-2.5-pro

    # GitHub Copilot CLI
    - copilot
```

## Provider Details

### Claude

- **Binary**: `claude` (or custom via `:command`)
- **Output format**: Stream JSON (newline-delimited JSON events)
- **Model**: Determined by the `claude` CLI (no `--model` flag override)
- **Flag translation**: All `claude_flags` are passed through directly

### Gemini

- **Binary**: `gemini` (or custom via `:command`)
- **Output format**: Stream JSON
- **Model**: Configurable via `/model` suffix (e.g., `gemini/gemini-2.5-pro`)
- **Flag translation**: Recognized Claude flags are translated; unsupported flags (e.g., `--max-turns`) are silently dropped

### Copilot

- **Binary**: `copilot` (or custom via `:command`)
- **Output format**: Plain text (not JSON)
- **Model**: Defaults to `claude-sonnet-4.6`; overridable via a `--model <name>` entry in `claude_flags`. The `/model` suffix in the provider string is **not** honored for Copilot.
- **Flag translation**: Unrecognized Claude flags are silently dropped
- **Note**: Not included in defaults — must be explicitly added to the providers list

## Fallback Chain

The provider chain works as follows:

1. Smith attempts work using the first provider in the list
2. If the provider returns a **rate limit error**, the next provider is tried
3. Rate limits are detected via:
   - Exit code 2 from the CLI
   - Error subtype containing `rate_limit` or `overloaded`
   - Stderr containing phrases like "rate limit", "quota exceeded", "too many requests"
4. If **all providers are rate-limited**:
   - The bead is released back to the queue (status reset to open)
   - The pipeline waits for `rate_limit_backoff` (default 5m) before the bead can be retried
   - The bead slot stays reserved during backoff to prevent immediate re-claiming

## Provider Quotas

Running `forge status` shows current quota information per provider:

- Requests remaining / limit
- Tokens remaining / limit
- Reset times for both

This helps monitor capacity across providers.

## Claude Flags

The `settings.claude_flags` list is passed to whichever provider is active:

```yaml
settings:
  claude_flags:
    - --dangerously-skip-permissions
    - --max-turns
    - "50"
```

Each provider handles these flags differently:
- **Claude**: Passed through as-is
- **Gemini**: Translates recognized flags (e.g., `--model`); drops unsupported ones
- **Copilot**: Drops unrecognized flags silently
