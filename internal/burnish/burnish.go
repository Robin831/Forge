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

// Needs-attention message prefixes. They exist so a later cycle can tell which
// entries this package raised, and which of them a merged PR resolves.
const (
	// AttentionUnverified marks a fix that reached the PR without a passing
	// verification. The work landed, so a merge clears the entry.
	AttentionUnverified = "review fix unverified: "
	// AttentionUnpushed marks a fix commit that exists only in a preserved
	// worktree. A merge does NOT clear this one — the commit still never
	// reached the remote, and the checkout still holds the only copy.
	AttentionUnpushed = "review fix unpushed: "
)

// DefaultVerifyRetries is how many EXTRA verification runs a burnish attempt
// gets after the first one times out. A timeout is usually a wedged test
// process rather than a genuinely slow suite, so one clean re-run resolves most
// of them for the price of one more temper cycle. Mirrored by
// config.SettingsConfig.BurnishVerifyRetries.
const DefaultVerifyRetries = 1

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
	// VerifyRetries is how many extra verification runs a timed-out
	// verification gets before burnish falls back to pushing unverified.
	// When 0 the package default (DefaultVerifyRetries) is applied; a negative
	// value disables the retry.
	VerifyRetries int
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
	// Unverified is true when the pushed fix was NOT confirmed by temper
	// because verification exceeded its deadline. Addressed is still true — the
	// commit is on the PR — but the caller must report it as unverified rather
	// than as a clean success.
	Unverified bool
	// VerifyTimedOut is true when at least one verification run of the final
	// attempt hit its deadline, whatever burnish then did about it.
	VerifyTimedOut bool
	// HeadSHA is the worktree's HEAD after the last Smith attempt, when
	// burnish had reason to resolve it. Empty when it was never resolved.
	HeadSHA string
	// UnpushedHead is set when a finished fix commit could not be pushed. The
	// commit exists only in the worktree, so the caller must preserve that
	// worktree and surface the SHA rather than tearing it down.
	UnpushedHead string
	// RateLimited is true when every configured provider rejected the session
	// as rate limited, so no fix work even started. Error is set as well, but
	// callers must treat this dispatch as "never happened" — it says nothing
	// about the PR and must not consume any attempt budget.
	RateLimited bool
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
	// VerifyRetries is how many extra verification runs a timed-out
	// verification gets before burnish falls back to pushing unverified.
	// When 0 the package default (DefaultVerifyRetries) is applied; a negative
	// value disables the retry.
	VerifyRetries int
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
		result.RateLimited = true
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
	vp := verifyParams{
		prNumber:     p.PRNumber,
		beadID:       p.BeadID,
		anvilName:    p.AnvilName,
		branch:       p.Branch,
		worktreePath: p.WorktreePath,
		workerID:     p.WorkerID,
		db:           p.DB,
		timeout:      resolveVerifyTimeout(p.VerifyTimeout),
		retries:      resolveVerifyRetries(p.VerifyRetries),
	}
	log.Printf("[burnish] PR #%d bead=%s: verification starting (timeout=%s, retries=%d)",
		p.PRNumber, p.BeadID, vp.timeout, vp.retries)
	temperCfg := resolveTemperConfig(p.WorktreePath, p.TemperConfig, p.DetectOptions, p.GoRaceDetection)
	verifyOutc, verifyRuns := runVerifyRetrying(ctx, vp, temperCfg)
	if err := hookRunFn(ctx, p.WorkerID, "after_temper", hooks.HookCmd(p.Hooks, "after_temper"), hEnv); err != nil {
		log.Printf("[burnish] PR #%d: after_temper hook failed (non-fatal): %v", p.PRNumber, err)
	}
	if verifyOutc.timedOut {
		if p.DB != nil {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d: batch verification timed out after %s across %d run(s) (%s)",
					p.PRNumber, vp.timeout, verifyRuns, ErrVerifyTimeoutReason),
				p.BeadID, p.AnvilName)
		}
		// The timeout resolver pushes the fix unverified when it can, so a
		// still-unaddressed result means the commit is stranded and there is
		// nothing to resolve threads for.
		resolveVerifyTimeoutOutcome(ctx, vp, verifyRuns, result)
		if !result.Addressed {
			result.Duration = time.Since(start)
			return result
		}
	} else {
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
				// The fix is committed and exists nowhere else: name it so the
				// caller keeps the worktree instead of tearing it down.
				result.HeadSHA = localHead
				result.UnpushedHead = localHead
				preserveWork(vp, result, err.Error())
				result.Error = fmt.Errorf("push after temper verification: %w", err)
				result.Duration = time.Since(start)
				return result
			}
			log.Printf("[burnish] PR #%d bead=%s: push complete", p.PRNumber, p.BeadID)
		} else {
			log.Printf("[burnish] PR #%d bead=%s: Smith already pushed (HEAD matches origin/%s), skipping explicit push", p.PRNumber, p.BeadID, p.Branch)
		}

		log.Printf("[burnish] PR #%d: batch review fix verified and pushed for %d comments", p.PRNumber, len(actionable))
		result.HeadSHA = localHead
		result.Addressed = true
	}

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
			fmt.Sprintf("PR #%d: batch addressed %d comments%s", p.PRNumber, len(actionable), unverifiedSuffix(result)),
			p.BeadID, p.AnvilName)
	}

	result.Duration = time.Since(start)
	return result
}

// unverifiedSuffix annotates a success message when the fix reached the PR
// without a passing verification, so the event log never reads as a clean
// success for a run that never completed one.
func unverifiedSuffix(result *FixResult) string {
	if result.Unverified {
		return " (UNVERIFIED — verification timed out)"
	}
	return ""
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
			result.RateLimited = true
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
		vp := verifyParams{
			prNumber:     p.PRNumber,
			beadID:       p.BeadID,
			anvilName:    p.AnvilName,
			branch:       p.Branch,
			worktreePath: p.WorktreePath,
			workerID:     p.WorkerID,
			db:           p.DB,
			timeout:      resolveVerifyTimeout(p.VerifyTimeout),
			retries:      resolveVerifyRetries(p.VerifyRetries),
		}
		log.Printf("[burnish] PR #%d bead=%s: verification starting (attempt=%d, timeout=%s, retries=%d)",
			p.PRNumber, p.BeadID, attempt, vp.timeout, vp.retries)
		temperCfg := resolveTemperConfig(p.WorktreePath, p.TemperConfig, p.DetectOptions, p.GoRaceDetection)
		verifyOutc, verifyRuns := runVerifyRetrying(ctx, vp, temperCfg)
		if err := hookRunFn(ctx, p.WorkerID, "after_temper", hooks.HookCmd(p.Hooks, "after_temper"), hEnv); err != nil {
			log.Printf("[burnish] PR #%d: after_temper hook failed (non-fatal): %v", p.PRNumber, err)
		}
		if verifyOutc.timedOut {
			_ = p.DB.LogEvent(state.EventBurnishFailed,
				fmt.Sprintf("PR #%d: verification timed out after %s across %d run(s) (%s) on attempt %d",
					p.PRNumber, vp.timeout, verifyRuns, ErrVerifyTimeoutReason, attempt),
				p.BeadID, p.AnvilName)
			// Never loop back to Smith here: a timeout says nothing about the
			// diff, so another attempt would rebuild identical work. The
			// resolver pushes the commit unverified when it can, and preserves
			// it when it cannot — either way this attempt is the last one.
			resolveVerifyTimeoutOutcome(ctx, vp, verifyRuns, result)
			if !result.Addressed {
				result.Duration = time.Since(start)
				return result
			}
			log.Printf("[burnish] PR #%d: Review fixes pushed UNVERIFIED on attempt %d", p.PRNumber, attempt)
		} else {
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
					// A verified fix that could not be pushed exists only here.
					result.HeadSHA = localHead
					result.UnpushedHead = localHead
					preserveWork(vp, result, err.Error())
					result.Error = fmt.Errorf("push after temper verification: %w", err)
					result.Duration = time.Since(start)
					return result
				}
				log.Printf("[burnish] PR #%d bead=%s: push complete (attempt=%d)", p.PRNumber, p.BeadID, attempt)
			}

			log.Printf("[burnish] PR #%d: Review fixes verified and pushed on attempt %d", p.PRNumber, attempt)
			result.HeadSHA = localHead
			result.Addressed = true
		}

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
			fmt.Sprintf("PR #%d: Addressed %d comments on attempt %d%s",
				p.PRNumber, len(actionable), attempt, unverifiedSuffix(result)),
			p.BeadID, p.AnvilName)

		result.Duration = time.Since(start)
		return result
	}

	// Every attempt was spent without a push. Whatever the last Smith committed
	// exists only in the worktree, so name it rather than letting teardown take
	// it: the caller preserves the checkout when UnpushedHead is set.
	if head, err := gitRevParseFn(ctx, p.WorktreePath, "HEAD"); err == nil && head != "" {
		if remote, _ := gitRevParseFn(ctx, p.WorktreePath, "origin/"+p.Branch); remote != head {
			result.HeadSHA = head
			result.UnpushedHead = head
			preserveWork(verifyParams{
				prNumber:     p.PRNumber,
				beadID:       p.BeadID,
				anvilName:    p.AnvilName,
				branch:       p.Branch,
				worktreePath: p.WorktreePath,
				db:           p.DB,
			}, result, fmt.Sprintf("exhausted %d fix attempts without a verified push", p.MaxAttempts))
		}
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

// resolveVerifyRetries returns how many extra verification runs a timed-out
// verification gets. Zero means "unset" and falls back to the package default,
// mirroring resolveVerifyTimeout; a negative value is the explicit opt-out for
// an anvil whose suite is slow enough that a second full run is not worth the
// wall clock.
func resolveVerifyRetries(n int) int {
	switch {
	case n == 0:
		return DefaultVerifyRetries
	case n < 0:
		return 0
	default:
		return n
	}
}

// verifyParams is the slice of a fix attempt that verification needs. Fix and
// BatchFix carry different parameter structs but must resolve a timeout
// identically, so both project onto this.
type verifyParams struct {
	prNumber     int
	beadID       string
	anvilName    string
	branch       string
	worktreePath string
	workerID     string
	db           *state.DB
	timeout      time.Duration
	retries      int
}

// runVerifyRetrying runs verification, re-running it after a timeout up to
// vp.retries extra times. It returns the outcome of the last run and how many
// runs it took.
//
// A verification timeout is nearly always a wedged test process rather than a
// suite that genuinely needs longer, so one clean re-run resolves most of them
// — and doing so before falling back to an unverified push means the fallback
// stays rare enough to be worth escalating when it happens.
func runVerifyRetrying(ctx context.Context, vp verifyParams, cfg temper.Config) (verifyOutcome, int) {
	runs := 0
	var outc verifyOutcome
	for attempt := 0; attempt <= vp.retries; attempt++ {
		if attempt > 0 {
			log.Printf("[burnish] PR #%d bead=%s: re-running verification after timeout (retry %d/%d)",
				vp.prNumber, vp.beadID, attempt, vp.retries)
			if vp.db != nil {
				_ = vp.db.LogEvent(state.EventBurnishFailed,
					fmt.Sprintf("PR #%d: verification timed out (%s), retrying (%d/%d)",
						vp.prNumber, ErrVerifyTimeoutReason, attempt, vp.retries),
					vp.beadID, vp.anvilName)
			}
		}
		outc = runVerifyWithTimeout(ctx, vp.prNumber, vp.beadID, vp.anvilName, vp.worktreePath, cfg, vp.db, vp.timeout)
		runs++
		if !outc.timedOut {
			return outc, runs
		}
		// An outer cancellation will never produce a different answer, and
		// retrying under a dead context just burns the remaining budget.
		if ctx.Err() != nil {
			return outc, runs
		}
	}
	return outc, runs
}

// resolveVerifyTimeoutOutcome decides what happens to a finished fix commit
// that verification could not confirm.
//
// The old behaviour was the worst of every option: log a WARN, skip the push,
// delete the worktree, and report the cycle complete. The commit — a correct,
// finished fix — survived only as an unreferenced object, the operator saw a
// success table, and the next Bellows cycle re-detected the unchanged
// changes-requested review and spent another full Smith run rebuilding exactly
// the same diff (Forge-xl50).
//
// Burnish verification is advisory, unlike Temper in the pipeline: every
// burnish output lands on an open PR where humans, Copilot and Assay review the
// new head anyway. So the fix is pushed and marked unverified. That both keeps
// the work and moves the PR head, which is what actually stops the loop — the
// next cycle reviews something new instead of re-deriving the same diff.
//
// If the push itself fails there is nothing left to do but keep the commit:
// UnpushedHead is set so the caller preserves the worktree instead of deleting
// it, and the SHA is named in the escalation.
func resolveVerifyTimeoutOutcome(ctx context.Context, vp verifyParams, runs int, result *FixResult) {
	result.VerifyTimedOut = true

	localHead, headErr := gitRevParseFn(ctx, vp.worktreePath, "HEAD")
	result.HeadSHA = localHead
	remoteHead, _ := gitRevParseFn(ctx, vp.worktreePath, "origin/"+vp.branch)

	timeoutDetail := fmt.Sprintf("verification timed out after %s across %d run(s) (%s)",
		vp.timeout, runs, ErrVerifyTimeoutReason)

	// Already on the remote (Smith pushed despite being told not to, or made no
	// commit at all): nothing is at risk, but the outcome is still unverified.
	if localHead != "" && localHead == remoteHead {
		log.Printf("[burnish] PR #%d bead=%s: %s — HEAD already matches origin/%s, marking unverified",
			vp.prNumber, vp.beadID, timeoutDetail, vp.branch)
		markUnverified(vp, result, localHead, timeoutDetail)
		return
	}

	if headErr != nil || localHead == "" {
		// We cannot even name what is at risk. Refuse to declare the cycle
		// finished and keep the worktree so nothing is deleted blind.
		result.UnpushedHead = "unknown"
		result.Error = fmt.Errorf("review fix: %s and the worktree HEAD could not be resolved: %w", timeoutDetail, headErr)
		preserveWork(vp, result, "unresolved HEAD")
		return
	}

	log.Printf("[burnish] PR #%d bead=%s: %s — pushing %s unverified (burnish verification is advisory; the PR is re-reviewed)",
		vp.prNumber, vp.beadID, timeoutDetail, shortSHA(localHead))
	if err := gitPushFn(ctx, vp.worktreePath, vp.branch); err != nil {
		log.Printf("[burnish] PR #%d bead=%s: WARN unverified push failed: %v", vp.prNumber, vp.beadID, err)
		result.UnpushedHead = localHead
		result.Error = fmt.Errorf("review fix: %s and the unverified push failed: %w", timeoutDetail, err)
		preserveWork(vp, result, err.Error())
		return
	}
	markUnverified(vp, result, localHead, timeoutDetail)
}

// markUnverified records a fix that reached the PR without a passing
// verification. Addressed stays true — the commit IS on the PR — but the run is
// escalated rather than reported as a clean success, because "the operator sees
// success" was half of what made the original bug expensive.
func markUnverified(vp verifyParams, result *FixResult, headSHA, detail string) {
	result.Addressed = true
	result.Unverified = true
	result.HeadSHA = headSHA

	msg := fmt.Sprintf("PR #%d: review fix %s pushed UNVERIFIED — %s",
		vp.prNumber, shortSHA(headSHA), detail)
	if vp.db == nil {
		return
	}
	_ = vp.db.LogEvent(state.EventBurnishUnverifiedPush, msg, vp.beadID, vp.anvilName)
	if vp.beadID != "" {
		if err := vp.db.MarkNeedsHuman(vp.beadID, vp.anvilName, AttentionUnverified+msg); err != nil {
			log.Printf("[burnish] PR #%d: failed to raise needs-attention for unverified push: %v", vp.prNumber, err)
		}
	}
}

// preserveWork records a fix commit that exists only in the worktree. The
// caller reads result.UnpushedHead and keeps the worktree; this names the SHA
// and the checkout so recovery is a cherry-pick rather than an archaeology
// session in `git fsck --lost-found`.
func preserveWork(vp verifyParams, result *FixResult, cause string) {
	msg := fmt.Sprintf("PR #%d: review fix commit %s is UNPUSHED and kept in worktree %s (branch %s) — %s",
		vp.prNumber, shortSHA(result.UnpushedHead), vp.worktreePath, vp.branch, cause)
	log.Printf("[burnish] %s", msg)
	if vp.db == nil {
		return
	}
	_ = vp.db.LogEvent(state.EventBurnishWorkPreserved, msg, vp.beadID, vp.anvilName)
	if vp.beadID != "" {
		if err := vp.db.MarkNeedsHuman(vp.beadID, vp.anvilName, AttentionUnpushed+msg); err != nil {
			log.Printf("[burnish] PR #%d: failed to raise needs-attention for preserved work: %v", vp.prNumber, err)
		}
	}
	if vp.workerID != "" {
		_ = vp.db.UpdateWorkerStatus(vp.workerID, state.WorkerFailed)
	}
}

// shortSHA abbreviates a commit id for operator-facing messages.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
// It routes through SpawnWithOptions so burnish's session logs are named
// burnish-<ts>-<seq>.log rather than the default smith- prefix.
var smithSpawnFn = func(ctx context.Context, worktreePath, promptText, logDir string, pv provider.Provider, extraFlags []string) (*smith.Process, error) {
	return smith.SpawnWithOptions(ctx, worktreePath, promptText, logDir, pv, extraFlags, smith.SpawnOptions{LogPrefix: "burnish"})
}

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
