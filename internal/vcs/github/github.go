// Package github implements the vcs.Provider interface for GitHub using the
// gh CLI and GitHub GraphQL API.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/vcs"
)

// Provider implements vcs.Provider for GitHub repositories.
type Provider struct{}

// New returns a new GitHub VCS provider.
func New() *Provider {
	return &Provider{}
}

func (p *Provider) Name() string { return "GitHub" }

func (p *Provider) CreatePR(ctx context.Context, params ghpr.CreateParams) (*ghpr.PR, error) {
	return ghpr.Create(ctx, params)
}

func (p *Provider) MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error {
	return ghpr.Merge(ctx, worktreePath, prNumber, strategy)
}

func (p *Provider) IsPRMerged(ctx context.Context, worktreePath string, prNumber int) (bool, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "view",
		fmt.Sprintf("%d", prNumber), "--json", "state", "--jq", ".state"))
	cmd.Dir = worktreePath

	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "MERGED", nil
}

func (p *Provider) CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*ghpr.PRStatus, error) {
	return ghpr.CheckStatus(ctx, worktreePath, prNumber)
}

func (p *Provider) CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*ghpr.PRStatus, error) {
	return ghpr.CheckStatusLight(ctx, worktreePath, prNumber)
}

func (p *Provider) ListOpen(ctx context.Context, worktreePath string) ([]ghpr.OpenPR, error) {
	return ghpr.ListOpen(ctx, worktreePath)
}

func (p *Provider) GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error) {
	return ghpr.GetRepoOwnerAndName(ctx, worktreePath)
}

func (p *Provider) FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error) {
	return ghpr.FetchUnresolvedThreadCount(ctx, worktreePath, prNumber)
}

func (p *Provider) FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ghpr.ReviewRequest, error) {
	return ghpr.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
}

// FetchReviewComments returns unresolved review comments on a PR via GraphQL
// and PR-level review comments via gh pr view.
func (p *Provider) FetchReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]vcs.ReviewComment, error) {
	owner, repo, err := ghpr.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("getting repo owner and name: %w", err)
	}

	// 1. Fetch unresolved threads via GraphQL (paginated).
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

	var comments []vcs.ReviewComment
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
				comments = append(comments, vcs.ReviewComment{
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

	// 2. Fetch PR-level reviews via gh pr view.
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
		for _, r := range prData.Reviews {
			if r.State != "APPROVED" && r.State != "DISMISSED" && len(r.Body) > 20 {
				comments = append(comments, vcs.ReviewComment{
					Author: r.Author.Login,
					Body:   r.Body,
					State:  r.State,
				})
			}
		}
		for _, c := range prData.Comments {
			if len(c.Body) > 20 {
				comments = append(comments, vcs.ReviewComment{
					Author: c.Author.Login,
					Body:   c.Body,
				})
			}
		}
	}

	return comments, nil
}

// ResolveReviewThread marks a review thread as resolved via GraphQL mutation.
func (p *Provider) ResolveReviewThread(ctx context.Context, worktreePath string, threadID string) error {
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

// FetchPRChecks returns the raw CI check output for a PR via `gh pr checks`.
func (p *Provider) FetchPRChecks(ctx context.Context, worktreePath string, prNumber int) (string, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "checks", fmt.Sprintf("%d", prNumber)))
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// FetchCIFailureLogs returns the failed job logs for a GitHub Actions run.
func (p *Provider) FetchCIFailureLogs(ctx context.Context, worktreePath string, runID string) (string, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "run", "view", runID, "--log-failed"))
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

// copilotBotLogins are the GitHub bot accounts that produce automated code
// review comments (Copilot code review, GitHub Actions bots).
var copilotBotLogins = map[string]bool{
	"copilot[bot]":         true,
	"github-actions[bot]":  true,
	"copilot":              true,
}

// ghReviewComment is the JSON shape returned by `gh api` for PR review comments.
type ghReviewComment struct {
	Body string `json:"body"`
	Path string `json:"path"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// FetchBotReviewComments returns review comments authored by Copilot and other
// GitHub code review bots via the REST API.
func (p *Provider) FetchBotReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]vcs.PRComment, error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber)
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "api", endpoint, "--paginate"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	raw, err := parsePaginatedComments(stdout.Bytes())
	if err != nil {
		return nil, err
	}

	var comments []vcs.PRComment
	for _, c := range raw {
		login := strings.ToLower(c.User.Login)
		if copilotBotLogins[login] {
			comments = append(comments, vcs.PRComment{
				Body:     c.Body,
				User:     c.User.Login,
				Path:     c.Path,
				PRNumber: prNumber,
			})
		}
	}
	return comments, nil
}

// parsePaginatedComments decodes stdout from `gh api --paginate`, which
// concatenates one JSON array per page (e.g. [page1...][page2...]).
func parsePaginatedComments(data []byte) ([]ghReviewComment, error) {
	var all []ghReviewComment
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var page []ghReviewComment
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing gh response: %w", err)
		}
		all = append(all, page...)
	}
	return all, nil
}

// ListMergedPRs returns the most recently merged PR numbers.
func (p *Provider) ListMergedPRs(ctx context.Context, worktreePath string, limit int) ([]int, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "list",
		"--state=merged", "--limit", fmt.Sprintf("%d", limit), "--json=number"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr list: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("parsing PR list: %w", err)
	}

	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums, nil
}

// Compile-time interface check.
var _ vcs.Provider = (*Provider)(nil)
