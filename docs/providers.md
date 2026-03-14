# Providers

Forge supports multiple AI providers as a fallback chain. When a provider hits rate limits, the next provider in the list is tried automatically.

## Configuration

```yaml
settings:
  providers:
    - claude
    - gemini/gemini-2.5-pro
    - gemini/gemini-2.5-flash
  smith_providers:
    - claude/claude-opus-4-6
  rate_limit_backoff: 5m
```

Default for `providers`: `["claude", "gemini"]` when unset.

### `smith_providers` vs `providers`

| Setting | Used by | Purpose |
|---------|---------|---------|
| `providers` | CI fix, review fix, rebase workers | General-purpose provider chain for lifecycle workers. |
| `smith_providers` | Smith, Warden, Schematic | Dispatch pipeline provider chain. Falls back to `providers` when empty. |

This split lets you run a more capable (and expensive) model for initial implementation while using a lighter model for automated fix cycles. For example:

```yaml
settings:
  providers:
    - claude                     # Lifecycle workers use default Claude
  smith_providers:
    - claude/claude-opus-4-6     # Smith gets Opus for complex implementation
```

## Provider String Syntax

Each entry in the `providers` list uses this format:

```
kind[:command_or_backend][/model]
```

| Component | Description | Example |
|-----------|-------------|---------|
| `kind` | Provider type: `claude`, `gemini`, `copilot`, or `openai` | `gemini` |
| `:command` | Optional custom binary path | `gemini:mybin` |
| `:backend` | Known backend name (e.g. `ollama`) — sets env vars instead of binary | `claude:ollama` |
| `/model` | Optional model override | `gemini/gemini-2.5-pro` |

When the `:` value matches a known backend name, environment variables are injected into the subprocess instead of overriding the binary. Currently supported backends:

| Backend | Provider | Environment Variables |
|---------|----------|----------------------|
| `ollama` | `claude` | `ANTHROPIC_BASE_URL=http://localhost:11434`, `ANTHROPIC_AUTH_TOKEN=ollama`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` |

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

    # Claude CLI targeting local Ollama instance
    - claude:ollama

    # Claude CLI targeting Ollama with specific model
    - claude:ollama/qwen2.5-coder:32b
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

### Ollama (Local Models)

Claude Code supports Ollama natively via the Anthropic Messages API compatibility layer. Instead of a separate provider kind, use the `claude:ollama` backend syntax to redirect the `claude` CLI to a local Ollama instance.

- **Binary**: `claude` (same CLI, different API endpoint)
- **Output format**: Stream JSON (same as standard Claude)
- **Model**: Set via `/model` suffix (e.g., `claude:ollama/qwen2.5-coder:32b`)
- **Prerequisites**: Ollama installed and running locally with a capable coding model
- **Hardware**: GPU with 16GB+ VRAM or Apple M-series with 32GB+ RAM recommended

```yaml
settings:
  providers:
    - claude                              # Try Anthropic API first
    - claude:ollama/qwen2.5-coder:32b     # Fall back to local Ollama
```

Since Ollama has no API rate limits (only hardware constraints), it works well as a fallback when the Anthropic API is rate-limited. However, local inference is typically slower, so placing it after cloud providers in the chain is recommended.

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
