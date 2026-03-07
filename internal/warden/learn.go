package warden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// PRComment represents a review comment fetched from GitHub.
type PRComment struct {
	Body     string `json:"body"`
	User     string `json:"user"`
	Path     string `json:"path"`
	PRNumber int    `json:"pr_number"`
}

// ghReviewComment is the JSON shape returned by `gh api`.
type ghReviewComment struct {
	Body string `json:"body"`
	Path string `json:"path"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// FetchCopilotComments retrieves review comments on a PR that were authored by
// copilot[bot], github-actions[bot], or copilot via the gh CLI.
func FetchCopilotComments(ctx context.Context, repoDir string, prNumber int) ([]PRComment, error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber)
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "api", endpoint, "--paginate"))
	cmd.Dir = repoDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	var raw []ghReviewComment
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parsing gh response: %w", err)
	}

	var comments []PRComment
	for _, c := range raw {
		login := strings.ToLower(c.User.Login)
		if login == "copilot[bot]" || login == "github-actions[bot]" || login == "copilot" {
			comments = append(comments, PRComment{
				Body:     c.Body,
				User:     c.User.Login,
				Path:     c.Path,
				PRNumber: prNumber,
			})
		}
	}
	return comments, nil
}

// FetchRecentPRNumbers returns the most recent merged PR numbers for a repo.
func FetchRecentPRNumbers(ctx context.Context, repoDir string, limit int) ([]int, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "list",
		"--state=merged", "--limit", fmt.Sprintf("%d", limit), "--json=number"))
	cmd.Dir = repoDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr list: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("parsing PR list: %w", err)
	}

	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums, nil
}

// DistillRule uses a Claude session to convert a set of similar review
// comments into a single warden rule. Returns the rule or an error.
func DistillRule(ctx context.Context, comments []PRComment, repoDir string) (*Rule, error) {
	if len(comments) == 0 {
		return nil, fmt.Errorf("no comments to distill")
	}

	// Build a prompt for Claude to generate a rule
	var sb strings.Builder
	sb.WriteString("You are helping create a code review checklist rule from Copilot review comments.\n\n")
	sb.WriteString("Given these review comments, create a single reusable review rule.\n\n")
	sb.WriteString("## Comments\n\n")
	for i, c := range comments {
		sb.WriteString(fmt.Sprintf("### Comment %d (PR #%d, file: %s)\n%s\n\n", i+1, c.PRNumber, c.Path, c.Body))
	}
	sb.WriteString(`## Output Format

Respond with ONLY a JSON object (no markdown fences, no explanation) in this exact format:

{"id": "short-kebab-id", "category": "concurrency|ui|error-handling|security|testing|performance|style|other", "pattern": "What code pattern triggers this check", "check": "What the reviewer should verify"}
`)

	prompt := sb.String()

	cmd := executil.HideWindow(exec.CommandContext(ctx, "claude", "--print", "--max-turns", "1", "-p", prompt))
	cmd.Dir = repoDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude distill: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	// Extract JSON from Claude's output
	output := strings.TrimSpace(stdout.String())
	jsonStr := extractJSON(output)
	if jsonStr == "" {
		// Try the raw output directly
		jsonStr = output
	}

	var rule Rule
	if err := json.Unmarshal([]byte(jsonStr), &rule); err != nil {
		return nil, fmt.Errorf("parsing distilled rule: %w (output: %s)", err, output)
	}

	if rule.ID == "" || rule.Check == "" {
		return nil, fmt.Errorf("distilled rule missing required fields (id or check)")
	}

	// Set provenance
	prNums := map[int]bool{}
	for _, c := range comments {
		prNums[c.PRNumber] = true
	}
	sortedNums := make([]int, 0, len(prNums))
	for n := range prNums {
		sortedNums = append(sortedNums, n)
	}
	sort.Ints(sortedNums)
	var sources []string
	for _, n := range sortedNums {
		sources = append(sources, fmt.Sprintf("copilot:PR#%d", n))
	}
	rule.Source = strings.Join(sources, ", ")
	rule.Added = time.Now().Format("2006-01-02")

	return &rule, nil
}

// GroupComments groups similar comments by normalized comment body text.
// Comments with identical (case-folded, whitespace-collapsed) body text are
// merged into the same group. Returns groups where each group contains
// related comments.
func GroupComments(comments []PRComment) [][]PRComment {
	if len(comments) == 0 {
		return nil
	}

	// Simple grouping: each unique comment body text becomes one group.
	// Duplicates (same body) are merged into the same group.
	seen := map[string]int{} // normalized body -> group index
	var groups [][]PRComment

	for _, c := range comments {
		key := normalizeBody(c.Body)
		if idx, ok := seen[key]; ok {
			groups[idx] = append(groups[idx], c)
		} else {
			seen[key] = len(groups)
			groups = append(groups, []PRComment{c})
		}
	}
	return groups
}

// normalizeBody returns a lowered, trimmed version for dedup comparison.
func normalizeBody(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Collapse whitespace
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
