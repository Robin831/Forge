package depupdate

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
)

// branchName returns the batch-update branch name for the given date string
// (formatted as YYYY-MM-DD).
func branchName(dateStr string) string {
	return "deps/batch-update-" + dateStr
}

// CheckoutUpdateBranch creates or checks out the batch-update branch for the
// current date (deps/batch-update-<YYYY-MM-DD>) in the given anvil directory.
// The branch is based on origin's default branch so that the diff never
// includes unrelated changes from a feature branch or detached HEAD.
// If the branch already exists locally it is checked out. If it exists only on
// the remote it is fetched and tracked.
// Returns the branch name and an error if the branch cannot be created or
// checked out.
func CheckoutUpdateBranch(ctx context.Context, anvilPath string) (string, error) {
	dateStr := time.Now().Format("2006-01-02")
	branch := branchName(dateStr)

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

	gitOutput := func(timeout time.Duration, args ...string) (string, error) {
		cmdCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
		cmd.Dir = anvilPath
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
		}
		return stdout.String(), nil
	}

	// Determine the default branch on origin so the deps branch is based on
	// origin/<default> rather than whatever HEAD is currently checked out.
	// Fall back to creating from the current HEAD if detection fails.
	var baseRef string
	if remotesOut, err := gitOutput(5*time.Second, "remote"); err == nil {
		hasOrigin := false
		for _, line := range strings.Split(remotesOut, "\n") {
			if strings.TrimSpace(line) == "origin" {
				hasOrigin = true
				break
			}
		}
		if hasOrigin {
			// Prefer the symbolic origin/HEAD ref (e.g. "origin/main").
			if headRef, err := gitOutput(5*time.Second, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
				headRef = strings.TrimSpace(headRef)
				if headRef != "" {
					if strings.HasPrefix(headRef, "origin/") {
						baseRef = headRef
					} else {
						baseRef = "origin/" + headRef
					}
				}
			} else {
				// Fallback: parse "git remote show origin" for the "HEAD branch:" line.
				if remoteShow, err2 := gitOutput(10*time.Second, "remote", "show", "origin"); err2 == nil {
					for _, line := range strings.Split(remoteShow, "\n") {
						line = strings.TrimSpace(line)
						const prefix = "HEAD branch:"
						if strings.HasPrefix(line, prefix) {
							name := strings.TrimSpace(strings.TrimPrefix(line, prefix))
							if name != "" {
								baseRef = "origin/" + name
							}
							break
						}
					}
				}
			}
		}
	}

	// If we resolved a default-branch ref on origin, fetch it and base the
	// deps branch on it. Otherwise fall back to creating from the current HEAD.
	if baseRef != "" {
		defaultBranchName := strings.TrimPrefix(baseRef, "origin/")
		if defaultBranchName == "" || strings.Contains(defaultBranchName, " ") {
			log.Printf("depupdate: invalid default branch ref %q, falling back to current HEAD", baseRef)
			baseRef = ""
		} else if err := git(30*time.Second, "fetch", "origin", defaultBranchName); err != nil {
			log.Printf("depupdate: failed to fetch %q: %v; falling back to current HEAD", baseRef, err)
			baseRef = ""
		}
	}

	if baseRef != "" {
		if err := git(30*time.Second, "checkout", "-B", branch, baseRef); err != nil {
			return "", fmt.Errorf("depupdate: create branch %q from %q: %w", branch, baseRef, err)
		}
		return branch, nil
	}

	// No origin or detection failed — fall back to the existing local/remote logic.
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
	return branch, nil
}

// CreatePR pushes the given branch to origin and opens a single pull request
// summarising all updated groups for the given anvil. The branch must have
// already been created and checked out by CheckoutUpdateBranch. Returns the PR
// URL reported by gh, or an error if any step fails.
func CreatePR(ctx context.Context, anvilPath, anvilName, branch string, groups []UpdateGroup) (string, error) {
	dateStr := time.Now().Format("2006-01-02")

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

// minimalBead is used for parsing bd list --json output.
type minimalBead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CloseMatchingDepBeads closes any open depcheck beads whose title references a
// package covered by one of the provided UpdateGroups. It is called after
// CreatePR returns successfully so that stale "Deps(<eco>): update <pkg> …"
// beads are removed from the queue instead of accumulating as resolved work.
func CloseMatchingDepBeads(ctx context.Context, anvilPath string, groups []UpdateGroup) error {
	// Build a set of all package paths covered by the groups.
	pkgSet := make(map[string]struct{})
	for _, g := range groups {
		for _, u := range g.Updates {
			pkgSet[u.Path] = struct{}{}
		}
	}
	if len(pkgSet) == 0 {
		return nil
	}

	// Fetch open beads from the anvil directory.
	listCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(listCtx, "bd", "list", "--status=open", "--limit", "0", "--json"))
	cmd.Dir = anvilPath
	var listErr bytes.Buffer
	cmd.Stderr = &listErr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("depupdate: bd list --status=open in %s: %w (stderr: %s)", anvilPath, err, strings.TrimSpace(listErr.String()))
	}

	var beads []minimalBead
	if err := json.Unmarshal(out, &beads); err != nil {
		return fmt.Errorf("depupdate: parse bd list output: %w", err)
	}

	const closeReason = "Updated via forge update-deps"

	for _, b := range beads {
		if !isDepUpdateTitle(b.Title) {
			continue
		}
		pkg := extractPackageFromTitle(b.Title)
		if _, ok := pkgSet[pkg]; !ok {
			continue
		}
		closeCtx, closeCancel := context.WithTimeout(ctx, 30*time.Second)
		closeCmd := executil.HideWindow(exec.CommandContext(closeCtx, "bd", "close", b.ID, "--reason", closeReason))
		closeCmd.Dir = anvilPath
		var closeErr bytes.Buffer
		closeCmd.Stderr = &closeErr
		if err := closeCmd.Run(); err != nil {
			log.Printf("[depupdate] warning: bd close %s failed: %v (%s)", b.ID, err, strings.TrimSpace(closeErr.String()))
		} else {
			log.Printf("[depupdate] Closed stale dep bead %s (%s)", b.ID, pkg)
		}
		closeCancel()
	}

	return nil
}

// isDepUpdateTitle returns true when the bead title follows the standardized
// depcheck format: "Deps(<ecosystem>): update <package> …"
func isDepUpdateTitle(title string) bool {
	return strings.HasPrefix(title, "Deps(") && strings.Contains(title, "): update ")
}

// extractPackageFromTitle parses the package name out of a depcheck bead title
// of the form "Deps(<ecosystem>): update <package> <old> → <new>".
// Returns an empty string when the title does not match.
func extractPackageFromTitle(title string) string {
	// Find ": update " separator.
	const sep = "): update "
	idx := strings.Index(title, sep)
	if idx < 0 {
		return ""
	}
	rest := title[idx+len(sep):]
	// The package name is the first whitespace-delimited token.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
