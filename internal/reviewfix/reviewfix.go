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

	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// MaxAttempts is the maximum fix attempts for review comments.
const MaxAttempts = 2

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
	// ExtraFlags for Claude CLI.
	ExtraFlags []string
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

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		result.Attempts = attempt

		// Step 2: Build fix prompt
		prompt := buildReviewFixPrompt(p, actionable)

		// Step 3: Spawn Smith
		_ = p.DB.LogEvent("review_fix_started",
			fmt.Sprintf("PR #%d: attempt %d, %d comments", p.PRNumber, attempt, len(actionable)),
			p.BeadID, p.AnvilName)

		logDir := p.WorktreePath + "/.forge-logs"
		process, err := smith.Spawn(ctx, p.WorktreePath, prompt, logDir, p.ExtraFlags)
		if err != nil {
			result.Error = fmt.Errorf("spawning smith for review fix: %w", err)
			result.Duration = time.Since(start)
			return result
		}

		smithResult := process.Wait()
		if smithResult.ExitCode != 0 {
			log.Printf("[reviewfix] PR #%d: Smith fix attempt %d failed (exit %d)", p.PRNumber, attempt, smithResult.ExitCode)
			_ = p.DB.LogEvent("review_fix_failed",
				fmt.Sprintf("PR #%d: Smith exit %d on attempt %d", p.PRNumber, smithResult.ExitCode, attempt),
				p.BeadID, p.AnvilName)
			continue
		}

		// Smith succeeded — assume fixes were committed and pushed
		log.Printf("[reviewfix] PR #%d: Review fixes applied on attempt %d", p.PRNumber, attempt)
		result.Addressed = true
		_ = p.DB.LogEvent("review_fix_success",
			fmt.Sprintf("PR #%d: Addressed %d comments on attempt %d", p.PRNumber, len(actionable), attempt),
			p.BeadID, p.AnvilName)
		result.Duration = time.Since(start)
		return result
	}

	result.Error = fmt.Errorf("could not address review comments after %d attempts", MaxAttempts)
	_ = p.DB.LogEvent("review_fix_exhausted",
		fmt.Sprintf("PR #%d: Exhausted %d fix attempts for %d comments", p.PRNumber, MaxAttempts, len(actionable)),
		p.BeadID, p.AnvilName)
	result.Duration = time.Since(start)
	return result
}

// fetchReviewComments gets PR review comments via gh CLI.
func fetchReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]ReviewComment, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", prNumber),
		"--json", "reviews,comments",
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr view: %w\nstderr: %s", err, stderr.String())
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
			Path string `json:"path"`
			Line int    `json:"line"`
			ID   string `json:"id"`
		} `json:"comments"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &prData); err != nil {
		return nil, fmt.Errorf("parsing pr data: %w", err)
	}

	var comments []ReviewComment

	// Review-level comments
	for _, r := range prData.Reviews {
		if r.Body != "" {
			comments = append(comments, ReviewComment{
				Author: r.Author.Login,
				Body:   r.Body,
				State:  r.State,
			})
		}
	}

	// Inline comments
	for _, c := range prData.Comments {
		comments = append(comments, ReviewComment{
			Author:   c.Author.Login,
			Body:     c.Body,
			Path:     c.Path,
			Line:     c.Line,
			ThreadID: c.ID,
		})
	}

	return comments, nil
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
