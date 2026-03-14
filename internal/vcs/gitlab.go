package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// GitLabProvider implements the Provider interface for GitLab using the glab CLI.
type GitLabProvider struct{}

// NewGitLabProvider returns a new GitLab VCS provider.
func NewGitLabProvider() *GitLabProvider {
	return &GitLabProvider{}
}

// Platform returns GitLab.
func (g *GitLabProvider) Platform() Platform {
	return GitLab
}

// CreatePR creates a merge request using the glab CLI.
func (g *GitLabProvider) CreatePR(ctx context.Context, params CreateParams) (*PR, error) {
	if params.Base == "" {
		params.Base = "main"
	}

	if params.Title == "" {
		params.Title = fmt.Sprintf("forge: %s", params.BeadID)
	}

	if params.Body == "" {
		params.Body = buildPRBody(params)
	}

	args := []string{
		"mr", "create",
		"--title", params.Title,
		"--description", params.Body,
		"--target-branch", params.Base,
		"--source-branch", params.Branch,
		"--no-editor",
		"--yes",
	}
	if params.Draft {
		args = append(args, "--draft")
	}

	log.Printf("[gitlab] Creating MR for %s on branch %s", params.BeadID, params.Branch)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", args...))
	cmd.Dir = params.WorktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab mr create failed: %w\nstderr: %s", err, stderr.String())
	}

	output := stdout.String()
	mrURL := extractGlabURL(output)
	mrNumber := extractMRNumber(mrURL)

	log.Printf("[gitlab] Created MR: %s", mrURL)

	return &PR{
		Number:  mrNumber,
		URL:     mrURL,
		Title:   params.Title,
		Branch:  params.Branch,
		Base:    params.Base,
		BeadID:  params.BeadID,
		Anvil:   params.AnvilName,
		Created: time.Now(),
	}, nil
}

// MergePR merges a merge request using the glab CLI.
// Valid strategies: "squash", "merge", "rebase". Defaults to "squash" if empty.
func (g *GitLabProvider) MergePR(ctx context.Context, worktreePath string, prNumber int, strategy string) error {
	if strategy == "" {
		strategy = "squash"
	}

	allowedStrategies := map[string]bool{
		"squash": true,
		"merge":  true,
		"rebase": true,
	}
	if !allowedStrategies[strategy] {
		log.Printf("[gitlab] Invalid merge strategy %q, defaulting to squash", strategy)
		strategy = "squash"
	}

	args := []string{
		"mr", "merge", strconv.Itoa(prNumber),
		"--yes",
		"--remove-source-branch",
	}

	// GitLab supports squash via --squash flag; rebase via --rebase.
	// "merge" is the default for glab and requires no extra flag.
	switch strategy {
	case "squash":
		args = append(args, "--squash")
	case "rebase":
		args = append(args, "--rebase")
	}

	log.Printf("[gitlab] Merging MR !%d with strategy %s", prNumber, strategy)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", args...))
	cmd.Dir = worktreePath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("glab mr merge failed: %w\nstderr: %s", err, stderr.String())
	}

	log.Printf("[gitlab] Merged MR !%d", prNumber)
	return nil
}

// glabMRStatus is the JSON structure returned by glab mr view --output json.
type glabMRStatus struct {
	IID            int    `json:"iid"`
	State          string `json:"state"`
	MergeStatus    string `json:"merge_status"`
	HasConflicts   bool   `json:"has_conflicts"`
	HeadPipeline   *glabPipeline `json:"head_pipeline"`
	WebURL         string `json:"web_url"`
	SourceBranch   string `json:"source_branch"`
	Draft          bool   `json:"draft"`
}

// glabPipeline represents a GitLab CI pipeline status.
type glabPipeline struct {
	Status string    `json:"status"`
	Jobs   []glabJob `json:"jobs"`
}

// glabJob represents a single CI job in a pipeline.
type glabJob struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// glabApproval is the JSON structure returned by the approvals API.
type glabApproval struct {
	Approved     bool `json:"approved"`
	ApprovalRule []struct {
		Approved bool `json:"approved"`
	} `json:"approval_rules_left"`
	ApprovedBy []struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	} `json:"approved_by"`
}

// glabNote represents a discussion note (comment) on a merge request.
type glabNote struct {
	ID         int    `json:"id"`
	Body       string `json:"body"`
	System     bool   `json:"system"`
	Resolvable bool   `json:"resolvable"`
	Resolved   bool   `json:"resolved"`
	Author     struct {
		Username string `json:"username"`
	} `json:"author"`
	Type string `json:"type"`
}

// CheckStatus returns the full status of a merge request.
func (g *GitLabProvider) CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	status, err := g.fetchMRView(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}

	// Fetch unresolved thread count
	threadCount, err := g.FetchUnresolvedThreadCount(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitlab] Warning: could not fetch unresolved thread count for MR !%d: %v", prNumber, err)
	} else {
		status.UnresolvedThreads = threadCount
	}

	// Fetch approval state
	reviews, err := g.fetchApprovals(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitlab] Warning: could not fetch approvals for MR !%d: %v", prNumber, err)
	} else {
		status.Reviews = reviews
	}

	// Fetch pending review requests (unapproved approval rules)
	reviewRequests, err := g.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitlab] Warning: could not fetch pending review requests for MR !%d: %v", prNumber, err)
	} else {
		status.ReviewRequests = reviewRequests
	}

	return status, nil
}

// CheckStatusLight returns a lightweight status focused on reviewRequests and mergeability.
// It fetches pending review requests in addition to the basic MR view, since callers
// (e.g. immediately after PR creation) rely on ReviewRequests being populated.
func (g *GitLabProvider) CheckStatusLight(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	status, err := g.fetchMRView(ctx, worktreePath, prNumber)
	if err != nil {
		return nil, err
	}

	reviewRequests, err := g.FetchPendingReviewRequests(ctx, worktreePath, prNumber)
	if err != nil {
		log.Printf("[gitlab] Warning: could not fetch pending review requests for MR !%d: %v", prNumber, err)
	} else {
		status.ReviewRequests = reviewRequests
	}

	return status, nil
}

// ListOpenPRs returns all open merge requests in the repository.
func (g *GitLabProvider) ListOpenPRs(ctx context.Context, worktreePath string) ([]OpenPR, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "mr", "list",
		"--state", "opened",
		"--output", "json",
		"--per-page", "100",
	))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab mr list failed: %w\nstderr: %s", err, stderr.String())
	}

	var raw []struct {
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		Description  string `json:"description"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parsing mr list: %w", err)
	}

	out := make([]OpenPR, len(raw))
	for i, r := range raw {
		out[i] = OpenPR{
			Number: r.IID,
			Title:  r.Title,
			Branch: r.SourceBranch,
			Body:   r.Description,
		}
	}
	return out, nil
}

// GetRepoOwnerAndName extracts the namespace (group/subgroup) and project name from the git remote.
func (g *GitLabProvider) GetRepoOwnerAndName(ctx context.Context, worktreePath string) (owner, repo string, err error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("git remote get-url origin: %w\nstderr: %s", err, stderr.String())
	}

	url := strings.TrimSpace(stdout.String())
	return ParseGitLabRepoURL(url)
}

// FetchUnresolvedThreadCount returns the number of unresolved discussion threads on a MR.
func (g *GitLabProvider) FetchUnresolvedThreadCount(ctx context.Context, worktreePath string, prNumber int) (int, error) {
	owner, repo, err := g.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return 0, err
	}

	// Use glab api to fetch discussions. The project path is owner/repo (or nested groups).
	projectPath := owner + "/" + repo
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/discussions", urlEncode(projectPath), prNumber)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "api", endpoint, "--per-page", "100"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("glab api discussions failed: %w\nstderr: %s", err, stderr.String())
	}

	var discussions []struct {
		Notes []glabNote `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &discussions); err != nil {
		return 0, fmt.Errorf("parsing discussions: %w", err)
	}

	count := 0
	for _, d := range discussions {
		for _, note := range d.Notes {
			if note.Resolvable && !note.Resolved {
				count++
				break // count each discussion once
			}
		}
	}
	return count, nil
}

// FetchPendingReviewRequests returns pending review requests for a MR.
// GitLab uses approvers/approval rules rather than explicit review requests.
// We map unapproved required approvers to review requests.
func (g *GitLabProvider) FetchPendingReviewRequests(ctx context.Context, worktreePath string, prNumber int) ([]ReviewRequest, error) {
	owner, repo, err := g.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	projectPath := owner + "/" + repo
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/approval_state", urlEncode(projectPath), prNumber)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "api", endpoint))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Not all GitLab instances have approval features; return empty on error.
		log.Printf("[gitlab] Warning: could not fetch approval state for MR !%d: %v", prNumber, err)
		return nil, nil
	}

	var approvalState struct {
		Rules []struct {
			Name              string `json:"name"`
			Approved          bool   `json:"approved"`
			EligibleApprovers []struct {
				Username string `json:"username"`
				Name     string `json:"name"`
			} `json:"eligible_approvers"`
		} `json:"rules"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &approvalState); err != nil {
		return nil, fmt.Errorf("parsing approval state: %w", err)
	}

	var requests []ReviewRequest
	for _, rule := range approvalState.Rules {
		if rule.Approved {
			continue
		}
		for _, approver := range rule.EligibleApprovers {
			requests = append(requests, ReviewRequest{
				Login: approver.Username,
				Name:  approver.Name,
			})
		}
	}
	return requests, nil
}

// FetchPRChecks returns CI check results for a merge request by inspecting
// the head pipeline jobs.
func (g *GitLabProvider) FetchPRChecks(ctx context.Context, worktreePath string, prNumber int) (string, []CICheck, error) {
	status, err := g.fetchMRView(ctx, worktreePath, prNumber)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	var failing []CICheck
	for _, check := range status.StatusCheckRollup {
		fmt.Fprintf(&b, "%s\t%s\n", check.Name, check.Conclusion)
		if check.Conclusion == "FAILURE" {
			failing = append(failing, CICheck{
				Name:   check.Name,
				Status: "fail",
			})
		}
	}
	return b.String(), failing, nil
}

// FetchCILogs fetches CI job logs from GitLab pipelines for failing checks.
func (g *GitLabProvider) FetchCILogs(ctx context.Context, worktreePath string, checks []CICheck) (map[string]string, error) {
	if len(checks) == 0 {
		return nil, nil
	}

	owner, repo, err := g.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	projectPath := owner + "/" + repo
	result := make(map[string]string)

	for _, check := range checks {
		// GitLab job logs are fetched by job name from the pipeline.
		// Try to find the job ID via the jobs API, then fetch its trace.
		endpoint := fmt.Sprintf("projects/%s/jobs?scope[]=failed&per_page=20", urlEncode(projectPath))

		cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "api", endpoint))
		cmd.Dir = worktreePath

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.Printf("[gitlab] Warning: failed to list failed jobs: %v", err)
			continue
		}

		var jobs []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &jobs); err != nil {
			log.Printf("[gitlab] Warning: failed to parse jobs response: %v", err)
			continue
		}

		for _, job := range jobs {
			if job.Name != check.Name {
				continue
			}
			traceEndpoint := fmt.Sprintf("projects/%s/jobs/%d/trace", urlEncode(projectPath), job.ID)
			traceCmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "api", traceEndpoint))
			traceCmd.Dir = worktreePath

			var traceOut, traceErr bytes.Buffer
			traceCmd.Stdout = &traceOut
			traceCmd.Stderr = &traceErr

			if err := traceCmd.Run(); err != nil {
				log.Printf("[gitlab] Warning: failed to fetch job %d trace: %v", job.ID, err)
				continue
			}
			result[check.Name] = traceOut.String()
			break
		}
	}

	return result, nil
}

// FetchReviewComments returns unresolved discussion threads and MR-level notes.
func (g *GitLabProvider) FetchReviewComments(ctx context.Context, worktreePath string, prNumber int) ([]ReviewComment, error) {
	owner, repo, err := g.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	projectPath := owner + "/" + repo
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/discussions", urlEncode(projectPath), prNumber)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "api", endpoint, "--per-page", "100"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab api discussions failed: %w\nstderr: %s", err, stderr.String())
	}

	var discussions []struct {
		ID    string     `json:"id"`
		Notes []glabNote `json:"notes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &discussions); err != nil {
		return nil, fmt.Errorf("parsing discussions: %w", err)
	}

	var comments []ReviewComment
	for _, d := range discussions {
		for _, note := range d.Notes {
			if note.System {
				continue
			}
			if note.Resolvable && note.Resolved {
				continue
			}
			if note.Body == "" {
				continue
			}
			comments = append(comments, ReviewComment{
				Author:   note.Author.Username,
				Body:     note.Body,
				ThreadID: d.ID,
			})
			break // one comment per discussion
		}
	}
	return comments, nil
}

// ResolveThread resolves a discussion thread on a GitLab MR.
func (g *GitLabProvider) ResolveThread(ctx context.Context, worktreePath string, threadID string) error {
	// GitLab resolves threads via the discussions API. We need the project
	// path and MR IID, but the threadID alone doesn't carry them. For now
	// this is a best-effort no-op; callers that need resolution should use
	// the platform-specific API directly until we thread MR context through.
	log.Printf("[gitlab] ResolveThread not yet fully implemented for thread %s", threadID)
	return nil
}

// fetchMRView retrieves merge request details via glab mr view.
func (g *GitLabProvider) fetchMRView(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "mr", "view",
		strconv.Itoa(prNumber),
		"--output", "json",
	))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab mr view failed: %w\nstderr: %s", err, stderr.String())
	}

	var mr glabMRStatus
	if err := json.Unmarshal(stdout.Bytes(), &mr); err != nil {
		return nil, fmt.Errorf("parsing mr status: %w", err)
	}

	status := &PRStatus{
		State:       mapGitLabState(mr.State),
		Mergeable:   mapGitLabMergeable(mr.MergeStatus, mr.HasConflicts),
		HeadRefName: mr.SourceBranch,
		URL:         mr.WebURL,
	}

	// Map pipeline status to CI check runs
	if mr.HeadPipeline != nil {
		if len(mr.HeadPipeline.Jobs) > 0 {
			for _, job := range mr.HeadPipeline.Jobs {
				status.StatusCheckRollup = append(status.StatusCheckRollup, CheckRun{
					Name:       job.Name,
					Status:     mapGitLabJobStatus(job.Status),
					Conclusion: mapGitLabJobConclusion(job.Status),
				})
			}
		} else {
			// No individual jobs, use pipeline-level status
			status.StatusCheckRollup = []CheckRun{
				{
					Name:       "pipeline",
					Status:     mapGitLabJobStatus(mr.HeadPipeline.Status),
					Conclusion: mapGitLabJobConclusion(mr.HeadPipeline.Status),
				},
			}
		}
	}

	return status, nil
}

// fetchApprovals retrieves approval information for a merge request.
func (g *GitLabProvider) fetchApprovals(ctx context.Context, worktreePath string, prNumber int) ([]Review, error) {
	owner, repo, err := g.GetRepoOwnerAndName(ctx, worktreePath)
	if err != nil {
		return nil, err
	}

	projectPath := owner + "/" + repo
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%d/approvals", urlEncode(projectPath), prNumber)

	cmd := executil.HideWindow(exec.CommandContext(ctx, "glab", "api", endpoint))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab api approvals failed: %w\nstderr: %s", err, stderr.String())
	}

	var approval glabApproval
	if err := json.Unmarshal(stdout.Bytes(), &approval); err != nil {
		return nil, fmt.Errorf("parsing approvals: %w", err)
	}

	var reviews []Review
	for _, a := range approval.ApprovedBy {
		reviews = append(reviews, Review{
			Author: ReviewAuthor{Login: a.User.Username},
			State:  "APPROVED",
		})
	}
	return reviews, nil
}

// mapGitLabState maps GitLab MR states to canonical VCS states.
func mapGitLabState(state string) string {
	switch strings.ToLower(state) {
	case "opened":
		return "OPEN"
	case "merged":
		return "MERGED"
	case "closed":
		return "CLOSED"
	default:
		return strings.ToUpper(state)
	}
}

// mapGitLabMergeable maps GitLab merge status to canonical mergeable values.
func mapGitLabMergeable(mergeStatus string, hasConflicts bool) string {
	if hasConflicts {
		return "CONFLICTING"
	}
	switch strings.ToLower(mergeStatus) {
	case "can_be_merged", "ci_must_pass", "ci_still_running":
		return "MERGEABLE"
	case "cannot_be_merged", "cannot_be_merged_recheck":
		return "CONFLICTING"
	default:
		return "UNKNOWN"
	}
}

// mapGitLabJobStatus maps GitLab CI job status to a normalized status string.
func mapGitLabJobStatus(status string) string {
	switch strings.ToLower(status) {
	case "success", "failed", "canceled", "skipped":
		return "COMPLETED"
	case "running", "pending", "created", "waiting_for_resource", "preparing":
		return "IN_PROGRESS"
	case "manual":
		return "QUEUED"
	default:
		return strings.ToUpper(status)
	}
}

// mapGitLabJobConclusion maps GitLab CI job status to a canonical conclusion.
func mapGitLabJobConclusion(status string) string {
	switch strings.ToLower(status) {
	case "success":
		return "SUCCESS"
	case "failed":
		return "FAILURE"
	case "canceled":
		return "CANCELLED"
	case "skipped":
		return "SKIPPED"
	case "manual":
		return "NEUTRAL"
	case "running", "pending", "created", "waiting_for_resource", "preparing":
		return "" // not concluded yet
	default:
		return strings.ToUpper(status)
	}
}

// ParseGitLabRepoURL parses a git remote URL into namespace (owner) and project name.
// Supports both HTTPS and SSH URLs, including nested groups.
func ParseGitLabRepoURL(rawURL string) (owner, repo string, err error) {
	rawURL = strings.TrimSuffix(rawURL, ".git")

	// Compute a credential-safe version of the URL for error messages.
	// HTTPS URLs can embed credentials (https://user:token@host/...) that must not be logged.
	safeURL := redactURL(rawURL)

	// SSH: git@gitlab.com:group/subgroup/project
	if strings.HasPrefix(rawURL, "git@") {
		parts := strings.SplitN(rawURL, ":", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("could not parse GitLab SSH URL: %s", safeURL)
		}
		path := parts[1]
		return splitNamespacePath(path, safeURL)
	}

	// HTTPS: https://gitlab.com/group/subgroup/project
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		// Strip scheme and host (including any embedded userinfo).
		_, rest, ok := strings.Cut(rawURL, "://")
		if !ok {
			return "", "", fmt.Errorf("could not parse GitLab URL: %s", safeURL)
		}
		_, path, ok := strings.Cut(rest, "/")
		if !ok {
			return "", "", fmt.Errorf("could not parse GitLab URL: %s", safeURL)
		}
		return splitNamespacePath(path, safeURL)
	}

	return "", "", fmt.Errorf("could not parse GitLab remote URL: %s", safeURL)
}

// splitNamespacePath splits "group/subgroup/project" into ("group/subgroup", "project").
func splitNamespacePath(path, rawURL string) (string, string, error) {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash < 0 {
		return "", "", fmt.Errorf("could not parse GitLab remote URL (no namespace): %s", rawURL)
	}
	return path[:lastSlash], path[lastSlash+1:], nil
}

// extractGlabURL extracts the MR URL from glab mr create output.
// glab typically prints the URL on its own line.
func extractGlabURL(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "/merge_requests/") || strings.Contains(line, "/-/merge_requests/") {
			// The line might contain "https://..." or a formatted string
			if idx := strings.Index(line, "http"); idx >= 0 {
				url := line[idx:]
				// Trim any trailing whitespace or formatting characters
				if spaceIdx := strings.IndexAny(url, " \t\n\r"); spaceIdx > 0 {
					url = url[:spaceIdx]
				}
				return url
			}
		}
	}
	// Fallback: return the last non-empty line (glab often puts URL there)
	var lastLine string
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lastLine = trimmed
		}
	}
	if lastLine != "" {
		if idx := strings.Index(lastLine, "http"); idx >= 0 {
			url := lastLine[idx:]
			if spaceIdx := strings.IndexAny(url, " \t\n\r"); spaceIdx > 0 {
				url = url[:spaceIdx]
			}
			return url
		}
		return lastLine
	}
	return strings.TrimSpace(output)
}

// extractMRNumber parses the MR number from a GitLab MR URL.
// URL format: https://gitlab.com/group/project/-/merge_requests/123
func extractMRNumber(url string) int {
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return 0
	}
	last := parts[len(parts)-1]
	n, err := strconv.Atoi(last)
	if err != nil {
		return 0
	}
	return n
}

// urlEncode encodes a project path for use in GitLab API endpoints.
// GitLab API requires the full namespace/project path to be URL-encoded
// (slashes → %2F, special characters percent-encoded).
// Each path segment is individually escaped via url.PathEscape, then
// joined with %2F so that characters like dots, spaces, and non-ASCII
// names in group/project paths are handled correctly.
func urlEncode(path string) string {
	segments := strings.Split(path, "/")
	encoded := make([]string, len(segments))
	for i, seg := range segments {
		encoded[i] = url.PathEscape(seg)
	}
	return strings.Join(encoded, "%2F")
}

