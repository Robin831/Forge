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
| **Crucible** | Epic orchestrator (opt-in via the `crucible` label on the parent; a child opts out with `independent`) |
| **Depcheck** | Multi-language dependency update scanner (Go, .NET, Node) |
| **Wicket**   | GitHub issue triage monitor — classifies issues and creates beads |
| **Quench**   | CI failure fix worker — spawns Smith with targeted fix prompt |
| **Burnish**  | Review comment fix worker — addresses PR review feedback |
| **Smelter**  | Batches pending warden rules into PRs                    |
| **QuestGiver** | E2E quest discovery and execution                      |
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
│  ┌──────────┐ ┌──────────┐  ┌───────────┐           │
│  │ Wicket  │ │ Smelter  │  │QuestGiver │           │
│  │(triage) │ │(rules PR)│  │(E2E tests)│           │
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
    smith:
      deny_patterns:
        files: ["*.env", "*.key"]       # Reject changes to sensitive files
        commands: ["rm -rf /*"]         # Block dangerous commands
    hooks:
      before_temper: 'npm ci'           # Run before each temper invocation
    temper:
      steps:                            # Custom build/test/lint steps
        - { name: build, command: npm, args: [run, build] }
        - { name: test,  command: npm, args: [run, test] }
    stage_providers:                    # Per-anvil provider overrides
      smith: [claude/claude-opus-4-6]

settings:
  poll_interval: 5m
  smith_timeout: 30m
  max_total_smiths: 3
  max_pipeline_iterations: 5   # Smith-Warden cycles in the pipeline loop (default 5)
  max_review_attempts: 2
  max_ci_fix_attempts: 5       # CI fix cycles per PR (default 5)
  max_review_fix_attempts: 5   # Review fix cycles per PR (default 5)
  max_rebase_attempts: 3       # Conflict rebase attempts per PR (default 3)
  max_lifecycle_workers: 2     # Concurrent quench/burnish/rebase/assay fix workers (default 2)
  daily_cost_limit: 50.00      # USD per day; 0 = no limit
  copilot_daily_request_limit: 300  # 300 for Pro, 1500 for Pro+; 0 = no limit
  bellows_interval: 2m         # PR monitor poll interval
  merge_strategy: squash       # squash | merge | rebase
  providers:
    - claude
    - gemini
  smith_providers:             # Deprecated: use stage_providers instead
    - claude/claude-opus-4-6
  stage_providers:             # Per-stage provider overrides
    smith: [claude/claude-opus-4-6]
    warden: [claude/claude-sonnet-4-6]
    schematic: [claude/claude-sonnet-4-6]
    cifix: [claude/claude-sonnet-4-6]
    reviewfix: [claude/claude-sonnet-4-6]
  schematic_enabled: false     # Pre-analysis for complex beads
  schematic_word_threshold: 100
  crucible_enabled: false      # Epic orchestration for parents labeled `crucible`
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
  teams:
    webhook_url: https://outlook.webhook.office.com/webhookb2/...
    events:                    # Empty = all events
      - pr_created
      - bead_failed
      - daily_cost
  webhooks:                    # Generic JSON webhook targets (optional)
    - name: my-dashboard
      url: https://example.com/api/webhooks/forge
      events: [pr_created, worker_done, release]
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
    → [hooks: before/after each stage] → Smith (Claude)
    → deny_patterns validation → Temper (build/test) → Warden (review)
    → PR creation → bd close → Bellows (monitor PR, CI fix, review fix, rebase)
```

Each step is tracked in SQLite state.db with full event logging and cost tracking.

### Crucible (Epic Orchestration)

Epic orchestration is **opt-in**: a parent bead must carry the `crucible` label
(or an `epic-branch:<name>` label, which also names the branch) before anything
epic-specific happens. Children of an unlabeled parent — including a parent typed
`epic` — dispatch as ordinary standalone beads: worktree from `main`, PR to
`main`, bd relations untouched. The unlabeled parent itself is closed
automatically once the last of its children closes, so a bead filed purely to
group work does not linger open behind finished children.

```bash
bd label add <parent-id> crucible
```

When a labeled parent has children, the **Crucible** takes over:

```
Detect opted-in parent → Create feature branch (feature/<parent-id>, or epic-branch:<name>)
    → Topological sort children → For each child: pipeline → PR → merge to feature branch
    → Final PR (feature branch → main) → Bellows monitors → Close parent on merge
```

Children of an orchestrating parent are withheld from the ordinary dispatch loop
so they are not run twice in one poll cycle.

A single child can opt back out with the `independent` label:

```bash
bd label add <child-id> independent
```

The Crucible never claims that child (nor its own subtree), it builds from
`main` and PRs to `main` like any standalone bead, and it is excluded from the
epic's completeness set — its work could never land on the feature branch, so an
open independent child does not hold up the epic's final PR.

Enable with `crucible_enabled: true` in `forge.yaml`. See
[docs/configuration.md](docs/configuration.md) for the full opt-in rules.

### Dependency Scanning

The **depcheck** monitor periodically scans anvils for outdated dependencies across multiple ecosystems:
- **Go**: `go list -m -u all`
- **Node**: npm/yarn outdated detection
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

### Pipeline Hooks

Shell commands can run before or after each pipeline stage (Schematic, Smith, Temper, Warden). "Before" hooks abort on non-zero exit; "after" hooks are best-effort. Hooks receive pipeline context via environment variables (`FORGE_BEAD_ID`, `FORGE_WORKTREE_PATH`, `FORGE_BRANCH`, etc.). Configure per-anvil under `hooks`.

### Smith Deny Patterns

Restrict what files Smith can modify and what commands it can execute. File patterns are matched against the git diff; command patterns are matched against bash commands in Smith's output. Violations reset the worktree and retry with feedback. Configure per-anvil under `smith.deny_patterns`.

### Custom Temper Commands

Override auto-detected build/test/lint with custom commands via the `temper` per-anvil config. Use the shorthand (`build`/`test`/`lint`) for simple cases, or `temper.steps` for ordered multi-step pipelines with per-step timeouts and required/optional control.

### Per-Stage Providers

Use `stage_providers` (global or per-anvil) to assign different AI providers to each pipeline stage: `smith`, `warden`, `schematic`, `cifix`, `reviewfix`. Fallback chain for `smith`, `warden`, and `schematic`: anvil `stage_providers` → global `stage_providers` → `smith_providers` (deprecated) → `providers`. For `cifix` and `reviewfix`: anvil `stage_providers` → global `stage_providers` → `providers`.

### Cost Tracking

Token usage and USD cost estimates are tracked per-bead and per-day. Set `daily_cost_limit` to automatically pause auto-dispatch when the daily budget is exceeded. View current costs via `forge status`.

AI PR reviews are also tracked per run, and aggregated by ISO week so a change in what a review costs is visible while it is happening rather than in a month-end total:

```bash
forge assay stats            # Run count, mean cost and mean duration per week,
                             # split by coverage outcome (complete vs partial)
```

The daemon writes the same summary to its log once a day, with a WARN when the current week's mean cost per run exceeds the trailing four weeks' by more than 1.5x.

For Assay specifically, `forge cost assay` reports spend split by whether a run was a PR's first review or a re-review, and by prompt-cache token class:

```bash
forge cost assay --since 2026-06-01 --until 2026-07-01
forge cost assay --format json --out before.json
```

See [docs/assay-cost-attribution.md](docs/assay-cost-attribution.md) for the methodology (what counts as a repeat run, why recorded and priced figures are never summed, and how rows predating cache instrumentation are reported).

`forge cost zero-findings` is the companion analysis: it isolates the runs that reported no findings and classifies each as a PR's first review, an nth review over an unchanged head commit, or an nth review over a head that moved — the split that says whether skipping a re-review could recover anything. It is read-only and gates nothing; see [docs/assay-zero-finding-analysis.md](docs/assay-zero-finding-analysis.md) for the methodology and the measured finding.

### Notifications

Forge supports two webhook notification styles:
- **MS Teams** — Rich Adaptive Cards for key events (PR created, bead failed, daily cost, worker done, bead decomposed). Configure under `notifications.teams`.
- **Generic webhooks** — Simple JSON payloads sent to any HTTP endpoint. Each target can filter events independently. Configure under `notifications.webhooks[]`.

See the `notifications` section of `forge.yaml` and [docs/configuration.md](docs/configuration.md) for the full reference.

## Requirements

- **Go 1.26+**
- **bd** (beads) 1.1.2+ — issue tracker. 1.1.2 is the floor because `bd show --json`
  only emits the `dependents` array when passed `--include-dependents`, and Forge reads
  that array to find a bead's children. `forge doctor` checks for the flag.
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
│   ├── adventurer/       # Headless browser quest executor
│   ├── anvilhealth/      # Wedged-anvil detection (beads DB mid-merge with conflicts)
│   ├── bellows/          # PR monitoring (CI fix, review fix, rebase)
│   ├── changelog/        # Changelog fragment parsing & assembly
│   ├── quench/           # CI failure fix worker (quench)
│   ├── config/           # Viper-based configuration
│   ├── cost/             # Token usage & USD cost tracking
│   ├── crucible/         # Parent-child bead orchestration (epic branches)
│   ├── daemon/           # Main background process, poll loop, IPC server
│   ├── depcheck/         # Multi-language dependency update scanner
│   ├── epic/             # Opt-in gate + branch name for epic orchestration
│   ├── executil/         # Platform-specific process execution
│   ├── forge/            # Core types and constants (version info)
│   ├── vcs/              # VCS provider interface & GitHub implementation
│   ├── hearth/           # Bubbletea TUI dashboard
│   ├── hooks/            # Pipeline hook execution (before/after each stage)
│   ├── hotreload/        # fsnotify config watcher
│   ├── ingot/            # Ingot data model & persistence (bead lifecycle)
│   ├── ipc/              # Named pipe / Unix socket protocol
│   ├── kiln/             # Preview environments: manifest, ports, supervision, health
│   ├── ledger/           # Interactive bead management TUI
│   ├── lifecycle/        # Worker lifecycle management
│   ├── notify/           # MS Teams webhook notifications
│   ├── pipeline/         # Smith → Temper → Warden orchestration
│   ├── poller/           # bd ready integration & Crucible detection
│   ├── prompt/           # Smith prompt builder
│   ├── provider/         # AI provider fallback chain
│   ├── questgiver/       # E2E quest discovery & execution
│   ├── rebase/           # Conflict rebase handling
│   ├── retry/            # Exponential backoff & retry logic
│   ├── burnish/          # Review comment fix worker (burnish)
│   ├── schematic/        # Pre-analysis worker (decompose complex beads)
│   ├── shutdown/         # Graceful shutdown & orphan cleanup
│   ├── smelter/          # Batches pending warden rules into PRs
│   ├── smith/            # Claude Code worker spawning & lifecycle
│   ├── state/            # SQLite state management (WAL mode)
│   ├── temper/           # Build/lint/test verification (Go, .NET, Node)
│   ├── vulncheck/        # Vulnerability scanning (govulncheck)
│   ├── warden/           # Code review agent & rule learning
│   ├── watchdog/         # Stale worker detection
│   ├── wicket/           # GitHub issue triage monitor
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

## Privacy

Forge runs entirely on your local machine and does not collect, transmit, or store any telemetry, analytics, or user data. All data stays in your local filesystem (`~/.forge/state.db`) and your git repositories. Network access is limited to operations you explicitly configure: GitHub API calls (via `gh` CLI), AI provider APIs (Claude, Gemini, Copilot), and any webhooks you set up.

## License

MIT — see [LICENSE](LICENSE).
