package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// scanNpm runs 'npm outdated --json' in directories containing package.json.
// Skips node_modules and .workers directories. Returns nil if no package.json found.
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

	for _, dir := range pkgDirs {
		updates, err := runNpmOutdated(ctx, s.timeout, dir)
		if err != nil {
			result.Error = fmt.Errorf("npm outdated in %s: %w", dir, err)
			return result
		}

		for _, u := range updates {
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
// skipping node_modules and .workers directories.
func findNpmProjects(root string) []string {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".workers" || name == ".git" {
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

	// npm outdated returns exit code 1 when outdated packages exist,
	// so we ignore the error and check the output instead.
	_ = cmd.Run()

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

	order := map[string]int{"major": 0, "minor": 1, "patch": 2}
	sort.Slice(updates, func(i, j int) bool {
		if updates[i].Kind != updates[j].Kind {
			return order[updates[i].Kind] < order[updates[j].Kind]
		}
		return updates[i].Path < updates[j].Path
	})

	return updates, nil
}
