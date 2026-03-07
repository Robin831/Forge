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

// scanGo runs 'go list -m -u all' in the given directory if it has a go.mod.
// Returns nil (no updates) if the directory is not a Go project.
func scanGo(ctx context.Context, dir string, timeout time.Duration) ([]DependencyUpdate, error) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil, nil // not a Go project, skip silently
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "go", "list", "-m", "-u", "all"))
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m -u all: %w: %s", err, stderr.String())
	}

	return parseGoListOutput(string(output)), nil
}

// parseGoListOutput parses the output of 'go list -m -u all' and returns
// modules that have updates available. Each output line looks like:
//
//	github.com/foo/bar v1.2.3 [v1.4.0]
//
// Lines without brackets have no update. The first line (the main module) is skipped.
func parseGoListOutput(output string) []DependencyUpdate {
	var updates []DependencyUpdate

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

		updates = append(updates, DependencyUpdate{
			Ecosystem: EcosystemGo,
			Package:   modPath,
			Current:   current,
			Latest:    latest,
			Kind:      classifyUpdate(current, latest),
		})
	}

	return updates
}
