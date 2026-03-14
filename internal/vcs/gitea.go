package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// GiteaProvider implements the Provider interface for Gitea/Forgejo instances
// using the Gitea REST API v1. Authentication is via a personal access token
// supplied through the GITEA_TOKEN or FORGEJO_TOKEN environment variable.
type GiteaProvider struct{}

// NewGiteaProvider returns a new Gitea/Forgejo VCS provider.
func NewGiteaProvider() *GiteaProvider {
	return &GiteaProvider{}
}

// Platform returns Gitea.
func (g *GiteaProvider) Platform() Platform {
	return Gitea
}

// giteaToken returns the API token from GITEA_TOKEN or FORGEJO_TOKEN.
func giteaToken() string {
	if t := os.Getenv("GITEA_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("FORGEJO_TOKEN")
}

// CreatePR creates a pull request using the Gitea API.
func (g *GiteaProvider) CreatePR(ctx context.Context, params CreateParams) (*PR, error) {
	if params.Base == "" {
		params.Base = "main"
	}
	if params.Title == "" {
		params.Title = fmt.Sprintf("forge: %s", params.BeadID)
	}
	if params.Body == "" {
		params.Body = buildPRBody(params)
	}

	baseURL, owner, repo, err := g.resolveRepo(ctx, params.WorktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolving Gitea repo: %w", err)
	}

	payload := giteaCreatePRRequest{
		Title: params.Title,
		Body:  params.Body,
		Head:  params.Branch,
		Base:  params.Base,
	}

	log.Printf("[gitea] Creating PR for %s on branch %s", params.BeadID, params.Branch)

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls", baseURL, url.PathEscape(owner), url.PathEscape(repo))
	var result giteaPullRequest
	if err := giteaAPIRequest(ctx, http.MethodPost, endpoint, payload, &result); err != nil {
		return nil, fmt.Errorf("gitea create PR failed: %w", err)
	}

	log.Printf("[gitea] Created PR #%d: %s", result.Number, result.HTMLURL)

	return &PR{
		Number:  result.Number,
		URL:     result.HTMLURL,
		Title:   params.Title,
		Branch:  params.Branch,
		Base:    params.Base,
		BeadID:  params.BeadID,
		Anvil:   params.AnvilName,
		Created: time.Now(),
	}, nil
}

// MergePR merges a pull request using the Gitea API.
// Valid strategies: "squash", "merge", "rebase". Defaults to "squash" if empty.
func (g *GiteaProvider) MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error {
	if strategy == "" {
		strategy = "squash"
	}

	allowedStrategies := map[string]bool{
		"squash": true,
		"merge":  true,
		"rebase": true,
	}
	if !allowedStrategies[strategy] {
		log.Printf("[gitea] Invalid merge strategy %q, defaulting to squash", strategy)
		strategy = "squash"
	}

	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return fmt.Errorf("resolving Gitea repo: %w", err)
	}

	log.Printf("[gitea] Merging PR #%d with strategy %s", prNumber, strategy)

	payload := giteaMergePRRequest{
		Do:                    strategy,
		DeleteBranchAfterMerge: true,
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/merge",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)
	if err := giteaAPIRequest(ctx, http.MethodPost, endpoint, payload, nil); err != nil {
		return fmt.Errorf("gitea merge PR failed: %w", err)
	}

	log.Printf("[gitea] Merged PR #%d", prNumber)
	return nil
}

// CheckStatus returns the full status of a pull request.
func (g *GiteaProvider) CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	status, err := g.fetchPRView(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}

	// Fetch CI status from commit status API
	ciChecks, err := g.fetchCIStatus(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch CI status for PR #%d: %v", prNumber, err)
	} else {
		status.StatusCheckRollup = ciChecks
	}

	// Fetch reviews
	reviews, err := g.fetchReviews(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch reviews for PR #%d: %v", prNumber, err)
	} else {
		status.Reviews = reviews
	}

	// Fetch unresolved review comments (Gitea uses review comments, not threads)
	threadCount, err := g.FetchUnresolvedThreadCount(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch unresolved comments for PR #%d: %v", prNumber, err)
	} else {
		status.UnresolvedThreads = threadCount
	}

	// Fetch pending review requests
	reviewRequests, err := g.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch review requests for PR #%d: %v", prNumber, err)
	} else {
		status.ReviewRequests = reviewRequests
	}

	return status, nil
}

// CheckStatusLight returns a lightweight status focused on mergeability and review requests.
func (g *GiteaProvider) CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	status, err := g.fetchPRView(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}

	reviewRequests, err := g.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch review requests for PR #%d: %v", prNumber, err)
	} else {
		status.ReviewRequests = reviewRequests
	}

	return status, nil
}

// ListOpenPRs returns all open pull requests in the repository.
func (g *GiteaProvider) ListOpenPRs(ctx context.Context, worktreePath string) ([]OpenPR, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolving Gitea repo: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls?state=open&limit=50",
		baseURL, url.PathEscape(owner), url.PathEscape(repo))

	var prs []giteaPullRequest
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &prs); err != nil {
		return nil, fmt.Errorf("gitea list PRs failed: %w", err)
	}

	out := make([]OpenPR, len(prs))
	for i, pr := range prs {
		out[i] = OpenPR{
			Number: pr.Number,
			Title:  pr.Title,
			Branch: pr.Head.Ref,
			Body:   pr.Body,
		}
	}
	return out, nil
}

// GetRepoOwnerAndName extracts the owner and repository name from the git remote.
func (g *GiteaProvider) GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git remote get-url origin: %w\nstderr: %s", err, stderr.String())
	}

	rawURL := strings.TrimSpace(stdout.String())
	_, o, r, parseErr := ParseGiteaRepoURL(rawURL)
	return o, r, parseErr
}

// FetchUnresolvedThreadCount returns the number of unresolved review comments on a PR.
// Gitea tracks review comments rather than threaded discussions. We count
// review comments that have not been resolved.
func (g *GiteaProvider) FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return 0, err
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/reviews",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)

	var reviews []giteaReview
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &reviews); err != nil {
		return 0, fmt.Errorf("gitea fetch reviews failed: %w", err)
	}

	count := 0
	for _, r := range reviews {
		if r.State == "REQUEST_CHANGES" {
			count++
		}
	}
	return count, nil
}

// FetchPendingReviewRequests returns pending review requests for a PR.
func (g *GiteaProvider) FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ReviewRequest, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/requested_reviewers",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)

	var result giteaRequestedReviewers
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
		// Not all Gitea instances support this endpoint; return empty on error.
		log.Printf("[gitea] Warning: could not fetch requested reviewers for PR #%d: %v", prNumber, err)
		return nil, nil
	}

	var requests []ReviewRequest
	for _, u := range result.Users {
		requests = append(requests, ReviewRequest{
			Login: u.Login,
			Name:  u.FullName,
		})
	}
	for _, team := range result.Teams {
		requests = append(requests, ReviewRequest{
			Slug: strconv.FormatInt(team.ID, 10),
			Name: team.Name,
		})
	}
	return requests, nil
}

// resolveRepo gets the base URL, owner, and repo from the git remote.
func (g *GiteaProvider) resolveRepo(ctx context.Context, worktreePath string) (baseURL, owner, repo string, err error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", "", "", fmt.Errorf("git remote get-url origin: %w\nstderr: %s", err, stderr.String())
	}

	rawURL := strings.TrimSpace(stdout.String())
	return ParseGiteaRepoURL(rawURL)
}

// fetchPRView retrieves pull request details via the Gitea API.
func (g *GiteaProvider) fetchPRView(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)

	var pr giteaPullRequest
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return nil, fmt.Errorf("gitea get PR failed: %w", err)
	}

	state := mapGiteaState(pr.State)
	if pr.Merged {
		state = "MERGED"
	}

	return &PRStatus{
		State:       state,
		Mergeable:   mapGiteaMergeable(pr.Mergeable, pr.State),
		HeadRefName: pr.Head.Ref,
		URL:         pr.HTMLURL,
	}, nil
}

// fetchCIStatus retrieves CI commit status for the PR's head commit.
func (g *GiteaProvider) fetchCIStatus(ctx context.Context, worktreePath string, prNumber int) ([]CheckRun, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	// First get the PR to find the head SHA
	prEndpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)

	var pr giteaPullRequest
	if err := giteaAPIRequest(ctx, http.MethodGet, prEndpoint, nil, &pr); err != nil {
		return nil, err
	}

	if pr.Head.SHA == "" {
		return nil, nil
	}

	// Fetch combined commit status
	statusEndpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/commits/%s/status",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), pr.Head.SHA)

	var combined giteaCombinedStatus
	if err := giteaAPIRequest(ctx, http.MethodGet, statusEndpoint, nil, &combined); err != nil {
		return nil, err
	}

	var checks []CheckRun
	for _, s := range combined.Statuses {
		checks = append(checks, CheckRun{
			Name:       s.Context,
			Status:     mapGiteaStatusState(s.Status),
			Conclusion: mapGiteaStatusConclusion(s.Status),
		})
	}
	return checks, nil
}

// fetchReviews retrieves review information for a pull request.
func (g *GiteaProvider) fetchReviews(ctx context.Context, worktreePath string, prNumber int) ([]Review, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/reviews",
		baseURL, url.PathEscape(owner), url.PathEscape(repo), prNumber)

	var giteaReviews []giteaReview
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &giteaReviews); err != nil {
		return nil, err
	}

	var reviews []Review
	for _, r := range giteaReviews {
		state := mapGiteaReviewState(r.State)
		if state == "" {
			continue // skip comment-only reviews
		}
		reviews = append(reviews, Review{
			Author: ReviewAuthor{Login: r.User.Login},
			State:  state,
			Body:   r.Body,
		})
	}
	return reviews, nil
}

// --- Gitea API types ---

type giteaCreatePRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

type giteaMergePRRequest struct {
	Do                     string `json:"Do"`
	DeleteBranchAfterMerge bool   `json:"delete_branch_after_merge"`
}

type giteaPullRequest struct {
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	HTMLURL   string      `json:"html_url"`
	Mergeable bool        `json:"mergeable"`
	Merged    bool        `json:"merged"`
	Head      giteaBranch `json:"head"`
	Base      giteaBranch `json:"base"`
}

type giteaBranch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type giteaReview struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
	State string `json:"state"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

type giteaRequestedReviewers struct {
	Users []struct {
		Login    string `json:"login"`
		FullName string `json:"full_name"`
	} `json:"users"`
	Teams []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"teams"`
}

type giteaCombinedStatus struct {
	State    string         `json:"state"`
	Statuses []giteaStatus `json:"statuses"`
}

type giteaStatus struct {
	Context string `json:"context"`
	Status  string `json:"status"`
}

// --- Gitea API helpers ---

// giteaAPIRequest performs an HTTP request to the Gitea API.
func giteaAPIRequest(ctx context.Context, method, endpoint string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if token := giteaToken(); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}
	}

	return nil
}

// --- URL parsing ---

// ParseGiteaRepoURL parses a git remote URL into base URL, owner, and repo name.
// Supports both HTTPS and SSH URLs for Gitea/Forgejo instances.
//
// HTTPS: https://gitea.example.com/owner/repo.git → ("https://gitea.example.com", "owner", "repo")
// SSH:   git@gitea.example.com:owner/repo.git     → ("https://gitea.example.com", "owner", "repo")
func ParseGiteaRepoURL(rawURL string) (baseURL, owner, repo string, err error) {
	rawURL = strings.TrimSuffix(rawURL, ".git")
	safeURL := redactURL(rawURL)

	// SSH: git@host:owner/repo or git@host:port/owner/repo
	if strings.HasPrefix(rawURL, "git@") {
		parts := strings.SplitN(rawURL, ":", 2)
		if len(parts) != 2 {
			return "", "", "", fmt.Errorf("could not parse Gitea SSH URL: %s", safeURL)
		}
		host := strings.TrimPrefix(parts[0], "git@")
		path := parts[1]

		// Handle ssh port in path (e.g., "2222/owner/repo")
		path = strings.TrimPrefix(path, "/")

		owner, repo, err = splitGiteaPath(path, safeURL)
		if err != nil {
			return "", "", "", err
		}
		return "https://" + host, owner, repo, nil
	}

	// HTTPS: https://host/owner/repo
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		u, parseErr := url.Parse(rawURL)
		if parseErr != nil {
			return "", "", "", fmt.Errorf("could not parse Gitea URL: %s", safeURL)
		}
		scheme := u.Scheme
		host := u.Host
		path := strings.TrimPrefix(u.Path, "/")
		path = strings.TrimSuffix(path, "/")

		owner, repo, err = splitGiteaPath(path, safeURL)
		if err != nil {
			return "", "", "", err
		}
		return scheme + "://" + host, owner, repo, nil
	}

	return "", "", "", fmt.Errorf("could not parse Gitea remote URL: %s", safeURL)
}

// splitGiteaPath splits "owner/repo" from a URL path.
// Unlike GitLab, Gitea uses flat owner/repo (no nested groups).
func splitGiteaPath(path, safeURL string) (string, string, error) {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("could not parse Gitea remote URL (expected owner/repo): %s", safeURL)
	}
	// Gitea uses owner/repo; if there's a third segment it's likely a port prefix — use last two.
	// Standard case: owner/repo
	return parts[0], parts[1], nil
}

// --- State mapping ---

// mapGiteaState maps Gitea PR states to canonical VCS states.
func mapGiteaState(state string) string {
	switch strings.ToLower(state) {
	case "open":
		return "OPEN"
	case "closed":
		// Gitea uses "closed" for both merged and closed PRs; the merged field
		// disambiguates, but at the state level we treat it as CLOSED. Callers
		// should check the merged flag separately when needed — fetchPRView
		// doesn't currently surface it, but the canonical flow handles this.
		return "CLOSED"
	default:
		return strings.ToUpper(state)
	}
}

// mapGiteaMergeable maps Gitea's mergeable boolean to canonical values.
func mapGiteaMergeable(mergeable bool, state string) string {
	if strings.ToLower(state) != "open" {
		return "UNKNOWN"
	}
	if mergeable {
		return "MERGEABLE"
	}
	return "CONFLICTING"
}

// mapGiteaReviewState maps Gitea review states to canonical VCS review states.
func mapGiteaReviewState(state string) string {
	switch strings.ToUpper(state) {
	case "APPROVED":
		return "APPROVED"
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	case "COMMENT":
		return "" // not a formal review decision
	case "REJECTED":
		return "CHANGES_REQUESTED"
	default:
		return ""
	}
}

// mapGiteaStatusState maps Gitea commit status states to normalized status strings.
func mapGiteaStatusState(status string) string {
	switch strings.ToLower(status) {
	case "success", "failure", "error":
		return "COMPLETED"
	case "pending":
		return "IN_PROGRESS"
	default:
		return strings.ToUpper(status)
	}
}

// mapGiteaStatusConclusion maps Gitea commit status states to canonical conclusions.
func mapGiteaStatusConclusion(status string) string {
	switch strings.ToLower(status) {
	case "success":
		return "SUCCESS"
	case "failure":
		return "FAILURE"
	case "error":
		return "FAILURE"
	case "pending":
		return "" // not concluded yet
	default:
		return strings.ToUpper(status)
	}
}
