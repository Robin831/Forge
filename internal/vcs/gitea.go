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

// giteaRepoInfo holds pre-resolved repository coordinates to avoid redundant
// git remote lookups within a single provider call.
type giteaRepoInfo struct {
	baseURL string
	owner   string
	repo    string
}

// giteaHTTPClient is a shared HTTP client with a reasonable timeout,
// used for all Gitea API requests instead of http.DefaultClient.
var giteaHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
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
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolving Gitea repo: %w", err)
	}

	ri := giteaRepoInfo{baseURL: baseURL, owner: owner, repo: repo}

	status, headSHA, err := g.fetchPRView(ctx, ri, prNumber)
	if err != nil {
		return nil, err
	}

	// Fetch CI status from commit status API using the head SHA from fetchPRView
	ciChecks, err := g.fetchCIStatus(ctx, ri, headSHA)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch CI status for PR #%d: %v", prNumber, err)
	} else {
		status.StatusCheckRollup = ciChecks
	}

	// Fetch reviews
	reviews, err := g.fetchReviews(ctx, ri, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch reviews for PR #%d: %v", prNumber, err)
	} else {
		status.Reviews = reviews
	}

	// Fetch unresolved review comments (Gitea uses review comments, not threads)
	threadCount, err := g.fetchUnresolvedThreads(ctx, ri, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch unresolved comments for PR #%d: %v", prNumber, err)
	} else {
		status.UnresolvedThreads = threadCount
	}

	// Fetch pending review requests
	reviewRequests, err := g.fetchPendingReviews(ctx, ri, prNumber)
	if err != nil {
		log.Printf("[gitea] Warning: could not fetch review requests for PR #%d: %v", prNumber, err)
	} else {
		status.ReviewRequests = reviewRequests
	}

	return status, nil
}

// CheckStatusLight returns a lightweight status focused on mergeability and review requests.
func (g *GiteaProvider) CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	baseURL, owner, repo, err := g.resolveRepo(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolving Gitea repo: %w", err)
	}

	ri := giteaRepoInfo{baseURL: baseURL, owner: owner, repo: repo}

	status, _, err := g.fetchPRView(ctx, ri, prNumber)
	if err != nil {
		return nil, err
	}

	reviewRequests, err := g.fetchPendingReviews(ctx, ri, prNumber)
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
	return g.fetchUnresolvedThreads(ctx, giteaRepoInfo{baseURL: baseURL, owner: owner, repo: repo}, prNumber)
}

// fetchUnresolvedThreads counts unresolved review comments on a PR.
// It uses the pull review comments endpoint and counts comments where
// the resolved field is explicitly false (i.e. the comment is resolvable
// but has not been resolved).
func (g *GiteaProvider) fetchUnresolvedThreads(ctx context.Context, ri giteaRepoInfo, prNumber int) (int, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/comments?limit=50",
		ri.baseURL, url.PathEscape(ri.owner), url.PathEscape(ri.repo), prNumber)

	var comments []giteaReviewComment
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &comments); err != nil {
		return 0, fmt.Errorf("gitea fetch review comments failed: %w", err)
	}

	count := 0
	for _, c := range comments {
		// A nil Resolved pointer means the comment is not resolvable (e.g. a
		// general comment). Only count comments that are explicitly unresolved.
		if c.Resolved != nil && !*c.Resolved {
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
	return g.fetchPendingReviews(ctx, giteaRepoInfo{baseURL: baseURL, owner: owner, repo: repo}, prNumber)
}

// fetchPendingReviews is the internal implementation that accepts pre-resolved repo info.
func (g *GiteaProvider) fetchPendingReviews(ctx context.Context, ri giteaRepoInfo, prNumber int) ([]ReviewRequest, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/requested_reviewers",
		ri.baseURL, url.PathEscape(ri.owner), url.PathEscape(ri.repo), prNumber)

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
// It returns both the PRStatus and the head commit SHA (needed by fetchCIStatus).
func (g *GiteaProvider) fetchPRView(ctx context.Context, ri giteaRepoInfo, prNumber int) (*PRStatus, string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d",
		ri.baseURL, url.PathEscape(ri.owner), url.PathEscape(ri.repo), prNumber)

	var pr giteaPullRequest
	if err := giteaAPIRequest(ctx, http.MethodGet, endpoint, nil, &pr); err != nil {
		return nil, "", fmt.Errorf("gitea get PR failed: %w", err)
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
	}, pr.Head.SHA, nil
}

// fetchCIStatus retrieves CI commit status for the given head commit SHA.
// The headSHA is obtained from fetchPRView to avoid a duplicate PR fetch.
func (g *GiteaProvider) fetchCIStatus(ctx context.Context, ri giteaRepoInfo, headSHA string) ([]CheckRun, error) {
	if headSHA == "" {
		return nil, nil
	}

	// Fetch combined commit status
	statusEndpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/commits/%s/status",
		ri.baseURL, url.PathEscape(ri.owner), url.PathEscape(ri.repo), headSHA)

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
func (g *GiteaProvider) fetchReviews(ctx context.Context, ri giteaRepoInfo, prNumber int) ([]Review, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls/%d/reviews",
		ri.baseURL, url.PathEscape(ri.owner), url.PathEscape(ri.repo), prNumber)

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

type giteaReviewComment struct {
	ID       int   `json:"id"`
	Resolved *bool `json:"resolved"`
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

	resp, err := giteaHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 10 * 1024 * 1024 // 10 MB
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
// Handles port-prefixed paths from SSH URLs (e.g. "2222/owner/repo").
func splitGiteaPath(path, safeURL string) (string, string, error) {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	parts := strings.SplitN(path, "/", 4)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("could not parse Gitea remote URL (expected owner/repo): %s", safeURL)
	}

	// If the first segment is purely numeric it's an SSH port prefix (e.g. "2222/owner/repo");
	// skip it and use the next two segments as owner/repo.
	if isNumeric(parts[0]) {
		if len(parts) < 3 || parts[2] == "" {
			return "", "", fmt.Errorf("could not parse Gitea remote URL (expected owner/repo after port): %s", safeURL)
		}
		return parts[1], parts[2], nil
	}

	return parts[0], parts[1], nil
}

// isNumeric returns true if s is a non-empty string of ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// --- State mapping ---

// mapGiteaState maps Gitea PR states to canonical VCS states.
func mapGiteaState(state string) string {
	switch strings.ToLower(state) {
	case "open":
		return "OPEN"
	case "closed":
		// Gitea uses "closed" for both merged and closed PRs; the merged field
		// disambiguates. fetchPRView checks pr.Merged and overrides to "MERGED"
		// when appropriate.
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
