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

// GitHubClient defines the GitHub operations used by the wicket package.
// All methods operate on a specific repository identified by "owner/repo".
type GitHubClient interface {
	// ListIssues returns open issues for the given repository. If labels is
	// non-empty, only issues that carry all of the listed label names are
	// returned.
	ListIssues(ctx context.Context, repo string, labels []string) ([]Issue, error)

	// GetIssue returns a single issue by number from the given repository.
	GetIssue(ctx context.Context, repo string, number int) (*Issue, error)

	// ListComments returns all comments on the specified issue, ordered by
	// creation time ascending.
	ListComments(ctx context.Context, repo string, number int) ([]Comment, error)

	// ListReactions returns all emoji reactions on the specified issue.
	ListReactions(ctx context.Context, repo string, number int) ([]Reaction, error)

	// CommentOnIssue posts body as a new comment on the specified issue.
	CommentOnIssue(ctx context.Context, repo string, number int, body string) error

	// AddLabels attaches the given label names to the specified issue.
	AddLabels(ctx context.Context, repo string, number int, labels []string) error

	// RemoveLabel removes a single label from the specified issue.
	RemoveLabel(ctx context.Context, repo string, number int, label string) error

	// CloseIssue closes the specified issue. reason is passed as --reason to gh
	// (e.g. "completed", "not planned"); an empty string omits the flag.
	CloseIssue(ctx context.Context, repo string, number int, reason string) error
}

// ghClient implements GitHubClient by shelling out to the gh CLI.
type ghClient struct{}

// NewGitHubClient returns a GitHubClient backed by the gh CLI.
func NewGitHubClient() GitHubClient {
	return &ghClient{}
}

// ghIssue is the JSON shape returned by `gh issue list --json` and
// `gh issue view --json`.
type ghIssue struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	CreatedAt string   `json:"createdAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (g *ghClient) ListIssues(ctx context.Context, repo string, labels []string) ([]Issue, error) {
	args := []string{
		"issue", "list",
		"--repo", repo,
		"--state", "open",
		"--json", "number,title,body,state,createdAt,author,labels",
		"--limit", "100",
	}
	for _, l := range labels {
		args = append(args, "--label", l)
	}

	out, err := runGH(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("gh issue list %s: %w", repo, err)
	}

	var raw []ghIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue list parse %s: %w", repo, err)
	}

	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		issue, err := toIssue(r, repo)
		if err != nil {
			return nil, fmt.Errorf("gh issue list parse %s#%d: %w", repo, r.Number, err)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

func (g *ghClient) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	args := []string{
		"issue", "view", strconv.Itoa(number),
		"--repo", repo,
		"--json", "number,title,body,state,createdAt,author,labels",
	}

	out, err := runGH(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("gh issue view %s#%d: %w", repo, number, err)
	}

	var raw ghIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue view parse %s#%d: %w", repo, number, err)
	}

	issue, err := toIssue(raw, repo)
	if err != nil {
		return nil, fmt.Errorf("gh issue view parse %s#%d: %w", repo, number, err)
	}
	return &issue, nil
}

// ghAPIComment is the JSON shape of a comment from the GitHub REST API.
type ghAPIComment struct {
	ID   int64  `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// ghAPIReaction is the JSON shape of a reaction from the GitHub REST API.
type ghAPIReaction struct {
	Content string `json:"content"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g *ghClient) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	args := []string{
		"api", fmt.Sprintf("repos/%s/issues/%d/comments", repo, number),
		"--paginate",
	}
	out, err := runGH(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("gh api comments %s#%d: %w", repo, number, err)
	}

	var raw []ghAPIComment
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse comments %s#%d: %w", repo, number, err)
	}

	comments := make([]Comment, 0, len(raw))
	for _, r := range raw {
		var createdAt time.Time
		if r.CreatedAt != "" {
			createdAt, _ = time.Parse(time.RFC3339, r.CreatedAt)
		}
		comments = append(comments, Comment{
			ID:        r.ID,
			Author:    r.User.Login,
			Body:      r.Body,
			CreatedAt: createdAt,
		})
	}
	return comments, nil
}

func (g *ghClient) ListReactions(ctx context.Context, repo string, number int) ([]Reaction, error) {
	args := []string{
		"api", fmt.Sprintf("repos/%s/issues/%d/reactions", repo, number),
		"--paginate",
	}
	out, err := runGH(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("gh api reactions %s#%d: %w", repo, number, err)
	}

	var raw []ghAPIReaction
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse reactions %s#%d: %w", repo, number, err)
	}

	reactions := make([]Reaction, 0, len(raw))
	for _, r := range raw {
		reactions = append(reactions, Reaction{
			Content: r.Content,
			User:    r.User.Login,
		})
	}
	return reactions, nil
}

func (g *ghClient) CommentOnIssue(ctx context.Context, repo string, number int, body string) error {
	cmd := executil.HideWindow(exec.CommandContext(
		ctx,
		"gh",
		"issue", "comment", strconv.Itoa(number),
		"--repo", repo,
		"--body-file", "-",
	))
	cmd.Stdin = strings.NewReader(body)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("gh issue comment %s#%d: %v: %s", repo, number, err, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("gh issue comment %s#%d: %w", repo, number, err)
	}
	return nil
}

func (g *ghClient) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	args := []string{
		"issue", "edit", strconv.Itoa(number),
		"--repo", repo,
	}
	for _, l := range labels {
		args = append(args, "--add-label", l)
	}
	_, err := runGH(ctx, args)
	if err != nil {
		return fmt.Errorf("gh issue edit add-label %s#%d: %w", repo, number, err)
	}
	return nil
}

func (g *ghClient) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	args := []string{
		"issue", "edit", strconv.Itoa(number),
		"--repo", repo,
		"--remove-label", label,
	}
	_, err := runGH(ctx, args)
	if err != nil {
		return fmt.Errorf("gh issue edit remove-label %s#%d: %w", repo, number, err)
	}
	return nil
}

func (g *ghClient) CloseIssue(ctx context.Context, repo string, number int, reason string) error {
	args := []string{
		"issue", "close", strconv.Itoa(number),
		"--repo", repo,
	}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	_, err := runGH(ctx, args)
	if err != nil {
		return fmt.Errorf("gh issue close %s#%d: %w", repo, number, err)
	}
	return nil
}

// runGH executes the gh CLI with the provided arguments and returns stdout.
func runGH(ctx context.Context, args []string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w\nstderr: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// toIssue converts the raw gh JSON shape to an Issue.
func toIssue(r ghIssue, repo string) (Issue, error) {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	var createdAt time.Time
	if r.CreatedAt != "" {
		var err error
		createdAt, err = time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			return Issue{}, fmt.Errorf("parse createdAt %q: %w", r.CreatedAt, err)
		}
	}
	return Issue{
		Number:    r.Number,
		Repo:      repo,
		Title:     r.Title,
		Body:      r.Body,
		Author:    r.Author.Login,
		Labels:    labels,
		CreatedAt: createdAt,
	}, nil
}

// MockGitHubClient is a test double for GitHubClient. Tests set the On*
// fields to inject canned responses or capture calls.
type MockGitHubClient struct {
	// OnListIssues is called by ListIssues if non-nil.
	OnListIssues func(ctx context.Context, repo string, labels []string) ([]Issue, error)
	// OnGetIssue is called by GetIssue if non-nil.
	OnGetIssue func(ctx context.Context, repo string, number int) (*Issue, error)
	// OnListComments is called by ListComments if non-nil.
	OnListComments func(ctx context.Context, repo string, number int) ([]Comment, error)
	// OnListReactions is called by ListReactions if non-nil.
	OnListReactions func(ctx context.Context, repo string, number int) ([]Reaction, error)
	// OnCommentOnIssue is called by CommentOnIssue if non-nil.
	OnCommentOnIssue func(ctx context.Context, repo string, number int, body string) error
	// OnAddLabels is called by AddLabels if non-nil.
	OnAddLabels func(ctx context.Context, repo string, number int, labels []string) error
	// OnRemoveLabel is called by RemoveLabel if non-nil.
	OnRemoveLabel func(ctx context.Context, repo string, number int, label string) error
	// OnCloseIssue is called by CloseIssue if non-nil.
	OnCloseIssue func(ctx context.Context, repo string, number int, reason string) error

	// Recorded calls — populated regardless of whether an On* func is set.
	CommentCalls  []CommentCall
	AddLabelCalls []AddLabelCall
	RemoveCalls   []RemoveCall
	CloseCalls    []CloseCall
}

// CommentCall records arguments from a CommentOnIssue invocation.
type CommentCall struct {
	Repo   string
	Number int
	Body   string
}

// AddLabelCall records arguments from an AddLabels invocation.
type AddLabelCall struct {
	Repo   string
	Number int
	Labels []string
}

// RemoveCall records arguments from a RemoveLabel invocation.
type RemoveCall struct {
	Repo   string
	Number int
	Label  string
}

// CloseCall records arguments from a CloseIssue invocation.
type CloseCall struct {
	Repo   string
	Number int
}

func (m *MockGitHubClient) ListIssues(ctx context.Context, repo string, labels []string) ([]Issue, error) {
	if m.OnListIssues != nil {
		return m.OnListIssues(ctx, repo, labels)
	}
	return nil, nil
}

func (m *MockGitHubClient) GetIssue(ctx context.Context, repo string, number int) (*Issue, error) {
	if m.OnGetIssue != nil {
		return m.OnGetIssue(ctx, repo, number)
	}
	return nil, nil
}

func (m *MockGitHubClient) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	if m.OnListComments != nil {
		return m.OnListComments(ctx, repo, number)
	}
	return nil, nil
}

func (m *MockGitHubClient) ListReactions(ctx context.Context, repo string, number int) ([]Reaction, error) {
	if m.OnListReactions != nil {
		return m.OnListReactions(ctx, repo, number)
	}
	return nil, nil
}

func (m *MockGitHubClient) CommentOnIssue(ctx context.Context, repo string, number int, body string) error {
	m.CommentCalls = append(m.CommentCalls, CommentCall{Repo: repo, Number: number, Body: body})
	if m.OnCommentOnIssue != nil {
		return m.OnCommentOnIssue(ctx, repo, number, body)
	}
	return nil
}

func (m *MockGitHubClient) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	cp := make([]string, len(labels))
	copy(cp, labels)
	m.AddLabelCalls = append(m.AddLabelCalls, AddLabelCall{Repo: repo, Number: number, Labels: cp})
	if m.OnAddLabels != nil {
		return m.OnAddLabels(ctx, repo, number, labels)
	}
	return nil
}

func (m *MockGitHubClient) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	m.RemoveCalls = append(m.RemoveCalls, RemoveCall{Repo: repo, Number: number, Label: label})
	if m.OnRemoveLabel != nil {
		return m.OnRemoveLabel(ctx, repo, number, label)
	}
	return nil
}

func (m *MockGitHubClient) CloseIssue(ctx context.Context, repo string, number int, reason string) error {
	m.CloseCalls = append(m.CloseCalls, CloseCall{Repo: repo, Number: number})
	if m.OnCloseIssue != nil {
		return m.OnCloseIssue(ctx, repo, number, reason)
	}
	return nil
}
