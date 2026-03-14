// Package vcs defines the VCS (Version Control System) provider interface
// that abstracts platform-specific operations (PR creation, merging, status
// checks, etc.) so Forge can work with GitHub, GitLab, Forgejo, Bitbucket,
// and Azure DevOps.
//
// The interface mirrors the operations currently performed by the ghpr package,
// which becomes the GitHub implementation of this interface.
package vcs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ForPlatform returns a Provider for the given platform.
// An empty string defaults to GitHub. Unsupported platforms return an error.
func ForPlatform(platform string) (Provider, error) {
	p, err := ParsePlatform(platform)
	if err != nil {
		return nil, err
	}
	switch p {
	case GitLab:
		return NewGitLabProvider(), nil
	default:
		return nil, fmt.Errorf("VCS provider not yet implemented for platform %q", p)
	}
}

// Platform identifies a VCS hosting platform.
type Platform string

const (
	GitHub     Platform = "github"
	GitLab    Platform = "gitlab"
	Gitea     Platform = "gitea"
	Bitbucket Platform = "bitbucket"
	AzureDevOps Platform = "azuredevops"
)

// ValidPlatforms is the set of recognised platform values.
var ValidPlatforms = map[Platform]bool{
	GitHub:      true,
	GitLab:     true,
	Gitea:      true,
	Bitbucket:  true,
	AzureDevOps: true,
}

// ParsePlatform normalises and validates a platform string.
// It trims surrounding whitespace and folds the input to lowercase before
// matching, so "GitHub", " GITLAB ", etc. are accepted.
// An empty string defaults to GitHub.
func ParsePlatform(s string) (Platform, error) {
	if s == "" {
		return GitHub, nil
	}
	p := Platform(strings.ToLower(strings.TrimSpace(s)))
	if !ValidPlatforms[p] {
		return "", fmt.Errorf("unknown VCS platform %q (valid: github, gitlab, gitea, bitbucket, azuredevops)", s)
	}
	return p, nil
}

// PR represents a created pull/merge request, independent of the hosting platform.
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

// CreateParams holds the inputs for creating a pull/merge request.
type CreateParams struct {
	// WorktreePath is the git worktree directory to run CLI commands from.
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

	// BeadTitle is the bead's human-readable title (used in the PR body).
	BeadTitle string
	// BeadDescription is the bead's problem/task description (used in the PR body).
	BeadDescription string
	// BeadType is the bead's issue type (bug, feature, task, etc.).
	BeadType string
	// ChangeSummary is a summary of what changed (from warden review or diff stat).
	ChangeSummary string
}

// PRStatus represents the platform-agnostic state of a pull/merge request.
type PRStatus struct {
	// State is the PR lifecycle state. Platforms map their native values
	// to these canonical strings: "OPEN", "MERGED", "CLOSED".
	State             string
	StatusCheckRollup []CheckRun
	Reviews           []Review
	ReviewRequests    []ReviewRequest
	// Mergeable indicates conflict state. Canonical values:
	// "MERGEABLE", "CONFLICTING", "UNKNOWN".
	Mergeable         string
	UnresolvedThreads int
	HeadRefName       string
	URL               string
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
		return true
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

// HasPendingReviewRequests returns true if there are outstanding review requests.
func (s *PRStatus) HasPendingReviewRequests() bool {
	return len(s.ReviewRequests) > 0
}

// CheckRun represents a CI check on a PR.
type CheckRun struct {
	Name       string
	Status     string
	Conclusion string
}

// ReviewAuthor identifies the author of a review.
type ReviewAuthor struct {
	Login string
}

// Review represents a PR review.
type Review struct {
	Author ReviewAuthor
	State  string
	Body   string
}

// ReviewRequest represents a pending review request on a PR.
type ReviewRequest struct {
	Login string
	Slug  string
	Name  string
}

// OpenPR is a lightweight view of a PR used for reconciliation.
type OpenPR struct {
	Number int
	Title  string
	Branch string
	Body   string
}

// MergeabilityInputs holds the computed boolean inputs for UpdatePRMergeability,
// extracted from a PRStatus.
type MergeabilityInputs struct {
	HasConflicts         bool
	HasUnresolvedThreads bool
	HasPendingReviews    bool
}

// MergeabilityFromStatus converts a PRStatus into mergeability inputs.
func MergeabilityFromStatus(s *PRStatus) MergeabilityInputs {
	return MergeabilityInputs{
		HasConflicts:         s.Mergeable == "CONFLICTING",
		HasUnresolvedThreads: s.UnresolvedThreads > 0,
		HasPendingReviews:    s.HasPendingReviewRequests(),
	}
}

// Provider is the interface that VCS platform implementations must satisfy.
// Each method corresponds to an operation Forge performs against the hosting
// platform (currently all done via the gh CLI in the ghpr package).
type Provider interface {
	// CreatePR creates a pull/merge request and returns its metadata.
	CreatePR(ctx context.Context, params CreateParams) (*PR, error)

	// MergePR merges the PR identified by prNumber using the given strategy
	// ("squash", "merge", or "rebase").
	MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error

	// CheckStatus returns the full status of a PR including CI checks,
	// reviews, unresolved threads, and mergeability.
	CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error)

	// CheckStatusLight returns a lightweight status (no unresolved thread
	// count pagination). Use when only reviewRequests/mergeable are needed.
	CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error)

	// ListOpenPRs returns all open PRs in the repository.
	ListOpenPRs(ctx context.Context, worktreePath string) ([]OpenPR, error)

	// GetRepoOwnerAndName extracts the owner and repository name from the
	// git remote. The semantics of "owner" vary by platform (org, group,
	// project namespace, etc.).
	GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error)

	// FetchUnresolvedThreadCount returns the number of unresolved review
	// threads on a PR. Platforms without thread resolution tracking should
	// return 0, nil.
	FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error)

	// FetchPendingReviewRequests returns pending review requests, including
	// bot reviewers. Platforms that don't distinguish reviewer types may
	// return the standard review request list.
	FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ReviewRequest, error)

	// Platform returns which platform this provider implements.
	Platform() Platform
}
