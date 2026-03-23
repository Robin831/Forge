package wicket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// GitHubClient is an interface for GitHub issue operations, wrapping the gh CLI.
// A mock implementation is used in tests.
type GitHubClient interface {
	// ListIssues fetches open issues from the given repo (owner/repo).
	// limit caps the number of issues returned.
	ListIssues(ctx context.Context, repo string, limit int) ([]Issue, error)
	// GetIssue fetches a single issue by number.
	GetIssue(ctx context.Context, repo string, number int) (*Issue, error)
	// CommentOnIssue posts a comment on the given issue.
	CommentOnIssue(ctx context.Context, repo string, number int, body string) error
	// AddLabels adds labels to the given issue.
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
	// RemoveLabel removes a single label from the given issue.
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
}

// ghClient is the production implementation that shells out to the gh CLI.
type ghClient struct {
	// timeout is the per-command deadline. Zero uses a built-in default.
	timeout time.Duration
}

const defaultGHTimeout = 30 * time.Second

// newGHClient returns a production GitHubClient.
func newGHClient(timeout time.Duration) GitHubClient {
	if timeout <= 0 {
		timeout = defaultGHTimeout
	}
	return &ghClient{timeout: timeout}
}

func (c *ghClient) cmdCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// ListIssues runs: gh issue list --repo <repo> --state open --json ...
func (c *ghClient) ListIssues(ctx context.Context, repo string, limit int) ([]Issue, error) {
	cmdCtx, cancel := c.cmdCtx(ctx)
	defer cancel()

	if limit <= 0 {
		limit = 25
	}

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "issue", "list",
		"--repo", repo,
		"--state", "open",
		"--json", "number,title,body,state,author,labels,comments,createdAt,updatedAt,url,isPullRequest",
		"--limit", strconv.Itoa(limit),
	))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list %s: %w: %s", repo, err, stderr.String())
	}

	var issues []Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing gh issue list output: %w", err)
	}
	return issues, nil
}

// GetIssue runs: gh issue view <number> --repo <repo> --json ...
func (c *ghClient) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	cmdCtx, cancel := c.cmdCtx(ctx)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "issue", "view",
		strconv.Itoa(number),
		"--repo", repo,
		"--json", "number,title,body,state,author,labels,comments,createdAt,updatedAt,url,isPullRequest",
	))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue view %s#%d: %w: %s", repo, number, err, stderr.String())
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing gh issue view output: %w", err)
	}
	return &issue, nil
}

// CommentOnIssue runs: gh issue comment <number> --repo <repo> --body <body>
func (c *ghClient) CommentOnIssue(ctx context.Context, repo string, number int, body string) error {
	cmdCtx, cancel := c.cmdCtx(ctx)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "issue", "comment",
		strconv.Itoa(number),
		"--repo", repo,
		"--body", body,
	))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue comment %s#%d: %w: %s", repo, number, err, stderr.String())
	}
	return nil
}

// AddLabels runs: gh issue edit <number> --repo <repo> --add-label "l1,l2"
func (c *ghClient) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	cmdCtx, cancel := c.cmdCtx(ctx)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "issue", "edit",
		strconv.Itoa(number),
		"--repo", repo,
		"--add-label", strings.Join(labels, ","),
	))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue edit (add-label) %s#%d: %w: %s", repo, number, err, stderr.String())
	}
	return nil
}

// parseRepoFromPath resolves "owner/repo" by running `git remote get-url origin`
// in the given directory and parsing the URL.
func parseRepoFromPath(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "remote", "get-url", "origin"))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}

	remoteURL := strings.TrimSpace(string(out))
	return parseOwnerRepo(remoteURL)
}

// parseOwnerRepo extracts "owner/repo" from a GitHub remote URL.
// Supports both https://github.com/owner/repo.git and git@github.com:owner/repo.git formats.
func parseOwnerRepo(remoteURL string) (string, error) {
	// Handle ssh format: git@github.com:owner/repo.git
	if strings.HasPrefix(remoteURL, "git@") {
		colon := strings.LastIndex(remoteURL, ":")
		if colon < 0 {
			return "", fmt.Errorf("unexpected ssh remote URL format: %q", remoteURL)
		}
		path := strings.TrimSuffix(remoteURL[colon+1:], ".git")
		return path, nil
	}

	// Handle https format: https://github.com/owner/repo.git
	// Strip scheme and host.
	if idx := strings.Index(remoteURL, "://"); idx >= 0 {
		rest := remoteURL[idx+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return "", fmt.Errorf("unexpected https remote URL format: %q", remoteURL)
		}
		path := strings.TrimSuffix(rest[slash+1:], ".git")
		return path, nil
	}

	return "", fmt.Errorf("unrecognised remote URL format: %q", remoteURL)
}

// RemoveLabel runs: gh issue edit <number> --repo <repo> --remove-label <label>
func (c *ghClient) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	cmdCtx, cancel := c.cmdCtx(ctx)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "issue", "edit",
		strconv.Itoa(number),
		"--repo", repo,
		"--remove-label", label,
	))

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue edit (remove-label) %s#%d: %w: %s", repo, number, err, stderr.String())
	}
	return nil
}
