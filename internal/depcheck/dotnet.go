package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// scanDotnet runs 'dotnet list package --outdated --format json' in directories
// containing *.csproj or *.sln files. Skips bin/obj directories.
// Returns nil if no .NET project is found.
func (s *Scanner) scanDotnet(ctx context.Context, anvil, path string) *CheckResult {
	projectFiles := findDotnetProjects(path)
	if len(projectFiles) == 0 {
		return nil
	}

	result := &CheckResult{
		Anvil:     anvil,
		Path:      path,
		Ecosystem: "NuGet",
		Checked:   time.Now(),
	}

	for _, projFile := range projectFiles {
		updates, err := runDotnetOutdated(ctx, s.timeout, filepath.Dir(projFile), projFile)
		if err != nil {
			result.Error = fmt.Errorf("dotnet list package in %s: %w", projFile, err)
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

// findDotnetProjects walks the anvil directory for *.sln files first, then
// *.csproj files. If a .sln is found, its directory's csproj files are skipped
// (the sln covers them). Skips bin, obj, and .workers directories.
func findDotnetProjects(root string) []string {
	slnDirs := map[string]bool{}
	var slnFiles []string
	var csprojFiles []string

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "bin" || name == "obj" || name == ".workers" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext == ".sln" {
			slnFiles = append(slnFiles, path)
			slnDirs[filepath.Dir(path)] = true
		} else if ext == ".csproj" {
			csprojFiles = append(csprojFiles, path)
		}
		return nil
	})

	// Prefer sln files; only include csproj files not covered by a sln.
	var result []string
	result = append(result, slnFiles...)
	for _, csproj := range csprojFiles {
		dir := filepath.Dir(csproj)
		covered := false
		for slnDir := range slnDirs {
			if dir == slnDir || strings.HasPrefix(dir, slnDir+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, csproj)
		}
	}

	return result
}

// dotnetOutdatedResponse represents the JSON output of
// 'dotnet list package --outdated --format json'.
type dotnetOutdatedResponse struct {
	Projects []dotnetProject `json:"projects"`
}

type dotnetProject struct {
	Path       string              `json:"path"`
	Frameworks []dotnetFramework   `json:"frameworks"`
}

type dotnetFramework struct {
	Framework    string           `json:"framework"`
	TopLevel     []dotnetPackage  `json:"topLevelPackages"`
}

type dotnetPackage struct {
	ID              string `json:"id"`
	ResolvedVersion string `json:"resolvedVersion"`
	LatestVersion   string `json:"latestVersion"`
}

// runDotnetOutdated runs 'dotnet list <project> package --outdated --format json'
// and parses the output.
func runDotnetOutdated(ctx context.Context, timeout time.Duration, dir, projFile string) ([]ModuleUpdate, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"dotnet", "list", projFile, "package", "--outdated", "--format", "json"))
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("dotnet list package: %w\nstderr: %s", err, stderr.String())
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil, nil
	}

	var resp dotnetOutdatedResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing dotnet list package output: %w", err)
	}

	// Deduplicate across frameworks — same package may appear in multiple TFMs.
	seen := map[string]bool{}
	var updates []ModuleUpdate

	for _, proj := range resp.Projects {
		for _, fw := range proj.Frameworks {
			for _, pkg := range fw.TopLevel {
				if pkg.ResolvedVersion == "" || pkg.LatestVersion == "" {
					continue
				}
				if pkg.ResolvedVersion == pkg.LatestVersion {
					continue
				}
				if seen[pkg.ID] {
					continue
				}
				seen[pkg.ID] = true

				kind := classifyUpdate(pkg.ResolvedVersion, pkg.LatestVersion)
				updates = append(updates, ModuleUpdate{
					Path:    pkg.ID,
					Current: pkg.ResolvedVersion,
					Latest:  pkg.LatestVersion,
					Kind:    kind,
				})
			}
		}
	}

	sortUpdates(updates)
	return updates, nil
}
