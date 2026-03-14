// Package github implements the vcs.Provider interface for GitHub
// using the gh CLI.
package github

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
	"github.com/Robin831/Forge/internal/vcs"
)

// Provider implements vcs.Provider for GitHub using the gh CLI.
type Provider struct {
	db *state.DB
}

// New creates a GitHub VCS provider. The state DB is optional (may be nil);
// when non-nil, CreatePR records the PR and runs an initial mergeability check.
func New(db *state.DB) *Provider {
	return &Provider{db: db}
}

func init() {
	vcs.RegisterGitHubProvider(func() vcs.Provider { return New(nil) })
}

// Platform returns vcs.GitHub.
func (p *Provider) Platform() vcs.Platform {
	return vcs.GitHub
}

// CreatePR creates a pull request using the gh CLI and optionally records it
// in the state DB when a DB was provided at construction time.
func (p *Provider) CreatePR(ctx context.Context, params vcs.CreateParams) (*vcs.PR, error) {
	if params.Base == "" {
		params.Base = "main"
	}

	params.Title = selectTitle(ctx, params)

	if params.Body == "" {
		params.Body = buildDefaultBody(params)
	}

	args := []string{
		"pr", "create",
		"--title", params.Title,
		"--body", params.Body,
		"--base", params.Base,
		"--head", params.Branch,
	}
	if params.Draft {
		args = append(args, "--draft")
	}

	log.Printf("[ghpr] Creating PR for %s on branch %s", params.BeadID, params.Branch)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = params.WorktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr create failed: %w\nstderr: %s", err, stderr.String())
	}

	prURL := strings.TrimSpace(stdout.String())
	log.Printf("[ghpr] Created PR: %s", prURL)

	prNumber := extractPRNumber(prURL)

	pr := &vcs.PR{
		Number:  prNumber,
		URL:     prURL,
		Title:   params.Title,
		Branch:  params.Branch,
		Base:    params.Base,
		BeadID:  params.BeadID,
		Anvil:   params.AnvilName,
		Created: time.Now(),
	}

	// Record in state DB
	if p.db != nil {
		dbPR := &state.PR{
			Number:     prNumber,
			Anvil:      params.AnvilName,
			BeadID:     params.BeadID,
			Branch:     params.Branch,
			BaseBranch: params.Base,
			Status:     state.PROpen,
			CreatedAt:  pr.Created,
		}
		if err := p.db.InsertPR(dbPR); err != nil {
			log.Printf("[ghpr] failed to insert PR in DB (bead=%s, anvil=%s, number=%d): %v", params.BeadID, params.AnvilName, prNumber, err)
		} else {
			_ = p.db.LogEvent(
				state.EventPRCreated,
				fmt.Sprintf("PR #%d: %s", prNumber, prURL),
				params.BeadID,
				params.AnvilName,
			)

			// Run a light mergeability check immediately so the DB reflects
			// current merge conflict state before the first Bellows poll.
			if status, err := p.CheckStatusLight(ctx, params.WorktreePath, prNumber); err != nil {
				log.Printf("[ghpr] warning: failed to CheckStatusLight for PR #%d (worktree %q): %v", prNumber, params.WorktreePath, err)
			} else if dbPR.ID != 0 {
				if err := p.db.UpdatePRMergeability(
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

// MergePR merges a PR using the gh CLI with the specified strategy.
// Valid strategies: "squash", "merge", "rebase". Defaults to "squash" if empty.
func (p *Provider) MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error {
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

// CheckStatus gets the full status of a PR via gh pr view and GraphQL.
func (p *Provider) CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*vcs.PRStatus, error) {
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

	var status vcs.PRStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		return nil, fmt.Errorf("parsing pr status: %w", err)
	}

	// Fetch unresolved thread count via GraphQL
	count, err := p.FetchUnresolvedThreadCount(ctx, worktreePath, prNumber)
	if err == nil {
		status.UnresolvedThreads = count
	} else {
		log.Printf("[ghpr] Warning: could not fetch unresolved thread count for PR #%d: %v", prNumber, err)
	}

	// Fetch pending review requests via GraphQL. The gh CLI's --json
	// reviewRequests field does not include Bot reviewers (e.g., Copilot),
	// so we use GraphQL which returns all reviewer types.
	gqlRequests, err := p.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err == nil {
		status.ReviewRequests = gqlRequests
	} else {
		log.Printf("[ghpr] Warning: could not fetch pending review requests for PR #%d: %v (falling back to gh pr view data)", prNumber, err)
	}

	return &status, nil
}

// CheckStatusLight gets the review-request and mergeable state of a PR without
// fetching unresolved thread counts (which requires expensive GraphQL pagination).
func (p *Provider) CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*vcs.PRStatus, error) {
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

	var status vcs.PRStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		return nil, fmt.Errorf("parsing pr status: %w", err)
	}

	return &status, nil
}

// ListOpenPRs returns all open PRs in the repository.
func (p *Provider) ListOpenPRs(ctx context.Context, worktreePath string) ([]vcs.OpenPR, error) {
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
	out := make([]vcs.OpenPR, len(raw))
	for i, r := range raw {
		out[i] = vcs.OpenPR{Number: r.Number, Title: r.Title, Branch: r.HeadRefName, Body: r.Body}
	}
	return out, nil
}

// GetRepoOwnerAndName extracts the owner and repository name from git remote origin.
func (p *Provider) GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error) {
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

// FetchUnresolvedThreadCount uses the GraphQL API to count unresolved review threads.
func (p *Provider) FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error) {
	owner, repo, err := p.GetRepoOwnerAndName(ctx, worktreePath)
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
func (p *Provider) FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]vcs.ReviewRequest, error) {
	owner, repo, err := p.GetRepoOwnerAndName(ctx, worktreePath)
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

	var requests []vcs.ReviewRequest
	for _, node := range gqlData.Data.Repository.PullRequest.ReviewRequests.Nodes {
		r := node.RequestedReviewer
		requests = append(requests, vcs.ReviewRequest{
			Login: r.Login,
			Slug:  r.Slug,
			Name:  r.Name,
		})
	}
	return requests, nil
}

// ParseRepoURL parses a git remote URL into owner and repository name.
func ParseRepoURL(url string) (owner, repo string, err error) {
	url = strings.TrimSuffix(url, ".git")

	if strings.Contains(url, "github.com") {
		if after, ok := strings.CutPrefix(url, "https://"); ok {
			parts := strings.Split(after, "/")
			if len(parts) >= 3 {
				return parts[1], parts[2], nil
			}
		} else if after, ok := strings.CutPrefix(url, "git@"); ok {
			parts := strings.Split(after, ":")
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

// selectTitle determines the PR title to use.
func selectTitle(ctx context.Context, p vcs.CreateParams) string {
	if strings.Contains(p.Title, "[no-changelog]") {
		return p.Title
	}
	if p.BeadID != "" && strings.Contains(p.Title, p.BeadID) {
		return p.Title
	}
	if p.BeadTitle != "" {
		return fmt.Sprintf("%s (%s)", p.BeadTitle, p.BeadID)
	}
	if subject := commitSubject(ctx, p.WorktreePath, p.Branch); subject != "" {
		return fmt.Sprintf("%s (%s)", subject, p.BeadID)
	}
	if p.Title != "" {
		return p.Title
	}
	return fmt.Sprintf("forge: %s", p.BeadID)
}

// commitSubject returns the subject line of the most recent commit on the
// given branch.
func commitSubject(ctx context.Context, worktreePath, branch string) string {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "log", branch, "--format=%s", "-1"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
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
func buildDefaultBody(p vcs.CreateParams) string {
	var b strings.Builder

	if p.ChangeSummary != "" {
		b.WriteString("## Changes\n\n")
		b.WriteString(p.ChangeSummary)
		b.WriteString("\n\n")
	}

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
