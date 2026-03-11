// Package cifix spawns a Smith worker to fix CI failures on a PR branch.
//
// When Bellows detects CI failures, cifix checks out the existing PR branch,
// fetches failing check details from GitHub, runs Temper to reproduce local
// failures, then spawns Smith with a targeted fix prompt. For GitHub-only
// checks that Temper cannot reproduce (e.g. changelog-check), Smith receives
// the failing check names and CI logs directly.
package cifix

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
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
	// DetectOptions controls optional steps during Temper auto-detection.
	// When TemperConfig is nil, these options are forwarded to
	// temper.DefaultConfig so that per-anvil settings (e.g. DisableGolangciLint)
	// are respected by the cifix worker.
	DetectOptions *temper.DetectOptions
	// GoRaceDetection enables a separate 'go test -race' step in Temper.
	// Only used during auto-detection (when TemperConfig is nil).
	GoRaceDetection bool
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

// checkResult represents a parsed GitHub check from `gh pr checks` output.
type checkResult struct {
	Name       string
	Status     string // "pass", "fail", "pending", etc.
	Link       string
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

		// Step 1: Fetch GitHub check status to identify what's actually failing.
		ghChecksOutput, fetchErr := fetchPRChecks(ctx, p.WorktreePath, p.PRNumber)
		ghChecksFetched := fetchErr == nil
		if fetchErr != nil {
			log.Printf("[cifix] Warning: failed to fetch GitHub PR checks: %v", fetchErr)
		}
		failingChecks := parseFailingChecks(ghChecksOutput)
		if len(failingChecks) > 0 {
			names := make([]string, len(failingChecks))
			for i, c := range failingChecks {
				names[i] = c.Name
			}
			log.Printf("[cifix] PR #%d: failing GitHub checks: %s", p.PRNumber, strings.Join(names, ", "))
		}

		// Step 2: Run Temper to reproduce failures locally.
		temperCfg := p.TemperConfig
		if temperCfg == nil {
			detected := temper.DefaultConfigWithRace(p.WorktreePath, p.DetectOptions, p.GoRaceDetection)
			temperCfg = &detected
		}

		temperResult := temper.Run(ctx, p.WorktreePath, *temperCfg, p.DB, p.BeadID, p.AnvilName)
		result.LastTemperResult = temperResult

		if temperResult.Passed {
			if !ghChecksFetched {
				// GitHub check status is unknown — do not treat as fixed; spawn Smith to be safe.
				log.Printf("[cifix] PR #%d: Temper passes but GitHub check status unknown (fetch failed) — spawning Smith to verify",
					p.PRNumber)
			} else if len(failingChecks) == 0 {
				// Temper passes AND GitHub confirms no checks are failing — truly fixed.
				log.Printf("[cifix] PR #%d: Temper passes and no GitHub checks failing — CI is fixed", p.PRNumber)
				result.Fixed = true
				result.Duration = time.Since(start)
				return result
			} else {
				// Temper passes but GitHub has failing checks that Temper doesn't cover.
				// Don't return early — spawn Smith to fix the GitHub-only failures.
				log.Printf("[cifix] PR #%d: Temper passes locally but %d GitHub check(s) still failing — spawning Smith for GitHub-only fixes",
					p.PRNumber, len(failingChecks))
			}
		}

		// Step 3: Fetch CI logs for failing checks to give Smith better context.
		ciLogs := fetchFailingCheckLogs(ctx, p.WorktreePath, failingChecks)

		// Step 4: Build fix prompt.
		var prompt string
		if temperResult.Passed {
			// GitHub-only failures — build a targeted prompt without temper output.
			prompt = buildGitHubOnlyFixPrompt(p, failingChecks, ghChecksOutput, ciLogs)
		} else {
			// Local failures (possibly combined with GitHub-only failures).
			prompt = buildCIFixPrompt(p, temperResult, ghChecksOutput, failingChecks, ciLogs)
		}

		failedStep := temperResult.FailedStep
		if failedStep == "" && len(failingChecks) > 0 {
			failedStep = "github:" + failingChecks[0].Name
		}
		_ = p.DB.LogEvent(state.EventCIFixStarted,
			fmt.Sprintf("PR #%d: attempt %d, failed step: %s", p.PRNumber, attempt, failedStep),
			p.BeadID, p.AnvilName)

		// Snapshot HEAD so we can compute the fix diff after Smith completes.
		baseCommit, revParseErr := gitRevParse(ctx, p.WorktreePath, "HEAD")
		if revParseErr != nil {
			log.Printf("[cifix] PR #%d: failed to snapshot HEAD for warden learning: %v", p.PRNumber, revParseErr)
		}

		// Step 5: Spawn Smith.
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
			// Persist quota for every attempt (including rate-limited ones) so the
			// dashboard does not undercount in the all-providers-rate-limited case.
			if smithResult.Quota != nil && p.DB != nil {
				if err := p.DB.UpsertProviderQuota(string(pv.Kind), smithResult.Quota); err != nil {
					log.Printf("[cifix] PR #%d: Failed to update provider %s quota in DB: %v", p.PRNumber, pv.Label(), err)
				}
			}
			if pv.Kind == provider.Copilot && !smithResult.RateLimited {
				if m := cost.CopilotPremiumMultiplier(pv.Model); m > 0 && p.DB != nil {
					_ = p.DB.AddCopilotRequest(cost.Today(), m)
				}
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

		// Step 6: Verify the fix.
		verifyResult := temper.Run(ctx, p.WorktreePath, *temperCfg, p.DB, p.BeadID, p.AnvilName)
		result.LastTemperResult = verifyResult

		if verifyResult.Passed {
			log.Printf("[cifix] PR #%d: Fixed on attempt %d (local verification passed)", p.PRNumber, attempt)
			result.Fixed = true
			_ = p.DB.LogEvent(state.EventCIFixSuccess,
				fmt.Sprintf("PR #%d: Fixed on attempt %d", p.PRNumber, attempt),
				p.BeadID, p.AnvilName)
			result.Duration = time.Since(start)

			// Learn from this CI fix: compute diff synchronously, then learn asynchronously
			// so the success path returns without waiting for Claude distillation.
			if baseCommit != "" {
				fixDiff, diffErr := gitDiff(ctx, p.WorktreePath, baseCommit, "HEAD")
				if diffErr != nil {
					log.Printf("[cifix] PR #%d: failed to compute fix diff for warden learning: %v", p.PRNumber, diffErr)
				} else {
					prNum := p.PRNumber
					anvilPath, worktreePath := p.AnvilPath, p.WorktreePath
					logsCopy := make(map[string]string, len(ciLogs))
					for k, v := range ciLogs {
						logsCopy[k] = v
					}
					go func() {
						learnCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancel()
						if err := warden.LearnFromCIFix(learnCtx, anvilPath, worktreePath, logsCopy, fixDiff, prNum); err != nil {
							log.Printf("[cifix] PR #%d: warden learn from CI fix: %v", prNum, err)
						}
					}()
				}
			}

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

// buildCIFixPrompt creates a targeted prompt for Smith to fix CI failures
// that include both local temper failures and (optionally) GitHub-only failures.
func buildCIFixPrompt(p FixParams, tr *temper.Result, ghChecks string, failingChecks []checkResult, ciLogs map[string]string) string {
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
				output := truncateOutput(step.Output, 4000)
				fmt.Fprintf(&b, "Output:\n```\n%s\n```\n\n", output)
			}
		}
	}

	// List failing GitHub checks (may overlap with local failures or be GitHub-only).
	if len(failingChecks) > 0 {
		fmt.Fprintf(&b, "## Failing GitHub Checks\n\n")
		for _, check := range failingChecks {
			fmt.Fprintf(&b, "- **%s** (status: %s)\n", check.Name, check.Status)
		}
		b.WriteString("\n")
	}

	// Include GitHub CI logs for failing checks that temper doesn't cover.
	if len(ciLogs) > 0 {
		fmt.Fprintf(&b, "## GitHub CI Logs for Failing Checks\n\n")
		checkNames := make([]string, 0, len(ciLogs))
		for name := range ciLogs {
			checkNames = append(checkNames, name)
		}
		sort.Strings(checkNames)
		for _, checkName := range checkNames {
			fmt.Fprintf(&b, "### %s\n", checkName)
			fmt.Fprintf(&b, "```\n%s\n```\n\n", truncateOutput(ciLogs[checkName], 4000))
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

// buildGitHubOnlyFixPrompt creates a prompt for failures that only exist on
// GitHub CI and cannot be reproduced by Temper locally (e.g. changelog-check).
func buildGitHubOnlyFixPrompt(p FixParams, failingChecks []checkResult, ghChecks string, ciLogs map[string]string) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are fixing CI failures on PR #%d (branch: %s) for bead %s.

**IMPORTANT**: These failures are GitHub-only CI checks that cannot be reproduced locally.
Local build, lint, and test all pass. The failing checks are only enforced by GitHub CI.

## GitHub PR Checks Output

%s

## Failing GitHub Checks

`, p.PRNumber, p.Branch, p.BeadID, ghChecks)

	for _, check := range failingChecks {
		fmt.Fprintf(&b, "- **%s** (status: %s)\n", check.Name, check.Status)
	}
	b.WriteString("\n")

	// Include CI logs for each failing check (sorted for stable output).
	if len(ciLogs) > 0 {
		fmt.Fprintf(&b, "## GitHub CI Logs\n\n")
		checkNames := make([]string, 0, len(ciLogs))
		for name := range ciLogs {
			checkNames = append(checkNames, name)
		}
		sort.Strings(checkNames)
		for _, checkName := range checkNames {
			fmt.Fprintf(&b, "### %s\n", checkName)
			fmt.Fprintf(&b, "```\n%s\n```\n\n", truncateOutput(ciLogs[checkName], 4000))
		}
	}

	// Check-specific guidance.
	for _, check := range failingChecks {
		lowerName := strings.ToLower(check.Name)
		if strings.Contains(lowerName, "changelog") {
			fmt.Fprintf(&b, `## Changelog Check Guidance

The **%s** check is failing because a changelog fragment is missing.
Create a changelog fragment file at 'changelog.d/<bead-id>.md' with this format:

`+"```"+`markdown
category: Fixed
- **Short bold summary** - Detail about the fix. (%s)
`+"```"+`

Use the bead ID '%s' in the filename: 'changelog.d/%s.md'
Look at existing files in changelog.d/ for examples of the expected format.

`, check.Name, p.BeadID, p.BeadID, p.BeadID)
		}
	}

	fmt.Fprintf(&b, `## Instructions

1. Analyze the failing GitHub CI checks listed above
2. Fix the root cause for each failing check
3. Do NOT modify build, lint, or test code — those all pass locally
4. Commit fixes with message: "fix: resolve CI failures for %s"
5. Push to branch: %s

## Working Directory

%s
`, p.BeadID, p.Branch, p.WorktreePath)

	return b.String()
}

// parseFailingChecks parses the output of `gh pr checks` to identify failing checks.
// The output format is tab-separated: NAME\tSTATUS\tDURATION\tLINK
func parseFailingChecks(ghChecksOutput string) []checkResult {
	if ghChecksOutput == "" {
		return nil
	}
	var failing []checkResult
	for _, line := range strings.Split(ghChecksOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		status := strings.TrimSpace(fields[1])
		var link string
		if len(fields) >= 4 {
			link = strings.TrimSpace(fields[3])
		}
		if status == "fail" {
			failing = append(failing, checkResult{Name: name, Status: status, Link: link})
		}
	}
	return failing
}

// runIDRegexp extracts a GitHub Actions run ID from a check run URL.
// URLs look like: https://github.com/owner/repo/actions/runs/12345/jobs/67890
var runIDRegexp = regexp.MustCompile(`/actions/runs/(\d+)`)

// fetchFailingCheckLogs fetches CI logs for failing checks from GitHub Actions.
// Returns a map of check name → log output.
func fetchFailingCheckLogs(ctx context.Context, worktreePath string, checks []checkResult) map[string]string {
	if len(checks) == 0 {
		return nil
	}

	// Collect unique run IDs from check URLs.
	seenRuns := make(map[string]bool)
	runLogs := make(map[string]string) // runID → log output
	for _, check := range checks {
		matches := runIDRegexp.FindStringSubmatch(check.Link)
		if len(matches) < 2 {
			continue
		}
		runID := matches[1]
		if seenRuns[runID] {
			continue
		}
		seenRuns[runID] = true

		logs, err := fetchRunFailedLogs(ctx, worktreePath, runID)
		if err != nil {
			log.Printf("[cifix] Warning: failed to fetch logs for run %s: %v", runID, err)
			continue
		}
		runLogs[runID] = logs
	}

	// Map check names to their run's logs.
	result := make(map[string]string)
	for _, check := range checks {
		matches := runIDRegexp.FindStringSubmatch(check.Link)
		if len(matches) < 2 {
			continue
		}
		if logs, ok := runLogs[matches[1]]; ok && logs != "" {
			result[check.Name] = logs
		}
	}
	return result
}

// fetchRunFailedLogs fetches the failed job logs for a GitHub Actions run.
func fetchRunFailedLogs(ctx context.Context, worktreePath, runID string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "run", "view", runID, "--log-failed")
	executil.HideWindow(cmd)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return "", fmt.Errorf("gh run view --log-failed: %w: %s", err, trimmed)
		}
		return "", fmt.Errorf("gh run view --log-failed: %w", err)
	}
	return string(out), nil
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

// truncateOutput returns the last maxLen characters of output, prepending
// a truncation marker if it was shortened.
func truncateOutput(output string, maxLen int) string {
	if len(output) <= maxLen {
		return output
	}
	return "... (truncated)\n" + output[len(output)-maxLen:]
}

// gitRevParse resolves a git ref (e.g. "HEAD") to its commit hash.
func gitRevParse(ctx context.Context, dir, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", ref)
	executil.HideWindow(cmd)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %s (%w)", ref, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitDiff returns the diff between two commits in the given directory.
func gitDiff(ctx context.Context, dir, from, to string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", from+".."+to)
	executil.HideWindow(cmd)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff %s..%s: %s (%w)", from, to, strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}
