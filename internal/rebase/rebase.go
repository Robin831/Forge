// Package rebase resolves merge conflicts by rebasing a PR branch onto main.
//
// When Bellows detects a merge conflict, the daemon calls Rebase, which runs
// git fetch + git rebase origin/main + git push --force-with-lease in the PR's
// worktree. On success the PR is no longer conflicting and CI can proceed.
package rebase

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/Robin831/Forge/internal/state"
)

// Params holds the inputs for a rebase attempt.
type Params struct {
	// WorktreePath is the git worktree for the PR branch.
	WorktreePath string
	// Branch is the PR branch name.
	Branch string
	// BaseBranch is the branch to rebase onto (default: "main").
	BaseBranch string
	// BeadID for logging.
	BeadID string
	// AnvilName for logging.
	AnvilName string
	// PRNumber for logging.
	PRNumber int
	// DB for event logging (may be nil).
	DB *state.DB
}

// Result holds the outcome of a rebase attempt.
type Result struct {
	Success bool
	Output  string
	Error   error
}

// Rebase fetches origin and rebases the branch onto BaseBranch, then force-pushes.
// It is safe to call from concurrent goroutines; each call operates in its own worktree.
func Rebase(ctx context.Context, p Params) Result {
	if p.BaseBranch == "" {
		p.BaseBranch = "main"
	}

	logEvent(p.DB, state.EventRebaseStarted,
		fmt.Sprintf("PR #%d: rebase onto origin/%s started", p.PRNumber, p.BaseBranch),
		p.BeadID, p.AnvilName)

	log.Printf("[rebase] PR #%d: starting rebase of %q onto origin/%s", p.PRNumber, p.Branch, p.BaseBranch)

	// Step 1: fetch latest remote state.
	if res := runGit(ctx, p.WorktreePath, "fetch", "origin"); res.Error != nil {
		msg := fmt.Sprintf("PR #%d: git fetch failed: %s", p.PRNumber, res.Output)
		log.Printf("[rebase] %s", msg)
		logEvent(p.DB, state.EventRebaseFailed, msg, p.BeadID, p.AnvilName)
		return Result{Success: false, Output: res.Output, Error: res.Error}
	}

	// Step 2: abort any existing rebase before starting (idempotent).
	_ = runGit(ctx, p.WorktreePath, "rebase", "--abort")

	// Step 3: rebase onto origin/<base>.
	rebaseTarget := "origin/" + p.BaseBranch
	res := runGit(ctx, p.WorktreePath, "rebase", rebaseTarget)
	if res.Error != nil {
		// Abort to leave the worktree clean for the next attempt.
		_ = runGit(ctx, p.WorktreePath, "rebase", "--abort")
		msg := fmt.Sprintf("PR #%d: git rebase %s failed: %s", p.PRNumber, rebaseTarget, res.Output)
		log.Printf("[rebase] %s", msg)
		logEvent(p.DB, state.EventRebaseFailed, msg, p.BeadID, p.AnvilName)
		return Result{Success: false, Output: res.Output, Error: res.Error}
	}

	// Step 4: push the rebased branch.
	push := runGit(ctx, p.WorktreePath, "push", "--force-with-lease", "origin", p.Branch)
	if push.Error != nil {
		msg := fmt.Sprintf("PR #%d: git push --force-with-lease failed: %s", p.PRNumber, push.Output)
		log.Printf("[rebase] %s", msg)
		logEvent(p.DB, state.EventRebaseFailed, msg, p.BeadID, p.AnvilName)
		return Result{Success: false, Output: push.Output, Error: push.Error}
	}

	msg := fmt.Sprintf("PR #%d: rebased onto %s and pushed successfully", p.PRNumber, rebaseTarget)
	log.Printf("[rebase] %s", msg)
	logEvent(p.DB, state.EventRebaseSuccess, msg, p.BeadID, p.AnvilName)
	return Result{Success: true, Output: push.Output}
}

type cmdResult struct {
	Output string
	Error  error
}

func runGit(ctx context.Context, dir string, args ...string) cmdResult {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return cmdResult{Output: output, Error: fmt.Errorf("git %s: %w\n%s", args[0], err, output)}
	}
	return cmdResult{Output: output}
}

func logEvent(db *state.DB, evt state.EventType, msg, beadID, anvil string) {
	if db == nil {
		return
	}
	_ = db.LogEvent(evt, msg, beadID, anvil)
}
