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

// scanDotnet runs 'dotnet list package --outdated --format json' for the
// project files src reports. Returns nil if no .NET project is found.
func (s *Scanner) scanDotnet(ctx context.Context, anvil, path string, src manifestSource) *CheckResult {
	paths, err := src.Paths(ctx)
	if err != nil {
		return &CheckResult{
			Anvil:   anvil,
			Path:    path,
			Checked: time.Now(),
			Error:   fmt.Errorf("listing files in %s: %w", src.Describe(), err),
		}
	}

	projectFiles := dotnetProjectPaths(paths)
	if len(projectFiles) == 0 {
		return nil
	}

	result := &CheckResult{
		Anvil:     anvil,
		Path:      path,
		Ecosystem: "NuGet",
		Checked:   time.Now(),
	}

	// Track seen packages across all project files to avoid duplicates
	// when the same package appears in multiple .sln/.csproj files.
	seen := map[string]bool{}
	var collected []ModuleUpdate
	pins := newMSBuildPins(src)

	for _, rel := range projectFiles {
		projFile := filepath.Join(path, filepath.FromSlash(rel))
		// A project tracked at the ref but absent from the checkout has
		// nothing for dotnet to read. Skip it rather than fail the whole
		// ecosystem: the rest of the solution is still scannable.
		if _, statErr := os.Stat(projFile); statErr != nil {
			continue
		}

		updates, err := runDotnetOutdated(ctx, s.timeout, filepath.Dir(projFile), projFile)
		if err != nil {
			result.Error = fmt.Errorf("dotnet list package in %s: %w", projFile, err)
			return result
		}

		// A PackageReference pins a resolved version, so a stale one reported
		// off the checkout is replaced by what the source actually commits —
		// reconciled against THIS project's own pins, before the cross-project
		// dedupe below folds anything together.
		updates = reconcileWithCommitted(updates, pins.forProject(ctx, paths, pathDir(rel)), true)

		for _, u := range updates {
			if seen[u.Path] {
				continue
			}
			seen[u.Path] = true
			collected = append(collected, u)
		}
	}

	sortUpdates(collected)

	for _, u := range collected {
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

// msbuildPins folds a source's MSBuild manifests into the package → version
// map that applies to one project, reading each file at most once across every
// project of a scan.
type msbuildPins struct {
	src   manifestSource
	cache map[string]map[string]string
}

func newMSBuildPins(src manifestSource) *msbuildPins {
	return &msbuildPins{src: src, cache: map[string]map[string]string{}}
}

// forProject folds the manifests in paths that apply to a project rooted at
// projectDir. A file that cannot be read is skipped: an incomplete map only
// ever means fewer entries are reconciled, never a wrong one.
func (p *msbuildPins) forProject(ctx context.Context, paths []string, projectDir string) map[string]string {
	refs := map[string]string{}
	for _, path := range paths {
		if !isMSBuildManifest(path) || !msbuildAppliesTo(path, projectDir) {
			continue
		}
		for name, version := range p.read(ctx, path) {
			refs[name] = version
		}
	}
	return refs
}

func (p *msbuildPins) read(ctx context.Context, path string) map[string]string {
	if parsed, ok := p.cache[path]; ok {
		return parsed
	}
	var parsed map[string]string
	if data, err := p.src.Read(ctx, path); err == nil {
		parsed = parsePackageRefs(data)
	}
	p.cache[path] = parsed
	return parsed
}

// msbuildAppliesTo reports whether the pins in manifest path apply to a project
// rooted at projectDir.
//
// The scope matters because reconciliation DROPS an update whose committed pin
// is already the latest version. Folded repo-wide, one project in a monorepo
// that has been upgraded silences the identical update in every other project
// that has not — a real update dropped with no trace of it anywhere. So a
// sibling's manifest is out of scope, and only two things are in it:
//
//   - a manifest inside the project's own tree (its .csproj, and for a .sln the
//     project files under it, which is exactly the set `dotnet list package`
//     reports on for that solution);
//   - a directory-level .props/.targets in an ANCESTOR directory, because
//     MSBuild imports Directory.Build.props and Directory.Packages.props by
//     walking up from the project — central package management pins live
//     nowhere else.
func msbuildAppliesTo(path, projectDir string) bool {
	dir := pathDir(path)
	if dirWithin(dir, projectDir) {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".props", ".targets":
		return dirWithin(projectDir, dir)
	}
	return false
}

// isMSBuildManifest reports whether a path holds NuGet package pins — a project
// file, or the central Directory.Packages.props / Directory.Build.props.
func isMSBuildManifest(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	case ".props", ".targets":
		return true
	}
	return false
}

// dotnetProjectPaths selects the project files to run `dotnet list package`
// against from a list of repo-relative paths: *.sln first, then any *.csproj
// not already covered by one (the sln covers its own directory tree).
func dotnetProjectPaths(paths []string) []string {
	slnDirs := map[string]bool{}
	var slnFiles []string
	var csprojFiles []string

	for _, p := range paths {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".sln":
			slnFiles = append(slnFiles, p)
			slnDirs[pathDir(p)] = true
		case ".csproj":
			csprojFiles = append(csprojFiles, p)
		}
	}

	// Prefer sln files; only include csproj files not covered by a sln.
	result := append([]string{}, slnFiles...)
	for _, csproj := range csprojFiles {
		dir := pathDir(csproj)
		covered := false
		for slnDir := range slnDirs {
			if dirWithin(dir, slnDir) {
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

// dirWithin reports whether dir is root or sits under it. Both are
// repo-relative and forward-slashed, so the repository root is "" — which every
// directory is under, and which no prefix test spells correctly.
func dirWithin(dir, root string) bool {
	if root == "" || dir == root {
		return true
	}
	return strings.HasPrefix(dir, root+"/")
}

// dotnetOutdatedResponse represents the JSON output of
// 'dotnet list package --outdated --format json'.
type dotnetOutdatedResponse struct {
	Projects []dotnetProject `json:"projects"`
}

type dotnetProject struct {
	Path       string            `json:"path"`
	Frameworks []dotnetFramework `json:"frameworks"`
}

type dotnetFramework struct {
	Framework string          `json:"framework"`
	TopLevel  []dotnetPackage `json:"topLevelPackages"`
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
