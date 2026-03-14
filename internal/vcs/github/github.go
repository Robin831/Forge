// Package github implements the vcs.Provider interface for GitHub using the
// gh CLI and GitHub GraphQL API.
package github

import (
	"context"

	"github.com/Robin831/Forge/internal/ghpr"
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
