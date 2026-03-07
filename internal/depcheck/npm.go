package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// npmOutdatedEntry represents one package from 'npm outdated --json'.
type npmOutdatedEntry struct {
	Current  string `json:"current"`
	Wanted   string `json:"wanted"`
	Latest   string `json:"latest"`
	Location string `json:"location"`
}

// scanNpm runs 'npm outdated --json' in the given directory.
// subdir is the relative path within the anvil for labeling updates.
func scanNpm(ctx context.Context, dir, subdir string, timeout time.Duration) ([]DependencyUpdate, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "npm", "outdated", "--json"))
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// npm outdated exits non-zero when outdated packages exist — that's expected.
	// Only treat it as an error if we got no stdout at all.
	err := cmd.Run()
	if err != nil && stdout.Len() == 0 {
		return nil, fmt.Errorf("npm outdated: %w: %s", err, stderr.String())
	}

	return parseNpmOutput(stdout.Bytes(), subdir)
}

// parseNpmOutput parses the JSON output from npm outdated --json.
// The format is: { "package-name": { "current": "1.0.0", "wanted": "1.0.1", "latest": "2.0.0" }, ... }
func parseNpmOutput(data []byte, subdir string) ([]DependencyUpdate, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "{}" {
		return nil, nil
	}

	var packages map[string]npmOutdatedEntry
	if err := json.Unmarshal(data, &packages); err != nil {
		return nil, fmt.Errorf("parsing npm output: %w", err)
	}

	var updates []DependencyUpdate
	for name, entry := range packages {
		if entry.Current == "" || entry.Latest == "" || entry.Current == entry.Latest {
			continue
		}

		updates = append(updates, DependencyUpdate{
			Ecosystem: EcosystemNpm,
			Package:   name,
			Current:   entry.Current,
			Latest:    entry.Latest,
			Kind:      classifyUpdate(entry.Current, entry.Latest),
			Subdir:    subdir,
		})
	}

	return updates, nil
}
