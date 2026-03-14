package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// runNpmOutdatedFn is the function used to invoke npm outdated. It is a
// package-level variable so tests can replace it without requiring npm to be
// installed on the test machine.
var runNpmOutdatedFn = runNpmOutdated

// scanNpm runs 'npm outdated --json' in directories containing package.json.
// Skips node_modules, .workers, .worktrees, bin, and obj directories.
// Deduplicates packages across projects. Returns nil if no package.json found.
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

	// Track seen packages across all package.json files to avoid duplicates
	// when the same package appears in multiple projects (e.g. worktree copies).
	seen := map[string]bool{}

	for _, dir := range pkgDirs {
		updates, err := runNpmOutdatedFn(ctx, s.timeout, dir)
		if err != nil {
			result.Error = fmt.Errorf("npm outdated in %s: %w", dir, err)
			return result
		}

		for _, u := range updates {
			if seen[u.Path] {
				continue
			}
			seen[u.Path] = true
			switch u.Kind {
			case "patch":
				result.Patch = append(result.Patch, u)
			case "minor":
				result.Minor = append(result.Minor, u)
			case "major":
				result.Major = append(result.Major, u)
			}
		}
	}

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

// runNpmOutdated runs 'npm outdated --json' in the given directory and
// parses the output into ModuleUpdate entries.
func runNpmOutdated(ctx context.Context, timeout time.Duration, dir string) ([]ModuleUpdate, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "npm", "outdated", "--json"))
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// npm outdated exits with code 1 when outdated packages exist — that is
	// expected. Any other error type (binary not found, context cancelled, etc.)
	// indicates the scan could not run at all and should be propagated.
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, fmt.Errorf("npm outdated: %w", err)
		}
		// ExitError is expected when packages are outdated; continue parsing.
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" || output == "{}" {
		return nil, nil
	}

	var outdated map[string]npmOutdatedEntry
	if err := json.Unmarshal([]byte(output), &outdated); err != nil {
		return nil, fmt.Errorf("parsing npm outdated output: %w\nstderr: %s", err, stderr.String())
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
