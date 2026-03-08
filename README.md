# The Forge

An autonomous AI orchestrator that coordinates multiple Claude Code agents working across FHI repositories. The Forge monitors beads (issues), spawns AI workers in isolated git worktrees, reviews their output, and manages the full lifecycle from implementation through PR creation.

## Naming

The Forge uses a blacksmith metaphor throughout:

| Component    | Role                                                    |
| ------------ | ------------------------------------------------------- |
| **Hearth**   | Daemon process + TUI dashboard                          |
| **Smith**    | Implementation worker (Claude Code session)             |
| **Warden**   | Review agent (validates Smith output, learns rules)     |
| **Temper**   | Build/lint/test verification (Go, .NET, Node)           |
| **Bellows**  | PR monitor (CI failures, review comments, merge conflicts) |
| **Schematic**| Pre-analysis worker (decomposes complex beads)          |
| **Crucible** | Epic orchestrator (parent-child beads on feature branches) |
| **Depcheck** | Multi-language dependency update scanner (Go, .NET, Node) |
| **Anvil**    | Repository workspace                                    |
| **Heat**     | Work batch / session                                    |

## Architecture

```
┌──────────────────────────────────────────────────────┐
│  Hearth (daemon)                                     │
│  ┌─────────┐  ┌──────────┐  ┌───────────┐           │
│  │ Poller  │  │ WorkerPool│  │ Bellows   │           │
│  │(bd ready)│  │(Smiths)  │  │(PR watch) │           │
│  └─────────┘  └──────────┘  └───────────┘           │
│  ┌──────────┐ ┌──────────┐  ┌───────────┐           │
│  │ Depcheck │ │ Crucible │  │ Watchdog  │           │
│  │(dep scan)│ │(epics)   │  │(stale det)│           │
│  └──────────┘ └──────────┘  └───────────┘           │
│        │            │              │                 │
│        ▼            ▼              ▼                 │
│  ┌──────────────────────────────────────┐            │
│  │    SQLite state.db  │  Cost Tracker  │            │
│  └──────────────────────────────────────┘            │
│        │                                             │
│        ▼                                             │
│  ┌──────────────────────────────────────┐            │
│  │     Named Pipe / Unix Socket IPC     │            │
│  └──────────────────────────────────────┘            │
└──────────────────────────────────────────────────────┘
         │
         ▼
┌──────────────────────────────────────┐
│  TUI (Hearth)                        │
│  Queue    │         │ Live Activity  │
│  Crucibles│ Workers │ Events         │
│  R.Merge  │         │                │
│  Attn     │         │                │
└──────────────────────────────────────┘
```

## Quick Start

```bash
# Build
go build -o forge ./cmd/forge

# Check installation health
forge doctor

# Configure anvils (repositories to orchestrate)
forge anvil add heimdall C:\source\fhigit\Heimdall
forge anvil add metadata C:\source\fhigit\Fhi.Metadata

# Start the daemon
forge up

# Open TUI dashboard
forge hearth

# Check status
forge status

# View work history and events
forge history
forge history events
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
  crucible_enabled: false      # Epic orchestration for parent-child beads
  depcheck_interval: 168h      # Dependency scan interval (0 to disable)
  vulncheck_enabled: true      # Vulnerability scanning with govulncheck
  vulncheck_interval: 24h     # Vuln scan interval (0 to disable)
  auto_learn_rules: false      # Learn Warden rules from Copilot review comments
  stale_interval: 5m           # Stale worker detection (0 to disable)
  claude_flags:
    - --dangerously-skip-permissions
    - --max-turns
    - "50"

notifications:
  enabled: false
  teams_webhook_url: https://outlook.webhook.office.com/webhookb2/...
  events:                      # Empty = all events
    - pr_created
    - bead_failed
    - daily_cost
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

Each step is tracked in SQLite state.db with full event logging and cost tracking.

### Crucible (Epic Orchestration)

When a ready bead has children (blocks other beads), the **Crucible** takes over:

```
Detect parent bead → Create feature branch (feature/<parent-id>)
    → Topological sort children → For each child: pipeline → PR → merge to feature branch
    → Final PR (feature branch → main) → Bellows monitors → Close parent on merge
```

Enable with `crucible_enabled: true` in `forge.yaml`.

### Dependency Scanning

The **depcheck** monitor periodically scans anvils for outdated dependencies across multiple ecosystems:
- **Go**: `go list -m -u all`
- **Node**: npm outdated detection (via `npm outdated --json`)
- **.NET**: NuGet package update detection

Patch and minor updates produce auto-dispatch beads; major version bumps produce "needs attention" beads for manual review. Configure via `depcheck_interval` (default: weekly) and `depcheck_timeout`. Set `depcheck_interval: 0` to disable.

### Vulnerability Scanning

The daemon runs `govulncheck` on Go anvils on a configurable schedule (default: daily). Discovered vulnerabilities automatically create prioritized beads. Run manually with `forge scan`. Configure via `vulncheck_interval` and `vulncheck_enabled`.

### Warden Rule Learning

The Warden can learn review rules from GitHub Copilot comments on merged PRs. Learned rules are stored per-anvil in `.forge/warden-rules.yaml` and applied during future reviews. Enable with `auto_learn_rules: true`, or manage manually:

```bash
forge warden learn --anvil heimdall    # Learn from recent PR comments
forge warden list --anvil heimdall     # List learned rules
forge warden forget <rule-id> --anvil heimdall  # Remove a rule
```

### Cost Tracking

Token usage and USD cost estimates are tracked per-bead and per-day. Set `daily_cost_limit` to automatically pause auto-dispatch when the daily budget is exceeded. View current costs via `forge status`.

### Notifications

MS Teams webhook notifications for key events (PR created, bead failed, daily cost summary, worker done, bead decomposed). Configure in the `notifications` section of `forge.yaml`.

## Requirements

- **Go 1.26+**
- **bd** (beads) — issue tracker
- **claude** — Claude Code CLI
- **gh** — GitHub CLI (authenticated)
- **git** — with worktree support

Run `forge doctor` to verify all dependencies are installed and configured correctly.

## Project Structure

```
Forge/
├── cmd/forge/            # CLI entry point (Cobra commands)
│   └── main.go
├── internal/
│   ├── autostart/        # Windows Task Scheduler integration
│   ├── bellows/          # PR monitoring (CI fix, review fix, rebase)
│   ├── changelog/        # Changelog fragment parsing & assembly
│   ├── cifix/            # CI failure fix worker
│   ├── config/           # Viper-based configuration
│   ├── cost/             # Token usage & USD cost tracking
│   ├── crucible/         # Parent-child bead orchestration (epic branches)
│   ├── daemon/           # Main background process, poll loop, IPC server
│   ├── depcheck/         # Multi-language dependency update scanner
│   ├── executil/         # Platform-specific process execution
│   ├── forge/            # Core Forge types and interfaces
│   ├── ghpr/             # GitHub PR creation & management
│   ├── hearth/           # Bubbletea TUI dashboard
│   ├── hotreload/        # fsnotify config watcher
│   ├── ipc/              # Named pipe / Unix socket protocol
│   ├── lifecycle/        # Worker lifecycle management
│   ├── notify/           # MS Teams webhook notifications
│   ├── pipeline/         # Smith → Temper → Warden orchestration
│   ├── poller/           # bd ready integration & Crucible detection
│   ├── prompt/           # Smith prompt builder
│   ├── provider/         # AI provider fallback chain
│   ├── rebase/           # Conflict rebase handling
│   ├── retry/            # Exponential backoff & retry logic
│   ├── reviewfix/        # Review comment fix worker
│   ├── schematic/        # Pre-analysis worker (decompose complex beads)
│   ├── shutdown/         # Graceful shutdown & orphan cleanup
│   ├── smith/            # Claude Code worker spawning & lifecycle
│   ├── state/            # SQLite state management (WAL mode)
│   ├── temper/           # Build/lint/test verification (Go, .NET, Node)
│   ├── vulncheck/        # Vulnerability scanning (govulncheck)
│   ├── warden/           # Code review agent & rule learning
│   ├── watchdog/         # Stale worker detection
│   ├── worker/           # Worker process abstraction
│   └── worktree/         # Git worktree creation/removal
├── docs/                 # Reference documentation
├── changelog.d/          # Changelog fragments (per-bead)
├── forge.yaml            # Configuration (user-created)
├── go.mod
├── go.sum
├── AGENTS.md
├── README.md
└── LICENSE
```

## License

MIT — see [LICENSE](LICENSE).
