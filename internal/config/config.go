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
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for The Forge.
type Config struct {
	Anvils        map[string]AnvilConfig `mapstructure:"anvils" yaml:"anvils"`
	Settings      SettingsConfig         `mapstructure:"settings" yaml:"settings"`
	Notifications NotificationsConfig    `mapstructure:"notifications" yaml:"notifications,omitempty"`
	// Assay holds the global Assay (AI PR review) configuration. It lives on
	// the top-level Config — which is serialized by plain yaml.Marshal (Save)
	// and unmarshalled by viper — rather than on SettingsConfig, whose custom
	// MarshalYAML shadow struct would silently drop new fields.
	Assay AssayConfig `mapstructure:"assay" yaml:"assay,omitempty"`
	// SelfDeploy configures Forge's automatic rebuild-and-restart of its own
	// daemon binary after a merge lands on its repository. Disabled by default.
	// Top-level (like Assay) so it survives the SettingsConfig MarshalYAML
	// shadow struct.
	SelfDeploy SelfDeployConfig `mapstructure:"self_deploy" yaml:"self_deploy,omitempty"`
}

// SelfDeployConfig gates and parameterises Forge self-deploy: rebuilding and
// restarting the daemon's own binary when a PR merges on Forge's repository.
// Disabled by default — the whole flow is inert unless Enabled is true and an
// Anvil is named.
type SelfDeployConfig struct {
	// Enabled turns the feature on. Default false: no pull/build/restart ever
	// happens while this is unset.
	Enabled bool `mapstructure:"enabled" yaml:"enabled,omitempty"`
	// Anvil is the name of the registered anvil that is Forge's own repository.
	// A merge is only acted on when its event anvil matches this. Required when
	// Enabled — an empty value leaves the feature inert.
	Anvil string `mapstructure:"anvil" yaml:"anvil,omitempty"`
	// RepoPath is the source checkout to `git pull` and build from. When empty
	// the daemon falls back to the configured anvil's path.
	RepoPath string `mapstructure:"repo_path" yaml:"repo_path,omitempty"`
	// BinaryPath is the live binary replaced on deploy. Defaults to ~/bin/forge.
	BinaryPath string `mapstructure:"binary_path" yaml:"binary_path,omitempty"`
	// UnitName is the systemd unit restarted after the swap. Defaults to "forge".
	UnitName string `mapstructure:"unit_name" yaml:"unit_name,omitempty"`
	// RestartCommand is the executable used to restart the unit. Defaults to
	// "systemctl". Set it to (for example) "sudo" when the daemon runs as an
	// unprivileged user that needs elevation to restart a system unit — pair it
	// with RestartArgs: ["systemctl"] so the final invocation is
	// `sudo systemctl restart <unit>`.
	RestartCommand string `mapstructure:"restart_command" yaml:"restart_command,omitempty"`
	// RestartArgs are arguments inserted before "restart <unit>". Use it to wire
	// the invocation to the daemon's privileges/user context — e.g. ["--user"]
	// for a `systemctl --user` unit, or ["systemctl"] when RestartCommand is
	// "sudo". Empty (the default) yields a plain `systemctl restart <unit>`.
	RestartArgs []string `mapstructure:"restart_args" yaml:"restart_args,omitempty"`
	// Branch is the base branch a merge must target to trigger a deploy, and the
	// branch pulled before building. Defaults to "main".
	Branch string `mapstructure:"branch" yaml:"branch,omitempty"`
	// BuildTarget is the `go build` package target. Defaults to "./cmd/forge".
	BuildTarget string `mapstructure:"build_target" yaml:"build_target,omitempty"`
	// MaxDrainWait bounds how long the deploy waits for active workers to finish
	// after pausing dispatch before giving up. The drain check is retried on a
	// short ticker for the whole window rather than sampled once, so a deploy
	// triggered while a Smith is mid-run lands in the gap as soon as it opens.
	// Defaults to 30m.
	MaxDrainWait time.Duration `mapstructure:"max_drain_wait" yaml:"max_drain_wait,omitempty"`
	// DrainTimeout is the former name of MaxDrainWait, still read so existing
	// configs keep working. MaxDrainWait wins when both are set.
	//
	// Deprecated: use MaxDrainWait (self_deploy.max_drain_wait).
	DrainTimeout time.Duration `mapstructure:"drain_timeout" yaml:"drain_timeout,omitempty"`
}

// DefaultSelfDeployMaxDrainWait is the fallback used when neither MaxDrainWait
// nor the deprecated DrainTimeout is set.
const DefaultSelfDeployMaxDrainWait = 30 * time.Minute

// DefaultSelfDeployDrainTimeout is the former name of
// DefaultSelfDeployMaxDrainWait.
//
// Deprecated: use DefaultSelfDeployMaxDrainWait.
const DefaultSelfDeployDrainTimeout = DefaultSelfDeployMaxDrainWait

// ResolvedBinaryPath returns the configured binary path with a leading "~"
// expanded to the user's home directory, defaulting to ~/bin/forge.
func (s SelfDeployConfig) ResolvedBinaryPath() string {
	p := s.BinaryPath
	if p == "" {
		p = filepath.Join("~", "bin", "forge")
	}
	return expandHomePath(p)
}

// ResolvedRepoPath returns the source checkout to build from. When RepoPath is
// unset it falls back to the given anvil path (typically the self_deploy anvil).
func (s SelfDeployConfig) ResolvedRepoPath(anvilPath string) string {
	p := s.RepoPath
	if p == "" {
		p = anvilPath
	}
	return expandHomePath(p)
}

// ResolvedUnitName returns the systemd unit name, defaulting to "forge".
func (s SelfDeployConfig) ResolvedUnitName() string {
	if s.UnitName == "" {
		return "forge"
	}
	return s.UnitName
}

// ResolvedRestartCommand returns the restart executable, defaulting to
// "systemctl". Paired with RestartArgs, this wires the restart invocation to the
// daemon's privileges/user context (e.g. "sudo" + ["systemctl"], or "systemctl"
// + ["--user"]).
func (s SelfDeployConfig) ResolvedRestartCommand() string {
	if s.RestartCommand == "" {
		return "systemctl"
	}
	return s.RestartCommand
}

// ResolvedBranch returns the branch a merge must target, defaulting to "main".
func (s SelfDeployConfig) ResolvedBranch() string {
	if s.Branch == "" {
		return "main"
	}
	return s.Branch
}

// ResolvedBuildTarget returns the go build target, defaulting to "./cmd/forge".
func (s SelfDeployConfig) ResolvedBuildTarget() string {
	if s.BuildTarget == "" {
		return "./cmd/forge"
	}
	return s.BuildTarget
}

// ResolvedMaxDrainWait returns how long a deploy may wait for workers to drain:
// MaxDrainWait when set, else the deprecated DrainTimeout, else the 30m default.
// A zero or negative value on either field falls through to the next candidate,
// so callers never have to handle "unset".
func (s SelfDeployConfig) ResolvedMaxDrainWait() time.Duration {
	if s.MaxDrainWait > 0 {
		return s.MaxDrainWait
	}
	if s.DrainTimeout > 0 {
		return s.DrainTimeout
	}
	return DefaultSelfDeployMaxDrainWait
}

// ResolvedDrainTimeout returns the resolved maximum drain wait.
//
// Deprecated: use ResolvedMaxDrainWait.
func (s SelfDeployConfig) ResolvedDrainTimeout() time.Duration {
	return s.ResolvedMaxDrainWait()
}

// expandHomePath expands a leading "~" (or "~/") in p to the current user's
// home directory. It is a best-effort helper: if the home directory cannot be
// resolved the original path is returned unchanged.
func expandHomePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
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
	// PreviewEnabled controls whether Kiln preview environments may be
	// started for this anvil. When nil (default), the global
	// settings.preview_enabled applies. Set to false to opt this anvil out
	// entirely. An anvil without a .forge/preview.yaml manifest offers no
	// preview regardless of this setting.
	PreviewEnabled *bool `mapstructure:"preview_enabled" yaml:"preview_enabled,omitempty"`
	// PreviewAuto opts this anvil into starting previews without being asked.
	// Empty or "off" (the default) means previews only start on request;
	// "ready_to_merge" starts one when Bellows announces a PR ready to merge.
	// Automatic starts obey the same limits as manual ones: previews must be
	// enabled for the anvil, the anvil needs a .forge/preview.yaml manifest,
	// preview_max_concurrent still caps them, and the idle reaper still
	// collects them. Default off because a preview costs real memory for as
	// long as it runs.
	PreviewAuto string `mapstructure:"preview_auto" yaml:"preview_auto,omitempty"`
	// PreviewQuests opts this anvil into running its QuestGiver E2E quests
	// against a running preview environment instead of the anvil's fixed
	// quest URL. Default false: a quest run drives a real browser against
	// whatever the preview serves, so it only happens for anvils that asked
	// for it. Requires previews to be enabled for the anvil — validation
	// rejects preview_quests: true on an anvil that cannot run previews.
	PreviewQuests bool `mapstructure:"preview_quests" yaml:"preview_quests,omitempty"`
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

	// Assay holds a per-anvil overlay over the global Assay configuration.
	// When set, its non-zero fields override the corresponding global values
	// via Config.ResolvedAssay. When nil (default), the global Assay applies.
	Assay *AssayConfig `mapstructure:"assay" yaml:"assay,omitempty"`
}

// AnvilSettings is the serializable projection of an anvil's per-anvil
// settings exposed by the config API (GET /api/forge/config), keyed as
// anvils.<name>.<key>. It is the documented JSON contract that the config
// write path persists and the settings UI consumes.
//
// The *bool fields are tri-state: a non-nil pointer serializes to the literal
// JSON true/false, while nil serializes to JSON null. Null means "inherit /
// unset" — the anvil has no explicit override and the corresponding global
// setting (or built-in default) applies. This distinction must survive the
// JSON round-trip, so callers must NOT collapse nil to false.
//
// AutoMerge, PreviewQuests and WicketAutoDispatch are plain booleans (no
// inherit semantics); they are always present as true/false.
type AnvilSettings struct {
	AutoMerge          bool   `json:"auto_merge"`
	SchematicEnabled   *bool  `json:"schematic_enabled"`
	GolangciLint       *bool  `json:"golangci_lint"`
	GoRaceDetection    *bool  `json:"go_race_detection"`
	DepcheckEnabled    *bool  `json:"depcheck_enabled"`
	QuestgiverEnabled  *bool  `json:"questgiver_enabled"`
	PreviewEnabled     *bool  `json:"preview_enabled"`
	PreviewAuto        string `json:"preview_auto"`
	PreviewQuests      bool   `json:"preview_quests"`
	WicketEnabled      *bool  `json:"wicket_enabled"`
	WicketAutoDispatch bool   `json:"wicket_auto_dispatch"`

	// Non-boolean per-anvil scalars (Forge-85wn). Plain values (no inherit
	// semantics): MaxSmiths caps this anvil's concurrent workers; AutoDispatch
	// selects the dispatch mode; AutoDispatchTag/MinPriority parameterise it;
	// Platform selects the VCS provider.
	MaxSmiths               int    `json:"max_smiths"`
	AutoDispatch            string `json:"auto_dispatch"`
	AutoDispatchTag         string `json:"auto_dispatch_tag"`
	AutoDispatchMinPriority int    `json:"auto_dispatch_min_priority"`
	Platform                string `json:"platform"`

	// Composite per-anvil overrides (Forge-vo5a). Nil slices/maps and the
	// empty WicketTriagePrompt serialize to JSON null / "" and mean
	// "inherit" — the anvil has no explicit override and the global setting
	// (or built-in default) applies. The fields lack omitempty so a nil map /
	// slice round-trips as an explicit null rather than being dropped.
	StageProviders     map[string][]string `json:"stage_providers"`
	WicketTrustedUsers []string            `json:"wicket_trusted_users"`
	WicketIgnoreUsers  []string            `json:"wicket_ignore_users"`
	WicketRepos        []string            `json:"wicket_repos"`
	WicketIssueLabels  []string            `json:"wicket_issue_labels"`
	WicketTriagePrompt string              `json:"wicket_triage_prompt"`
}

// AnvilSettingsMap projects every configured anvil into an AnvilSettings
// keyed by anvil name. The result is always non-nil (an empty map when no
// anvils are configured) so it marshals to "{}" rather than null. Tri-state
// *bool fields are deep-copied so map entries never alias the same pointer.
func (c *Config) AnvilSettingsMap() map[string]AnvilSettings {
	out := make(map[string]AnvilSettings, len(c.Anvils))
	for name, anvil := range c.Anvils {
		out[name] = AnvilSettings{
			AutoMerge:               anvil.AutoMerge,
			SchematicEnabled:        copyBool(anvil.SchematicEnabled),
			GolangciLint:            copyBool(anvil.GolangciLint),
			GoRaceDetection:         copyBool(anvil.GoRaceDetection),
			DepcheckEnabled:         copyBool(anvil.DepcheckEnabled),
			QuestgiverEnabled:       copyBool(anvil.QuestgiverEnabled),
			PreviewEnabled:          copyBool(anvil.PreviewEnabled),
			PreviewAuto:             anvil.PreviewAuto,
			PreviewQuests:           anvil.PreviewQuests,
			WicketEnabled:           copyBool(anvil.WicketEnabled),
			WicketAutoDispatch:      anvil.WicketAutoDispatch,
			MaxSmiths:               anvil.MaxSmiths,
			AutoDispatch:            anvil.AutoDispatch,
			AutoDispatchTag:         anvil.AutoDispatchTag,
			AutoDispatchMinPriority: anvil.AutoDispatchMinPriority,
			Platform:                anvil.Platform,
			StageProviders:          copyStringSliceMap(anvil.StageProviders),
			WicketTrustedUsers:      copyStringSlice(anvil.WicketTrustedUsers),
			WicketIgnoreUsers:       copyStringSlice(anvil.WicketIgnoreUsers),
			WicketRepos:             copyStringSlice(anvil.WicketRepos),
			WicketIssueLabels:       copyStringSlice(anvil.WicketIssueLabels),
			WicketTriagePrompt:      anvil.WicketTriagePrompt,
		}
	}
	return out
}

// copyStringSlice returns a copy of s, or nil when s is nil/empty, preserving
// the nil ("inherit/unset") distinction for the per-anvil settings projection
// and ensuring map entries never alias the source slice.
func copyStringSlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// copyStringSliceMap deep-copies a map[string][]string, returning nil when m is
// nil/empty so it serializes as JSON null (inherit) and no entry aliases the
// source slices.
func copyStringSliceMap(m map[string][]string) map[string][]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = copyStringSlice(v)
	}
	return out
}

// copyBool returns a new pointer to a copy of *b, or nil when b is nil. It
// preserves the tri-state nil ("inherit/unset") semantics while ensuring no
// two AnvilSettings entries share the same underlying pointer.
func copyBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
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
	// TolerateHostCrash, when true, re-classifies a non-zero exit from this
	// step as a pass IF the output shows a completed all-passed .NET test
	// summary AND an explicit test-host crash/abort marker. It exists for the
	// api-test step on repos whose .NET test host occasionally OOMs/crashes at
	// teardown after every test has already passed (VSTest then returns
	// non-zero with no failing test), producing false temper failures. A real
	// test failure (Failed: N>0) or a build error (no crash marker) still fails.
	TolerateHostCrash bool `mapstructure:"tolerate_host_crash" yaml:"tolerate_host_crash,omitempty"`
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
	// immediately re-claim it. It is also the duration of the daemon's global
	// rate-limit hold: any worker observing every provider rate limited defers
	// ALL automatic AI dispatch (new beads, CI/review fixes, Assay) for this
	// long, without consuming attempt budgets. Defaults to 5 minutes.
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
	//
	// The gate accounts for in-flight (not-yet-recorded) spend: it projects the
	// sum of active workers' reserved estimates plus one per-worker estimate on
	// top of the recorded total, so N concurrent workers cannot overshoot the
	// limit by roughly N × per-bead cost. See PerWorkerCostEstimate.
	DailyCostLimit float64 `mapstructure:"daily_cost_limit" yaml:"daily_cost_limit,omitempty"`
	// PerWorkerCostEstimate is the floor (USD) used to estimate the in-flight
	// spend of a single not-yet-completed worker when projecting against
	// DailyCostLimit. The daemon maintains a rolling average of recorded
	// per-bead cost and uses max(rolling average, this floor) so the estimate
	// is never zero before any cost data exists. A value <= 0 falls back to
	// DefaultPerWorkerCostEstimate. Only relevant when daily_cost_limit > 0.
	PerWorkerCostEstimate float64 `mapstructure:"per_worker_cost_estimate" yaml:"per_worker_cost_estimate,omitempty"`
	// MaxCIFixAttempts is the maximum number of CI fix cycles per PR before
	// the PR is considered exhausted. Default: 5.
	MaxCIFixAttempts int `mapstructure:"max_ci_fix_attempts" yaml:"max_ci_fix_attempts"`
	// MaxReviewFixAttempts is the maximum number of review fix cycles per PR
	// before the PR is considered exhausted. Default: 5.
	MaxReviewFixAttempts int `mapstructure:"max_review_fix_attempts" yaml:"max_review_fix_attempts"`
	// MaxSameHeadReviewFixes bounds review fix cycles dispatched against one
	// UNCHANGED PR head. max_review_fix_attempts bounds the PR's whole life and
	// never resets, so it cannot distinguish a PR that is progressing (each
	// round pushes a new head) from one rebuilding the identical diff every
	// Bellows cycle — the second is what burns a full Smith run per cycle for
	// nothing. Exceeding this raises a Needs Attention entry naming the PR,
	// head SHA and attempt count instead of dispatching again; the count resets
	// as soon as the head moves. Default: 2. A value <= 0 falls back to the
	// default — the breaker cannot be disabled from config, only widened.
	MaxSameHeadReviewFixes int `mapstructure:"max_same_head_review_fixes" yaml:"max_same_head_review_fixes"`
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
	// BurnishVerifyRetries is how many EXTRA verification runs a burnish
	// attempt gets after the first one exceeds burnish_verify_timeout. A
	// timeout is usually a wedged test process rather than a genuinely slow
	// suite, so one clean re-run resolves most of them.
	//
	// When every run times out, burnish does NOT discard the fix: it pushes the
	// commit marked unverified (burnish verification is advisory — the fix
	// lands on a PR that humans, Copilot and Assay review anyway) and raises a
	// Needs Attention entry. Only if that push also fails is the worktree
	// preserved instead of removed, with the dangling SHA named. Default: 1.
	// Omitting the field (or setting it to 0) uses the default; set a negative
	// value to fall straight through to the unverified push without re-running.
	BurnishVerifyRetries int `mapstructure:"burnish_verify_retries" yaml:"burnish_verify_retries"`
	// MaxRebaseAttempts is the maximum number of conflict rebase attempts per
	// PR before the PR is considered exhausted. Default: 3.
	MaxRebaseAttempts int `mapstructure:"max_rebase_attempts" yaml:"max_rebase_attempts"`
	// MaxLifecycleWorkers caps how many lifecycle/bellows fix workers
	// (quench/cifix, burnish/reviewfix, rebase, assay) may run concurrently
	// across all PRs and anvils. Each fix worker spawns its own Claude session
	// of comparable length to a Smith, and they are deliberately excluded from
	// the max_total_smiths dispatch cap (see state.ActiveDispatchWorkers), so
	// without their own ceiling a burst of stuck PRs can fan out unbounded
	// Claude sessions and OOM the host (Forge-3m06). A value <= 0 falls back to
	// DefaultMaxLifecycleWorkers. Default: 2.
	MaxLifecycleWorkers int `mapstructure:"max_lifecycle_workers" yaml:"max_lifecycle_workers"`
	// MergeStrategy controls how PRs are merged from the Hearth TUI.
	// Valid values: "squash" (default), "merge", "rebase".
	MergeStrategy string `mapstructure:"merge_strategy" yaml:"merge_strategy,omitempty"`
	// EmptyDiffAction controls what happens when a pipeline run is approved but
	// its branch has no commits relative to the base branch — the work already
	// landed on main (e.g. a sibling PR shipped the same change first). Opening
	// a PR in that state always fails ("No commits between main and
	// forge/<bead>") and every retry reproduces the same empty branch, so the
	// outcome is terminal either way. Valid values: "attention" (default —
	// raise a Needs Attention entry for the operator) and "close" (close the
	// bead with a note). Unrecognised values fall back to "attention".
	// See EmptyDiffAction / ResolveEmptyDiffAction.
	EmptyDiffAction string `mapstructure:"empty_diff_action" yaml:"empty_diff_action,omitempty"`
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
	// AnvilHealthCheck controls the wedged-anvil check run once per full poll
	// per anvil: a single dolt_conflicts query that detects a beads database
	// left mid-merge with unresolved conflicts (every bd write against such an
	// anvil is rolled back). When wedged, the anvil is surfaced in
	// needs-attention and skipped for dispatch until the conflicts clear.
	// Default: true.
	AnvilHealthCheck *bool `mapstructure:"anvil_health_check" yaml:"anvil_health_check,omitempty"`
	// LogRetentionDays is how many days a preserved bead-log directory under
	// ~/.forge/logs/<beadID>/ is kept after its newest file. The retention
	// sweep removes older directories (unless the bead has a running worker).
	// 0 disables the sweep entirely. Default: 30.
	LogRetentionDays int `mapstructure:"log_retention_days" yaml:"log_retention_days"`
	// LogSweepInterval is how often the preserved bead-log retention sweep
	// runs. Hot-reloadable. Default: 24h (daily).
	LogSweepInterval time.Duration `mapstructure:"log_sweep_interval" yaml:"log_sweep_interval,omitempty"`
	// GoRaceDetection enables the Go race detector (-race flag) as a
	// separate temper step globally. Per-anvil settings override this.
	// Default: false.
	GoRaceDetection bool `mapstructure:"go_race_detection" yaml:"go_race_detection"`
	// TemperStepTimeout is the default timeout applied to a Temper verification
	// step whose own per-step timeout is unset. A per-step Timeout still
	// overrides this. Raising it lets long-but-legitimate test suites finish
	// instead of being killed and reported as a phantom failure. Default: 5m.
	TemperStepTimeout time.Duration `mapstructure:"temper_step_timeout" yaml:"temper_step_timeout,omitempty"`
	// TemperGitTimeout is the timeout for internal git invocations made during
	// Temper verification (e.g. the VerifyClean status check). Default: 30s.
	TemperGitTimeout time.Duration `mapstructure:"temper_git_timeout" yaml:"temper_git_timeout,omitempty"`
	// WorktreeGitTimeout is the timeout for checkout-heavy git invocations made
	// while preparing a worker worktree: `git worktree add`, `fetch`, `push`,
	// `checkout`, `reset`, `clean` and `submodule`. Cheap metadata commands
	// (rev-parse, show-ref, branch, config) keep their own tight 60s bound and
	// are unaffected. A cold full-tree checkout of a large anvil under
	// memory/disk pressure legitimately exceeds a minute, and the deadline
	// SIGKILLs git, so too low a value burns a bead's first attempt. A value
	// <= 0 falls back to worktree.DefaultGitCheckoutTimeout. Default: 5m.
	WorktreeGitTimeout time.Duration `mapstructure:"worktree_git_timeout" yaml:"worktree_git_timeout,omitempty"`
	// BdTimeout is the timeout applied to every `bd` subprocess Forge spawns
	// (ready/list/show/create/update/close/sql). bd talks to a Dolt backend
	// that may be remote, so a single write can legitimately take tens of
	// seconds; the deadline SIGKILLs bd, which used to surface as a bare
	// "signal: killed" with no hint that a deadline had fired. A value <= 0
	// falls back to executil.DefaultBdTimeout. Default: 5m.
	BdTimeout time.Duration `mapstructure:"bd_timeout" yaml:"bd_timeout,omitempty"`
	// TemperOutputCap is the maximum number of bytes of combined stdout+stderr
	// retained per Temper step. Output beyond the cap is head+tail truncated
	// with an elision marker, bounding both memory and the warden/fix prompt
	// that embeds the output. A value <= 0 falls back to the package default
	// (256 KiB). Default: 262144.
	TemperOutputCap int `mapstructure:"temper_output_cap" yaml:"temper_output_cap,omitempty"`
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

	// PreviewEnabled is the master gate for Kiln preview environments. When
	// false (default), no preview can be started regardless of per-anvil
	// settings or the presence of a .forge/preview.yaml manifest.
	PreviewEnabled bool `mapstructure:"preview_enabled" yaml:"preview_enabled"`
	// PreviewMaxConcurrent caps how many previews may run at once. Previews
	// cost real memory (a database, an API and a dev server each), so the cap
	// is deliberately low. 0 means DefaultPreviewMaxConcurrent.
	//
	// Reaching the cap rejects the start rather than queueing it: the error
	// names the limit and the beads holding the slots, so the operator can
	// stop one and try again. Set PreviewEvictLRU to trade that rejection for
	// automatic eviction.
	PreviewMaxConcurrent int `mapstructure:"preview_max_concurrent" yaml:"preview_max_concurrent,omitempty"`
	// PreviewEvictLRU decides what happens when a preview is requested while
	// preview_max_concurrent is already reached. False (the default) rejects
	// the request and names the previews holding the slots. True stops the
	// least recently used preview to make room — convenient on a box where
	// previews are opened and abandoned, at the cost of a preview someone may
	// still have open in a tab disappearing under them.
	PreviewEvictLRU bool `mapstructure:"preview_evict_lru" yaml:"preview_evict_lru,omitempty"`
	// PreviewIdleTimeout is how long a preview may go unused before it is
	// torn down. 0 disables the idle reaper (previews then run until stopped
	// or until their PR merges/closes). Defaults to 30m.
	PreviewIdleTimeout time.Duration `mapstructure:"preview_idle_timeout" yaml:"preview_idle_timeout,omitempty"`
	// PreviewPortRange is the inclusive "min-max" TCP port range previews
	// allocate service ports from. Defaults to DefaultPreviewPortRange.
	PreviewPortRange string `mapstructure:"preview_port_range" yaml:"preview_port_range,omitempty"`
	// PreviewBindHost is the address preview services bind to. Defaults to
	// 127.0.0.1 (loopback only); set it to 0.0.0.0 to reach previews from a
	// LAN or VPN. Manifests reference it as {{.BindHost}} so a service is told
	// what to listen on instead of hardcoding an address that disagrees with
	// this setting. Preview URLs bypass the Hearth login, so widen this only on
	// a trusted network.
	PreviewBindHost string `mapstructure:"preview_bind_host" yaml:"preview_bind_host,omitempty"`
	// PreviewPublicHost is the hostname used when displaying preview links
	// (e.g. the box's WireGuard or LAN name). Empty means PreviewBindHost.
	PreviewPublicHost string `mapstructure:"preview_public_host" yaml:"preview_public_host,omitempty"`
	// PreviewProxyBase is the DNS suffix previews are addressed under when
	// Forge fronts them by hostname instead of by port: a bead's preview
	// answers on "<label>.<base>" and one of its services on
	// "<label>--<service>.<base>", where <label> is kiln.PreviewLabel of the
	// bead id. Empty (the default) switches host-based routing off entirely
	// and leaves preview links on host:port.
	//
	// It is a bare DNS name — no scheme, no port, no leading dot — and is
	// lowercased on validation. A wildcard record (*.preview.example.test)
	// pointing at the Forge box is what makes it resolve.
	PreviewProxyBase string `mapstructure:"preview_proxy_base" yaml:"preview_proxy_base,omitempty"`
	// PreviewProxyAuth decides whether a request arriving on a preview
	// hostname has to prove it belongs to a signed-in Hearth operator.
	//
	// "session" (the default, and what an empty value means) gates every
	// proxied request on a Hearth web session — either the session cookie
	// itself when preview_proxy_base shares a registrable suffix with the
	// Hearth host, or a short-lived signed token minted into the preview link
	// and exchanged for a preview-scoped cookie when it does not.
	//
	// "none" turns the gate off and serves previews to anyone who can resolve
	// the hostname. That is the posture the raw host:port previews already had,
	// so it is the honest opt-out for a trusted network — and an explicit one,
	// because on a box reachable from anywhere else it hands unauthenticated
	// strangers a branch build.
	PreviewProxyAuth string `mapstructure:"preview_proxy_auth" yaml:"preview_proxy_auth,omitempty"`

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

	// BusEnabled toggles the in-process event Bus. When true, the daemon
	// constructs a state.Bus (buffered to BusBufferSize) and wires it into the
	// state DB so every logged event is fanned out to real-time subscribers
	// (SSE/IPC). When false (the default, for safe rollout), no Bus is
	// constructed and the DB's Publish path no-ops, retaining the legacy
	// polling behaviour where consumers re-read events via EventsSince.
	// This is the single switch sibling SSE/IPC consumers gate on.
	BusEnabled bool `mapstructure:"bus_enabled" yaml:"bus_enabled"`
	// BusBufferSize is the per-subscriber channel buffer for the in-process
	// event Bus. It bounds how many events a slow consumer can fall behind
	// before the Bus drops the oldest and delivers a gap marker prompting a
	// re-sync. Only relevant when BusEnabled is true. A value <= 0 falls back
	// to DefaultBusBufferSize. Default: 256.
	BusBufferSize int `mapstructure:"bus_buffer_size" yaml:"bus_buffer_size,omitempty"`

	// SSEPollFallback forces the /api/activity/stream SSE endpoint back onto
	// the legacy 2s polling loop even when the in-process event Bus is enabled.
	// It is a one-release safety valve: if the bus-based replay-then-live path
	// misbehaves in production, set this to true to revert just the activity
	// stream to polling without disabling the Bus for other consumers. When
	// false (the default) the endpoint uses the Bus whenever one is wired.
	//
	// DEPRECATED: this fallback path is scheduled for removal in the next
	// release once the bus-based activity stream has proven stable. Do not
	// build new behaviour on top of it.
	SSEPollFallback bool `mapstructure:"sse_poll_fallback" yaml:"sse_poll_fallback,omitempty"`

	// Warden holds review-time rule filtering settings. These control how many
	// learned warden rules are injected into the Warden review prompt and
	// which filter passes are applied.
	Warden WardenSettings `mapstructure:"warden" yaml:"warden,omitempty"`

	// ForgeChat configures the Beads-Forge per-turn AI loop (drafter, grilling,
	// plan, emit). Currently exposes turn_timeout so operators can lift the
	// per-turn budget without recompiling.
	ForgeChat ForgeChatSettings `mapstructure:"forgechat" yaml:"forgechat,omitempty"`

	// Pricing maps a model key to its per-million-token USD rates, used as the
	// fallback cost estimate for providers that do not self-report a cost
	// (Copilot, Gemini, OpenAI/Codex). Claude self-reports total_cost_usd and
	// is unaffected. Each entry overrides the built-in default for that model;
	// models not listed keep their default rates. Hot-reloadable. See
	// cost.DefaultPricingTable for the default keys (claude-sonnet,
	// claude-haiku, claude-opus, gemini, openai) and values.
	//
	// mapstructure:"-" excludes this from viper decoding — model keys often
	// contain dots (e.g. "claude-opus-4.6"), which viper treats as nested-key
	// delimiters and would mangle. Load() populates it directly from the raw
	// YAML instead; see loadPricingTablesFromYAML.
	Pricing map[string]ModelPricing `mapstructure:"-" yaml:"pricing,omitempty"`

	// CopilotPremiumMultipliers maps a Copilot model name to its premium-request
	// multiplier (e.g. claude-opus-4.6 = 3x). Each entry overrides the built-in
	// default for that model; models not listed keep their default multiplier.
	// Hot-reloadable. See cost.DefaultCopilotPremiumMultipliers for defaults.
	//
	// mapstructure:"-" excludes this from viper decoding — see the note on
	// Pricing above; keys like "claude-opus-4.6" would otherwise be mangled by
	// viper's "." key delimiter. Load() populates it from the raw YAML.
	CopilotPremiumMultipliers map[string]float64 `mapstructure:"-" yaml:"copilot_premium_multipliers,omitempty"`
}

// ModelPricing defines per-million-token USD rates for a single model, used by
// the fallback cost estimator when a provider does not self-report its cost.
// It mirrors cost.Pricing; the daemon converts between the two when pushing
// settings.pricing into the cost package.
type ModelPricing struct {
	InputPerM      float64 `mapstructure:"input_per_m" yaml:"input_per_m"`
	OutputPerM     float64 `mapstructure:"output_per_m" yaml:"output_per_m"`
	CacheReadPerM  float64 `mapstructure:"cache_read_per_m" yaml:"cache_read_per_m,omitempty"`
	CacheWritePerM float64 `mapstructure:"cache_write_per_m" yaml:"cache_write_per_m,omitempty"`
}

// DefaultMaxLifecycleWorkers is the fallback concurrency cap for lifecycle/bellows
// fix workers (quench/burnish/rebase/assay) when settings.max_lifecycle_workers is
// unset or <= 0. Kept deliberately small because each lifecycle worker spawns its
// own Claude session and they are not counted against the max_total_smiths dispatch
// cap; an unbounded fan-out previously OOM-crashed the host (Forge-3m06).
const DefaultMaxLifecycleWorkers = 2

// DefaultPerWorkerCostEstimate is the fallback per-worker in-flight cost estimate
// (USD) used by the daily_cost_limit gate when settings.per_worker_cost_estimate
// is unset or <= 0 and no rolling per-bead cost average has accumulated yet. It is
// deliberately conservative so the gate reserves a non-zero amount for each active
// worker from the very first dispatch, preventing the "N concurrent workers blow
// past the limit by ~N × per-bead cost" overshoot (Forge-s3w7).
const DefaultPerWorkerCostEstimate = 2.0

// DefaultBusBufferSize is the fallback per-subscriber buffer for the in-process
// event Bus, used when settings.bus_buffer_size is unset or <= 0. It mirrors the
// historical daemon default so enabling the Bus without tuning the buffer keeps
// the previously-hardcoded behaviour.
const DefaultBusBufferSize = 256

// ResolvedBusBufferSize returns the effective event-Bus per-subscriber buffer
// size, applying DefaultBusBufferSize when BusBufferSize is unset or <= 0.
// Callers wiring the Bus should use this rather than reading BusBufferSize
// directly so an omitted value never collapses to state.NewBus's minimum of 1.
func (s SettingsConfig) ResolvedBusBufferSize() int {
	if s.BusBufferSize <= 0 {
		return DefaultBusBufferSize
	}
	return s.BusBufferSize
}

// DefaultTemperOutputCap is the fallback per-step combined-output byte cap
// (256 KiB) used when settings.temper_output_cap is unset or <= 0. It mirrors
// temper.DefaultOutputCap; kept here to avoid a config→temper import cycle.
const DefaultTemperOutputCap = 256 * 1024

// MaxForgeChatTurnTimeout is the hard upper bound for settings.forgechat.turn_timeout.
// Values above this are clamped on load and a warning is logged. Picked so that
// even a worst-case grilling turn returns to the user before the browser /
// reverse-proxy gives up on the long-poll HTTP request.
const MaxForgeChatTurnTimeout = 15 * time.Minute

// DefaultForgeChatTurnTimeout is the default wall-clock budget for a single
// forgechat turn. Mirrored as forgechat.defaultTurnTimeout — keep the two in
// sync if either is changed.
const DefaultForgeChatTurnTimeout = 5 * time.Minute

// DefaultForgeChatTurnExpiry is how long a completed (or errored) turn is
// retained in the process-local TurnStore before garbage collection drops it.
// Once dropped, a reconnecting SSE client receives a graceful "turn expired"
// event and refetches the canonical messages rather than seeing a 404.
const DefaultForgeChatTurnExpiry = 30 * time.Minute

// DefaultForgeChatTurnRetentionCap is the maximum number of turns retained in
// the TurnStore. When exceeded, the oldest completed turns are evicted first.
// A non-positive configured value disables the cap.
const DefaultForgeChatTurnRetentionCap = 1000

// ForgeChatSettings configures the Beads-Forge per-turn AI loop.
type ForgeChatSettings struct {
	// TurnTimeout caps the wall-clock duration of a single forgechat turn.
	// Defaults to DefaultForgeChatTurnTimeout (5m). Values above
	// MaxForgeChatTurnTimeout (15m) are clamped on load with a slog.Warn.
	// Zero/unset falls back to the default at runtime.
	TurnTimeout time.Duration `mapstructure:"turn_timeout" yaml:"turn_timeout,omitempty"`
	// TurnExpiry is how long a completed turn stays in the in-memory
	// TurnStore before garbage collection removes it. Zero/unset falls back
	// to DefaultForgeChatTurnExpiry (30m).
	TurnExpiry time.Duration `mapstructure:"turn_expiry" yaml:"turn_expiry,omitempty"`
	// TurnRetentionCap caps the number of turns retained in the TurnStore;
	// the oldest completed turns are evicted once the cap is exceeded. Zero
	// falls back to DefaultForgeChatTurnRetentionCap (1000); a negative value
	// disables the cap entirely.
	TurnRetentionCap int `mapstructure:"turn_retention_cap" yaml:"turn_retention_cap,omitempty"`
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

// ResolvedTurnExpiry returns the effective TurnStore expiry after applying the
// default. Zero/unset (and negative) values fall back to
// DefaultForgeChatTurnExpiry.
func (f ForgeChatSettings) ResolvedTurnExpiry() time.Duration {
	if f.TurnExpiry <= 0 {
		return DefaultForgeChatTurnExpiry
	}
	return f.TurnExpiry
}

// ResolvedTurnRetentionCap returns the effective TurnStore retention cap. Zero
// (unset) falls back to DefaultForgeChatTurnRetentionCap; a negative value is
// returned as-is so callers can treat it as "unlimited / cap disabled".
func (f ForgeChatSettings) ResolvedTurnRetentionCap() int {
	if f.TurnRetentionCap == 0 {
		return DefaultForgeChatTurnRetentionCap
	}
	return f.TurnRetentionCap
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
	TurnTimeout      string `yaml:"turn_timeout,omitempty"`
	TurnExpiry       string `yaml:"turn_expiry,omitempty"`
	TurnRetentionCap int    `yaml:"turn_retention_cap,omitempty"`
}

// MarshalYAML serialises SettingsConfig with time.Duration fields as
// human-readable strings (e.g. "30s", "5m0s") instead of nanosecond ints.
func (s SettingsConfig) MarshalYAML() (interface{}, error) {
	// Shadow struct with durations replaced by strings.
	type shadow struct {
		PollInterval              string              `yaml:"poll_interval"`
		SmithTimeout              string              `yaml:"smith_timeout"`
		MaxTotalSmiths            int                 `yaml:"max_total_smiths"`
		MaxReviewAttempts         int                 `yaml:"max_review_attempts"`
		MaxPipelineIterations     int                 `yaml:"max_pipeline_iterations"`
		ClaudeFlags               []string            `yaml:"claude_flags"`
		Providers                 []string            `yaml:"providers,omitempty"`
		RateLimitBackoff          string              `yaml:"rate_limit_backoff"`
		SmithProviders            []string            `yaml:"smith_providers,omitempty"`
		StageProviders            map[string][]string `yaml:"stage_providers,omitempty"`
		SchematicEnabled          bool                `yaml:"schematic_enabled"`
		SchematicWordThreshold    int                 `yaml:"schematic_word_threshold,omitempty"`
		BellowsInterval           string              `yaml:"bellows_interval"`
		DailyCostLimit            float64             `yaml:"daily_cost_limit,omitempty"`
		PerWorkerCostEstimate     float64             `yaml:"per_worker_cost_estimate,omitempty"`
		MaxCIFixAttempts          int                 `yaml:"max_ci_fix_attempts"`
		MaxReviewFixAttempts      int                 `yaml:"max_review_fix_attempts"`
		MaxSameHeadReviewFixes    int                 `yaml:"max_same_head_review_fixes,omitempty"`
		MaxRebaseAttempts         int                 `yaml:"max_rebase_attempts"`
		MaxLifecycleWorkers       int                 `yaml:"max_lifecycle_workers"`
		BurnishVerifyTimeout      string              `yaml:"burnish_verify_timeout,omitempty"`
		BurnishVerifyRetries      int                 `yaml:"burnish_verify_retries,omitempty"`
		MergeStrategy             string              `yaml:"merge_strategy,omitempty"`
		EmptyDiffAction           string              `yaml:"empty_diff_action,omitempty"`
		StaleInterval             string              `yaml:"stale_interval"`
		DepcheckInterval          string              `yaml:"depcheck_interval,omitempty"`
		DepcheckTimeout           string              `yaml:"depcheck_timeout,omitempty"`
		VulncheckInterval         string              `yaml:"vulncheck_interval,omitempty"`
		VulncheckTimeout          string              `yaml:"vulncheck_timeout,omitempty"`
		VulncheckEnabled          *bool               `yaml:"vulncheck_enabled,omitempty"`
		AnvilHealthCheck          *bool               `yaml:"anvil_health_check,omitempty"`
		LogRetentionDays          int                 `yaml:"log_retention_days"`
		LogSweepInterval          string              `yaml:"log_sweep_interval,omitempty"`
		GoRaceDetection           bool                `yaml:"go_race_detection"`
		TemperStepTimeout         string              `yaml:"temper_step_timeout,omitempty"`
		TemperGitTimeout          string              `yaml:"temper_git_timeout,omitempty"`
		WorktreeGitTimeout        string              `yaml:"worktree_git_timeout,omitempty"`
		BdTimeout                 string              `yaml:"bd_timeout,omitempty"`
		TemperOutputCap           int                 `yaml:"temper_output_cap,omitempty"`
		AutoLearnRules            bool                `yaml:"auto_learn_rules"`
		CopilotDailyRequestLimit  int                 `yaml:"copilot_daily_request_limit,omitempty"`
		CrucibleEnabled           bool                `yaml:"crucible_enabled"`
		AutoMergeCrucibleChildren *bool               `yaml:"auto_merge_crucible_children,omitempty"`
		WardenModelOverride       string              `yaml:"warden_model_override,omitempty"`
		SchematicModelOverride    string              `yaml:"schematic_model_override,omitempty"`

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

		PreviewEnabled       bool   `yaml:"preview_enabled"`
		PreviewMaxConcurrent int    `yaml:"preview_max_concurrent,omitempty"`
		PreviewEvictLRU      bool   `yaml:"preview_evict_lru,omitempty"`
		PreviewIdleTimeout   string `yaml:"preview_idle_timeout,omitempty"`
		PreviewPortRange     string `yaml:"preview_port_range,omitempty"`
		PreviewBindHost      string `yaml:"preview_bind_host,omitempty"`
		PreviewPublicHost    string `yaml:"preview_public_host,omitempty"`
		PreviewProxyBase     string `yaml:"preview_proxy_base,omitempty"`
		PreviewProxyAuth     string `yaml:"preview_proxy_auth,omitempty"`

		WicketEnabled             bool                    `yaml:"wicket_enabled"`
		WicketInterval            string                  `yaml:"wicket_interval"`
		WicketProvider            string                  `yaml:"wicket_provider,omitempty"`
		WicketBatchSize           int                     `yaml:"wicket_batch_size,omitempty"`
		WicketProcessedLabel      string                  `yaml:"wicket_processed_label,omitempty"`
		WicketNeedsHumanLabel     string                  `yaml:"wicket_needs_human_label,omitempty"`
		WicketBeadCreatedLabel    string                  `yaml:"wicket_bead_created_label,omitempty"`
		WicketTriggerLabel        string                  `yaml:"wicket_trigger_label,omitempty"`
		WicketStaleDays           int                     `yaml:"wicket_stale_days,omitempty"`
		BdReadyLimit              int                     `yaml:"bd_ready_limit,omitempty"`
		CruciblePollInterval      string                  `yaml:"crucible_poll_interval,omitempty"`
		ForgeID                   string                  `yaml:"forge_id,omitempty"`
		Warden                    WardenSettings          `yaml:"warden,omitempty"`
		ForgeChat                 forgeChatShadow         `yaml:"forgechat,omitempty"`
		Pricing                   map[string]ModelPricing `yaml:"pricing,omitempty"`
		CopilotPremiumMultipliers map[string]float64      `yaml:"copilot_premium_multipliers,omitempty"`
	}

	sh := shadow{
		PollInterval:           durationString(s.PollInterval),
		SmithTimeout:           durationString(s.SmithTimeout),
		MaxTotalSmiths:         s.MaxTotalSmiths,
		MaxReviewAttempts:      s.MaxReviewAttempts,
		MaxPipelineIterations:  s.MaxPipelineIterations,
		ClaudeFlags:            s.ClaudeFlags,
		Providers:              s.Providers,
		RateLimitBackoff:       durationString(s.RateLimitBackoff),
		SmithProviders:         s.SmithProviders,
		StageProviders:         s.StageProviders,
		SchematicEnabled:       s.SchematicEnabled,
		SchematicWordThreshold: s.SchematicWordThreshold,
		BellowsInterval:        durationString(s.BellowsInterval),
		DailyCostLimit:         s.DailyCostLimit,
		PerWorkerCostEstimate:  s.PerWorkerCostEstimate,
		MaxCIFixAttempts:       s.MaxCIFixAttempts,
		MaxReviewFixAttempts:   s.MaxReviewFixAttempts,
		MaxSameHeadReviewFixes: s.MaxSameHeadReviewFixes,
		MaxRebaseAttempts:      s.MaxRebaseAttempts,
		MaxLifecycleWorkers:    s.MaxLifecycleWorkers,
		BurnishVerifyTimeout: func() string {
			if s.BurnishVerifyTimeout > 0 {
				return durationString(s.BurnishVerifyTimeout)
			}
			return ""
		}(),
		BurnishVerifyRetries:      s.BurnishVerifyRetries,
		MergeStrategy:             s.MergeStrategy,
		EmptyDiffAction:           s.EmptyDiffAction,
		StaleInterval:             durationString(s.StaleInterval),
		VulncheckEnabled:          s.VulncheckEnabled,
		AnvilHealthCheck:          s.AnvilHealthCheck,
		LogRetentionDays:          s.LogRetentionDays,
		GoRaceDetection:           s.GoRaceDetection,
		TemperOutputCap:           s.TemperOutputCap,
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

		PreviewEnabled:       s.PreviewEnabled,
		PreviewMaxConcurrent: s.PreviewMaxConcurrent,
		PreviewEvictLRU:      s.PreviewEvictLRU,
		PreviewPortRange:     s.PreviewPortRange,
		PreviewBindHost:      s.PreviewBindHost,
		PreviewPublicHost:    s.PreviewPublicHost,
		PreviewProxyBase:     s.PreviewProxyBase,
		PreviewProxyAuth:     s.PreviewProxyAuth,

		WicketEnabled:             s.WicketEnabled,
		WicketProvider:            s.WicketProvider,
		WicketBatchSize:           s.WicketBatchSize,
		WicketProcessedLabel:      s.WicketProcessedLabel,
		WicketNeedsHumanLabel:     s.WicketNeedsHumanLabel,
		WicketBeadCreatedLabel:    s.WicketBeadCreatedLabel,
		WicketTriggerLabel:        s.WicketTriggerLabel,
		WicketStaleDays:           s.WicketStaleDays,
		BdReadyLimit:              s.BdReadyLimit,
		ForgeID:                   s.ForgeID,
		Warden:                    s.Warden,
		Pricing:                   s.Pricing,
		CopilotPremiumMultipliers: s.CopilotPremiumMultipliers,
		ForgeChat: forgeChatShadow{
			TurnTimeout: func() string {
				if s.ForgeChat.TurnTimeout > 0 && s.ForgeChat.TurnTimeout != DefaultForgeChatTurnTimeout {
					return durationString(s.ForgeChat.TurnTimeout)
				}
				return ""
			}(),
			TurnExpiry: func() string {
				if s.ForgeChat.TurnExpiry > 0 && s.ForgeChat.TurnExpiry != DefaultForgeChatTurnExpiry {
					return durationString(s.ForgeChat.TurnExpiry)
				}
				return ""
			}(),
			TurnRetentionCap: func() int {
				if s.ForgeChat.TurnRetentionCap != 0 && s.ForgeChat.TurnRetentionCap != DefaultForgeChatTurnRetentionCap {
					return s.ForgeChat.TurnRetentionCap
				}
				return 0
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
	if s.LogSweepInterval > 0 {
		sh.LogSweepInterval = durationString(s.LogSweepInterval)
	}
	if s.TemperStepTimeout > 0 {
		sh.TemperStepTimeout = durationString(s.TemperStepTimeout)
	}
	if s.TemperGitTimeout > 0 {
		sh.TemperGitTimeout = durationString(s.TemperGitTimeout)
	}
	if s.WorktreeGitTimeout > 0 {
		sh.WorktreeGitTimeout = durationString(s.WorktreeGitTimeout)
	}
	if s.BdTimeout > 0 {
		sh.BdTimeout = durationString(s.BdTimeout)
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
	// Always emit PreviewIdleTimeout so an intentional 0 (idle reaper off) is
	// persisted rather than silently reverting to the 30m default on reload.
	sh.PreviewIdleTimeout = durationString(s.PreviewIdleTimeout)
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

// IsAnvilHealthCheckEnabled returns true unless anvil_health_check is explicitly
// false. Defaults to true: the check is a single query per anvil per full poll.
func (s SettingsConfig) IsAnvilHealthCheckEnabled() bool {
	if s.AnvilHealthCheck == nil {
		return true
	}
	return *s.AnvilHealthCheck
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

// Actions for settings.empty_diff_action — what to do with a bead whose
// approved branch turns out to have no commits against its base.
const (
	// EmptyDiffActionAttention raises a Needs Attention entry and leaves the
	// bead open for the operator to judge. This is the default.
	EmptyDiffActionAttention = "attention"
	// EmptyDiffActionClose closes the bead with a note explaining that the
	// work is already present on the base branch.
	EmptyDiffActionClose = "close"
)

// ResolveEmptyDiffAction normalises a settings.empty_diff_action value. It
// returns the action to apply and whether the raw value was recognised; an
// empty value is a valid "use the default" and reports ok=true, while an
// unrecognised value falls back to EmptyDiffActionAttention with ok=false so
// the caller can warn about the typo instead of silently closing beads.
func ResolveEmptyDiffAction(raw string) (action string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return EmptyDiffActionAttention, true
	case EmptyDiffActionAttention:
		return EmptyDiffActionAttention, true
	case EmptyDiffActionClose:
		return EmptyDiffActionClose, true
	default:
		return EmptyDiffActionAttention, false
	}
}

// ResolvedEmptyDiffAction returns the effective empty-diff action, falling back
// to EmptyDiffActionAttention for unset or unrecognised values.
func (s SettingsConfig) ResolvedEmptyDiffAction() string {
	action, _ := ResolveEmptyDiffAction(s.EmptyDiffAction)
	return action
}

// Kiln preview environment defaults. See docs/plans/preview-environments.md.
const (
	// DefaultPreviewMaxConcurrent is the number of previews allowed to run
	// simultaneously when preview_max_concurrent is unset.
	DefaultPreviewMaxConcurrent = 2
	// DefaultPreviewIdleTimeout is how long an unused preview survives when
	// preview_idle_timeout is unset.
	DefaultPreviewIdleTimeout = 30 * time.Minute
	// DefaultPreviewPortRange is the port range previews allocate from when
	// preview_port_range is unset.
	//
	// It sits below every common ephemeral (dynamic) port floor — Linux's
	// net.ipv4.ip_local_port_range is typically 32768-60999, Windows' dynamic
	// range starts at 49152 — because Kiln allocates a port minutes before the
	// service binds it, and a range inside the ephemeral range can have that
	// port handed to an outbound connection in between ("address already in
	// use" at service start). It also stays clear of the Kubernetes NodePort
	// convention (30000-32767) to avoid confusing operators, even though
	// NodePorts live on the node rather than in the pod's netns.
	DefaultPreviewPortRange = "24000-24999"
	// DefaultPreviewBindHost keeps preview services on loopback unless the
	// operator opts into a wider bind address.
	DefaultPreviewBindHost = "127.0.0.1"
	// MinPreviewIdleTimeout is the smallest accepted non-zero idle timeout.
	MinPreviewIdleTimeout = time.Minute
	// minPreviewPort is the lowest port a preview range may start at
	// (privileged ports are off limits).
	minPreviewPort = 1024
	// maxPreviewPort is the highest port a preview range may end at.
	maxPreviewPort = 65535
)

// Accepted values for a per-anvil preview_auto. The zero value ("") is
// equivalent to PreviewAutoOff, so an anvil that says nothing starts no
// previews on its own.
const (
	// PreviewAutoOff starts previews only when something asks for one
	// (the Hearth button, the preview_start IPC command). The default.
	PreviewAutoOff = "off"
	// PreviewAutoReadyToMerge starts a preview when Bellows announces one of
	// the anvil's PRs ready to merge — the moment a human is most likely to
	// want to look at the branch running.
	PreviewAutoReadyToMerge = "ready_to_merge"
)

// PreviewAutoModes lists the accepted preview_auto values, for validation and
// for the config API's enum options.
var PreviewAutoModes = []string{PreviewAutoOff, PreviewAutoReadyToMerge}

// normalizePreviewAuto lowercases and trims a configured preview_auto so
// "Ready_To_Merge" and " ready_to_merge " resolve like the canonical spelling.
// An empty value normalizes to PreviewAutoOff.
func normalizePreviewAuto(raw string) string {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return PreviewAutoOff
	}
	return mode
}

// IsValidPreviewAuto reports whether raw is an accepted preview_auto value
// (empty counts: it means "off").
func IsValidPreviewAuto(raw string) bool {
	switch normalizePreviewAuto(raw) {
	case PreviewAutoOff, PreviewAutoReadyToMerge:
		return true
	}
	return false
}

// ResolvedPreviewMaxConcurrent returns the effective preview concurrency cap,
// substituting the default for an unset (0) value.
func (s SettingsConfig) ResolvedPreviewMaxConcurrent() int {
	if s.PreviewMaxConcurrent <= 0 {
		return DefaultPreviewMaxConcurrent
	}
	return s.PreviewMaxConcurrent
}

// ResolvedPreviewBindHost returns the address preview services bind to,
// substituting the loopback default for an unset value.
func (s SettingsConfig) ResolvedPreviewBindHost() string {
	if host := strings.TrimSpace(s.PreviewBindHost); host != "" {
		return host
	}
	return DefaultPreviewBindHost
}

// ResolvedPreviewPublicHost returns the hostname used in displayed preview
// links, falling back to the bind host when preview_public_host is unset.
func (s SettingsConfig) ResolvedPreviewPublicHost() string {
	if host := strings.TrimSpace(s.PreviewPublicHost); host != "" {
		return host
	}
	return s.ResolvedPreviewBindHost()
}

// ResolvedPreviewProxyBase returns the normalized preview_proxy_base — trimmed,
// lowercased and without its trailing root dot — or "" when host-based preview
// routing is switched off. It never reports an error: an invalid base is caught
// by Validate at load time, and callers on the request path need an answer, not
// a second error to handle.
func (s SettingsConfig) ResolvedPreviewProxyBase() string {
	base, err := NormalizePreviewProxyBase(s.PreviewProxyBase)
	if err != nil {
		return ""
	}
	return base
}

// IsPreviewProxyEnabled reports whether previews are addressed by hostname
// (preview_proxy_base is set to a usable DNS name) rather than by port.
func (s SettingsConfig) IsPreviewProxyEnabled() bool {
	return s.ResolvedPreviewProxyBase() != ""
}

// The accepted values of settings.preview_proxy_auth. An empty setting means
// PreviewProxyAuthSession: gating is the default, so forgetting to configure it
// cannot leave previews open.
const (
	// PreviewProxyAuthSession gates proxied preview requests on a Hearth
	// session (shared session cookie, or a signed token exchanged for a
	// preview-scoped cookie).
	PreviewProxyAuthSession = "session"
	// PreviewProxyAuthNone serves proxied previews unauthenticated.
	PreviewProxyAuthNone = "none"
)

// ResolvedPreviewProxyAuth returns the effective preview_proxy_auth mode —
// always one of the constants above. Like ResolvedPreviewProxyBase it never
// errors: an unknown value is rejected by Validate at load time, and the
// request path falls back to the gated mode rather than opening previews up
// because a config typo made it past validation (a hot-reload, say).
func (s SettingsConfig) ResolvedPreviewProxyAuth() string {
	mode, err := NormalizePreviewProxyAuth(s.PreviewProxyAuth)
	if err != nil {
		return PreviewProxyAuthSession
	}
	return mode
}

// NormalizePreviewProxyAuth validates preview_proxy_auth and returns the
// canonical mode. Empty is valid and means PreviewProxyAuthSession.
func NormalizePreviewProxyAuth(raw string) (string, error) {
	switch mode := strings.ToLower(strings.TrimSpace(raw)); mode {
	case "":
		return PreviewProxyAuthSession, nil
	case PreviewProxyAuthSession, PreviewProxyAuthNone:
		return mode, nil
	default:
		return "", fmt.Errorf("%q is not a known mode (expected %q or %q)",
			raw, PreviewProxyAuthSession, PreviewProxyAuthNone)
	}
}

// maxDNSName is the longest a fully qualified DNS name may be, and
// maxDNSLabel the longest one of its dot-separated labels (RFC 1035). Kiln's
// preview hostnames put a bead label in front of the configured base, so the
// base itself is held to the same limits.
const (
	maxDNSName  = 253
	maxDNSLabel = 63
)

// NormalizePreviewProxyBase validates preview_proxy_base and returns it in the
// shape everything else compares against: trimmed, lowercased, without a
// trailing root dot. An empty value is valid and means the feature is off.
//
// The value is a bare DNS name, so a scheme, a port, a path or a leading dot
// are rejected rather than quietly stripped — each of those is a different
// mistake and saying which one it is beats guessing what was meant. Single-label
// bases ("localtest") are allowed: they are what a hosts-file or a local
// resolver setup uses.
func NormalizePreviewProxyBase(raw string) (string, error) {
	base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if base == "" {
		return "", nil
	}
	if i := strings.Index(base, "://"); i >= 0 {
		return "", fmt.Errorf("%q must be a bare DNS name without a scheme (drop the %q)", raw, base[:i+3])
	}
	if strings.ContainsAny(base, "/?#") {
		return "", fmt.Errorf("%q must be a bare DNS name without a path", raw)
	}
	if strings.Contains(base, ":") {
		return "", fmt.Errorf("%q must be a bare DNS name without a port", raw)
	}
	if strings.HasPrefix(base, ".") {
		return "", fmt.Errorf("%q must not start with a dot", raw)
	}
	if len(base) > maxDNSName {
		return "", fmt.Errorf("%q is longer than %d characters", raw, maxDNSName)
	}
	for _, label := range strings.Split(base, ".") {
		switch {
		case label == "":
			return "", fmt.Errorf("%q has an empty label", raw)
		case len(label) > maxDNSLabel:
			return "", fmt.Errorf("%q: label %q is longer than %d characters", raw, label, maxDNSLabel)
		case strings.HasPrefix(label, "-"), strings.HasSuffix(label, "-"):
			return "", fmt.Errorf("%q: label %q must not start or end with a hyphen", raw, label)
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return "", fmt.Errorf("%q: label %q contains %q, which is not allowed in a DNS name", raw, label, string(r))
		}
	}
	return base, nil
}

// PreviewPortRangeBounds parses preview_port_range into its inclusive lower
// and upper bounds, applying DefaultPreviewPortRange when unset.
func (s SettingsConfig) PreviewPortRangeBounds() (int, int, error) {
	raw := strings.TrimSpace(s.PreviewPortRange)
	if raw == "" {
		raw = DefaultPreviewPortRange
	}
	return ParsePortRange(raw)
}

// ParsePortRange parses a "min-max" port range (e.g. "24000-24999"), returning
// its inclusive bounds. Both ends must be unprivileged ports and min < max.
func ParsePortRange(raw string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("port range %q must be in the form \"min-max\" (e.g. %q)", raw, DefaultPreviewPortRange)
	}
	lo, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("port range %q: invalid lower bound %q", raw, strings.TrimSpace(parts[0]))
	}
	hi, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("port range %q: invalid upper bound %q", raw, strings.TrimSpace(parts[1]))
	}
	if lo < minPreviewPort || hi > maxPreviewPort {
		return 0, 0, fmt.Errorf("port range %q must stay within %d-%d", raw, minPreviewPort, maxPreviewPort)
	}
	if lo >= hi {
		return 0, 0, fmt.Errorf("port range %q: lower bound must be less than upper bound", raw)
	}
	return lo, hi, nil
}

// IsPreviewEnabledForAnvil reports whether Kiln previews may run for the named
// anvil: the global preview_enabled gate must be on, and the anvil must not
// have opted out via its own preview_enabled: false. An unknown anvil name, or
// one without an explicit override, inherits the global setting.
func (c *Config) IsPreviewEnabledForAnvil(name string) bool {
	if !c.Settings.PreviewEnabled {
		return false
	}
	if anvil, ok := c.Anvils[name]; ok && anvil.PreviewEnabled != nil {
		return *anvil.PreviewEnabled
	}
	return true
}

// PreviewAutoForAnvil returns the effective automatic-preview mode for the named
// anvil, normalized to one of PreviewAutoModes.
//
// It answers PreviewAutoOff whenever previews cannot run for the anvil at all,
// so callers need one check rather than two: an anvil that opted out of
// previews (or a Forge with previews disabled globally) cannot have them
// started automatically either. An unknown anvil or an unrecognized value is
// also off — validation rejects a bad value at load time, and a typo in a
// hot-reloaded config must not be read as "start previews".
func (c *Config) PreviewAutoForAnvil(name string) string {
	if c == nil || !c.IsPreviewEnabledForAnvil(name) {
		return PreviewAutoOff
	}
	anvil, ok := c.Anvils[name]
	if !ok {
		return PreviewAutoOff
	}
	if mode := normalizePreviewAuto(anvil.PreviewAuto); mode == PreviewAutoReadyToMerge {
		return mode
	}
	return PreviewAutoOff
}

// IsPreviewAutoReadyToMerge reports whether the named anvil starts a preview on
// the ready-to-merge transition.
func (c *Config) IsPreviewAutoReadyToMerge(name string) bool {
	return c.PreviewAutoForAnvil(name) == PreviewAutoReadyToMerge
}

// IsPreviewQuestsEnabledForAnvil reports whether the named anvil opted into
// running its E2E quests against a preview environment.
//
// Like PreviewAutoForAnvil it folds the preview gates in, so callers need one
// check rather than three: an anvil whose previews are off (globally or per
// anvil) has nothing to run quests against, and an unknown anvil is off. Load
// time validation rejects preview_quests: true on an anvil that cannot run
// previews, but a hot-reloaded config can still reach here in that state and
// must not be read as "run quests".
func (c *Config) IsPreviewQuestsEnabledForAnvil(name string) bool {
	if c == nil || !c.IsPreviewEnabledForAnvil(name) {
		return false
	}
	anvil, ok := c.Anvils[name]
	if !ok {
		return false
	}
	return anvil.PreviewQuests
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

// AssayConfig configures the Assay AI PR review subsystem. It exists both as
// a top-level Config section (global defaults) and as a per-anvil overlay
// (AnvilConfig.Assay). Tri-state booleans use *bool so an unset value inherits
// from the global config, following the VulncheckEnabled / SmelterEnabled idiom.
type AssayConfig struct {
	Enabled           *bool    `mapstructure:"enabled" yaml:"enabled,omitempty"`
	ShadowMode        *bool    `mapstructure:"shadow_mode" yaml:"shadow_mode,omitempty"`
	DebounceSeconds   *int     `mapstructure:"debounce_seconds" yaml:"debounce_seconds,omitempty"`
	DailyCostLimitUSD *float64 `mapstructure:"daily_cost_limit_usd" yaml:"daily_cost_limit_usd,omitempty"`
	MaxRuns           *int     `mapstructure:"max_runs" yaml:"max_runs,omitempty"`
	TriageProvider    string   `mapstructure:"triage_provider" yaml:"triage_provider,omitempty"`
	ReviewProvider    string   `mapstructure:"review_provider" yaml:"review_provider,omitempty"`
	ModelTier         string   `mapstructure:"model_tier" yaml:"model_tier,omitempty"`
	TriageModel       string   `mapstructure:"triage_model" yaml:"triage_model,omitempty"` // model hint
	ReviewModel       string   `mapstructure:"review_model" yaml:"review_model,omitempty"` // model hint
	MaxDiffBytes      *int     `mapstructure:"max_diff_bytes" yaml:"max_diff_bytes,omitempty"`
	MaxBaseFileBytes  *int     `mapstructure:"max_base_file_bytes" yaml:"max_base_file_bytes,omitempty"`
	NitCap            *int     `mapstructure:"nit_cap" yaml:"nit_cap,omitempty"`
	SkipDrafts        *bool    `mapstructure:"skip_drafts" yaml:"skip_drafts,omitempty"`
	SkipPaths         []string `mapstructure:"skip_paths" yaml:"skip_paths,omitempty"`
	// MaxTurnsPerPass bounds each review pass agent session (every file read
	// costs a turn). Unset or <= 0 uses the engine default. Raise it for repos
	// whose rules file and code layout need more reading than the default
	// allows — the telltale is passes dying at error_max_turns with turns at
	// exactly the cap on modest diffs.
	MaxTurnsPerPass *int `mapstructure:"max_turns_per_pass" yaml:"max_turns_per_pass,omitempty"`
	// MaxCostPerPassUSD is the estimated-spend ceiling for a single review
	// pass session. The engine accumulates the session's cost as its turns
	// complete and stops the pass once it crosses this value, reporting
	// error_max_cost — a named stop, never a quiet success. It is the
	// per-session counterpart to DailyCostLimitUSD: the daily cap notices a
	// runaway only after it has spent the day's budget, while this one bounds
	// the single session that is spending it. Unset uses
	// defaultAssayMaxCostPerPassUSD; <= 0 disables the ceiling.
	MaxCostPerPassUSD *float64 `mapstructure:"max_cost_per_pass_usd" yaml:"max_cost_per_pass_usd,omitempty"`
	// Incremental controls delta reviews: when a PR has been reviewed before,
	// review only the changes pushed since the last reviewed commit instead of
	// the whole base..head diff again. Defaults to true when unset. Falls back
	// to a full review automatically when the last reviewed commit is no
	// longer an ancestor of the head (force-push/rebase).
	Incremental *bool `mapstructure:"incremental" yaml:"incremental,omitempty"`
	// MaxFindingsPerPR caps the cumulative number of findings — every
	// severity, Important included — a PR may accumulate across all of its
	// Assay reviews. Unset uses defaultAssayMaxFindingsPerPR; <= 0 means no
	// cap.
	MaxFindingsPerPR *int `mapstructure:"max_findings_per_pr" yaml:"max_findings_per_pr,omitempty"`
}

// IsEnabled returns whether Assay is active. Defaults to false when unset.
func (a AssayConfig) IsEnabled() bool {
	if a.Enabled == nil {
		return false
	}
	return *a.Enabled
}

// IsShadowMode returns whether Assay runs in shadow mode (no public side
// effects). Defaults to true when unset — shadow is the safe default.
func (a AssayConfig) IsShadowMode() bool {
	if a.ShadowMode == nil {
		return true
	}
	return *a.ShadowMode
}

// IsSkipDrafts returns whether draft PRs are skipped. Defaults to true when unset.
func (a AssayConfig) IsSkipDrafts() bool {
	if a.SkipDrafts == nil {
		return true
	}
	return *a.SkipDrafts
}

// GetDebounceSeconds returns the debounce seconds value, defaulting to 0 when unset.
func (a AssayConfig) GetDebounceSeconds() int {
	if a.DebounceSeconds == nil {
		return 0
	}
	return *a.DebounceSeconds
}

// GetDailyCostLimitUSD returns the daily cost limit, defaulting to 0 when unset.
func (a AssayConfig) GetDailyCostLimitUSD() float64 {
	if a.DailyCostLimitUSD == nil {
		return 0
	}
	return *a.DailyCostLimitUSD
}

// defaultAssayMaxRuns is the fallback per-PR Assay run cap when max_runs is
// unset. Assay reviews on every new head SHA, so without a cap the
// Assay→Burnish→new-head loop runs until a review finds nothing; two passes
// (initial review + one re-review of the fixes) is plenty in practice.
const defaultAssayMaxRuns = 2

// GetMaxRuns returns the maximum number of executed Assay reviews per PR,
// defaulting to defaultAssayMaxRuns when unset. A value <= 0 means no cap.
func (a AssayConfig) GetMaxRuns() int {
	if a.MaxRuns == nil {
		return defaultAssayMaxRuns
	}
	return *a.MaxRuns
}

// GetMaxDiffBytes returns the max diff bytes, defaulting to 0 when unset.
func (a AssayConfig) GetMaxDiffBytes() int {
	if a.MaxDiffBytes == nil {
		return 0
	}
	return *a.MaxDiffBytes
}

// GetMaxBaseFileBytes returns the max base file bytes, defaulting to 0 when unset.
func (a AssayConfig) GetMaxBaseFileBytes() int {
	if a.MaxBaseFileBytes == nil {
		return 0
	}
	return *a.MaxBaseFileBytes
}

// GetNitCap returns the nit cap, defaulting to 0 when unset.
func (a AssayConfig) GetNitCap() int {
	if a.NitCap == nil {
		return 0
	}
	return *a.NitCap
}

// GetMaxTurnsPerPass returns the per-pass agent turn budget, or 0 when unset
// (the assay engine then applies its built-in default).
func (a AssayConfig) GetMaxTurnsPerPass() int {
	if a.MaxTurnsPerPass == nil {
		return 0
	}
	return *a.MaxTurnsPerPass
}

// defaultAssayMaxCostPerPassUSD is the fallback per-pass spend ceiling when
// max_cost_per_pass_usd is unset. It is a runaway brake, not a routine budget:
// a review pass that spends this much has burned a third of the default
// daily_cost_limit_usd on one session, which is the shape of a pass looping on
// tool calls rather than one reading a large diff. Deployments running a
// premium model over big diffs should raise it (or set 0) rather than have
// ordinary passes clipped.
const defaultAssayMaxCostPerPassUSD = 1.50

// GetMaxCostPerPassUSD returns the per-pass USD spend ceiling, defaulting to
// defaultAssayMaxCostPerPassUSD when unset. A value <= 0 disables the ceiling.
func (a AssayConfig) GetMaxCostPerPassUSD() float64 {
	if a.MaxCostPerPassUSD == nil {
		return defaultAssayMaxCostPerPassUSD
	}
	return *a.MaxCostPerPassUSD
}

// IsIncremental returns whether repeat reviews are scoped to the changes since
// the last reviewed commit. Defaults to true when unset — re-reviewing the
// whole PR on every push is what buried PRs in duplicate comments.
func (a AssayConfig) IsIncremental() bool {
	if a.Incremental == nil {
		return true
	}
	return *a.Incremental
}

// defaultAssayMaxFindingsPerPR is the fallback cumulative per-PR findings cap
// when max_findings_per_pr is unset. Generous enough that a healthy PR never
// notices it; a hard brake on the pathological runs that used to accumulate
// comments without bound.
const defaultAssayMaxFindingsPerPR = 30

// GetMaxFindingsPerPR returns the cumulative per-PR findings cap, defaulting
// to defaultAssayMaxFindingsPerPR when unset. A value <= 0 means no cap.
func (a AssayConfig) GetMaxFindingsPerPR() int {
	if a.MaxFindingsPerPR == nil {
		return defaultAssayMaxFindingsPerPR
	}
	return *a.MaxFindingsPerPR
}

// ResolvedAssay returns the effective Assay configuration for the named anvil.
// It starts from the global c.Assay and overlays the anvil's *AssayConfig when
// present: pointer fields (*bool, *int, *float64) override when non-nil; string
// fields override when non-empty; SkipPaths overrides when len>0. Unknown anvils
// or anvils without an Assay override return the global config unchanged.
func (c *Config) ResolvedAssay(anvilName string) AssayConfig {
	resolved := c.Assay
	anvil, ok := c.Anvils[anvilName]
	if !ok || anvil.Assay == nil {
		return resolved
	}
	o := anvil.Assay
	if o.Enabled != nil {
		resolved.Enabled = o.Enabled
	}
	if o.ShadowMode != nil {
		resolved.ShadowMode = o.ShadowMode
	}
	if o.SkipDrafts != nil {
		resolved.SkipDrafts = o.SkipDrafts
	}
	if o.DebounceSeconds != nil {
		resolved.DebounceSeconds = o.DebounceSeconds
	}
	if o.DailyCostLimitUSD != nil {
		resolved.DailyCostLimitUSD = o.DailyCostLimitUSD
	}
	if o.MaxRuns != nil {
		resolved.MaxRuns = o.MaxRuns
	}
	if o.TriageProvider != "" {
		resolved.TriageProvider = o.TriageProvider
	}
	if o.ReviewProvider != "" {
		resolved.ReviewProvider = o.ReviewProvider
	}
	if o.ModelTier != "" {
		resolved.ModelTier = o.ModelTier
	}
	if o.TriageModel != "" {
		resolved.TriageModel = o.TriageModel
	}
	if o.ReviewModel != "" {
		resolved.ReviewModel = o.ReviewModel
	}
	if o.MaxDiffBytes != nil {
		resolved.MaxDiffBytes = o.MaxDiffBytes
	}
	if o.MaxBaseFileBytes != nil {
		resolved.MaxBaseFileBytes = o.MaxBaseFileBytes
	}
	if o.NitCap != nil {
		resolved.NitCap = o.NitCap
	}
	if o.MaxTurnsPerPass != nil {
		resolved.MaxTurnsPerPass = o.MaxTurnsPerPass
	}
	if o.MaxCostPerPassUSD != nil {
		resolved.MaxCostPerPassUSD = o.MaxCostPerPassUSD
	}
	if o.Incremental != nil {
		resolved.Incremental = o.Incremental
	}
	if o.MaxFindingsPerPR != nil {
		resolved.MaxFindingsPerPR = o.MaxFindingsPerPR
	}
	if len(o.SkipPaths) > 0 {
		resolved.SkipPaths = o.SkipPaths
	}
	return resolved
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
			RateLimitBackoff:       5 * time.Minute,
			BellowsInterval:        2 * time.Minute,
			MaxCIFixAttempts:       5,
			MaxReviewFixAttempts:   5,
			MaxSameHeadReviewFixes: 2,
			MaxRebaseAttempts:      3,
			MaxLifecycleWorkers:    DefaultMaxLifecycleWorkers,
			BurnishVerifyTimeout:   5 * time.Minute,
			BurnishVerifyRetries:   1,
			StaleInterval:          5 * time.Minute,
			TemperStepTimeout:      5 * time.Minute,
			TemperGitTimeout:       30 * time.Second,
			WorktreeGitTimeout:     5 * time.Minute,
			BdTimeout:              executil.DefaultBdTimeout,
			TemperOutputCap:        DefaultTemperOutputCap,
			DepcheckInterval:       168 * time.Hour, // weekly
			DepcheckTimeout:        5 * time.Minute,
			VulncheckInterval:      24 * time.Hour,
			VulncheckTimeout:       10 * time.Minute,
			LogRetentionDays:       30,
			LogSweepInterval:       24 * time.Hour,
			SmelterInterval:        8 * time.Hour,
			QuestgiverInterval:     24 * time.Hour,
			AdventurerTimeout:      5 * time.Minute,
			// Kiln preview environments: off by default; the rest of the
			// values only matter once preview_enabled is turned on.
			PreviewEnabled:       false,
			PreviewMaxConcurrent: DefaultPreviewMaxConcurrent,
			// Rejection, not eviction, is the default: a preview is something
			// an operator asked for and may still be looking at.
			PreviewEvictLRU:    false,
			PreviewIdleTimeout: DefaultPreviewIdleTimeout,
			PreviewPortRange:   DefaultPreviewPortRange,
			PreviewBindHost:    DefaultPreviewBindHost,
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
			// Event Bus disabled by default for safe rollout; buffer sized to
			// the historical daemon default so enabling it needs no tuning.
			BusEnabled:    false,
			BusBufferSize: DefaultBusBufferSize,
			// Activity SSE stream uses the Bus (when enabled) by default; the
			// poll fallback is an opt-in one-release safety valve.
			SSEPollFallback: false,
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
		Assay: AssayConfig{
			Enabled:    boolPtr(false),
			ShadowMode: boolPtr(true),
			SkipDrafts: boolPtr(true),
			// Pilot-safety defaults: 5-minute debounce coalesces rapid push
			// bursts; nit_cap=5 mirrors the design's noise budget; a non-zero
			// daily cost cap means an unconfigured deployment can't silently
			// burn Max-plan quota on a runaway PR loop.
			DebounceSeconds:   intPtr(300),
			DailyCostLimitUSD: float64Ptr(5.0),
			MaxRuns:           intPtr(defaultAssayMaxRuns),
			MaxDiffBytes:      intPtr(250000),
			MaxBaseFileBytes:  intPtr(100000),
			NitCap:            intPtr(5),
			MaxCostPerPassUSD: float64Ptr(defaultAssayMaxCostPerPassUSD),
		},
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func intPtr(i int) *int {
	return &i
}

func float64Ptr(f float64) *float64 {
	return &f
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
	v.SetDefault("settings.max_lifecycle_workers", DefaultMaxLifecycleWorkers)
	v.SetDefault("settings.burnish_verify_timeout", "5m")
	v.SetDefault("settings.stale_interval", "5m")
	v.SetDefault("settings.temper_step_timeout", "5m")
	v.SetDefault("settings.temper_git_timeout", "30s")
	v.SetDefault("settings.worktree_git_timeout", "5m")
	v.SetDefault("settings.temper_output_cap", DefaultTemperOutputCap)
	v.SetDefault("settings.depcheck_interval", "168h")
	v.SetDefault("settings.depcheck_timeout", "5m")
	v.SetDefault("settings.vulncheck_interval", "24h")
	v.SetDefault("settings.vulncheck_timeout", "10m")
	v.SetDefault("settings.vulncheck_enabled", true)
	v.SetDefault("settings.log_retention_days", 30)
	v.SetDefault("settings.log_sweep_interval", "24h")
	v.SetDefault("settings.smelter_enabled", true)
	v.SetDefault("settings.smelter_interval", "8h")
	v.SetDefault("settings.questgiver_interval", "24h")
	v.SetDefault("settings.adventurer_timeout", "5m")
	v.SetDefault("settings.preview_enabled", false)
	v.SetDefault("settings.preview_max_concurrent", DefaultPreviewMaxConcurrent)
	v.SetDefault("settings.preview_evict_lru", false)
	v.SetDefault("settings.preview_idle_timeout", DefaultPreviewIdleTimeout.String())
	v.SetDefault("settings.preview_port_range", DefaultPreviewPortRange)
	v.SetDefault("settings.preview_bind_host", DefaultPreviewBindHost)
	v.SetDefault("settings.preview_public_host", "")
	v.SetDefault("settings.preview_proxy_base", "")
	v.SetDefault("settings.preview_proxy_auth", "")
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
	v.SetDefault("settings.bus_enabled", false)
	v.SetDefault("settings.bus_buffer_size", DefaultBusBufferSize)
	v.SetDefault("settings.sse_poll_fallback", false)
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
	if raw := v.GetString("settings.temper_step_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid temper_step_timeout %q: %w", raw, err)
		}
		cfg.Settings.TemperStepTimeout = d
	}
	if raw := v.GetString("settings.temper_git_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid temper_git_timeout %q: %w", raw, err)
		}
		cfg.Settings.TemperGitTimeout = d
	}
	if raw := v.GetString("settings.worktree_git_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid worktree_git_timeout %q: %w", raw, err)
		}
		cfg.Settings.WorktreeGitTimeout = d
	}
	if raw := v.GetString("settings.bd_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid bd_timeout %q: %w", raw, err)
		}
		cfg.Settings.BdTimeout = d
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
	if raw := v.GetString("settings.log_sweep_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid log_sweep_interval %q: %w", raw, err)
		}
		cfg.Settings.LogSweepInterval = d
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
	if raw := v.GetString("settings.preview_idle_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid preview_idle_timeout %q: %w", raw, err)
		}
		cfg.Settings.PreviewIdleTimeout = d
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
	if raw := v.GetString("settings.forgechat.turn_expiry"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid forgechat.turn_expiry %q: %w", raw, err)
		}
		cfg.Settings.ForgeChat.TurnExpiry = d
	}

	// Pricing tables carry mapstructure:"-" so viper skips them (their model
	// keys frequently contain dots, which viper's "." delimiter would mangle).
	// Load them directly from the raw YAML instead.
	if used := v.ConfigFileUsed(); used != "" {
		if err := loadPricingTablesFromYAML(used, &cfg.Settings); err != nil {
			return nil, err
		}
	}

	// Decrypt any enc:-prefixed webhook URLs written by Hytte.
	decryptWebhookURLs(&cfg)

	return &cfg, nil
}

// loadPricingTablesFromYAML reads settings.pricing and
// settings.copilot_premium_multipliers directly from the config file, bypassing
// viper. Their keys are model identifiers that frequently contain dots (e.g.
// "claude-opus-4.6"); viper treats "." as a nested-key delimiter and would
// mangle "claude-opus-4.6: 3" into {"claude-opus-4": {"6": 3}}, failing to
// decode into float64/ModelPricing. gopkg.in/yaml.v3 preserves dotted keys
// verbatim. A nil map (key absent from the file) leaves the existing value
// untouched.
func loadPricingTablesFromYAML(path string, settings *SettingsConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config for pricing tables: %w", err)
	}
	var shadow struct {
		Settings struct {
			Pricing                   map[string]ModelPricing `yaml:"pricing"`
			CopilotPremiumMultipliers map[string]float64      `yaml:"copilot_premium_multipliers"`
		} `yaml:"settings"`
	}
	if err := yaml.Unmarshal(data, &shadow); err != nil {
		return fmt.Errorf("parsing pricing tables: %w", err)
	}
	if shadow.Settings.Pricing != nil {
		settings.Pricing = shadow.Settings.Pricing
	}
	if shadow.Settings.CopilotPremiumMultipliers != nil {
		settings.CopilotPremiumMultipliers = shadow.Settings.CopilotPremiumMultipliers
	}
	return nil
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
// validateAssay checks the numeric Assay settings that a bad value silently
// breaks. It is called for the global block and for every per-anvil overlay
// under the same rules, since an overlay is what the daemon actually resolves
// and running the check on only one of the two leaves the other unguarded.
//
// prefix names the block being checked ("assay", `anvil "x": assay`) so the
// message points at the value the operator wrote.
func validateAssay(prefix string, a AssayConfig) []string {
	var errs []string
	if v := a.MaxCostPerPassUSD; v != nil {
		if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
			errs = append(errs, fmt.Sprintf("%s.max_cost_per_pass_usd must be a non-negative finite number (set 0 to disable the per-pass ceiling)", prefix))
		}
	}
	return errs
}

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
	if c.Settings.PerWorkerCostEstimate < 0 || math.IsNaN(c.Settings.PerWorkerCostEstimate) || math.IsInf(c.Settings.PerWorkerCostEstimate, 0) {
		errs = append(errs, "settings.per_worker_cost_estimate must be a non-negative finite number (omit or set to 0 to use the default)")
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
	if c.Settings.MaxSameHeadReviewFixes < 0 {
		errs = append(errs, "settings.max_same_head_review_fixes must not be negative (omit or set to 0 to use the default)")
	}
	if c.Settings.MaxRebaseAttempts < 1 {
		errs = append(errs, "settings.max_rebase_attempts must be >= 1")
	}
	if c.Settings.MaxLifecycleWorkers < 0 {
		errs = append(errs, "settings.max_lifecycle_workers must not be negative (omit or set to 0 to use the default)")
	}
	if c.Settings.BurnishVerifyTimeout < 0 {
		errs = append(errs, "settings.burnish_verify_timeout must not be negative (omit or set to 0 to use the package default)")
	} else if c.Settings.BurnishVerifyTimeout > 0 && c.Settings.BurnishVerifyTimeout < 30*time.Second {
		errs = append(errs, "settings.burnish_verify_timeout must be >= 30s when set explicitly (omit or set to 0 to use the package default)")
	}

	if c.Settings.WorktreeGitTimeout < 0 {
		errs = append(errs, "settings.worktree_git_timeout must not be negative (omit or set to 0 to use the default)")
	} else if c.Settings.WorktreeGitTimeout > 0 && c.Settings.WorktreeGitTimeout < 30*time.Second {
		errs = append(errs, "settings.worktree_git_timeout must be >= 30s when set explicitly (omit or set to 0 to use the default)")
	}

	if c.Settings.BdTimeout < 0 {
		errs = append(errs, "settings.bd_timeout must not be negative (omit or set to 0 to use the default)")
	} else if c.Settings.BdTimeout > 0 && c.Settings.BdTimeout < 30*time.Second {
		errs = append(errs, "settings.bd_timeout must be >= 30s when set explicitly (omit or set to 0 to use the default)")
	}

	if c.Settings.CopilotDailyRequestLimit < 0 {
		errs = append(errs, "settings.copilot_daily_request_limit must be >= 0 (0 = no limit)")
	}
	if c.Settings.LogRetentionDays < 0 {
		errs = append(errs, "settings.log_retention_days must be >= 0 (0 disables the log retention sweep)")
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

	if c.Settings.PreviewMaxConcurrent < 0 {
		errs = append(errs, "settings.preview_max_concurrent must not be negative (omit or set to 0 to use the default)")
	}
	if c.Settings.PreviewIdleTimeout < 0 {
		errs = append(errs, "settings.preview_idle_timeout must not be negative (set to 0 to disable the idle reaper)")
	} else if c.Settings.PreviewIdleTimeout > 0 && c.Settings.PreviewIdleTimeout < MinPreviewIdleTimeout {
		errs = append(errs, fmt.Sprintf("settings.preview_idle_timeout must be >= %s when enabled (or 0 to disable)", MinPreviewIdleTimeout))
	}
	if _, _, err := c.Settings.PreviewPortRangeBounds(); err != nil {
		errs = append(errs, fmt.Sprintf("settings.preview_port_range: %s", err))
	}
	if _, err := NormalizePreviewProxyBase(c.Settings.PreviewProxyBase); err != nil {
		errs = append(errs, fmt.Sprintf("settings.preview_proxy_base: %s", err))
	}
	if _, err := NormalizePreviewProxyAuth(c.Settings.PreviewProxyAuth); err != nil {
		errs = append(errs, fmt.Sprintf("settings.preview_proxy_auth: %s", err))
	}

	if c.Settings.CruciblePollInterval < 0 {
		errs = append(errs, "settings.crucible_poll_interval must not be negative (set to 0 to disable two-tier polling)")
	} else if c.Settings.CruciblePollInterval > 0 && c.Settings.CruciblePollInterval < 30*time.Second {
		errs = append(errs, "settings.crucible_poll_interval must be >= 30s when enabled (or 0 to disable)")
	}

	errs = append(errs, validateAssay("assay", c.Assay)...)

	for name, anvil := range c.Anvils {
		if anvil.Assay != nil {
			errs = append(errs, validateAssay(fmt.Sprintf("anvil %q: assay", name), *anvil.Assay)...)
		}
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

		if !IsValidPreviewAuto(anvil.PreviewAuto) {
			errs = append(errs, fmt.Sprintf("anvil %q: invalid preview_auto %q (must be %s)",
				name, anvil.PreviewAuto, strings.Join(PreviewAutoModes, "|")))
		}

		// preview_quests has nothing to run against unless previews can run
		// for this anvil, so say so at load time rather than silently doing
		// nothing when someone clicks "run quests".
		if anvil.PreviewQuests && !c.IsPreviewEnabledForAnvil(name) {
			if !c.Settings.PreviewEnabled {
				errs = append(errs, fmt.Sprintf("anvil %q: preview_quests requires settings.preview_enabled: true", name))
			} else {
				errs = append(errs, fmt.Sprintf("anvil %q: preview_quests requires preview_enabled for this anvil (it is set to false)", name))
			}
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

	if c.SelfDeploy.Enabled {
		if c.SelfDeploy.Anvil == "" {
			errs = append(errs, "self_deploy.anvil is required when self_deploy.enabled is true")
		} else if _, ok := c.Anvils[c.SelfDeploy.Anvil]; !ok {
			errs = append(errs, fmt.Sprintf("self_deploy.anvil %q does not match any configured anvil", c.SelfDeploy.Anvil))
		}
	}
	if c.SelfDeploy.MaxDrainWait < 0 {
		errs = append(errs, "self_deploy.max_drain_wait must not be negative (omit or set to 0 to use the default)")
	}
	if c.SelfDeploy.DrainTimeout < 0 {
		errs = append(errs, "self_deploy.drain_timeout must not be negative (omit or set to 0 to use the default)")
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
