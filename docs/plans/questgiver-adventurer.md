# QuestGiver & Adventurer — Automated E2E Test Scenarios

> GitHub Discussion: #223

## Problem

Forge can build, lint, and unit-test code (via Temper), but has no way to validate that the application actually works from a user's perspective. There's no mechanism for defining user-facing test scenarios ("as a user I want to log in and create an image") and automatically executing them against a running application.

## Proposal

Two new components:

- **QuestGiver** — Discovers and maintains test scenario documents ("quests") in each anvil. Runs on a schedule, like depcheck/vulncheck.
- **Adventurer** — Executes quests by driving a browser, then reports results. Creates beads for failures so Forge can auto-fix regressions.

## Architecture

These follow the **standalone monitor** pattern (like depcheck and vulncheck), not the pipeline-stage pattern. They run as background goroutines in the daemon on a configurable interval.

```
Daemon (background)
  └── QuestGiver Monitor
        ├── Scan anvils for quest documents
        ├── For each quest: Adventurer.Execute()
        ├── On failure: bd create (auto-bead)
        └── Log events to state DB
```

## Quest Document Format

Quests live in each anvil at `.forge/quests/` as YAML files:

```yaml
# .forge/quests/login-and-create.yaml
name: "Login and create image"
description: "Verify a user can log in and create a new image"
url: "http://localhost:3000"
tags: [smoke, auth, images]
steps:
  - action: navigate
    url: "{{.BaseURL}}/login"
  - action: fill
    selector: "#email"
    value: "test@example.com"
  - action: fill
    selector: "#password"
    value: "testpass123"
  - action: click
    selector: "button[type=submit]"
  - action: wait
    selector: ".dashboard"
    timeout: 10s
  - action: navigate
    url: "{{.BaseURL}}/images/new"
  - action: fill
    selector: "#title"
    value: "Test Image"
  - action: click
    selector: "#create-btn"
  - action: assert
    selector: ".success-message"
    contains: "Image created"
```

This keeps quests declarative, versionable, and readable by both humans and the Adventurer. Template variables (`{{.BaseURL}}`) are resolved from quest config.

## Package Structure

```
internal/
├── questgiver/
│   ├── questgiver.go   # Monitor: scan loop, discovery, orchestration
│   └── quest.go        # Quest document types and YAML parsing
├── adventurer/
│   └── adventurer.go   # Browser executor: runs quest steps, captures results
```

### QuestGiver Types

```go
package questgiver

type Quest struct {
    Name        string   `yaml:"name"`
    Description string   `yaml:"description"`
    URL         string   `yaml:"url"`
    Tags        []string `yaml:"tags"`
    Steps       []Step   `yaml:"steps"`
    FilePath    string   // populated at discovery time
}

type Step struct {
    Action   string        `yaml:"action"`   // navigate, fill, click, wait, assert, screenshot
    URL      string        `yaml:"url,omitempty"`
    Selector string        `yaml:"selector,omitempty"`
    Value    string        `yaml:"value,omitempty"`
    Contains string        `yaml:"contains,omitempty"`
    Timeout  time.Duration `yaml:"timeout,omitempty"`
}

type Monitor struct {
    db        *state.DB
    interval  time.Duration
    timeout   time.Duration
    anvils    map[string]string // name → path
    logger    *slog.Logger
}

func New(db *state.DB, interval, timeout time.Duration, anvils map[string]string) *Monitor
func (m *Monitor) Run(ctx context.Context) error
func (m *Monitor) DiscoverQuests(anvilPath string) ([]Quest, error)
```

### Adventurer Types

```go
package adventurer

type Result struct {
    QuestName    string
    Passed       bool
    Duration     time.Duration
    FailedStep   int           // -1 if all passed
    ErrorMessage string
    Screenshots  []string      // file paths to captured screenshots
    StepResults  []StepResult
}

type StepResult struct {
    Index    int
    Action   string
    Passed   bool
    Duration time.Duration
    Error    string
}

type Executor struct {
    timeout time.Duration
    logger  *slog.Logger
}

func New(timeout time.Duration, logger *slog.Logger) *Executor
func (e *Executor) Execute(ctx context.Context, quest *questgiver.Quest) *Result
```

### Browser Automation

The Adventurer needs a browser automation tool. Options:

1. **Playwright via CLI** (`npx playwright test`) — rich API, good error messages, widely used
2. **Rod (Go-native)** — Pure Go library for Chrome DevTools Protocol, no Node dependency
3. **chromedp (Go-native)** — Another Go CDP library, lower-level

**Recommendation: Rod** — keeps the Go-only dependency chain, no Node/npx required, good enough for declarative step execution. Falls back gracefully if Chrome isn't installed.

## Configuration

Add to `internal/config/config.go` `SettingsConfig`:

```go
QuestgiverEnabled  *bool         `mapstructure:"questgiver_enabled" yaml:"questgiver_enabled,omitempty"`
QuestgiverInterval time.Duration `mapstructure:"questgiver_interval" yaml:"questgiver_interval,omitempty"`
AdventurerTimeout  time.Duration `mapstructure:"adventurer_timeout" yaml:"adventurer_timeout,omitempty"`
```

In `forge.yaml`:

```yaml
settings:
  questgiver_enabled: true
  questgiver_interval: 24h    # how often to run quests
  adventurer_timeout: 5m      # max time per quest execution
```

Per-anvil override in anvil config:

```yaml
anvils:
  - name: my-app
    path: /path/to/app
    questgiver:
      enabled: true
      base_url: "http://localhost:3000"   # template variable for quests
      setup_cmd: "podman compose up -d"    # run before quest execution
      teardown_cmd: "podman compose down" # run after quest execution
```

## Daemon Integration

Wire into `internal/daemon/daemon.go` following the depcheck/vulncheck pattern:

```go
// In Daemon struct
questgiverMonitor *questgiver.Monitor

// In Run(), after depcheck/vulncheck setup
if d.config().Settings.IsQuestgiverEnabled() {
    anvils := filterQuestgiverAnvils(monitorAnvils, d.cfg.Load().Anvils)
    d.questgiverMonitor = questgiver.New(d.db,
        d.config().Settings.QuestgiverInterval,
        d.config().Settings.AdventurerTimeout,
        anvils)
    go d.questgiverMonitor.Run(ctx)
}
```

## Event Types

Add to `internal/state/db.go`:

```go
EventQuestgiverScanDone EventType = "questgiver_scan_done"
EventAdventurerStarted  EventType = "adventurer_started"
EventAdventurerPassed   EventType = "adventurer_passed"
EventAdventurerFailed   EventType = "adventurer_failed"
EventTestBeadCreated    EventType = "test_bead_created"
```

## Bead Creation on Failure

When a quest fails, QuestGiver creates a bead using `bd create` (same pattern as depcheck/vulncheck):

```go
func (m *Monitor) createFailureBead(ctx context.Context, anvilPath string, quest Quest, result *adventurer.Result) error {
    title := fmt.Sprintf("E2E failure: %s — step %d (%s)",
        quest.Name, result.FailedStep, result.ErrorMessage)

    description := fmt.Sprintf(
        "## E2E Test Failure\n\n"+
            "**Quest**: %s\n"+
            "**Failed Step**: #%d — %s\n"+
            "**Error**: %s\n\n"+
            "### Quest File\n`%s`\n\n"+
            "### Steps to Reproduce\nRun: `forge quest run %s`\n",
        quest.Name, result.FailedStep,
        quest.Steps[result.FailedStep].Action,
        result.ErrorMessage, quest.FilePath, quest.Name)

    cmd := exec.CommandContext(ctx, "bd", "create",
        "--title", title,
        "--description", description,
        "--type", "bug",
        "--priority=1")
    cmd.Dir = anvilPath
    return cmd.Run()
}
```

### Deduplication

Before creating a bead, check if one already exists for the same quest failure (same pattern as vulncheck):

```go
func (m *Monitor) failureBeadExists(ctx context.Context, anvilPath, questName string) bool {
    cmd := exec.CommandContext(ctx, "bd", "search", questName, "--json")
    cmd.Dir = anvilPath
    out, err := cmd.Output()
    if err != nil {
        return false
    }
    // Parse JSON, check for open beads matching this quest
    return strings.Contains(string(out), questName)
}
```

## CLI Commands

```bash
forge quest list [--anvil <name>]         # List discovered quests across anvils
forge quest run <quest-name> --anvil <name>  # Manually run a specific quest
forge quest results [--anvil <name>]      # Show recent quest results
```

## Data Flow

```
Timer tick (every 24h by default)
  │
  ├─ For each anvil with questgiver_enabled:
  │    ├─ Run setup_cmd (e.g. docker compose up)
  │    ├─ Discover quests from .forge/quests/*.yaml
  │    ├─ For each quest:
  │    │    ├─ adventurer.Execute(quest)
  │    │    ├─ Log event (passed/failed)
  │    │    └─ If failed AND no existing bead → bd create
  │    └─ Run teardown_cmd (e.g. docker compose down)
  │
  └─ Log questgiver_scan_done event
```

## Implementation Order

1. **Quest types + YAML parsing** (`internal/questgiver/quest.go`)
2. **Discovery logic** (`internal/questgiver/questgiver.go`) — scan `.forge/quests/`
3. **Adventurer executor** (`internal/adventurer/adventurer.go`) — Rod-based browser automation
4. **Bead creation** — failure → `bd create` with dedup
5. **Config + daemon wiring** — settings, monitor lifecycle
6. **CLI** — `forge quest list|run|results`

Each step is independently testable. Steps 1-2 need no browser dependency and can be unit-tested with fixture YAML files.

## Future Enhancements (Out of Scope)

- AI-generated quests from code analysis (Claude reads routes/handlers → suggests scenarios)
- Screenshot comparison (visual regression)
- Quest result history in state.db for trend tracking
- Parallel quest execution across anvils
