// Package ghpr creates pull requests via the gh CLI.
//
// After Warden approves, this package runs `gh pr create` to file a PR
// with the bead reference in the title and body.
package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// PR represents a created pull request.
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

// CreateParams holds the inputs for PR creation.
type CreateParams struct {
	// WorktreePath is the git worktree directory to run gh from.
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
	// DB for recording the PR.
	DB *state.DB
}

// Create files a pull request using the gh CLI and records it in the state DB.
func Create(ctx context.Context, p CreateParams) (*PR, error) {
	if p.Base == "" {
		p.Base = "main"
	}

	if p.Title == "" {
		p.Title = fmt.Sprintf("forge: %s", p.BeadID)
	}

	if p.Body == "" {
		p.Body = buildDefaultBody(p.BeadID, p.Branch)
	}

	// Build gh pr create args
	args := []string{
		"pr", "create",
		"--title", p.Title,
		"--body", p.Body,
		"--base", p.Base,
		"--head", p.Branch,
	}
	if p.Draft {
		args = append(args, "--draft")
	}

	log.Printf("[ghpr] Creating PR for %s on branch %s", p.BeadID, p.Branch)

	// Run gh pr create
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = p.WorktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr create failed: %w\nstderr: %s", err, stderr.String())
	}

	prURL := strings.TrimSpace(stdout.String())
	log.Printf("[ghpr] Created PR: %s", prURL)

	// Extract PR number from URL (https://github.com/owner/repo/pull/123)
	prNumber := extractPRNumber(prURL)

	pr := &PR{
		Number:  prNumber,
		URL:     prURL,
		Title:   p.Title,
		Branch:  p.Branch,
		Base:    p.Base,
		BeadID:  p.BeadID,
		Anvil:   p.AnvilName,
		Created: time.Now(),
	}

	// Record in state DB
	if p.DB != nil {
		dbPR := &state.PR{
			Number:    prNumber,
			Anvil:     p.AnvilName,
			BeadID:    p.BeadID,
			Branch:    p.Branch,
			Status:    state.PROpen,
			CreatedAt: pr.Created,
		}
		_ = p.DB.InsertPR(dbPR)
		_ = p.DB.LogEvent(state.EventPRCreated,
			fmt.Sprintf("PR #%d: %s", prNumber, prURL), p.BeadID, p.AnvilName)
	}

	return pr, nil
}

// CheckStatus gets the current status of a PR via gh pr view.
func CheckStatus(ctx context.Context, worktreePath string, prNumber int) (*PRStatus, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", prNumber),
		"--json", "state,statusCheckRollup,reviews,mergeable",
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr view failed: %w\nstderr: %s", err, stderr.String())
	}

	var status PRStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		return nil, fmt.Errorf("parsing pr status: %w", err)
	}

	return &status, nil
}

// PRStatus represents the GitHub state of a PR.
type PRStatus struct {
	State             string        `json:"state"`
	StatusCheckRollup []CheckRun    `json:"statusCheckRollup"`
	Reviews           []Review      `json:"reviews"`
	Mergeable         string        `json:"mergeable"`
}

// CheckRun represents a CI check on the PR.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Review represents a PR review.
type Review struct {
	Author string `json:"author"`
	State  string `json:"state"`
	Body   string `json:"body"`
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
		return true // no checks configured
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

// NeedsChanges returns true if any review requests changes.
func (s *PRStatus) NeedsChanges() bool {
	for _, r := range s.Reviews {
		if r.State == "CHANGES_REQUESTED" {
			return true
		}
	}
	return false
}

// buildDefaultBody creates a standard PR body referencing the bead.
func buildDefaultBody(beadID, branch string) string {
	return fmt.Sprintf(`## Automated PR by The Forge

**Bead**: %s
**Branch**: %s

This PR was generated by The Forge autonomous pipeline.
A Smith agent implemented the changes, Temper verified the build,
and Warden approved the code review.

---
*Generated by [The Forge](https://github.com/Robin831/Forge)*`, beadID, branch)
}

// extractPRNumber parses the PR number from a GitHub PR URL.
func extractPRNumber(url string) int {
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return 0
	}
	last := parts[len(parts)-1]
	var n int
	fmt.Sscanf(last, "%d", &n)
	return n
}
