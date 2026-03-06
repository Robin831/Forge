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
	// WorkerID is the state DB worker ID, used to update the log path
	// so the Hearth TUI can display live activity.
	WorkerID string
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

	// Step 1: fetch so Smith has up-to-date refs.
	if res := runGit(p.WorktreePath, "fetch", "origin"); res.err != nil {
		return p.fail(fmt.Sprintf("git fetch failed: %s", firstLine(res.output)), res.err)
	}

	// Step 2: abort any in-progress rebase so the tree is clean.
	_ = runGit(p.WorktreePath, "rebase", "--abort")

	// Step 3: hand the entire rebase (including conflict resolution) to Smith
	// in a single call. Smith runs the rebase, resolves every conflict round,
	// and pushes the result — no round-tripping between us and the AI.
	if err := p.rebaseWithSmith(ctx, providers); err != nil {
		return p.fail(firstLine(err.Error()), err)
	}

	msg := fmt.Sprintf("PR #%d: rebased onto origin/%s and pushed successfully", p.PRNumber, p.BaseBranch)
	log.Printf("[rebase] %s", msg)
	logEvent(p.DB, state.EventRebaseSuccess, msg, p.BeadID, p.AnvilName)
	return Result{Success: true}
}

// rebaseWithSmith runs a single Smith session that performs the entire rebase:
// starts it, resolves any conflict rounds, and pushes the result.
func (p *Params) rebaseWithSmith(ctx context.Context, providers []provider.Provider) error {
	prompt := buildRebasePrompt(p.WorktreePath, p.Branch, p.BaseBranch)
	logDir := p.WorktreePath + "/.forge-logs"

	var lastErr error
	for pi, pv := range providers {
		if pi > 0 {
			log.Printf("[rebase] PR #%d: provider %s rate-limited, trying %s",
				p.PRNumber, providers[pi-1].Label(), pv.Label())
		}
		process, err := smith.SpawnWithProvider(ctx, p.WorktreePath, prompt, logDir, pv, p.ExtraFlags)
		if err != nil {
			return fmt.Errorf("spawning Smith (%s): %w", pv.Label(), err)
		}
		if p.WorkerID != "" && p.DB != nil {
			if err := p.DB.UpdateWorkerLogPath(p.WorkerID, process.LogPath); err != nil {
				log.Printf("[rebase] warning: failed to update worker log path for worker %s: %v", p.WorkerID, err)
			}
		}
		result := process.Wait()
		if result.ResultSubtype == "success" && !result.IsError {
			result.RateLimited = false
		}
		if result.RateLimited {
			lastErr = fmt.Errorf("provider %s rate-limited", pv.Label())
			continue
		}
		if result.ExitCode != 0 || result.IsError {
			return fmt.Errorf("Smith (%s) exited %d (subtype=%s)", pv.Label(), result.ExitCode, result.ResultSubtype)
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("all %d providers rate-limited: %w", len(providers), lastErr)
	}
	return fmt.Errorf("all %d providers failed", len(providers))
}

// buildRebasePrompt returns a prompt instructing Smith to run the complete
// rebase workflow — start, resolve all conflict rounds, push — in one session.
func buildRebasePrompt(worktreePath, branch, baseBranch string) string {
	var sb strings.Builder
	target := "origin/" + baseBranch

	sb.WriteString("You are rebasing a pull request branch onto the latest upstream code.\n\n")
	sb.WriteString(fmt.Sprintf("Working directory: %s\n", worktreePath))
	sb.WriteString(fmt.Sprintf("PR branch: %s\n", branch))
	sb.WriteString(fmt.Sprintf("Rebasing onto: %s\n\n", target))

	sb.WriteString("## Your job\n\n")
	sb.WriteString("Run the complete rebase workflow from start to finish:\n\n")
	sb.WriteString("1. Run: `GIT_EDITOR=true GIT_SEQUENCE_EDITOR=true git rebase " + target + "`\n")
	sb.WriteString("2. If it exits 0 immediately — no conflicts, skip to step 6.\n")
	sb.WriteString("3. If it exits non-zero — check for conflicted files:\n")
	sb.WriteString("   `git diff --name-only --diff-filter=U`\n")
	sb.WriteString("4. For EACH conflicted file: read it, remove ALL conflict markers\n")
	sb.WriteString("   (`<<<<<<< HEAD`, `=======`, `>>>>>>>`) and write the resolved content back.\n")
	sb.WriteString("   - Between `<<<<<<< HEAD` and `=======` is the UPSTREAM code (" + baseBranch + ").\n")
	sb.WriteString("   - Between `=======` and `>>>>>>>` is the PR branch's incoming change.\n")
	sb.WriteString("   - Prefer the PR's incoming change unless it clearly conflicts with\n")
	sb.WriteString("     an upstream rename or removal. When both sides add independent code,\n")
	sb.WriteString("     keep both. Never leave a conflict marker in the file.\n")
	sb.WriteString("5. Run: `git add .` then `GIT_EDITOR=true GIT_SEQUENCE_EDITOR=true git rebase --continue`\n")
	sb.WriteString("   - If it exits 0 → rebase complete, go to step 6.\n")
	sb.WriteString("   - If it exits non-zero → more conflicts; go back to step 3.\n")
	sb.WriteString("   Repeat until `git rebase --continue` exits 0.\n")
	sb.WriteString("6. Run: `git push --force-with-lease origin " + branch + "`\n\n")

	sb.WriteString("## Rules\n\n")
	sb.WriteString("- Always use `GIT_EDITOR=true GIT_SEQUENCE_EDITOR=true` before every git rebase command\n")
	sb.WriteString("  to prevent interactive editor prompts.\n")
	sb.WriteString("- Do NOT open a PR, do NOT commit individually — the rebase handles commits.\n")
	sb.WriteString("- If `git push --force-with-lease` fails, try once with `git push --force origin " + branch + "`.\n")
	sb.WriteString("- If the rebase cannot be completed (e.g. a file was deleted upstream),\n")
	sb.WriteString("  run `git rebase --abort` and report the error clearly.\n")

	return sb.String()
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
