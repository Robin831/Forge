# The Forge

An autonomous AI orchestrator that coordinates multiple Claude Code agents working across FHI repositories. The Forge monitors beads (issues), spawns AI workers in isolated git worktrees, reviews their output, and manages the full lifecycle from implementation through PR creation.

## Naming

The Forge uses a blacksmith metaphor throughout:

| Component   | Role                                        |
| ----------- | ------------------------------------------- |
| **Hearth**  | Daemon process + TUI dashboard              |
| **Smith**   | Implementation worker (Claude Code session) |
| **Warden**  | Review agent (validates Smith output)       |
| **Temper**  | Build/lint/test verification                |
| **Bellows** | PR monitor (CI failures, review comments)   |
| **Schematic** | Pre-analysis worker (decomposes complex beads) |
| **Depcheck** | Periodic Go module dependency update scanner  |
| **Anvil**   | Repository workspace                        |
| **Heat**    | Work batch / session                        |

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
┌──────────────────────────────────────┐
│  TUI (Hearth)                        │
│  Queue    │         │ Live Activity  │
│  R.Merge  │ Workers │ Events         │
│  Attn     │         │                │
└──────────────────────────────────────┘
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
  max_ci_fix_attempts: 5       # CI fix cycles per PR (default 5)
  max_review_fix_attempts: 5   # Review fix cycles per PR (default 5)
  max_rebase_attempts: 3       # Conflict rebase attempts per PR (default 3)
  daily_cost_limit: 50.00      # USD per day; 0 = no limit
  bellows_interval: 2m         # PR monitor poll interval
  merge_strategy: squash       # squash | merge | rebase
  providers:
    - claude
    - gemini
  smith_providers:             # Optional: separate chain for Smith/Warden
    - claude/claude-opus-4-6
  schematic_enabled: false     # Pre-analysis for complex beads
  schematic_word_threshold: 100
  claude_flags:
    - --dangerously-skip-permissions
    - --max-turns
    - "50"
```

See [docs/configuration.md](docs/configuration.md) for the full reference.

### Auto-Dispatch Modes

| Mode | Description |
|------|-------------|
| `all` | (Default) Dispatch all ready beads found in the anvil. |
| `tagged` | Only dispatch beads where one of the bead's tags exactly matches `auto_dispatch_tag` (case-insensitive). |
| `priority` | Only dispatch beads with priority <= `auto_dispatch_min_priority`. |
| `off` | Never auto-dispatch; beads must be started manually via `forge queue run`. |

## Worker Pipeline

```
bd ready → Claim bead → Create worktree → [Schematic (optional pre-analysis)]
    → Smith (Claude) → Temper (build/test) → Warden (review)
    → PR creation → bd close → Bellows (monitor PR, CI fix, review fix, rebase)
```

Each step is tracked in SQLite state.db with full event logging.

### Dependency Scanning

The daemon includes a **depcheck** monitor that periodically runs `go list -m -u all` on Go anvils to detect outdated dependencies. Patch and minor updates produce auto-dispatch beads; major version bumps produce "needs attention" beads for manual review. Configure via `depcheck_interval` (default: weekly) and `depcheck_timeout` in `forge.yaml`. Set `depcheck_interval: 0` to disable.

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
│   ├── bellows/        # PR monitoring (CI fix, review fix, rebase)
│   ├── depcheck/       # Periodic Go dependency update checker
│   ├── schematic/      # Pre-analysis worker (decompose complex beads)
│   ├── hearth/         # Daemon and TUI dashboard
│   ├── state/          # SQLite state management
│   └── bd/             # Beads CLI integration
├── changelog.d/        # Changelog fragments (per-bead)
├── forge.yaml          # Configuration (user-created)
├── go.mod
├── go.sum
├── AGENTS.md
├── README.md
└── LICENSE
```

## License

MIT — see [LICENSE](LICENSE).
