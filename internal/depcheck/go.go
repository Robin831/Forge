package depcheck

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// scanGo runs 'go list -m -u all' in an anvil directory if it has a go.mod.
// Returns nil if the anvil is not a Go project.
func (s *Scanner) scanGo(ctx context.Context, anvil, path string) *CheckResult {
	// Only check Go projects
	modPath := filepath.Join(path, "go.mod")
	if _, err := os.Stat(modPath); err != nil {
		if os.IsNotExist(err) {
			return nil // not a Go project
		}
		return &CheckResult{
			Anvil:   anvil,
			Path:    path,
			Checked: time.Now(),
			Error:   fmt.Errorf("stat %s: %w", modPath, err),
		}
	}

	result := &CheckResult{
		Anvil:     anvil,
		Path:      path,
		Ecosystem: "Go",
		Checked:   time.Now(),
	}

	cmdCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "go", "list", "-m", "-u", "all"))
	cmd.Dir = path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		result.Error = fmt.Errorf("go list -m -u all: %w: %s", err, stderr.String())
		return result
	}

	updates := parseGoListOutput(string(output))
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

	return result
}

// parseGoListOutput parses the output of 'go list -m -u all' and returns
// modules that have updates available. Each output line looks like:
//
//	github.com/foo/bar v1.2.3 [v1.4.0]
//
// Lines without brackets have no update. The first line (the main module) is skipped.
func parseGoListOutput(output string) []ModuleUpdate {
	var updates []ModuleUpdate

	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // skip main module
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Look for [vX.Y.Z] update marker
		bracketStart := strings.Index(line, "[")
		bracketEnd := strings.Index(line, "]")
		if bracketStart < 0 || bracketEnd < 0 || bracketEnd <= bracketStart {
			continue // no update available
		}

		latest := line[bracketStart+1 : bracketEnd]
		prefix := strings.TrimSpace(line[:bracketStart])
		parts := strings.Fields(prefix)
		if len(parts) < 2 {
			continue
		}

		modPath := parts[0]
		current := parts[1]

		kind := classifyUpdate(current, latest)

		updates = append(updates, ModuleUpdate{
			Path:    modPath,
			Current: current,
			Latest:  latest,
			Kind:    kind,
		})
	}

	sortUpdates(updates)
	return updates
}
