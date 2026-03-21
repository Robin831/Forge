package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// branchName returns the batch-update branch name for the given date string
// (formatted as YYYY-MM-DD).
func branchName(dateStr string) string {
	return "deps/batch-update-" + dateStr
}

// CreatePR creates or checks out a batch-update branch, pushes it to origin,
// and opens a single pull request summarising all updated groups for the given
// anvil. It returns the PR URL reported by gh, or an error if any step fails.
//
// The branch name is deps/batch-update-<YYYY-MM-DD> derived from the current
// date. If the branch already exists locally it is checked out rather than
// created anew. If it exists only on the remote, it is fetched and tracked.
func CreatePR(ctx context.Context, anvilPath, anvilName string, groups []UpdateGroup) (string, error) {
	dateStr := time.Now().Format("2006-01-02")
	branch := branchName(dateStr)

	// git helper: run a git command with a per-command timeout.
	git := func(timeout time.Duration, args ...string) error {
		cmdCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
		cmd.Dir = anvilPath
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
		}
		return nil
	}

	// Try to create the branch. On failure, determine whether the branch
	// already exists locally or only on the remote, and handle accordingly.
	if err := git(30*time.Second, "checkout", "-b", branch); err != nil {
		// Check if the branch exists locally.
		if errExists := git(10*time.Second, "rev-parse", "--verify", branch); errExists == nil {
			// Branch exists locally — just check it out.
			if err2 := git(30*time.Second, "checkout", branch); err2 != nil {
				return "", fmt.Errorf("depupdate: checkout existing branch %q: %w", branch, err2)
			}
		} else {
			// Branch doesn't exist locally; try to fetch it from the remote.
			if err3 := git(30*time.Second, "fetch", "origin", branch); err3 != nil {
				// Not on remote either — surface the original creation error.
				return "", fmt.Errorf("depupdate: create branch %q: %w", branch, err)
			}
			remoteRef := "origin/" + branch
			if err4 := git(30*time.Second, "checkout", "-B", branch, remoteRef); err4 != nil {
				return "", fmt.Errorf("depupdate: checkout branch %q from %q: %w", branch, remoteRef, err4)
			}
		}
	}

	// Push branch and set upstream tracking reference. Allow more time for push.
	if err := git(120*time.Second, "push", "--set-upstream", "origin", branch); err != nil {
		return "", fmt.Errorf("depupdate: push branch %q: %w", branch, err)
	}

	baseBranch := detectDefaultBranch(ctx, anvilPath)
	title := fmt.Sprintf("chore(deps): batch update %s %s", anvilName, dateStr)
	body := buildPRBody(groups, dateStr)

	ghCtx, ghCancel := context.WithTimeout(ctx, 60*time.Second)
	defer ghCancel()

	// Use --json url -q .url so the output is always exactly the PR URL,
	// regardless of any extra warnings or prompts gh might emit.
	cmd := executil.HideWindow(exec.CommandContext(ghCtx, "gh", "pr", "create",
		"--title", title,
		"--body", body,
		"--head", branch,
		"--base", baseBranch,
		"--json", "url",
		"-q", ".url",
	))
	cmd.Dir = anvilPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		if strings.Contains(stderrStr, "already exists") {
			// PR already open for this branch — try to extract URL from
			// combined stdout+stderr first, then fall back to gh pr view.
			existing := extractPRURL(stdout.String() + " " + stderrStr)
			if existing == "" {
				viewCtx, viewCancel := context.WithTimeout(ctx, 30*time.Second)
				defer viewCancel()
				viewCmd := executil.HideWindow(exec.CommandContext(viewCtx, "gh", "pr", "view",
					"--head", branch, "--json", "url", "-q", ".url"))
				viewCmd.Dir = anvilPath
				var viewOut bytes.Buffer
				viewCmd.Stdout = &viewOut
				if viewErr := viewCmd.Run(); viewErr == nil {
					existing = strings.TrimSpace(viewOut.String())
				}
			}
			if existing == "" {
				return "", fmt.Errorf("depupdate: PR already exists for %s but URL could not be determined", anvilName)
			}
			log.Printf("[depupdate] PR already exists for %s on branch %s: %s", anvilName, branch, existing)
			return existing, nil
		}
		return "", fmt.Errorf("depupdate: gh pr create failed: %w\nstderr: %s", err, stderrStr)
	}

	prURL := strings.TrimSpace(stdout.String())
	log.Printf("[depupdate] Created PR for %s: %s", anvilName, prURL)
	return prURL, nil
}

// detectDefaultBranch returns the remote's default branch name (e.g. "main").
// Falls back to "main" if the symbolic ref cannot be resolved.
func detectDefaultBranch(ctx context.Context, anvilPath string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"))
	cmd.Dir = anvilPath
	out, err := cmd.Output()
	if err != nil {
		return "main"
	}
	ref := strings.TrimSpace(string(out))
	// ref is like "origin/main" — strip the remote prefix.
	if _, after, ok := strings.Cut(ref, "/"); ok {
		return after
	}
	return ref
}

// buildPRBody constructs a markdown PR description grouped by update kind.
func buildPRBody(groups []UpdateGroup, date string) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Automated dependency batch update for %s.\n\n", date)

	// Collect groups by kind.
	byKind := map[string][]UpdateGroup{
		"major": {},
		"minor": {},
		"patch": {},
	}
	for _, g := range groups {
		k := g.Kind
		if k != "major" && k != "minor" {
			k = "patch"
		}
		byKind[k] = append(byKind[k], g)
	}

	writeSection := func(heading string, gs []UpdateGroup) {
		if len(gs) == 0 {
			return
		}
		fmt.Fprintf(&sb, "## %s\n\n", heading)
		for _, g := range gs {
			fmt.Fprintf(&sb, "### %s\n\n", g.Name)
			for _, u := range g.Updates {
				fmt.Fprintf(&sb, "- `%s`: %s → %s\n", u.Path, u.Current, u.Latest)
			}
			sb.WriteString("\n")
		}
	}

	writeSection("Major Updates", byKind["major"])
	writeSection("Minor Updates", byKind["minor"])
	writeSection("Patch Updates", byKind["patch"])

	sb.WriteString("---\n_Generated by Forge depupdate._\n")
	return sb.String()
}

// extractPRURL attempts to find a GitHub PR URL in a string (e.g. from gh stderr).
func extractPRURL(s string) string {
	for _, word := range strings.Fields(s) {
		if strings.HasPrefix(word, "https://github.com/") && strings.Contains(word, "/pull/") {
			return strings.TrimRight(word, ".,;")
		}
	}
	return ""
}
