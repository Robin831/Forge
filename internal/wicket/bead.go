package wicket

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/Robin831/Forge/internal/executil"
)

// issueKey uniquely identifies a GitHub issue.
type issueKey struct {
	Repo   string
	Number int
}

// beadStore holds the mapping from GitHub issues to bead IDs.
type beadStore struct {
	mu      sync.Mutex
	mapping map[issueKey]string
}

var wicketIssues = &beadStore{
	mapping: make(map[issueKey]string),
}

// BeadIDFor returns the bead ID previously recorded for the given issue, and
// whether one exists.
func BeadIDFor(repo string, number int) (string, bool) {
	wicketIssues.mu.Lock()
	defer wicketIssues.mu.Unlock()
	id, ok := wicketIssues.mapping[issueKey{Repo: repo, Number: number}]
	return id, ok
}

// issueURL constructs the canonical GitHub URL for an issue.
func issueURL(repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo, number)
}

// bdRunner is the function used to execute `bd create`. Tests replace this to
// avoid spawning a real subprocess.
var bdRunner func(ctx context.Context, args []string) (string, error) = defaultBDRunner

func defaultBDRunner(ctx context.Context, args []string) (string, error) {
	cmdArgs := append([]string{"create"}, args...)
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", cmdArgs...))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		se := strings.TrimSpace(stderr.String())
		if se != "" {
			return "", fmt.Errorf("bd create: %v: %s", err, se)
		}
		return "", fmt.Errorf("bd create: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// buildBDArgs constructs the argument list for `bd create` (without the
// "create" verb itself). Separated for unit-testability.
func buildBDArgs(decision TriageDecision, issue Issue, priority int) []string {
	sourceURL := issueURL(issue.Repo, issue.Number)
	desc := decision.BeadDescription
	if sourceURL != "" {
		desc = fmt.Sprintf("%s\n\nSource: %s", desc, sourceURL)
	}

	return []string{
		"--title", decision.BeadTitle,
		"--description", desc,
		"--type", "task",
		"--priority", fmt.Sprintf("%d", priority),
		"--tag", "wicket",
		"--tag", "github-issue",
		"--silent",
	}
}

// parseBDOutput extracts the bead ID from the output of `bd create --silent`.
// The command prints the bead ID (e.g. "Forge-abc1") on its own line.
func parseBDOutput(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("bd create returned empty output")
}

// CreateBead creates a bead from the given triage decision and GitHub issue,
// stores the issue→bead mapping in wicket_issues, and returns the new bead ID.
// priority is the bd priority (0–4).
func CreateBead(ctx context.Context, decision TriageDecision, issue Issue, priority int) (string, error) {
	args := buildBDArgs(decision, issue, priority)

	output, err := bdRunner(ctx, args)
	if err != nil {
		return "", fmt.Errorf("create bead for %s#%d: %w", issue.Repo, issue.Number, err)
	}

	beadID, err := parseBDOutput(output)
	if err != nil {
		return "", fmt.Errorf("create bead for %s#%d: %w", issue.Repo, issue.Number, err)
	}

	wicketIssues.mu.Lock()
	wicketIssues.mapping[issueKey{Repo: issue.Repo, Number: issue.Number}] = beadID
	wicketIssues.mu.Unlock()

	return beadID, nil
}
