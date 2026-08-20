// Package temper runs build, lint, and test verification in a worktree.
//
// Temper ("tempering the steel") validates that Claude's changes compile,
// pass linting, and pass tests before progressing to the Warden review stage.
// Commands are configurable per-anvil.
package temper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// vitestSingleThreadArgs caps Vitest worker concurrency to one thread. On
// memory-constrained hosts the default Vitest pool spawns N-CPU workers, each
// of which can balloon to 1+ GB RSS on Vite-based frontends — enough to push
// the Forge daemon, the Smith subprocess, and the temper test runner past the
// host's RAM and trigger an OOM kill of the Smith. Single-threaded mode is
// slower but functionally identical. Users who know their host can take more
// can override the test step entirely via `temper.commands` / `temper.steps`.
var vitestSingleThreadArgs = []string{
	"--pool=threads",
	"--poolOptions.threads.maxThreads=1",
	"--poolOptions.threads.minThreads=1",
}

// Classification categorises why a step failed so the pipeline can treat
// infrastructure and timeout failures differently from real code failures.
// A passed or skipped step carries the empty classification.
type Classification string

const (
	// ClassificationTestFailure is a genuine test failure — the code under
	// test is wrong. This is the default classification for a non-zero exit.
	ClassificationTestFailure Classification = "test_failure"
	// ClassificationBuildError is a compile/build failure. Reserved: it is
	// defined for callers that can detect a compile-failure signal, but Temper
	// does not currently auto-assign it (a build step that fails is reported as
	// test_failure unless it matches an infra/timeout pattern).
	ClassificationBuildError Classification = "build_error"
	// ClassificationTimeout means the step was killed because its deadline
	// (per-step Timeout or the configured temper_step_timeout) was exceeded.
	ClassificationTimeout Classification = "timeout"
	// ClassificationInfra means the step died from an infrastructure/host
	// failure — a signal death (SIGKILL/SIGSEGV/SIGABRT/SIGBUS, e.g. an OOM
	// kill) or a host-crash marker in the output (generalising the .NET
	// test-host crash carve-out). Not evidence that Smith's code is wrong.
	ClassificationInfra Classification = "infra"
)

// IsRetryableWithoutSmith reports whether a failure with this classification
// should be retried by re-running Temper (without invoking Smith) rather than
// looping Smith on what may be a phantom failure. Timeouts and infra failures
// qualify; real test/build failures do not.
func (c Classification) IsRetryableWithoutSmith() bool {
	return c == ClassificationTimeout || c == ClassificationInfra
}

// StepResult captures the outcome of a single verification step.
type StepResult struct {
	// Name identifies the step (e.g., "build", "lint", "test").
	Name string
	// Command is the full command that was run.
	Command string
	// ExitCode is the process exit code.
	ExitCode int
	// Output is the combined stdout+stderr, bounded to the configured output
	// cap via head+tail truncation with an elision marker.
	Output string
	// Duration is how long the step took.
	Duration time.Duration
	// Passed indicates whether the step succeeded.
	Passed bool
	// Optional mirrors the Step.Optional flag — failure here does not fail
	// the overall check. Surfaced so summaries can render it distinctly.
	Optional bool
	// Skipped is true when the step did not run at all — either because no
	// changed file matched its Paths globs or because Temper blocked it.
	Skipped bool
	// SkipReason names why a skipped step did not run, so a caller reading
	// the result can tell a path-gated skip from a blocked one. Empty for
	// steps that ran.
	SkipReason string
	// Classification categorises a failure (test_failure/build_error/timeout/
	// infra). Empty for passed or skipped steps.
	Classification Classification
}

// Skip reasons recorded on a StepResult that did not run.
const (
	// SkipReasonPathFilter means no changed file matched the step's Paths
	// globs, so running it could not have told the caller anything.
	SkipReasonPathFilter = "changed-file gating"
	// SkipReasonBlockedInstall means Temper refused to run a destructive
	// npm install against a linked node_modules.
	SkipReasonBlockedInstall = "blocked destructive npm install"
)

// Result is the overall Temper verification result.
type Result struct {
	// Steps is the ordered list of step results.
	Steps []StepResult
	// Passed is true if all required steps passed (optional steps may have
	// warned without affecting this flag).
	Passed bool
	// Duration is the total time for all steps.
	Duration time.Duration
	// FailedStep is the name of the first failed step, or empty if all passed.
	FailedStep string
	// Classification is the classification of the first failed step, or empty
	// if all passed. Lets the pipeline distinguish timeout/infra failures
	// (retryable without Smith) from real code failures.
	Classification Classification
	// Summary is a human-readable summary of the verification.
	Summary string
}

// SkippedStepNames returns the names of the steps that were skipped because
// no changed file matched their Paths globs. Callers that run Temper outside
// the pipeline (burnish, quench) log these so a skip reads as a skip in their
// own logs rather than as a step that quietly passed.
func SkippedStepNames(r *Result) []string {
	if r == nil {
		return nil
	}
	var names []string
	for _, s := range r.Steps {
		if s.Skipped && s.SkipReason == SkipReasonPathFilter {
			names = append(names, s.Name)
		}
	}
	return names
}

// Step defines a verification step to run.
type Step struct {
	// Name identifies the step.
	Name string
	// Command is the shell command to run.
	Command string
	// Args are the command arguments.
	Args []string
	// Dir is the working directory (relative to worktree, or absolute).
	// If empty, runs in the worktree root.
	Dir string
	// Timeout is the maximum duration for this step. Zero means the
	// Config.StepTimeout (which itself defaults to DefaultStepTimeout).
	Timeout time.Duration
	// Optional means failure here doesn't fail the overall check.
	Optional bool
	// Paths is a list of glob patterns (doublestar syntax). When non-empty,
	// the step is skipped if no changed files match any pattern.
	Paths []string
	// VerifyClean is a list of pathspecs (relative to the worktree) that must
	// remain clean — meaning unchanged versus HEAD with no untracked files —
	// after the step completes. When the step itself passes but produces
	// changes under any of these paths, the step is converted to a failure
	// with a message explaining that the committed artifacts are stale. Use
	// this for steps that rebuild committed build output (e.g. an embedded
	// frontend bundle): if running `npm run build` mutates the committed
	// dist/ directory, the bundle Smith committed does not match the source.
	VerifyClean []string
	// VerifyNoConflictMarkers is a list of pathspecs (relative to the
	// worktree) that must not contain git merge-conflict markers
	// (`<<<<<<<`, `=======`, `>>>>>>>` — 7+ consecutive marker characters at
	// the start of a line). When set, temper walks the listed paths and
	// fails the step when any marker is found, naming the offending
	// file:line. Unlike VerifyClean this scan does NOT depend on a rebuild
	// — when Command is empty the step becomes a cheap scan-only check that
	// runs unconditionally (no Paths gating). That independence is the
	// point: a rebase Smith can commit conflict markers into a built bundle
	// that the freshness check would miss when the rebuild is skipped or
	// happens to land the same bytes.
	VerifyNoConflictMarkers []string
	// TolerateHostCrash re-classifies a non-zero exit as a pass when the output
	// shows a completed all-passed .NET test summary AND a test-host crash/abort
	// marker — the "testhost OOM'd at teardown after all tests passed" case that
	// otherwise fails the step for no real test failure. See
	// dotnetTestHostCrashTolerable.
	TolerateHostCrash bool
}

// Config holds per-anvil verification configuration.
type Config struct {
	// Steps is the ordered list of verification steps.
	Steps []Step
	// GoRaceDetection is a configuration hint indicating whether Go race
	// detection should be enabled (e.g., by adding a separate "race" step
	// such as 'go test -race -short ./...'). It does not automatically
	// modify Steps; callers are responsible for constructing Steps
	// accordingly (e.g., via DefaultConfigWithRace). Default is false since
	// -race slows tests and increases memory usage.
	GoRaceDetection bool
	// ChangedFiles is the list of file paths changed in the current diff
	// (relative to the repository root). When non-nil, steps with Paths
	// globs are checked against this list and skipped when no files match.
	// A nil value means "unknown" and disables path-based filtering.
	ChangedFiles []string
	// StepTimeout is the default per-step timeout applied to any step whose
	// own Step.Timeout is zero. A value <= 0 falls back to DefaultStepTimeout.
	// Sourced from the temper_step_timeout setting.
	StepTimeout time.Duration
	// GitTimeout is the timeout for internal git invocations (e.g. the
	// VerifyClean status check). A value <= 0 falls back to DefaultGitTimeout.
	// Sourced from the temper_git_timeout setting.
	GitTimeout time.Duration
	// OutputCap is the maximum number of bytes of combined stdout+stderr
	// retained per step (head+tail, with an elision marker for the middle).
	// A value <= 0 falls back to DefaultOutputCap.
	OutputCap int
}

// Default timeouts and output cap applied when the corresponding Config field
// is unset. StepTimeout and GitTimeout are overridable via the
// temper_step_timeout / temper_git_timeout settings; OutputCap keeps step
// output (and therefore the warden/fix prompt embedding) bounded.
const (
	// DefaultStepTimeout is the per-step timeout used when neither Step.Timeout
	// nor Config.StepTimeout is set.
	DefaultStepTimeout = 5 * time.Minute
	// DefaultGitTimeout is the timeout for internal git invocations.
	DefaultGitTimeout = 30 * time.Second
	// DefaultOutputCap is the default per-step combined-output byte cap (256 KiB).
	DefaultOutputCap = 256 * 1024
)

// DetectOptions controls optional steps during auto-detection.
type DetectOptions struct {
	// DisableGolangciLint skips the golangci-lint step even if the binary
	// is available. When false (default), golangci-lint is added as an
	// optional step for Go projects if the binary is found on PATH.
	DisableGolangciLint bool
}

// DetectOptionsFromAnvilFlag converts a nullable boolean anvil config flag into
// DetectOptions. When golangciLint is non-nil and false, golangci-lint is
// disabled. This centralises the anvil-config → DetectOptions translation so
// all call sites stay in sync when new detection toggles are added.
func DetectOptionsFromAnvilFlag(golangciLint *bool) *DetectOptions {
	if golangciLint != nil && !*golangciLint {
		return &DetectOptions{DisableGolangciLint: true}
	}
	return nil
}

// DefaultConfig returns a default config that auto-detects the project type.
func DefaultConfig(worktreePath string, opts *DetectOptions) Config {
	return Config{
		Steps: detectSteps(worktreePath, opts, false),
	}
}

// TemperYAML represents the per-anvil .forge/temper.yaml configuration.
type TemperYAML struct {
	GoRaceDetection *bool `yaml:"go_race_detection"`
}

// LoadAnvilConfig loads per-anvil temper configuration from .forge/temper.yaml
// within the given anvil path. Returns (nil, nil) if the file does not exist.
// Returns a non-nil error for read or parse failures so the caller can decide
// how to surface it (e.g., log once per change, return structured error).
func LoadAnvilConfig(anvilPath string) (*TemperYAML, error) {
	path := filepath.Join(anvilPath, ".forge", "temper.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file is legitimately absent for this anvil.
			return nil, nil
		}
		return nil, fmt.Errorf("temper: failed to read config %s: %w", path, err)
	}

	var cfg TemperYAML
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("temper: failed to parse config %s: %w", path, err)
	}
	return &cfg, nil
}

// ConfigFromCommands builds a Config from explicit command strings.
// Each non-empty command becomes a Step. The command string is split on
// whitespace into the executable and arguments (no shell expansion).
// If all commands are empty, it returns nil so callers can fall back to
// auto-detection.
func ConfigFromCommands(build, test, lint string, lintRequired bool) *Config {
	build = strings.TrimSpace(build)
	test = strings.TrimSpace(test)
	lint = strings.TrimSpace(lint)
	if build == "" && test == "" && lint == "" {
		return nil
	}
	var steps []Step
	if build != "" {
		cmd, args := splitCommand(build)
		steps = append(steps, Step{
			Name:    "build",
			Command: cmd,
			Args:    args,
			Timeout: 3 * time.Minute,
		})
	}
	if lint != "" {
		cmd, args := splitCommand(lint)
		steps = append(steps, Step{
			Name:     "lint",
			Command:  cmd,
			Args:     args,
			Timeout:  3 * time.Minute,
			Optional: !lintRequired,
		})
	}
	if test != "" {
		cmd, args := splitCommand(test)
		steps = append(steps, Step{
			Name:    "test",
			Command: cmd,
			Args:    args,
			Timeout: 5 * time.Minute,
		})
	}
	return &Config{Steps: steps}
}

// ConfigFromSteps builds a Config from an ordered list of TemperStepConfig
// entries. Returns nil if the slice is empty so callers can fall back to
// auto-detection.
func ConfigFromSteps(steps []config.TemperStepConfig) *Config {
	if len(steps) == 0 {
		return nil
	}
	out := make([]Step, len(steps))
	for i, s := range steps {
		timeout := s.Timeout
		if timeout == 0 {
			timeout = 5 * time.Minute
		}
		optional := false
		if s.Required != nil && !*s.Required {
			optional = true
		}
		out[i] = Step{
			Name:                    strings.TrimSpace(s.Name),
			Command:                 strings.TrimSpace(s.Command),
			Args:                    s.Args,
			Dir:                     strings.TrimSpace(s.Dir),
			Timeout:                 timeout,
			Optional:                optional,
			Paths:                   s.Paths,
			VerifyClean:             s.VerifyClean,
			VerifyNoConflictMarkers: s.VerifyNoConflictMarkers,
			TolerateHostCrash:       s.TolerateHostCrash,
		}
	}
	return &Config{Steps: out}
}

// splitCommand splits a command string into the executable and arguments.
// It splits on whitespace only and does not perform shell expansion. For
// commands requiring shell features (pipes, redirections, etc.), wrap that
// logic in a repository script/wrapper command, or invoke a shell explicitly
// as the command.
func splitCommand(cmd string) (string, []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return cmd, nil
	}
	return parts[0], parts[1:]
}

// DefaultConfigWithRace returns a default config with race detection support.
func DefaultConfigWithRace(worktreePath string, opts *DetectOptions, raceEnabled bool) Config {
	return Config{
		Steps:           detectSteps(worktreePath, opts, raceEnabled),
		GoRaceDetection: raceEnabled,
	}
}

// matchesChangedFiles returns true if any file in changedFiles matches any of
// the given glob patterns (doublestar syntax). Returns true when patterns is
// empty or nil (step always runs). Returns false when changedFiles is empty and
// patterns is non-empty (nothing to match). An invalid glob pattern is logged
// and treated as a match so the step runs rather than being silently skipped.
func matchesChangedFiles(patterns, changedFiles []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, f := range changedFiles {
		for _, p := range patterns {
			ok, err := doublestar.Match(p, f)
			if err != nil {
				log.Printf("[temper] invalid glob pattern %q: %v — treating as match", p, err)
				return true
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// verifyCleanCheck runs `git status --porcelain` against the given pathspecs
// in the worktree and returns its trimmed output. Empty output means no
// modifications, additions, or untracked files exist under those paths.
//
// A configurable timeout (gitTimeout, defaulting to DefaultGitTimeout) guards
// against a stuck git invocation. The git repo-location env vars are stripped
// (executil.CleanGitEnv) so the -C flag reliably targets the correct repo even
// when Forge itself runs inside a git worktree.
func verifyCleanCheck(ctx context.Context, worktreePath string, pathspecs []string, gitTimeout time.Duration) (string, error) {
	if len(pathspecs) == 0 {
		return "", nil
	}
	if gitTimeout <= 0 {
		gitTimeout = DefaultGitTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	args := []string{"-C", worktreePath, "status", "--porcelain", "--"}
	args = append(args, pathspecs...)
	cmd := executil.HideWindow(exec.CommandContext(checkCtx, "git", args...))
	cmd.Env = executil.CleanGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git status --porcelain: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ChangedFilesFromGit returns the list of changed file paths (relative to the
// repo root) by running "git diff --name-only <base>..HEAD" in the worktree.
// It returns an error if git fails or times out, so callers can log a warning
// and distinguish "no changes" from "couldn't compute changes".
//
// Deletions and renames are both represented by their old path as well as
// their new one, because a step is gated on the files the diff TOUCHES, not on
// the files that survive it: deleting the last .cs file in a project, or moving
// a Go file out of the tree, changes what builds just as much as editing it
// does. A deletion already names its (only) path, but rename detection would
// otherwise report a move as the destination alone — so --no-renames is passed
// and a move is reported as a delete plus an add, naming both sides.
//
// Like verifyCleanCheck it runs with the git repo-location env vars stripped:
// an inherited GIT_DIR would otherwise make the diff describe the ambient
// repository's files instead of the worktree's.
func ChangedFilesFromGit(ctx context.Context, worktreePath, baseBranch string) ([]string, error) {
	if baseBranch == "" {
		return nil, nil
	}
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath, "diff", "--name-only", "--no-renames", baseBranch+"..HEAD"))
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only --no-renames %s..HEAD: %w", baseBranch, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Run executes all verification steps in sequence.
// It stops on the first non-optional failure.
// db, beadID, and anvil are used to log lifecycle events; db may be nil to skip logging.
func Run(ctx context.Context, worktreePath string, cfg Config, db *state.DB, beadID, anvil string) *Result {
	result := &Result{}
	start := time.Now()

	// Resolve the effective step timeout, git timeout, and output cap, each
	// falling back to its package default when the Config leaves it unset.
	stepTimeout := cfg.StepTimeout
	if stepTimeout <= 0 {
		stepTimeout = DefaultStepTimeout
	}
	gitTimeout := cfg.GitTimeout
	if gitTimeout <= 0 {
		gitTimeout = DefaultGitTimeout
	}
	outputCap := cfg.OutputCap
	if outputCap <= 0 {
		outputCap = DefaultOutputCap
	}

	if db != nil {
		_ = db.LogEvent(state.EventTemperStarted, fmt.Sprintf("Starting %d verification step(s) for %s", len(cfg.Steps), beadID), beadID, anvil)
	}

	for _, step := range cfg.Steps {
		// Scan-only conflict-marker step: no Command, just a content scan
		// over VerifyNoConflictMarkers. Runs unconditionally — Paths gating
		// is intentionally bypassed so committed merge markers are caught
		// even when no source change would trigger the surrounding rebuild.
		if step.Command == "" && len(step.VerifyNoConflictMarkers) > 0 {
			stepResult := runConflictMarkerScan(ctx, worktreePath, step)
			result.Steps = append(result.Steps, stepResult)
			if !stepResult.Passed && !step.Optional {
				result.FailedStep = step.Name
				result.Classification = stepResult.Classification
				break
			}
			continue
		}

		// Skip steps whose path filters don't match any changed files.
		if len(step.Paths) > 0 && cfg.ChangedFiles != nil && !matchesChangedFiles(step.Paths, cfg.ChangedFiles) {
			log.Printf("[temper] Skipping step %q: no changed files match paths %v", step.Name, step.Paths)
			result.Steps = append(result.Steps, StepResult{
				Name:       step.Name,
				Command:    fmt.Sprintf("%s %s", step.Command, strings.Join(step.Args, " ")),
				Passed:     true,
				Skipped:    true,
				SkipReason: SkipReasonPathFilter,
				Optional:   step.Optional,
			})
			continue
		}

		// Block destructive npm install commands when node_modules is a
		// junction/symlink — npm ci does rm -rf node_modules which would
		// destroy the main checkout's dependencies or fail with EPERM.
		if isDestructiveNpmInstall(step) {
			stepDir := resolveStepDir(worktreePath, step.Dir)
			if isNodeModulesLinked(stepDir) {
				log.Printf("[temper] Blocking step %q: node_modules is a symlink/junction and %s %s would destroy the linked target", step.Name, step.Command, strings.Join(step.Args, " "))
				result.Steps = append(result.Steps, StepResult{
					Name:       step.Name,
					Command:    fmt.Sprintf("%s %s", step.Command, strings.Join(step.Args, " ")),
					ExitCode:   -1,
					Output:     fmt.Sprintf("Blocked: node_modules in %s is a symlink or junction pointing to the main checkout. Running %q would destroy the shared node_modules. Remove this step from your temper configuration — dependencies are already available via the junction.", stepDir, step.Command+" "+strings.Join(step.Args, " ")),
					Passed:     true,
					Skipped:    true,
					SkipReason: SkipReasonBlockedInstall,
					Optional:   step.Optional,
				})
				continue
			}
		}

		stepResult := runStep(ctx, worktreePath, step, stepTimeout, outputCap)

		// VerifyClean: if the step succeeded but mutated tracked or untracked
		// files under one of the VerifyClean pathspecs, the committed artifact
		// is out of sync with current source — convert the step to a failure
		// so the pipeline loops back to Smith with actionable feedback.
		if stepResult.Passed && len(step.VerifyClean) > 0 {
			dirty, err := verifyCleanCheck(ctx, worktreePath, step.VerifyClean, gitTimeout)
			if err != nil {
				log.Printf("[temper] VerifyClean check for step %q failed: %v", step.Name, err)
				stepResult.Passed = false
				stepResult.ExitCode = -1
				stepResult.Output = fmt.Sprintf("VerifyClean check could not be performed for step %q: %v\nCannot confirm that committed artifacts are up to date — treating as failure.", step.Name, err)
			} else if dirty != "" {
				stepResult.Passed = false
				stepResult.ExitCode = -1
				msg := fmt.Sprintf(
					"Step %q ran successfully but modified committed files under %v.\n"+
						"This means the committed build output is stale relative to the current source.\n"+
						"Run the same step locally and commit the regenerated artifacts so the\n"+
						"committed bundle matches a fresh build of the source.\n\n"+
						"git status --porcelain output:\n%s",
					step.Name, step.VerifyClean, dirty,
				)
				if stepResult.Output == "" {
					stepResult.Output = msg
				} else {
					stepResult.Output = stepResult.Output + "\n\n" + msg
				}
			}
		}

		result.Steps = append(result.Steps, stepResult)

		if !stepResult.Passed && !step.Optional {
			result.FailedStep = step.Name
			result.Classification = stepResult.Classification
			break
		}
	}

	result.Duration = time.Since(start)
	result.Passed = result.FailedStep == ""
	result.Summary = buildSummary(result)

	// Persist the full, untruncated output of every step to the worktree log
	// directory so it survives worktree cleanup for post-mortem debugging.
	writeTemperLog(worktreePath, result)

	if db != nil {
		if result.Passed {
			optWarn := 0
			for _, s := range result.Steps {
				if s.Optional && !s.Passed {
					optWarn++
				}
			}
			var msg string
			if optWarn > 0 {
				msg = fmt.Sprintf("All required checks passed in %.1fs (%d optional step(s) warned)", result.Duration.Seconds(), optWarn)
			} else {
				msg = fmt.Sprintf("All required checks passed in %.1fs (no optional warnings)", result.Duration.Seconds())
			}
			_ = db.LogEvent(state.EventTemperPassed, msg, beadID, anvil)
		} else {
			_ = db.LogEvent(state.EventTemperFailed, fmt.Sprintf("Failed at step %q in %.1fs", result.FailedStep, result.Duration.Seconds()), beadID, anvil)
		}
	}

	return result
}

// stepStatus returns the short status label ("PASS", "FAIL", "WARN", "SKIP")
// for a step, matching the classification used in buildSummary.
func stepStatus(s StepResult) string {
	switch {
	case s.Skipped:
		return "SKIP"
	case s.Passed:
		return "PASS"
	case s.Optional:
		return "WARN"
	default:
		return "FAIL"
	}
}

// writeTemperLog persists the full, untruncated output of every temper step to
// <worktreePath>/.forge-logs/temper-<ts>-<seq>.log. The pipeline's
// preserveWorktreeLogs copies this file — alongside the stage-named Smith logs
// — to ~/.forge/logs/<beadID>/, so failed-test forensics survive worktree
// cleanup. Unlike the 1000-char ingot OutputSummary, this log contains the
// complete combined stdout/stderr of each step. Writing is best-effort: any
// error is logged and otherwise ignored so a log-persistence failure never
// affects the verification outcome.
var temperLogSeq atomic.Int64

func writeTemperLog(worktreePath string, result *Result) {
	logDir := filepath.Join(worktreePath, ".forge-logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Printf("[temper] failed to create log dir %s: %v", logDir, err)
		return
	}
	seq := temperLogSeq.Add(1)
	logPath := filepath.Join(logDir, fmt.Sprintf("temper-%d-%d.log", time.Now().UnixMilli(), seq))

	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("[temper] failed to create temper log %s: %v", logPath, err)
		return
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	defer func() {
		if err := w.Flush(); err != nil {
			log.Printf("[temper] failed to flush temper log %s: %v", logPath, err)
		}
	}()

	fmt.Fprintf(w, "Temper verification — %d step(s), total %.1fs\n", len(result.Steps), result.Duration.Seconds())
	if result.Passed {
		fmt.Fprint(w, "Overall: PASS\n")
	} else {
		fmt.Fprintf(w, "Overall: FAIL (first failing step: %s)\n", result.FailedStep)
	}
	for i, s := range result.Steps {
		fmt.Fprintf(w, "\n=== [%d] %s — %s ===\n", i+1, s.Name, stepStatus(s))
		fmt.Fprintf(w, "Command:  %s\n", s.Command)
		fmt.Fprintf(w, "Exit:     %d\n", s.ExitCode)
		fmt.Fprintf(w, "Duration: %.1fs\n", s.Duration.Seconds())
		if s.Optional {
			fmt.Fprint(w, "Optional: true\n")
		}
		if s.Skipped {
			fmt.Fprint(w, "Skipped:  true\n")
		}
		fmt.Fprint(w, "Output:\n")
		fmt.Fprint(w, s.Output)
		if !strings.HasSuffix(s.Output, "\n") {
			fmt.Fprint(w, "\n")
		}
	}
}

// runStep executes a single verification step. defaultTimeout is the resolved
// per-step timeout used when the step does not set its own Timeout; outputCap
// bounds the retained combined stdout+stderr.
func runStep(ctx context.Context, worktreePath string, step Step, defaultTimeout time.Duration, outputCap int) StepResult {
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := worktreePath
	if step.Dir != "" {
		if strings.HasPrefix(step.Dir, "/") || strings.HasPrefix(step.Dir, "\\") || (len(step.Dir) >= 2 && step.Dir[1] == ':') {
			dir = step.Dir
		} else {
			dir = worktreePath + "/" + step.Dir
		}
	}

	start := time.Now()

	cmd := executil.HideWindow(exec.CommandContext(stepCtx, step.Command, step.Args...))
	// Put the step in its own process group / Windows process group so we can
	// terminate any background descendants it spawned. Build scripts (e.g. a
	// storybook smoke test) may launch long-running servers like
	// `npx http-server storybook-static` that would otherwise outlive the
	// parent and hold worktree files open on Windows, blocking the next
	// worktree recreation for the same bead.
	executil.SetProcessGroup(cmd)
	cmd.Dir = dir

	// Bound the retained output with a head+tail buffer so a verbose test suite
	// cannot balloon memory or the warden/fix prompt that embeds StepResult.Output.
	output := newHeadTailBuffer(outputCap)
	cmd.Stdout = output
	cmd.Stderr = output

	err := cmd.Run()
	// Reap the tree even on success — a step may exit cleanly while leaving
	// background children alive (e.g. a test runner that spawns an
	// http-server as a side effect).
	_ = executil.KillProcessTree(cmd)
	duration := time.Since(start)
	ps := cmd.ProcessState

	exitCode := 0
	passed := true
	if err != nil {
		passed = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	outStr := output.String()

	// Tolerate a .NET test-host teardown crash: when opted in, a non-zero exit
	// whose output shows all tests passed AND an explicit host-crash marker is
	// treated as a pass. This is the "testhost OOM'd at teardown after every
	// test passed" case — a real test failure (Failed: N>0) or a build error
	// (no crash marker) is NOT tolerated.
	if !passed && step.TolerateHostCrash && dotnetTestHostCrashTolerable(outStr) {
		log.Printf("[temper] step %q: exit %d tolerated — all tests passed but the .NET test host crashed at teardown", step.Name, exitCode)
		outStr += fmt.Sprintf(
			"\n\n[temper] NOTE: step exited %d, but every test passed and the .NET test host crashed/aborted at teardown (tolerate_host_crash). Treated as PASS — this is a host-level failure, not a test failure.\n",
			exitCode)
		passed = true
	}

	var classification Classification
	if !passed {
		classification = classifyFailure(stepCtx, ps, outStr)
		if classification == ClassificationTimeout {
			log.Printf("[temper] step %q: killed after exceeding timeout %s — classified as timeout", step.Name, timeout)
		} else if classification == ClassificationInfra {
			log.Printf("[temper] step %q: exit %d classified as infra (signal death or host-crash marker)", step.Name, exitCode)
		}
	}

	return StepResult{
		Name:           step.Name,
		Command:        fmt.Sprintf("%s %s", step.Command, strings.Join(step.Args, " ")),
		ExitCode:       exitCode,
		Output:         outStr,
		Duration:       duration,
		Passed:         passed,
		Optional:       step.Optional,
		Classification: classification,
	}
}

// classifyFailure categorises why a step failed. Order matters: a timeout kill
// also manifests as a signal death, so the context deadline is checked first.
// A summary showing genuine test failures is authoritative — a real test
// failure that also crashed the host is a test_failure, not infra.
func classifyFailure(stepCtx context.Context, ps *os.ProcessState, output string) Classification {
	if stepCtx.Err() == context.DeadlineExceeded {
		return ClassificationTimeout
	}
	if dotnetTestFailureRE.MatchString(output) {
		return ClassificationTestFailure
	}
	if infraFailureRE.MatchString(output) {
		return ClassificationInfra
	}
	if signaled, _ := processSignaled(ps); signaled {
		return ClassificationInfra
	}
	return ClassificationTestFailure
}

// infraFailureRE matches output markers that indicate an infrastructure/host
// failure rather than a real test failure — generalising the .NET test-host
// crash carve-out to Go and native process deaths. Kept specific to
// process-death signatures (not bare "out of memory" which can legitimately
// appear in test assertions) to avoid mis-classifying real failures.
var infraFailureRE = regexp.MustCompile(`(?i)test host process crashed|active test run was aborted|testhost process .*(crashed|exited)|aborted \(core dumped\)|signal: killed|signal: segmentation fault|signal: (aborted|bus error)|fatal error: runtime: out of memory|runtime: out of memory`)

// dotnetTestHostCrashRE detects an explicit .NET/VSTest test-host crash or abort
// marker — the signal that a non-zero exit is a host-level failure rather than a
// failing test. Covers the observed "Test host process crashed : Out of memory"
// plus the common VSTest phrasings and a native "Aborted (core dumped)".
var dotnetTestHostCrashRE = regexp.MustCompile(`(?i)test host process crashed|active test run was aborted|testhost process .*(crashed|exited)|aborted \(core dumped\)`)

// dotnetTestPassedSummaryRE matches a VSTest all-passed summary line, e.g.
// "Passed!  - Failed:     0, Passed:   455, ...". Requires the "Passed!" prefix
// and a zero failed count.
var dotnetTestPassedSummaryRE = regexp.MustCompile(`Passed!\s*-\s*Failed:\s*0\b`)

// dotnetTestFailureRE matches a failing VSTest summary — either the "Failed!"
// prefix or a non-zero failed count — so a run with any real test failure is
// never tolerated.
var dotnetTestFailureRE = regexp.MustCompile(`Failed!\s*-\s*Failed:|Failed:\s*[1-9]`)

// dotnetTestHostCrashTolerable reports whether a non-zero test-step exit is a
// post-test .NET host crash rather than a real test failure. It requires BOTH a
// completed all-passed summary with no failing tests AND an explicit host-crash
// marker, so a genuine test failure (Failed: N>0) or a build failure (no crash
// marker) still fails the step.
func dotnetTestHostCrashTolerable(output string) bool {
	if !dotnetTestHostCrashRE.MatchString(output) {
		return false
	}
	if dotnetTestFailureRE.MatchString(output) {
		return false
	}
	return dotnetTestPassedSummaryRE.MatchString(output)
}

// Default `paths` globs attached to auto-detected steps so changed-file gating
// works on an anvil that has configured nothing. They are deliberately WIDER
// than the files a compiler reads: the cost of a false match is one step that
// runs unnecessarily, and the cost of a miss is a verification that reports
// PASS over code it never looked at. So each set covers sources, the manifests
// that pin their dependencies, and the fixture trees their tests read.
//
// Explicit `temper.steps` from an anvil's config never pass through here — a
// step that declares no `paths` there still runs unconditionally, as before.
var (
	// defaultGoPaths gates the Go build/vet/lint/test steps. `**/go.mod`
	// (not just the root one) covers multi-module repos, testdata is in
	// because Go tests read fixtures from it, and the cgo/assembly
	// extensions are in because `go build` compiles them.
	defaultGoPaths = []string{
		"**/*.go",
		"**/go.mod",
		"**/go.sum",
		"go.work",
		"go.work.sum",
		"**/testdata/**",
		"**/*.s",
		"**/*.c",
		"**/*.h",
	}
	// defaultDotnetPaths gates the `dotnet build` / `dotnet test` steps.
	// Beyond C# sources it covers the project/solution files, the MSBuild
	// fragments they import (Directory.Build.props/targets and friends),
	// the Razor/Blazor view formats that compile into the assembly, the
	// settings files a test host loads, and the resource files the
	// compiler embeds.
	defaultDotnetPaths = []string{
		"**/*.cs",
		"**/*.csproj",
		"**/*.vb",
		"**/*.vbproj",
		"**/*.fs",
		"**/*.fsproj",
		"*.sln",
		"**/*.sln",
		"**/*.slnx",
		"**/*.props",
		"**/*.targets",
		"**/appsettings*.json",
		"**/*.razor",
		"**/*.cshtml",
		"**/*.resx",
		"**/nuget.config",
		"**/NuGet.config",
		"**/packages.lock.json",
	}
)

// nodeDirPaths returns the default `paths` globs for a detected Node project
// rooted at dir.
//
// A project in a SUBDIRECTORY is gated on that whole subtree: everything the
// build and test scripts read lives under it, and gating on the directory
// rather than on a file-extension list means a config format nobody enumerated
// still triggers the step.
//
// A project at the repository ROOT is left ungated (nil). "Everything under
// the root" is every file in the repo, so the glob would be pure overhead —
// and, more importantly, a root-level Node project has no directory boundary
// separating its inputs from anything else in the repo, so there is no
// conservative subset to gate on.
func nodeDirPaths(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{dir + "/**"}
}

// detectSteps auto-detects project type and returns appropriate steps.
func detectSteps(worktreePath string, opts *DetectOptions, goRace bool) []Step {
	var steps []Step

	// Check for Go project
	if fileExists(worktreePath, "go.mod") {
		steps = append(steps, Step{
			Name:    "build",
			Command: "go",
			Args:    []string{"build", "./..."},
			Timeout: 3 * time.Minute,
			Paths:   defaultGoPaths,
		})
		steps = append(steps, Step{
			Name:    "vet",
			Command: "go",
			Args:    []string{"vet", "./..."},
			Timeout: 2 * time.Minute,
			Paths:   defaultGoPaths,
		})

		// golangci-lint: optional step, skipped if binary not found or disabled
		disableLint := opts != nil && opts.DisableGolangciLint
		if !disableLint {
			if _, err := exec.LookPath("golangci-lint"); err == nil {
				steps = append(steps, Step{
					Name:     "golangci-lint",
					Command:  "golangci-lint",
					Args:     []string{"run", "./..."},
					Timeout:  3 * time.Minute,
					Optional: true,
					Paths:    defaultGoPaths,
				})
			}
		}

		steps = append(steps, Step{
			Name:    "test",
			Command: "go",
			Args:    []string{"test", "-short", "./..."},
			Timeout: 5 * time.Minute,
			Paths:   defaultGoPaths,
		})
		if goRace {
			steps = append(steps, Step{
				Name:    "race",
				Command: "go",
				Args:    []string{"test", "-race", "-short", "./..."},
				Timeout: 10 * time.Minute,
				Paths:   defaultGoPaths,
			})
		}
	}

	// Check for .NET project
	if hasGlob(worktreePath, "*.sln") || hasGlob(worktreePath, "**/*.csproj") {
		steps = append(steps, Step{
			Name:    "build",
			Command: "dotnet",
			Args:    []string{"build", "--no-restore"},
			Timeout: 3 * time.Minute,
			Paths:   defaultDotnetPaths,
		})
		steps = append(steps, Step{
			Name:    "test",
			Command: "dotnet",
			Args:    []string{"test", "--no-build"},
			Timeout: 5 * time.Minute,
			Paths:   defaultDotnetPaths,
		})
	}

	// Check for Node.js project — at root and common subdirectories.
	for _, nodeDir := range detectNodeDirs(worktreePath) {
		prefix := ""
		dir := ""
		if nodeDir != "" {
			prefix = nodeDir + ":"
			dir = nodeDir
		}
		steps = append(steps, Step{
			Name:     prefix + "lint",
			Command:  "npm",
			Args:     []string{"run", "lint"},
			Dir:      dir,
			Timeout:  2 * time.Minute,
			Optional: true, // lint might not be configured
			Paths:    nodeDirPaths(nodeDir),
		})
		// When the test:run script invokes Vitest, pass through worker-cap
		// flags via `npm run ... --` so the pool stays single-threaded. See
		// vitestSingleThreadArgs for rationale.
		testArgs := []string{"run", "test:run"}
		if scriptUsesVitest(filepath.Join(worktreePath, nodeDir), "test:run") {
			testArgs = append(testArgs, "--")
			testArgs = append(testArgs, vitestSingleThreadArgs...)
		}
		steps = append(steps, Step{
			Name:     prefix + "test",
			Command:  "npm",
			Args:     testArgs,
			Dir:      dir,
			Timeout:  5 * time.Minute,
			Optional: true, // test script might not exist
			Paths:    nodeDirPaths(nodeDir),
		})
	}

	// Embedded frontend bundle pattern: detect projects that ship a frontend
	// inside a Go binary via go:embed. The Hearth 2.0 layout puts source at
	// internal/web/frontend and the committed build output at internal/web/dist.
	// When a Smith touches frontend src without rebuilding dist, the embedded
	// bundle in the deployed binary diverges from source — Forge-lmxc was
	// caused by exactly that. Append steps that rebuild and verify the bundle
	// when frontend files are present in the diff.
	for _, eb := range detectEmbeddedBundles(worktreePath) {
		steps = append(steps, eb.Steps()...)
	}

	// Fallback: just check if it builds
	if len(steps) == 0 {
		steps = append(steps, Step{
			Name:    "echo",
			Command: "echo",
			Args:    []string{"No build system detected"},
			Timeout: 5 * time.Second,
		})
	}

	return steps
}

// maxStepOutputLen is the maximum number of bytes of step output to
// include in the summary for a failed step. This keeps feedback actionable
// without overwhelming the Smith prompt with enormous test logs.
const maxStepOutputLen = 4000

// buildSummary creates a human-readable summary of the verification result.
// For failed steps, the actual command output (compiler errors, test failures,
// etc.) is included so that Smith can diagnose and fix the problems.
func buildSummary(r *Result) string {
	var b strings.Builder
	optionalWarnings := 0
	for _, s := range r.Steps {
		var status string
		switch {
		case s.Skipped:
			status = "SKIP"
		case s.Passed:
			status = "PASS"
		case s.Optional:
			status = "WARN"
			optionalWarnings++
		default:
			status = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %s (%.1fs)\n", status, s.Name, s.Duration.Seconds())

		// Include the actual output for failed steps so the next Smith
		// iteration knows exactly what went wrong and can fix it.
		if !s.Passed && s.Output != "" {
			output := strings.TrimRight(s.Output, "\n\r\t ")
			if len(output) > maxStepOutputLen {
				output = output[len(output)-maxStepOutputLen:]
				output = "... (truncated)\n" + output
			}
			fmt.Fprintf(&b, "\n```\n%s\n```\n\n", output)
		}
	}
	if r.Passed {
		if optionalWarnings > 0 {
			fmt.Fprintf(&b, "\nAll required checks passed in %.1fs (%d optional step(s) warned)", r.Duration.Seconds(), optionalWarnings)
		} else {
			fmt.Fprintf(&b, "\nAll required checks passed in %.1fs (no optional warnings)", r.Duration.Seconds())
		}
	} else {
		fmt.Fprintf(&b, "\nFailed at step: %s", r.FailedStep)
	}
	return b.String()
}

// nodeSubdirs is the list of common subdirectories where a Node.js project
// might live in hybrid repositories (e.g., Go at root, Node in web/).
var nodeSubdirs = []string{"web", "frontend", "client", "app", "ui"}

// embeddedBundle describes a frontend-source/built-output pair embedded into a
// Go binary via go:embed. The auto-detector emits temper steps that reinstall
// dependencies, rebuild the bundle, and verify the committed output matches a
// fresh build of the source.
type embeddedBundle struct {
	// Name is a short identifier used as a step-name prefix (e.g. "hearth").
	Name string
	// FrontendDir is the relative path to the frontend source directory
	// (containing package.json) within the worktree.
	FrontendDir string
	// DistDir is the relative path to the committed build output directory
	// within the worktree. This is what gets verified after `npm run build`.
	DistDir string
}

// embeddedBundleLayouts lists the known embedded-frontend layouts. Currently
// the only entry is Forge's own Hearth 2.0 web UI; new entries can be added
// for other anvils that ship an embedded bundle.
var embeddedBundleLayouts = []embeddedBundle{
	{Name: "hearth", FrontendDir: "internal/web/frontend", DistDir: "internal/web/dist"},
}

// detectEmbeddedBundles returns the embedded-bundle layouts present in the
// worktree. A layout matches when its FrontendDir contains a package.json and
// its DistDir contains an index.html (the marker file produced by Vite).
func detectEmbeddedBundles(worktreePath string) []embeddedBundle {
	var found []embeddedBundle
	for _, eb := range embeddedBundleLayouts {
		if !fileExists(filepath.Join(worktreePath, eb.FrontendDir), "package.json") {
			continue
		}
		if !fileExists(filepath.Join(worktreePath, eb.DistDir), "index.html") {
			continue
		}
		found = append(found, eb)
	}
	return found
}

// Steps returns the temper steps for an embedded-bundle layout: a conflict-
// marker scan over the committed dist, then install deps, then rebuild and
// verify the committed dist matches a fresh build. The Paths filter on the
// install/build steps ensures they only run when files in the frontend source
// or build config actually changed in the diff. The conflict-marker scan
// runs unconditionally (no Paths filter) so committed `<<<<<<<` / `=======`
// / `>>>>>>>` markers — e.g. from a rebase Smith that resolved a bundle
// conflict by leaving the markers in place — are caught even when the
// rebuild would otherwise be skipped.
func (eb embeddedBundle) Steps() []Step {
	paths := []string{
		eb.FrontendDir + "/src/**",
		eb.FrontendDir + "/package.json",
		eb.FrontendDir + "/package-lock.json",
		eb.FrontendDir + "/index.html",
		eb.FrontendDir + "/vite.config.ts",
		eb.FrontendDir + "/vite.config.js",
		eb.FrontendDir + "/tsconfig.json",
		eb.FrontendDir + "/tsconfig.app.json",
		eb.FrontendDir + "/tsconfig.node.json",
	}
	return []Step{
		{
			Name:                    eb.Name + "-frontend-conflict-markers",
			Timeout:                 30 * time.Second,
			VerifyNoConflictMarkers: []string{eb.DistDir},
		},
		{
			Name:    eb.Name + "-frontend-install",
			Command: "npm",
			Args:    []string{"install", "--no-audit", "--no-fund"},
			Dir:     eb.FrontendDir,
			Timeout: 5 * time.Minute,
			Paths:   paths,
		},
		{
			Name:    eb.Name + "-frontend-build",
			Command: "npm",
			Args:    []string{"run", "build"},
			Dir:     eb.FrontendDir,
			Timeout: 5 * time.Minute,
			Paths:   paths,
			// The committed dist/ must match a fresh build. If `npm run build`
			// modifies any file under DistDir, Smith committed a stale bundle.
			VerifyClean: []string{eb.DistDir},
		},
	}
}

// runConflictMarkerScan performs an in-process scan for git merge-conflict
// markers over the configured pathspecs and returns a StepResult. The scan
// fails the step when any line beginning with 7+ consecutive `<`, `=`, or
// `>` characters is found in any file present under the listed paths, and
// the failure message names the offending file:line entries so Smith (and a
// human reader) can locate them immediately.
func runConflictMarkerScan(ctx context.Context, worktreePath string, step Step) StepResult {
	timeout := step.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	hits, err := scanConflictMarkers(scanCtx, worktreePath, step.VerifyNoConflictMarkers)
	duration := time.Since(start)

	cmdSummary := "verify-no-conflict-markers " + strings.Join(step.VerifyNoConflictMarkers, " ")
	result := StepResult{
		Name:     step.Name,
		Command:  cmdSummary,
		Duration: duration,
		Optional: step.Optional,
	}
	if err != nil {
		result.Passed = false
		result.ExitCode = -1
		result.Output = fmt.Sprintf("Conflict-marker scan failed for %v: %v", step.VerifyNoConflictMarkers, err)
		return result
	}
	if len(hits) > 0 {
		result.Passed = false
		result.ExitCode = -1
		result.Output = fmt.Sprintf(
			"Found git merge-conflict markers in files under %v.\n"+
				"Lines starting with `<<<<<<<`, `=======`, or `>>>>>>>` mean a rebase or\n"+
				"merge was resolved by committing the conflict markers themselves rather\n"+
				"than the resolved content. Open each file, remove the markers, regenerate\n"+
				"the artifact (e.g. `npm run build`) if it is a built bundle, and commit\n"+
				"the cleaned result.\n\n"+
				"Offending lines:\n%s",
			step.VerifyNoConflictMarkers, strings.Join(hits, "\n"))
		return result
	}
	result.Passed = true
	return result
}

// scanConflictMarkers walks the given pathspecs (relative to worktreePath)
// and returns "<relative-path>:<line>" entries for every line that begins
// with a git merge-conflict marker. A non-existent pathspec is treated as
// "nothing to scan" rather than an error so the check is no-op friendly on
// repos that lack the configured directory. The walk is aborted when ctx is
// cancelled or its deadline is exceeded.
func scanConflictMarkers(ctx context.Context, worktreePath string, pathspecs []string) ([]string, error) {
	var hits []string
	for _, spec := range pathspecs {
		if err := ctx.Err(); err != nil {
			return hits, err
		}
		base := filepath.Join(worktreePath, spec)
		info, err := os.Stat(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", spec, err)
		}
		if info.IsDir() {
			walkErr := filepath.Walk(base, func(path string, fi os.FileInfo, werr error) error {
				if werr != nil {
					return werr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if fi.IsDir() {
					return nil
				}
				fileHits, ferr := scanFileForConflictMarkers(path, worktreePath)
				if ferr != nil {
					return ferr
				}
				hits = append(hits, fileHits...)
				return nil
			})
			if walkErr != nil {
				return nil, fmt.Errorf("walk %s: %w", spec, walkErr)
			}
		} else {
			fileHits, ferr := scanFileForConflictMarkers(base, worktreePath)
			if ferr != nil {
				return nil, ferr
			}
			hits = append(hits, fileHits...)
		}
	}
	return hits, nil
}

// scanFileForConflictMarkers reads a single file and returns "<rel>:<line>"
// entries for every line that starts with a conflict marker. Reader errors
// (e.g. a token longer than the scanner buffer in a minified bundle) are
// tolerated — the scan returns the hits it found so far rather than aborting,
// because false negatives on the long-line tail are preferable to losing the
// matches already discovered.
func scanFileForConflictMarkers(path, worktreePath string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	rel, relErr := filepath.Rel(worktreePath, path)
	if relErr != nil || rel == "" {
		rel = path
	}
	rel = filepath.ToSlash(rel)

	scanner := bufio.NewScanner(f)
	// Minified JS bundles routinely contain single lines well past the 64 KiB
	// default; bump the buffer to 8 MiB to cover realistic dist payloads.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var hits []string
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineHasConflictMarkerPrefix(scanner.Bytes()) {
			hits = append(hits, fmt.Sprintf("%s:%d", rel, lineNum))
		}
	}
	if serr := scanner.Err(); serr != nil {
		log.Printf("[temper] conflict-marker scan: %s: %v (continuing with %d hit(s) so far)", rel, serr, len(hits))
	}
	return hits, nil
}

// lineHasConflictMarkerPrefix reports whether the line starts with 7+
// consecutive `<`, `=`, or `>` characters — the shape of standard git
// conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`) including the 8-char
// variant git emits for some recursive-merge strategies.
func lineHasConflictMarkerPrefix(line []byte) bool {
	return hasRunPrefix(line, '<') || hasRunPrefix(line, '=') || hasRunPrefix(line, '>')
}

// hasRunPrefix returns true when line starts with at least 7 copies of c.
func hasRunPrefix(line []byte, c byte) bool {
	const minRun = 7
	if len(line) < minRun {
		return false
	}
	for i := 0; i < minRun; i++ {
		if line[i] != c {
			return false
		}
	}
	return true
}

// scriptUsesVitest reports whether the named script in dir/package.json
// invokes Vitest. Returns false when package.json is missing, unreadable, or
// does not declare the script — those cases are safe defaults because the
// fallback `npm run <script>` invocation will either no-op or be reported as
// an optional test failure by Temper without affecting the overall result.
func scriptUsesVitest(dir, scriptName string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	script, ok := pkg.Scripts[scriptName]
	if !ok {
		return false
	}
	return commandInvokesVitest(script)
}

// commandInvokesVitest reports whether the given npm script command line
// invokes vitest as one of its tokens. It matches `vitest`, `vitest run`,
// `npx vitest`, and `node_modules/.bin/vitest` style invocations without
// matching substrings that merely contain the letters (e.g. `vitestic`).
func commandInvokesVitest(script string) bool {
	for _, tok := range strings.Fields(script) {
		base := tok
		if i := strings.LastIndexAny(tok, "/\\"); i >= 0 {
			base = tok[i+1:]
		}
		base = strings.TrimSuffix(base, ".cmd")
		base = strings.TrimSuffix(base, ".exe")
		if base == "vitest" {
			return true
		}
	}
	return false
}

// detectNodeDirs returns the relative directories containing a package.json.
// An empty string entry means the root directory.
func detectNodeDirs(worktreePath string) []string {
	var dirs []string
	if fileExists(worktreePath, "package.json") {
		dirs = append(dirs, "")
	}
	for _, sub := range nodeSubdirs {
		if fileExists(filepath.Join(worktreePath, sub), "package.json") {
			dirs = append(dirs, sub)
		}
	}
	return dirs
}

// fileExists checks if a file exists at the given path.
func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// hasGlob checks if any file matching the glob pattern exists.
func hasGlob(dir, pattern string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, pattern))
	return len(matches) > 0
}

// resolveStepDir returns the absolute working directory for a step.
func resolveStepDir(worktreePath, stepDir string) string {
	if stepDir == "" {
		return worktreePath
	}
	if strings.HasPrefix(stepDir, "/") || strings.HasPrefix(stepDir, "\\") || (len(stepDir) >= 2 && stepDir[1] == ':') {
		return stepDir
	}
	return filepath.Join(worktreePath, stepDir)
}

// isNodeModulesLinked returns true if the node_modules directory in dir is a
// symlink or junction rather than a real directory.
func isNodeModulesLinked(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, "node_modules"))
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info)
}

// isDestructiveNpmInstall returns true if the step would run an npm command
// that wipes node_modules before installing (currently npm ci and
// npm clean-install). These are dangerous when node_modules is a junction.
func isDestructiveNpmInstall(step Step) bool {
	base := filepath.Base(step.Command)
	base = strings.TrimSuffix(base, ".cmd")
	base = strings.TrimSuffix(base, ".exe")

	if base != "npm" {
		return false
	}
	if len(step.Args) == 0 {
		return false
	}
	switch step.Args[0] {
	case "ci", "clean-install":
		return true
	}
	return false
}
