# Configuration Reference

Forge loads configuration from YAML files in this resolution order:

1. `--config` flag (explicit path)
2. `./forge.yaml` (working directory)
3. `~/.forge/config.yaml` (user home)

If no file is found, built-in defaults are used. The daemon hot-reloads the config file on change via fsnotify — no restart required.

## Full Example

```yaml
anvils:
  my-api:
    path: /path/to/repos/my-api
    platform: github               # VCS platform: github, gitlab, gitea (implemented); bitbucket, azuredevops (recognised but not yet implemented). Default: github
    max_smiths: 2
    auto_dispatch: all
    auto_merge: true               # Automatically merge PRs when ready (default: false)
    schematic_enabled: false       # Override global schematic setting for this anvil
    golangci_lint: null            # null/omit = auto-detect (runs only if binary found on PATH); false = disable
    go_race_detection: false       # Per-anvil race detector override
    depcheck_enabled: true         # Set to false to skip depcheck for this anvil
    hooks:                         # Pipeline stage hooks (optional)
      after_smith: './scripts/post-smith.sh'
      before_temper: './scripts/pre-temper.sh'  # runs for every temper invocation (pipeline, burnish, quench)

  my-frontend:
    path: /path/to/repos/my-frontend
    platform: gitlab               # Uses glab CLI for VCS operations
    max_smiths: 3
    auto_dispatch: tagged
    auto_dispatch_tag: forge-auto
    temper:
      steps:
        - { name: install,   command: npm, args: [ci],             dir: web, timeout: 5m }
        - { name: lint,      command: npm, args: [run, lint],      dir: web }
        - { name: typecheck, command: npm, args: [run, typecheck], dir: web }
        - { name: build,     command: npm, args: [run, build],     dir: web }
        - { name: test,      command: npm, args: [run, test:run],  dir: web }

  legacy-repo:
    path: /path/to/repos/legacy
    max_smiths: 1
    auto_dispatch: priority
    auto_dispatch_min_priority: 0

settings:
  poll_interval: 5m
  smith_timeout: 30m
  max_total_smiths: 4
  max_pipeline_iterations: 5
  max_review_attempts: 2
  max_ci_fix_attempts: 5
  max_review_fix_attempts: 5
  max_same_head_review_fixes: 2     # Review fixes against ONE unchanged PR head before escalating
  max_rebase_attempts: 3
  max_lifecycle_workers: 2          # Concurrent quench/burnish/rebase/assay fix workers
  burnish_verify_timeout: 5m        # Verify (Temper) deadline within one review-fix attempt
  burnish_verify_retries: 1         # Extra verification runs after a timeout, before pushing unverified
  merge_strategy: squash
  empty_diff_action: attention      # Approved branch with no commits vs base: attention | close
  daily_cost_limit: 50.00
  per_worker_cost_estimate: 2.00    # In-flight spend reserved per active worker for the cost gate
  copilot_daily_request_limit: 300  # 300 for Pro, 1500 for Pro+
  bellows_interval: 2m
  stale_interval: 5m
  go_race_detection: false         # Enable Go race detector globally (-race flag in Temper)
  temper_step_timeout: 5m          # Default timeout for a Temper step (per-step timeout still overrides)
  temper_git_timeout: 30s          # Timeout for internal git calls during Temper (e.g. VerifyClean)
  worktree_git_timeout: 5m         # Timeout for checkout-heavy git calls when preparing a worktree
  bd_timeout: 5m                   # Timeout for every bd subprocess (ready/show/create/update/close/sql)
  temper_output_cap: 262144        # Max bytes of combined stdout+stderr kept per step (head+tail truncated)
  claude_flags:
    - --dangerously-skip-permissions
    - --max-turns
    - "50"
  providers:
    - copilot/claude-sonnet-4-6  # warden/schematic overrides only apply to copilot entries
    - claude
    - gemini/gemini-2.5-pro
    - gemini/gemini-2.5-flash
  smith_providers:                           # deprecated — use stage_providers instead
    - claude/claude-opus-4-6
  stage_providers:                           # per-stage provider overrides
    smith: [claude/claude-opus-4-6]
    warden: [claude/claude-sonnet-4-6]
    schematic: [claude/claude-sonnet-4-6]
    cifix: [claude/claude-sonnet-4-6]
    reviewfix: [claude/claude-sonnet-4-6]
  # Note: warden_model_override and schematic_model_override only affect Copilot
  # provider entries. They are ignored when stage_providers.warden/schematic is set.
  warden_model_override: claude-haiku-4-5    # 0.33x premium for review
  schematic_model_override: claude-haiku-4-5 # 0.33x premium for analysis
  copilot_skip_warden_small_diffs: false     # opt-in: skip Warden for small Copilot diffs
  copilot_batch_ci_fixes: false              # opt-in: batch CI failures into one fix request
  copilot_batch_review_fixes: false          # opt-in: batch review comments into one fix request
  warden_full_rereview: false                # false = focused re-review; true = full review every iteration
  rate_limit_backoff: 5m
  schematic_enabled: true
  schematic_word_threshold: 150
  depcheck_interval: 168h
  depcheck_timeout: 5m
  vulncheck_enabled: true
  vulncheck_interval: 24h
  vulncheck_timeout: 10m
  anvil_health_check: true                   # detect a beads DB left mid-merge (wedged anvil)
  log_retention_days: 30                     # delete preserved bead logs older than this; 0 disables
  auto_learn_rules: true
  smelter_enabled: true
  smelter_interval: 8h
  questgiver_enabled: false
  questgiver_interval: 24h
  adventurer_timeout: 5m            # Max time for a single quest execution
  preview_enabled: false            # Kiln preview environments (master gate)
  preview_max_concurrent: 2
  preview_idle_timeout: 30m
  preview_port_range: '24000-24999'
  preview_bind_host: 127.0.0.1
  preview_public_host: ''           # Hostname used in preview links; empty = bind host
  preview_proxy_base: ''            # DNS suffix for host-based preview routing; empty = off
  preview_proxy_auth: session       # gate proxied previews on a Hearth session; 'none' opts out
  crucible_enabled: true
  crucible_poll_interval: 3m
  auto_merge_crucible_children: true
  warden:                          # review-time rule filtering (see Warden Rule Filtering)
    max_rules_per_review: 30
    archive_after_days: 180
    dedup_threshold: 0.6

# AI pull-request review (top-level, sibling of settings). See the Assay section.
assay:
  enabled: false
  shadow_mode: true
  nit_cap: 5

notifications:
  enabled: true

  # MS Teams (Adaptive Card format)
  teams:
    webhook_url: https://outlook.webhook.office.com/webhookb2/...
    events: [pr_created, bead_failed, daily_cost]  # empty = all events

  # Generic JSON webhook targets (optional)
  webhooks:
    - name: my-dashboard
      url: https://example.com/api/webhooks/forge
      events: [pr_created, worker_done, release]  # empty = all events
    - name: slack
      url: https://hooks.slack.com/services/...
      events: [bead_failed]
```

## Anvils

Each key under `anvils` is the anvil name. The name is used in CLI output, logs, and state tracking.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | string | **required** | Filesystem path to the repository root. Must contain a `.beads/` directory. |
| `platform` | string | `"github"` | VCS hosting platform for this anvil. Implemented: `github`, `gitlab`, `gitea`. Recognised but not yet implemented: `bitbucket`, `azuredevops`. Determines which VCS provider handles PR operations. See [Platform Requirements](#platform-requirements) below. |
| `max_smiths` | int | 1 | Maximum concurrent workers for this anvil. Values <= 0 are treated as 1. Overall concurrency is still limited by `max_total_smiths`. |
| `auto_dispatch` | string | `"all"` | Dispatch mode — see below. |
| `auto_dispatch_tag` | string | | Required when `auto_dispatch: tagged`. Beads must have this tag (case-insensitive) to be dispatched. |
| `auto_dispatch_min_priority` | int | 0 | Required when `auto_dispatch: priority`. Only beads with priority <= this value are dispatched. Range: 0-4. |
| `schematic_enabled` | bool\|null | null (use global) | Per-anvil override for `settings.schematic_enabled`. When set, takes precedence over the global setting. |
| `golangci_lint` | bool\|null | null (auto-detect) | Per-anvil override for golangci-lint in Temper. When null, golangci-lint runs if the binary is found on PATH. Set to `false` to disable. |
| `go_race_detection` | bool\|null | null (use global) | Per-anvil override for Go race detection in Temper. When set, takes precedence over the global setting. |
| `temper` | object\|null | null (auto-detect) | Custom Temper commands for this anvil. When set, replaces auto-detected build/test/lint steps. See [Custom Temper Commands](#custom-temper-commands) below. |
| `depcheck_enabled` | bool\|null | null (enabled) | Per-anvil toggle for depcheck scanning. When null, depcheck runs as normal. Set to `false` to skip this anvil entirely. |
| `auto_merge` | bool | `false` | When enabled, PRs that reach the ready-to-merge state (CI passing, no conflicts, no unresolved threads, no pending reviews) are automatically merged using the configured `merge_strategy`. External PRs (`ext-*`) are never auto-merged. |
| `questgiver_enabled` | bool\|null | null (use global) | Per-anvil override for QuestGiver E2E quest scanning. When null, uses `settings.questgiver_enabled`. Set to `false` to opt this anvil out entirely. |
| `preview_enabled` | bool\|null | null (use global) | Per-anvil override for Kiln preview environments. When null, uses `settings.preview_enabled`. Set to `false` to opt this anvil out entirely. An anvil without a `.forge/preview.yaml` manifest offers no preview regardless. See [Preview Environments (Kiln)](#preview-environments-kiln). |
| `preview_auto` | string | `"off"` | When to start a preview for this anvil without being asked. `off` = only on request; `ready_to_merge` = start one when a PR transitions to ready-to-merge. Still bounded by `preview_max_concurrent` and `preview_idle_timeout`; note the memory trade-off in [Automatic previews](#automatic-previews-preview_auto). |
| `preview_quests` | bool | `false` | Opt this anvil into running its E2E quests against a running preview environment instead of its fixed quest URL. Requires previews to be enabled for the anvil (config validation rejects it otherwise); quests only run while the preview is healthy. See [Quests against previews (`preview_quests`)](#quests-against-previews-preview_quests). |
| `questgiver_setup_cmd` | string | | Shell command to run before executing quests for this anvil (e.g. `"podman compose up -d"`). Runs in the anvil's root directory. If the command fails, quest execution is aborted. Used by `forge quest run` and the QuestGiver monitor. |
| `questgiver_teardown_cmd` | string | | Shell command to run after executing quests for this anvil (e.g. `"podman compose down"`). Runs in the anvil's root directory. Always runs even if quests fail; teardown failures are logged as warnings rather than errors. |
| `wicket_enabled` | bool\|null | null (use global) | Per-anvil override for Wicket issue triage scanning. When null, uses `settings.wicket_enabled`. Set to `false` to opt this anvil out entirely. |
| `wicket_trusted_users` | []string | `[]` | GitHub logins whose issues are automatically dispatched without extra review for this anvil. |
| `wicket_auto_dispatch` | bool | `false` | When true, triaged beads for this anvil are auto-dispatched without waiting for manual queue approval. |
| `wicket_issue_labels` | []string | `[]` | GitHub label names an issue must carry for Wicket to consider it eligible. Empty = all issues are eligible (subject to `wicket_trigger_label`). |
| `wicket_repos` | []string | `[]` | `"owner/repo"` strings Wicket scans for this anvil. When empty, the anvil's primary repository is derived from its git remote. |
| `wicket_triage_prompt` | string | | Optional prompt suffix appended to the default Wicket triage system prompt, allowing project-specific context or constraints to be injected. |
| `wicket_ignore_users` | []string | `[]` | GitHub logins to skip entirely when triaging issues for this anvil. In addition to this list, a built-in set of well-known bot accounts (dependabot[bot], renovate[bot], github-actions[bot], etc.) is always ignored. Comparison is case-insensitive. |
| `smith` | object\|null | null | Smith configuration for this anvil. Currently supports `deny_patterns` for file and command restrictions. See [Smith Deny Patterns](#smith-deny-patterns) below. |
| `hooks` | object\|null | null | Shell commands to run before/after each pipeline stage. See [Pipeline Hooks](#pipeline-hooks) below. |
| `stage_providers` | map[string][]string | `{}` (use global) | Per-anvil override for `settings.stage_providers`. When set, takes precedence over the global stage providers for beads in this anvil. Same keys/format. See [Settings](#settings) and the Wicket [Per-Anvil Settings](#per-anvil-settings) table. |
| `assay` | object\|null | null (use global) | Per-anvil overlay for the top-level `assay` (AI PR review) config. Non-empty fields override the corresponding global values. See [Assay — AI Pull-Request Review](#assay--ai-pull-request-review) below. |

### Smith Deny Patterns

Detect and reject Smith runs that modify sensitive files or use dangerous commands. Deny patterns are enforced after each Smith iteration via pipeline validation (for example, diff validation and command/log inspection). When violations are detected, the worktree is reset, any forbidden file changes are discarded, and Smith retries with feedback about the violation. This does not provide a pre-execution safety guarantee for commands within that iteration; it detects the violation afterward and prevents the resulting worktree state from being accepted. If violations persist after all iterations, the pipeline fails.

```yaml
anvils:
  myrepo:
    path: /path/to/repo
    smith:
      deny_patterns:
        files:
          - "*.env"
          - ".forge/*"
          - "*.key"
          - "*.pem"
        commands:
          - "rm -rf /"
          - "git push --force*"
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `smith.deny_patterns.files` | []string | `[]` | Glob patterns matched against file paths in the Smith diff. Patterns without `/` also match the basename (e.g. `*.env` matches `config/.env`). Patterns with `/` match path suffixes (e.g. `.forge/*` matches `src/.forge/config.yaml`). Uses `path.Match` syntax (platform-independent, always forward-slash). |
| `smith.deny_patterns.commands` | []string | `[]` | Glob patterns matched against bash commands executed by Smith. Extracted from stream-json tool_use events. `*` matches any sequence of characters including `/`, so patterns like `rm -rf /*` and `git push --force*` work as expected. |

### Pipeline Hooks

Configure shell commands that run before or after each pipeline stage. Hooks fire before/after every temper invocation — initial pipeline, burnish review-fix, and quench CI-fix — so setup commands like `npm ci` or container startup work uniformly across all temper runs. Hooks are executed via a platform-appropriate shell (`sh -c` on Unix, `cmd /c` on Windows) with the worktree as the working directory and receive pipeline context as environment variables. Each hook has a 60-second timeout.

```yaml
anvils:
  myrepo:
    path: /path/to/repo
    hooks:
      before_smith: './scripts/pre-smith.sh'
      after_smith: './scripts/post-smith.sh'
      before_temper: './scripts/pre-temper.sh'
      after_temper: './scripts/notify.sh'
      before_warden: './scripts/pre-warden.sh'
      after_warden: './scripts/post-warden.sh'
      before_schematic: 'echo "Starting schematic"'
      after_schematic: 'echo "Schematic done"'
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `hooks.before_schematic` | string | | Runs before the Schematic pre-analysis stage. |
| `hooks.after_schematic` | string | | Runs after the Schematic pre-analysis stage. |
| `hooks.before_smith` | string | | Runs before each Smith iteration. |
| `hooks.after_smith` | string | | Runs after each Smith iteration. |
| `hooks.before_temper` | string | | Runs before each Temper verification. |
| `hooks.after_temper` | string | | Runs after each Temper verification. |
| `hooks.before_warden` | string | | Runs before each Warden review. |
| `hooks.after_warden` | string | | Runs after each Warden review. |

**Hook behavior:**
- **Before hooks** abort the current stage on non-zero exit. Use them to enforce preconditions (e.g., required files, environment validation).
- **After hooks** are best-effort — failures are logged but do not abort the stage. Use them for notifications, metrics, or cleanup.
- Hooks run in the worktree directory, so relative paths resolve against the worktree.
- Temper hooks (`before_temper`, `after_temper`) fire for every temper invocation: initial pipeline, burnish (review-fix), and quench (CI-fix). This ensures setup commands (e.g. `npm ci`) apply uniformly.

**Environment variables** available to all hooks:

| Variable | Description |
|----------|-------------|
| `FORGE_BEAD_ID` | Unique bead identifier |
| `FORGE_WORKTREE_PATH` | Absolute path to the worker's worktree |
| `FORGE_BRANCH` | Git branch name |
| `FORGE_ANVIL_NAME` | Repository label |
| `FORGE_ANVIL_PATH` | Absolute path to the main repository |
| `FORGE_STAGE` | Current pipeline stage (`schematic`, `smith`, `temper`, `warden`) |
| `FORGE_ITERATION` | Current Smith-Warden cycle number (1-based) for `smith`/`warden` stages; `schematic` hooks currently receive `1` even though they run outside the Smith-Warden loop |

**Use cases:** custom linters, Slack/Teams notifications, prompt context injection, metrics collection, pre-flight checks.

**Node.js example** — install dependencies before every temper run so that build/lint/test steps always have fresh `node_modules`:

```yaml
anvils:
  my-node-app:
    path: /home/user/source/my-node-app
    hooks:
      before_temper: 'cd web && npm ci'
```

### Auto-Dispatch Modes

| Mode | Description |
|------|-------------|
| `all` | Dispatch all ready beads found in the anvil (default). |
| `tagged` | Only dispatch beads where a tag exactly matches `auto_dispatch_tag`. |
| `priority` | Only dispatch beads with priority <= `auto_dispatch_min_priority`. |
| `off` | Never auto-dispatch. Beads must be started manually via `forge queue run`. |

### Platform Requirements

Each platform requires specific CLI tools or environment variables to be available:

| Platform | Value | Requirement |
|----------|-------|-------------|
| GitHub | `github` | `gh` CLI, authenticated via `gh auth login`. |
| GitLab | `gitlab` | `glab` CLI, authenticated via `glab auth login`. |
| Gitea / Forgejo | `gitea` | `GITEA_TOKEN` (or `FORGEJO_TOKEN`) environment variable set to a personal access token with repo scope. `GITEA_URL` (or `FORGEJO_URL`) should be set to the instance base URL (e.g. `https://gitea.example.com`); if omitted, the URL is inferred from the git remote. |

Omitting `platform` or setting it to an empty string defaults to `github`. Existing configurations that don't specify a platform require no changes.

### Custom Temper Commands

By default, Temper auto-detects the project type (Go, .NET, Node) and runs appropriate build/test/lint steps. The `temper` object lets you override these with custom commands, enabling support for Python, Rust, or repos with non-standard build tooling.

Use the shorthand when your project has a straightforward build/test/lint shape. For more complex pipelines, use the `steps` list described in [Custom Temper Steps](#custom-temper-steps-advanced) below.

#### Shorthand (build/test/lint)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `temper.build` | string | | Custom build command (e.g. `"make build"`, `"cargo build"`). Replaces the auto-detected build step. |
| `temper.test` | string | | Custom test command (e.g. `"make test"`, `"pytest"`). Replaces the auto-detected test step. |
| `temper.lint` | string | | Custom lint command (e.g. `"make lint"`, `"ruff check ."`). Runs as an optional step by default (failure warns but doesn't block). Set `lint_required: true` to make lint failures fail the temper run. |
| `temper.lint_required` | bool | `false` | When `true`, failures in `temper.lint` fail the temper run instead of warning. Default matches legacy behavior where lint is an advisory-only step. |

When any `temper.build`, `temper.test`, or `temper.lint` command is set, **all** auto-detected steps are replaced — only the explicitly configured commands run. Omit a command field to skip that step entirely. Setting `temper.lint_required` by itself does not replace auto-detected steps; it only changes how a configured `temper.lint` command is handled.

Commands are split on whitespace into executable + arguments. For commands requiring shell features (pipes, redirections, command chaining, or inline environment variables), use a wrapper script committed in the repo, or invoke a shell explicitly (for example, `sh -c 'FOO=bar pytest -q | tee test.log'`). Per-anvil `.forge/temper.yaml` does not currently support custom build/test/lint commands.

**Example:**

```yaml
anvils:
  my-python-repo:
    path: /home/user/source/my-python-repo
    temper:
      build: "pip install -e ."
      test: "pytest"
      lint: "ruff check ."
  my-rust-repo:
    path: /home/user/source/my-rust-repo
    temper:
      build: "cargo build"
      test: "cargo test"
      lint: "cargo clippy"
  custom-makefile:
    path: /home/user/source/custom-project
    temper:
      build: "make build"
      test: "make test"
      lint: "make lint"
  strict-node-app:
    path: /home/user/source/strict-node-app
    temper:
      build: "npm run build"
      test: "npm run test:run"
      lint: "npm run lint"
      lint_required: true   # make lint failures fail the temper run, matching CI
```

#### Custom Temper Steps (advanced)

For pipelines that need more than three steps, per-step working directories, or per-step timeout/required control, use `temper.steps`. Steps run in the order listed; a required step failure stops the run.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | **required** | Step identifier; appears in logs, summaries, and failure events. Must be non-empty and unique within the list. |
| `command` | string | **required** | Executable to run (not shell-interpreted). For shell features, wrap in a checked-in script. |
| `args` | `[]string` | `[]` | Arguments to the command. Use list form; avoid embedding in `command`. |
| `dir` | string | worktree root | Working directory for the step. Relative paths resolve against the worktree; absolute paths used as-is. |
| `timeout` | duration | `5m` | Per-step timeout. Same parsing as elsewhere in Forge config (`5m`, `30s`, `1h`). |
| `required` | bool | `true` | When `true`, failure fails the whole temper run. When `false`, failure is reported as a warning. |
| `paths` | `[]string` | `[]` (always run) | Glob patterns (doublestar syntax, e.g. `client/**`, `**/*.go`). When set, the step is skipped if no changed files in the PR diff match any pattern. When empty or omitted, the step always runs. |
| `verify_clean` | `[]string` | `[]` (no check) | Pathspecs (relative to the worktree, e.g. `web/dist`) that must remain clean after the step runs. When the step succeeds but `git status --porcelain -- <pathspecs>` reports changes, the step is converted to a failure. Use this to enforce that committed build artifacts (e.g. an embedded frontend bundle) match a fresh build of the source. |
| `verify_no_conflict_markers` | `[]string` | `[]` (no check) | Pathspecs (relative to the worktree) that must not contain git merge-conflict markers (`<<<<<<<`, `=======`, `>>>>>>>` at line start). When set on a step with **no** `command`, Temper performs a cheap scan-only check that runs unconditionally (no `paths` gating) — complementary to `verify_clean`, which depends on a rebuild and can miss markers committed directly into build output. A scan-only step (empty `command`) is valid only when this is set. |
| `tolerate_host_crash` | bool | `false` | When `true`, re-classifies a non-zero exit from this step as a pass **only if** the output shows a completed, all-passed .NET test summary **and** an explicit test-host crash/abort marker. Exists for .NET test hosts that occasionally OOM/crash at teardown after every test has already passed, producing false Temper failures. A real test failure (`Failed: N>0`) or a build error (no crash marker) still fails. |

**Precedence:** If `temper.steps` is set and non-empty, it takes precedence over `temper.build`/`test`/`lint`. A warning is logged if both are present. If neither `steps` nor the shorthand fields are set, auto-detection applies as usual.

Both `temper.steps` and the shorthand form can be used on different anvils in the same config file.

If your steps need one-shot setup (e.g. `npm ci`) that should persist across all temper invocations (initial pipeline, burnish, quench), consider using a `before_temper` hook instead of a step. See [Pipeline Hooks](#pipeline-hooks).

##### Examples

**Node.js matching CI exactly:**

```yaml
anvils:
  my-node-app:
    path: /home/user/source/my-node-app
    temper:
      steps:
        - name: install
          command: npm
          args: [ci]
          dir: web
          timeout: 5m
        - name: lint
          command: npm
          args: [run, lint]
          dir: web
        - name: typecheck
          command: npm
          args: [run, typecheck]
          dir: web
        - name: build
          command: npm
          args: [run, build]
          dir: web
        - name: test
          command: npm
          args: [run, test:run]
          dir: web
```

**Go + Node mixed monorepo:**

```yaml
anvils:
  hytte:
    path: /home/user/source/Hytte
    temper:
      steps:
        - name: go-build
          command: go
          args: [build, ./...]
          paths: ["**/*.go", "go.mod", "go.sum"]
        - name: go-vet
          command: go
          args: [vet, ./...]
          paths: ["**/*.go"]
        - name: go-test
          command: go
          args: [test, -short, ./...]
          paths: ["**/*.go", "go.mod", "go.sum"]
        - name: npm-install
          command: npm
          args: [ci]
          dir: web
          timeout: 5m
          paths: ["web/**"]
        - name: npm-lint
          command: npm
          args: [run, lint]
          dir: web
          paths: ["web/**"]
        - name: npm-build
          command: npm
          args: [run, build]
          dir: web
          paths: ["web/**"]
```

When a PR only changes Go files, the npm steps are automatically skipped (and vice versa). Steps without `paths` always run.

**.NET with analyzers and format check:**

```yaml
anvils:
  my-dotnet-app:
    path: C:\src\my-dotnet-app
    temper:
      steps:
        - name: restore
          command: dotnet
          args: [restore]
        - name: format
          command: dotnet
          args: [format, --verify-no-changes, --no-restore]
        - name: build
          command: dotnet
          args: [build, --configuration, Release, --no-restore]
          timeout: 10m
        - name: test
          command: dotnet
          args: [test, --configuration, Release, --no-build, --logger, "trx;LogFileName=test.trx"]
          timeout: 15m
```

**Python with advisory step:**

```yaml
anvils:
  my-python-lib:
    path: /home/user/source/my-python-lib
    temper:
      steps:
        - name: install
          command: pip
          args: [install, -e, ".[dev]"]
        - name: ruff
          command: ruff
          args: [check, .]
        - name: mypy
          command: mypy
          args: [src]
          required: false   # advisory — warn but don't block
        - name: test
          command: pytest
          args: [-q]
```

## Settings

| Field | Type | Default | Min | Description |
|-------|------|---------|-----|-------------|
| `poll_interval` | duration | `5m` | `10s` | How often the poller checks for ready beads. |
| `smith_timeout` | duration | `30m` | `1m` | Maximum time a Smith worker can run before being killed. |
| `max_total_smiths` | int | `4` | `1` | Global limit on concurrent Smith workers across all anvils. |
| `max_pipeline_iterations` | int | `5` | `1` | Maximum Smith-Warden cycles in the initial pipeline loop before declaring failure. Controls how many times Smith can revise its implementation based on Temper or Warden feedback during a single bead run. |
| `max_review_attempts` | int | `2` | `1` | Maximum review-fix cycles Bellows will attempt on a PR when reviewers request changes after the PR is created. |
| `claude_flags` | []string | `[]` | | Additional flags passed to the Claude CLI (or translated for other providers). |
| `providers` | []string | `["claude", "gemini"]` | | Ordered provider fallback chain. See [Providers](providers.md). |
| `smith_providers` | []string | `[]` (uses `providers`) | | **Deprecated** — use `stage_providers` instead. Provider chain for Smith/Warden/Schematic only. Still honoured as fallback when the corresponding `stage_providers` key is not set. Same syntax as `providers`. |
| `stage_providers` | map[string][]string | `{}` | | Per-stage provider overrides. Keys: `smith`, `warden`, `schematic`, `cifix`, `reviewfix`. Each value is a provider chain in the same syntax as `providers`. Fallback: `stage_providers[stage]` → `smith_providers` (smith/warden/schematic) → `providers` → defaults. |
| `warden_model_override` | string | `""` | | When set, overrides the model used by Copilot provider entries for the Warden review stage only. **Ignored when `stage_providers.warden` is set.** Non-Copilot providers are unaffected. E.g. `claude-haiku-4-5` (0.33× premium) reduces review cost compared to `claude-sonnet-4-6` (1×). |
| `schematic_model_override` | string | `""` | | When set, overrides the model used by Copilot provider entries for the Schematic pre-analysis stage only. **Ignored when `stage_providers.schematic` is set.** Non-Copilot providers are unaffected. |
| `copilot_skip_warden_small_diffs` | bool | `false` | | When true, auto-approves small low-risk diffs (≤100 lines, docs/tests/config or ≤2 files, no security-sensitive paths, P3+) without running Warden when the primary provider is Copilot. Saves one premium request per skipped review. |
| `copilot_batch_ci_fixes` | bool | `false` | | When true and the primary provider is Copilot, batches all CI failures into a single Smith invocation instead of the default per-attempt loop. Saves premium requests when a PR has multiple failing checks. |
| `copilot_batch_review_fixes` | bool | `false` | | When true and the primary provider is Copilot, batches all review comments into a single Smith invocation instead of the default per-attempt loop. Saves premium requests when a PR has multiple review comments. |
| `warden_full_rereview` | bool | `false` | | When true, the Warden performs a full independent review on every iteration. When false (default), re-review iterations only check whether previously raised issues were addressed, preventing the whack-a-mole pattern. |
| `copilot_combined_smith_warden` | bool | `false` | | When true and the primary provider is Copilot, embeds Warden review criteria into the Smith prompt so Smith self-reviews its own diff. A real Warden still runs for P0-P1 beads, when the self-review flags concerns, or via random sampling. Saves 1+ premium requests per bead. |
| `copilot_warden_sample_rate` | float | `0.1` | | Probability (0.0–1.0) that a real Warden review is spawned even when Smith's self-review approves. Only used when `copilot_combined_smith_warden` is true. Set to 1.0 to always run real Warden (useful for validating self-review quality). |
| `warden` | object | (see below) | | Review-time filtering of learned Warden rules and Smelter archival thresholds. See [Warden Rule Filtering](#warden-rule-filtering) below. |
| `rate_limit_backoff` | duration | `5m` | | How long to wait before retrying when all providers are rate-limited. |
| `schematic_enabled` | bool | `false` | | Enable Schematic pre-worker globally for complex beads. |
| `schematic_word_threshold` | int | `100` | | Minimum word count in bead description to trigger Schematic analysis. |
| `bellows_interval` | duration | `2m` | `30s` | How often Bellows polls GitHub for PR status changes. A PR carrying the per-PR `bellows_detached` flag is skipped before the first emit on every one of those polls — see [Bellows Per-PR Flags](#bellows-per-pr-flags-managed-manually-assigned-detached). |
| `daily_cost_limit` | float | `0` (no limit) | | Maximum estimated USD spend per calendar day. Auto-dispatch pauses once the **projected** total — recorded spend plus the estimated in-flight spend of currently active workers — reaches the limit, and the gate is re-checked before **each** dispatch. This accounting prevents N concurrent workers from overshooting the limit by roughly N × per-bead cost. See `per_worker_cost_estimate`. |
| `per_worker_cost_estimate` | float | `2.00` | `0` (use default) | Floor (USD) used to estimate a single active worker's in-flight (not-yet-recorded) spend when projecting against `daily_cost_limit`. The daemon maintains a rolling average of recorded per-bead cost and uses `max(rolling average, this floor)`, so the reservation is never zero before any cost data exists. Only relevant when `daily_cost_limit > 0`. Lifecycle/bellows fix workers (quench/burnish/rebase/assay) also reserve this estimate so their spend counts against the gate; those workers are themselves **not** blocked by the gate (they fix already-open PRs), but their in-flight spend causes the gate to back off new Smith dispatch. `0` or unset falls back to the default of `2.00`. |
| `copilot_daily_request_limit` | int | `0` (no limit) | | Maximum weighted Copilot premium requests per calendar day (e.g. 300 for Pro, 1500 for Pro+). When the limit is reached or exceeded, the Copilot provider is skipped in the fallback chain. Displayed as a progress indicator in the Hearth Usage panel. |
| `pricing` | map | built-in defaults | | Per-model USD rates (per 1M tokens) used to **estimate** cost for providers that do not self-report it (Copilot, Gemini, OpenAI/Codex). Claude self-reports `total_cost_usd` and is unaffected. Each entry overrides the built-in default for that model key; unlisted models keep their defaults. Hot-reloadable. See [Pricing Tables](#pricing-tables) below. |
| `copilot_premium_multipliers` | map | built-in defaults | | Per-model Copilot premium-request multipliers (e.g. `claude-opus-4.6: 3`). Each entry overrides the built-in default for that model; unlisted models keep their defaults (and unknown models default to `1.0`). Hot-reloadable. See [Pricing Tables](#pricing-tables) below. |
| `max_ci_fix_attempts` | int | `5` | `1` | Maximum CI fix cycles per PR before marking as exhausted. |
| `max_review_fix_attempts` | int | `5` | `1` | Maximum review fix cycles per PR before marking as exhausted. |
| `max_same_head_review_fixes` | int | `2` | `0` (use default) | Maximum review fix cycles dispatched against **one unchanged PR head**. `max_review_fix_attempts` bounds the PR's whole life and never resets, so it cannot tell a PR that is progressing (each round pushes a new head) from one rebuilding the identical diff every Bellows cycle — the second burns a full Smith run per cycle for nothing. Exceeding this raises a Needs Attention entry naming the PR, head SHA and attempt count instead of dispatching again; the counter resets as soon as the head moves, and `forge queue retry` clears it. A manual review fix (Hearth's "fix comments") is never circuit-broken. `0` or unset falls back to `2` — the breaker can be widened but not disabled. |
| `burnish_verify_timeout` | duration | `5m` | `30s` (when set) | Maximum time allowed for the post-Smith Temper (verification) step within a single Burnish (review-fix) attempt. The push and thread-resolution steps that follow are not covered by this deadline. On timeout the Burnish worker logs a WARN and records the stable reason `warden_timeout` in the event log (`review_fix_failed`) — then re-runs verification up to `burnish_verify_retries` times and, if it still does not complete, pushes the fix marked **unverified** rather than discarding it (see `burnish_verify_retries`). The timeout cannot be disabled — omitting the field (or setting it to `0`) falls back to the `5m` default; a non-zero value must be at least `30s`. |
| `burnish_verify_retries` | int | `1` | | Extra verification runs a Burnish attempt gets after the first one exceeds `burnish_verify_timeout`. A timeout is usually a wedged test process rather than a genuinely slow suite, so one clean re-run resolves most of them. When every run times out the fix is **not** discarded: the commit is pushed marked unverified (Burnish verification is advisory — the fix lands on a PR that humans, Copilot and Assay review anyway), the run is recorded as `review_fix_unverified_push`, and a Needs Attention entry is raised. Only if that push also fails is the worktree preserved instead of removed, with the dangling SHA named (`review_fix_work_preserved`). Omitting the field (or `0`) uses the default; a negative value falls straight through to the unverified push. |
| `max_rebase_attempts` | int | `3` | `1` | Maximum conflict rebase attempts per PR before marking as exhausted. |
| `max_lifecycle_workers` | int | `2` | `0` (use default) | Global cap on concurrent lifecycle/bellows fix workers (quench/cifix, burnish/reviewfix, rebase, assay) across all PRs and anvils. Each fix worker spawns its own Claude session and is **not** counted against `max_total_smiths`, so this independent ceiling prevents a burst of stuck PRs from fanning out unbounded Claude sessions and OOM-crashing the host. `0` or unset falls back to the default of `2`. |
| `merge_strategy` | string | `"squash"` | | How PRs are merged from Hearth TUI. Valid: `squash`, `merge`, `rebase`. |
| `empty_diff_action` | string | `"attention"` | | What to do when a run is approved but its branch has **no commits** against the base — the work already landed on the base branch (e.g. a sibling PR shipped it first). The pipeline skips PR creation (it would fail with `No commits between <base> and <branch>`) and records a `smith_empty_result` event; the outcome never schedules a retry or counts against the dispatch circuit breaker, because a re-run reproduces the identical empty branch. Valid: `attention` (raise a Needs Attention entry and leave the bead open for the operator) and `close` (close the bead with a note). Unrecognised values log a warning and fall back to `attention`. |
| `stale_interval` | duration | `5m` | `30s` or `0` | How long a worker's log can go without modification before marking as stalled. `0` disables stale detection. |
| `go_race_detection` | bool | `false` | | Enable the `-race` flag for Go tests in Temper globally. Per-anvil `go_race_detection` overrides this. |
| `temper_step_timeout` | duration | `5m` | | Default timeout applied to a Temper verification step whose own per-step `timeout` is unset. A per-step timeout still overrides this. Raise it for long-but-legitimate test suites so they finish instead of being killed and reported as a phantom failure (timeouts are retried once without Smith, then escalated). |
| `temper_git_timeout` | duration | `30s` | | Timeout for internal git invocations made during Temper verification (e.g. the `VerifyClean` status check). |
| `worktree_git_timeout` | duration | `5m` | `30s` (when set) | Timeout for **checkout-heavy** git invocations made while preparing a worker worktree: `worktree add`/`remove`, `fetch`, `pull`, `push`, `checkout`, `reset`, `clean`, `submodule`. Cheap metadata commands (`rev-parse`, `show-ref`, `branch`, `config`, `worktree prune`) keep their own tight `60s` bound and are unaffected. Accepts a Go duration string (`5m`, `600s`). Raised from the old hardcoded `60s` because a cold full-tree checkout of a large anvil under memory/disk pressure (dolt plus an active Smith, swap in use) legitimately takes longer, and the deadline **SIGKILLs** git — which wasted the first attempt of most beads and burned retry budget. On timeout the error now names the command and the elapsed time instead of reporting a bare `signal: killed`. Omitting the field (or setting it to `0`) falls back to the `5m` default; a non-zero value must be at least `30s`. |
| `bd_timeout` | duration | `5m` | `30s` (when set) | Timeout applied to every `bd` subprocess Forge spawns — `bd ready` in the poll loop, plus `show`/`list`/`create`/`update`/`close`/`sql` from the pipeline, Bellows, Wicket, Depcheck, Schematic, the Ledger TUI and the web GUI. bd talks to a Dolt backend that may be remote, so one write can legitimately take tens of seconds; the deadline **SIGKILLs** bd, which used to surface as a bare `signal: killed` with no hint that a deadline had fired or which bd command it was. On timeout the error now names the command and the elapsed time. A caller with a tighter budget of its own (e.g. the 30s wedged-anvil probe) still wins, and that shorter budget is what the message reports. Omitting the field (or setting it to `0`) falls back to the `5m` default; a non-zero value must be at least `30s`. |
| `temper_output_cap` | int (bytes) | `262144` | | Maximum bytes of combined stdout+stderr retained per Temper step. Output beyond the cap is head+tail truncated with an elision marker, bounding both memory and the warden/fix prompt that embeds the output. |
| `depcheck_interval` | duration | `168h` | `1h` or `0` | How often the dependency checker scans anvils for outdated dependencies (Go, .NET, Node). `0` disables. |
| `depcheck_timeout` | duration | `5m` | | Maximum time for a single depcheck invocation per anvil. |
| `vulncheck_enabled` | bool | `true` | | Enable/disable vulnerability scanning entirely. When `false`, scheduled scanning and `forge scan` are disabled. |
| `vulncheck_interval` | duration | `24h` | `0` | How often `govulncheck` runs on registered Go anvils. `0` disables. |
| `vulncheck_timeout` | duration | `10m` | | Maximum time for a single govulncheck invocation per anvil. |
| `anvil_health_check` | bool | `true` | | Detect a "wedged" anvil: one whose beads (Dolt) working set is left mid-merge with unresolved conflicts, so every `bd` write against it is rolled back. One `dolt_conflicts` query per anvil per **full** poll. A wedged anvil is surfaced in Needs Attention (naming the conflicted tables, the conflict count and the branch divergence), logged at WARN, and skipped for dispatch until the conflicts clear — at which point the entry clears automatically. First detection (and recovery) also fires the `anvil_wedged` / `anvil_recovered` generic webhook events. When `false`, neither the check nor the dispatch gate runs, and any flag the check had raised is released on the next full poll so a disabled check cannot strand an anvil in Needs Attention. |
| `log_retention_days` | int | `30` | `0` | How many days a preserved bead-log directory under `~/.forge/logs/<beadID>/` is kept after its newest file. A daily sweep removes older directories (skipping any bead with a running worker) and clears the affected `workers.log_path` rows. `0` disables the sweep. Independent of `daemon.log` rotation. |
| `log_sweep_interval` | duration | `24h` | `0` | How often the preserved bead-log retention sweep runs. `0` disables scheduled sweeps (the `log_retention_days` setting still governs per-pass behavior). Hot-reloadable via config file change; takes effect on daemon restart. |
| `auto_learn_rules` | bool | `false` | | Automatically learn Warden review rules from Copilot comments when a PR is merged. Rules are saved to each anvil's `.forge/warden-rules.yaml`. |
| `smelter_enabled` | bool | `true` | | Enable/disable the Smelter background process. When `false`, scheduled smelter runs are disabled. |
| `smelter_interval` | duration | `8h` | `1h` or `0` | How often the Smelter runs its background processing. `0` disables scheduled runs. The Smelter skips the startup run if it already flushed within this interval, so daemon restarts don't produce redundant PRs. For low-volume setups where warden rules accumulate slowly, `48h` or `72h` is a reasonable value. |
| `crucible_enabled` | bool | `false` | | Enable Crucible auto-orchestration for **opted-in** parent beads with children (see [Epic Orchestration Is Opt-In](#epic-orchestration-is-opt-in)). When a parent carrying the `crucible` label blocks other beads, the Crucible creates its feature branch, dispatches children in topological order, merges each child PR, then creates a final PR to main. A parent that carries the label while this setting is `false` is not dispatched: it is escalated to Needs Attention and its children are held back, since they are already routed to a branch nothing would create. |
| `crucible_poll_interval` | duration | `3m` | `30s` or `0` | Interval for the slow unfiltered poll that rebuilds the Crucible parent-child (Blocks) graph. The fast path polls with a label filter every `poll_interval`; the slow path runs every `crucible_poll_interval` to discover parent-child relationships. `0` disables two-tier polling (all polls are unfiltered). |
| `auto_merge_crucible_children` | bool | `true` | | Auto-merge child PRs targeting a Crucible feature branch after the pipeline succeeds. Set to `false` to require manual merge of child PRs. |
| `forge_id` | string | `""` (hostname) | | Per-instance identifier embedded in the forge-managed marker on every PR Forge creates (`<!-- forge-managed: <id> -->`). When multiple Forge instances target the same anvil, this ID ensures each instance only manages the PRs it created. When empty, `os.Hostname()` is used; falls back to `"default"`. Set this explicitly in environments where the hostname is not stable (e.g. ephemeral pods). |
| `bus_enabled` | bool | `false` | | Enable the in-process event Bus that fans logged events out to real-time SSE/IPC consumers. Disabled by default for safe rollout: when off, no Bus is constructed and consumers fall back to legacy polling (re-reading events via `EventsSince`). Also settable at daemon startup with `--enable-bus`. |
| `bus_buffer_size` | int | `256` | `1024` | Per-subscriber channel buffer for the event Bus. Bounds how many events a slow consumer can fall behind before the Bus drops the oldest and delivers a gap marker prompting a re-sync. Only relevant when `bus_enabled` is true; a value `<= 0` falls back to `256`. Also settable at daemon startup with `--bus-buffer-size`. |
| `sse_poll_fallback` | bool | `false` | `true` | **Deprecated (removal planned next release).** Force the `/api/activity/stream` SSE endpoint back onto the legacy 2s polling loop even when `bus_enabled` is true. A one-release safety valve: if the bus-based replay-then-live activity stream misbehaves, set this to `true` to revert just that endpoint to polling without disabling the Bus for other consumers. Hot-reloadable — takes effect on the next SSE connect. |
| `questgiver_enabled` | bool | `false` | | Enable the QuestGiver E2E quest monitor globally. When false, no quest scanning occurs. |
| `questgiver_interval` | duration | `24h` | `0` | How often the QuestGiver polls anvils for quests. `0` disables. |
| `adventurer_timeout` | duration | `5m` | | Maximum time allowed for a single quest execution by the headless-browser Adventurer (used by `forge quest run` and the QuestGiver monitor). Must not be negative. |
| `preview_enabled` | bool | `false` | | Master gate for Kiln preview environments. When false, no preview can be started regardless of per-anvil settings or the presence of a manifest. See [Preview Environments (Kiln)](#preview-environments-kiln). |
| `preview_max_concurrent` | int | `2` | `0` (use default) | Maximum number of previews running at once. Each preview costs real memory (database, API, dev server), hence the low default. A start that hits the cap is **rejected**, not queued; the error names the limit and the beads holding the slots. See [When the cap is reached](#when-the-cap-is-reached-preview_max_concurrent-preview_evict_lru). |
| `preview_evict_lru` | bool | `false` | | What happens when a preview is requested while `preview_max_concurrent` is already reached. `false` rejects the request. `true` stops the least recently used preview to make room. Automatic previews (`preview_auto`) never evict — they are skipped when the box is full either way. |
| `preview_idle_timeout` | duration | `30m` | `1m` or `0` | How long a preview may go unused before it is torn down. `0` disables the idle reaper, leaving previews running until stopped explicitly or until their PR merges/closes. |
| `preview_port_range` | string | `"24000-24999"` | | Inclusive `"min-max"` TCP port range preview service ports are allocated from. Both ends must be within 1024-65535 and min must be less than max. **The range must stay below the host's ephemeral port floor** — see [Choosing a preview port range](#choosing-a-preview-port-range-preview_port_range). A range that overlaps it logs a WARN at daemon start but is never rejected. |
| `preview_bind_host` | string | `"127.0.0.1"` | | Address preview services bind to. The loopback default keeps previews reachable only from the Forge box; `0.0.0.0` exposes them to a LAN or VPN. Kiln probes health here, but a service listens where its own command line says — reference this value as `{{.BindHost}}` in the manifest so the two cannot drift. Ports bound here bypass the Hearth login entirely (only the hostname-based proxy is gated — see `preview_proxy_auth`), so widen this only on a trusted network. |
| `preview_public_host` | string | `""` (bind host) | | Hostname used when displaying preview links (e.g. the box's LAN or WireGuard name). Empty falls back to `preview_bind_host`. Ignored once `preview_proxy_base` is set: previews are then addressed by their own hostname, not by this one plus a port. |
| `preview_proxy_base` | string | `""` (off) | | DNS suffix previews are addressed under when Forge fronts them by hostname instead of by port: a bead's preview answers on `<label>.<base>` and one of its services on `<label>--<service>.<base>`. A bare DNS name — a scheme, port, path or leading dot is rejected at load time. Requires the Hearth web GUI (`FORGE_WEB_ENABLED`) plus wildcard DNS and a wildcard certificate ([Deploying the preview proxy](preview-proxy-deployment.md)). See [Host-based preview routing](#host-based-preview-routing-preview_proxy_base). Empty (the default) switches host-based routing off and leaves preview links on `host:port`. |
| `preview_proxy_auth` | string | `"session"` | `session`, `none` | Whether a request arriving on a preview hostname has to prove it comes from a signed-in Hearth operator. `session` (the default, and what an empty value means) gates every proxied request; `none` serves previews to anyone who can resolve the name. Only applies when `preview_proxy_base` is set — it never gates the raw `preview_bind_host` ports. See [Auth gating for proxied previews](#auth-gating-for-proxied-previews-preview_proxy_auth) and [Security posture: what a preview URL exposes](#security-posture-what-a-preview-url-exposes). |
| `wicket_enabled` | bool | `false` | | Enable the Wicket GitHub issue triage monitor globally. When false, no issue scanning occurs. |
| `wicket_interval` | duration | `15m` | `1m` or `0` | How often Wicket polls repositories for new issues. `0` disables. |
| `wicket_provider` | string | `""` (uses `providers`) | | AI provider used for triage decisions. When empty, the global `providers` chain is used. |
| `wicket_batch_size` | int | `20` | `1` | Maximum number of issues processed per scan cycle per repository. |
| `wicket_processed_label` | string | `"forge-wicket-processed"` | | GitHub label applied to issues that have already been triaged. |
| `wicket_needs_human_label` | string | `"forge-needs-human"` | | GitHub label applied to issues flagged for human review. |
| `wicket_bead_created_label` | string | `"forge-bead-created"` | | GitHub label applied to issues for which a bead was created. |
| `wicket_trigger_label` | string | `""` | | When non-empty, only issues carrying this label are processed (pull model). When empty (default), Wicket processes all issues without a trigger-label gate (push model). |
| `forgechat.turn_timeout` | duration | `5m` | (cap `15m`) | Wall-clock budget for a single Beads-Forge AI turn (drafter, grilling, plan, emit). When the budget is exceeded, the runner returns a sentinel chat message instead of the truncated streamed preamble and logs a warning. Values above `15m` are clamped on load. |
| `forgechat.turn_expiry` | duration | `30m` | | How long a completed Beads-Forge turn is retained in the in-memory TurnStore before garbage collection removes it. Once dropped, a reconnecting SSE client receives a graceful `turn_expired` event (and refetches the canonical messages) instead of a 404. A non-positive value disables expiry. |
| `forgechat.turn_retention_cap` | int | `1000` | | Maximum number of Beads-Forge turns retained in the TurnStore. When exceeded, the oldest completed turns are evicted first (in-flight turns are never evicted). A negative value disables the cap. |

Duration values use Go syntax: `30s`, `5m`, `1h30m`, `168h`, etc.

### Bellows Per-PR Flags: managed, manually assigned, detached

These three are **not** config settings — they are per-PR columns on the `prs`
table in `~/.forge/state.db`, set by IPC `pr_action` verbs rather than from
`forge.yaml`. They are listed here because they decide, per PR, how much of the
Bellows behaviour configured above actually applies to it.

| Flag | Default | Question it answers |
|------|---------|---------------------|
| `bellows_managed` | `1`, but `0` for externally-created (`ext-*`) PRs | Does Bellows run lifecycle workers for this PR **at all**? |
| `bellows_manually_assigned` | `0` | Did an operator pin the answer above, so the reconcile loop must not rewrite it? |
| `bellows_detached` | `0` | Is this PR managed but **muted**? |

The first two are one pair, set together by the `assign_bellows` /
`unassign_bellows` PR actions: assigning an `ext-*` PR sets both, so the
reconcile loop's defensive "external PRs are not ours" correction leaves the
user-pinned row alone.

`bellows_detached` is a separate axis, set by the `detach_bellows` /
`reattach_bellows` PR actions, and reconcile never touches it. That separation
is the point: un-assigning says "this PR was never ours", detaching says "this
PR is ours and I want the automatic loop off it for now", and folding the second
into the first would have made every mute a claim about ownership that the
reconcile loop is entitled to overwrite.

**What detaching stops.** Bellows returns before its first emit for a detached
PR, which also closes the steady-state re-emit branches ("still failing CI",
"still conflicting", "still unresolved threads") that fire on *unchanged* state.
No CI fix (quench), review fix (burnish), rebase or Assay review is dispatched —
including an action queued before the detach and drained after it — and the
quench/burnish/rebase workers already in flight for that PR are killed, so a
session mid-run cannot push one more commit to the branch that was just taken
off the loop.

**What detaching does not stop.** The PR's mergeability and terminal state are
still refreshed every poll: a muted PR is unwatched, not unknown, so the PR
panel keeps telling the truth and a detached PR that merges is recorded as
merged instead of being polled forever. What is *not* re-emitted while detached
is the post-merge bookkeeping: Bellows returns before the branch that dispatches
the bead close and worktree cleanup, so a PR that merges under a mute has the
merge recorded but leaves its bead open — and an open bead blocks its
dependents. Those two actions are the ones the detach gate deliberately still
*permits*, so a close already queued before the detach, a manual `forge queue`
close, or simply re-attaching gets the bookkeeping done; a long mute over a
merged PR wants one of the three. Manual verbs in general are unaffected:
`forge assay run`, `forge queue run` and the dashboard's own fix buttons still
run a single pass by hand on a muted PR. Detach means "stop automatic work", not
"brick the PR".

**Resuming.** Re-attaching clears the flag and drops Bellows' cached snapshot
for the PR, so failing CI, conflicts and unresolved threads that outlived the
mute are re-detected as fresh transitions rather than swallowed as state Bellows
has already seen. Nothing that was skipped is replayed and no worker is
restarted — resuming re-enables future dispatch only.

While a PR is detached its `bellows-<anvil>-<n>` monitor row stays in the
Workers panel carrying the `detached` status (Hearth marks it `⊘`) rather than
disappearing, so a mute is visible instead of looking like a lost monitor.

**Surfaces.** Of the assignment pair, only `assign_bellows` has a surface: it is
reachable from the dashboard's PR rows and from Hearth's PR menu. Un-assigning
is an IPC verb only. The detach pair likewise exists as the `detach_bellows` /
`reattach_bellows` `pr_action` verbs on the IPC socket only; the `forge bellows
stop` / `forge bellows resume` CLI commands, the Hearth PR-menu items and the
web route that wrap them are not implemented yet, and this section will name
them once they are.

### Epic Orchestration Is Opt-In

Parent/child relations in bd are used for plain grouping far more often than for
a shared feature branch, so **orchestration is opt-in**: a parent bead must carry
the `crucible` label for any epic routing to happen.

```bash
bd label add <parent-id> crucible
```

An `epic-branch:<name>` label counts as opt-in too — it names the branch on
purpose — and additionally overrides the derived branch name:

```bash
bd label add <parent-id> epic-branch:feature/checkout-rewrite
```

The name is screened before it reaches git (`epic.ValidBranchName`: no leading
`-`, no `..`, no `@{`, no whitespace or `~^:?*[\`, no `.lock`/trailing-`.`, no
empty or dot-leading path component, 200 characters max). A label naming a
branch git would reject — or would read as a flag — still opts the parent in,
but the branch falls back to the derived `feature/<parent-id>` rather than being
passed on verbatim.

Without one of those labels — **including when the parent is `issue_type: epic`**
— nothing epic-specific happens:

- children dispatch as ordinary standalone beads: worktree from `main`, PR to
  `main`, auto-merge to `main`;
- a dispatched parent runs the ordinary Smith → Temper → Warden pipeline like any
  other bead; no feature branch is created;
- the bd parent/child relations are untouched, and sibling ordering through
  `blocks`-type dependencies still gates via `bd ready` as before.

With the label:

- the shared branch is `feature/<parent-id>`, or the name given by
  `epic-branch:<name>`. Both the poller (which stamps children with it) and the
  Crucible (which creates it) derive that name the same way, so a child dispatched
  outside the Crucible can no longer base on a branch that never exists;
- children whose parent is orchestrating — in the same poll batch or already
  mid-run — are withheld from the ordinary dispatch loop, since the Crucible
  dispatches them itself;
- `crucible_enabled: false` makes the labeled parent a Needs Attention entry
  rather than a dispatch, naming the two ways out (enable the setting, or remove
  the label). Its children are withheld from dispatch for the same reason they
  are withheld from a running Crucible: they are stamped with a branch nothing
  is going to create, so dispatching them would hard-fail on "base branch not
  found on origin" and burn each child's dispatch circuit breaker every cycle.
  The escalation lands on the parent — the bead carrying the label — so clearing
  it is one action, not one per child.

Two more cases escalate the parent rather than dispatch it, both for the same
reason: the parent's label is what routes its children, so anything that
dispatches the parent alone leaves them pointing at a branch nobody creates.

- **Children exist but none are ready.** A parent's children are read from the
  poll batch, so children blocked on something else look identical to no
  children at all. Forge asks bd for the wider answer: with open children
  remaining it escalates (naming them) instead of merging the parent to main and
  closing it. Only a parent whose children are *all closed* — or that has none —
  runs the ordinary pipeline.
- **The schematic crucible check declines.** With `schematic_enabled: true`, an
  opted-in parent is inspected before the Crucible runs. A decline used to route
  the parent standalone; since the label is an explicit human opt-in that also
  routes every child, a decline is now escalated with the check's reason. Remove
  the label to take the check's recommendation, or set `schematic_enabled: false`
  so the label alone decides.

> **Migration:** before this change, any parent with children was a Crucible
> candidate and any `epic`-typed parent routed its children onto a feature
> branch. Add the `crucible` label to the parents that should keep being
> orchestrated; every other parent becomes independent with no config change.

#### Carving One Child Out: the `independent` Label

The parent opts the family in; a single child can opt itself back out with the
`independent` label:

```bash
bd label add <child-id> independent
```

That child is treated as an ordinary standalone bead even though its parent is
orchestrating:

- it is **never claimed** by the Crucible — not dispatched onto the feature
  branch, not counted in the run's child total, and not fetched at all (nor is
  the subtree hanging off it: carving a bead out carves out its own children,
  which reach main through its PR);
- it is **not stamped** with the epic branch, so it builds its worktree from
  `main` and PRs to `main`. Tagging it `forgeReady` dispatches it like any other
  bead, in parallel with the epic if it is ready;
- it is **not withheld** from the ordinary dispatch loop the way an orchestrated
  sibling is — nothing else would ever run it.

**It is excluded from the epic's completeness set entirely.** The Crucible
pauses rather than shipping an epic whose children did not all land on the
feature branch, but an `independent` child's work *could not* land there: it
goes to main through its own PR. Waiting for it would hold the final PR for
something the final PR cannot contain, so an open independent child neither
pauses the run nor blocks the final PR — and, for the same reason, an opted-in
parent whose only remaining open children are independent has nothing left to
orchestrate and runs the ordinary pipeline to main. The queued-children event
names the beads it excluded, so the smaller child count is never unexplained.

The label is inert everywhere else: a bead whose parent never opted in — or that
has no parent — is already independent, so the label changes nothing. On a bead
carrying both an opt-in (`crucible`/`epic-branch:<name>`) and `independent`, the
opt-out wins: that parent orchestrates nobody and its children dispatch to main,
rather than the parent running to main while its children are stamped with a
branch nothing then creates. The daemon WARNs once per bead when it sees the
combination, naming the bead and the effect, so the opt-in going inert is not
something you discover from children merging out of order. Matching follows the
same rules as the opt-in labels — trimmed and case-insensitive, so
` Independent ` counts, while `independent-ish` does not.

> **Do not add `independent` to a parent mid-flight.** The demotion takes effect
> on the next poll and unwinds nothing. If that parent's Crucible has already
> merged child PRs onto `feature/<parent-id>`, the parent will from then on
> dispatch an ordinary pipeline to `main` and the feature branch — carrying
> merged child work — is orphaned: no final PR is ever created for it. The
> opt-out is only safe on a parent *before* its orchestration starts. On a child
> it is safe at any time.

### Event Bus vs Legacy Polling

Real-time consumers — the web UI's activity and PR-findings SSE streams and the
Hearth TUI's IPC event feed — can be driven one of two ways:

- **Event Bus (real-time):** the daemon runs an in-process publish/subscribe Bus.
  Every logged event is fanned out to subscribers immediately, so a new event
  reaches a connected client in well under 100 ms instead of on the next poll
  tick. Enable it with `bus_enabled: true` (or the `--enable-bus` daemon flag).
- **Legacy polling (default):** with `bus_enabled: false` no Bus is constructed
  and consumers re-read events from the state DB via `EventsSince` on a ~2 s
  timer. This is the safe rollout default.

Three settings control the behaviour (all in the `settings:` block):

| Setting | Purpose |
|---------|---------|
| `bus_enabled` | Master switch. `true` wires the Bus into the state DB; `false` (default) keeps every consumer on legacy polling. Also settable at startup with `--enable-bus`. |
| `bus_buffer_size` | Per-subscriber channel buffer (default `256`). When a slow consumer falls this many events behind, the Bus drops the oldest buffered event and delivers a **gap marker**, prompting the consumer to re-sync missed events from the DB via `EventsSince`. A publisher is never blocked by a slow subscriber. Also settable with `--bus-buffer-size`. |
| `sse_poll_fallback` | **Deprecated, one-release safety valve.** Forces the `/api/activity/stream` SSE endpoint back onto the 2 s poll loop even when `bus_enabled` is true, without disabling the Bus for other consumers. Hot-reloadable — takes effect on the next SSE connect. Scheduled for removal once the bus-based stream has proven stable; do not build new behaviour on it. |

```yaml
settings:
  bus_enabled: true       # real-time SSE/IPC delivery
  bus_buffer_size: 256    # per-subscriber buffer before a gap marker fires
  sse_poll_fallback: false  # deprecated escape hatch; leave false
```

When the Bus is enabled the SSE activity stream uses a **replay-then-live**
handover: it subscribes to the Bus *before* replaying the recent-event backlog
(so nothing published mid-replay is lost), replays via `EventsSince`, then hands
over to the live channel while de-duplicating any event already emitted during
replay. A resuming client's `Last-Event-ID` header seeds the replay cursor so it
receives exactly the events it missed — no duplicates, no gaps.

### Warden Rule Filtering

When `auto_learn_rules` is enabled, the Warden accumulates review rules per anvil
in `.forge/warden-rules.yaml`. To keep the review prompt small, Forge filters out
rules that can't apply to the current diff before rendering the checklist. These
settings live under `settings.warden`.

```yaml
settings:
  warden:
    max_rules_per_review: 30   # cap on rules emitted per review; 0 = default 30; negative = no cap
    use_all_rules: false       # true bypasses the three filter passes (keeps only the cap)
    filter_path_glob: true     # filter by Rule.Paths against the changed files
    filter_category: true      # filter by Rule.Category against the extension→category map
    filter_pattern_grep: true  # substring-match ≥4-char words from Rule.Pattern against the diff
    archive_after_days: 180    # Smelter staleness sweep threshold; 0 = default 180; negative disables
    dedup_threshold: 0.6       # similarity score above which duplicate rules are archived; 0 = default 0.6
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `warden.max_rules_per_review` | int | `30` | Caps the number of rules emitted in the review checklist after filtering. `0` (or omit) uses the default of `30`; a negative value disables the cap entirely; a positive value sets an explicit cap. |
| `warden.use_all_rules` | bool | `false` | When `true`, bypasses the three filter passes and applies only `max_rules_per_review`. Useful for A/B comparison against the pre-filter behavior. |
| `warden.filter_path_glob` | bool | `true` | Enables filtering by each rule's `Paths` against the changed files in the diff. |
| `warden.filter_category` | bool | `true` | Enables filtering by each rule's `Category` against the in-code extension → category map. |
| `warden.filter_pattern_grep` | bool | `true` | Enables substring matching of ≥4-character words from each rule's `Pattern` against the diff. |
| `warden.archive_after_days` | int | `180` | Staleness threshold (days) used by the Smelter's Pass 2 sweep: a rule older than this with no recent source activity is archived with reason `stale`. `0` (or omit) uses the default of `180`; a negative value disables the pass ("never archive"). |
| `warden.dedup_threshold` | float | `0.6` | Similarity score (0.0–1.0) above which two active rules are treated as duplicates and the older entry is archived with reason `duplicate`. `0` (or omit) uses the default of `0.6`. |

## Assay — AI Pull-Request Review

**Assay** is a multi-pass AI review of open pull requests (a Triage pass followed
by parallel Logic / Security / Conventions / Tests / Repo passes) triggered by
Bellows. It is configured under the **top-level** `assay` key (a sibling of
`settings`, not nested inside it), with an optional per-anvil overlay under each
anvil's `assay` key. Pointer-typed fields are tri-state: an unset value inherits
from the global `assay` block (and, for the per-anvil overlay, from the built-in
default).

Repeat reviews are **incremental** by default: once a PR has been reviewed, a
new head is reviewed as the delta since the last reviewed commit, with earlier
findings injected into the prompts as already-reported. The top-level summary
is a single per-PR comment edited in place on each run (keyed by a hidden
`<!-- assay-summary -->` marker); each edit opens with a stamp naming the
reviewed head and explaining that later runs edit the comment, and superseded
summaries are archived — newest first — in a collapsible "Previous Assay
summaries" section rather than overwritten (oldest entries are trimmed if the
comment would exceed GitHub's size limit). The `nit_cap` /
`max_findings_per_pr` budgets are cumulative across the PR's whole review
history — together these are what keep a PR's comment volume flat across
repeat reviews.

```yaml
assay:
  enabled: false             # master switch (default false)
  shadow_mode: true          # true = write findings but post nothing (default true)
  skip_drafts: true          # skip draft PRs (default true)
  debounce_seconds: 300      # coalesce rapid pushes before reviewing (default 300)
  daily_cost_limit_usd: 5.0  # per-day USD cap for Assay reviews (default 5.0)
  max_runs: 2                # executed reviews per PR; <=0 = no cap (default 2)
  max_diff_bytes: 250000     # cap on the diff embedded in pass prompts (default 250000)
  max_base_file_bytes: 100000 # cap on base-file context bytes (default 100000)
  nit_cap: 5                 # cumulative per-PR Nit budget; <=0 = no cap (default 5)
  incremental: true          # repeat reviews read only the changes since the last
                             # reviewed commit (default true)
  max_findings_per_pr: 30    # cumulative per-PR findings cap, every severity;
                             # <=0 = no cap (default 30)
  triage_provider: claude    # provider spec for the cheap triage pass
  review_provider: claude    # provider spec for the five deep passes
  model_tier: default        # semantic label recorded for observability
  triage_model: ""           # model hint for triage (empty = provider default)
  review_model: ""           # model hint for the deep passes (empty = provider default)
  skip_paths:                # doublestar globs excluded from review
    - "**/*.pb.go"
    - "web/dist/**"

anvils:
  my-api:
    path: /repos/my-api
    assay:                   # per-anvil overlay — only non-empty fields override the global block
      enabled: true
      shadow_mode: false
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool\|null | `false` | Master switch. When false, Assay never runs. |
| `shadow_mode` | bool\|null | `true` | When true, Assay writes findings to `pr_findings` but posts nothing publicly — the safe default. Set to `false` to post review comments. |
| `skip_drafts` | bool\|null | `true` | Skip draft PRs. |
| `debounce_seconds` | int\|null | `300` | Seconds to coalesce rapid push bursts before running a review, so a flurry of commits triggers one review rather than many. |
| `daily_cost_limit_usd` | float\|null | `5.0` | Per-calendar-day USD cap on Assay review spend. Prevents a runaway PR loop from silently burning quota. Set `0` for no cap. While the cap is exhausted, due reviews are skipped and their heads count as reviewed for merge readiness — Bellows logs each skip and records an `assay_skipped` event once per PR head. |
| `max_runs` | int\|null | `2` | Maximum number of executed Assay reviews per PR (Assay re-reviews on every new head SHA). A value `<= 0` means no cap. |
| `max_diff_bytes` | int\|null | `250000` | Caps the size of the diff embedded in pass prompts. `<= 0` falls back to the shared `diff.MaxBytes` default. |
| `max_base_file_bytes` | int\|null | `100000` | Caps the base-file context bytes included with the diff. |
| `max_turns_per_pass` | int | `0` (engine default, 12) | | Agent turn budget for each review pass (every file read costs a turn). Raise for repos whose rules file and layout need more reading than the default — the telltale is passes failing `error_max_turns` at exactly the cap on modest diffs. |
| `nit_cap` | int\|null | `5` | Cumulative per-PR budget for Nit-severity findings. Nits still open from earlier reviews count against it, so a repeat review cannot add another `nit_cap` on top of the last run's. `<= 0` means no cap. |
| `incremental` | bool\|null | `true` | On a repeat review, feed the passes only the changes pushed since the last successfully reviewed commit (the delta), with the prior findings listed in the prompt as already-reported. Falls back to a full review automatically when the last reviewed commit is no longer an ancestor of the head (force-push/rebase). A repeat push whose delta touches nothing in the net PR diff (upstream merge, revert) is recorded as a skipped run instead of re-reviewing the whole PR. |
| `max_findings_per_pr` | int\|null | `30` | Cumulative cap on the findings a PR may accumulate across all of its reviews — every severity, Important included. Open findings from earlier runs count against it. The hard brake on total comment volume; `<= 0` means no cap. |
| `triage_provider` | string | `""` (Claude) | Provider spec (same syntax as `settings.providers`) for the cheap triage pass. Empty defaults to Claude. |
| `review_provider` | string | `""` (Claude) | Provider spec for the five deep passes. Empty defaults to Claude. |
| `model_tier` | string | `"default"` | Semantic label describing desired model strength (e.g. `default`, `fast`). Recorded for observability; the concrete model always comes from the model hints below, never a baked-in ID. |
| `triage_model` | string | `""` | Model hint for the triage pass. Empty means "let the provider pick its default model". |
| `review_model` | string | `""` | Model hint for the deep passes. Empty means "let the provider pick its default model". |
| `skip_paths` | []string | `[]` | Doublestar globs whose files are excluded from review (their hunks are dropped before the diff reaches any pass). |

**Per-anvil overlay:** each anvil may set an `assay` block whose non-zero fields
override the corresponding global values (pointer fields override when non-nil;
string fields when non-empty; `skip_paths` when non-empty). Anvils without an
`assay` block inherit the global configuration unchanged.

## Pricing Tables

Forge tracks token spend per bead and per day. **Claude self-reports an exact
`total_cost_usd`** in its stream output, so Claude cost is never estimated. For
providers that do not report a cost (Copilot, Gemini, OpenAI/Codex), Forge
estimates spend from token counts and a configurable per-model pricing table.

Because these are estimates, Forge logs an info line the first time a fallback
price is applied for a given provider/model each day
(`cost: applying fallback pricing estimate …`) so you can spot drift between the
table and real provider billing.

### `settings.pricing`

Overrides per-model USD rates **per 1M tokens**. Overrides are overlaid on top
of the built-in defaults, so you only need to list the models you want to change.

| Model key | Input | Output | Cache read | Cache write |
|-----------|-------|--------|------------|-------------|
| `claude-sonnet` (also the Copilot fallback) | `3.00` | `15.00` | `0.30` | `3.75` |
| `claude-haiku` | `1.00` | `5.00` | `0.10` | `1.25` |
| `claude-opus` | `15.00` | `75.00` | `1.50` | `18.75` |
| `gemini` | `3.50` | `10.50` | `0.00` | `0.00` |
| `openai` | `2.50` | `10.00` | `0.00` | `0.00` |

When a fallback estimate runs, Forge first looks for an exact model-key match,
then infers a family from the model name (e.g. a Copilot `claude-opus-4.6`
resolves to the `claude-opus` row), then falls back to the provider's default
key. So you can add rows keyed by a specific model id if you need finer control.

```yaml
settings:
  pricing:
    gemini:
      input_per_m: 4.00
      output_per_m: 12.00
    claude-opus:
      input_per_m: 15.00
      output_per_m: 75.00
      cache_read_per_m: 1.50
      cache_write_per_m: 18.75
```

### `settings.copilot_premium_multipliers`

Overrides the premium-request weight for a Copilot model. Weighted requests
count against `copilot_daily_request_limit`. Overrides are overlaid on the
built-in defaults (e.g. `claude-opus-4.6: 3`, `claude-opus-4.6-fast: 30`,
`claude-haiku-4.5: 0.33`, most `gpt-5.x: 1`, `gpt-5-mini`/`gpt-4.1: 0`). Any
model not present in the table defaults to `1.0`.

```yaml
settings:
  copilot_premium_multipliers:
    claude-opus-4.6: 3
    claude-haiku-4.5: 0.33
```

Both maps are hot-reloadable: editing the config file re-applies them to running
workers without a daemon restart.

## Log Management

Forge keeps its own logs bounded with two independent mechanisms under `~/.forge/logs/`:

- **`daemon.log` rotation** — the daemon log is size-rotated at 50 MB, keeping 3
  gzip-compressed backups (`daemon-<timestamp>.log.gz`). Rotation is automatic and
  not configurable via `forge.yaml`. An already-oversized `daemon.log` (e.g. one
  that predates rotation) is rotated out on the first write after upgrade, so total
  daemon-log disk use stays bounded.
- **Preserved bead-log retention sweep** — when a worktree is cleaned up, a worker's
  logs are preserved under `~/.forge/logs/<beadID>/`. A daily sweep deletes these
  directories once their newest file is older than `log_retention_days` (default 30),
  skipping any bead that currently has a running worker. When a directory is removed,
  the affected `workers.log_path` rows are cleared so the API reports "no log" rather
  than a dangling path, and one summary event (`log_sweep_done`) is emitted per sweep.
  Set `log_retention_days: 0` to disable the sweep. The sweep never touches the live
  `daemon.log` file — the two mechanisms are independent.

## Self-Deploy — Automatic Daemon Rebuild on Merge

When Forge orchestrates its own repository, daemon-side fixes only take effect
once someone rebuilds the production binary (`~/bin/forge`) by hand — a step
that is easy to forget, causing "phantom" regressions where a fix is merged but
the running binary is weeks behind `main`. Self-deploy closes that gap: when a
PR merges on Forge's own anvil, the daemon drains its workers, rebuilds the
binary from source, verifies it, atomically swaps it into place (keeping the
previous binary for rollback), and restarts the systemd unit.

**Disabled by default.** The entire flow is inert unless `self_deploy.enabled`
is `true` and `self_deploy.anvil` names the anvil that is Forge's own repo.

```yaml
self_deploy:
  enabled: true             # default false — nothing happens while unset
  anvil: forge              # required: the registered anvil that is Forge's repo
  repo_path: ~/source/Forge # optional: source to pull+build (default: the anvil's path)
  binary_path: ~/bin/forge  # optional: live binary to replace (default ~/bin/forge)
  unit_name: forge          # optional: systemd unit to restart (default "forge")
  restart_command: systemctl # optional: restart executable (default "systemctl")
  restart_args: []          # optional: args before "restart <unit>" (e.g. ["--user"])
  branch: main              # optional: base branch a merge must target (default "main")
  build_target: ./cmd/forge # optional: go build target (default "./cmd/forge")
  max_drain_wait: 30m       # optional: how long to wait for workers to finish (default 30m)
```

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enabled` | bool | `false` | Master switch. While false nothing is pulled, built, swapped or restarted. |
| `anvil` | string | `""` | The registered anvil that is Forge's own repository. Required when `enabled`; a merge is only acted on when its event anvil matches. |
| `repo_path` | path | the anvil's `path` | Source checkout to `git pull --ff-only` and build from. A leading `~` is expanded. |
| `binary_path` | path | `~/bin/forge` | The live binary replaced on deploy. The outgoing binary is preserved at `<binary_path>.prev`. |
| `unit_name` | string | `forge` | The systemd unit restarted after the swap. |
| `restart_command` | string | `systemctl` | Executable used to restart the unit (see *Restart privileges* below). |
| `restart_args` | []string | `[]` | Arguments inserted before `restart <unit_name>` — e.g. `[--user]`, or `[systemctl]` when `restart_command` is `sudo`. |
| `branch` | string | `main` | Base branch a merged PR must target to trigger a deploy, and the branch pulled before building. |
| `build_target` | string | `./cmd/forge` | The `go build` package target. |
| `max_drain_wait` | duration | `30m` | How long the daemon keeps dispatch paused waiting for active workers to finish before giving up and resuming. The drain check is re-run every 10s for the whole window; the deploy proceeds as soon as the forge is idle. `0`/omitted uses the default; a negative value is rejected at load. |
| `drain_timeout` | duration | — | **Deprecated** alias for `max_drain_wait`. |

`max_drain_wait` was previously called `drain_timeout`. The old key is still
read, so existing configs keep working; `max_drain_wait` wins when both are set
(a zero or negative value on either field falls through to the next candidate,
so `max_drain_wait: 0` still means "use the 30m default", not "do not wait").
Sizing it is a trade-off between how long a merged fix may sit undeployed and
how long dispatch stays paused: the wait only ever ends early, so a generous
value costs nothing when the forge is idle, and `smith_timeout` is a reasonable
lower bound — anything shorter systematically loses to a Smith that started just
before the merge.

`self_deploy` is a top-level YAML block (a sibling of `settings`, like `assay`).
The `FORGE_`-prefixed environment overrides documented below cover `settings.*`
keys; configure self-deploy in the YAML file.

**Restart privileges / user** — the restart runs `<restart_command>
<restart_args...> restart <unit_name>`. Wire it to however the daemon is allowed
to restart its unit:

- **System unit as root** (the default): leave both unset →
  `systemctl restart forge`.
- **System unit as an unprivileged user** that has sudo rights for the restart:
  `restart_command: sudo`, `restart_args: [systemctl]` →
  `sudo systemctl restart forge` (the sudoers rule must be NOPASSWD, since the
  restart runs non-interactively).
- **User unit** (`systemctl --user`): `restart_args: [--user]` →
  `systemctl --user restart forge`.

Those are the logical invocations; the actual argv is wrapped so the restart
survives the unit stop — see the next section.

### Detached restart

The daemon is asking systemd to stop the unit it is itself running in, so a
restart child spawned the ordinary way is killed by its own request: it inherits
the daemon's cgroup, and `systemctl restart` SIGKILLs everything in that cgroup
before the restart completes. Forge saw this as
`sudo [systemctl restart forge]: signal: killed` — a "failed" restart — and
rolled a perfectly good binary back.

The restart is therefore spawned fully detached, in three independent ways:

- **No context.** The child is started with `exec.Command`, never
  `exec.CommandContext`, so neither the deploy deadline nor daemon shutdown can
  cancel it. It is never waited on, and its stdio is left nil (`/dev/null`)
  rather than inheriting handles that close when the daemon dies.
- **New session.** `setsid` (Unix) puts it in its own session and process group,
  so signals aimed at the daemon's group miss it.
- **Own cgroup.** The invocation is wrapped in `systemd-run --scope --collect`
  with a transient unit named `forge-selfdeploy-<sha>-<pid>` (SHA truncated to
  12 chars, PID appended so back-to-back deploys never collide on a scope name
  systemd has not yet collected). The scope lives outside `forge.service`'s
  cgroup, so stopping the unit no longer kills the process performing the
  restart, and `--collect` makes systemd clean the scope up afterwards.

The resulting argv depends on how the restart is wired:

| Config | Restart argv |
|--------|--------------|
| defaults | `systemd-run --scope --collect --unit=forge-selfdeploy-<sha>-<pid> systemctl restart forge` |
| `restart_command: sudo`, `restart_args: [systemctl]` | `sudo systemd-run --scope --collect --unit=… systemctl restart forge` |
| `restart_args: [--user]` | `systemd-run --user --scope --collect --unit=… systemctl --user restart forge` |
| `systemd-run` not on `PATH` | `systemctl restart --no-block forge` |

Two details in that table matter. A privilege wrapper (`sudo`, `doas`,
`pkexec`) stays in *front* of `systemd-run`, because creating a system scope
needs the privileges the wrapper grants. And `--user` propagates to
`systemd-run` as well, since a `systemctl --user` unit lives in the user manager
and its scope has to be created there too.

When `systemd-run` is unavailable the fallback is `restart --no-block`, which
hands the job to PID 1 and returns immediately instead of waiting inside the
doomed cgroup. It is weaker than a scope — it does not move the process out of
the unit, it only stops it *waiting* there — but it is enough in practice: the
child exits as soon as the job is enqueued, so the stop has nothing left to
kill.

Either way the exact argv, the mode used, the build SHA, the binary path and the
rollback path are logged to `daemon.log` *before* the spawn, so a restart that
is killed mid-flight is still diagnosable afterwards.

Once the restart has been spawned Forge does not wait on it: a nil result means
"restart requested", not "restart completed". Only a failure to *spawn* (missing
executable, permission denied) counts as a failed restart and triggers rollback.

Off Linux there is no unit cgroup to escape; the Windows build applies the same
intent with `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS` so the child keeps no
console and stays out of the daemon's Ctrl-C group.

### How it behaves

- Triggered only by a `pr_merged` event whose anvil matches `self_deploy.anvil`
  and whose base branch matches `self_deploy.branch`. A merged PR with no
  recorded base branch is skipped rather than assumed to target `branch`, so an
  unrelated merge cannot trigger a production restart.
- **Drain guardrail** — dispatch is paused, then the drain check is re-run every
  10s until no worker is active (including operator-paused workers, which still
  hold a worktree). "Active" means a worker that owns a live process or pipeline
  — a Smith, or a quench/burnish/rebase/Assay fix worker — plus a pipeline
  finishing up after review (its row reads `monitoring` from warden approval
  until the PR is created and the worktree released). Bellows' per-PR monitor
  rows do **not** count: they carry no PID and nothing a restart could
  interrupt, and they live for as long as their PR stays open, so waiting on one
  would defer every deploy for the full `max_drain_wait` whenever a PR sat in the
  fix loop. Because a deploy is triggered by a merge — exactly when a
  Smith is most likely to still be mid-run — the wait is bounded rather than
  sampled once: the deploy lands in the first gap that opens. If workers are
  still active after `max_drain_wait`, the deploy is deferred (a
  `self_deploy_skipped` event is logged with the elapsed time and the beads that
  held it up) and any pause the deploy introduced is undone. The pause is undone
  on *every* non-restart exit — drain timeout, build failure, rollback — so a
  failed deploy can never leave the daemon paused; a pause that predates the
  deploy is an operator decision and is left alone. A drain check that *errors*
  (e.g. a transient `state.db` failure) is not evidence that the forge is idle,
  so polling continues; if no check succeeds for the whole window the deploy
  aborts with that error instead of being reported as a deferral.
- **The drain pause names itself** — while the drain is held, `forge status`
  reports `Dispatch  PAUSED (self-deploy drain, waiting on 2 workers, max 30m) —
  running workers continue` rather than `PAUSED (manual)`, and Hearth and the web
  dashboard show the same cause. The reason rides along the existing
  `dispatch_paused` boolean as `dispatch_pause_reason` (`manual` | `self-deploy`)
  and `dispatch_pause_detail` in the IPC/`/api/status` payload, so a client that
  only reads the boolean is unaffected. A pause that predates the deploy is
  restored with its own reason, so an operator pause is never relabelled.
- **Verify before swap** — the freshly built binary must pass `forge version`
  and `forge --help` (exit 0). If verification fails, the live binary is left
  untouched.
- **Rollback** — the outgoing binary is preserved at `<binary_path>.prev`. If
  the restart cannot be spawned at all (missing executable, permission denied),
  the previous binary is restored automatically. Once the detached restart has
  started, Forge does not wait on it — a nil result means "restart requested",
  not "restart completed".
- **Events** — `self_deploy_started`, `self_deploy_success`,
  `self_deploy_rollback`, `self_deploy_failed`, and `self_deploy_skipped` are
  written to the event log.
- **Needs Attention** — a deploy that ends anywhere other than "new binary live
  and restarting" also raises an entry in Hearth's Needs Attention list. See the
  next section.
- **Single-flight** — a second merge while a deploy is in progress is ignored;
  the in-flight deploy already pulls the latest tip.

The systemd unit should use `Restart=always` (or an equivalent) so the daemon
comes back after the restart terminates the running process.

### Needs Attention on a failed or rolled-back deploy

A failed deploy is otherwise silent. After a rollback the daemon keeps running
the old binary exactly as before — same PID behaviour, same logs, no error on
any dashboard — so the only symptom is `forge version` quietly disagreeing with
`origin/main`, which nobody checks until a merged fix appears not to work. Every
non-restart outcome is therefore persisted (in the `deploy_failures` table of
`~/.forge/state.db`) and surfaced as a Needs Attention entry, badged
`⚙ DEPLOY` in Hearth.

| Reason | Title | State of the host | Cleared when |
|--------|-------|-------------------|--------------|
| `drain_timeout` | Self-deploy deferred: workers did not drain | Nothing was touched; the previously running build is still live and correct for the code it was built from. | The next deploy drains successfully. |
| `swap_failed` | Self-deploy failed / rolled back: binary swap failed | The new binary could not be moved into place. The previous binary is restored when one existed. | The next deploy reaches its restart. |
| `restart_failed` | Self-deploy rolled back: restart failed | The new binary was installed but the restart never started; the previous binary was put back. | The next deploy reaches its restart. |
| `rollback_failed` | Self-deploy failed: restart AND rollback failed | The worst case: the on-disk binary is the new, never-started build while the running process is still the old one. A stop/start or a crash-restart will bring up the untested build. | The next deploy reaches its restart. |

The title carries the targeted unit (`… (unit forge)`), and the detail line
answers "is the merged fix live?" without a shell:

```
attempted 0f4be00a1b2c; restored a48fc6e91234; restart failed: exec: "systemctl": executable file not found in $PATH; at 2026-08-06T09:14:22Z
```

Fields are omitted when unknown — `restored …` only appears when a rollback
actually put the previous binary back (so it is absent for both `drain_timeout`,
where nothing moved, and `rollback_failed`, where the restore itself failed).

There is **one entry per (anvil, failure mode)**, so a later deferral cannot
overwrite the record of an earlier rollback: they describe different problems
and the rollback is the more serious one. Repeating the same failure refreshes
the existing entry rather than stacking duplicates.

Entries are anvil-level, not bead-scoped — there is no worker to retry and no
bead to dismiss. They clear themselves rather than needing an operator action:
the `drain_timeout` entry the moment a later deploy drains, everything else once
a deploy gets as far as requesting its restart. An entry that is still listed
therefore means the condition still holds.

### Spotting a stale binary

The question a `⚙ DEPLOY` entry raises is always the same: *is the running
binary the merged code?* Self-deploy builds with a plain `go build` (no
`-ldflags`), so `Version` stays `dev` and `Build` is the 7-character VCS
revision Go embeds from the checkout (with a `-dirty` suffix if the tree was
modified at build time). That makes the check a two-line comparison:

```bash
forge version                                   # forge dev (build 0f4be00)
git -C ~/source/Forge rev-parse --short=7 origin/main
```

If those disagree, the running daemon predates `origin/main`. Supporting
evidence:

```bash
ls -l ~/bin/forge ~/bin/forge.prev              # .prev is the preserved outgoing binary
grep self-deploy ~/.forge/logs/daemon.log | tail -20
forge history events | grep self_deploy         # started / success / rollback / failed / skipped
systemctl list-units 'forge-selfdeploy-*'       # a transient restart scope that never finished
journalctl -u forge --since '1 hour ago'
```

- A successful deploy leaves the outgoing binary at `<binary_path>.prev`, so
  `.prev` existing is normal.
- A rollback *renames* `.prev` back over the live binary, so after a completed
  rollback there is no `.prev` and `~/bin/forge` is the old build again.
- After `rollback_failed` both exist and `~/bin/forge` is the **new** build that
  was never started — restarting the unit will bring that untested binary up.
  Decide deliberately: either finish the deploy (`systemctl restart forge`) or
  put the old binary back (`mv -f ~/bin/forge.prev ~/bin/forge`) before anything
  restarts the unit for you.

Nothing retries automatically. A deferred or failed deploy is picked up by the
next merge on `self_deploy.branch`; to deploy immediately, use the manual path
below.

### Manual fallback — `restart.sh`

`scripts/restart.sh` (deployed to `~/.forge/restart.sh`) remains the manual
rebuild-and-restart path and is what Hytte's "Rebuild & Restart" button invokes.
It exports `PATH` to include the Go toolchain (`/usr/local/go/bin`) plus
`~/go/bin` and `~/bin`, so it works when run from a bare `systemd-run`/cron
context that does not inherit the login `PATH` (which previously failed with
`go: command not found`).

## Preview Environments (Kiln)

**Kiln** starts on-demand preview environments for a worker's branch so a
UI-heavy change can be looked at instead of only read as a diff. A preview is a
detached checkout of the branch plus the services a project declares in its
manifest — no containers, just supervised local subprocesses, the same model
Smith, Temper and hooks use.

Two things must be true before an anvil can produce a preview:

1. `settings.preview_enabled` is `true` and the anvil has not set
   `preview_enabled: false` (the per-anvil tri-state defaults to inheriting the
   global value), and
2. the anvil's **main checkout** contains `.forge/preview.yaml`.

The manifest format — services, ports, health checks, template variables and
the main-checkout-only rule — is documented in
[preview-manifest.md](preview-manifest.md). Two complete worked manifests (a Go
API + Vite client, and a .NET API + React client sharing an MSSQL server) with
the per-preview database setup/teardown scripts that go with them are in
[preview-manifests.md](preview-manifests.md).

```yaml
settings:
  preview_enabled: true          # master gate, default false
  preview_max_concurrent: 2      # previews running at once
  preview_evict_lru: false       # cap reached: false = reject, true = stop the LRU preview
  preview_idle_timeout: 30m      # tear down after this much inactivity; 0 disables
  preview_port_range: '24000-24999'
  preview_bind_host: 127.0.0.1   # 0.0.0.0 to reach previews from a LAN/VPN
  preview_public_host: ''        # hostname shown in links; empty = bind host
  preview_proxy_base: ''         # DNS suffix for host-based routing; empty = off
  preview_proxy_auth: session    # session (default) | none — see Auth gating below

anvils:
  my-api:
    path: /home/robin/source/MyApi
    preview_enabled: true        # null (omitted) inherits settings.preview_enabled
    preview_auto: ready_to_merge # off (default) | ready_to_merge
    preview_quests: true         # run this anvil's E2E quests against the preview
```

The three **per-anvil** keys are hot-reloadable: they are resolved per request,
so editing `forge.yaml` is enough for the next preview start, automatic start or
quest run to obey the new value. The `settings.*` block above is read once, at
startup — changing it logs a WARN naming the key and needs a daemon restart. See
[Hot Reload](#hot-reload). The one exception worth knowing: if the daemon
started with previews off for *every* anvil, no preview manager was built, so
opting an anvil in later also needs a restart — the daemon warns when it spots
exactly that.

### Choosing a preview port range (`preview_port_range`)

Preview service ports come out of `preview_port_range` (default
`24000-24999`). The one hard constraint on an override is that **the range must
sit entirely below the host's ephemeral port floor** — the range the kernel
hands out as the *source* port of outbound connections:

| Platform | Ephemeral range | Where to check |
|----------|-----------------|----------------|
| Linux | typically `32768-60999` | `cat /proc/sys/net/ipv4/ip_local_port_range` (`sysctl net.ipv4.ip_local_port_range`) |
| Windows | `49152-65535` | `netsh int ipv4 show dynamicport tcp` |

**Why it matters.** Kiln bind-tests a port when it allocates it, but the service
that port belongs to only binds it once it has finished starting — for a cold
`dotnet run` or `npm install` that is minutes later. If the allocated port lies
inside the ephemeral range, anything in the same network namespace that opens an
outbound connection during that window can be handed the port as its local port,
and the preview service then dies at bind:

```
Failed to bind to address http://127.0.0.1:42000: address already in use
```

The failure is timing-dependent and leaves no trace — the connection that took
the port is long gone by the time anyone looks — so it reads like a Kiln
allocator bug when it is really the kernel doing exactly what it was configured
to do. A range below the ephemeral floor cannot lose that race. (This is also
why the same manifest can pass every test on a Windows laptop, whose dynamic
range starts at 49152, and fail on a Linux box or a Kubernetes pod.)

The default avoids `30000-32767` as well, even though that band is below the
usual Linux floor: it is the Kubernetes NodePort convention, and reusing it for
previews is confusing to operators reading a cluster's port map — NodePorts live
on the node rather than in the pod's network namespace, so the clash is one of
comprehension, not of binding.

**Validation is a warning, never a rejection.** When the configured range
overlaps the host ephemeral range, the daemon logs a WARN at start naming both
ranges and the failure mode, then carries on. An operator may have narrowed or
moved the kernel range (`sysctl -w net.ipv4.ip_local_port_range="40000 60999"`),
or may simply have decided to live with the risk — so an existing config with an
explicit overlapping range keeps working exactly as before.

### Host-based preview routing (`preview_proxy_base`)

By default a preview is reached at `host:port` — a different port per service,
per preview, allocated out of `preview_port_range`. `preview_proxy_base` gives
previews stable names instead: set it to a DNS suffix and a bead's preview is
addressed as

```
<label>.<base>              # the entry service
<label>--<service>.<base>   # a named service in the same preview
```

where `<label>` is the bead id reduced to a DNS label and `<service>` a manifest
service name reduced the same way. `--` separates the two because a label never
contains one.

**The label mapping rule.** A bead id becomes a hostname label by:

1. trimming and lowercasing;
2. keeping `a-z` and `0-9` as they are;
3. folding every other character — `-`, `_`, `.`, `/`, a space — to a single
   `-`, with runs collapsed into one and no leading or trailing `-`
   (`_` is the character worth calling out: it is legal in the internal preview
   id Kiln uses for environment variables and directory names, and illegal in a
   hostname, so it is folded here and nowhere else);
4. prefixing `p-` if the result would start with a digit, since a DNS label may
   not;
5. falling back to `preview` if nothing is left.

| Bead id | Host label | Entry hostname under `preview.example.test` |
|---------|-----------|---------------------------------------------|
| `Forge-a1b2` | `forge-a1b2` | `forge-a1b2.preview.example.test` |
| `Forge_a1b2` | `forge-a1b2` | *(collides with the row above — see below)* |
| `Forge.a1b2` | `forge-a1b2` | *(collides with the row above — see below)* |
| `WI-12` | `wi-12` | `wi-12.preview.example.test` |
| `123-abc` | `p-123-abc` | `p-123-abc.preview.example.test` |

Manifest service names get a narrower fold of their own — lowercased, with `.`
and `_` turned into `-` — which is what the `--<service>` half of a hostname is
built from and matched against. It is not injective either (`api_v1` and
`api.v1` both become `api-v1`), but two services of one manifest colliding is a
manifest bug in the same class as two of them declaring the same port, so it is
fixed by renaming rather than detected at run time.

Bead ids longer than 63 characters produce an over-long label, which is not a
legal DNS label. Nothing truncates it — silently shortening would create a
second, quieter source of the collisions described below — so such an id simply
has no working preview hostname.

```yaml
settings:
  preview_proxy_base: preview.example.test
```

Requests then look like `forge-a1b2.preview.example.test` (the entry service)
and `forge-a1b2--api.preview.example.test` (its `api` service). Making those
names resolve is a DNS job: a wildcard record (`*.preview.example.test`)
pointing at the Forge box, or the equivalent entries in a hosts file for a
single-label base like `localtest`. Both forms are exactly one label deep, so a
single-level wildcard covers them — which is also what makes one wildcard TLS
certificate enough. [Deploying the preview
proxy](preview-proxy-deployment.md) works through the DNS, ingress and
certificate prerequisites for a Kubernetes deployment.

The Hearth 2.0 web server (`FORGE_WEB_ENABLED`) does the routing itself: a
request whose `Host` matches one of the two forms above is forwarded to the
preview's loopback port before any route matching happens, so the preview sees
the request byte for byte — same path, same query, same `Host` header, no
decompression, no redirect following. Streaming responses (SSE, HMR, long-poll)
are flushed as they arrive and websocket upgrades pass through. Traffic on any
other host — including the base itself, which is the dashboard's own name — goes
to the dashboard exactly as before, so switching the setting on cannot affect
it.

With the setting on, **every** preview link Forge hands out is the hostname
rather than `host:port` — the Preview button and the bead panel, the `/previews`
fleet page, and `forge preview list` in the terminal, all built by one helper
(`kiln.EntryURL`) so a link you read in the browser and one you copy out of the
CLI cannot disagree. The dashboard's links carry the scheme and port of the
request that reached Hearth (it answers preview names on its own listener) plus
an access token when the auth gate needs one; the daemon has no request to
follow, so the URL it reports over IPC — what the CLI prints — is plain
`https://<label>.<base>/`.

Proxied requests are gated on a Hearth session by default — see
[Auth gating for proxied previews](#auth-gating-for-proxied-previews-preview_proxy_auth).
The one thing the forward does change is Hearth's own cookies, which are
stripped on the way to the preview.

Two things deliberately stay on the port: the health checks Kiln runs against a
starting service, and the browser the preview quest runner drives
(`preview_quests`). Both run on the Forge box, where the loopback port is the
short way to the same process and a preview hostname would mean a DNS lookup and
an auth gate to reach it.

Browsing a preview through the proxy counts as activity, so the idle reaper
(`preview_idle_timeout`) never tears down a preview somebody is looking at. A
host that names no live preview gets a `404` saying which state it is in — `no
live preview for forge-a1b2`, `preview forge-a1b2 is stopped`, `preview
forge-a1b2 has no service "api"` — rather than the dashboard's login page.

The value is a bare DNS name. A scheme, a port, a path, a leading dot, an empty
or over-long label, or a character outside `[a-z0-9-]` is rejected at load time
rather than silently stripped; the value is lowercased and a trailing root dot
is dropped. Empty — the default — switches the whole feature off, and preview
links stay on `host:port`.

**Label collisions are refused at start, not at request time.** The label
mapping is not injective: `Forge-a1b2` and `Forge_a1b2` both fold to
`forge-a1b2`. With `preview_proxy_base` set, starting a preview for a bead
whose label matches a live preview's fails immediately —

```
kiln: bead ids "Forge-a1b2", "Forge_a1b2" all map to the preview host label
"forge-a1b2" — settings.preview_proxy_base routes by that label, so only one of
them can be previewed at a time
```

— because the alternative is two previews sharing one address and requests for
either landing on whichever the proxy resolves first. With the setting empty
nothing routes by hostname, so the check does not run and both previews start
normally.

### Auth gating for proxied previews (`preview_proxy_auth`)

A preview serves an unreviewed branch build. Behind a rotating loopback port
that was contained; behind a stable wildcard hostname it is not, so by default
every request Forge proxies to a preview has to come from an operator who is
signed in to Hearth. `preview_proxy_auth: none` turns that off for a trusted
network — it is the only way to get an unauthenticated proxied preview, and it
has to be written down.

Gating applies to the hostname-based proxy only. The raw `preview_bind_host`
ports are unchanged and remain unauthenticated; if they are reachable from
anywhere you do not trust, that is what `preview_bind_host` is for.

There are two ways a request proves itself, and which one is used is decided
automatically:

1. **The Hearth session cookie.** When the Hearth host and `preview_proxy_base`
   share a registrable parent domain — `hearth.example.com` and
   `preview.example.com` share `example.com` — the session cookie is issued with
   `Domain` set to that parent, so the browser sends it to preview hostnames too
   and the proxy simply validates the session the operator already has. Nothing
   extra is minted and signing out of Hearth revokes preview access with it.

   The parent has to be a real registrable domain. Two hosts that share only a
   public suffix (`a.github.io` and `b.github.io`) or only a TLD share nothing
   as far as this is concerned, and neither do IP addresses or `localhost`.

   The tradeoff of a shared `Domain` is that a preview can *write* cookies at
   that parent — a hostile branch could clobber the operator's session cookie
   and log them out. It cannot read the session (Forge strips its own cookies
   before forwarding, and they are `HttpOnly` regardless) and it cannot fixate
   one (logging in rotates the token), so the worst case is a nuisance from code
   you are already running on your own box. Deployments that would rather not
   accept even that can put the previews under a different registrable domain,
   which moves them onto the token path below.

2. **A short-lived signed link token.** When the widening above would not be
   legitimate, the preview link Hearth renders carries a `_forge_token` query
   parameter: an HMAC over the preview's label and an expiry, minted for an
   already-authenticated operator and valid for two minutes. The proxy verifies
   it on first contact, replaces it with a preview-scoped cookie (HttpOnly,
   good for eight hours, scoped to `preview_proxy_base` so per-service
   hostnames are covered) and redirects to the same URL without the token, so
   it does not linger in the address bar, history or a referrer.

   The cookie authorises exactly the preview named in the token: presenting one
   preview's grant on another's hostname is a `401`.

A request that proves neither is refused, never quietly served. A browser
navigation is redirected to the Hearth login on the apex of
`preview_proxy_base` — the host the dashboard itself answers on — with the
preview URL in a `next` parameter; anything else (an XHR, an asset, a
websocket) gets a `401` whose body says what is missing. An expired,
mismatched or unsigned token is a `401` that says which.

Hearth's own cookies are stripped from every request before it is forwarded.
The preview upstream is branch code and has no business seeing the session
cookie that the shared-`Domain` mechanism above puts within its reach.

Preview link tokens and grants are signed with a per-process secret, so they do
not survive a daemon restart. Neither do previews (Kiln clears them on
startup), so nothing is lost: reopen the preview from the dashboard.

An unauthenticated browser navigation is redirected to `/login` on the **apex of
`preview_proxy_base`**, carrying the preview URL in a `next` parameter. That
apex therefore has to be a host Hearth itself answers on — see
[Deploying the preview proxy](preview-proxy-deployment.md) for what that means
for DNS, an ingress and a certificate.

The login consumes `next` and sends the browser back to the preview once a
session exists, on both halves of the flow: the sign-in itself, and the `GET
/login` of someone who turns out to already have a session. The parameter is
never followed as given — it arrives from a link anyone can send, so it is
honoured only when it names a preview hostname under the *current*
`preview_proxy_base`. Anything else (another host, an embedded credential, a
non-HTTP scheme, the apex itself) is dropped and the login lands on the
dashboard as before; a bad `next` never fails the sign-in. Validation is done
by the daemon, not the dashboard, and the browser only follows the URL the
daemon hands back.

One case deliberately declines the round trip: where the Hearth host and
`preview_proxy_base` share no registrable parent, the session cookie is not
widened to reach preview hostnames (see the two paths above), so following
`next` would hit the gate again and bounce straight back to the login. Those
deployments land on the dashboard instead, where the preview link carries a
`_forge_token` that does work.

### Security posture: what a preview URL exposes

A preview hostname serves an **unreviewed branch build of somebody's
application**, wired to whatever that branch's manifest starts — typically a dev
server, an API process and a database seeded by the manifest's `setup` script.
Anything that application can read, a preview URL can expose; anything it can
write, a preview URL can drive. It is not a read-only rendering of a diff, and
it runs on the Forge box with the daemon's own filesystem and network reach.

Preview hostnames are also **predictable, not secret**. The label is a
deterministic fold of the bead id (see the table above) and the base is fixed
config, so anyone who knows or can guess a bead id knows the URL. Wildcard DNS
answers for every name under the base whether a preview exists or not, and a
certificate issued for `*.<base>` appears in public Certificate Transparency
logs. Nothing about the address is a credential.

What that adds up to, per mode:

| | `preview_proxy_auth: session` (default) | `preview_proxy_auth: none` |
|---|---|---|
| Who can reach a preview | Anyone holding a valid Hearth session cookie for the shared parent domain, or a `_forge_token` link Hearth minted for a signed-in operator (exchanged for a per-preview cookie, 8h). | Anyone who can resolve the hostname and reach the listener — no credential of any kind. |
| A stranger who knows the URL | Gets `401` (or a redirect to the Hearth login). Cannot tell whether the preview exists: authorisation runs *before* the lookup, so the 404-with-a-reason is only ever shown to an authorised caller. | Gets the application, at whatever privilege the branch build grants an anonymous visitor. |
| Blast radius of one leaked link | Nothing at all where the session cookie is shared (the link carries no credential). Where it carries a `_forge_token`: that one preview, and only if the leak is opened within two minutes; the cookie it exchanges for is scoped to that single label. Neither is a Hearth session — no API call and no other preview. | Every running preview, since the hostnames are predictable — one leaked link is the whole fleet. |
| Enumeration | A caller with no session cannot probe which beads have previews. | Anyone can walk bead ids and find every live preview. |
| What the preview can do back | Cannot read Hearth's cookies (stripped before forwarding, and `HttpOnly` regardless). Under the shared-cookie path it *can* write a cookie at the shared parent — worst case, logging the operator out. Under the token path it cannot even do that. | Same, plus the application is exposed to unauthenticated input from anyone. |

Use `none` only where the listener itself is already the boundary — a private
LAN, a WireGuard network, a box no untrusted client can route to — and where a
branch build being world-readable is acceptable for every anvil that can produce
a preview, not just the ones you have in mind. **Do not use `none` where preview
content includes non-public data**: a manifest whose `setup` restores a
production-shaped dataset makes that dataset an anonymous HTTP fetch away, and
the ports themselves have no separate defence.

Two things the gate deliberately does **not** cover:

- **The raw `preview_bind_host` ports.** Gating is a property of the hostname
  proxy. A service's own port is unauthenticated in both modes, which is why the
  bind host defaults to `127.0.0.1`; widening it to `0.0.0.0` reopens exactly the
  exposure the proxy gate closes, on a network path that never sees the gate.
- **The previewed application's own authentication.** Forge proves you are an
  operator, not that you are authorised inside the app. Whatever the branch build
  does with an anonymous visitor is what a signed-in operator gets too.

### When the cap is reached (`preview_max_concurrent`, `preview_evict_lru`)

`preview_max_concurrent` bounds how many previews run at once — started
previews and starts still in flight both count, so two requests racing for the
last slot cannot both win it.

**The default is rejection.** A start over the cap fails immediately, before any
worktree is checked out or any port is allocated, with an error naming the limit
and the beads holding the slots:

```
kiln: preview limit reached (2 of 2 in use, preview_max_concurrent=2; held by:
Forge-a1b2, Forge-c3d4) — stop one of those previews first, or set
settings.preview_evict_lru to have Kiln evict the least recently used one
```

Rejection is the default because a preview is something a person asked for and
may still be looking at. Requests are never queued: a preview that quietly
starts ten minutes later, after whoever wanted it gave up, costs memory and
serves nobody. `forge preview list` shows what holds the slots and
`forge preview stop <bead-id>` frees one.

**`preview_evict_lru: true` trades that for eviction.** A start that hits the
cap stops the preview with the oldest last-active time — the same clock the
idle reaper reads, bumped on start and on every preview API call — and takes
its slot. Eviction is a full teardown (services killed, the manifest's
`teardown` run, checkout removed, row deleted), not just a released slot. Two
things are deliberately never evicted: a preview whose start is still in flight,
and any preview at all on behalf of an **automatic** preview (`preview_auto`),
which is skipped when the box is full rather than taking a slot away from a
preview someone opened by hand.

Turn it on when previews are opened and abandoned faster than the idle timeout
reclaims them, and the alternative is an operator hunting for something to
stop. Leave it off if a preview disappearing from under an open browser tab
would be worse than a start that says no.

### Automatic previews (`preview_auto`)

By default a preview starts only when someone asks for one — the Preview button
in Hearth 2.0, the ad-hoc form on its `/previews` page, `forge preview start`,
or `preview_start` over IPC. An anvil can instead opt into starting one
automatically:

| Value | Behavior |
|-------|----------|
| `off` (default, also the value of an omitted key) | Previews start only on request. |
| `ready_to_merge` | Bellows starts a preview the moment one of this anvil's PRs transitions to ready-to-merge (CI green, no conflicts, no unresolved threads or pending reviews). |

`ready_to_merge` fires **once per transition**, not once per poll: a PR that
stays ready does not restart its preview, and a PR that goes back to needing
fixes and becomes ready again gets a fresh one. Automatic starts are subject to
exactly the same limits as manual ones — `settings.preview_enabled` and the
anvil's `preview_enabled` must both allow previews, the anvil needs a
`.forge/preview.yaml`, `preview_max_concurrent` still applies, and the idle
reaper still tears the preview down after `preview_idle_timeout`. When the cap
is already reached the auto-start is **skipped silently**: the reason is logged
(`auto-preview skipped: preview_max_concurrent reached`) but nothing is raised
for human attention, because nobody asked for that preview in the first place.
That holds even with `preview_evict_lru` on — an automatic start never evicts a
preview an operator started.
A successful automatic start is recorded as a `preview_auto_started` event, so
a preview appearing on its own is explainable from the event log. Previews are
never auto-started for external (`ext-*`) PRs.

Combined with the QuestGiver integration, `ready_to_merge` gives per-PR browser
E2E: the branch comes up on its own and the anvil's quests run against it while
the PR waits to be merged.

**Memory trade-off.** This is off by default because it inverts the cost model.
An on-demand preview exists only for the minutes someone is looking at it; with
`ready_to_merge`, every PR that goes green brings a full application stack up —
a database, an API process, a dev server — and holds that memory until the idle
timeout expires (30m by default) or the PR merges. On a box merging several PRs
an hour that is a permanent multi-hundred-megabyte resident cost rather than an
occasional one, and previews that nobody opens still pay it. Before enabling
it, size `preview_max_concurrent` against the memory the box can actually
spare (the cap is what stops a burst of ready PRs from exhausting it — starts
past the cap are dropped, not queued) and consider shortening
`preview_idle_timeout`, since an auto-started preview has no one watching to
stop it.

### Quests against previews (`preview_quests`)

An anvil's E2E quests (`<anvil>/.forge/quests/*.yaml`) normally run against the
fixed `url` declared in each quest file. With `preview_quests: true` the same
quests can be run against a live preview instead: the preview's entry service
URL is substituted into the `{{.BaseURL}}` placeholder that quest steps use.

```yaml
# <anvil>/.forge/quests/login.yaml
name: login-flow
url: http://localhost:3000   # used when no preview URL is supplied
steps:
  - action: navigate
    url: '{{.BaseURL}}/login'
```

`{{.BaseURL}}` is expanded in every step's `url` and `value`, with any trailing
slash trimmed off the base so `{{.BaseURL}}/login` never produces a double
slash. A quest that hardcodes absolute URLs still works — expansion only
touches the placeholders.

The gates are deliberately narrow, because a quest run drives a real browser
against a real application:

- `preview_quests` must be `true` for the anvil (default `false`), and previews
  must be enabled for it — `preview_quests: true` on an anvil that cannot run
  previews is a config validation error.
- A preview for the requested head commit must exist and be **healthy** (every
  service passing its health check). A missing, starting, degraded or failed
  preview means the run is skipped with a reason rather than run against a
  half-started stack.

Each run is tagged with the preview id and the head SHA it ran against, so
downstream reporting can post results once per commit rather than once per
attempt.

Quests are **never** started automatically by opting in: `preview_quests: true`
only makes the action available. A run happens when someone asks for one (the
Hearth 2.0 button, or `preview_quest_run` over IPC). Nor does a preview run use
the anvil's `questgiver_setup_cmd` / `questgiver_teardown_cmd` — those exist to
bring an environment up for the scheduled scan, and the preview *is* the
environment. Preview runs also create no beads, unlike a failing scheduled scan.

**Where the results appear.** In two places, both informational:

1. **The Hearth 2.0 preview panel.** The bead-detail preview panel grows a **Run
   quests** button for anvils that opted in, and only while the preview is
   healthy — the same two gates the backend enforces, so the endpoint answers
   `403` to anyone who posts without them. A run is dispatched asynchronously:
   the panel gets a run id immediately and polls it, showing the overall status
   (running/passed/failed/skipped/errored), a row per quest with its failing step
   and message, and thumbnails of any screenshots the quests captured.
2. **A comment on the bead's pull request.** When a run reaches a verdict and the
   bead has an open PR, Forge posts a single comment with a pass/fail table (one
   row per quest, with duration and the failing step's message) and a screenshots
   section. The comment is keyed to the head SHA by a hidden
   `<!-- forge-preview-quest: <sha> -->` marker, so re-running the same commit
   **edits** its existing comment while a new head gets a fresh one. Runs that
   were skipped by a gate, or that fell over before exercising the branch, are
   not reported at all — and neither is a bead with no open PR.

Forge hosts no artifact store, so screenshots are listed by their path on the
Forge host in the PR comment rather than embedded. The panel serves them
directly and does show thumbnails.

**Results never block anything.** This is a guarantee, not a default:

- No check run and no commit status is ever created from a quest run, so a red
  run cannot turn a PR's merge box red.
- Nothing reads a run back — not a pipeline stage, not Bellows, not the merge
  path, not Warden or Assay. The only reader is the status command the panel
  polls.
- A run is never persisted to `state.db`. It lives in daemon memory for the life
  of the daemon and is gone after a restart, which is what keeps the point above
  true by construction.
- A failure to post the PR comment (no `gh`, no network, a lost race with a
  closing PR) is logged and dropped; it affects neither the run nor the PR.

A red run is a prompt for whoever is reviewing the branch, not a block on it,
which is what both the panel and the PR comment say next to a failure.

**Troubleshooting.**

| Symptom | Cause and fix |
|---------|---------------|
| No **Run quests** button on the panel | The anvil is not in the opt-in set. Check `preview_quests: true` under the anvil (not under `settings`) and that previews are enabled for it; the previews payload exposes the opted-in anvils as `quest_anvils`. |
| Run rejected with *"preview … is starting/failed, not healthy"* | Every service must pass its health check before quests run — a half-started stack produces browser failures that say nothing about the branch. Watch the per-service health in the panel and read the failing service's log (`~/.forge/logs/<beadID>/preview-<service>.log`, also reachable from the panel). A service that never goes healthy is usually a wrong `health.path`/`health.port` in `.forge/preview.yaml`, or a `setup` command that failed. |
| Run rejected with *"no preview running for bead …"* | The preview was torn down between the page load and the click — most often by the idle reaper (`preview_idle_timeout`), by its PR merging or closing, or by LRU eviction when `preview_evict_lru` is on. Start it again. |
| Run rejected with *"has no entry service URL"* | The manifest's entry service resolved to nothing to point a browser at. Exactly one service must carry `entry: true` in `.forge/preview.yaml` when the manifest declares more than one; a single-service manifest is the entry implicitly. |
| Quests fail immediately on a healthy preview | The steps are probably not using the placeholder. Only `{{.BaseURL}}` in a step's `url`/`value` is redirected at the preview; a step with a hardcoded `http://localhost:3000` still talks to whatever is on that port, not the preview. |
| No screenshots in the panel or the comment | Screenshots are only captured by explicit `screenshot` steps — a quest without one produces none. Beyond that: the panel serves an image by its position in the run, so a capture whose file was moved or deleted since the run reads as *"screenshot file is gone"*, and only image extensions are served. |
| Screenshots named by path instead of shown in the PR comment | Expected. No screenshot uploader is wired (Forge has no artifact store), so the comment names the file on the Forge host. The panel's thumbnails are the way to look at them. |
| Panel shows no run after a daemon restart | Runs are memory-only by design (see above). The PR comment from the last run survives; the run itself does not. |

**Security note.** A preview served on its raw `preview_bind_host` port is
served by the previewed application itself, not by Hearth, so it is **not**
behind the Hearth login. The default `preview_bind_host: 127.0.0.1` keeps those
ports reachable only from the Forge box. Setting `0.0.0.0` exposes every running
preview to anything that can reach the box on the configured port range — only
do that on a trusted network (LAN, WireGuard). The authenticated alternative is
[host-based routing](#host-based-preview-routing-preview_proxy_base), where
Hearth fronts previews itself and gates them on a session; what each mode
exposes and to whom is spelled out in
[Security posture: what a preview URL exposes](#security-posture-what-a-preview-url-exposes).

**Cost note.** A preview holds a database, an API process and a dev server open
for as long as it runs, which is why `preview_max_concurrent` defaults to `2`
and the idle reaper defaults to 30 minutes. Raise them deliberately.

## Wicket — GitHub Issue Triage

**Wicket** is a background monitor that polls GitHub repositories for new issues, classifies them using an AI provider, and automatically creates beads, requests clarification from the issue author, or flags the issue for human review.

### Push vs Pull Model

Wicket supports two operating modes controlled by `wicket_trigger_label`:

| Model | Configuration | Behavior |
|-------|--------------|-----------|
| **Push** (default) | `wicket_trigger_label: ""` | Wicket processes **all** new issues as they appear, without waiting for a human to label them. Suitable when you want every issue evaluated automatically. |
| **Pull** | `wicket_trigger_label: "forge-triage"` | Wicket only processes issues that carry the specified label. A human (or automation) must apply the label before the issue enters the triage queue. Suitable for high-volume repositories where you want selective intake. |

### Global Settings

These settings live under the top-level `settings` key.

| Field | Default | Description |
|-------|---------|-------------|
| `wicket_enabled` | `false` | Master switch — no issue scanning occurs when false. |
| `wicket_interval` | `15m` | How often Wicket polls each repository for new issues. Non-positive values (e.g. `0`) fall back to the default `15m` interval. |
| `wicket_provider` | `""` (global `providers`) | AI provider used for triage decisions. When empty, the global `providers` chain is used. |
| `wicket_batch_size` | `20` | Maximum number of issues processed per scan cycle per repository. |
| `wicket_processed_label` | `"forge-wicket-processed"` | GitHub label applied to every issue Wicket has triaged (prevents re-processing). |
| `wicket_needs_human_label` | `"forge-needs-human"` | GitHub label applied to issues the AI flagged for human review. |
| `wicket_bead_created_label` | `"forge-bead-created"` | GitHub label applied to issues for which a bead was created. |
| `wicket_trigger_label` | `""` | When non-empty, only issues carrying this label are processed (pull model). |
| `wicket_stale_days` | `14` | Days without an author reply before a clarification request is marked stale. After a further 7 days the issue is closed automatically. |

### Poller / Queue Settings

These settings live under the top-level `settings` key.

| Field | Default | Description |
|-------|---------|-------------|
| `bd_ready_limit` | `100` | Maximum number of beads returned by `bd ready --limit`. Increase if an anvil has more than 100 ready beads. |

### Per-Anvil Settings

These settings are placed under the anvil's key in `anvils`.

| Field | Default | Description |
|-------|---------|-------------|
| `wicket_enabled` | null (global) | Per-anvil override. Set to `false` to opt this anvil out entirely. |
| `wicket_repos` | `[]` | `"owner/repo"` strings to scan for this anvil. When empty, the primary repository is inferred from the anvil's git remote. |
| `wicket_trusted_users` | `[]` | GitHub logins whose issues are automatically dispatched without extra human review. |
| `wicket_auto_dispatch` | `false` | When true, beads created by Wicket for this anvil are auto-dispatched without manual approval. |
| `wicket_issue_labels` | `[]` | Label filter — an issue must carry all of these labels to be eligible. Empty means all issues are eligible. |
| `wicket_ignore_users` | `[]` | GitHub logins to skip entirely. Known bot accounts (dependabot, renovate, etc.) are always ignored regardless of this list. |
| `wicket_triage_prompt` | `""` | Optional text appended to the default triage system prompt, for project-specific context or constraints. |
| `stage_providers` | `{}` | Per-anvil stage provider overrides. Same keys/format as global `stage_providers`. Resolution: anvil `stage_providers[stage]` → global `stage_providers[stage]` → `smith_providers` → `providers` for `smith`/`warden`/`schematic`; anvil `stage_providers[stage]` → global `stage_providers[stage]` → `providers` for `cifix`/`reviewfix`. |

### `wicket_repos` — Multi-Repo Scanning

By default Wicket resolves the target repository from the anvil's git remote. When `wicket_repos` is set, it **replaces** remote inference entirely — the listed repositories are scanned instead. Include the primary repo in the list if you still want it scanned alongside any extras.

```yaml
anvils:
  my-service:
    path: /repos/my-service
    wicket_repos:
      - myorg/my-service          # the service repo itself
      - myorg/my-service-issues   # dedicated issue tracker
      - myorg/shared-platform     # cross-team issues that affect this service
```

### Example Configurations

#### Minimal — single repo, push model

```yaml
settings:
  wicket_enabled: true
  wicket_interval: 15m

anvils:
  my-api:
    path: /repos/my-api
    wicket_auto_dispatch: true
```

#### Multi-Repo — dedicated issue tracker + trusted contributors

```yaml
settings:
  wicket_enabled: true
  wicket_interval: 10m
  wicket_batch_size: 50

anvils:
  platform:
    path: /repos/platform
    wicket_repos:
      - myorg/platform
      - myorg/platform-issues
    wicket_trusted_users:
      - alice
      - bob
    wicket_auto_dispatch: true
    wicket_ignore_users:
      - legacy-bot
```

#### Trigger-Label — pull model with selective intake

```yaml
settings:
  wicket_enabled: true
  wicket_trigger_label: forge-triage   # only labelled issues are processed

anvils:
  legacy:
    path: /repos/legacy
    wicket_issue_labels:
      - bug
      - enhancement
    wicket_triage_prompt: |
      This is a legacy codebase. Prefer conservative, low-risk changes.
      Only create beads for issues labelled 'bug' or 'enhancement'.
```

## Notifications

Forge supports two styles of webhook notifications:

1. **MS Teams** — Rich Adaptive Cards with color-coded severity, configured under `notifications.teams` (or the legacy `notifications.teams_webhook_url` field).
2. **Generic webhooks** — Simple JSON payloads (`event_type`, `bead_id`, `anvil`, `message`, `timestamp`) delivered to any HTTP endpoint, configured under `notifications.webhooks`.

### Example Configuration

```yaml
notifications:
  enabled: true

  # MS Teams (Adaptive Card format)
  teams:
    webhook_url: 'https://outlook.webhook.office.com/webhookb2/...'
    events: [bead_failed, daily_cost, pr_ready_to_merge]  # empty = all

  # Generic JSON webhooks (one or more targets)
  webhooks:
    - name: dashboard
      url: 'https://example.com/api/webhooks/forge'
      events: [pr_created, worker_done, release]  # empty = all
    - name: slack
      url: 'https://hooks.slack.com/services/...'
      events: [bead_failed]
```

### Top-Level Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Master switch — disables all notifications when false. |
| `teams_webhook_url` | string | | Legacy flat-style Teams webhook URL. Use `teams.webhook_url` instead. |
| `events` | []string | `[]` (all) | Legacy flat-style Teams event filter. Use `teams.events` instead. |
| `release_webhook_urls` | []string | | Legacy list of generic-JSON URLs for `release_published` events. |
| `pr_ready_webhook_urls` | []string | | Legacy list of generic-JSON URLs for `pr_ready_to_merge` events. |

### `notifications.teams`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `webhook_url` | string | | Teams incoming webhook URL (HTTPS required). Overrides `teams_webhook_url`. |
| `events` | []string | `[]` (all) | Event filter. Overrides the top-level `events` field. |

### `notifications.webhooks[]`

Each entry in the list defines a generic JSON webhook target.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | | Friendly name for logging. |
| `url` | string | | Target URL (HTTPS recommended). |
| `events` | []string | `[]` (all) | Event filter. Empty means subscribe to all events. |

### Supported Events

| Event | Teams | Generic | Description |
|-------|-------|---------|-------------|
| `pr_created` | ✓ | ✓ | A pull request was created. |
| `bead_failed` | ✓ | ✓ | A bead exhausted retries and needs human intervention. |
| `daily_cost` | ✓ | ✓ | Daily token usage and cost summary. |
| `worker_done` | ✓ | ✓ | A worker successfully completed its pipeline. |
| `bead_decomposed` | ✓ | ✓ | Schematic split a bead into sub-beads; the parent is now blocked. |
| `pr_ready_to_merge` | ✓ | ✓ | A PR passed CI and warden approval and is ready to merge. |
| `anvil_wedged` | — | ✓ | An anvil's beads database was found mid-merge with unresolved conflicts; every `bd` write against it is rolled back until a human resolves it. Sent once per wedge. |
| `anvil_recovered` | — | ✓ | A previously wedged anvil accepts work again. |
| `release_published` | ✓ | — | New Forge release (Teams Adaptive Card, via `forge notify release`). |
| `release` | — | ✓ | New Forge release (GenericPayload, via `forge notify release`). |

Teams webhooks receive rich Adaptive Cards. Generic webhook targets receive a uniform JSON object:

```json
{
  "event_type": "pr_created",
  "bead_id": "Forge-42",
  "anvil": "my-api",
  "message": "PR #7 created: https://github.com/org/repo/pull/7",
  "timestamp": "2026-03-11T10:00:00Z"
}
```

## Environment Variable Overrides

Environment variables with the `FORGE_` prefix override YAML values. Nested keys use underscores as separators. Every scalar `settings.*` key can be overridden this way — uppercase the dotted key path and replace each `.` with `_` (e.g. `settings.temper_output_cap` → `FORGE_SETTINGS_TEMPER_OUTPUT_CAP`, `settings.forgechat.turn_timeout` → `FORGE_SETTINGS_FORGECHAT_TURN_TIMEOUT`). The table below lists common overrides; it is not exhaustive. The `pricing` and `copilot_premium_multipliers` maps are the exception — they are loaded directly from the YAML file (not viper), so they cannot be set via environment variables.

| Variable | Overrides |
|----------|-----------|
| `FORGE_SETTINGS_POLL_INTERVAL` | `settings.poll_interval` |
| `FORGE_SETTINGS_SMITH_TIMEOUT` | `settings.smith_timeout` |
| `FORGE_SETTINGS_MAX_TOTAL_SMITHS` | `settings.max_total_smiths` |
| `FORGE_SETTINGS_MAX_PIPELINE_ITERATIONS` | `settings.max_pipeline_iterations` |
| `FORGE_SETTINGS_MAX_REVIEW_ATTEMPTS` | `settings.max_review_attempts` |
| `FORGE_SETTINGS_RATE_LIMIT_BACKOFF` | `settings.rate_limit_backoff` |
| `FORGE_SETTINGS_SCHEMATIC_ENABLED` | `settings.schematic_enabled` |
| `FORGE_SETTINGS_SCHEMATIC_WORD_THRESHOLD` | `settings.schematic_word_threshold` |
| `FORGE_SETTINGS_BELLOWS_INTERVAL` | `settings.bellows_interval` |
| `FORGE_SETTINGS_DAILY_COST_LIMIT` | `settings.daily_cost_limit` |
| `FORGE_SETTINGS_PER_WORKER_COST_ESTIMATE` | `settings.per_worker_cost_estimate` |
| `FORGE_SETTINGS_COPILOT_DAILY_REQUEST_LIMIT` | `settings.copilot_daily_request_limit` |
| `FORGE_SETTINGS_MAX_CI_FIX_ATTEMPTS` | `settings.max_ci_fix_attempts` |
| `FORGE_SETTINGS_MAX_REVIEW_FIX_ATTEMPTS` | `settings.max_review_fix_attempts` |
| `FORGE_SETTINGS_MAX_SAME_HEAD_REVIEW_FIXES` | `settings.max_same_head_review_fixes` |
| `FORGE_SETTINGS_MAX_REBASE_ATTEMPTS` | `settings.max_rebase_attempts` |
| `FORGE_SETTINGS_MAX_LIFECYCLE_WORKERS` | `settings.max_lifecycle_workers` |
| `FORGE_SETTINGS_BURNISH_VERIFY_TIMEOUT` | `settings.burnish_verify_timeout` |
| `FORGE_SETTINGS_BURNISH_VERIFY_RETRIES` | `settings.burnish_verify_retries` |
| `FORGE_SETTINGS_MERGE_STRATEGY` | `settings.merge_strategy` |
| `FORGE_SETTINGS_EMPTY_DIFF_ACTION` | `settings.empty_diff_action` |
| `FORGE_SETTINGS_STALE_INTERVAL` | `settings.stale_interval` |
| `FORGE_SETTINGS_TEMPER_STEP_TIMEOUT` | `settings.temper_step_timeout` |
| `FORGE_SETTINGS_TEMPER_GIT_TIMEOUT` | `settings.temper_git_timeout` |
| `FORGE_SETTINGS_WORKTREE_GIT_TIMEOUT` | `settings.worktree_git_timeout` |
| `FORGE_SETTINGS_BD_TIMEOUT` | `settings.bd_timeout` |
| `FORGE_SETTINGS_TEMPER_OUTPUT_CAP` | `settings.temper_output_cap` |
| `FORGE_SETTINGS_DEPCHECK_INTERVAL` | `settings.depcheck_interval` |
| `FORGE_SETTINGS_DEPCHECK_TIMEOUT` | `settings.depcheck_timeout` |
| `FORGE_SETTINGS_VULNCHECK_ENABLED` | `settings.vulncheck_enabled` |
| `FORGE_SETTINGS_VULNCHECK_INTERVAL` | `settings.vulncheck_interval` |
| `FORGE_SETTINGS_VULNCHECK_TIMEOUT` | `settings.vulncheck_timeout` |
| `FORGE_SETTINGS_ANVIL_HEALTH_CHECK` | `settings.anvil_health_check` |
| `FORGE_SETTINGS_LOG_RETENTION_DAYS` | `settings.log_retention_days` |
| `FORGE_SETTINGS_LOG_SWEEP_INTERVAL` | `settings.log_sweep_interval` |
| `FORGE_SETTINGS_AUTO_LEARN_RULES` | `settings.auto_learn_rules` |
| `FORGE_SETTINGS_SMELTER_ENABLED` | `settings.smelter_enabled` |
| `FORGE_SETTINGS_SMELTER_INTERVAL` | `settings.smelter_interval` |
| `FORGE_SETTINGS_GO_RACE_DETECTION` | `settings.go_race_detection` |
| `FORGE_SETTINGS_CRUCIBLE_ENABLED` | `settings.crucible_enabled` |
| `FORGE_SETTINGS_CRUCIBLE_POLL_INTERVAL` | `settings.crucible_poll_interval` |
| `FORGE_SETTINGS_BUS_ENABLED` | `settings.bus_enabled` |
| `FORGE_SETTINGS_BUS_BUFFER_SIZE` | `settings.bus_buffer_size` |
| `FORGE_SETTINGS_SSE_POLL_FALLBACK` | `settings.sse_poll_fallback` |
| `FORGE_SETTINGS_AUTO_MERGE_CRUCIBLE_CHILDREN` | `settings.auto_merge_crucible_children` |
| `FORGE_SETTINGS_QUESTGIVER_ENABLED` | `settings.questgiver_enabled` |
| `FORGE_SETTINGS_QUESTGIVER_INTERVAL` | `settings.questgiver_interval` |
| `FORGE_SETTINGS_ADVENTURER_TIMEOUT` | `settings.adventurer_timeout` |
| `FORGE_SETTINGS_PREVIEW_ENABLED` | `settings.preview_enabled` |
| `FORGE_SETTINGS_PREVIEW_MAX_CONCURRENT` | `settings.preview_max_concurrent` |
| `FORGE_SETTINGS_PREVIEW_EVICT_LRU` | `settings.preview_evict_lru` |
| `FORGE_SETTINGS_PREVIEW_IDLE_TIMEOUT` | `settings.preview_idle_timeout` |
| `FORGE_SETTINGS_PREVIEW_PORT_RANGE` | `settings.preview_port_range` |
| `FORGE_SETTINGS_PREVIEW_BIND_HOST` | `settings.preview_bind_host` |
| `FORGE_SETTINGS_PREVIEW_PUBLIC_HOST` | `settings.preview_public_host` |
| `FORGE_SETTINGS_PREVIEW_PROXY_BASE` | `settings.preview_proxy_base` |
| `FORGE_SETTINGS_PREVIEW_PROXY_AUTH` | `settings.preview_proxy_auth` |
| `FORGE_SETTINGS_WICKET_ENABLED` | `settings.wicket_enabled` |
| `FORGE_SETTINGS_WICKET_INTERVAL` | `settings.wicket_interval` |
| `FORGE_SETTINGS_WICKET_PROVIDER` | `settings.wicket_provider` |
| `FORGE_SETTINGS_WICKET_BATCH_SIZE` | `settings.wicket_batch_size` |
| `FORGE_SETTINGS_WICKET_PROCESSED_LABEL` | `settings.wicket_processed_label` |
| `FORGE_SETTINGS_WICKET_NEEDS_HUMAN_LABEL` | `settings.wicket_needs_human_label` |
| `FORGE_SETTINGS_WICKET_BEAD_CREATED_LABEL` | `settings.wicket_bead_created_label` |
| `FORGE_SETTINGS_WICKET_TRIGGER_LABEL` | `settings.wicket_trigger_label` |
| `FORGE_SETTINGS_WICKET_STALE_DAYS` | `settings.wicket_stale_days` |
| `FORGE_SETTINGS_BD_READY_LIMIT` | `settings.bd_ready_limit` |
| `FORGE_SETTINGS_FORGE_ID` | `settings.forge_id` |
| `FORGE_SETTINGS_WARDEN_MODEL_OVERRIDE` | `settings.warden_model_override` |
| `FORGE_SETTINGS_SCHEMATIC_MODEL_OVERRIDE` | `settings.schematic_model_override` |
| `FORGE_NOTIFICATIONS_ENABLED` | `notifications.enabled` |
| `FORGE_NOTIFICATIONS_TEAMS_WEBHOOK_URL` | `notifications.teams_webhook_url` |

Duration values from environment variables are parsed as Go duration strings (e.g., `"5m"`, `"30s"`).

Per-anvil configuration is best managed in the YAML file, as the flat environment variable namespace doesn't map cleanly to the nested `anvils` map.

## Validation Rules

The config is validated at load time. Errors are reported as a list:

- `max_total_smiths` must be >= 1
- `max_pipeline_iterations` must be >= 1
- `max_review_attempts` must be >= 1
- `max_ci_fix_attempts` must be >= 1
- `max_review_fix_attempts` must be >= 1
- `max_rebase_attempts` must be >= 1
- `max_lifecycle_workers` must not be negative (omit or set to 0 to use the default)
- `max_same_head_review_fixes` must not be negative (omit or set to 0 to use the default)
- `burnish_verify_timeout` must not be negative, and must be >= 30s when set explicitly (omit or set to 0 to use the package default)
- `poll_interval` must be >= 10s
- `smith_timeout` must be >= 1m
- `bellows_interval` must be >= 30s
- `daily_cost_limit` must be a non-negative finite number
- `per_worker_cost_estimate` must be a non-negative finite number (omit or set to 0 to use the default)
- `copilot_daily_request_limit` must be >= 0 (0 = no limit)
- `copilot_warden_sample_rate` must be a finite value in [0.0, 1.0]
- `log_retention_days` must be >= 0 (0 disables the log retention sweep)
- `stale_interval` must be >= 30s when enabled, or 0 to disable
- `smelter_interval` must be >= 1h when enabled, or 0 to disable
- `questgiver_interval` must be > 0 when questgiver is enabled, or 0 to disable
- `adventurer_timeout` must not be negative
- `preview_max_concurrent` must not be negative (omit or set to 0 to use the default)
- `preview_idle_timeout` must be >= 1m when enabled, or 0 to disable the idle reaper
- `preview_port_range` must be `"min-max"` with both ends within 1024-65535 and min < max
- `depcheck_interval` must be >= 1h when enabled, or 0 to disable
- `depcheck_timeout` must not be negative
- `crucible_poll_interval` must be >= 30s when enabled, or 0 to disable two-tier polling
- Each anvil `path` must be non-empty
- Each anvil `platform` (if set) must be one of: `github`, `gitlab`, `gitea`, `bitbucket`, `azuredevops` (note: `bitbucket` and `azuredevops` are recognised by the validator but not yet implemented)
- Each anvil `max_smiths` must be >= 0
- `auto_dispatch` must be one of: `all`, `tagged`, `priority`, `off`
- If `auto_dispatch: tagged`, then `auto_dispatch_tag` must be non-empty
- If `auto_dispatch: priority`, then `auto_dispatch_min_priority` must be 0-4
- If an anvil sets `temper.lint_required: true`, then `temper.lint` (or `temper.steps`) must be set
- Within `temper.steps`: each step `name` must be non-empty and unique; each `command` must be non-empty unless `verify_no_conflict_markers` is set (scan-only step); each step `timeout` must be non-negative
- If `self_deploy.enabled` is true, `self_deploy.anvil` must be non-empty and match a configured anvil
- `self_deploy.max_drain_wait` must not be negative (omit or set to 0 to use the 30m default); the deprecated `self_deploy.drain_timeout` is validated the same way

## Hot Reload

The daemon watches `forge.yaml` via fsnotify. When the file changes, **only a subset of settings are hot-reloaded**:

- `poll_interval` is re-read and the new value takes effect on the next cycle
- `smith_timeout` is re-read and used for newly started smiths
- `max_total_smiths` is re-read and applied to subsequent scheduling decisions
- `max_lifecycle_workers` is re-read and applied to subsequent lifecycle fix-worker dispatches
- `claude_flags` are re-read and used for newly started smiths
- `smith_providers` and `stage_providers` are re-read and used for newly dispatched beads
- `empty_diff_action` is re-read per dispatch and applies to beads dispatched after the change
- `copilot_combined_smith_warden` toggles combined Smith+Warden mode at runtime
- `copilot_warden_sample_rate` adjusts the sampling rate at runtime
- `smelter_enabled` enables or disables the Smelter background process at runtime
- `smelter_interval` changes the Smelter schedule; takes effect on the next scheduled run
- `pricing` and `copilot_premium_multipliers` are re-applied to the cost estimator for subsequent cost calculations
- `notifications.*` (webhook URL, enabled, events, etc.) are re-read and applied immediately
- `anvils.<name>.preview_enabled` is re-read per preview start, so the next `forge preview start` (or Preview button) obeys the new value
- `anvils.<name>.preview_auto` is re-read on the next ready-to-merge transition
- `anvils.<name>.preview_quests` is re-read per quest run
- Other per-anvil keys the daemon applies live: `auto_merge` (next ready-to-merge transition), `max_smiths`, `path`, `stage_providers`, `assay.*`, plus adding or removing an anvil entry
- In-flight workers are **not** interrupted

All other configuration changes (including `providers`, `rate_limit_backoff`, `daily_cost_limit`, `merge_strategy`, per-anvil keys not listed above, and scheduling fields not listed above) **require a daemon restart** to take effect.

### Changes that need a restart are reported

A reload that touches a setting the daemon read once, at startup, logs a WARN
naming it rather than dropping it silently:

```
WARN config change requires a daemon restart to take effect keys="settings.preview_enabled: false → true"
```

The Kiln globals are named individually, because that is where the boundary
bites in practice — the preview manager, its port allocator and its idle reaper
are all built once from these values:

| Restart-only setting | Why |
|---|---|
| `settings.preview_enabled` | The preview manager is constructed at startup only when this is on (and at least one anvil opts in); with it off there is nothing to hand a later opt-in to |
| `settings.preview_port_range` | The port allocator holds live allocation state |
| `settings.preview_bind_host` | Baked into the allocator and into every running service's manifest expansion |
| `settings.preview_public_host` | Baked into running services' manifest expansion (displayed links do follow the new value) |
| `settings.preview_max_concurrent`, `settings.preview_evict_lru`, `settings.preview_idle_timeout` | Captured by the manager and its reaper at construction |

`settings.preview_proxy_base` and `settings.preview_proxy_auth` are **not** on
that list: the web layer reads both live, so host-based routing and the preview
auth mode do follow a reload.

Any other edit that changes the file without moving a hot-reloadable setting
gets a generic line — an ignored edit never passes without a trace:

```
WARN config file changed but no hot-reloadable setting differs; a daemon restart is required to apply the edit
```

One more case has its own warning: enabling `preview_enabled` for an anvil on a
daemon that started with previews off for *every* anvil. The config change
applies, but no preview manager was built to serve it, so previews still refuse
until a restart:

```
WARN anvil(s) opted into Kiln previews but no preview manager is running; the manager is built at startup, so a daemon restart is required anvils=munin
```
