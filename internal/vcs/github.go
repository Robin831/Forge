package vcs

import (
	"context"

	"github.com/Robin831/Forge/internal/ghpr"
)

// GitHubProvider implements the Provider interface for GitHub using the gh CLI.
// It delegates to the existing ghpr package, converting between vcs and ghpr types.
type GitHubProvider struct{}

// NewGitHubProvider returns a new GitHub VCS provider.
func NewGitHubProvider() *GitHubProvider {
	return &GitHubProvider{}
}

// Platform returns GitHub.
func (g *GitHubProvider) Platform() Platform {
	return GitHub
}

// CreatePR creates a pull request using the gh CLI via the ghpr package.
func (g *GitHubProvider) CreatePR(ctx context.Context, params CreateParams) (*PR, error) {
	ghParams := ghpr.CreateParams{
		WorktreePath:    params.WorktreePath,
		BeadID:          params.BeadID,
		Title:           params.Title,
		Body:            params.Body,
		Branch:          params.Branch,
		Base:            params.Base,
		AnvilName:       params.AnvilName,
		Draft:           params.Draft,
		BeadTitle:       params.BeadTitle,
		BeadDescription: params.BeadDescription,
		BeadType:        params.BeadType,
		ChangeSummary:   params.ChangeSummary,
		// DB intentionally nil — state recording is handled by the pipeline layer.
	}

	ghPR, err := ghpr.Create(ctx, ghParams)
	if err != nil {
		return nil, err
	}

	return &PR{
		Number:  ghPR.Number,
		URL:     ghPR.URL,
		Title:   ghPR.Title,
		Branch:  ghPR.Branch,
		Base:    ghPR.Base,
		BeadID:  ghPR.BeadID,
		Anvil:   ghPR.Anvil,
		Created: ghPR.Created,
	}, nil
}

// MergePR merges a pull request using the gh CLI.
func (g *GitHubProvider) MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error {
	return ghpr.Merge(ctx, worktreePath, prNumber, strategy)
}

// CheckStatus returns the full status of a pull request.
func (g *GitHubProvider) CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	s, err := ghpr.CheckStatus(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}
	return convertGHPRStatus(s), nil
}

// CheckStatusLight returns a lightweight status without thread or review details.
func (g *GitHubProvider) CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	s, err := ghpr.CheckStatusLight(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}
	return convertGHPRStatus(s), nil
}

// ListOpenPRs returns all open pull requests in the repository.
func (g *GitHubProvider) ListOpenPRs(ctx context.Context, worktreePath string) ([]OpenPR, error) {
	ghPRs, err := ghpr.ListOpen(ctx, worktreePath)
	if err != nil {
		return nil, err
	}
	out := make([]OpenPR, len(ghPRs))
	for i, pr := range ghPRs {
		out[i] = OpenPR{
			Number: pr.Number,
			Title:  pr.Title,
			Branch: pr.Branch,
			Body:   pr.Body,
		}
	}
	return out, nil
}

// GetRepoOwnerAndName extracts the owner and repository name from the git remote.
func (g *GitHubProvider) GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error) {
	return ghpr.GetRepoOwnerAndName(ctx, worktreePath)
}

// FetchUnresolvedThreadCount returns the number of unresolved review threads on a PR.
func (g *GitHubProvider) FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error) {
	return ghpr.FetchUnresolvedThreadCount(ctx, worktreePath, prNumber)
}

// FetchPendingReviewRequests returns pending review requests including bot reviewers.
func (g *GitHubProvider) FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ReviewRequest, error) {
	ghRequests, err := ghpr.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}
	out := make([]ReviewRequest, len(ghRequests))
	for i, r := range ghRequests {
		out[i] = ReviewRequest{
			Login: r.Login,
			Slug:  r.Slug,
			Name:  r.Name,
		}
	}
	return out, nil
}

// convertGHPRStatus converts a ghpr.PRStatus to a vcs.PRStatus.
func convertGHPRStatus(s *ghpr.PRStatus) *PRStatus {
	status := &PRStatus{
		State:             s.State,
		Mergeable:         s.Mergeable,
		UnresolvedThreads: s.UnresolvedThreads,
		HeadRefName:       s.HeadRefName,
		URL:               s.URL,
	}
	for _, c := range s.StatusCheckRollup {
		status.StatusCheckRollup = append(status.StatusCheckRollup, CheckRun{
			Name:       c.Name,
			Status:     c.Status,
			Conclusion: c.Conclusion,
		})
	}
	for _, r := range s.Reviews {
		status.Reviews = append(status.Reviews, Review{
			Author: ReviewAuthor{Login: r.Author.Login},
			State:  r.State,
			Body:   r.Body,
		})
	}
	for _, r := range s.ReviewRequests {
		status.ReviewRequests = append(status.ReviewRequests, ReviewRequest{
			Login: r.Login,
			Slug:  r.Slug,
			Name:  r.Name,
		})
	}
	return status
}
