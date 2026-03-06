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
	"os"
	"os/exec"
	"path/filepath"
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

	// Step 3: start the rebase and loop through all conflict rounds.
	// A rebase with N commits can produce N separate conflict stops; we call
	// Smith for each round and keep calling --continue until git exits 0.
	rebaseTarget := "origin/" + p.BaseBranch
	res := runGit(p.WorktreePath, "rebase", rebaseTarget)
	if res.err != nil {
		const maxConflictRounds = 20
		for round := 1; round <= maxConflictRounds; round++ {
			conflicted := conflictedFiles(p.WorktreePath)
			if len(conflicted) == 0 {
				// No conflict markers — git stopped for a different reason.
				_ = runGit(p.WorktreePath, "rebase", "--abort")
				return p.fail(fmt.Sprintf("git rebase %s failed (round %d, no conflicts): %s",
					rebaseTarget, round, firstLine(res.output)), res.err)
			}

			log.Printf("[rebase] PR #%d round %d: %d conflicted file(s) — invoking Smith",
				p.PRNumber, round, len(conflicted))
			if err := p.resolveWithSmith(ctx, conflicted, providers); err != nil {
				_ = runGit(p.WorktreePath, "rebase", "--abort")
				return p.fail(fmt.Sprintf("Smith could not resolve conflicts (round %d): %s",
					round, firstLine(err.Error())), err)
			}

			if r := runGit(p.WorktreePath, "add", "."); r.err != nil {
				_ = runGit(p.WorktreePath, "rebase", "--abort")
				return p.fail(fmt.Sprintf("git add after Smith (round %d): %s",
					round, firstLine(r.output)), r.err)
			}

			// --continue: applies the next commit. Exits 0 when the rebase is
			// fully complete; exits non-zero when another conflict round follows.
			// GIT_EDITOR=true prevents an interactive editor opening on Windows.
			continueCmd := exec.Command("git", "rebase", "--continue")
			continueCmd.Dir = p.WorktreePath
			continueCmd.Env = append(continueCmd.Environ(), "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
			executil.HideWindow(continueCmd)
			out, err := continueCmd.CombinedOutput()
			continueOut := strings.TrimSpace(string(out))
			if err == nil {
				// Rebase complete — break out of the loop.
				log.Printf("[rebase] PR #%d: rebase complete after %d conflict round(s)", p.PRNumber, round)
				break
			}

			// git rebase --continue exits non-zero for two reasons:
			//   a) more conflict rounds remain  → conflictedFiles() will be non-empty next iteration
			//   b) genuine failure (bad state)  → conflictedFiles() will be empty next iteration
			// Log what git said and let the next iteration decide.
			log.Printf("[rebase] PR #%d round %d: --continue output (will retry if more conflicts): %s",
				p.PRNumber, round, firstLine(continueOut))
			// Store the latest continue result to propagate if no conflicts found next round.
			res = cmdResult{output: continueOut, err: err}

			if round == maxConflictRounds {
				_ = runGit(p.WorktreePath, "rebase", "--abort")
				return p.fail(fmt.Sprintf("rebase exceeded %d conflict rounds; giving up", maxConflictRounds),
					fmt.Errorf("too many conflict rounds"))
			}
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
	prompt := buildConflictPrompt(p.WorktreePath, p.Branch, p.BaseBranch, conflicted)
	logDir := p.WorktreePath + "/.forge-logs"

	var lastErr error
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
		// Mirror reviewfix: a success subtype overrides the RateLimited flag
		// (Claude can set RateLimited=true AND ResultSubtype="success" on some responses).
		if result.ResultSubtype == "success" && !result.IsError {
			result.RateLimited = false
		}
		if result.RateLimited {
			lastErr = fmt.Errorf("provider %s rate-limited", pv.Kind)
			continue
		}
		if result.ExitCode != 0 || result.IsError {
			return fmt.Errorf("Smith (%s) exited %d (subtype=%s)", pv.Kind, result.ExitCode, result.ResultSubtype)
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("all %d providers rate-limited: %w", len(providers), lastErr)
	}
	return fmt.Errorf("all %d providers failed", len(providers))
}

// maxConflictFileBytes is the maximum bytes to inline per conflicted file in the prompt.
const maxConflictFileBytes = 8192

// buildConflictPrompt creates a detailed prompt asking the AI to resolve conflict markers,
// including the actual content of each conflicted file.
func buildConflictPrompt(worktreePath, branch, baseBranch string, conflicted []string) string {
	var sb strings.Builder
	sb.WriteString("You are resolving git merge conflicts that occurred while rebasing a pull request branch onto main.\n\n")
	sb.WriteString(fmt.Sprintf("Branch being rebased: %s\n", branch))
	sb.WriteString(fmt.Sprintf("Rebasing onto: origin/%s\n\n", baseBranch))
	sb.WriteString("In a rebase conflict:\n")
	sb.WriteString("  - The section between <<<<<<< HEAD and ======= is the CURRENT upstream code (from origin/" + baseBranch + ")\n")
	sb.WriteString("  - The section between ======= and >>>>>>> is the INCOMING change from the PR branch\n\n")
	sb.WriteString("Your task: resolve ALL conflicts in the files below. Make a clear, decisive decision\n")
	sb.WriteString("about what the final code should look like. In most cases the PR's incoming changes\n")
	sb.WriteString("are the intended contribution — keep them unless they obviously conflict with the\n")
	sb.WriteString("upstream logic (e.g. a function was renamed or removed). When both sides add code,\n")
	sb.WriteString("combine them. When they contradict, prefer the PR's changes.\n\n")
	sb.WriteString("RULES:\n")
	sb.WriteString("1. Remove EVERY conflict marker: <<<<<<<, =======, >>>>>>>.\n")
	sb.WriteString("2. Do NOT leave any unresolved marker in the file.\n")
	sb.WriteString("3. Edit the files directly — do NOT run any git commands.\n")
	sb.WriteString("4. Preserve correct syntax (Go, TypeScript, YAML, etc.) in the final file.\n\n")

	sb.WriteString("---\nCONFLICTED FILES:\n---\n\n")
	for _, f := range conflicted {
		sb.WriteString(fmt.Sprintf("### %s\n", f))
		fulPath := filepath.Join(worktreePath, f)
		data, err := os.ReadFile(fulPath)
		if err != nil {
			sb.WriteString(fmt.Sprintf("(could not read file: %v)\n", err))
		} else {
			contents := data
			truncated := false
			if len(contents) > maxConflictFileBytes {
				contents = contents[:maxConflictFileBytes]
				truncated = true
			}
			sb.WriteString("```\n")
			sb.Write(contents)
			if truncated {
				sb.WriteString(fmt.Sprintf("\n... (truncated; full file at %s)\n", fulPath))
			}
			sb.WriteString("```\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n")
	sb.WriteString("Now edit each file to resolve its conflicts and write the corrected content back.\n")
	sb.WriteString(fmt.Sprintf("Working directory: %s\n", worktreePath))
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
