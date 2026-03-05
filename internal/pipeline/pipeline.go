// Package pipeline orchestrates the full Smith → Temper → Warden → feedback loop.
//
// The pipeline runs a bead through:
//  1. Smith implementation
//  2. Temper build/test verification
//  3. Warden code review
//  4. If request_changes: re-run Smith with feedback, repeat (up to max iterations)
//  5. Final verdict → done or failed
package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
)

// MaxIterations is the maximum number of Smith-Warden cycles.
const MaxIterations = 3

// Outcome represents the final result of the pipeline.
type Outcome struct {
	// Success is true if the bead was implemented and approved.
	Success bool
	// Verdict is the final Warden verdict.
	Verdict warden.Verdict
	// Iterations is how many Smith-Warden cycles were run.
	Iterations int
	// SmithResult is the last Smith result.
	SmithResult *smith.Result
	// TemperResult is the last Temper result.
	TemperResult *temper.Result
	// ReviewResult is the last Warden review result.
	ReviewResult *warden.ReviewResult
	// Duration is the total pipeline duration.
	Duration time.Duration
	// WorkerID is the worker ID used.
	WorkerID string
	// Branch is the git branch used.
	Branch string
	// Error is set if the pipeline failed before reaching a verdict.
	Error error
}

// Params holds the dependencies for running a pipeline.
type Params struct {
	DB              *state.DB
	WorktreeManager *worktree.Manager
	PromptBuilder   *prompt.Builder
	AnvilName       string
	AnvilConfig     config.AnvilConfig
	Bead            poller.Bead
	ExtraFlags      []string
	TemperConfig    *temper.Config // nil = auto-detect
	// Providers is the ordered list of AI providers to try.
	// If empty, provider.Defaults() is used (Claude → Gemini).
	Providers       []provider.Provider
}

// Run executes the full Smith → Temper → Warden pipeline for a bead.
func Run(ctx context.Context, p Params) *Outcome {
	start := time.Now()
	outcome := &Outcome{}
	workerID := fmt.Sprintf("%s-%s-%d", p.AnvilName, p.Bead.ID, time.Now().Unix())
	outcome.WorkerID = workerID

	// Step 1: Create worktree
	log.Printf("[pipeline:%s] Creating worktree for bead %s", workerID, p.Bead.ID)
	wt, err := p.WorktreeManager.Create(ctx, p.AnvilConfig.Path, p.Bead.ID)
	if err != nil {
		outcome.Error = fmt.Errorf("creating worktree: %w", err)
		outcome.Duration = time.Since(start)
		return outcome
	}
	outcome.Branch = wt.Branch
	defer func() {
		log.Printf("[pipeline:%s] Cleaning up worktree", workerID)
		_ = p.WorktreeManager.Remove(ctx, p.AnvilConfig.Path, wt)
	}()

	// Record worker in state DB
	dbWorker := &state.Worker{
		ID:        workerID,
		BeadID:    p.Bead.ID,
		Anvil:     p.AnvilName,
		Branch:    wt.Branch,
		Status:    state.WorkerRunning,
		StartedAt: time.Now(),
	}
	_ = p.DB.InsertWorker(dbWorker)
	_ = p.DB.LogEvent(state.EventBeadClaimed, fmt.Sprintf("Pipeline started for %s", p.Bead.ID), p.Bead.ID, p.AnvilName)

	// Build initial prompt
	beadCtx := prompt.BeadContext{
		BeadID:       p.Bead.ID,
		Title:        p.Bead.Title,
		Description:  p.Bead.Description,
		IssueType:    p.Bead.IssueType,
		Priority:     p.Bead.Priority,
		Parent:       p.Bead.Parent,
		Branch:       wt.Branch,
		AnvilName:    p.AnvilName,
		AnvilPath:    p.AnvilConfig.Path,
		WorktreePath: wt.Path,
	}

	customTmpl := prompt.LoadCustomTemplate(p.AnvilConfig.Path)
	if customTmpl != "" {
		p.PromptBuilder.CustomTemplate = customTmpl
	}

	promptText, err := p.PromptBuilder.Build(beadCtx)
	if err != nil {
		outcome.Error = fmt.Errorf("building prompt: %w", err)
		outcome.Duration = time.Since(start)
		_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
		return outcome
	}

	// Resolve provider list.
	providers := p.Providers
	if len(providers) == 0 {
		providers = provider.Defaults()
	}
	activeProviderIdx := 0

	// Feedback loop
	var currentPrompt = promptText

	for iteration := 1; iteration <= MaxIterations; iteration++ {
		outcome.Iterations = iteration
		log.Printf("[pipeline:%s] Iteration %d/%d", workerID, iteration, MaxIterations)

		// Step 2: Run Smith (with provider fallback on rate limit)
		log.Printf("[pipeline:%s] Running Smith (provider: %s)", workerID, providers[activeProviderIdx].Kind)
		_ = p.DB.LogEvent(state.EventSmithStarted, fmt.Sprintf("Iteration %d (provider: %s)", iteration, providers[activeProviderIdx].Kind), p.Bead.ID, p.AnvilName)

		logDir := wt.Path + "/.forge-logs"

		var smithResult *smith.Result
		for pi := activeProviderIdx; pi < len(providers); pi++ {
			pv := providers[pi]
			if pi > activeProviderIdx {
				log.Printf("[pipeline:%s] Provider %s rate limited, retrying with %s", workerID, providers[pi-1].Kind, pv.Kind)
				_ = p.DB.LogEvent(state.EventSmithStarted,
					fmt.Sprintf("Rate limit fallback to provider %s (iteration %d)", pv.Kind, iteration),
					p.Bead.ID, p.AnvilName)
			}
			process, err := smith.SpawnWithProvider(ctx, wt.Path, currentPrompt, logDir, pv, p.ExtraFlags)
			if err != nil {
				outcome.Error = fmt.Errorf("spawning smith (%s): %w", pv.Kind, err)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
				_ = p.DB.LogEvent(state.EventSmithFailed, err.Error(), p.Bead.ID, p.AnvilName)
				outcome.Duration = time.Since(start)
				return outcome
			}
			_ = p.DB.UpdateWorkerPID(workerID, process.PID)
			smithResult = process.Wait()

			// A process that exits 0, or produces a genuine success result event
			// (subtype:"success" with is_error:false), completed successfully. The
			// Claude CLI handles internal retries for rate limits and resumes
			// automatically. Any rate_limit_event or non-zero exit code we saw was
			// either a warning or a transient block that resolved.
			// IMPORTANT: subtype:"success" + is_error:true is a hard rate-limit
			// rejection — Claude returns this when it couldn't start the session.
			// Do NOT clear RateLimited in that case; fall back to another provider.
			if smithResult.ExitCode == 0 || (smithResult.ResultSubtype == "success" && !smithResult.IsError) {
				smithResult.RateLimited = false
			}

			if smithResult.Quota != nil {
				if err := p.DB.UpsertProviderQuota(string(pv.Kind), smithResult.Quota); err != nil {
					log.Printf("[pipeline:%s] Failed to update provider %s quota in DB: %v", workerID, pv.Kind, err)
				} else {
					resetStr := "n/a"
					if smithResult.Quota.RequestsReset != nil {
						resetStr = time.Until(*smithResult.Quota.RequestsReset).Round(time.Minute).String()
					}
					log.Printf("[pipeline:%s] Provider %s quota updated: %d/%d requests, %d/%d tokens remaining (reset in %s)",
						workerID, pv.Kind,
						smithResult.Quota.RequestsRemaining, smithResult.Quota.RequestsLimit,
						smithResult.Quota.TokensRemaining, smithResult.Quota.TokensLimit, resetStr)
				}
			}

			if !smithResult.RateLimited {
				activeProviderIdx = pi // remember for the next iteration
				break
			}
		}
		outcome.SmithResult = smithResult

		if smithResult.RateLimited {
			log.Printf("[pipeline:%s] All providers rate limited", workerID)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				"All providers rate limited — cannot continue",
				p.Bead.ID, p.AnvilName)
			outcome.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			outcome.Duration = time.Since(start)
			return outcome
		}

		if smithResult.ExitCode != 0 {
			log.Printf("[pipeline:%s] Smith failed exit=%d stderr=%s", workerID, smithResult.ExitCode, smithResult.ErrorOutput)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				fmt.Sprintf("Exit code %d after %.1fs: %s", smithResult.ExitCode, smithResult.Duration.Seconds(), smithResult.ErrorOutput),
				p.Bead.ID, p.AnvilName)
			outcome.Error = fmt.Errorf("smith exit code %d", smithResult.ExitCode)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			outcome.Duration = time.Since(start)
			return outcome
		}

		_ = p.DB.LogEvent(state.EventSmithDone,
			fmt.Sprintf("Completed in %.1fs ($%.4f)", smithResult.Duration.Seconds(), smithResult.CostUSD),
			p.Bead.ID, p.AnvilName)

		if s := smithResult.GeminiStats; s != nil {
			_ = p.DB.LogEvent(state.EventSmithStats,
				fmt.Sprintf("tokens_in=%d tokens_out=%d total=%d cached=%d input=%d tool_calls=%d duration_ms=%d",
					s.InputTokens, s.OutputTokens, s.TotalTokens, s.Cached, s.Input, s.ToolCalls, s.DurationMs),
				p.Bead.ID, p.AnvilName)
		}

		// Step 3: Run Temper (build/test)
		log.Printf("[pipeline:%s] Running Temper verification", workerID)
		temperCfg := p.TemperConfig
		if temperCfg == nil {
			detected := temper.DefaultConfig(wt.Path)
			temperCfg = &detected
		}

		temperResult := temper.Run(ctx, wt.Path, *temperCfg, p.DB, p.Bead.ID, p.AnvilName)
		outcome.TemperResult = temperResult

		if !temperResult.Passed {
			log.Printf("[pipeline:%s] Temper failed at step: %s", workerID, temperResult.FailedStep)

			if iteration < MaxIterations {
				// Build feedback prompt for Smith to fix build/test failures
				currentPrompt = buildFixPrompt(beadCtx, "build/test", temperResult.Summary, nil)
				continue
			}

			// Final iteration and still failing
			outcome.Error = fmt.Errorf("temper verification failed after %d iterations: %s", iteration, temperResult.FailedStep)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			outcome.Duration = time.Since(start)
			return outcome
		}

		// Step 4: Run Warden review
		log.Printf("[pipeline:%s] Running Warden review", workerID)
		_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerReviewing)

		reviewResult, err := warden.Review(ctx, wt.Path, p.Bead.ID, p.AnvilConfig.Path, p.DB, p.AnvilName, providers...)
		if err != nil {
			log.Printf("[pipeline:%s] Warden error: %v", workerID, err)
			// Warden failure is not fatal — default to approve and let human review
			outcome.ReviewResult = &warden.ReviewResult{
				Verdict: warden.VerdictApprove,
				Summary: "Warden failed, defaulting to approve for human review",
			}
			outcome.Verdict = warden.VerdictApprove
			outcome.Success = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
			_ = p.DB.LogEvent(state.EventWardenPass, "Warden failed, defaulting to approve", p.Bead.ID, p.AnvilName)
			outcome.Duration = time.Since(start)
			return outcome
		}

		outcome.ReviewResult = reviewResult

		switch reviewResult.Verdict {
		case warden.VerdictApprove:
			log.Printf("[pipeline:%s] Warden approved", workerID)
			outcome.Verdict = warden.VerdictApprove
			outcome.Success = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
			_ = p.DB.LogEvent(state.EventWardenPass, reviewResult.Summary, p.Bead.ID, p.AnvilName)
			outcome.Duration = time.Since(start)
			return outcome

		case warden.VerdictReject:
			log.Printf("[pipeline:%s] Warden rejected", workerID)
			outcome.Verdict = warden.VerdictReject
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			_ = p.DB.LogEvent(state.EventWardenReject, reviewResult.Summary, p.Bead.ID, p.AnvilName)
			outcome.Duration = time.Since(start)
			return outcome

		case warden.VerdictRequestChanges:
			log.Printf("[pipeline:%s] Warden requests changes (iteration %d)", workerID, iteration)
			_ = p.DB.LogEvent(state.EventWardenReject,
				fmt.Sprintf("Request changes (iteration %d): %s", iteration, reviewResult.Summary),
				p.Bead.ID, p.AnvilName)

			if iteration < MaxIterations {
				// Build feedback prompt with review issues
				currentPrompt = buildFixPrompt(beadCtx, "review", reviewResult.Summary, reviewResult.Issues)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerRunning)
				continue
			}

			// Max iterations reached with request_changes
			outcome.Verdict = warden.VerdictRequestChanges
			outcome.Error = fmt.Errorf("warden still requesting changes after %d iterations", iteration)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			outcome.Duration = time.Since(start)
			return outcome
		}
	}

	outcome.Duration = time.Since(start)
	return outcome
}

// buildFixPrompt creates a prompt for Smith to fix issues found by Temper or Warden.
func buildFixPrompt(beadCtx prompt.BeadContext, source, summary string, issues []warden.ReviewIssue) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are continuing work on bead %s in the %s repository.

## Previous Attempt

Your previous implementation had issues identified by the %s:

%s

`, beadCtx.BeadID, beadCtx.AnvilName, source, summary)

	if len(issues) > 0 {
		b.WriteString("## Specific Issues to Fix\n\n")
		for i, issue := range issues {
			fmt.Fprintf(&b, "%d. **[%s]** %s", i+1, issue.Severity, issue.Message)
			if issue.File != "" {
				fmt.Fprintf(&b, " (in `%s`", issue.File)
				if issue.Line > 0 {
					fmt.Fprintf(&b, " line %d", issue.Line)
				}
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, `## Instructions

1. Fix ALL the issues listed above
2. Make sure the project builds and tests pass
3. Commit your fixes with a clear message
4. Push to the branch: %s

## Original Task

**Bead**: %s
**Title**: %s

%s

## Working Directory

You are working in: %s
Branch: %s
`,
		beadCtx.Branch,
		beadCtx.BeadID, beadCtx.Title, beadCtx.Description,
		beadCtx.WorktreePath, beadCtx.Branch)

	return b.String()
}
