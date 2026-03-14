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

	// BeadTitle is the bead's human-readable title (used in the PR body).
	BeadTitle string
	// BeadDescription is the bead's problem/task description (used in the PR body).
	BeadDescription string
	// BeadType is the bead's issue type (bug, feature, task, etc.).
	BeadType string
	// ChangeSummary is a summary of what changed (from warden review or diff stat).
	ChangeSummary string
}

// selectTitle determines the PR title to use.
//
// Priority order:
//  1. Titles containing special CI markers (e.g. "[no-changelog]") are
//     preserved verbatim — callers such as Bellows rely on these markers.
//  2. When Title already contains the BeadID it is already anchored to the
//     bead (e.g. Crucible adds "(parent) (<BeadID>)"). Preserve it so
//     disambiguating suffixes are not stripped.
//  3. When BeadTitle is set and the title is not yet anchored, the PR title
//     is set to "BeadTitle (BeadID)". This prevents the Smith's last commit
//     message — which may describe an incidental fix discovered during
//     implementation — from overriding the PR title.
//  4. Fall back to the branch's commit subject when no BeadTitle is available
//     (e.g. callers that do not populate BeadTitle).
//  5. Use the explicitly provided Title as-is if the commit subject is empty.
//  6. Return "forge: BeadID" when no other information is available.
func selectTitle(ctx context.Context, p CreateParams) string {
	// Preserve explicitly provided titles that contain special markers used
	// by callers like Bellows for CI (e.g. "[no-changelog]").
	if strings.Contains(p.Title, "[no-changelog]") {
		return p.Title
	}
	// If the title is already anchored to the bead (contains the bead ID),
	// preserve it as-is. This handles callers like Crucible that build
	// custom titles such as "<bead title> (parent) (<bead ID>)".
	if p.BeadID != "" && strings.Contains(p.Title, p.BeadID) {
		return p.Title
	}
	// Anchor to the bead title when available so the PR title reflects the
	// bead's intent rather than whatever the Smith committed last.
	if p.BeadTitle != "" {
		return fmt.Sprintf("%s (%s)", p.BeadTitle, p.BeadID)
	}
	// No structured bead title: try the branch's commit subject. Smith writes
	// commit messages in English, so this is a reasonable fallback.
	if subject := commitSubject(ctx, p.WorktreePath, p.Branch); subject != "" {
		return fmt.Sprintf("%s (%s)", subject, p.BeadID)
	}
	// Use the explicitly provided title if commit subject lookup failed.
	if p.Title != "" {
		return p.Title
	}
	return fmt.Sprintf("forge: %s", p.BeadID)
}

// Create files a pull request using the gh CLI and records it in the state DB.
func Create(ctx context.Context, p CreateParams) (*PR, error) {
	if p.Base == "" {
		p.Base = "main"
	}

	p.Title = selectTitle(ctx, p)

	if p.Body == "" {
		p.Body = buildDefaultBody(p)
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
		if err := p.DB.InsertPR(dbPR); err != nil {
			log.Printf("[ghpr] failed to insert PR in DB (bead=%s, anvil=%s, number=%d): %v", p.BeadID, p.AnvilName, prNumber, err)
		} else {
			_ = p.DB.LogEvent(
				state.EventPRCreated,
				fmt.Sprintf("PR #%d: %s", prNumber, prURL),
				p.BeadID,
				p.AnvilName,
			)

			// Run a light mergeability check immediately so the DB reflects
			// current merge conflict state before the first Bellows poll.
			// Unresolved thread counts are not populated by this light check;
			// Bellows' full status check later is the authoritative source for
			// both unresolved threads and clearing has_pending_reviews. We keep
			// the safe default (true) from InsertPR until Bellows confirms otherwise.
			if status, err := CheckStatusLight(ctx, p.WorktreePath, prNumber); err != nil {
				log.Printf("[ghpr] warning: failed to CheckStatusLight for PR #%d (worktree %q): %v", prNumber, p.WorktreePath, err)
			} else if dbPR.ID != 0 {
				if err := p.DB.UpdatePRMergeability(
					dbPR.ID,
					false,                             // CI hasn't run yet; Bellows is authoritative
					status.Mergeable == "CONFLICTING", // only conflict state is reliable from CheckStatusLight
					false,                             // unresolved threads not fetched; Bellows is authoritative
					true,                              // keep pending reviews safe default until Bellows confirms
					false,                             // approval not checked here; Bellows is authoritative
				); err != nil {
					log.Printf("[ghpr] warning: failed to UpdatePRMergeability for PR record %d (PR #%d): %v", dbPR.ID, prNumber, err)
				}
			}
		}
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

// MergeabilityInputs holds the computed boolean inputs for UpdatePRMergeability,
// extracted from a PRStatus. This struct exists so the conversion logic can be
// unit-tested without invoking the gh CLI.
type MergeabilityInputs struct {
	HasConflicts         bool
	HasUnresolvedThreads bool
	HasPendingReviews    bool
}

// MergeabilityFromStatus converts a PRStatus into the boolean inputs needed by
// state.DB.UpdatePRMergeability.
func MergeabilityFromStatus(s *PRStatus) MergeabilityInputs {
	return MergeabilityInputs{
		HasConflicts:         s.Mergeable == "CONFLICTING",
		HasUnresolvedThreads: s.UnresolvedThreads > 0,
		HasPendingReviews:    s.HasPendingReviewRequests(),
	}
}

// CheckStatusLight gets the review-request and mergeable state of a PR without
// fetching unresolved thread counts (which requires expensive GraphQL pagination).
// Use this when you only need reviewRequests/mergeable — e.g., right after PR creation.
func CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", prNumber),
		"--json", "state,reviewRequests,mergeable",
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

	return &status, nil
}

// CheckStatus gets the current status of a PR via gh pr view.
func CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", prNumber),
		"--json", "state,statusCheckRollup,reviews,reviewRequests,mergeable,headRefName,url",
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

	// Fetch pending review requests via GraphQL. The gh CLI's --json
	// reviewRequests field does not include Bot reviewers (e.g., Copilot),
	// so we use GraphQL which returns all reviewer types.
	gqlRequests, err := FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err == nil {
		status.ReviewRequests = gqlRequests
	} else {
		log.Printf("[ghpr] Warning: could not fetch pending review requests for PR #%d: %v (falling back to gh pr view data)", prNumber, err)
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

// FetchPendingReviewRequests uses GraphQL to check for pending review requests,
// including Bot reviewers (e.g., copilot-pull-request-reviewer) that the gh CLI's
// --json reviewRequests field does not serialize.
func FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ReviewRequest, error) {
	owner, repo, err := GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	query := `
	query($owner:String!, $repo:String!, $pr:Int!) {
		repository(owner:$owner, name:$repo) {
			pullRequest(number:$pr) {
				reviewRequests(first:25) {
					nodes {
						requestedReviewer {
							__typename
							... on User { login }
							... on Team { slug name }
							... on Bot  { login }
							... on Mannequin { login }
						}
					}
				}
			}
		}
	}`

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh",
		"api", "graphql",
		"-f", "query="+query,
		"-f", "owner="+owner,
		"-f", "repo="+repo,
		"-F", fmt.Sprintf("pr=%d", prNumber),
	))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w\nstderr: %s", err, stderr.String())
	}

	var gqlData struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewRequests struct {
						Nodes []struct {
							RequestedReviewer struct {
								TypeName string `json:"__typename"`
								Login    string `json:"login"`
								Slug     string `json:"slug"`
								Name     string `json:"name"`
							} `json:"requestedReviewer"`
						} `json:"nodes"`
					} `json:"reviewRequests"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &gqlData); err != nil {
		return nil, fmt.Errorf("parsing graphql response: %w", err)
	}

	var requests []ReviewRequest
	for _, node := range gqlData.Data.Repository.PullRequest.ReviewRequests.Nodes {
		r := node.RequestedReviewer
		requests = append(requests, ReviewRequest{
			Login: r.Login,
			Slug:  r.Slug,
			Name:  r.Name,
		})
	}
	return requests, nil
}

// OpenPR is a lightweight view of a GitHub PR used for reconciliation.
type OpenPR struct {
	Number int
	Title  string
	Branch string
	Body   string
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
// Supports HTTPS and SSH URLs for any hosting platform (GitHub, GitLab,
// Gitea/Forgejo, Bitbucket, Azure DevOps, self-hosted instances).
func ParseRepoURL(url string) (owner, repo string, err error) {
	url = strings.TrimSuffix(url, ".git")

	// HTTPS: https://host/owner/repo or https://host/owner/repo.git
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		// Strip scheme
		idx := strings.Index(url, "://")
		path := url[idx+3:]
		parts := strings.Split(path, "/")
		// Expect at least host/owner/repo (3 parts)
		if len(parts) >= 3 {
			return parts[1], parts[2], nil
		}
	}

	// SSH: git@host:owner/repo or ssh://git@host/owner/repo
	if strings.HasPrefix(url, "ssh://") {
		// ssh://git@host/owner/repo → strip scheme and user@host
		idx := strings.Index(url, "://")
		rest := url[idx+3:]
		if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
			rest = rest[atIdx+1:]
		}
		parts := strings.Split(rest, "/")
		if len(parts) >= 3 {
			return parts[1], parts[2], nil
		}
	}

	if strings.HasPrefix(url, "git@") {
		// git@host:owner/repo
		colonParts := strings.SplitN(strings.TrimPrefix(url, "git@"), ":", 2)
		if len(colonParts) == 2 {
			subParts := strings.Split(colonParts[1], "/")
			if len(subParts) >= 2 {
				return subParts[0], subParts[1], nil
			}
		}
	}

	return "", "", fmt.Errorf("could not parse remote URL: %s", url)
}

// PRStatus represents the GitHub state of a PR.
type PRStatus struct {
	State             string          `json:"state"`
	StatusCheckRollup []CheckRun      `json:"statusCheckRollup"`
	Reviews           []Review        `json:"reviews"`
	ReviewRequests    []ReviewRequest `json:"reviewRequests"`
	Mergeable         string          `json:"mergeable"`
	UnresolvedThreads int             `json:"unresolvedThreads"`
	HeadRefName       string          `json:"headRefName"`
	URL               string          `json:"url"`
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

// commitSubject returns the subject line of the most recent commit on the
// given branch. It uses `git log` to read the commit message, which Smith
// writes in English regardless of the bead's language. Returns empty string
// on any error.
func commitSubject(ctx context.Context, worktreePath, branch string) string {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "log", branch, "--format=%s", "-1"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Retry with origin-prefixed ref (branch may only exist on remote).
		cmd2 := executil.HideWindow(exec.CommandContext(ctx, "git", "log", "origin/"+branch, "--format=%s", "-1"))
		cmd2.Dir = worktreePath
		var stdout2 bytes.Buffer
		cmd2.Stdout = &stdout2
		if err2 := cmd2.Run(); err2 != nil {
			return ""
		}
		return strings.TrimSpace(stdout2.String())
	}
	return strings.TrimSpace(stdout.String())
}

// buildDefaultBody creates a structured PR body from bead metadata and change context.
// The body is written in English: the change summary (from Warden review) is used as
// the primary description, and the original bead description is included as context
// in case it is in a non-English language.
func buildDefaultBody(p CreateParams) string {
	var b strings.Builder

	// Lead with change summary (English, from Warden review) when available.
	if p.ChangeSummary != "" {
		b.WriteString("## Changes\n\n")
		b.WriteString(p.ChangeSummary)
		b.WriteString("\n\n")
	}

	// Include the original bead description as context. It may be in a
	// non-English language, so we label it clearly.
	if p.BeadDescription != "" {
		header := "## Original Issue"
		if p.BeadType != "" {
			header = fmt.Sprintf("## Original Issue (%s)", p.BeadType)
		}
		if p.BeadTitle != "" {
			fmt.Fprintf(&b, "%s: %s\n\n", header, p.BeadTitle)
		} else {
			fmt.Fprintf(&b, "%s\n\n", header)
		}
		b.WriteString(p.BeadDescription)
		b.WriteString("\n\n")
	}

	// Footer
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Bead: %s | Branch: %s\n", p.BeadID, p.Branch)
	b.WriteString("Generated by [The Forge](https://github.com/Robin831/Forge) (Smith → Temper → Warden)")

	return b.String()
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
