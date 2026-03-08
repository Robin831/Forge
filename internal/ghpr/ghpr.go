// Package ghpr creates pull requests via the gh CLI.
//
// After Warden approves, this package runs `gh pr create` to file a PR
// with the bead reference in the title and body.
package ghpr

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
	"github.com/Robin831/Forge/internal/state"
)

// PR represents a created pull request.
type PR struct {
	Number  int
	URL     string
	Title   string
	Branch  string
	Base    string
	BeadID  string
	Anvil   string
	Created time.Time
}

// CreateParams holds the inputs for PR creation.
type CreateParams struct {
	// WorktreePath is the git worktree directory to run gh from.
	WorktreePath string
	// BeadID to reference in the PR.
	BeadID string
	// Title for the PR (auto-generated if empty).
	Title string
	// Body for the PR (auto-generated if empty).
	Body string
	// Branch is the feature branch name.
	Branch string
	// Base is the target branch (default: main).
	Base string
	// AnvilName for state tracking.
	AnvilName string
	// Draft creates a draft PR if true.
	Draft bool
	// DB for recording the PR.
	DB *state.DB
}

// Create files a pull request using the gh CLI and records it in the state DB.
func Create(ctx context.Context, p CreateParams) (*PR, error) {
	if p.Base == "" {
		p.Base = "main"
	}

	if p.Title == "" {
		p.Title = fmt.Sprintf("forge: %s", p.BeadID)
	}

	if p.Body == "" {
		p.Body = buildDefaultBody(p.BeadID, p.Branch)
	}

	// Build gh pr create args
	args := []string{
		"pr", "create",
		"--title", p.Title,
		"--body", p.Body,
		"--base", p.Base,
		"--head", p.Branch,
	}
	if p.Draft {
		args = append(args, "--draft")
	}

	log.Printf("[ghpr] Creating PR for %s on branch %s", p.BeadID, p.Branch)

	// Run gh pr create
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = p.WorktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr create failed: %w\nstderr: %s", err, stderr.String())
	}

	prURL := strings.TrimSpace(stdout.String())
	log.Printf("[ghpr] Created PR: %s", prURL)

	// Extract PR number from URL (https://github.com/owner/repo/pull/123)
	prNumber := extractPRNumber(prURL)

	pr := &PR{
		Number:  prNumber,
		URL:     prURL,
		Title:   p.Title,
		Branch:  p.Branch,
		Base:    p.Base,
		BeadID:  p.BeadID,
		Anvil:   p.AnvilName,
		Created: time.Now(),
	}

	// Record in state DB
	if p.DB != nil {
		dbPR := &state.PR{
			Number:     prNumber,
			Anvil:      p.AnvilName,
			BeadID:     p.BeadID,
			Branch:     p.Branch,
			BaseBranch: p.Base,
			Status:     state.PROpen,
			CreatedAt:  pr.Created,
		}
		_ = p.DB.InsertPR(dbPR)
		_ = p.DB.LogEvent(state.EventPRCreated,
			fmt.Sprintf("PR #%d: %s", prNumber, prURL), p.BeadID, p.AnvilName)
	}

	return pr, nil
}

// Merge merges a PR using the gh CLI with the specified strategy.
// Valid strategies: "squash", "merge", "rebase". Defaults to "squash" if empty.
func Merge(ctx context.Context, worktreePath string, prNumber int, strategy string) error {
	if strategy == "" {
		strategy = "squash"
	}

	allowedStrategies := map[string]bool{
		"squash": true,
		"merge":  true,
		"rebase": true,
	}
	if !allowedStrategies[strategy] {
		log.Printf("[ghpr] Invalid merge strategy %q, defaulting to squash", strategy)
		strategy = "squash"
	}

	args := []string{
		"pr", "merge", fmt.Sprintf("%d", prNumber),
		"--" + strategy,
		"--delete-branch",
	}

	log.Printf("[ghpr] Merging PR #%d with strategy %s", prNumber, strategy)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = worktreePath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge failed: %w\nstderr: %s", err, stderr.String())
	}

	log.Printf("[ghpr] Merged PR #%d", prNumber)
	return nil
}

// CheckStatus gets the current status of a PR via gh pr view.
func CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", prNumber),
		"--json", "state,statusCheckRollup,reviews,reviewRequests,mergeable,headRefName",
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr view failed: %w\nstderr: %s", err, stderr.String())
	}

	var status PRStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		return nil, fmt.Errorf("parsing pr status: %w", err)
	}

	// Fetch unresolved thread count via GraphQL (Bug 1)
	count, err := FetchUnresolvedThreadCount(ctx, worktreePath, prNumber)
	if err == nil {
		status.UnresolvedThreads = count
	} else {
		log.Printf("[ghpr] Warning: could not fetch unresolved thread count for PR #%d: %v", prNumber, err)
	}

	return &status, nil
}

// FetchUnresolvedThreadCount uses the GraphQL API to count unresolved review threads.
// Paginates through all threads (100 per page) to ensure an accurate count on large PRs.
func FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error) {
	owner, repo, err := GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return 0, err
	}

	query := `
	query($owner:String!, $repo:String!, $pr:Int!, $cursor:String) {
		repository(owner:$owner, name:$repo) {
			pullRequest(number:$pr) {
				reviewThreads(first:100, after:$cursor) {
					pageInfo { hasNextPage endCursor }
					nodes { isResolved }
				}
			}
		}
	}`

	count := 0
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
			return 0, fmt.Errorf("gh api graphql: %w\nstderr: %s", err, stderr.String())
		}

		var gqlData struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []struct {
								IsResolved bool `json:"isResolved"`
							} `json:"nodes"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}

		if err := json.Unmarshal(stdout.Bytes(), &gqlData); err != nil {
			return 0, fmt.Errorf("parsing graphql response: %w", err)
		}

		for _, node := range gqlData.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if !node.IsResolved {
				count++
			}
		}

		if !gqlData.Data.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		cursor = gqlData.Data.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
	}
	return count, nil
}

// OpenPR is a lightweight view of a GitHub PR used for reconciliation.
type OpenPR struct {
	Number  int
	Title   string
	Branch  string
	Body    string
}

// ListOpen returns all open PRs in the repository for the given worktree path.
func ListOpen(ctx context.Context, worktreePath string) ([]OpenPR, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "list",
		"--state", "open",
		"--json", "number,title,headRefName,body",
		"--limit", "100",
	))
	cmd.Dir = worktreePath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr list failed: %w\nstderr: %s", err, stderr.String())
	}
	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parsing pr list: %w", err)
	}
	out := make([]OpenPR, len(raw))
	for i, r := range raw {
		out[i] = OpenPR{Number: r.Number, Title: r.Title, Branch: r.HeadRefName, Body: r.Body}
	}
	return out, nil
}

// GetRepoOwnerAndName extracts the owner and repository name from git remote origin.
func GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = worktreePath
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git remote get-url origin: %w\nstderr: %s", err, stderr.String())
	}
	url := strings.TrimSpace(stdout.String())
	return ParseRepoURL(url)
}

// ParseRepoURL parses a git remote URL into owner and repository name.
func ParseRepoURL(url string) (owner, repo string, err error) {
	url = strings.TrimSuffix(url, ".git")

	if strings.Contains(url, "github.com") {
		if strings.HasPrefix(url, "https://") {
			parts := strings.Split(strings.TrimPrefix(url, "https://"), "/")
			if len(parts) >= 3 {
				return parts[1], parts[2], nil
			}
		} else if strings.HasPrefix(url, "git@") {
			parts := strings.Split(strings.TrimPrefix(url, "git@"), ":")
			if len(parts) == 2 {
				subParts := strings.Split(parts[1], "/")
				if len(subParts) == 2 {
					return subParts[0], subParts[1], nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("could not parse remote URL: %s", url)
}

// PRStatus represents the GitHub state of a PR.
type PRStatus struct {
	State             string           `json:"state"`
	StatusCheckRollup []CheckRun       `json:"statusCheckRollup"`
	Reviews           []Review         `json:"reviews"`
	ReviewRequests    []ReviewRequest  `json:"reviewRequests"`
	Mergeable         string           `json:"mergeable"`
	UnresolvedThreads int              `json:"unresolvedThreads"`
	HeadRefName       string           `json:"headRefName"`
}

// CheckRun represents a CI check on the PR.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// ReviewAuthor is the author object returned by the GitHub API.
type ReviewAuthor struct {
	Login string `json:"login"`
}

// Review represents a PR review.
type Review struct {
	Author ReviewAuthor `json:"author"`
	State  string       `json:"state"`
	Body   string       `json:"body"`
}

// ReviewRequest represents a pending review request on a PR.
// GitHub returns either a user login or a team slug depending on the request type.
type ReviewRequest struct {
	Login string `json:"login"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
}

// IsMerged returns true if the PR has been merged.
func (s *PRStatus) IsMerged() bool {
	return s.State == "MERGED"
}

// IsClosed returns true if the PR has been closed without merging.
func (s *PRStatus) IsClosed() bool {
	return s.State == "CLOSED"
}

// CIsPassing returns true if all CI checks have passed.
func (s *PRStatus) CIsPassing() bool {
	if len(s.StatusCheckRollup) == 0 {
		return true // no checks configured
	}
	for _, check := range s.StatusCheckRollup {
		if check.Conclusion != "SUCCESS" && check.Conclusion != "NEUTRAL" && check.Conclusion != "SKIPPED" {
			return false
		}
	}
	return true
}

// HasApproval returns true if at least one review is APPROVED.
func (s *PRStatus) HasApproval() bool {
	for _, r := range s.Reviews {
		if r.State == "APPROVED" {
			return true
		}
	}
	return false
}

// NeedsChanges returns true if any review requests changes or there are unresolved threads.
func (s *PRStatus) NeedsChanges() bool {
	for _, r := range s.Reviews {
		if r.State == "CHANGES_REQUESTED" {
			return true
		}
	}
	return s.UnresolvedThreads > 0
}

// HasPendingReviewRequests returns true if there are outstanding review requests
// that have not yet been fulfilled (i.e., the reviewer hasn't submitted a review).
func (s *PRStatus) HasPendingReviewRequests() bool {
	return len(s.ReviewRequests) > 0
}

// buildDefaultBody creates a standard PR body referencing the bead.
func buildDefaultBody(beadID, branch string) string {
	return fmt.Sprintf(`## Automated PR by The Forge

**Bead**: %s
**Branch**: %s

This PR was generated by The Forge autonomous pipeline.
A Smith agent implemented the changes, Temper verified the build,
and Warden approved the code review.

---
*Generated by [The Forge](https://github.com/Robin831/Forge)*`, beadID, branch)
}

// extractPRNumber parses the PR number from a GitHub PR URL.
func extractPRNumber(url string) int {
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return 0
	}
	last := parts[len(parts)-1]
	var n int
	fmt.Sscanf(last, "%d", &n)
	return n
}
