// Package config handles loading and validating Forge configuration from
// forge.yaml files and environment variable overrides.
//
// Config resolution order (first found wins):
//  1. --config flag (explicit path)
//  2. ./forge.yaml (working directory)
//  3. ~/.forge/config.yaml (user home)
//
// Environment variables override file values with the FORGE_ prefix:
//
//	FORGE_SETTINGS_POLL_INTERVAL=60s
//	FORGE_SETTINGS_MAX_TOTAL_SMITHS=4
package config

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/vcs"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for The Forge.
type Config struct {
	Anvils        map[string]AnvilConfig `mapstructure:"anvils" yaml:"anvils"`
	Settings      SettingsConfig         `mapstructure:"settings" yaml:"settings"`
	Notifications NotificationsConfig    `mapstructure:"notifications" yaml:"notifications,omitempty"`
}

// AnvilConfig defines a registered repository (anvil).
type AnvilConfig struct {
	Path                    string `mapstructure:"path" yaml:"path"`
	MaxSmiths               int    `mapstructure:"max_smiths" yaml:"max_smiths"`
	AutoDispatch            string `mapstructure:"auto_dispatch" yaml:"auto_dispatch"`
	AutoDispatchTag         string `mapstructure:"auto_dispatch_tag" yaml:"auto_dispatch_tag,omitempty"`
	AutoDispatchMinPriority int    `mapstructure:"auto_dispatch_min_priority" yaml:"auto_dispatch_min_priority"`
	// Platform specifies the VCS hosting platform for this anvil.
	// Valid values: "github" (default), "gitlab", "gitea", "bitbucket", "azuredevops".
	// Determines which VCS provider is used for PR operations.
	Platform string `mapstructure:"platform" yaml:"platform,omitempty"`
	// SchematicEnabled controls whether the Schematic pre-worker runs for
	// beads in this anvil. When nil, the global setting is used. Set to
	// a pointer to false to disable per-anvil.
	SchematicEnabled *bool `mapstructure:"schematic_enabled" yaml:"schematic_enabled,omitempty"`
	// GolangciLint controls whether golangci-lint runs as a Temper step
	// for Go projects. When nil (default), golangci-lint runs if the
	// binary is found on PATH. Set to a pointer to false to disable.
	GolangciLint *bool `mapstructure:"golangci_lint" yaml:"golangci_lint,omitempty"`
	// GoRaceDetection enables the Go race detector (-race flag) as a
	// separate temper step for this anvil. When nil, the global setting
	// is used. Default off since -race slows tests and increases memory.
	GoRaceDetection *bool `mapstructure:"go_race_detection" yaml:"go_race_detection,omitempty"`
	// DepcheckEnabled controls whether the depcheck monitor scans this
	// anvil for outdated dependencies. When nil (default), depcheck runs
	// as normal (opt-out). Set to false to skip this anvil entirely.
	DepcheckEnabled *bool `mapstructure:"depcheck_enabled" yaml:"depcheck_enabled,omitempty"`
	// QuestgiverEnabled controls whether the QuestGiver monitor runs
	// quests for this anvil. When nil (default), the global setting is used.
	// Set to false to skip this anvil entirely.
	QuestgiverEnabled *bool `mapstructure:"questgiver_enabled" yaml:"questgiver_enabled,omitempty"`
	// AutoMerge enables automatic merging of PRs when they reach the
	// ready-to-merge state (CI passing, no conflicts, no unresolved
	// threads, no pending reviews). External PRs (ext-*) are never
	// auto-merged. Default: false.
	AutoMerge bool `mapstructure:"auto_merge" yaml:"auto_merge,omitempty"`
	// QuestgiverSetupCmd is a shell command to run before quest execution
	// for this anvil (e.g. "podman compose up -d").
	QuestgiverSetupCmd string `mapstructure:"questgiver_setup_cmd" yaml:"questgiver_setup_cmd,omitempty"`
	// QuestgiverTeardownCmd is a shell command to run after quest execution
	// for this anvil (e.g. "podman compose down").
	QuestgiverTeardownCmd string `mapstructure:"questgiver_teardown_cmd" yaml:"questgiver_teardown_cmd,omitempty"`

	// Temper holds optional custom build/test/lint commands for this anvil.
	// When any field is set, Forge disables the auto-detected Temper steps
	// for this anvil and runs only the commands explicitly configured here.
	// This enables support for Python, Rust, or repos with non-standard
	// build tooling. Each value is a command string (e.g. "make build",
	// "cargo test"). Commands are split on whitespace; for shell features,
	// invoke a checked-in wrapper script from the configured command.
	Temper *TemperCommandsConfig `mapstructure:"temper" yaml:"temper,omitempty"`

	// WicketEnabled controls whether the Wicket issue triage monitor scans
	// this anvil. When nil (default), the global WicketEnabled setting is
	// used. Set to false to opt this anvil out entirely.
	WicketEnabled *bool `mapstructure:"wicket_enabled" yaml:"wicket_enabled,omitempty"`
	// WicketTrustedUsers is the list of GitHub logins whose issues are
	// automatically dispatched without extra review for this anvil.
	WicketTrustedUsers []string `mapstructure:"wicket_trusted_users" yaml:"wicket_trusted_users,omitempty"`
	// WicketAutoDispatch, when true, automatically dispatches triaged beads
	// for this anvil without waiting for manual queue approval.
	WicketAutoDispatch bool `mapstructure:"wicket_auto_dispatch" yaml:"wicket_auto_dispatch,omitempty"`
	// WicketIssueLabels is the list of GitHub label names that an issue must
	// carry for Wicket to consider it eligible for triage in this anvil.
	// An empty list means all issues are eligible (subject to WicketTriggerLabel).
	WicketIssueLabels []string `mapstructure:"wicket_issue_labels" yaml:"wicket_issue_labels,omitempty"`
	// WicketRepos is the list of "owner/repo" strings Wicket scans for this
	// anvil. When empty, the anvil's primary repository is derived from its
	// git remote.
	WicketRepos []string `mapstructure:"wicket_repos" yaml:"wicket_repos,omitempty"`
	// WicketTriagePrompt is an optional prompt suffix appended to the default
	// Wicket triage system prompt, allowing project-specific context or
	// constraints to be injected.
	WicketTriagePrompt string `mapstructure:"wicket_triage_prompt" yaml:"wicket_triage_prompt,omitempty"`
	// WicketIgnoreUsers is the list of GitHub logins to skip entirely when
	// triaging issues for this anvil. In addition to this list, a set of
	// well-known bot accounts (dependabot[bot], renovate[bot], etc.) is
	// always ignored. Comparison is case-insensitive.
	WicketIgnoreUsers []string `mapstructure:"wicket_ignore_users" yaml:"wicket_ignore_users,omitempty"`

	// StageProviders is a per-anvil override for stage_providers. When set,
	// these take precedence over the global stage_providers for beads in this
	// anvil. Same keys/format as settings.stage_providers.
	StageProviders map[string][]string `mapstructure:"stage_providers" yaml:"stage_providers,omitempty"`

	// Smith holds optional Smith configuration for this anvil, including
	// deny patterns for files and commands.
	Smith *SmithConfig `mapstructure:"smith" yaml:"smith,omitempty"`

	// Hooks defines optional shell commands to run before/after each
	// pipeline stage. Commands receive context as environment variables
	// (FORGE_BEAD_ID, FORGE_WORKTREE_PATH, FORGE_BRANCH, FORGE_ANVIL_NAME,
	// FORGE_ANVIL_PATH, FORGE_STAGE, FORGE_ITERATION). A non-zero exit
	// from a "before" hook aborts the stage; "after" hook failures are
	// logged but do not abort the pipeline.
	Hooks *HooksConfig `mapstructure:"hooks" yaml:"hooks,omitempty"`
}

// SmithConfig holds per-anvil Smith configuration.
type SmithConfig struct {
	// DenyPatterns defines file and command patterns that Smith is not
	// allowed to modify or execute. Violations are detected post-Smith
	// via diff validation and cause the pipeline to fail the iteration.
	DenyPatterns *DenyPatternsConfig `mapstructure:"deny_patterns" yaml:"deny_patterns,omitempty"`
}

// DenyPatternsConfig holds glob patterns for files and commands that Smith
// must not touch. File patterns are matched against changed file paths in
// the diff (using filepath.Match semantics). Command patterns are matched
// against bash commands found in Smith's output log.
type DenyPatternsConfig struct {
	// Files is a list of glob patterns matched against file paths in the
	// diff output. Examples: "*.env", ".forge/*", "*.key", "*.pem".
	Files []string `mapstructure:"files" yaml:"files,omitempty"`
	// Commands is a list of glob patterns matched against bash commands
	// executed by Smith. Examples: "rm -rf /", "git push --force*".
	Commands []string `mapstructure:"commands" yaml:"commands,omitempty"`
}

// HooksConfig defines optional shell commands that run before/after each
// pipeline stage. Each field is a shell command string executed via a
// platform-appropriate shell (sh -c on Unix, cmd /c on Windows).
// Commands receive pipeline context as environment variables.
type HooksConfig struct {
	BeforeSchematic string `mapstructure:"before_schematic" yaml:"before_schematic,omitempty"`
	AfterSchematic  string `mapstructure:"after_schematic" yaml:"after_schematic,omitempty"`
	BeforeSmith     string `mapstructure:"before_smith" yaml:"before_smith,omitempty"`
	AfterSmith      string `mapstructure:"after_smith" yaml:"after_smith,omitempty"`
	BeforeTemper    string `mapstructure:"before_temper" yaml:"before_temper,omitempty"`
	AfterTemper     string `mapstructure:"after_temper" yaml:"after_temper,omitempty"`
	BeforeWarden    string `mapstructure:"before_warden" yaml:"before_warden,omitempty"`
	AfterWarden     string `mapstructure:"after_warden" yaml:"after_warden,omitempty"`
}

// TemperCommandsConfig holds custom build/test/lint commands for an anvil.
// When set on an AnvilConfig, these commands replace auto-detected Temper steps.
type TemperCommandsConfig struct {
	// Build is the build command (e.g. "make build", "cargo build").
	Build string `mapstructure:"build" yaml:"build,omitempty"`
	// Test is the test command (e.g. "make test", "pytest").
	Test string `mapstructure:"test" yaml:"test,omitempty"`
	// Lint is the lint command (e.g. "make lint", "ruff check .").
	Lint string `mapstructure:"lint" yaml:"lint,omitempty"`
	// LintRequired makes lint failures fail the temper run instead of warning.
	// Default false preserves legacy behavior where lint is advisory-only.
	LintRequired bool `mapstructure:"lint_required" yaml:"lint_required,omitempty"`
	// Steps is an ordered list of named verification steps. When non-empty,
	// it takes precedence over Build/Test/Lint. Each step runs in input order;
	// a required step failure stops the run.
	Steps []TemperStepConfig `mapstructure:"steps" yaml:"steps,omitempty"`
}

// TemperStepConfig defines a single named step in the temper.steps list.
type TemperStepConfig struct {
	// Name identifies the step in logs, summaries, and failure events.
	Name string `mapstructure:"name" yaml:"name"`
	// Command is the executable to run (not shell-interpreted).
	Command string `mapstructure:"command" yaml:"command"`
	// Args are the command arguments.
	Args []string `mapstructure:"args" yaml:"args,omitempty"`
	// Dir is the working directory for the step. Relative paths resolve
	// against the worktree root; absolute paths are used as-is.
	Dir string `mapstructure:"dir" yaml:"dir,omitempty"`
	// Timeout is the per-step timeout. Defaults to 5m when zero.
	Timeout time.Duration `mapstructure:"timeout" yaml:"timeout,omitempty"`
	// Required controls whether failure fails the whole temper run.
	// Pointer so we can distinguish unset (defaults to true) from explicit false.
	Required *bool `mapstructure:"required" yaml:"required,omitempty"`
	// Paths is an optional list of glob patterns (doublestar syntax, e.g.
	// "client/**", "*.cs"). When set, the step is skipped if none of the
	// changed files in the diff match any pattern. When empty or nil the
	// step always runs (backward compatible).
	Paths []string `mapstructure:"paths" yaml:"paths,omitempty"`
	// VerifyClean is an optional list of pathspecs (relative to the worktree)
	// that must remain clean after the step runs. When the step succeeds but
	// `git status --porcelain -- <pathspecs>` reports any changes, the step
	// is converted to a failure. Use this to enforce that committed build
	// artifacts (e.g. an embedded frontend bundle) match a fresh build of
	// the source.
	VerifyClean []string `mapstructure:"verify_clean" yaml:"verify_clean,omitempty"`
	// VerifyNoConflictMarkers is an optional list of pathspecs (relative to
	// the worktree) that must not contain git merge-conflict markers
	// (`<<<<<<<`, `=======`, `>>>>>>>` at line start). When set on a step
	// with no Command, temper performs a cheap scan-only check that runs
	// unconditionally (no Paths gating) — complementary to verify_clean,
	// which depends on a rebuild and can miss markers committed directly
	// into build output.
	VerifyNoConflictMarkers []string `mapstructure:"verify_no_conflict_markers" yaml:"verify_no_conflict_markers,omitempty"`
}

// IsEmpty returns true if no custom commands are configured.
func (t *TemperCommandsConfig) IsEmpty() bool {
	return t == nil || (t.Build == "" && t.Test == "" && t.Lint == "" && len(t.Steps) == 0)
}

// SettingsConfig holds global operational settings.
type SettingsConfig struct {
	PollInterval      time.Duration `mapstructure:"poll_interval" yaml:"poll_interval"`
	SmithTimeout      time.Duration `mapstructure:"smith_timeout" yaml:"smith_timeout"`
	MaxTotalSmiths    int           `mapstructure:"max_total_smiths" yaml:"max_total_smiths"`
	MaxReviewAttempts int           `mapstructure:"max_review_attempts" yaml:"max_review_attempts"`
	// MaxPipelineIterations is the maximum number of Smith-Warden cycles
	// in the initial pipeline loop before declaring failure. This controls
	// how many times Smith can be asked to revise its implementation based
	// on Temper or Warden feedback during a single bead run. Default: 5.
	MaxPipelineIterations int      `mapstructure:"max_pipeline_iterations" yaml:"max_pipeline_iterations"`
	ClaudeFlags           []string `mapstructure:"claude_flags" yaml:"claude_flags"`
	// Providers is the ordered list of AI providers to try.
	// Each entry is a Kind string ("claude", "gemini") or "kind:command" pair.
	// When a provider signals a rate limit the next one in the list is tried.
	// Defaults to ["claude", "gemini"] when empty.
	Providers []string `mapstructure:"providers" yaml:"providers,omitempty"`
	// RateLimitBackoff is how long dispatchBead waits after releasing a bead
	// back to open when all providers are rate limited. During this window the
	// bead slot stays reserved (activeBeads) so the poller does not
	// immediately re-claim it. Defaults to 5 minutes.
	RateLimitBackoff time.Duration `mapstructure:"rate_limit_backoff" yaml:"rate_limit_backoff"`
	// SmithProviders is the ordered list of AI providers used specifically for
	// dispatch pipeline (Smith + Warden + Schematic). When empty, Providers is
	// used as fallback. This lets smiths run a more capable model (e.g.
	// claude/claude-opus-4-6) while lifecycle workers (quench, burnish) use a
	// lighter model. Accepts the same "kind/model" format as Providers.
	//
	// Deprecated: Use StageProviders for per-stage configuration. SmithProviders
	// is still honoured as a fallback for smith/warden/schematic when the
	// corresponding StageProviders key is not set.
	SmithProviders []string `mapstructure:"smith_providers" yaml:"smith_providers,omitempty"`
	// StageProviders maps pipeline stage names to their own provider chains.
	// Supported keys: "smith", "warden", "schematic", "cifix" (quench), "reviewfix" (burnish).
	// Each value uses the same "kind/model" format as Providers. When a stage
	// key is missing, the fallback chain is:
	//   stage_providers[stage] → smith_providers (smith/warden/schematic only) → providers → defaults
	StageProviders map[string][]string `mapstructure:"stage_providers" yaml:"stage_providers,omitempty"`
	// SchematicEnabled enables the Schematic pre-worker globally. When true,
	// beads that exceed the word threshold or carry the "decompose" tag are
	// analysed before Smith starts. Default: false.
	SchematicEnabled bool `mapstructure:"schematic_enabled" yaml:"schematic_enabled"`
	// SchematicWordThreshold is the minimum word count in a bead description
	// to trigger automatic schematic analysis. When this value is zero or
	// unset, the daemon applies an effective default of 100.
	SchematicWordThreshold int `mapstructure:"schematic_word_threshold" yaml:"schematic_word_threshold,omitempty"`
	// BellowsInterval is how often the Bellows PR monitor polls GitHub for
	// status changes on open PRs. Defaults to 2 minutes.
	BellowsInterval time.Duration `mapstructure:"bellows_interval" yaml:"bellows_interval"`
	// DailyCostLimit is the maximum estimated USD spend per calendar day.
	// When the running total exceeds this value, auto-dispatch is paused until
	// the next calendar day. Zero means no limit (default).
	DailyCostLimit float64 `mapstructure:"daily_cost_limit" yaml:"daily_cost_limit,omitempty"`
	// MaxCIFixAttempts is the maximum number of CI fix cycles per PR before
	// the PR is considered exhausted. Default: 5.
	MaxCIFixAttempts int `mapstructure:"max_ci_fix_attempts" yaml:"max_ci_fix_attempts"`
	// MaxReviewFixAttempts is the maximum number of review fix cycles per PR
	// before the PR is considered exhausted. Default: 5.
	MaxReviewFixAttempts int `mapstructure:"max_review_fix_attempts" yaml:"max_review_fix_attempts"`
	// BurnishVerifyTimeout is the maximum time allowed for the post-Smith
	// temper (verification) step in a single burnish attempt. The push and
	// thread-resolution steps that follow are not covered by this deadline.
	// When temper does not return within this window the burnish worker logs
	// a WARN line and returns an error whose message contains the stable
	// reason string "warden_timeout"; the reason is recorded in the event
	// log (EventBurnishFailed) and in the returned error value — it is NOT
	// stored in a separate column of the workers table. The caller is
	// expected to transition the worker row to WorkerFailed and let the
	// daemon's normal recovery path re-dispatch. Default: 5m. Omitting the
	// field (or setting it to 0) falls back to the package default — the
	// timeout cannot be disabled, because doing so would re-introduce the
	// original silent-hang bug (Forge-j67a). When set explicitly the value
	// must be at least 30s.
	BurnishVerifyTimeout time.Duration `mapstructure:"burnish_verify_timeout" yaml:"burnish_verify_timeout"`
	// MaxRebaseAttempts is the maximum number of conflict rebase attempts per
	// PR before the PR is considered exhausted. Default: 3.
	MaxRebaseAttempts int `mapstructure:"max_rebase_attempts" yaml:"max_rebase_attempts"`
	// MergeStrategy controls how PRs are merged from the Hearth TUI.
	// Valid values: "squash" (default), "merge", "rebase".
	MergeStrategy string `mapstructure:"merge_strategy" yaml:"merge_strategy,omitempty"`
	// StaleInterval is how long a worker's log file can go without being
	// modified before the worker is marked as stalled. A value of 0 disables
	// stale detection. Defaults to 5 minutes.
	StaleInterval time.Duration `mapstructure:"stale_interval" yaml:"stale_interval"`
	// DepcheckInterval is how often the dependency checker runs 'go list -m -u all'
	// on Go anvils. A value of 0 disables depcheck. Defaults to 168h (weekly).
	DepcheckInterval time.Duration `mapstructure:"depcheck_interval" yaml:"depcheck_interval,omitempty"`
	// DepcheckTimeout is the maximum time allowed for a single 'go list -m -u all'
	// invocation per anvil. Defaults to 5 minutes.
	DepcheckTimeout time.Duration `mapstructure:"depcheck_timeout" yaml:"depcheck_timeout,omitempty"`
	// VulncheckInterval is how often govulncheck runs on registered Go anvils.
	// Set to 0 to disable scheduled scanning. Default: 24h (daily).
	VulncheckInterval time.Duration `mapstructure:"vulncheck_interval" yaml:"vulncheck_interval,omitempty"`
	// VulncheckTimeout is the maximum time allowed for a single govulncheck
	// invocation per anvil (govulncheck downloads the vuln DB on first run).
	// Defaults to 10 minutes.
	VulncheckTimeout time.Duration `mapstructure:"vulncheck_timeout" yaml:"vulncheck_timeout,omitempty"`
	// VulncheckEnabled controls whether vulnerability scanning is active.
	// When false, scheduled scanning and "forge scan" are disabled regardless
	// of VulncheckInterval. Default: true.
	VulncheckEnabled *bool `mapstructure:"vulncheck_enabled" yaml:"vulncheck_enabled,omitempty"`
	// GoRaceDetection enables the Go race detector (-race flag) as a
	// separate temper step globally. Per-anvil settings override this.
	// Default: false.
	GoRaceDetection bool `mapstructure:"go_race_detection" yaml:"go_race_detection"`
	// AutoLearnRules enables automatic learning of Warden review rules from
	// Copilot comments when a PR is merged. Bellows will fetch Copilot review
	// comments, distill them into rules via Claude, and save them to the
	// anvil's .forge/warden-rules.yaml. Default: false.
	AutoLearnRules bool `mapstructure:"auto_learn_rules" yaml:"auto_learn_rules"`
	// CopilotDailyRequestLimit is the maximum number of weighted Copilot
	// premium requests per calendar day. When the running total exceeds this
	// value, the Copilot provider is skipped in the fallback chain (other
	// providers are unaffected). Zero means no limit (default).
	// Premium requests are weighted by model multiplier (e.g. opus 4.6 = 3x).
	CopilotDailyRequestLimit int `mapstructure:"copilot_daily_request_limit" yaml:"copilot_daily_request_limit,omitempty"`
	// CrucibleEnabled enables automatic Crucible orchestration for parent beads
	// that have children (blocks other beads). When true, the daemon detects
	// parent beads during polling and dispatches them through the Crucible
	// instead of the normal pipeline. Default: false.
	CrucibleEnabled bool `mapstructure:"crucible_enabled" yaml:"crucible_enabled"`
	// AutoMergeCrucibleChildren controls whether child PRs targeting a Crucible
	// feature branch are automatically merged (squash) after the pipeline
	// succeeds. Default: true.
	AutoMergeCrucibleChildren *bool `mapstructure:"auto_merge_crucible_children" yaml:"auto_merge_crucible_children,omitempty"`
	// WardenModelOverride sets an alternative model for Copilot provider entries
	// when running the Warden review stage. Non-Copilot providers are unaffected.
	// Useful for routing review to a cheaper model (e.g. claude-haiku-4-5 at 0.33x
	// premium) while keeping Smith on a stronger model. Empty = use provider default.
	WardenModelOverride string `mapstructure:"warden_model_override" yaml:"warden_model_override,omitempty"`
	// SchematicModelOverride sets an alternative model for Copilot provider entries
	// when running the Schematic pre-analysis stage. Non-Copilot providers are
	// unaffected. Empty = use provider default.
	SchematicModelOverride string `mapstructure:"schematic_model_override" yaml:"schematic_model_override,omitempty"`
	// CopilotSkipWardenSmallDiffs enables automatic Warden skip for small,
	// low-risk diffs when the primary provider is Copilot. Saves one premium
	// request for trivial changes (docs, tests, or ≤2 files under 100 lines).
	// Default: false (opt-in).
	CopilotSkipWardenSmallDiffs bool `mapstructure:"copilot_skip_warden_small_diffs" yaml:"copilot_skip_warden_small_diffs"`
	// CopilotBatchCIFixes enables batching multiple CI failures into a single
	// Smith invocation when the provider is Copilot. Saves premium requests
	// when a PR has multiple failing checks. Default: false (opt-in).
	CopilotBatchCIFixes bool `mapstructure:"copilot_batch_ci_fixes" yaml:"copilot_batch_ci_fixes"`
	// CopilotBatchReviewFixes enables batching multiple review comments into a
	// single Smith invocation when the provider is Copilot. Saves premium
	// requests when a PR has multiple review comments. Default: false (opt-in).
	CopilotBatchReviewFixes bool `mapstructure:"copilot_batch_review_fixes" yaml:"copilot_batch_review_fixes"`
	// WardenFullRereview, when true, forces the Warden to do a full independent
	// review on every iteration instead of a focused re-review that only checks
	// whether prior feedback was addressed. Default: false (focused re-review).
	WardenFullRereview bool `mapstructure:"warden_full_rereview" yaml:"warden_full_rereview"`
	// CopilotCombinedSmithWarden embeds Warden review criteria into the Smith
	// prompt so Smith self-reviews its own diff, eliminating the separate
	// Warden request. A real Warden still runs for P0-P1 beads, when the
	// self-review flags concerns, or via random sampling. Only effective when
	// the primary provider is Copilot. Default: false (opt-in, high risk).
	CopilotCombinedSmithWarden bool `mapstructure:"copilot_combined_smith_warden" yaml:"copilot_combined_smith_warden"`
	// CopilotWardenSampleRate is the probability (0.0–1.0) that a real Warden
	// review is spawned even when the self-review approves, for quality
	// validation. Only used when CopilotCombinedSmithWarden is true.
	// Default: 0.1 (10%).
	CopilotWardenSampleRate float64 `mapstructure:"copilot_warden_sample_rate" yaml:"copilot_warden_sample_rate"`
	// SmelterEnabled controls whether the Smelter background process is active.
	// When true (default), the Smelter runs on a schedule defined by
	// SmelterInterval. Set to false to disable.
	SmelterEnabled *bool `mapstructure:"smelter_enabled" yaml:"smelter_enabled,omitempty"`
	// SmelterInterval is how often the Smelter runs its background processing.
	// Defaults to 8h. Set to 0 to disable scheduled runs.
	SmelterInterval time.Duration `mapstructure:"smelter_interval" yaml:"smelter_interval,omitempty"`
	// QuestgiverEnabled controls whether the QuestGiver monitor is active
	// globally. When nil (default), QuestGiver is disabled. Set to true to
	// enable scheduled quest execution.
	QuestgiverEnabled *bool `mapstructure:"questgiver_enabled" yaml:"questgiver_enabled,omitempty"`
	// QuestgiverInterval is how often the QuestGiver monitor polls anvils
	// for quests to execute. Defaults to 24h (daily).
	QuestgiverInterval time.Duration `mapstructure:"questgiver_interval" yaml:"questgiver_interval,omitempty"`
	// AdventurerTimeout is the maximum time allowed for a single quest
	// execution. Defaults to 5 minutes.
	AdventurerTimeout time.Duration `mapstructure:"adventurer_timeout" yaml:"adventurer_timeout,omitempty"`

	// WicketEnabled controls whether the Wicket issue triage monitor is
	// active globally. When false (default), no issue scanning occurs.
	WicketEnabled bool `mapstructure:"wicket_enabled" yaml:"wicket_enabled"`
	// WicketInterval is how often Wicket polls repositories for new issues.
	// Defaults to 15m.
	WicketInterval time.Duration `mapstructure:"wicket_interval" yaml:"wicket_interval,omitempty"`
	// WicketProvider is the AI provider used for triage decisions. When
	// empty, the global Providers chain is used.
	WicketProvider string `mapstructure:"wicket_provider" yaml:"wicket_provider,omitempty"`
	// WicketBatchSize is the maximum number of issues processed per scan
	// cycle per repository. Defaults to 20.
	WicketBatchSize int `mapstructure:"wicket_batch_size" yaml:"wicket_batch_size,omitempty"`
	// WicketProcessedLabel is the GitHub label applied to issues that have
	// already been triaged. Defaults to "forge-wicket-processed".
	WicketProcessedLabel string `mapstructure:"wicket_processed_label" yaml:"wicket_processed_label,omitempty"`
	// WicketNeedsHumanLabel is the GitHub label applied to issues flagged
	// for human review. Defaults to "forge-needs-human".
	WicketNeedsHumanLabel string `mapstructure:"wicket_needs_human_label" yaml:"wicket_needs_human_label,omitempty"`
	// WicketBeadCreatedLabel is the GitHub label applied to issues for which
	// a bead was created. Defaults to "forge-bead-created".
	WicketBeadCreatedLabel string `mapstructure:"wicket_bead_created_label" yaml:"wicket_bead_created_label,omitempty"`
	// WicketTriggerLabel is the GitHub label that, when non-empty, is required
	// for Wicket to process an issue (pull model). Issues without this label
	// are skipped entirely. When empty (the default), Wicket processes all
	// issues in push-model fashion without any trigger-label gate.
	WicketTriggerLabel string `mapstructure:"wicket_trigger_label" yaml:"wicket_trigger_label,omitempty"`
	// WicketStaleDays is the number of days without an author reply before a
	// Wicket issue awaiting clarification is marked as stale. After a further 7
	// days, the issue is closed automatically. Defaults to 14.
	WicketStaleDays int `mapstructure:"wicket_stale_days" yaml:"wicket_stale_days,omitempty"`
	// BdReadyLimit is the --limit passed to 'bd ready --json'. bd defaults to
	// 10 which can hide labeled lower-priority beads. Default: 100.
	BdReadyLimit int `mapstructure:"bd_ready_limit" yaml:"bd_ready_limit,omitempty"`
	// CruciblePollInterval is the interval for the slow unfiltered poll that
	// rebuilds the Crucible Blocks graph. The fast path polls with a label
	// filter every PollInterval; the slow path runs every CruciblePollInterval
	// to discover parent-child relationships for Crucible detection.
	// Default: 3m. Set to 0 to disable two-tier polling (all polls unfiltered).
	CruciblePollInterval time.Duration `mapstructure:"crucible_poll_interval" yaml:"crucible_poll_interval,omitempty"`
	// ForgeID is the per-instance identifier embedded in the forge-managed
	// marker on every PR Forge creates (`<!-- forge-managed: <id> -->`). When
	// multiple Forge instances point at the same anvil, this id is what lets
	// each instance recognise its own PRs during bellows reconciliation
	// instead of racing for ownership of any forge-authored PR.
	// When empty, ResolvedForgeID() falls back to os.Hostname(), then to a
	// fixed default. Override this in deployments where the host name is not
	// stable (e.g. ephemeral pods that may share a hostname).
	ForgeID string `mapstructure:"forge_id" yaml:"forge_id,omitempty"`

	// Warden holds review-time rule filtering settings. These control how many
	// learned warden rules are injected into the Warden review prompt and
	// which filter passes are applied.
	Warden WardenSettings `mapstructure:"warden" yaml:"warden,omitempty"`

	// ForgeChat configures the Beads-Forge per-turn AI loop (drafter, grilling,
	// plan, emit). Currently exposes turn_timeout so operators can lift the
	// per-turn budget without recompiling.
	ForgeChat ForgeChatSettings `mapstructure:"forgechat" yaml:"forgechat,omitempty"`
}

// MaxForgeChatTurnTimeout is the hard upper bound for settings.forgechat.turn_timeout.
// Values above this are clamped on load and a warning is logged. Picked so that
// even a worst-case grilling turn returns to the user before the browser /
// reverse-proxy gives up on the long-poll HTTP request.
const MaxForgeChatTurnTimeout = 15 * time.Minute

// DefaultForgeChatTurnTimeout is the default wall-clock budget for a single
// forgechat turn. Mirrored as forgechat.defaultTurnTimeout — keep the two in
// sync if either is changed.
const DefaultForgeChatTurnTimeout = 5 * time.Minute

// ForgeChatSettings configures the Beads-Forge per-turn AI loop.
type ForgeChatSettings struct {
	// TurnTimeout caps the wall-clock duration of a single forgechat turn.
	// Defaults to DefaultForgeChatTurnTimeout (5m). Values above
	// MaxForgeChatTurnTimeout (15m) are clamped on load with a slog.Warn.
	// Zero/unset falls back to the default at runtime.
	TurnTimeout time.Duration `mapstructure:"turn_timeout" yaml:"turn_timeout,omitempty"`
}

// ResolvedTurnTimeout returns the effective per-turn timeout after applying
// the default and the hard cap. Callers (e.g. NewClaudeRunner wiring) should
// use this rather than reading TurnTimeout directly.
func (f ForgeChatSettings) ResolvedTurnTimeout() time.Duration {
	if f.TurnTimeout <= 0 {
		return DefaultForgeChatTurnTimeout
	}
	if f.TurnTimeout > MaxForgeChatTurnTimeout {
		return MaxForgeChatTurnTimeout
	}
	return f.TurnTimeout
}

// WardenSettings configures the review-time rule filter applied to the
// warden-rules.yaml entries before they are rendered into the Warden review
// prompt. The defaults shrink a typical prompt by filtering out rules that
// can't match the current diff (path/category/pattern grep).
type WardenSettings struct {
	// MaxRulesPerReview caps the number of rules emitted in the checklist
	// after filtering. Omitted or zero uses the default of 30. Negative
	// values disable the cap entirely. Positive values set an explicit cap.
	MaxRulesPerReview int `mapstructure:"max_rules_per_review" yaml:"max_rules_per_review,omitempty"`
	// UseAllRules, when true, bypasses the three filter passes and applies
	// only the MaxRulesPerReview cap. Useful for A/B comparison against the
	// pre-filter behavior. Default: false.
	UseAllRules bool `mapstructure:"use_all_rules" yaml:"use_all_rules,omitempty"`
	// FilterPathGlob enables filtering by Rule.Paths against the changed
	// files in the diff. Pointer so unset means "use default (true)".
	FilterPathGlob *bool `mapstructure:"filter_path_glob" yaml:"filter_path_glob,omitempty"`
	// FilterCategory enables filtering by Rule.Category against the
	// in-code extension → category map. Pointer so unset means "use default
	// (true)".
	FilterCategory *bool `mapstructure:"filter_category" yaml:"filter_category,omitempty"`
	// FilterPatternGrep enables substring matching of ≥4-char words from
	// Rule.Pattern against the diff. Pointer so unset means "use default
	// (true)".
	FilterPatternGrep *bool `mapstructure:"filter_pattern_grep" yaml:"filter_pattern_grep,omitempty"`
	// ArchiveAfterDays controls the staleness threshold (in days) used by
	// the Smelter's Pass 2 staleness sweep. A rule whose Added date is
	// older than this threshold and has had no recent source activity is
	// moved to the archive store with reason="stale". Zero falls back to
	// the default of 180 days; a negative value disables the pass
	// (callers may use it to mean "never archive").
	ArchiveAfterDays int `mapstructure:"archive_after_days" yaml:"archive_after_days,omitempty"`
	// DedupThreshold is the similarity score (0.0–1.0) above which two
	// active rules are considered duplicates and the older entry is moved to
	// the archive with reason "duplicate". Zero falls back to the default
	// of 0.6.
	DedupThreshold float64 `mapstructure:"dedup_threshold" yaml:"dedup_threshold,omitempty"`
}

// ResolvedArchiveAfterDays returns the effective archive-after threshold in
// days. Zero (unset) resolves to the default of 180; negative values are
// returned as-is so callers can treat them as "never archive".
func (w WardenSettings) ResolvedArchiveAfterDays() int {
	if w.ArchiveAfterDays == 0 {
		return 180
	}
	return w.ArchiveAfterDays
}

// ResolvedDedupThreshold returns the effective dedup-similarity threshold.
// Zero (unset) resolves to the default of 0.6.
func (w WardenSettings) ResolvedDedupThreshold() float64 {
	if w.DedupThreshold == 0 {
		return 0.6
	}
	return w.DedupThreshold
}

// IsFilterPathGlobEnabled returns true unless the toggle is explicitly false.
func (w WardenSettings) IsFilterPathGlobEnabled() bool {
	if w.FilterPathGlob == nil {
		return true
	}
	return *w.FilterPathGlob
}

// IsFilterCategoryEnabled returns true unless the toggle is explicitly false.
func (w WardenSettings) IsFilterCategoryEnabled() bool {
	if w.FilterCategory == nil {
		return true
	}
	return *w.FilterCategory
}

// IsFilterPatternGrepEnabled returns true unless the toggle is explicitly
// false.
func (w WardenSettings) IsFilterPatternGrepEnabled() bool {
	if w.FilterPatternGrep == nil {
		return true
	}
	return *w.FilterPatternGrep
}

// ResolvedMaxRulesPerReview returns the cap to pass to FilterRules.
// Zero (unset) → 30 (default). Negative → 0 (no cap; capRules treats ≤0 as
// unlimited). Positive → returned as-is.
func (w WardenSettings) ResolvedMaxRulesPerReview() int {
	switch {
	case w.MaxRulesPerReview == 0:
		return 30
	case w.MaxRulesPerReview < 0:
		return 0
	default:
		return w.MaxRulesPerReview
	}
}

// durationString returns the duration string, or omits zero values.
func durationString(d time.Duration) string {
	return d.String()
}

// forgeChatShadow renders ForgeChatSettings with the duration field as a
// human-readable string ("5m0s") instead of nanoseconds. turn_timeout is
// omitted when it equals the default so the file stays clean unless the
// operator has explicitly changed it.
type forgeChatShadow struct {
	TurnTimeout string `yaml:"turn_timeout,omitempty"`
}

// MarshalYAML serialises SettingsConfig with time.Duration fields as
// human-readable strings (e.g. "30s", "5m0s") instead of nanosecond ints.
func (s SettingsConfig) MarshalYAML() (interface{}, error) {
	// Shadow struct with durations replaced by strings.
	type shadow struct {
		PollInterval              string   `yaml:"poll_interval"`
		SmithTimeout              string   `yaml:"smith_timeout"`
		MaxTotalSmiths            int      `yaml:"max_total_smiths"`
		MaxReviewAttempts         int      `yaml:"max_review_attempts"`
		MaxPipelineIterations     int      `yaml:"max_pipeline_iterations"`
		ClaudeFlags               []string `yaml:"claude_flags"`
		Providers                 []string `yaml:"providers,omitempty"`
		RateLimitBackoff          string   `yaml:"rate_limit_backoff"`
		SmithProviders            []string            `yaml:"smith_providers,omitempty"`
		StageProviders            map[string][]string `yaml:"stage_providers,omitempty"`
		SchematicEnabled          bool                `yaml:"schematic_enabled"`
		SchematicWordThreshold    int      `yaml:"schematic_word_threshold,omitempty"`
		BellowsInterval           string   `yaml:"bellows_interval"`
		DailyCostLimit            float64  `yaml:"daily_cost_limit,omitempty"`
		MaxCIFixAttempts          int      `yaml:"max_ci_fix_attempts"`
		MaxReviewFixAttempts      int      `yaml:"max_review_fix_attempts"`
		MaxRebaseAttempts         int      `yaml:"max_rebase_attempts"`
		BurnishVerifyTimeout      string   `yaml:"burnish_verify_timeout,omitempty"`
		MergeStrategy             string   `yaml:"merge_strategy,omitempty"`
		StaleInterval             string   `yaml:"stale_interval"`
		DepcheckInterval          string   `yaml:"depcheck_interval,omitempty"`
		DepcheckTimeout           string   `yaml:"depcheck_timeout,omitempty"`
		VulncheckInterval         string   `yaml:"vulncheck_interval,omitempty"`
		VulncheckTimeout          string   `yaml:"vulncheck_timeout,omitempty"`
		VulncheckEnabled          *bool    `yaml:"vulncheck_enabled,omitempty"`
		GoRaceDetection           bool     `yaml:"go_race_detection"`
		AutoLearnRules            bool     `yaml:"auto_learn_rules"`
		CopilotDailyRequestLimit  int      `yaml:"copilot_daily_request_limit,omitempty"`
		CrucibleEnabled           bool     `yaml:"crucible_enabled"`
		AutoMergeCrucibleChildren *bool    `yaml:"auto_merge_crucible_children,omitempty"`
		WardenModelOverride       string   `yaml:"warden_model_override,omitempty"`
		SchematicModelOverride    string   `yaml:"schematic_model_override,omitempty"`

		CopilotSkipWardenSmallDiffs bool    `yaml:"copilot_skip_warden_small_diffs"`
		CopilotBatchCIFixes         bool    `yaml:"copilot_batch_ci_fixes"`
		CopilotBatchReviewFixes     bool    `yaml:"copilot_batch_review_fixes"`
		WardenFullRereview          bool    `yaml:"warden_full_rereview"`
		CopilotCombinedSmithWarden  bool    `yaml:"copilot_combined_smith_warden"`
		CopilotWardenSampleRate     float64 `yaml:"copilot_warden_sample_rate"`
		SmelterEnabled              *bool   `yaml:"smelter_enabled,omitempty"`
		SmelterInterval             string  `yaml:"smelter_interval"`
		QuestgiverEnabled           *bool   `yaml:"questgiver_enabled,omitempty"`
		QuestgiverInterval          string  `yaml:"questgiver_interval,omitempty"`
		AdventurerTimeout           string  `yaml:"adventurer_timeout,omitempty"`

		WicketEnabled          bool   `yaml:"wicket_enabled"`
		WicketInterval         string `yaml:"wicket_interval"`
		WicketProvider         string `yaml:"wicket_provider,omitempty"`
		WicketBatchSize        int    `yaml:"wicket_batch_size,omitempty"`
		WicketProcessedLabel   string `yaml:"wicket_processed_label,omitempty"`
		WicketNeedsHumanLabel  string `yaml:"wicket_needs_human_label,omitempty"`
		WicketBeadCreatedLabel string `yaml:"wicket_bead_created_label,omitempty"`
		WicketTriggerLabel     string `yaml:"wicket_trigger_label,omitempty"`
		WicketStaleDays          int            `yaml:"wicket_stale_days,omitempty"`
		BdReadyLimit             int            `yaml:"bd_ready_limit,omitempty"`
		CruciblePollInterval     string         `yaml:"crucible_poll_interval,omitempty"`
		ForgeID                  string         `yaml:"forge_id,omitempty"`
		Warden                   WardenSettings `yaml:"warden,omitempty"`
		ForgeChat                forgeChatShadow `yaml:"forgechat,omitempty"`
	}

	sh := shadow{
		PollInterval:              durationString(s.PollInterval),
		SmithTimeout:              durationString(s.SmithTimeout),
		MaxTotalSmiths:            s.MaxTotalSmiths,
		MaxReviewAttempts:         s.MaxReviewAttempts,
		MaxPipelineIterations:     s.MaxPipelineIterations,
		ClaudeFlags:               s.ClaudeFlags,
		Providers:                 s.Providers,
		RateLimitBackoff:          durationString(s.RateLimitBackoff),
		SmithProviders:            s.SmithProviders,
		StageProviders:            s.StageProviders,
		SchematicEnabled:          s.SchematicEnabled,
		SchematicWordThreshold:    s.SchematicWordThreshold,
		BellowsInterval:           durationString(s.BellowsInterval),
		DailyCostLimit:            s.DailyCostLimit,
		MaxCIFixAttempts:          s.MaxCIFixAttempts,
		MaxReviewFixAttempts:      s.MaxReviewFixAttempts,
		MaxRebaseAttempts:         s.MaxRebaseAttempts,
		BurnishVerifyTimeout:      func() string {
			if s.BurnishVerifyTimeout > 0 {
				return durationString(s.BurnishVerifyTimeout)
			}
			return ""
		}(),
		MergeStrategy:             s.MergeStrategy,
		StaleInterval:             durationString(s.StaleInterval),
		VulncheckEnabled:          s.VulncheckEnabled,
		GoRaceDetection:           s.GoRaceDetection,
		AutoLearnRules:            s.AutoLearnRules,
		CopilotDailyRequestLimit:  s.CopilotDailyRequestLimit,
		CrucibleEnabled:           s.CrucibleEnabled,
		AutoMergeCrucibleChildren: s.AutoMergeCrucibleChildren,
		WardenModelOverride:       s.WardenModelOverride,
		SchematicModelOverride:    s.SchematicModelOverride,

		CopilotSkipWardenSmallDiffs: s.CopilotSkipWardenSmallDiffs,
		CopilotBatchCIFixes:         s.CopilotBatchCIFixes,
		CopilotBatchReviewFixes:     s.CopilotBatchReviewFixes,
		WardenFullRereview:          s.WardenFullRereview,
		CopilotCombinedSmithWarden:  s.CopilotCombinedSmithWarden,
		CopilotWardenSampleRate:     s.CopilotWardenSampleRate,
		SmelterEnabled:              s.SmelterEnabled,
		QuestgiverEnabled:           s.QuestgiverEnabled,

		WicketEnabled:          s.WicketEnabled,
		WicketProvider:         s.WicketProvider,
		WicketBatchSize:        s.WicketBatchSize,
		WicketProcessedLabel:   s.WicketProcessedLabel,
		WicketNeedsHumanLabel:  s.WicketNeedsHumanLabel,
		WicketBeadCreatedLabel: s.WicketBeadCreatedLabel,
		WicketTriggerLabel:     s.WicketTriggerLabel,
		WicketStaleDays:        s.WicketStaleDays,
		BdReadyLimit:           s.BdReadyLimit,
		ForgeID:                s.ForgeID,
		Warden:                 s.Warden,
		ForgeChat: forgeChatShadow{
			TurnTimeout: func() string {
				if s.ForgeChat.TurnTimeout > 0 && s.ForgeChat.TurnTimeout != DefaultForgeChatTurnTimeout {
					return durationString(s.ForgeChat.TurnTimeout)
				}
				return ""
			}(),
		},
	}

	if s.CruciblePollInterval > 0 {
		sh.CruciblePollInterval = durationString(s.CruciblePollInterval)
	}

	// Only include non-zero optional durations.
	if s.DepcheckInterval > 0 {
		sh.DepcheckInterval = durationString(s.DepcheckInterval)
	}
	if s.DepcheckTimeout > 0 {
		sh.DepcheckTimeout = durationString(s.DepcheckTimeout)
	}
	if s.VulncheckInterval > 0 {
		sh.VulncheckInterval = durationString(s.VulncheckInterval)
	}
	if s.VulncheckTimeout > 0 {
		sh.VulncheckTimeout = durationString(s.VulncheckTimeout)
	}
	// Always emit SmelterInterval so an intentional 0 (disable scheduled runs)
	// is persisted and not silently dropped back to the 8h default on next load.
	sh.SmelterInterval = durationString(s.SmelterInterval)
	if s.QuestgiverInterval > 0 {
		sh.QuestgiverInterval = durationString(s.QuestgiverInterval)
	}
	if s.AdventurerTimeout > 0 {
		sh.AdventurerTimeout = durationString(s.AdventurerTimeout)
	}
	// Always emit WicketInterval so an intentional 0 (disable scheduled polling)
	// is persisted and not silently dropped back to the 15m default on next load.
	sh.WicketInterval = durationString(s.WicketInterval)

	return sh, nil
}

// ProvidersForStage returns the provider spec list for the given pipeline stage.
// Resolution order: stage_providers[stage] → smith_providers (for smith, warden,
// schematic only) → providers. Returns nil when all levels are empty (caller
// should apply provider.Defaults).
func (s SettingsConfig) ProvidersForStage(stage string) []string {
	return ProvidersForStageWithAnvil(s, nil, stage)
}

// ProvidersForStageWithAnvil returns the provider spec list for a pipeline stage,
// checking the per-anvil stage_providers first. Resolution order:
//
//	anvil.stage_providers[stage] → settings.stage_providers[stage] →
//	settings.smith_providers (smith/warden/schematic only) → settings.providers →
//	nil (caller applies provider.Defaults).
func ProvidersForStageWithAnvil(s SettingsConfig, anvil *AnvilConfig, stage string) []string {
	// Per-anvil stage_providers takes highest priority.
	if anvil != nil {
		if sp, ok := anvil.StageProviders[stage]; ok && len(sp) > 0 {
			return sp
		}
	}
	if sp, ok := s.StageProviders[stage]; ok && len(sp) > 0 {
		return sp
	}
	// SmithProviders is the legacy fallback for dispatch-pipeline stages.
	switch stage {
	case "smith", "warden", "schematic":
		if len(s.SmithProviders) > 0 {
			return s.SmithProviders
		}
	}
	return s.Providers
}

// ExplicitStageProvidersWithAnvil returns the provider spec for a stage only if
// it is explicitly configured in stage_providers (per-anvil or global). Unlike
// ProvidersForStageWithAnvil it does NOT fall back to smith_providers or
// providers, so callers can distinguish "explicitly overridden" from "inherited".
// Returns nil when the stage has no explicit override.
func ExplicitStageProvidersWithAnvil(s SettingsConfig, anvil *AnvilConfig, stage string) []string {
	if anvil != nil {
		if sp, ok := anvil.StageProviders[stage]; ok && len(sp) > 0 {
			return sp
		}
	}
	if sp, ok := s.StageProviders[stage]; ok && len(sp) > 0 {
		return sp
	}
	return nil
}

// IsVulncheckEnabled returns true unless vulncheck_enabled is explicitly false.
func (s SettingsConfig) IsVulncheckEnabled() bool {
	if s.VulncheckEnabled == nil {
		return true
	}
	return *s.VulncheckEnabled
}

// IsAutoMergeCrucibleChildren returns true unless auto_merge_crucible_children
// is explicitly false. Defaults to true.
func (s SettingsConfig) IsAutoMergeCrucibleChildren() bool {
	if s.AutoMergeCrucibleChildren == nil {
		return true
	}
	return *s.AutoMergeCrucibleChildren
}

// IsSmelterEnabled returns true unless smelter_enabled is explicitly false.
// Defaults to true.
func (s SettingsConfig) IsSmelterEnabled() bool {
	if s.SmelterEnabled == nil {
		return true
	}
	return *s.SmelterEnabled
}

// IsQuestgiverEnabled returns true only when questgiver_enabled is explicitly true.
// Defaults to false (nil = disabled).
func (s SettingsConfig) IsQuestgiverEnabled() bool {
	if s.QuestgiverEnabled == nil {
		return false
	}
	return *s.QuestgiverEnabled
}

// ResolvedForgeID returns the forge instance identifier used to mark PRs Forge
// creates (`<!-- forge-managed: <id> -->`). Resolution order:
//
//  1. settings.forge_id, if set
//  2. os.Hostname()
//  3. "default"
//
// In deployments running multiple Forge instances against the same anvil,
// each instance MUST have a distinct value here (or distinct hostnames) so
// reconcileOpenPRs only adopts PRs the current instance created.
func (s SettingsConfig) ResolvedForgeID() string {
	if id := strings.TrimSpace(s.ForgeID); id != "" {
		return id
	}
	if hn, err := os.Hostname(); err == nil {
		if hn = strings.TrimSpace(hn); hn != "" {
			return hn
		}
	}
	return "default"
}

// TeamsNotificationConfig holds configuration for the MS Teams webhook.
// Used in the new nested notifications.teams config structure.
type TeamsNotificationConfig struct {
	WebhookURL string   `mapstructure:"webhook_url" yaml:"webhook_url,omitempty"`
	Events     []string `mapstructure:"events" yaml:"events,omitempty"`
}

// WebhookTargetConfig defines a single generic JSON webhook target.
// Each target receives a simple JSON payload (not an Adaptive Card) and can
// filter which events it receives.
type WebhookTargetConfig struct {
	Name   string   `mapstructure:"name" yaml:"name"`
	URL    string   `mapstructure:"url" yaml:"url"`
	Events []string `mapstructure:"events" yaml:"events,omitempty"`
}

// NotificationsConfig holds webhook and notification settings.
type NotificationsConfig struct {
	// Legacy flat fields — kept for backward compatibility.
	TeamsWebhookURL string `mapstructure:"teams_webhook_url" yaml:"teams_webhook_url,omitempty"`
	Enabled         bool   `mapstructure:"enabled" yaml:"enabled"`
	// Events to notify on. Empty = all. Options: pr_created, bead_failed, daily_cost, worker_done, bead_decomposed, release_published, pr_ready_to_merge.
	Events []string `mapstructure:"events" yaml:"events,omitempty"`
	// ReleaseWebhookURLs is a list of generic JSON webhook URLs that receive
	// a release_published payload when 'forge notify release' is called.
	// These receive a simple JSON object (not a Teams Adaptive Card) suitable
	// for custom dashboards or other receivers.
	ReleaseWebhookURLs []string `mapstructure:"release_webhook_urls" yaml:"release_webhook_urls,omitempty"`
	// PRReadyWebhookURLs is a list of generic JSON webhook URLs that receive
	// a pr_ready_to_merge payload when a PR enters ready-to-merge state.
	// These receive a simple JSON object (not a Teams Adaptive Card) suitable
	// for custom dashboards or other receivers.
	PRReadyWebhookURLs []string `mapstructure:"pr_ready_webhook_urls" yaml:"pr_ready_webhook_urls,omitempty"`

	// Teams holds the new nested Teams webhook configuration.
	// If set, takes precedence over the legacy teams_webhook_url and events fields.
	Teams TeamsNotificationConfig `mapstructure:"teams" yaml:"teams,omitempty"`
	// Webhooks is a list of generic JSON webhook targets. Each target can filter
	// events independently. Supported events: pr_created, worker_done, bead_failed,
	// bead_decomposed, pr_ready_to_merge, release, daily_cost.
	Webhooks []WebhookTargetConfig `mapstructure:"webhooks" yaml:"webhooks,omitempty"`
}

// ResolvedTeamsURL returns the effective Teams webhook URL.
// The new nested teams.webhook_url takes precedence over the legacy teams_webhook_url field.
func (n NotificationsConfig) ResolvedTeamsURL() string {
	if n.Teams.WebhookURL != "" {
		return n.Teams.WebhookURL
	}
	return n.TeamsWebhookURL
}

// ResolvedTeamsEvents returns the effective Teams event filter.
// The new nested teams.events takes precedence over the legacy events field.
func (n NotificationsConfig) ResolvedTeamsEvents() []string {
	if len(n.Teams.Events) > 0 {
		return n.Teams.Events
	}
	return n.Events
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Anvils: make(map[string]AnvilConfig),
		Settings: SettingsConfig{
			PollInterval:          5 * time.Minute,
			SmithTimeout:          30 * time.Minute,
			MaxTotalSmiths:        4,
			MaxReviewAttempts:     2,
			MaxPipelineIterations: 5,
			ClaudeFlags:           []string{},
			// No Providers default here — provider.FromConfig handles empty slice.
			RateLimitBackoff:     5 * time.Minute,
			BellowsInterval:      2 * time.Minute,
			MaxCIFixAttempts:     5,
			MaxReviewFixAttempts: 5,
			MaxRebaseAttempts:    3,
			BurnishVerifyTimeout: 5 * time.Minute,
			StaleInterval:        5 * time.Minute,
			DepcheckInterval:     168 * time.Hour, // weekly
			DepcheckTimeout:      5 * time.Minute,
			VulncheckInterval:    24 * time.Hour,
			VulncheckTimeout:     10 * time.Minute,
			SmelterInterval:    8 * time.Hour,
			QuestgiverInterval: 24 * time.Hour,
			AdventurerTimeout:  5 * time.Minute,
			// Copilot combined Smith+Warden mode settings.
			CopilotWardenSampleRate: 0.1,
			// Wicket issue triage monitor defaults.
			WicketInterval:         15 * time.Minute,
			WicketBatchSize:        20,
			WicketProcessedLabel:   "forge-wicket-processed",
			WicketNeedsHumanLabel:  "forge-needs-human",
			WicketBeadCreatedLabel: "forge-bead-created",
			WicketTriggerLabel:     "",
			BdReadyLimit:           100,
			CruciblePollInterval:   3 * time.Minute,
			Warden: WardenSettings{
				MaxRulesPerReview: 30,
				UseAllRules:       false,
				FilterPathGlob:    boolPtr(true),
				FilterCategory:    boolPtr(true),
				FilterPatternGrep: boolPtr(true),
				ArchiveAfterDays:  180,
				DedupThreshold:    0.6,
			},
			ForgeChat: ForgeChatSettings{
				TurnTimeout: DefaultForgeChatTurnTimeout,
			},
		},
	}
}

func boolPtr(b bool) *bool {
	return &b
}

// Load reads the configuration from the given file path, or auto-discovers
// forge.yaml from the working directory or ~/.forge/config.yaml.
// Environment variables with the FORGE_ prefix override file values.
func Load(configFile string) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("settings.poll_interval", "5m")
	v.SetDefault("settings.smith_timeout", "30m")
	v.SetDefault("settings.max_total_smiths", 4)
	v.SetDefault("settings.max_review_attempts", 2)
	v.SetDefault("settings.max_pipeline_iterations", 5)
	v.SetDefault("settings.claude_flags", []string{})
	v.SetDefault("settings.rate_limit_backoff", "5m")
	v.SetDefault("settings.bellows_interval", "2m")
	v.SetDefault("settings.max_ci_fix_attempts", 5)
	v.SetDefault("settings.max_review_fix_attempts", 5)
	v.SetDefault("settings.max_rebase_attempts", 3)
	v.SetDefault("settings.burnish_verify_timeout", "5m")
	v.SetDefault("settings.stale_interval", "5m")
	v.SetDefault("settings.depcheck_interval", "168h")
	v.SetDefault("settings.depcheck_timeout", "5m")
	v.SetDefault("settings.vulncheck_interval", "24h")
	v.SetDefault("settings.vulncheck_timeout", "10m")
	v.SetDefault("settings.vulncheck_enabled", true)
	v.SetDefault("settings.smelter_enabled", true)
	v.SetDefault("settings.smelter_interval", "8h")
	v.SetDefault("settings.questgiver_interval", "24h")
	v.SetDefault("settings.adventurer_timeout", "5m")
	v.SetDefault("settings.copilot_warden_sample_rate", 0.1)
	v.SetDefault("settings.wicket_enabled", false)
	v.SetDefault("settings.wicket_interval", "15m")
	v.SetDefault("settings.wicket_batch_size", 20)
	v.SetDefault("settings.wicket_processed_label", "forge-wicket-processed")
	v.SetDefault("settings.wicket_needs_human_label", "forge-needs-human")
	v.SetDefault("settings.wicket_bead_created_label", "forge-bead-created")
	v.SetDefault("settings.wicket_trigger_label", "")
	v.SetDefault("settings.bd_ready_limit", 100)
	v.SetDefault("settings.crucible_poll_interval", "3m")
	v.SetDefault("settings.warden.max_rules_per_review", 30)
	v.SetDefault("settings.warden.use_all_rules", false)
	v.SetDefault("settings.warden.filter_path_glob", true)
	v.SetDefault("settings.warden.filter_category", true)
	v.SetDefault("settings.warden.filter_pattern_grep", true)
	v.SetDefault("settings.warden.archive_after_days", 180)
	v.SetDefault("settings.warden.dedup_threshold", 0.6)
	v.SetDefault("settings.forgechat.turn_timeout", DefaultForgeChatTurnTimeout.String())

	// Environment variable support: FORGE_SETTINGS_POLL_INTERVAL etc.
	// SetEnvKeyReplacer maps dotted config keys (settings.auto_learn_rules) to
	// underscore env vars (FORGE_SETTINGS_AUTO_LEARN_RULES).
	v.SetEnvPrefix("FORGE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Config file resolution — matches the package doc above:
	//   1. --config flag (explicit path)
	//   2. ./forge.yaml (working directory)
	//   3. ~/.forge/config.yaml (user home)
	//
	// Probe explicitly rather than rely on viper's SetConfigName/AddConfigPath
	// search, since that combination forces a single config-name and would
	// look for ~/.forge/forge.yaml — disagreeing with the documented
	// ~/.forge/config.yaml. ~/.forge/forge.yaml is probed last for
	// backward compatibility.
	if configFile != "" {
		v.SetConfigFile(configFile)
	} else if path := resolveDefaultConfigPath(); path != "" {
		v.SetConfigFile(path)
	}

	// Read config (file not found is OK — we'll use defaults + env)
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			// File exists but can't be parsed
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	cfg := Defaults()
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Set per-anvil defaults and parse durations
	for name, anvil := range cfg.Anvils {
		if anvil.AutoDispatch == "" {
			anvil.AutoDispatch = "all"
		}
		cfg.Anvils[name] = anvil
	}

	// Parse durations from string values (viper returns strings from YAML)
	if raw := v.GetString("settings.poll_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_interval %q: %w", raw, err)
		}
		cfg.Settings.PollInterval = d
	}
	if raw := v.GetString("settings.smith_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid smith_timeout %q: %w", raw, err)
		}
		cfg.Settings.SmithTimeout = d
	}
	if raw := v.GetString("settings.rate_limit_backoff"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid rate_limit_backoff %q: %w", raw, err)
		}
		cfg.Settings.RateLimitBackoff = d
	}
	if raw := v.GetString("settings.bellows_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid bellows_interval %q: %w", raw, err)
		}
		cfg.Settings.BellowsInterval = d
	}
	if raw := v.GetString("settings.stale_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid stale_interval %q: %w", raw, err)
		}
		cfg.Settings.StaleInterval = d
	}
	if raw := v.GetString("settings.burnish_verify_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid burnish_verify_timeout %q: %w", raw, err)
		}
		cfg.Settings.BurnishVerifyTimeout = d
	}
	if raw := v.GetString("settings.depcheck_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid depcheck_interval %q: %w", raw, err)
		}
		cfg.Settings.DepcheckInterval = d
	}
	if raw := v.GetString("settings.depcheck_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid depcheck_timeout %q: %w", raw, err)
		}
		cfg.Settings.DepcheckTimeout = d
	}
	if raw := v.GetString("settings.vulncheck_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid vulncheck_interval %q: %w", raw, err)
		}
		cfg.Settings.VulncheckInterval = d
	}
	if raw := v.GetString("settings.vulncheck_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid vulncheck_timeout %q: %w", raw, err)
		}
		cfg.Settings.VulncheckTimeout = d
	}
	if raw := v.GetString("settings.smelter_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid smelter_interval %q: %w", raw, err)
		}
		cfg.Settings.SmelterInterval = d
	}
	if raw := v.GetString("settings.questgiver_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid questgiver_interval %q: %w", raw, err)
		}
		cfg.Settings.QuestgiverInterval = d
	}
	if raw := v.GetString("settings.adventurer_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid adventurer_timeout %q: %w", raw, err)
		}
		cfg.Settings.AdventurerTimeout = d
	}
	if raw := v.GetString("settings.wicket_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid wicket_interval %q: %w", raw, err)
		}
		cfg.Settings.WicketInterval = d
	}
	if raw := v.GetString("settings.crucible_poll_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid crucible_poll_interval %q: %w", raw, err)
		}
		cfg.Settings.CruciblePollInterval = d
	}
	if raw := v.GetString("settings.forgechat.turn_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid forgechat.turn_timeout %q: %w", raw, err)
		}
		cfg.Settings.ForgeChat.TurnTimeout = d
	}
	if cfg.Settings.ForgeChat.TurnTimeout <= 0 {
		cfg.Settings.ForgeChat.TurnTimeout = DefaultForgeChatTurnTimeout
	} else if cfg.Settings.ForgeChat.TurnTimeout > MaxForgeChatTurnTimeout {
		slog.Warn("forgechat.turn_timeout exceeds hard cap; clamping",
			"configured", cfg.Settings.ForgeChat.TurnTimeout,
			"clamped", MaxForgeChatTurnTimeout,
		)
		cfg.Settings.ForgeChat.TurnTimeout = MaxForgeChatTurnTimeout
	}

	// Decrypt any enc:-prefixed webhook URLs written by Hytte.
	decryptWebhookURLs(&cfg)

	return &cfg, nil
}

// ConfigFilePath returns the path of the config file that was loaded,
// or empty string if no file was found.
func ConfigFilePath(configFile string) string {
	if configFile != "" {
		v := viper.New()
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			return ""
		}
		return v.ConfigFileUsed()
	}
	return resolveDefaultConfigPath()
}

// resolveDefaultConfigPath probes the documented default locations for a
// config file when the caller has not set --config. Returns the first match,
// or "" when nothing is found. Resolution order matches the package doc:
//
//  1. ./forge.yaml (working directory)
//  2. ~/.forge/config.yaml (user home, the documented path)
//  3. ~/.forge/forge.yaml (backward-compat shim)
func resolveDefaultConfigPath() string {
	if _, err := os.Stat("forge.yaml"); err == nil {
		return "forge.yaml"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"config.yaml", "forge.yaml"} {
		candidate := filepath.Join(home, ".forge", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// Validate checks the config for logical errors.
func (c *Config) Validate() []string {
	var errs []string

	if c.Settings.MaxTotalSmiths < 1 {
		errs = append(errs, "settings.max_total_smiths must be >= 1")
	}
	if c.Settings.MaxReviewAttempts < 1 {
		errs = append(errs, "settings.max_review_attempts must be >= 1")
	}
	if c.Settings.MaxPipelineIterations < 1 {
		errs = append(errs, "settings.max_pipeline_iterations must be >= 1")
	}
	if c.Settings.PollInterval < 10*time.Second {
		errs = append(errs, "settings.poll_interval must be >= 10s")
	}
	if c.Settings.SmithTimeout < 1*time.Minute {
		errs = append(errs, "settings.smith_timeout must be >= 1m")
	}
	if c.Settings.BellowsInterval < 30*time.Second {
		errs = append(errs, "settings.bellows_interval must be >= 30s")
	}
	if c.Settings.DailyCostLimit < 0 || math.IsNaN(c.Settings.DailyCostLimit) || math.IsInf(c.Settings.DailyCostLimit, 0) {
		errs = append(errs, "settings.daily_cost_limit must be a non-negative finite number")
	}
	if c.Settings.StaleInterval < 0 {
		errs = append(errs, "settings.stale_interval must not be negative (set to 0 to disable)")
	} else if c.Settings.StaleInterval > 0 && c.Settings.StaleInterval < 30*time.Second {
		errs = append(errs, "settings.stale_interval must be >= 30s when enabled (or 0 to disable)")
	}
	if c.Settings.MaxCIFixAttempts < 1 {
		errs = append(errs, "settings.max_ci_fix_attempts must be >= 1")
	}
	if c.Settings.MaxReviewFixAttempts < 1 {
		errs = append(errs, "settings.max_review_fix_attempts must be >= 1")
	}
	if c.Settings.MaxRebaseAttempts < 1 {
		errs = append(errs, "settings.max_rebase_attempts must be >= 1")
	}
	if c.Settings.BurnishVerifyTimeout < 0 {
		errs = append(errs, "settings.burnish_verify_timeout must not be negative (omit or set to 0 to use the package default)")
	} else if c.Settings.BurnishVerifyTimeout > 0 && c.Settings.BurnishVerifyTimeout < 30*time.Second {
		errs = append(errs, "settings.burnish_verify_timeout must be >= 30s when set explicitly (omit or set to 0 to use the package default)")
	}

	if c.Settings.CopilotDailyRequestLimit < 0 {
		errs = append(errs, "settings.copilot_daily_request_limit must be >= 0 (0 = no limit)")
	}
	if math.IsNaN(c.Settings.CopilotWardenSampleRate) || math.IsInf(c.Settings.CopilotWardenSampleRate, 0) ||
		c.Settings.CopilotWardenSampleRate < 0 || c.Settings.CopilotWardenSampleRate > 1 {
		errs = append(errs, "settings.copilot_warden_sample_rate must be a finite value in [0.0, 1.0]")
	}

	if c.Settings.DepcheckInterval < 0 {
		errs = append(errs, "settings.depcheck_interval must not be negative (set to 0 to disable)")
	} else if c.Settings.DepcheckInterval > 0 && c.Settings.DepcheckInterval < 1*time.Hour {
		errs = append(errs, "settings.depcheck_interval must be >= 1h when enabled (or 0 to disable)")
	}
	if c.Settings.DepcheckTimeout < 0 {
		errs = append(errs, "settings.depcheck_timeout must not be negative")
	}

	if c.Settings.SmelterInterval < 0 {
		errs = append(errs, "settings.smelter_interval must not be negative (set to 0 to disable)")
	} else if c.Settings.IsSmelterEnabled() && c.Settings.SmelterInterval > 0 && c.Settings.SmelterInterval < 1*time.Hour {
		errs = append(errs, "settings.smelter_interval must be >= 1h when enabled (or 0 to disable)")
	}

	if c.Settings.QuestgiverInterval < 0 {
		errs = append(errs, "settings.questgiver_interval must not be negative (set to 0 to disable)")
	} else if c.Settings.IsQuestgiverEnabled() && c.Settings.QuestgiverInterval == 0 {
		errs = append(errs, "settings.questgiver_interval must be > 0 when questgiver is enabled")
	}
	if c.Settings.AdventurerTimeout < 0 {
		errs = append(errs, "settings.adventurer_timeout must not be negative")
	}

	if c.Settings.CruciblePollInterval < 0 {
		errs = append(errs, "settings.crucible_poll_interval must not be negative (set to 0 to disable two-tier polling)")
	} else if c.Settings.CruciblePollInterval > 0 && c.Settings.CruciblePollInterval < 30*time.Second {
		errs = append(errs, "settings.crucible_poll_interval must be >= 30s when enabled (or 0 to disable)")
	}

	for name, anvil := range c.Anvils {
		if anvil.Path == "" {
			errs = append(errs, fmt.Sprintf("anvil %q: path is required", name))
		}
		if anvil.MaxSmiths < 0 {
			errs = append(errs, fmt.Sprintf("anvil %q: max_smiths must be >= 0", name))
		}

		if _, err := vcs.ParsePlatform(anvil.Platform); err != nil {
			errs = append(errs, fmt.Sprintf("anvil %q: %s", name, err))
		}

		switch anvil.AutoDispatch {
		case "all", "tagged", "priority", "off", "":
			// valid
		default:
			errs = append(errs, fmt.Sprintf("anvil %q: invalid auto_dispatch %q (must be all|tagged|priority|off)", name, anvil.AutoDispatch))
		}

		if anvil.AutoDispatch == "tagged" && anvil.AutoDispatchTag == "" {
			errs = append(errs, fmt.Sprintf("anvil %q: auto_dispatch_tag must be non-empty when auto_dispatch is \"tagged\"", name))
		}
		if anvil.AutoDispatch == "priority" && (anvil.AutoDispatchMinPriority < 0 || anvil.AutoDispatchMinPriority > 4) {
			errs = append(errs, fmt.Sprintf("anvil %q: auto_dispatch_min_priority must be 0-4 (0 = critical-only) when auto_dispatch is \"priority\"", name))
		}

		if anvil.Temper != nil && anvil.Temper.LintRequired && anvil.Temper.Lint == "" && len(anvil.Temper.Steps) == 0 {
			errs = append(errs, fmt.Sprintf("anvil %q: temper.lint_required is true but temper.lint is not set", name))
		}

		if anvil.Temper != nil {
			seen := make(map[string]bool)
			for i, step := range anvil.Temper.Steps {
				trimmedName := strings.TrimSpace(step.Name)
				trimmedCommand := strings.TrimSpace(step.Command)

				if trimmedName == "" {
					errs = append(errs, fmt.Sprintf("anvil %q: temper.steps[%d].name must be non-empty", name, i))
				}
				if trimmedCommand == "" && len(step.VerifyNoConflictMarkers) == 0 {
					errs = append(errs, fmt.Sprintf("anvil %q: temper.steps[%d].command must be non-empty (or set verify_no_conflict_markers for a scan-only step)", name, i))
				}
				if trimmedName != "" {
					if seen[trimmedName] {
						errs = append(errs, fmt.Sprintf("anvil %q: temper.steps has duplicate name %q", name, trimmedName))
					}
					seen[trimmedName] = true
				}
				if step.Timeout < 0 {
					errs = append(errs, fmt.Sprintf("anvil %q: temper.steps[%d].timeout must be non-negative", name, i))
				}
			}
		}
	}

	return errs
}

// Save writes the config to the specified file path in YAML format.
// It uses yaml.Marshal with yaml struct tags so that every config field
// is persisted automatically — no new field can be silently dropped.
func Save(cfg *Config, path string) error {
	// Ensure directory exists.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	return os.WriteFile(path, data, 0o644)
}
