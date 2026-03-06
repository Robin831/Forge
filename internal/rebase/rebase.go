// Package rebase resolves merge conflicts by rebasing a PR branch onto main.
//
// When Bellows detects a merge conflict, Rebase runs git fetch + rebase. If the
// rebase has conflicts that cannot be auto-resolved by git, it invokes a Smith
// (Claude/Gemini) worker to resolve them, then continues the rebase.
package rebase

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
)

// Params holds the inputs for a rebase attempt.
type Params struct {
	// WorktreePath is the git worktree for this PR branch.
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
	// ExtraFlags for the AI CLI.
	ExtraFlags []string
	// Providers is the ordered list of AI providers to try.
	// If empty, provider.Defaults() is used.
	Providers []provider.Provider
}

// Result holds the outcome of a rebase attempt.
type Result struct {
	Success bool
	Output  string
	Error   error
}

// Rebase fetches origin and rebases the branch onto BaseBranch, then force-pushes.
// If git cannot auto-resolve conflicts it invokes Smith (Claude/Gemini) to fix them.
func Rebase(ctx context.Context, p Params) Result {
	if p.BaseBranch == "" {
		p.BaseBranch = "main"
	}

	providers := p.Providers
	if len(providers) == 0 {
		providers = provider.Defaults()
	}

	logEvent(p.DB, state.EventRebaseStarted,
		fmt.Sprintf("PR #%d: rebase onto origin/%s started", p.PRNumber, p.BaseBranch),
		p.BeadID, p.AnvilName)

	log.Printf("[rebase] PR #%d: starting rebase of %q onto origin/%s", p.PRNumber, p.Branch, p.BaseBranch)

	// Step 1: fetch latest remote state.
	if res := runGit(p.WorktreePath, "fetch", "origin"); res.err != nil {
		return p.fail(fmt.Sprintf("git fetch failed: %s", firstLine(res.output)), res.err)
	}

	// Step 2: abort any in-progress rebase to leave the tree clean.
	_ = runGit(p.WorktreePath, "rebase", "--abort")

	// Step 3: start the rebase.
	rebaseTarget := "origin/" + p.BaseBranch
	res := runGit(p.WorktreePath, "rebase", rebaseTarget)
	if res.err != nil {
		// Check whether git left conflicted files that Smith can resolve.
		conflicted := conflictedFiles(p.WorktreePath)
		if len(conflicted) == 0 {
			_ = runGit(p.WorktreePath, "rebase", "--abort")
			return p.fail(fmt.Sprintf("git rebase %s failed: %s", rebaseTarget, firstLine(res.output)), res.err)
		}

		log.Printf("[rebase] PR #%d: %d conflicted files — invoking Smith", p.PRNumber, len(conflicted))
		if err := p.resolveWithSmith(ctx, conflicted, providers); err != nil {
			_ = runGit(p.WorktreePath, "rebase", "--abort")
			return p.fail(fmt.Sprintf("Smith could not resolve conflicts: %s", firstLine(err.Error())), err)
		}

		// Stage all resolved files and continue the rebase.
		if r := runGit(p.WorktreePath, "add", "."); r.err != nil {
			_ = runGit(p.WorktreePath, "rebase", "--abort")
			return p.fail(fmt.Sprintf("git add after Smith: %s", firstLine(r.output)), r.err)
		}
		// Use GIT_EDITOR=true so --continue never opens an interactive editor.
		continueCmd := exec.Command("git", "rebase", "--continue")
		continueCmd.Dir = p.WorktreePath
		continueCmd.Env = append(continueCmd.Environ(), "GIT_EDITOR=true")
		executil.HideWindow(continueCmd)
		out, err := continueCmd.CombinedOutput()
		if err != nil {
			_ = runGit(p.WorktreePath, "rebase", "--abort")
			return p.fail(
				fmt.Sprintf("git rebase --continue failed: %s", firstLine(strings.TrimSpace(string(out)))),
				err,
			)
		}
	}

	// Step 4: push the rebased branch.
	push := runGit(p.WorktreePath, "push", "--force-with-lease", "origin", p.Branch)
	if push.err != nil {
		return p.fail(fmt.Sprintf("git push --force-with-lease failed: %s", firstLine(push.output)), push.err)
	}

	msg := fmt.Sprintf("PR #%d: rebased onto %s and pushed successfully", p.PRNumber, rebaseTarget)
	log.Printf("[rebase] %s", msg)
	logEvent(p.DB, state.EventRebaseSuccess, msg, p.BeadID, p.AnvilName)
	return Result{Success: true}
}

// resolveWithSmith invokes a Smith worker (Claude/Gemini) to resolve conflicts.
func (p *Params) resolveWithSmith(ctx context.Context, conflicted []string, providers []provider.Provider) error {
	prompt := buildConflictPrompt(p.WorktreePath, conflicted)
	logDir := p.WorktreePath + "/.forge-logs"

	for pi, pv := range providers {
		if pi > 0 {
			log.Printf("[rebase] PR #%d: provider %s rate-limited, trying %s",
				p.PRNumber, providers[pi-1].Kind, pv.Kind)
		}
		process, err := smith.SpawnWithProvider(ctx, p.WorktreePath, prompt, logDir, pv, p.ExtraFlags)
		if err != nil {
			return fmt.Errorf("spawning Smith (%s): %w", pv.Kind, err)
		}
		result := process.Wait()
		if result.RateLimited {
			continue
		}
		if result.ExitCode != 0 || result.IsError {
			return fmt.Errorf("Smith exited %d (subtype=%s)", result.ExitCode, result.ResultSubtype)
		}
		return nil
	}
	return fmt.Errorf("all %d providers are rate-limited or failed", len(providers))
}

// buildConflictPrompt creates a prompt asking the AI to resolve conflict markers.
func buildConflictPrompt(worktreePath string, conflicted []string) string {
	var sb strings.Builder
	sb.WriteString("You are resolving git merge conflicts in a pull request rebase.\n\n")
	sb.WriteString("The following files have conflict markers (<<<<<<, =======, >>>>>>>):\n")
	for _, f := range conflicted {
		sb.WriteString("  - " + f + "\n")
	}
	sb.WriteString("\nFor each conflicted file:\n")
	sb.WriteString("1. Read the file to see the conflict markers\n")
	sb.WriteString("2. Resolve the conflict by choosing the correct content, combining both sides intelligently\n")
	sb.WriteString("3. Remove ALL conflict markers (<<<<<<, =======, >>>>>>>)\n")
	sb.WriteString("4. Write the resolved content back to the file\n\n")
	sb.WriteString("Do NOT run any git commands — only edit the files. ")
	sb.WriteString("Preserve the intent of both the incoming changes and the existing code.\n")
	sb.WriteString("Working directory: " + worktreePath + "\n")
	return sb.String()
}

// conflictedFiles returns files with unresolved conflict markers.
func conflictedFiles(dir string) []string {
	res := runGit(dir, "diff", "--name-only", "--diff-filter=U")
	if res.err != nil || res.output == "" {
		return nil
	}
	var files []string
	for _, line := range strings.Split(res.output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// fail logs a failure and returns a Result.
func (p *Params) fail(msg string, err error) Result {
	full := fmt.Sprintf("PR #%d: %s", p.PRNumber, msg)
	log.Printf("[rebase] %s", full)
	logEvent(p.DB, state.EventRebaseFailed, full, p.BeadID, p.AnvilName)
	return Result{Success: false, Output: msg, Error: err}
}

type cmdResult struct {
	output string
	err    error
}

func runGit(dir string, args ...string) cmdResult {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	executil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return cmdResult{output: output, err: fmt.Errorf("git %s: %w", args[0], err)}
	}
	return cmdResult{output: output}
}

// firstLine returns only the first non-empty line, safe for single-line DB storage.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > 200 {
		return s[:197] + "..."
	}
	return s
}

func logEvent(db *state.DB, evt state.EventType, msg, beadID, anvil string) {
	if db == nil {
		return
	}
	_ = db.LogEvent(evt, msg, beadID, anvil)
}
