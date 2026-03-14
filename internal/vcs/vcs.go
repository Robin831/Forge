// Package vcs defines the VCS provider interface for pull request operations.
//
// Forge is decoupled from any specific hosting platform through this interface.
// Each supported platform (GitHub, GitLab, Gitea/Forgejo, Bitbucket, Azure DevOps)
// implements the Provider interface in its own sub-package.
package vcs

import (
	"context"

	"github.com/Robin831/Forge/internal/ghpr"
)

// Platform identifies a VCS hosting platform.
type Platform string

const (
	PlatformGitHub      Platform = "github"
	PlatformGitLab      Platform = "gitlab"
	PlatformGitea       Platform = "gitea"
	PlatformBitbucket   Platform = "bitbucket"
	PlatformAzureDevOps Platform = "azuredevops"
)

// ReviewComment represents a review comment on a pull request.
// This is the platform-agnostic type used by the VCS interface.
type ReviewComment struct {
	Author   string // login or display name of the commenter
	Body     string
	Path     string // file path (empty for PR-level comments)
	Line     int    // line number (0 if not applicable)
	State    string // review state (e.g. "CHANGES_REQUESTED")
	ThreadID string // platform-specific thread identifier for resolving
}

// PRComment represents a review comment fetched from the platform's API,
// used for automated rule learning (e.g. from Copilot on GitHub).
type PRComment struct {
	Body     string
	User     string // login of the comment author
	Path     string // file path the comment is on
	PRNumber int
}

// Provider abstracts pull request operations for a VCS hosting platform.
//
// All methods that interact with a repository accept a worktreePath parameter
// specifying the local git checkout directory. The provider uses this to determine
// the remote repository context (owner, repo, etc.).
type Provider interface {
	// Name returns a human-readable name for the provider (e.g. "GitHub", "GitLab").
	Name() string

	// CreatePR creates a pull request and optionally records it in the state DB.
	CreatePR(ctx context.Context, p ghpr.CreateParams) (*ghpr.PR, error)

	// MergePR merges an open pull request using the given strategy.
	// Valid strategies: "squash", "merge", "rebase". Empty defaults to "squash".
	MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error

	// IsPRMerged checks whether a pull request has been merged.
	IsPRMerged(ctx context.Context, worktreePath string, prNumber int) (bool, error)

	// CheckStatus gets the full status of a PR including CI checks, reviews,
	// unresolved threads, and mergeability.
	CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*ghpr.PRStatus, error)

	// CheckStatusLight gets a lightweight status of a PR (state, review requests,
	// mergeable) without fetching expensive data like unresolved thread counts.
	CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*ghpr.PRStatus, error)

	// ListOpen returns all open PRs in the repository.
	ListOpen(ctx context.Context, worktreePath string) ([]ghpr.OpenPR, error)

	// GetRepoOwnerAndName extracts the owner and repository name from the
	// git remote origin URL.
	GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error)

	// FetchUnresolvedThreadCount returns the number of unresolved review
	// threads on a PR.
	FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error)

	// FetchPendingReviewRequests returns pending review requests on a PR,
	// including bot reviewers.
	FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ghpr.ReviewRequest, error)

	// FetchReviewComments returns review comments on a PR, including both
	// inline thread comments and PR-level review comments.
	FetchReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]ReviewComment, error)

	// ResolveReviewThread marks a review thread as resolved.
	// The threadID format is platform-specific.
	ResolveReviewThread(ctx context.Context, worktreePath string, threadID string) error

	// FetchPRChecks returns the raw CI check status output for a PR.
	// The output format is platform-specific; callers parse it accordingly.
	FetchPRChecks(ctx context.Context, worktreePath string, prNumber int) (string, error)

	// FetchCIFailureLogs returns the failure logs for a CI run.
	// The runID format is platform-specific (e.g. GitHub Actions run ID).
	FetchCIFailureLogs(ctx context.Context, worktreePath string, runID string) (string, error)

	// FetchBotReviewComments returns review comments authored by the platform's
	// automated code review bot (e.g. Copilot on GitHub, Code Suggestions on GitLab).
	FetchBotReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]PRComment, error)

	// ListMergedPRs returns the most recently merged PR numbers, up to limit.
	ListMergedPRs(ctx context.Context, worktreePath string, limit int) ([]int, error)
}

// ValidPlatform returns true if the platform string is a known VCS platform.
func ValidPlatform(p string) bool {
	switch Platform(p) {
	case PlatformGitHub, PlatformGitLab, PlatformGitea, PlatformBitbucket, PlatformAzureDevOps:
		return true
	default:
		return p == "" // empty is valid (defaults to github)
	}
}
