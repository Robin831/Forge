// Package vcs defines the VCS (Version Control System) provider interface
// that abstracts platform-specific operations (PR creation, merging, status
// checks, etc.) so Forge can work with GitHub, GitLab, Forgejo, Bitbucket,
// and Azure DevOps.
//
// The GitHub implementation lives in the vcs/github sub-package.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// ErrPRAlreadyExists is returned by CreatePR when a PR already exists for
// the given branch. Callers should use errors.Is to check for this sentinel.
var ErrPRAlreadyExists = errors.New("pull request already exists for branch")

// forgeManagedMarkerPrefix and forgeManagedMarkerSuffix bracket the per-instance
// identifier inside the HTML comment that Forge embeds in every PR body it
// creates. The full marker is `<!-- forge-managed: <id> -->`. The id is the
// forge instance identifier so that, in deployments running multiple Forge
// instances against the same anvil, each instance only adopts PRs it created
// itself instead of racing for ownership of any forge-authored PR.
const (
	forgeManagedMarkerPrefix = "<!-- forge-managed: "
	forgeManagedMarkerSuffix = " -->"
)

// legacyForgeManagedMarker is the pre-instance-id marker emitted by Forge
// versions that shipped before Forge-i1g7. It is intentionally NOT recognised
// as ours by IsForgeManagedBy: in multi-forge setups any instance could have
// created the PR, and the ambiguous marker is exactly what caused the bug we
// are fixing. Detection is only useful for migration logging.
const legacyForgeManagedMarker = "<!-- forge-managed: true -->"

// defaultForgeID is used when no instance id has been configured. It keeps
// the marker well-formed (never `<!-- forge-managed:  -->`) for code paths
// that build PR bodies without first calling SetForgeID — for example tests
// or one-shot CLI invocations.
const defaultForgeID = "default"

// currentForgeID holds the active instance id used by EnsureForgeManagedMarker
// and the PR-body builders. It is atomic.Value so the daemon can update it
// from the hot-reload watcher without racing in-flight CreatePR calls.
var currentForgeID atomic.Value

// SetForgeID stores the active forge instance id. The daemon calls this once
// at startup from settings.forge_id (falling back to the host name) and again
// from the config hot-reload watcher when the value changes.
func SetForgeID(id string) {
	if id == "" {
		id = defaultForgeID
	}
	currentForgeID.Store(id)
}

// ForgeID returns the active forge instance id, or defaultForgeID if SetForgeID
// has not been called.
func ForgeID() string {
	if v := currentForgeID.Load(); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return defaultForgeID
}

// MarkerForID returns the forge-managed marker that identifies PRs created by
// the given forge instance. Use this in tests and in PR-body builders that
// want to emit a marker for a specific instance instead of the daemon-wide one.
func MarkerForID(id string) string {
	if id == "" {
		id = defaultForgeID
	}
	return forgeManagedMarkerPrefix + id + forgeManagedMarkerSuffix
}

// EnsureForgeManagedMarker appends the forge-managed marker for the active
// instance to body if no marker for this instance is already present. Bodies
// authored by Forge must always include the marker so that reconcileOpenPRs
// can distinguish them from external PRs that happen to reference a bead ID
// AND from PRs authored by other Forge instances pointing at the same anvil.
func EnsureForgeManagedMarker(body string) string {
	marker := MarkerForID(ForgeID())
	if strings.Contains(body, marker) {
		return body
	}
	if body == "" {
		return marker
	}
	if strings.HasSuffix(body, "\n") {
		return body + marker
	}
	return body + "\n" + marker
}

// IsForgeManagedBy reports whether a PR body carries the forge-managed marker
// for the given forge instance id. Returns false for the legacy generic marker
// (`<!-- forge-managed: true -->`) — under the new multi-instance scheme there
// is no way to tell which instance authored a legacy-marked PR, so the safe
// default is "not mine". Callers must NOT infer ownership from a
// "**Bead**: <id>" reference alone — that path is what Forge-m1ui closed.
func IsForgeManagedBy(body, id string) bool {
	return strings.Contains(body, MarkerForID(id))
}

// IsLegacyForgeManaged reports whether a PR body carries the legacy
// pre-instance-id forge-managed marker. Useful for migration logging only —
// reconciliation must NOT adopt legacy-marked PRs as bellows-managed because
// in a multi-forge deployment we cannot tell which instance authored them.
func IsLegacyForgeManaged(body string) bool {
	return strings.Contains(body, legacyForgeManagedMarker)
}

// ghIssuePathPattern matches the path portion of a GitHub issue URL like
// /org/repo/issues/42
var ghIssuePathPattern = regexp.MustCompile(`^/.+/.+/issues/(\d+)$`)

// GitHubIssueNumber extracts a GitHub issue number from an external_ref value.
// Recognised formats:
//   - "gh-42"                                     → "42"
//   - "https://github.com/org/repo/issues/42"     → "42"
//
// Returns "" for non-GitHub references (e.g. "jira-123", GitLab URLs, or any
// URL whose host is not github.com), empty strings, or malformed values.
func GitHubIssueNumber(externalRef string) string {
	if externalRef == "" {
		return ""
	}
	// Shorthand: gh-<number>
	if num, ok := strings.CutPrefix(externalRef, "gh-"); ok && num != "" {
		// Validate it's all digits.
		for _, c := range num {
			if c < '0' || c > '9' {
				return ""
			}
		}
		return num
	}
	// Full URL: must be a github.com URL with path /org/repo/issues/<number>.
	u, err := url.Parse(externalRef)
	if err != nil || u.Hostname() != "github.com" {
		return ""
	}
	if m := ghIssuePathPattern.FindStringSubmatch(u.Path); len(m) == 2 {
		return m[1]
	}
	return ""
}

// closesRe matches existing "Closes #N" lines (case-insensitive) to
// avoid injecting duplicates.
var closesRe = regexp.MustCompile(`(?i)\bCloses\s+#\d+`)

// ClosesPattern returns the compiled regexp for matching "Closes #N" lines.
func ClosesPattern() *regexp.Regexp { return closesRe }

// InjectClosesLine appends a "Closes #N" line to body if the externalRef
// identifies a GitHub issue and the body does not already contain one.
// Returns the (possibly modified) body.
func InjectClosesLine(body, externalRef string) string {
	num := GitHubIssueNumber(externalRef)
	if num == "" {
		return body
	}
	if closesRe.MatchString(body) {
		return body
	}
	return body + "\n\nCloses #" + num
}

// buildPRBody creates a structured PR/MR description from bead metadata.
// This is the canonical body builder for all vcs providers; it mirrors the
// logic previously in the old ghpr package, so that GitHub and GitLab PR
// bodies stay consistent.
func buildPRBody(p CreateParams) string {
	var b strings.Builder

	// Lead with the author-written change summary (changelog fragment) when
	// available. The warden verdict is intentionally NOT rendered here — it
	// lives in a separate section below so reviewers can tell the two apart.
	if p.ChangeSummary != "" {
		b.WriteString("## Changes\n\n")
		b.WriteString(p.ChangeSummary)
		b.WriteString("\n\n")
	}

	if p.ReviewerNotes != "" {
		b.WriteString("## Reviewer's approval notes\n\n")
		b.WriteString(p.ReviewerNotes)
		b.WriteString("\n\n")
	}

	// Include the original bead description as context.
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

	// Inject Closes #N for GitHub issue references before the footer,
	// but only if the body doesn't already contain one (e.g. from Smith's
	// change summary).
	if num := GitHubIssueNumber(p.ExternalRef); num != "" && !closesRe.MatchString(b.String()) {
		fmt.Fprintf(&b, "Closes #%s\n\n", num)
	}

	// Footer
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Bead: %s | Branch: %s\n", p.BeadID, p.Branch)
	b.WriteString("Generated by [The Forge](https://github.com/Robin831/Forge) (Smith → Temper → Warden)\n")
	b.WriteString(MarkerForID(ForgeID()))

	return b.String()
}

// gitHubFactory is populated by internal/vcs/github via RegisterGitHubProvider.
// This avoids a circular import (vcs/github imports vcs for the Provider interface).
var gitHubFactory func() Provider

// RegisterGitHubProvider registers the GitHub provider factory.
// Called from init() in internal/vcs/github.
func RegisterGitHubProvider(f func() Provider) {
	gitHubFactory = f
}

// NewGitHubProvider returns a new GitHub VCS provider.
// Requires internal/vcs/github to have been imported (directly or via a
// blank import) so that its init() registers the factory.
func NewGitHubProvider() Provider {
	if gitHubFactory != nil {
		return gitHubFactory()
	}
	return nil
}

// ForPlatform returns a Provider for the given platform.
// An empty string defaults to GitHub. Unsupported platforms return an error.
// For GitHub, internal/vcs/github must be imported (e.g. via blank import) to
// register the provider factory before calling this function.
func ForPlatform(platform string) (Provider, error) {
	p, err := ParsePlatform(platform)
	if err != nil {
		return nil, err
	}
	switch p {
	case GitHub:
		prov := NewGitHubProvider()
		if prov == nil {
			return nil, fmt.Errorf("GitHub VCS provider not available: import github.com/Robin831/Forge/internal/vcs/github")
		}
		return prov, nil
	case GitLab:
		return NewGitLabProvider(), nil
	case Gitea:
		return NewGiteaProvider(), nil
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
	// ChangeSummary is the author-written summary of what changed. It is
	// rendered under the '## Changes' heading in the PR body and is sourced
	// from a parsed changelog fragment — never from the warden review verdict.
	ChangeSummary string
	// ReviewerNotes is an approval/review-speak summary produced by the Warden
	// (or any other reviewer) to be rendered under a separate heading. It is
	// intentionally kept distinct from ChangeSummary so warden verdicts do not
	// leak into the '## Changes' section when a changelog fragment is missing.
	ReviewerNotes string
	// ExternalRef is an optional external tracker reference (e.g. "gh-42" or a
	// GitHub issue URL). When it identifies a GitHub issue, buildPRBody injects
	// a "Closes #N" line so the PR auto-closes the issue on merge.
	ExternalRef string
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
	// HeadSHA is the commit OID at the head of the PR branch. Used by the
	// Assay review trigger to detect whether the current head has been
	// reviewed yet (compared against the last reviewed SHA).
	HeadSHA string `json:"headRefOid"`
	// IsDraft is true when the PR is a draft. Draft PRs are skipped by the
	// Assay trigger when skip_drafts is enabled.
	IsDraft bool `json:"isDraft"`
	URL     string
	Title   string
}

// IsMerged returns true if the PR has been merged.
func (s *PRStatus) IsMerged() bool {
	return s.State == "MERGED"
}

// IsClosed returns true if the PR has been closed without merging.
func (s *PRStatus) IsClosed() bool {
	return s.State == "CLOSED"
}

// CIsInProgress returns true if any CI check is still running (not yet completed).
// GitHub check runs use Status values like "IN_PROGRESS", "QUEUED", "PENDING",
// or "WAITING". StatusContext items use State values like "PENDING" or "EXPECTED".
func (s *PRStatus) CIsInProgress() bool {
	for _, check := range s.StatusCheckRollup {
		// Handle StatusContext items (State populated, Status empty).
		if check.State != "" && check.Status == "" {
			st := strings.ToUpper(check.State)
			if st == "PENDING" || st == "EXPECTED" {
				return true
			}
			continue // SUCCESS, FAILURE, ERROR are terminal states
		}

		st := strings.ToUpper(check.Status)
		if st == "IN_PROGRESS" || st == "QUEUED" || st == "PENDING" || st == "WAITING" || st == "REQUESTED" {
			return true
		}
		// A check marked COMPLETED but with no conclusion is in a transient
		// state — treat it as still in progress until the conclusion arrives.
		if st == "COMPLETED" && check.Conclusion == "" {
			return true
		}
		// Unknown/empty status with no conclusion is likely in progress.
		if st != "COMPLETED" && check.Conclusion == "" {
			return true
		}
	}
	return false
}

// CIsPassing returns true if all CI checks have completed with a passing result.
// Checks that are still in progress are treated as not passing (their empty
// Conclusion does not match SUCCESS/NEUTRAL/SKIPPED). Use CIsInProgress() to
// distinguish "failing" from "not yet completed".
func (s *PRStatus) CIsPassing() bool {
	if len(s.StatusCheckRollup) == 0 {
		return true
	}
	for _, check := range s.StatusCheckRollup {
		// Handle StatusContext items (State populated, Status empty).
		if check.State != "" && check.Status == "" {
			st := strings.ToUpper(check.State)
			if st != "SUCCESS" {
				return false // PENDING, EXPECTED, FAILURE, ERROR
			}
			continue
		}

		conclusion := strings.ToUpper(check.Conclusion)
		if conclusion != "SUCCESS" && conclusion != "NEUTRAL" && conclusion != "SKIPPED" {
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
//
// GitHub's statusCheckRollup returns two types: CheckRun (GitHub Actions) and
// StatusContext (legacy commit status API). They use different field schemas:
//
//   - CheckRun:      Name, Status (QUEUED/IN_PROGRESS/COMPLETED/…), Conclusion (SUCCESS/FAILURE/…)
//   - StatusContext:  Context (the check name), State (PENDING/SUCCESS/FAILURE/ERROR/EXPECTED)
//
// Both types are deserialized into this struct. Callers should use the helper
// methods (CIsInProgress, CIsPassing) which handle both schemas.
type CheckRun struct {
	Name       string
	Status     string // CheckRun: QUEUED, IN_PROGRESS, COMPLETED, WAITING, PENDING, REQUESTED
	Conclusion string // CheckRun: SUCCESS, FAILURE, NEUTRAL, SKIPPED, CANCELLED, TIMED_OUT, etc.
	// State and Context are populated for GitHub StatusContext items.
	State   string // StatusContext: PENDING, SUCCESS, FAILURE, ERROR, EXPECTED
	Context string // StatusContext: the check name (e.g., "ci/build")
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

// CICheck represents a CI check result from the platform.
type CICheck struct {
	Name   string // check name
	Status string // platform-normalized: "pass", "fail", "pending"
	Link   string // platform-specific URL to the check run details
}

// ReviewComment represents a review comment on a PR/MR.
type ReviewComment struct {
	Author   string // reviewer login
	Body     string // comment text
	Path     string // file path (empty for PR-level comments)
	Line     int    // line number (0 for PR-level comments)
	State    string // "CHANGES_REQUESTED", etc. (empty for thread comments)
	ThreadID string // platform-specific thread identifier
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
// platform (GitHub via gh CLI, GitLab via glab CLI, etc.).
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

	// GetPRByHeadBranch returns the open PR whose head branch matches the
	// given branch name, or nil if none exists. Prefer this over ListOpenPRs
	// when looking up a specific branch to avoid the 100-PR scan limit.
	GetPRByHeadBranch(ctx context.Context, worktreePath, branch string) (*OpenPR, error)

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

	// FetchPRChecks returns the CI check results for a PR. The raw string
	// is a human-readable summary suitable for inclusion in prompts; the
	// CICheck slice contains only the failing checks parsed from the output.
	FetchPRChecks(ctx context.Context, worktreePath string, prNumber int) (raw string, failing []CICheck, err error)

	// FetchCILogs retrieves CI log output for the given failing checks.
	// Returns a map of check name → log text. Checks without available
	// logs are omitted from the result.
	FetchCILogs(ctx context.Context, worktreePath string, checks []CICheck) (map[string]string, error)

	// FetchReviewComments returns review comments and unresolved threads
	// on a PR, including PR-level review comments and inline thread comments.
	FetchReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]ReviewComment, error)

	// ResolveThread marks a review thread as resolved. Platforms without
	// thread resolution support should return nil.
	ResolveThread(ctx context.Context, worktreePath string, threadID string) error

	// Platform returns which platform this provider implements.
	Platform() Platform
}

// redactURL removes userinfo (embedded credentials) from HTTP/HTTPS URLs so they
// are safe to include in log messages and errors. SSH-style URLs (git@...) do not
// embed credentials and are returned unchanged.
func redactURL(rawURL string) string {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[redacted URL]"
	}
	u.User = nil
	return u.String()
}
