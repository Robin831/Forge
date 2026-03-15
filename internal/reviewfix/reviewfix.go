// Package reviewfix spawns a Smith worker to address PR review comments.
//
// When Bellows detects "changes requested" on a PR, reviewfix fetches the
// review comments via the VCS provider, constructs a targeted fix prompt,
// and spawns Smith to address them. It then pushes the fixes to the PR branch.
package reviewfix

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

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

	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		result.Attempts = attempt

		// Step 2: Build fix prompt
		prompt := buildReviewFixPrompt(p, actionable)

		// Step 3: Spawn Smith (with provider fallback on rate limit)
		_ = p.DB.LogEvent(state.EventReviewFixStarted,
			fmt.Sprintf("PR #%d: attempt %d, %d comments (provider: %s)", p.PRNumber, attempt, len(actionable), providers[activeProviderIdx].Label()),
			p.BeadID, p.AnvilName)

		logDir := p.WorktreePath + "/.forge-logs"
		var smithResult *smith.Result
		for pi := activeProviderIdx; pi < len(providers); pi++ {
			pv := providers[pi]
			if pi > activeProviderIdx {
				log.Printf("[burnish] PR #%d: Provider %s rate limited, retrying with %s",
					p.PRNumber, providers[pi-1].Label(), pv.Label())
				_ = p.DB.LogEvent(state.EventReviewFixSmithError,
					fmt.Sprintf("PR #%d attempt %d: %s rate limited, falling back to %s",
						p.PRNumber, attempt, providers[pi-1].Label(), pv.Label()),
					p.BeadID, p.AnvilName)
			}
			process, err := smith.SpawnWithProvider(ctx, p.WorktreePath, prompt, logDir, pv, p.ExtraFlags)
			if err != nil {
				result.Error = fmt.Errorf("spawning smith (%s) for review fix: %w", pv.Label(), err)
				_ = p.DB.LogEvent(state.EventReviewFixFailed, result.Error.Error(), p.BeadID, p.AnvilName)
				result.Duration = time.Since(start)
				return result
			}
			if p.WorkerID != "" && p.DB != nil {
				if err := p.DB.UpdateWorkerLogPath(p.WorkerID, process.LogPath); err != nil {
					log.Printf("[burnish] PR #%d: failed to update worker log path for worker %s to %q: %v",
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

		// If all providers are rate-limited, abort rather than burning more attempts.
		if smithResult.RateLimited {
			log.Printf("[burnish] PR #%d: All providers rate limited on attempt %d", p.PRNumber, attempt)
			_ = p.DB.LogEvent(state.EventReviewFixFailed,
				fmt.Sprintf("PR #%d attempt %d: all providers rate limited", p.PRNumber, attempt),
				p.BeadID, p.AnvilName)
			_ = p.DB.LogEvent(state.EventReviewFixSmithError,
				fmt.Sprintf("PR #%d attempt %d: rate_limited (all %d providers exhausted)", p.PRNumber, attempt, len(providers)),
				p.BeadID, p.AnvilName)
			result.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
			result.Duration = time.Since(start)
			return result
		}

		if smithResult.ExitCode != 0 {
			log.Printf("[burnish] PR #%d: Smith fix attempt %d failed (exit %d, subtype=%s)",
				p.PRNumber, attempt, smithResult.ExitCode, smithResult.ResultSubtype)
			_ = p.DB.LogEvent(state.EventReviewFixFailed,
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
				_ = p.DB.LogEvent(state.EventReviewFixSmithError,
					fmt.Sprintf("PR #%d attempt %d: %s", p.PRNumber, attempt, errDetail),
					p.BeadID, p.AnvilName)
			}
			continue
		}

		// Smith succeeded — assume fixes were committed and pushed
		log.Printf("[burnish] PR #%d: Review fixes applied on attempt %d", p.PRNumber, attempt)
		result.Addressed = true

		// Resolve threads after successful fix.
		// Only thread-level comments (with a ThreadID from GraphQL) can be resolved
		// via the API; review-level CHANGES_REQUESTED comments have no thread ID.
		resolvedCount := 0
		for _, comment := range actionable {
			if comment.ThreadID == "" {
				continue
			}
			if err := p.VCS.ResolveThread(ctx, p.WorktreePath, comment.ThreadID); err != nil {
				log.Printf("[burnish] PR #%d: Warning: failed to resolve thread %s: %v", p.PRNumber, comment.ThreadID, err)
				_ = p.DB.LogEvent(state.EventReviewFixFailed,
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

		_ = p.DB.LogEvent(state.EventReviewFixSuccess,
			fmt.Sprintf("PR #%d: Addressed %d comments on attempt %d", p.PRNumber, len(actionable), attempt),
			p.BeadID, p.AnvilName)

		result.Duration = time.Since(start)
		return result
	}

	result.Error = fmt.Errorf("could not address review comments after %d attempts", p.MaxAttempts)
	_ = p.DB.LogEvent(state.EventReviewFixFailed,
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
3. **Run the test suite** (e.g. "go test ./..." for Go, "dotnet test" for .NET, "npm test" or "npx vitest run" for Node/frontend) and fix any failures before continuing — do NOT commit or push if tests are failing
4. Commit with message: "fix: address review comments for %s"
5. Push to branch: %s

## Working Directory

%s
`, p.BeadID, p.Branch, p.WorktreePath)

	return b.String()
}
