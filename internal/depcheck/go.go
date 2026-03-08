package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// scanGo runs 'go list -m -u -json all' in an anvil directory if it has a go.mod.
// Only direct dependencies are included — indirect (transitive) deps cannot be
// independently upgraded and would cause Smith to produce no-diff results.
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

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "go", "list", "-m", "-u", "-json", "all"))
	cmd.Dir = path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		result.Error = fmt.Errorf("go list -m -u -json all: %w: %s", err, stderr.String())
		return result
	}

	updates := parseGoJSONOutput(output)
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

// goModule represents a single module entry from 'go list -m -u -json all'.
type goModule struct {
	Path     string    `json:"Path"`
	Version  string    `json:"Version"`
	Indirect bool      `json:"Indirect"`
	Main     bool      `json:"Main"`
	Update   *goUpdate `json:"Update"`
}

type goUpdate struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
}

// parseGoJSONOutput parses the JSON-stream output of 'go list -m -u -json all'.
// go list -json outputs concatenated JSON objects (not an array), so we use
// a streaming decoder. Only direct dependencies with available updates are returned.
func parseGoJSONOutput(data []byte) []ModuleUpdate {
	var updates []ModuleUpdate

	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var mod goModule
		if err := dec.Decode(&mod); err != nil {
			break
		}

		// Skip the main module, indirect deps, and modules without updates.
		if mod.Main || mod.Indirect || mod.Update == nil {
			continue
		}

		kind := classifyUpdate(mod.Version, mod.Update.Version)
		updates = append(updates, ModuleUpdate{
			Path:    mod.Path,
			Current: mod.Version,
			Latest:  mod.Update.Version,
			Kind:    kind,
		})
	}

	sortUpdates(updates)
	return updates
}
