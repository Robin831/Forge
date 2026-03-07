// Package temper runs build, lint, and test verification in a worktree.
//
// Temper ("tempering the steel") validates that Claude's changes compile,
// pass linting, and pass tests before progressing to the Warden review stage.
// Commands are configurable per-anvil.
package temper

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
	"gopkg.in/yaml.v3"
)

// StepResult captures the outcome of a single verification step.
type StepResult struct {
	// Name identifies the step (e.g., "build", "lint", "test").
	Name string
	// Command is the full command that was run.
	Command string
	// ExitCode is the process exit code.
	ExitCode int
	// Output is the combined stdout+stderr.
	Output string
	// Duration is how long the step took.
	Duration time.Duration
	// Passed indicates whether the step succeeded.
	Passed bool
	// Optional mirrors the Step.Optional flag — failure here does not fail
	// the overall check. Surfaced so summaries can render it distinctly.
	Optional bool
}

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
	// Summary is a human-readable summary of the verification.
	Summary string
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
	// Timeout is the maximum duration for this step. Zero means 5 minutes.
	Timeout time.Duration
	// Optional means failure here doesn't fail the overall check.
	Optional bool
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
}

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

// DefaultConfigWithRace returns a default config with race detection support.
func DefaultConfigWithRace(worktreePath string, opts *DetectOptions, raceEnabled bool) Config {
	return Config{
		Steps:           detectSteps(worktreePath, opts, raceEnabled),
		GoRaceDetection: raceEnabled,
	}
}

// Run executes all verification steps in sequence.
// It stops on the first non-optional failure.
// db, beadID, and anvil are used to log lifecycle events; db may be nil to skip logging.
func Run(ctx context.Context, worktreePath string, cfg Config, db *state.DB, beadID, anvil string) *Result {
	result := &Result{}
	start := time.Now()

	if db != nil {
		_ = db.LogEvent(state.EventTemperStarted, fmt.Sprintf("Starting %d verification step(s) for %s", len(cfg.Steps), beadID), beadID, anvil)
	}

	for _, step := range cfg.Steps {
		stepResult := runStep(ctx, worktreePath, step)
		result.Steps = append(result.Steps, stepResult)

		if !stepResult.Passed && !step.Optional {
			result.FailedStep = step.Name
			break
		}
	}

	result.Duration = time.Since(start)
	result.Passed = result.FailedStep == ""
	result.Summary = buildSummary(result)

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

// runStep executes a single verification step.
func runStep(ctx context.Context, worktreePath string, step Step) StepResult {
	timeout := step.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
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
	cmd.Dir = dir

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	err := cmd.Run()
	duration := time.Since(start)

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

	return StepResult{
		Name:     step.Name,
		Command:  fmt.Sprintf("%s %s", step.Command, strings.Join(step.Args, " ")),
		ExitCode: exitCode,
		Output:   output.String(),
		Duration: duration,
		Passed:   passed,
		Optional: step.Optional,
	}
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
		})
		steps = append(steps, Step{
			Name:    "vet",
			Command: "go",
			Args:    []string{"vet", "./..."},
			Timeout: 2 * time.Minute,
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
				})
			}
		}

		steps = append(steps, Step{
			Name:    "test",
			Command: "go",
			Args:    []string{"test", "-short", "./..."},
			Timeout: 5 * time.Minute,
		})
		if goRace {
			steps = append(steps, Step{
				Name:    "race",
				Command: "go",
				Args:    []string{"test", "-race", "-short", "./..."},
				Timeout: 10 * time.Minute,
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
		})
		steps = append(steps, Step{
			Name:    "test",
			Command: "dotnet",
			Args:    []string{"test", "--no-build"},
			Timeout: 5 * time.Minute,
		})
	}

	// Check for Node.js project
	if fileExists(worktreePath, "package.json") {
		steps = append(steps, Step{
			Name:    "lint",
			Command: "npm",
			Args:    []string{"run", "lint"},
			Timeout: 2 * time.Minute,
			Optional: true, // lint might not be configured
		})
		steps = append(steps, Step{
			Name:    "test",
			Command: "npm",
			Args:    []string{"run", "test:run"},
			Timeout: 5 * time.Minute,
			Optional: true, // test script might not exist
		})
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
