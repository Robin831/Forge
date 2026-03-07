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

// dotnetOutdatedOutput represents the JSON output of 'dotnet list package --outdated --format json'.
type dotnetOutdatedOutput struct {
	Version    int              `json:"version"`
	Parameters json.RawMessage  `json:"parameters"`
	Projects   []dotnetProject  `json:"projects"`
}

type dotnetProject struct {
	Path       string                  `json:"path"`
	Frameworks []dotnetTargetFramework `json:"frameworks"`
}

type dotnetTargetFramework struct {
	Framework    string          `json:"framework"`
	TopLevelPkgs []dotnetPackage `json:"topLevelPackages"`
}

type dotnetPackage struct {
	ID               string `json:"id"`
	RequestedVersion string `json:"requestedVersion"`
	ResolvedVersion  string `json:"resolvedVersion"`
	LatestVersion    string `json:"latestVersion"`
}

// scanDotnet runs 'dotnet list package --outdated --format json' in the given directory.
// subdir is the relative path within the anvil for labeling updates.
func scanDotnet(ctx context.Context, dir, subdir string, timeout time.Duration) ([]DependencyUpdate, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"dotnet", "list", "package", "--outdated", "--format", "json"))
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("dotnet list package --outdated: %w: %s", err, stderr.String())
	}

	return parseDotnetOutput(output, subdir)
}

// parseDotnetOutput parses the JSON output from dotnet list package --outdated --format json.
func parseDotnetOutput(data []byte, subdir string) ([]DependencyUpdate, error) {
	var result dotnetOutdatedOutput
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parsing dotnet output: %w", err)
	}

	seen := make(map[string]bool)
	var updates []DependencyUpdate

	for _, proj := range result.Projects {
		for _, fw := range proj.Frameworks {
			for _, pkg := range fw.TopLevelPkgs {
				if pkg.LatestVersion == "" || pkg.ResolvedVersion == pkg.LatestVersion {
					continue
				}

				// Deduplicate across target frameworks within the same project
				key := pkg.ID + "@" + pkg.ResolvedVersion + "->" + pkg.LatestVersion
				if seen[key] {
					continue
				}
				seen[key] = true

				updates = append(updates, DependencyUpdate{
					Ecosystem: EcosystemDotnet,
					Package:   pkg.ID,
					Current:   pkg.ResolvedVersion,
					Latest:    pkg.LatestVersion,
					Kind:      classifyUpdate(pkg.ResolvedVersion, pkg.LatestVersion),
					Subdir:    subdir,
				})
			}
		}
	}

	return updates, nil
}
