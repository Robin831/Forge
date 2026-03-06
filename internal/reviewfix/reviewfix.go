// Package reviewfix spawns a Smith worker to address PR review comments.
//
// When Bellows detects "changes requested" on a PR, reviewfix fetches the
// review comments via gh, constructs a targeted fix prompt, and spawns
// Smith to address them. It then pushes the fixes to the PR branch.
package reviewfix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
)

// DefaultCopilotReviewer is the GitHub reviewer handle for GitHub Copilot PR reviews.
// It aliases ghpr.DefaultReviewer to avoid duplicating the default value.
var DefaultCopilotReviewer = ghpr.DefaultReviewer

// ReviewComment represents a PR review comment from GitHub.
type ReviewComment struct {
	Author   string `json:"author"`
	Body     string `json:"body"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	State    string `json:"state"`
	ThreadID string `json:"id"`
}

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
	// MaxAttempts is the maximum fix attempts for review comments.
	MaxAttempts int
	// Reviewer is the GitHub reviewer handle to request re-review from after pushing fixes.
	// Defaults to DefaultCopilotReviewer ("copilot-pull-request-reviewer") if empty.
	Reviewer string
	// ExtraFlags for Claude CLI.
	ExtraFlags []string
	// Providers is the ordered list of AI providers to try.
	// If empty, provider.Defaults() is used (Claude → Gemini).
	Providers []provider.Provider
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
		log.Printf("[reviewfix] PR #%d: MaxAttempts=%d is not positive; defaulting to 1 attempt", p.PRNumber, p.MaxAttempts)
		p.MaxAttempts = 1
	}

	// Step 1: Fetch review comments
	comments, err := fetchReviewComments(ctx, p.WorktreePath, p.PRNumber)
	if err != nil {
		result.Error = fmt.Errorf("fetching review comments: %w", err)
		result.Duration = time.Since(start)
		return result
	}

	result.CommentsFound = len(comments)
	if len(comments) == 0 {
		log.Printf("[reviewfix] PR #%d: No review comments found", p.PRNumber)
		result.Addressed = true
		result.Duration = time.Since(start)
		return result
	}

	// Filter to unresolved/actionable comments
	actionable := filterActionableComments(comments)
	if len(actionable) == 0 {
		log.Printf("[reviewfix] PR #%d: No actionable comments", p.PRNumber)
		result.Addressed = true
		result.Duration = time.Since(start)
		return result
	}

	log.Printf("[reviewfix] PR #%d: %d actionable review comments", p.PRNumber, len(actionable))

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
				log.Printf("[reviewfix] PR #%d: Provider %s rate limited, retrying with %s",
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

		// If all providers are rate-limited, abort rather than burning more attempts.
		if smithResult.RateLimited {
			log.Printf("[reviewfix] PR #%d: All providers rate limited on attempt %d", p.PRNumber, attempt)
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
			log.Printf("[reviewfix] PR #%d: Smith fix attempt %d failed (exit %d, subtype=%s)",
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
		log.Printf("[reviewfix] PR #%d: Review fixes applied on attempt %d", p.PRNumber, attempt)
		result.Addressed = true

		// Resolve threads after successful fix.
		// Only thread-level comments (with a ThreadID from GraphQL) can be resolved
		// via the API; review-level CHANGES_REQUESTED comments have no thread ID.
		resolvedCount := 0
		for _, comment := range actionable {
			if comment.ThreadID == "" {
				continue
			}
			if err := resolveThread(ctx, p.WorktreePath, comment.ThreadID); err != nil {
				log.Printf("[reviewfix] PR #%d: Warning: failed to resolve thread %s: %v", p.PRNumber, comment.ThreadID, err)
				_ = p.DB.LogEvent(state.EventReviewFixFailed,
					fmt.Sprintf("PR #%d: resolve thread %s failed: %v", p.PRNumber, comment.ThreadID, err),
					p.BeadID, p.AnvilName)
			} else {
				resolvedCount++
				log.Printf("[reviewfix] PR #%d: Resolved thread %s (by @%s)", p.PRNumber, comment.ThreadID, comment.Author)
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
			log.Printf("[reviewfix] PR #%d: Resolved %d/%d threads on GitHub", p.PRNumber, resolvedCount, len(actionable))
		}

		_ = p.DB.LogEvent(state.EventReviewFixSuccess,
			fmt.Sprintf("PR #%d: Addressed %d comments on attempt %d", p.PRNumber, len(actionable), attempt),
			p.BeadID, p.AnvilName)

		// Request re-review so Copilot (or the configured reviewer) re-examines
		// the PR after the fix commit is pushed. Without this, the review cycle
		// stalls because the reviewer is never notified.
		reviewer := p.Reviewer
		if reviewer == "" {
			reviewer = DefaultCopilotReviewer
		}
		if err := ghpr.RequestReReview(ctx, p.WorktreePath, p.PRNumber, reviewer); err != nil {
			log.Printf("[reviewfix] PR #%d: could not request re-review from %s: %v", p.PRNumber, reviewer, err)
			// Treat re-review request failure as a failed fix cycle so Bellows can retry or escalate.
			result.Error = fmt.Errorf("PR #%d: request re-review from %s failed: %w", p.PRNumber, reviewer, err)
			_ = p.DB.LogEvent(state.EventReReviewRequestFailed,
				fmt.Sprintf("PR #%d: re-review request failed after addressing comments: %v", p.PRNumber, err),
				p.BeadID, p.AnvilName)
			result.Duration = time.Since(start)
			return result
		}
		_ = p.DB.LogEvent(state.EventReReviewRequested,
			fmt.Sprintf("PR #%d: requested re-review from %s", p.PRNumber, reviewer),
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

// fetchReviewComments gets PR review comments via GraphQL and gh CLI.
func fetchReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]ReviewComment, error) {
	owner, repo, err := ghpr.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("getting repo owner and name: %w", err)
	}

	// 1. Fetch unresolved threads via GraphQL (paginated — avoids missing threads on large PRs)
	query := `
	query($owner:String!, $repo:String!, $pr:Int!, $cursor:String) {
		repository(owner:$owner, name:$repo) {
			pullRequest(number:$pr) {
				reviewThreads(first:100, after:$cursor) {
					pageInfo { hasNextPage endCursor }
					nodes {
						id
						isResolved
						comments(first:1) {
							nodes { path line body author { login } }
						}
					}
				}
			}
		}
	}`

	type gqlPage struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									Path   string `json:"path"`
									Line   int    `json:"line"`
									Body   string `json:"body"`
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}

	var comments []ReviewComment
	cursor := ""
	for {
		args := []string{
			"api", "graphql",
			"-f", "query=" + query,
			"-f", "owner=" + owner,
			"-f", "repo=" + repo,
			"-F", fmt.Sprintf("pr=%d", prNumber),
		}
		if cursor != "" {
			args = append(args, "-f", "cursor="+cursor)
		} else {
			args = append(args, "-F", "cursor=null")
		}

		cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
		cmd.Dir = worktreePath

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("gh api graphql: %w\nstderr: %s", err, stderr.String())
		}

		var page gqlPage
		if err := json.Unmarshal(stdout.Bytes(), &page); err != nil {
			return nil, fmt.Errorf("parsing graphql response: %w", err)
		}

		for _, thread := range page.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if thread.IsResolved {
				continue
			}
			if len(thread.Comments.Nodes) > 0 {
				c := thread.Comments.Nodes[0]
				comments = append(comments, ReviewComment{
					Author:   c.Author.Login,
					Body:     c.Body,
					Path:     c.Path,
					Line:     c.Line,
					ThreadID: thread.ID,
				})
			}
		}

		if !page.Data.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		cursor = page.Data.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
	}

	// 2. Fetch PR-level reviews via gh pr view
	args2 := []string{
		"pr", "view", fmt.Sprintf("%d", prNumber),
		"--json", "reviews,comments",
	}

	cmd2 := executil.HideWindow(exec.CommandContext(ctx, "gh", args2...))
	cmd2.Dir = worktreePath

	var stdout2, stderr2 bytes.Buffer
	cmd2.Stdout = &stdout2
	cmd2.Stderr = &stderr2

	if err := cmd2.Run(); err != nil {
		return comments, nil // Return what we have from threads if this fails
	}

	var prData struct {
		Reviews []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body  string `json:"body"`
			State string `json:"state"`
		} `json:"reviews"`
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body string `json:"body"`
		} `json:"comments"`
	}

	if err := json.Unmarshal(stdout2.Bytes(), &prData); err == nil {
		// Review-level comments
		for _, r := range prData.Reviews {
			if r.State != "APPROVED" && r.State != "DISMISSED" && len(r.Body) > 20 {
				comments = append(comments, ReviewComment{
					Author: r.Author.Login,
					Body:   r.Body,
					State:  r.State,
				})
			}
		}
		// PR-level general comments
		for _, c := range prData.Comments {
			if len(c.Body) > 20 {
				comments = append(comments, ReviewComment{
					Author: c.Author.Login,
					Body:   c.Body,
				})
			}
		}
	}

	return comments, nil
}

func resolveThread(ctx context.Context, worktreePath string, threadID string) error {
	query := `
	mutation($threadId:ID!) {
		resolveReviewThread(input:{threadId:$threadId}) {
			thread { isResolved }
		}
	}`

	args := []string{
		"api", "graphql",
		"-f", "query=" + query,
		"-f", "threadId=" + threadID,
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = worktreePath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api graphql (resolve): %w\nstderr: %s", err, stderr.String())
	}

	return nil
}

// filterActionableComments keeps only comments that need action.
func filterActionableComments(comments []ReviewComment) []ReviewComment {
	var actionable []ReviewComment
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
func buildReviewFixPrompt(p FixParams, comments []ReviewComment) string {
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
3. Ensure the project still builds and tests pass after changes
4. Commit with message: "fix: address review comments for %s"
5. Push to branch: %s

## Working Directory

%s
`, p.BeadID, p.Branch, p.WorktreePath)

	return b.String()
}
