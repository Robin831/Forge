package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// runNpmInstallFn is the function used to invoke npm ci/install. It is a
// package-level variable so tests can replace it without requiring npm.
var runNpmInstallFn = runNpmInstall

// runNpmOutdatedFn is the function used to invoke npm outdated. It is a
// package-level variable so tests can replace it without requiring npm to be
// installed on the test machine.
var runNpmOutdatedFn = runNpmOutdated

// runNpmCmdFn is the function used to execute npm subcommands. It is a
// package-level variable so tests can stub npm command execution without
// requiring npm to be installed.
var runNpmCmdFn = runNpmCmd

// scanNpm runs 'npm outdated --json' in the directories src reports a
// package.json in. Deduplicates packages across projects, keeping the most
// severe update (major > minor > patch) when the same package appears in
// multiple package.json files. Returns nil if no package.json found, or if a
// live Kiln preview holds the anvil's node_modules (see previewHoldsNodeModules).
func (s *Scanner) scanNpm(ctx context.Context, anvil, path string, src manifestSource) *CheckResult {
	if s.previewHoldsNodeModules(anvil) {
		return nil
	}

	paths, err := src.Paths(ctx)
	if err != nil {
		return &CheckResult{
			Anvil:   anvil,
			Path:    path,
			Checked: time.Now(),
			Error:   fmt.Errorf("listing files in %s: %w", src.Describe(), err),
		}
	}

	pkgFiles := npmPackageFiles(paths)
	if len(pkgFiles) == 0 {
		return nil
	}

	result := &CheckResult{
		Anvil:     anvil,
		Path:      path,
		Ecosystem: "npm",
		Checked:   time.Now(),
	}

	// kindRank maps update kind to a numeric severity so we can keep the most
	// severe update when the same package appears in multiple package.json files.
	kindRank := map[string]int{"patch": 0, "minor": 1, "major": 2}

	// Track the best (most severe) update seen per package across all projects.
	best := map[string]ModuleUpdate{}
	// The ranges every committed package.json declares, folded together.
	committed := map[string]string{}

	for _, rel := range pkgFiles {
		if data, readErr := src.Read(ctx, rel); readErr == nil {
			for name, version := range parsePackageJSONDeps(data) {
				committed[name] = version
			}
		}

		dir := localDir(path, rel)
		// A project tracked at the ref but absent from the checkout has no
		// node_modules for npm to read; skip it rather than fail the ecosystem.
		if _, statErr := os.Stat(dir); statErr != nil {
			continue
		}

		// Re-check immediately before the call whose first act is `npm ci`: a
		// preview can start after the scan began. What is left is the window
		// between this check and the spawn a few statements later, which is
		// accepted — a preview starting inside it is no more likely than the
		// pre-existing race with a worker's own npm build step.
		if s.previewHoldsNodeModules(anvil) {
			return nil
		}

		updates, err := runNpmOutdatedFn(ctx, s.timeout, dir)
		if err != nil {
			result.Error = fmt.Errorf("npm outdated in %s: %w", dir, err)
			return result
		}

		for _, u := range updates {
			existing, ok := best[u.Path]
			if !ok || kindRank[u.Kind] > kindRank[existing.Kind] {
				best[u.Path] = u
			}
		}
	}

	deduped := make([]ModuleUpdate, 0, len(best))
	for _, u := range best {
		deduped = append(deduped, u)
	}
	// package.json declares ranges rather than resolved versions, so an entry
	// upstream has already bumped to the latest is dropped, but nothing the
	// installed tree reported is rewritten from a range.
	for _, u := range reconcileWithCommitted(deduped, committed, false) {
		switch u.Kind {
		case "patch":
			result.Patch = append(result.Patch, u)
		case "minor":
			result.Minor = append(result.Minor, u)
		case "major":
			result.Major = append(result.Major, u)
		}
	}
	sortUpdates(result.Patch)
	sortUpdates(result.Minor)
	sortUpdates(result.Major)

	return result
}

// previewHoldsNodeModules reports whether a live Kiln preview on this anvil
// makes the npm scan unsafe, logging the bead holding it so a skipped cycle is
// explainable from the log alone.
//
// runNpmOutdated syncs node_modules with `npm ci` before reading versions, and
// `npm ci` begins by deleting node_modules. A preview's worktree has that same
// node_modules linked into it (see internal/worktree/nodemodules.go), so the
// delete reaches through the link: on Windows it aborts partway on the first
// locked native module and leaves the checkout gutted, and on Linux it succeeds
// while the preview's dev server keeps serving from deleted inodes. Both leave
// every worktree linked to the checkout without its dependencies.
//
// The whole npm half of the cycle is skipped, not just the sync: reporting
// versions read from a tree we deliberately refused to sync would be more
// misleading than reporting nothing. .NET and Go scanning are unaffected —
// neither deletes anything.
func (s *Scanner) previewHoldsNodeModules(anvil string) bool {
	bead := s.previewHolder(anvil)
	if bead == "" {
		return false
	}
	log.Printf("[depcheck] %s: skipping npm scan — Kiln preview for bead %s holds this checkout's node_modules", anvil, bead)
	return true
}

// npmPackageFiles selects the package.json files among a list of repo-relative
// paths, in lexicographic order so a run is reproducible.
func npmPackageFiles(paths []string) []string {
	var files []string
	for _, p := range paths {
		if p == "package.json" || strings.HasSuffix(p, "/package.json") {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files
}

// findNpmProjects is npmPackageFiles over a working-tree walk, returning the
// absolute directories holding each package.json.
func findNpmProjects(root string) []string {
	var dirs []string
	for _, rel := range npmPackageFiles(walkWorktreePaths(root)) {
		dirs = append(dirs, localDir(root, rel))
	}
	return dirs
}

// npmOutdatedEntry represents a single entry from 'npm outdated --json'.
type npmOutdatedEntry struct {
	Current string `json:"current"`
	Wanted  string `json:"wanted"`
	Latest  string `json:"latest"`
}

// runNpmInstall ensures node_modules are synced with package-lock.json for
// accurate 'npm outdated' results. If package-lock.json is present, it runs
// 'npm ci --ignore-scripts'. If no lock file exists, it is a no-op to avoid
// creating or mutating tracked files in the worktree.
// If timeout is 0, ctx is used as-is (the caller is expected to own the deadline).
func runNpmInstall(ctx context.Context, timeout time.Duration, dir string) error {
	lockPath := filepath.Join(dir, "package-lock.json")
	if _, err := os.Stat(lockPath); err != nil {
		if os.IsNotExist(err) {
			// No lock file present; skip syncing to avoid generating package-lock.json.
			return nil
		}
		return fmt.Errorf("stat package-lock.json: %w", err)
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "npm", "ci", "--ignore-scripts"))
	cmd.Dir = dir

	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm ci --ignore-scripts: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// runNpmCmd executes npm with the given args in dir, returning stdout bytes.
// If timeout is 0, ctx is used as-is (the caller is expected to own the deadline).
// Stdout is returned alongside any error so callers can inspect output even when
// npm exits with a non-zero code (e.g. exit status 1 from 'npm outdated').
func runNpmCmd(ctx context.Context, timeout time.Duration, dir string, args ...string) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "npm", args...))
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("npm %s: %w (stderr: %s)", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// runNpmOutdated runs 'npm outdated --json' in the given directory and
// parses the output into ModuleUpdate entries. It first runs npm ci/install
// to sync node_modules with the lock file so reported versions are accurate.
// A single context deadline covers both the install and outdated steps.
func runNpmOutdated(ctx context.Context, timeout time.Duration, dir string) ([]ModuleUpdate, error) {
	// Enforce a single overall deadline across both install and outdated so
	// neither step can individually consume the full timeout budget.
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Sync node_modules with lock file before checking for outdated packages.
	// If install fails, log and continue — stale data is better than no data.
	if err := runNpmInstallFn(cmdCtx, 0, dir); err != nil {
		log.Printf("[depcheck] warning: %s in %s, continuing with potentially stale versions", err, dir)
	}

	// npm outdated exits with code 1 when outdated packages exist — that is
	// expected. Any other error type (binary not found, context cancelled, etc.)
	// indicates the scan could not run at all and should be propagated.
	out, err := runNpmCmdFn(cmdCtx, 0, dir, "outdated", "--json")
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, err
		}
		// ExitError is expected when packages are outdated; continue parsing.
	}

	output := strings.TrimSpace(string(out))
	if output == "" || output == "{}" {
		return nil, nil
	}

	var outdated map[string]npmOutdatedEntry
	if err := json.Unmarshal([]byte(output), &outdated); err != nil {
		return nil, fmt.Errorf("parsing npm outdated output: %w", err)
	}

	var updates []ModuleUpdate
	for pkg, entry := range outdated {
		if entry.Current == "" || entry.Latest == "" {
			continue
		}
		if entry.Current == entry.Latest {
			continue
		}

		kind := classifyUpdate(entry.Current, entry.Latest)
		updates = append(updates, ModuleUpdate{
			Path:      pkg,
			Current:   entry.Current,
			Latest:    entry.Latest,
			Kind:      kind,
			SourceDir: dir,
		})
	}

	sortUpdates(updates)
	return updates, nil
}
