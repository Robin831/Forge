# Ingots — Bundling Beads with PRs and Test Results

> GitHub Discussion: #222

## Problem

Today, a bead's lifecycle is scattered across three separate tables (`workers`, `prs`, `events`) with no unified record tying them together. Temper test results are logged as free-text events and discarded after the pipeline run. There's no way to query "which beads failed in temper this week?" or "what was the test output for this PR?" without parsing log files.

## Proposal

An **Ingot** is a compound record that bundles:

- One **Bead** (logical work unit from bd)
- One **PR** (GitHub pull request)
- One **Worker lifecycle** (Smith → Temper → Warden → PR creation)
- Structured **test/build results** (granular step results from Temper)

The ingot tracks the full bead→PR→merge journey in a single queryable record.

## Data Model

### New Tables (SQLite, `~/.forge/state.db`)

#### `ingots`

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER PK | Auto-increment |
| `bead_id` | TEXT NOT NULL | Bead identifier |
| `anvil` | TEXT NOT NULL | Anvil name |
| `pr_id` | INTEGER | FK to `prs.id`, NULL until PR created |
| `worker_id` | TEXT | Link to last worker |
| `status` | TEXT NOT NULL | Lifecycle status (see below) |
| `created_at` | TEXT NOT NULL | Timestamp |
| `updated_at` | TEXT NOT NULL | Timestamp |
| `temper_passed` | INTEGER | Boolean: all required steps passed |
| `temper_failed_step` | TEXT | Name of first failed step |
| `temper_duration_ms` | INTEGER | Total temper duration |
| `pr_number` | INTEGER | GitHub PR number |
| `pr_url` | TEXT | Full PR URL |
| `title` | TEXT | Bead title |
| `branch` | TEXT | Worktree branch name |

**Unique constraint**: `(bead_id, anvil)`

**Status values:**

```
init → smith → temper → warden → approved → pr_open → pr_merged
                  ↘         ↘         ↘                ↗
                   failed    failed    failed    stalled
```

#### `ingot_test_results`

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER PK | Auto-increment |
| `ingot_id` | INTEGER FK | References `ingots.id` |
| `step_index` | INTEGER | Execution order (0-based) |
| `step_name` | TEXT | "build", "lint", "test", "race", etc. |
| `command` | TEXT | Full command string |
| `exit_code` | INTEGER | Process exit code |
| `duration_ms` | INTEGER | Step duration |
| `passed` | INTEGER | Boolean |
| `optional` | INTEGER | Boolean: optional steps don't fail overall |
| `output_summary` | TEXT | First ~1000 chars or key errors |
| `full_output_path` | TEXT | Path to full output file (optional) |
| `recorded_at` | TEXT | Timestamp |

## Package Structure

### New: `internal/ingot/`

```
internal/ingot/
├── ingot.go    # Ingot, TestResult, Status types
└── db.go       # CRUD operations on ingot tables
```

**Key types:**

```go
type Status string // init|smith|temper|warden|approved|pr_open|pr_merged|failed|stalled

type Ingot struct {
    ID               int
    BeadID           string
    Anvil            string
    PRID             *int
    WorkerID         string
    Status           Status
    TemperPassed     bool
    TemperFailedStep string
    TemperDurationMs int
    PRNumber         *int
    PRURL            string
    Title            string
    Branch           string
    TestResults      []TestResult // eager-loaded
    CreatedAt        time.Time
    UpdatedAt        time.Time
}

type TestResult struct {
    ID             int
    IngotID        int
    StepIndex      int
    StepName       string
    Command        string
    ExitCode       int
    DurationMs     int
    Passed         bool
    Optional       bool
    OutputSummary  string
    FullOutputPath string
    RecordedAt     time.Time
}
```

**DB methods:**

- `InsertIngot(ingot *Ingot) error`
- `UpdateIngotStatus(beadID, anvil string, status Status) error`
- `UpdateIngotTemperResults(beadID, anvil string, passed bool, failedStep string, durationMs int) error`
- `UpdateIngotPR(beadID, anvil string, prNum int, prURL string, prID int) error`
- `GetIngot(beadID, anvil string) (*Ingot, error)`
- `GetIngotsByStatus(status Status, limit int) ([]Ingot, error)`
- `InsertTestResult(tr *TestResult) error`
- `GetTestResults(ingotID int) ([]TestResult, error)`

## Integration Points

### Pipeline (`internal/pipeline/pipeline.go`)

Update `Run()` to create and maintain ingot records at each stage transition:

1. **After worktree creation** → `InsertIngot(status: init)`
2. **Before Smith** → `UpdateIngotStatus(smith)`
3. **Before Temper** → `UpdateIngotStatus(temper)`
4. **After Temper** → Store each `StepResult` as `TestResult`, update temper summary fields
5. **Before Warden** → `UpdateIngotStatus(warden)`
6. **After Warden approves** → `UpdateIngotStatus(approved)`
7. **After PR creation** → `UpdateIngotPR(...)`, `UpdateIngotStatus(pr_open)`
8. **On failure** → `UpdateIngotStatus(failed)`

### Bellows (`internal/bellows/bellows.go`)

When a PR merges or is closed, update the linked ingot:

- PR merged → `UpdateIngotStatus(pr_merged)`
- PR closed without merge → `UpdateIngotStatus(failed)`

### State DB (`internal/state/db.go`)

Add migration to create the `ingots` and `ingot_test_results` tables. The existing `temper.Result` struct already has all the granular step data — no changes needed in temper itself.

## IPC & CLI

### New IPC commands

- `get_ingots` — list ingots with optional filters (anvil, status, limit)
- `get_ingot` — single ingot with eager-loaded test results

### New CLI subcommand: `forge ingots`

```bash
forge ingots list [--anvil <name>] [--status <status>]
forge ingots show <bead-id> [--anvil <name>]
```

Example output for `forge ingots show`:

```
Ingot: Forge-abc1 (myrepo)
Status:   pr_open (#42)
Branch:   forge-Forge-abc1

Temper Results:
  ✓ build            1.2s   exit=0
  ✓ lint (optional)  0.8s   exit=0
  ✓ test             45.3s  exit=0
  ✓ test -race       32.1s  exit=0

PR #42: https://github.com/org/repo/pull/42
CI: passing | Review: pending
```

### Hearth TUI (`internal/hearth/`)

Add ingot status counts to the existing dashboard. Consider a detail view when drilling into a specific worker/PR that shows the ingot's test results inline.

## Backwards Compatibility

- All new tables are additive; existing queries are unchanged
- Ingot creation is best-effort — pipeline works fine if ingot writes fail
- IPC responses add optional `omitempty` fields
- No breaking changes to existing worker or PR table schemas

## Implementation Order

1. **Schema + types**: `internal/ingot/` package with tables, types, CRUD
2. **Pipeline integration**: Create/update ingots at each stage
3. **Bellows integration**: Update ingot on PR merge/close
4. **CLI**: `forge ingots list|show`
5. **IPC + Hearth**: Dashboard integration

Each step is independently shippable and testable.
