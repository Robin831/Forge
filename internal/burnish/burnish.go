// Package burnish spawns a Smith worker to address PR review comments.
//
// When Bellows detects "changes requested" on a PR, burnish fetches the
// review comments via the VCS provider, constructs a targeted fix prompt,
// and spawns Smith to address them. It then pushes the fixes to the PR branch.
package burnish

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/hooks"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vcs"
)

// smithExitTimeout is the deadline given to a Smith subprocess to exit after
// its I/O stream has been fully read. If Smith does not exit within this
// window (e.g. due to slow cleanup on Windows), the process is killed so the
// burnish worker can advance to thread resolution and completion.
const smithExitTimeout = 30 * time.Second

// DefaultVerifyTimeout is the default deadline for the post-Smith verification
// (temper) step in a single burnish attempt. If temper does not return within
// this window the burnish worker logs WARN, marks the worker failed with a
// stable error string, and returns so the daemon can re-dispatch via its
// normal recovery path. Mirrored by config.SettingsConfig.BurnishVerifyTimeout
// for runtime override.
//
// To reproduce the timeout in development, override the temper test step with
// a long-running command on the anvil's forge.yaml (e.g. `temper.steps: - {
// name: test, command: sleep, args: [999] }`) and trigger a burnish round.
// The burnish worker should emit "verification starting" then "WARN
// verification timeout after 5m0s (reason=warden_timeout)" in journalctl.
const DefaultVerifyTimeout = 5 * time.Minute

// ErrVerifyTimeoutReason is the stable error/event string emitted when burnish
// gives up waiting on verification. The bead retry path and operator dashboards
// match on this exact string, so do not change without coordinating.
const ErrVerifyTimeoutReason = "warden_timeout"

// FixParams holds the inputs for a review fix attempt.
type FixParams struct {
	// WorktreePath is the git worktree for this PR's branch.
	WorktreePath string
	// BeadID for tracking.
	BeadID string
	// AnvilName for tracking.
	AnvilName string
	// AnvilPath is the repo root.
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
	// MaxAttempts is the maximum fix attempts for review comments.
	MaxAttempts int
	// ExtraFlags for Claude CLI.
	ExtraFlags []string
	// Providers is the ordered list of AI providers to try.
	// If empty, provider.Defaults() is used (Claude → Gemini).
	Providers []provider.Provider
	// VCS is the VCS provider for repository operations. When set,
	// it is used for GetRepoOwnerAndName instead of creating a throwaway instance.
	VCS vcs.Provider
	// TemperConfig is the per-anvil temper configuration. Nil means auto-detect.
	TemperConfig *temper.Config
	// DetectOptions controls temper auto-detection behavior (e.g. golangci-lint).
	DetectOptions *temper.DetectOptions
	// GoRaceDetection enables Go race detection in temper steps.
	GoRaceDetection bool
	// Hooks is the per-anvil hook configuration. When set, before_temper and
	// after_temper hooks fire around every temper invocation.
	Hooks *config.HooksConfig
	// VerifyTimeout caps the post-Smith verification (temper) step per attempt.
	// When <= 0 the package default (DefaultVerifyTimeout) is applied.
	VerifyTimeout time.Duration
}

// FixResult captures the outcome of addressing review comments.
type FixResult struct {
	// Addressed is true if Smith successfully pushed review fixes.
	Addressed bool
	// Attempts is how many fix cycles were tried.
	Attempts int
	// CommentsFound is how many review comments were fetched.
	CommentsFound int
	// Duration is the total time spent.
	Duration time.Duration
	// Error if the fix process itself failed.
	Error error
}

// BatchFixParams holds the inputs for a batched review fix attempt.
type BatchFixParams struct {
	// WorktreePath is the git worktree for this PR's branch.
	WorktreePath string
	// BeadID for tracking.
	BeadID string
	// AnvilName for tracking.
	AnvilName string
	// AnvilPath is the repo root (used to populate FORGE_ANVIL_PATH in hook env).
	AnvilPath string
	// PRNumber being fixed.
	PRNumber int
	// Branch name for the PR.
	Branch string
	// DB for state tracking.
	DB *state.DB
	// WorkerID is the state DB worker ID.
	WorkerID string
	// ExtraFlags for Claude CLI.
	ExtraFlags []string
	// Providers is the ordered list of AI providers to try.
	Providers []provider.Provider
	// Comments is the list of review comments to address.
	Comments []vcs.ReviewComment
	// VCS is the VCS provider for thread resolution.
	VCS vcs.Provider
	// TemperConfig is the per-anvil temper configuration. Nil means auto-detect.
	TemperConfig *temper.Config
	// DetectOptions controls temper auto-detection behavior (e.g. golangci-lint).
	DetectOptions *temper.DetectOptions
	// GoRaceDetection enables Go race detection in temper steps.
	GoRaceDetection bool
	// Hooks is the per-anvil hook configuration for temper hooks.
	Hooks *config.HooksConfig
	// VerifyTimeout caps the post-Smith verification (temper) step.
	// When <= 0 the package default (DefaultVerifyTimeout) is applied.
	VerifyTimeout time.Duration
}

// BatchFix combines multiple review comments into one Smith prompt.
// Use this when copilot_batch_review_fixes is enabled and the provider is Copilot.
func BatchFix(ctx context.Context, p BatchFixParams) *FixResult {
	start := time.Now()
	result := &FixResult{}

	result.CommentsFound = len(p.Comments)
	actionable := filterActionableComments(p.Comments)

	if len(actionable) == 0 {
		result.Addressed = true
		result.Duration = time.Since(start)
		return result
	}

	providers := p.Providers

	prompt := buildBatchReviewPrompt(p.PRNumber, p.Branch, p.BeadID, actionable)

	if p.DB != nil {
		_ = p.DB.LogEvent(state.EventBurnishStarted,
			fmt.Sprintf("PR #%d: batch fix for %d comments", p.PRNumber, len(actionable)),
			p.BeadID, p.AnvilName)
	}

	result.Attempts = 1

	logDir := p.WorktreePath + "/.forge-logs"
	var smithResult *smith.Result
	for pi := 0; pi < len(providers); pi++ {
		pv := providers[pi]
		if pi > 0 {
			log.Printf("[burnish] PR #%d: Provider %s rate limited, retrying with %s",
				p.PRNumber, providers[pi-1].Label(), pv.Label())
		}
		process, err := smithSpawnFn(ctx, p.WorktreePath, prompt, logDir, pv, p.ExtraFlags)
		if err != nil {
			result.Error = fmt.Errorf("spawning smith (%s) for batch review fix: %w", pv.Label(), err)
			result.Duration = time.Since(start)
			return result
		}
		if p.WorkerID != "" && p.DB != nil {
			if err := p.DB.UpdateWorkerLogPath(p.WorkerID, process.LogPath); err != nil {
				log.Printf("[burnish] PR #%d: failed to update worker log path: %v", p.PRNumber, err)
			}
		}
		smithResult = process.WaitWithExitTimeout(smithExitTimeout)
		if p.WorkerID != "" && p.DB != nil {
			_ = p.DB.UpdateWorkerSession(p.WorkerID, smithResult.SessionID, smith.SessionModel(smithResult, pv))
		}
		if smithResult.ResultSubtype == "success" && !smithResult.IsError {
			smithResult.RateLimited = false
		}
		if smithResult.Quota != nil && p.DB != nil {
			if err := p.DB.UpsertProviderQuota(string(pv.Kind), smithResult.Quota); err != nil {
				log.Printf("[burnish] PR #%d: Failed to update provider %s quota: %v", p.PRNumber, pv.Label(), err)
			}
		}
		if pv.Kind == provider.Copilot && !smithResult.RateLimited {
			if m := cost.CopilotPremiumMultiplier(pv.Model); m > 0 && p.DB != nil {
				_ = p.DB.AddCopilotRequest(cost.Today(), m)
			}
		}
		if !smithResult.RateLimited {
			break
		}
	}

	if smithResult == nil {
		result.Error = fmt.Errorf("batch review fix: no smith result (no providers available)")
		if p.DB != nil {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d batch fix: no smith result", p.PRNumber),
				p.BeadID, p.AnvilName)
		}
		result.Duration = time.Since(start)
		return result
	}

	if smithResult.RateLimited {
		result.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
		if p.DB != nil {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d batch fix: all providers rate limited", p.PRNumber),
				p.BeadID, p.AnvilName)
		}
		result.Duration = time.Since(start)
		return result
	}

	log.Printf("[burnish] PR #%d bead=%s: smith exit observed (exit=%d, subtype=%s)",
		p.PRNumber, p.BeadID, smithResult.ExitCode, smithResult.ResultSubtype)

	if smithResult.ExitCode != 0 {
		result.Error = fmt.Errorf("batch review fix failed (exit %d)", smithResult.ExitCode)
		if p.DB != nil {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d: batch Smith exit %d", p.PRNumber, smithResult.ExitCode),
				p.BeadID, p.AnvilName)
		}
		result.Duration = time.Since(start)
		return result
	}

	// Verify locally before pushing.
	hEnv := hooks.HookEnv{
		BeadID:       p.BeadID,
		WorktreePath: p.WorktreePath,
		Branch:       p.Branch,
		AnvilName:    p.AnvilName,
		AnvilPath:    p.AnvilPath,
		Stage:        "temper",
		Iteration:    1,
	}
	if err := hookRunFn(ctx, p.WorkerID, "before_temper", hooks.HookCmd(p.Hooks, "before_temper"), hEnv); err != nil {
		result.Error = fmt.Errorf("before_temper hook: %w", err)
		result.Duration = time.Since(start)
		return result
	}
	verifyTimeout := resolveVerifyTimeout(p.VerifyTimeout)
	log.Printf("[burnish] PR #%d bead=%s: verification starting (timeout=%s)",
		p.PRNumber, p.BeadID, verifyTimeout)
	temperCfg := resolveTemperConfig(p.WorktreePath, p.TemperConfig, p.DetectOptions, p.GoRaceDetection)
	verifyOutc := runVerifyWithTimeout(ctx, p.PRNumber, p.BeadID, p.AnvilName, p.WorktreePath, temperCfg, p.DB, verifyTimeout)
	if err := hookRunFn(ctx, p.WorkerID, "after_temper", hooks.HookCmd(p.Hooks, "after_temper"), hEnv); err != nil {
		log.Printf("[burnish] PR #%d: after_temper hook failed (non-fatal): %v", p.PRNumber, err)
	}
	if verifyOutc.timedOut {
		if p.DB != nil {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d: batch verification timed out after %s (%s)", p.PRNumber, verifyTimeout, ErrVerifyTimeoutReason),
				p.BeadID, p.AnvilName)
			if p.WorkerID != "" {
				_ = p.DB.UpdateWorkerStatus(p.WorkerID, state.WorkerFailed)
			}
		}
		result.Error = fmt.Errorf("batch review fix: verification timed out after %s (%s)", verifyTimeout, ErrVerifyTimeoutReason)
		result.Duration = time.Since(start)
		return result
	}
	verifyResult := verifyOutc.result
	log.Printf("[burnish] PR #%d bead=%s: verification result (passed=%t, failedStep=%s)",
		p.PRNumber, p.BeadID, verifyResult.Passed, verifyResult.FailedStep)
	if !verifyResult.Passed {
		log.Printf("[burnish] PR #%d: batch Temper failed (failed step: %s) — not pushing",
			p.PRNumber, verifyResult.FailedStep)
		if p.DB != nil {
			_ = p.DB.LogEvent(state.EventBurnishTemperFailed,
				fmt.Sprintf("PR #%d: batch temper failed at step %s", p.PRNumber, verifyResult.FailedStep),
				p.BeadID, p.AnvilName)
		}
		result.Error = fmt.Errorf("batch review fix: temper verification failed at step %s", verifyResult.FailedStep)
		result.Duration = time.Since(start)
		return result
	}

	// Temper passed — push the verified commits (unless Smith already pushed).
	localHead, _ := gitRevParseFn(ctx, p.WorktreePath, "HEAD")
	remoteHead, _ := gitRevParseFn(ctx, p.WorktreePath, "origin/"+p.Branch)
	if localHead == "" || localHead != remoteHead {
		log.Printf("[burnish] PR #%d bead=%s: push attempt starting (branch=%s)", p.PRNumber, p.BeadID, p.Branch)
		if err := gitPushFn(ctx, p.WorktreePath, p.Branch); err != nil {
			log.Printf("[burnish] PR #%d bead=%s: WARN push failed after temper-verified batch fix: %v", p.PRNumber, p.BeadID, err)
			if p.DB != nil {
				_ = p.DB.LogEvent(state.EventBurnishFailed,
					fmt.Sprintf("PR #%d: push failed after temper-verified batch fix: %v", p.PRNumber, err),
					p.BeadID, p.AnvilName)
			}
			result.Error = fmt.Errorf("push after temper verification: %w", err)
			result.Duration = time.Since(start)
			return result
		}
		log.Printf("[burnish] PR #%d bead=%s: push complete", p.PRNumber, p.BeadID)
	} else {
		log.Printf("[burnish] PR #%d bead=%s: Smith already pushed (HEAD matches origin/%s), skipping explicit push", p.PRNumber, p.BeadID, p.Branch)
	}

	log.Printf("[burnish] PR #%d: batch review fix verified and pushed for %d comments", p.PRNumber, len(actionable))
	result.Addressed = true

	// Resolve threads after successful fix.
	resolvedCount := 0
	resolvableCount := 0
	if p.VCS != nil {
		log.Printf("[burnish] PR #%d bead=%s: thread resolution starting", p.PRNumber, p.BeadID)
		for _, comment := range actionable {
			if comment.ThreadID == "" {
				continue
			}
			resolvableCount++
			if err := p.VCS.ResolveThread(ctx, p.WorktreePath, comment.ThreadID); err != nil {
				log.Printf("[burnish] PR #%d: Warning: failed to resolve thread %s: %v", p.PRNumber, comment.ThreadID, err)
			} else {
				resolvedCount++
			}
		}
		if resolvedCount > 0 {
			log.Printf("[burnish] PR #%d: Resolved %d/%d threads on GitHub", p.PRNumber, resolvedCount, resolvableCount)
		}
		log.Printf("[burnish] PR #%d bead=%s: thread resolution complete (resolved=%d/%d)",
			p.PRNumber, p.BeadID, resolvedCount, resolvableCount)
	}

	if p.DB != nil {
		_ = p.DB.LogEvent(state.EventBurnishSuccess,
			fmt.Sprintf("PR #%d: batch addressed %d comments", p.PRNumber, len(actionable)),
			p.BeadID, p.AnvilName)
	}

	result.Duration = time.Since(start)
	return result
}

// buildBatchReviewPrompt creates a prompt listing all review comments for Smith to address in one pass.
func buildBatchReviewPrompt(prNumber int, branch, beadID string, comments []vcs.ReviewComment) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are addressing multiple review comments on PR #%d (branch: %s) for bead %s.

## Review Comments to Address

`, prNumber, branch, beadID)

	for i, c := range comments {
		fmt.Fprintf(&b, "### %d.", i+1)
		if c.Author != "" {
			fmt.Fprintf(&b, " (by @%s)", c.Author)
		}
		b.WriteString("\n")

		if c.Path != "" {
			fmt.Fprintf(&b, "**File**: %s", c.Path)
			if c.Line > 0 {
				fmt.Fprintf(&b, " line %d", c.Line)
			}
			b.WriteString("\n")
		}

		fmt.Fprintf(&b, "\n%s\n\n", c.Body)
	}

	fmt.Fprintf(&b, `## Instructions

1. Address ALL %d review comments above in a single pass
2. Make the requested changes — follow each reviewer's guidance
3. Comments may come from more than one reviewer (e.g. GitHub Copilot and
   the Assay AI reviewer) and some may describe the SAME underlying issue in
   different words or at adjacent lines. Make ONE fix per distinct issue —
   do not apply the same change twice, and reconcile any that conflict.
4. **Run the test suite** and fix any failures before continuing
5. Commit with message: "fix: address review comments for %s"
6. Do NOT push — Forge will run verification and push for you. Exit cleanly after committing.

`, len(comments), beadID)

	return b.String()
}

// Fix fetches review comments and spawns Smith to address them.
func Fix(ctx context.Context, p FixParams) *FixResult {
	start := time.Now()
	result := &FixResult{}

	// Validate MaxAttempts to avoid silently skipping all attempts when unset or invalid.
	if p.MaxAttempts <= 0 {
		log.Printf("[burnish] PR #%d: MaxAttempts=%d is not positive; defaulting to 1 attempt", p.PRNumber, p.MaxAttempts)
		p.MaxAttempts = 1
	}

	// Ensure a VCS provider is available.
	if p.VCS == nil {
		result.Error = fmt.Errorf("VCS provider is required but was not set")
		result.Duration = time.Since(start)
		return result
	}

	// Step 1: Fetch review comments via VCS provider
	comments, err := p.VCS.FetchReviewComments(ctx, p.WorktreePath, p.PRNumber)
	if err != nil {
		result.Error = fmt.Errorf("fetching review comments: %w", err)
		result.Duration = time.Since(start)
		return result
	}

	result.CommentsFound = len(comments)
	if len(comments) == 0 {
		log.Printf("[burnish] PR #%d: No review comments found", p.PRNumber)
		result.Addressed = true
		result.Duration = time.Since(start)
		return result
	}

	// Filter to unresolved/actionable comments
	actionable := filterActionableComments(comments)
	if len(actionable) == 0 {
		log.Printf("[burnish] PR #%d: No actionable comments", p.PRNumber)
		result.Addressed = true
		result.Duration = time.Since(start)
		return result
	}

	log.Printf("[burnish] PR #%d: %d actionable review comments", p.PRNumber, len(actionable))

	// Resolve providers — default to Claude → Gemini if not specified.
	providers := p.Providers
	if len(providers) == 0 {
		providers = provider.Defaults()
	}
	activeProviderIdx := 0

	var lastTemperFailure *temper.Result

	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		result.Attempts = attempt

		// Step 2: Build fix prompt, including previous temper failure if any.
		prompt := buildReviewFixPrompt(p, actionable)
		if lastTemperFailure != nil {
			prompt = formatTemperFailureForPrompt(lastTemperFailure) + prompt
		}

		// Step 3: Spawn Smith (with provider fallback on rate limit)
		_ = p.DB.LogEvent(state.EventBurnishStarted,
			fmt.Sprintf("PR #%d: attempt %d, %d comments (provider: %s)", p.PRNumber, attempt, len(actionable), providers[activeProviderIdx].Label()),
			p.BeadID, p.AnvilName)
		log.Printf("[burnish] PR #%d bead=%s: spawning smith (attempt=%d, provider=%s)",
			p.PRNumber, p.BeadID, attempt, providers[activeProviderIdx].Label())

		logDir := p.WorktreePath + "/.forge-logs"
		var smithResult *smith.Result
		for pi := activeProviderIdx; pi < len(providers); pi++ {
			pv := providers[pi]
			if pi > activeProviderIdx {
				log.Printf("[burnish] PR #%d: Provider %s rate limited, retrying with %s",
					p.PRNumber, providers[pi-1].Label(), pv.Label())
				_ = p.DB.LogEvent(state.EventBurnishSmithError,
					fmt.Sprintf("PR #%d attempt %d: %s rate limited, falling back to %s",
						p.PRNumber, attempt, providers[pi-1].Label(), pv.Label()),
					p.BeadID, p.AnvilName)
			}
			process, err := smithSpawnFn(ctx, p.WorktreePath, prompt, logDir, pv, p.ExtraFlags)
			if err != nil {
				result.Error = fmt.Errorf("spawning smith (%s) for review fix: %w", pv.Label(), err)
				_ = p.DB.LogEvent(state.EventBurnishFailed, result.Error.Error(), p.BeadID, p.AnvilName)
				result.Duration = time.Since(start)
				return result
			}
			if p.WorkerID != "" && p.DB != nil {
				if err := p.DB.UpdateWorkerLogPath(p.WorkerID, process.LogPath); err != nil {
					log.Printf("[burnish] PR #%d: failed to update worker log path for worker %s to %q: %v",
						p.PRNumber, p.WorkerID, process.LogPath, err)
				}
			}
			smithResult = process.WaitWithExitTimeout(smithExitTimeout)
			if p.WorkerID != "" && p.DB != nil {
				_ = p.DB.UpdateWorkerSession(p.WorkerID, smithResult.SessionID, smith.SessionModel(smithResult, pv))
			}
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
					log.Printf("[burnish] PR #%d: Failed to update provider %s quota in DB: %v", p.PRNumber, pv.Label(), err)
				}
			}
			if pv.Kind == provider.Copilot && !smithResult.RateLimited {
				if m := cost.CopilotPremiumMultiplier(pv.Model); m > 0 {
					_ = p.DB.AddCopilotRequest(cost.Today(), m)
				}
			}
			if !smithResult.RateLimited {
				activeProviderIdx = pi
				break
			}
		}

		log.Printf("[burnish] PR #%d bead=%s: smith exit observed (attempt=%d, exit=%d, subtype=%s)",
			p.PRNumber, p.BeadID, attempt, smithResult.ExitCode, smithResult.ResultSubtype)

		// If all providers are rate-limited, abort rather than burning more attempts.
		if smithResult.RateLimited {
			log.Printf("[burnish] PR #%d: All providers rate limited on attempt %d", p.PRNumber, attempt)
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d attempt %d: all providers rate limited", p.PRNumber, attempt),
				p.BeadID, p.AnvilName)
			_ = p.DB.LogEvent(state.EventBurnishSmithError,
				fmt.Sprintf("PR #%d attempt %d: rate_limited (all %d providers exhausted)", p.PRNumber, attempt, len(providers)),
				p.BeadID, p.AnvilName)
			result.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
			result.Duration = time.Since(start)
			return result
		}

		if smithResult.ExitCode != 0 {
			log.Printf("[burnish] PR #%d: Smith fix attempt %d failed (exit %d, subtype=%s)",
				p.PRNumber, attempt, smithResult.ExitCode, smithResult.ResultSubtype)
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d: Smith exit %d on attempt %d", p.PRNumber, smithResult.ExitCode, attempt),
				p.BeadID, p.AnvilName)
			// Log error detail for root-cause debugging.
			errDetail := smithResult.ResultSubtype
			if errDetail == "" && smithResult.ErrorOutput != "" {
				errDetail = smithResult.ErrorOutput
				if len(errDetail) > 300 {
					errDetail = errDetail[:300] + "..."
				}
			}
			if errDetail != "" {
				_ = p.DB.LogEvent(state.EventBurnishSmithError,
					fmt.Sprintf("PR #%d attempt %d: %s", p.PRNumber, attempt, errDetail),
					p.BeadID, p.AnvilName)
			}
			continue
		}

		// Smith succeeded — verify locally before pushing.
		hEnv := hooks.HookEnv{
			BeadID:       p.BeadID,
			WorktreePath: p.WorktreePath,
			Branch:       p.Branch,
			AnvilName:    p.AnvilName,
			AnvilPath:    p.AnvilPath,
			Stage:        "temper",
			Iteration:    attempt,
		}
		if err := hookRunFn(ctx, p.WorkerID, "before_temper", hooks.HookCmd(p.Hooks, "before_temper"), hEnv); err != nil {
			result.Error = fmt.Errorf("before_temper hook: %w", err)
			result.Duration = time.Since(start)
			return result
		}
		verifyTimeout := resolveVerifyTimeout(p.VerifyTimeout)
		log.Printf("[burnish] PR #%d bead=%s: verification starting (attempt=%d, timeout=%s)",
			p.PRNumber, p.BeadID, attempt, verifyTimeout)
		temperCfg := resolveTemperConfig(p.WorktreePath, p.TemperConfig, p.DetectOptions, p.GoRaceDetection)
		verifyOutc := runVerifyWithTimeout(ctx, p.PRNumber, p.BeadID, p.AnvilName, p.WorktreePath, temperCfg, p.DB, verifyTimeout)
		if err := hookRunFn(ctx, p.WorkerID, "after_temper", hooks.HookCmd(p.Hooks, "after_temper"), hEnv); err != nil {
			log.Printf("[burnish] PR #%d: after_temper hook failed (non-fatal): %v", p.PRNumber, err)
		}
		if verifyOutc.timedOut {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d: verification timed out after %s (%s) on attempt %d",
					p.PRNumber, verifyTimeout, ErrVerifyTimeoutReason, attempt),
				p.BeadID, p.AnvilName)
			if p.WorkerID != "" && p.DB != nil {
				_ = p.DB.UpdateWorkerStatus(p.WorkerID, state.WorkerFailed)
			}
			result.Error = fmt.Errorf("review fix: verification timed out after %s (%s)", verifyTimeout, ErrVerifyTimeoutReason)
			result.Duration = time.Since(start)
			return result
		}
		verifyResult := verifyOutc.result
		log.Printf("[burnish] PR #%d bead=%s: verification result (attempt=%d, passed=%t, failedStep=%s)",
			p.PRNumber, p.BeadID, attempt, verifyResult.Passed, verifyResult.FailedStep)

		// Defensive check: did Smith push despite being told not to?
		smithAlreadyPushed := false
		localHead, _ := gitRevParseFn(ctx, p.WorktreePath, "HEAD")
		remoteHead, _ := gitRevParseFn(ctx, p.WorktreePath, "origin/"+p.Branch)
		if localHead != "" && localHead == remoteHead {
			smithAlreadyPushed = true
			log.Printf("[burnish] PR #%d: Smith already pushed (HEAD matches origin/%s)", p.PRNumber, p.Branch)
		}

		if !verifyResult.Passed {
			log.Printf("[burnish] PR #%d: Temper failed on attempt %d (failed step: %s) — looping with feedback",
				p.PRNumber, attempt, verifyResult.FailedStep)
			_ = p.DB.LogEvent(state.EventBurnishTemperFailed,
				fmt.Sprintf("PR #%d: temper failed on attempt %d at step %s", p.PRNumber, attempt, verifyResult.FailedStep),
				p.BeadID, p.AnvilName)
			lastTemperFailure = verifyResult
			continue
		}

		// Temper passed — push the verified commits.
		if !smithAlreadyPushed {
			log.Printf("[burnish] PR #%d bead=%s: push attempt starting (attempt=%d, branch=%s)",
				p.PRNumber, p.BeadID, attempt, p.Branch)
			if err := gitPushFn(ctx, p.WorktreePath, p.Branch); err != nil {
				log.Printf("[burnish] PR #%d bead=%s: WARN push failed after temper-verified fix: %v", p.PRNumber, p.BeadID, err)
				_ = p.DB.LogEvent(state.EventBurnishFailed,
					fmt.Sprintf("PR #%d: push failed after temper-verified fix: %v", p.PRNumber, err),
					p.BeadID, p.AnvilName)
				result.Error = fmt.Errorf("push after temper verification: %w", err)
				result.Duration = time.Since(start)
				return result
			}
			log.Printf("[burnish] PR #%d bead=%s: push complete (attempt=%d)", p.PRNumber, p.BeadID, attempt)
		}

		log.Printf("[burnish] PR #%d: Review fixes verified and pushed on attempt %d", p.PRNumber, attempt)
		result.Addressed = true

		// Resolve threads after successful fix.
		// Only thread-level comments (with a ThreadID from GraphQL) can be resolved
		// via the API; review-level CHANGES_REQUESTED comments have no thread ID.
		log.Printf("[burnish] PR #%d bead=%s: thread resolution starting (count=%d)",
			p.PRNumber, p.BeadID, len(actionable))
		resolvedCount := 0
		for _, comment := range actionable {
			if comment.ThreadID == "" {
				continue
			}
			if err := p.VCS.ResolveThread(ctx, p.WorktreePath, comment.ThreadID); err != nil {
				log.Printf("[burnish] PR #%d: Warning: failed to resolve thread %s: %v", p.PRNumber, comment.ThreadID, err)
				_ = p.DB.LogEvent(state.EventBurnishFailed,
					fmt.Sprintf("PR #%d: resolve thread %s failed: %v", p.PRNumber, comment.ThreadID, err),
					p.BeadID, p.AnvilName)
			} else {
				resolvedCount++
				log.Printf("[burnish] PR #%d: Resolved thread %s (by @%s)", p.PRNumber, comment.ThreadID, comment.Author)
				// Log resolved thread to DB so it's visible in forge history.
				body := comment.Body
				if len(body) > 120 {
					body = body[:120] + "..."
				}
				_ = p.DB.LogEvent(state.EventReviewThreadResolved,
					fmt.Sprintf("PR #%d: resolved thread by @%s — %s", p.PRNumber, comment.Author, body),
					p.BeadID, p.AnvilName)
			}
		}
		if resolvedCount > 0 {
			log.Printf("[burnish] PR #%d: Resolved %d/%d threads on GitHub", p.PRNumber, resolvedCount, len(actionable))
		}
		log.Printf("[burnish] PR #%d bead=%s: thread resolution complete (resolved=%d/%d)",
			p.PRNumber, p.BeadID, resolvedCount, len(actionable))

		_ = p.DB.LogEvent(state.EventBurnishSuccess,
			fmt.Sprintf("PR #%d: Addressed %d comments on attempt %d", p.PRNumber, len(actionable), attempt),
			p.BeadID, p.AnvilName)

		result.Duration = time.Since(start)
		return result
	}

	result.Error = fmt.Errorf("could not address review comments after %d attempts", p.MaxAttempts)
	_ = p.DB.LogEvent(state.EventBurnishFailed,
		fmt.Sprintf("PR #%d: Exhausted %d fix attempts for %d comments", p.PRNumber, p.MaxAttempts, len(actionable)),
		p.BeadID, p.AnvilName)
	result.Duration = time.Since(start)
	return result
}

// filterActionableComments keeps only comments that need action.
func filterActionableComments(comments []vcs.ReviewComment) []vcs.ReviewComment {
	var actionable []vcs.ReviewComment
	for _, c := range comments {
		// Skip bot comments and approvals
		if c.State == "APPROVED" || c.State == "DISMISSED" {
			continue
		}
		if c.Body == "" {
			continue
		}
		actionable = append(actionable, c)
	}
	return actionable
}

// buildReviewFixPrompt creates a targeted prompt for Smith to address review comments.
func buildReviewFixPrompt(p FixParams, comments []vcs.ReviewComment) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are addressing review comments on PR #%d (branch: %s) for bead %s.

## Review Comments to Address

`, p.PRNumber, p.Branch, p.BeadID)

	for i, c := range comments {
		fmt.Fprintf(&b, "### Comment %d", i+1)
		if c.Author != "" {
			fmt.Fprintf(&b, " (by @%s)", c.Author)
		}
		b.WriteString("\n")

		if c.Path != "" {
			fmt.Fprintf(&b, "**File**: %s", c.Path)
			if c.Line > 0 {
				fmt.Fprintf(&b, " line %d", c.Line)
			}
			b.WriteString("\n")
		}

		fmt.Fprintf(&b, "\n%s\n\n", c.Body)
	}

	fmt.Fprintf(&b, `## Instructions

1. Address ALL review comments above
2. Make the requested changes — follow the reviewer's guidance
3. **Run the test suite** (e.g. "go test ./..." for Go, "dotnet test" for .NET, "npm test" or "npx vitest run" for Node/frontend) and fix any failures before continuing — do NOT commit if tests are failing
4. Commit with message: "fix: address review comments for %s"
5. Do NOT push — Forge will run verification and push for you. Exit cleanly after committing.

`, p.BeadID)

	return b.String()
}

// resolveVerifyTimeout returns the verification timeout to use, defaulting to
// DefaultVerifyTimeout when the caller did not configure one.
func resolveVerifyTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultVerifyTimeout
	}
	return d
}

// verifyOutcome carries the result of a temper run executed under a deadline.
type verifyOutcome struct {
	result   *temper.Result
	timedOut bool
}

// runVerifyWithTimeout runs temperRunFn under a deadline. When the deadline
// fires before temper returns, the wrapper logs a WARN transition, returns
// timedOut=true, and lets temper's own goroutine finish in the background
// (the cancel call signals temper to abort cleanly via its context). The
// verification context is derived from ctx so an outer cancellation also
// unblocks the call. The timer and the result channel are independent
// signals so we cannot get into the race where Go's `select` picks resCh
// over a deadline-fired ctx.Done().
func runVerifyWithTimeout(ctx context.Context, prNumber int, beadID, anvilName, worktreePath string,
	cfg temper.Config, db *state.DB, timeout time.Duration) verifyOutcome {
	verifyCtx, cancel := context.WithCancel(ctx)
	resCh := make(chan *temper.Result, 1)
	go func() {
		// temper observes verifyCtx cancellation and aborts running steps.
		resCh <- temperRunFn(verifyCtx, worktreePath, cfg, db, beadID, anvilName)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-resCh:
		cancel()
		return verifyOutcome{result: r}
	case <-timer.C:
		log.Printf("[burnish] PR #%d bead=%s: WARN verification timeout after %s (reason=%s)",
			prNumber, beadID, timeout, ErrVerifyTimeoutReason)
		cancel()
		return verifyOutcome{timedOut: true}
	case <-ctx.Done():
		// Outer cancellation: wait for temper to observe it and return so we
		// don't leak the goroutine after the daemon proceeds.
		cancel()
		r := <-resCh
		return verifyOutcome{result: r}
	}
}

// hookRunFn is the function used to run hooks. Package-level variable for test stubbing.
var hookRunFn = hooks.RunHook

// smithSpawnFn is the function used to spawn Smith. Package-level variable for test stubbing.
var smithSpawnFn = smith.SpawnWithProvider

// temperRunFn is the function used to run temper verification.
// It is a package-level variable so tests can substitute a stub.
var temperRunFn = temper.Run

// gitPushFn is the function used to push commits to the remote.
// It is a package-level variable so tests can substitute a stub.
var gitPushFn = gitPush

// gitRevParseFn is the function used to resolve git refs. Package-level for test stubbing.
var gitRevParseFn = gitRevParse

// gitPush pushes the current branch to origin with --force-with-lease.
//
// Burnish reuses a per-bead worktree across review-fix runs. If that worktree
// survived a prior run (a pod restart skipped the deferred worktree Remove, or
// Remove failed on a file lock) its local base can lag the remote tip — which is
// Forge's OWN earlier push for this branch — and a plain `git push` is then
// rejected non-fast-forward, losing the just-temper-verified fix. So fetch the
// branch first to refresh refs/remotes/origin/<branch>, then push with
// --force-with-lease: the lease verifies against the true remote tip (so a
// genuine concurrent third-party push is never clobbered) while still letting the
// newly verified commits replace Forge's prior attempt. Mirrors smelter's push
// path (internal/smelter/smelter.go).
func gitPush(ctx context.Context, worktreePath, branch string) error {
	// Refresh the remote-tracking ref so --force-with-lease leases against the
	// actual remote tip rather than a stale local ref.
	fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fetchCmd := exec.CommandContext(fetchCtx, "git", "-C", worktreePath, "fetch", "origin", branch)
	executil.HideWindow(fetchCmd)
	if fout, ferr := fetchCmd.CombinedOutput(); ferr != nil {
		if strings.Contains(string(fout), "couldn't find remote ref") {
			// Branch not on origin yet (first push) or auto-deleted after merge.
			// Clear any stale tracking ref so --force-with-lease doesn't reject
			// with "(stale info)".
			pruneCmd := exec.CommandContext(fetchCtx, "git", "-C", worktreePath, "update-ref", "-d", "refs/remotes/origin/"+branch)
			executil.HideWindow(pruneCmd)
			_ = pruneCmd.Run() // best effort
		} else {
			return fmt.Errorf("git fetch origin %s: %w (output: %s)", branch, ferr, strings.TrimSpace(string(fout)))
		}
	}

	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "push", "--force-with-lease", "origin", branch)
	executil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push --force-with-lease origin %s: %w (output: %s)", branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitRevParse resolves a git ref to its commit hash.
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

// resolveTemperConfig returns the temper config to use, falling back to auto-detection.
func resolveTemperConfig(worktreePath string, cfg *temper.Config, opts *temper.DetectOptions, raceEnabled bool) temper.Config {
	if cfg != nil {
		return *cfg
	}
	return temper.DefaultConfigWithRace(worktreePath, opts, raceEnabled)
}

// formatTemperFailureForPrompt renders a temper failure as a prompt section
// that can be prepended to the next Smith attempt.
func formatTemperFailureForPrompt(r *temper.Result) string {
	if r == nil || r.Passed {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Previous Verification Failure\n\n")
	b.WriteString("Your previous attempt addressed the review comments but Temper failed when verifying:\n\n")
	for _, step := range r.Steps {
		if step.Passed {
			continue
		}
		fmt.Fprintf(&b, "Failed step: %s\nCommand: %s\nOutput:\n```\n%s\n```\n\n",
			step.Name, step.Command, truncateOutput(step.Output, 4000))
		break // Only show the first failed step.
	}
	b.WriteString("You must address BOTH the original review comments AND fix this verification failure before committing.\n\n")
	return b.String()
}

// truncateOutput returns the last maxLen characters of output, prepending
// a truncation marker if it was shortened.
func truncateOutput(output string, maxLen int) string {
	if len(output) <= maxLen {
		return output
	}
	return "... (truncated)\n" + output[len(output)-maxLen:]
}
