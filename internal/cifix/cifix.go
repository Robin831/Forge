// Package cifix spawns a Smith worker to fix CI failures on a PR branch.
//
// When Bellows detects CI failures, cifix checks out the existing PR branch,
// runs Temper to reproduce the failure, then spawns Smith with a targeted
// fix prompt. It retries up to a configurable number of attempts.
package cifix

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
)

// MaxAttempts is the maximum number of CI fix attempts per PR.
const MaxAttempts = 2

// FixParams holds the inputs for a CI fix attempt.
type FixParams struct {
	// WorktreePath is the git worktree for this PR's branch.
	WorktreePath string
	// BeadID for tracking.
	BeadID string
	// AnvilName for tracking.
	AnvilName string
	// AnvilPath to the repo root.
	AnvilPath string
	// PRNumber being fixed.
	PRNumber int
	// Branch name for the PR.
	Branch string
	// DB for state tracking.
	DB *state.DB
	// WorkerID is the state DB worker ID, used to update the log path
	// so the Hearth TUI can display live activity.
	WorkerID string
	// ExtraFlags for Claude CLI.
	ExtraFlags []string
	// TemperConfig overrides auto-detection if set.
	TemperConfig *temper.Config
	// Providers is the ordered list of AI providers to try.
	// If empty, provider.Defaults() is used (Claude → Gemini).
	Providers []provider.Provider
}

// FixResult captures the outcome of a CI fix attempt.
type FixResult struct {
	// Fixed is true if the CI issues were resolved.
	Fixed bool
	// Attempts is how many fix cycles were tried.
	Attempts int
	// LastTemperResult is from the last verification.
	LastTemperResult *temper.Result
	// Duration is the total time spent.
	Duration time.Duration
	// Error if the fix process itself failed.
	Error error
}

// Fix attempts to resolve CI failures on a PR branch.
func Fix(ctx context.Context, p FixParams) *FixResult {
	start := time.Now()
	result := &FixResult{}

	// Resolve providers — default to Claude → Gemini if not specified.
	providers := p.Providers
	if len(providers) == 0 {
		providers = provider.Defaults()
	}
	activeProviderIdx := 0

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		result.Attempts = attempt
		log.Printf("[cifix] PR #%d attempt %d/%d", p.PRNumber, attempt, MaxAttempts)

		// Step 1: Run Temper to reproduce failures
		temperCfg := p.TemperConfig
		if temperCfg == nil {
			detected := temper.DefaultConfig(p.WorktreePath, nil)
			temperCfg = &detected
		}

		temperResult := temper.Run(ctx, p.WorktreePath, *temperCfg, p.DB, p.BeadID, p.AnvilName)
		result.LastTemperResult = temperResult

		if temperResult.Passed {
			log.Printf("[cifix] PR #%d: Temper passes — CI may be fixed already", p.PRNumber)
			result.Fixed = true
			result.Duration = time.Since(start)
			return result
		}

		// Step 2: Build fix prompt from Temper failures + GitHub checks
		ghChecks, err := fetchPRChecks(ctx, p.WorktreePath, p.PRNumber)
		if err != nil {
			log.Printf("[cifix] Warning: failed to fetch GitHub PR checks: %v", err)
		}
		prompt := buildCIFixPrompt(p, temperResult, ghChecks)

		// Step 3: Spawn Smith
		_ = p.DB.LogEvent(state.EventCIFixStarted,
			fmt.Sprintf("PR #%d: attempt %d, failed step: %s", p.PRNumber, attempt, temperResult.FailedStep),
			p.BeadID, p.AnvilName)

		logDir := p.WorktreePath + "/.forge-logs"
		var smithResult *smith.Result
		for pi := activeProviderIdx; pi < len(providers); pi++ {
			pv := providers[pi]
			if pi > activeProviderIdx {
				log.Printf("[cifix] PR #%d: Provider %s rate limited, retrying with %s",
					p.PRNumber, providers[pi-1].Kind, pv.Kind)
			}
			process, err := smith.SpawnWithProvider(ctx, p.WorktreePath, prompt, logDir, pv, p.ExtraFlags)
			if err != nil {
				result.Error = fmt.Errorf("spawning smith (%s) for CI fix: %w", pv.Kind, err)
				result.Duration = time.Since(start)
				return result
			}
			if p.WorkerID != "" && p.DB != nil {
				if err := p.DB.UpdateWorkerLogPath(p.WorkerID, process.LogPath); err != nil {
					log.Printf("[cifix] PR #%d: failed to update worker log path for worker %s (log path: %s): %v",
						p.PRNumber, p.WorkerID, process.LogPath, err)
				}
			}
			smithResult = process.Wait()
			// Treat a genuine success event as not rate-limited.
			// Do NOT use ExitCode == 0 here: Claude can exit 0 with is_error:true
			// (subtype:"success") when the session was rate-limit rejected — that
			// is not a successful session and must not suppress the fallback.
			if smithResult.ResultSubtype == "success" && !smithResult.IsError {
				smithResult.RateLimited = false
			}
			if !smithResult.RateLimited {
				activeProviderIdx = pi
				break
			}
		}

		if smithResult.RateLimited {
			log.Printf("[cifix] PR #%d: All providers rate limited on attempt %d", p.PRNumber, attempt)
			_ = p.DB.LogEvent(state.EventCIFixFailed,
				fmt.Sprintf("PR #%d attempt %d: all providers rate limited", p.PRNumber, attempt),
				p.BeadID, p.AnvilName)
			result.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
			result.Duration = time.Since(start)
			return result
		}

		if smithResult.ExitCode != 0 {
			log.Printf("[cifix] PR #%d: Smith fix attempt %d failed (exit %d)", p.PRNumber, attempt, smithResult.ExitCode)
			_ = p.DB.LogEvent(state.EventCIFixFailed,
				fmt.Sprintf("PR #%d: Smith exit %d on attempt %d", p.PRNumber, smithResult.ExitCode, attempt),
				p.BeadID, p.AnvilName)
			continue
		}

		// Step 4: Verify the fix
		verifyResult := temper.Run(ctx, p.WorktreePath, *temperCfg, p.DB, p.BeadID, p.AnvilName)
		result.LastTemperResult = verifyResult

		if verifyResult.Passed {
			log.Printf("[cifix] PR #%d: Fixed on attempt %d", p.PRNumber, attempt)
			result.Fixed = true
			_ = p.DB.LogEvent(state.EventCIFixSuccess,
				fmt.Sprintf("PR #%d: Fixed on attempt %d", p.PRNumber, attempt),
				p.BeadID, p.AnvilName)
			result.Duration = time.Since(start)
			return result
		}

		log.Printf("[cifix] PR #%d: Temper still failing after attempt %d", p.PRNumber, attempt)
	}

	result.Error = fmt.Errorf("could not fix CI after %d attempts", MaxAttempts)
	_ = p.DB.LogEvent(state.EventCIFixFailed,
		fmt.Sprintf("PR #%d: Exhausted %d fix attempts", p.PRNumber, MaxAttempts),
		p.BeadID, p.AnvilName)
	result.Duration = time.Since(start)
	return result
}

// buildCIFixPrompt creates a targeted prompt for Smith to fix CI failures.
func buildCIFixPrompt(p FixParams, tr *temper.Result, ghChecks string) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are fixing CI failures on PR #%d (branch: %s) for bead %s.

## GitHub PR Checks Output

%s

## Local Temper Reproduction Summary

%s

## Failed Local Steps

`, p.PRNumber, p.Branch, p.BeadID, ghChecks, tr.Summary)

	for _, step := range tr.Steps {
		if !step.Passed {
			fmt.Fprintf(&b, "### %s (FAILED)\n", step.Name)
			fmt.Fprintf(&b, "Command: %s\n", step.Command)
			if step.Output != "" {
				// Truncate long output
				output := step.Output
				if len(output) > 4000 {
					output = output[len(output)-4000:]
					output = "... (truncated)\n" + output
				}
				fmt.Fprintf(&b, "Output:\n```\n%s\n```\n\n", output)
			}
		}
	}

	fmt.Fprintf(&b, `## Instructions

1. Analyze the CI failure output above (both GitHub and local)
2. Fix the root cause — do NOT just suppress warnings or skip tests
3. Ensure all build, lint, and test steps pass
4. Commit fixes with message: "fix: resolve CI failures for %s"
5. Push to branch: %s

## Working Directory

%s
`, p.BeadID, p.Branch, p.WorktreePath)

	return b.String()
}

func fetchPRChecks(ctx context.Context, worktreePath string, prNumber int) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "checks", fmt.Sprintf("%d", prNumber))
	executil.HideWindow(cmd)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
