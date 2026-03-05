# The Forge

An autonomous AI orchestrator that coordinates multiple Claude Code agents working across FHI repositories. The Forge monitors beads (issues), spawns AI workers in isolated git worktrees, reviews their output, and manages the full lifecycle from implementation through PR creation.

## Naming

The Forge uses a blacksmith metaphor throughout:

| Component | Role |
|-----------|------|
| **Hearth** | Daemon process + TUI dashboard |
| **Smith** | Implementation worker (Claude Code session) |
| **Warden** | Review agent (validates Smith output) |
| **Temper** | Build/lint/test verification |
| **Bellows** | PR monitor (CI failures, review comments) |
| **Anvil** | Repository workspace |
| **Heat** | Work batch / session |

## Architecture

```
┌─────────────────────────────────────────────┐
│  Hearth (daemon)                            │
│  ┌─────────┐  ┌──────────┐  ┌───────────┐  │
│  │ Poller  │  │ WorkerPool│  │ Bellows   │  │
│  │(bd ready)│  │(Smiths)  │  │(PR watch) │  │
│  └─────────┘  └──────────┘  └───────────┘  │
│        │            │              │        │
│        ▼            ▼              ▼        │
│  ┌──────────────────────────────────────┐   │
│  │         SQLite state.db              │   │
│  └──────────────────────────────────────┘   │
│        │                                    │
│        ▼                                    │
│  ┌──────────────────────────────────────┐   │
│  │     Named Pipe / Unix Socket IPC     │   │
│  └──────────────────────────────────────┘   │
└─────────────────────────────────────────────┘
         │
         ▼
┌──────────────┐
│  TUI (Hearth)│  Three-panel Bubbletea display
│  Queue│Workers│Events
└──────────────┘
```

## Quick Start

```bash
# Build
go build -o forge ./cmd/forge

# Configure anvils (repositories to orchestrate)
forge anvil add heimdall C:\source\fhigit\Heimdall
forge anvil add metadata C:\source\fhigit\Fhi.Metadata

# Start the daemon
forge up

# Open TUI dashboard
forge hearth

# Check status
forge status
```

## Configuration

Create `forge.yaml` in the working directory or `~/.forge/config.yaml`:

```yaml
anvils:
  heimdall:
    path: C:\source\fhigit\Heimdall
    max_smiths: 2
    auto_dispatch: all  # all | tagged | priority | off
  metadata:
    path: C:\source\fhigit\Fhi.Metadata
    max_smiths: 2
    auto_dispatch: tagged
    auto_dispatch_tag: 'forge-auto'
  legacy-repo:
    path: C:\source\fhigit\Legacy
    auto_dispatch: priority
    auto_dispatch_min_priority: 1  # Only P0 and P1

settings:
  poll_interval: 5m
  smith_timeout: 30m
  max_total_smiths: 3
  max_review_attempts: 2
  claude_flags: "--dangerously-skip-permissions --max-turns 50"
```

### Auto-Dispatch Modes

| Mode | Description |
|------|-------------|
| `all` | (Default) Dispatch all ready beads found in the anvil. |
| `tagged` | Only dispatch beads where one of the bead's tags exactly matches `auto_dispatch_tag` (case-insensitive). |
| `priority` | Only dispatch beads with priority <= `auto_dispatch_min_priority`. |
| `off` | Never auto-dispatch; beads must be started manually via `forge queue run`. |

## Worker Pipeline

```
bd ready → Claim bead → Create worktree → Smith (Claude) → Temper (build/test)
    → Warden (review) → PR creation → bd close → Bellows (monitor PR)
```

Each step is tracked in SQLite state.db with full event logging.

## Requirements

- **Go 1.26+**
- **bd** (beads) — issue tracker
- **claude** — Claude Code CLI
- **gh** — GitHub CLI
- **git** — with worktree support

## Project Structure

```
Forge/
├── cmd/forge/          # CLI entry point
│   └── main.go
├── internal/
│   ├── config/         # Viper-based configuration
│   ├── anvil/          # Repository management
│   ├── smith/          # Worker spawning and lifecycle
│   ├── warden/         # Review agent
│   ├── temper/         # Build/test verification
│   ├── bellows/        # PR monitoring
│   ├── hearth/         # Daemon and TUI
│   ├── state/          # SQLite state management
│   └── bd/             # Beads CLI integration
├── forge.yaml          # Configuration (user-created)
├── go.mod
├── go.sum
├── AGENTS.md
├── README.md
└── LICENSE
```

## License

MIT — see [LICENSE](LICENSE).
