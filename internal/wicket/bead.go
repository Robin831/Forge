package wicket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
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
func buildBDArgs(decision TriageDecision, issue Issue, priority int, anvilName string) []string {
	sourceURL := issueURL(issue.Repo, issue.Number)
	desc := decision.BeadDescription
	if sourceURL != "" {
		desc = fmt.Sprintf("%s\n\nSource: %s", desc, sourceURL)
	}

	args := []string{
		"--title", decision.BeadTitle,
		"--description", desc,
		"--type", "task",
		"--priority", fmt.Sprintf("%d", priority),
		"--tag", "wicket",
		"--tag", "github-issue",
		"--silent",
	}

	if anvilName != "" {
		meta, err := json.Marshal(map[string]string{"anvil_name": anvilName})
		if err == nil {
			args = append(args, "--metadata", string(meta))
		}
	}

	return args
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
// stores the issue→bead mapping in both wicket_issues (via db) and an
// in-memory cache, and returns the new bead ID.
//
// decision.Action must be ActionCreateBead and both BeadTitle and
// BeadDescription must be non-empty; otherwise CreateBead returns an error
// immediately so that misuse of the function fails fast.
//
// priority is the bd priority (0–4); values outside that range are rejected.
//
// anvilName is the name of the monitoring anvil that owns this issue; it is
// embedded in the bead's metadata so downstream code can route the bead back
// to the correct anvil. Pass an empty string to omit the metadata field.
//
// db may be nil, in which case persistence to state.db is skipped and only
// the in-memory cache is updated.
func CreateBead(ctx context.Context, db *state.DB, decision TriageDecision, issue Issue, priority int, anvilName string) (string, error) {
	if decision.Action != ActionCreateBead {
		return "", fmt.Errorf("CreateBead called with action %q; only %q is allowed", decision.Action, ActionCreateBead)
	}
	if decision.BeadTitle == "" {
		return "", fmt.Errorf("CreateBead: decision missing bead title")
	}
	if decision.BeadDescription == "" {
		return "", fmt.Errorf("CreateBead: decision missing bead description")
	}
	if priority < 0 || priority > 4 {
		return "", fmt.Errorf("invalid priority %d: must be between 0 and 4", priority)
	}

	args := buildBDArgs(decision, issue, priority, anvilName)

	output, err := bdRunner(ctx, args)
	if err != nil {
		return "", fmt.Errorf("create bead for %s#%d: %w", issue.Repo, issue.Number, err)
	}

	beadID, err := parseBDOutput(output)
	if err != nil {
		return "", fmt.Errorf("create bead for %s#%d: %w", issue.Repo, issue.Number, err)
	}

	// Update the in-memory cache for fast same-process lookups.
	wicketIssues.mu.Lock()
	wicketIssues.mapping[issueKey{Repo: issue.Repo, Number: issue.Number}] = beadID
	wicketIssues.mu.Unlock()

	// Persist the bead ID to state.db so the mapping survives restarts and is
	// visible to other components.
	if db != nil {
		if err := persistBeadID(db, issue, decision, beadID); err != nil {
			return "", fmt.Errorf("persist bead ID for %s#%d: %w", issue.Repo, issue.Number, err)
		}
	}

	return beadID, nil
}

// persistBeadID writes (or updates) the wicket_issues row for the given issue
// with the newly created beadID and state "bead_created". If no row exists
// yet, one is inserted.
func persistBeadID(db *state.DB, issue Issue, decision TriageDecision, beadID string) error {
	now := time.Now().UTC()
	wi := state.WicketIssue{
		Repo:         issue.Repo,
		IssueNumber:  issue.Number,
		Title:        issue.Title,
		Body:         issue.Body,
		Author:       issue.Author,
		State:        "bead_created",
		TriageAction: string(decision.Action),
		TriageReason: decision.Reason,
		BeadID:       beadID,
		ProcessedAt:  &now,
	}
	existing, err := db.GetWicketIssue(issue.Repo, issue.Number)
	if err != nil {
		return fmt.Errorf("look up wicket issue: %w", err)
	}
	if existing == nil {
		return db.InsertWicketIssue(wi)
	}
	return db.UpdateWicketIssue(wi)
}
