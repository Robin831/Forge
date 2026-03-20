package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

// scanNpm runs 'npm outdated --json' in directories containing package.json.
// Skips node_modules, .workers, .worktrees, bin, obj, and .git directories
// (via findNpmProjects). Deduplicates packages across projects, keeping the
// most severe update (major > minor > patch) when the same package appears
// in multiple package.json files. Returns nil if no package.json found.
func (s *Scanner) scanNpm(ctx context.Context, anvil, path string) *CheckResult {
	pkgDirs := findNpmProjects(path)
	if len(pkgDirs) == 0 {
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

	for _, dir := range pkgDirs {
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

	for _, u := range best {
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

// findNpmProjects walks the anvil directory for package.json files,
// skipping node_modules, .workers, .worktrees, bin, obj, and .git directories.
func findNpmProjects(root string) []string {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".workers" || name == ".worktrees" || name == "bin" || name == "obj" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "package.json" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
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
			Path:    pkg,
			Current: entry.Current,
			Latest:  entry.Latest,
			Kind:    kind,
		})
	}

	sortUpdates(updates)
	return updates, nil
}
