package depcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// runDotnetOutdatedFn is the function used to invoke `dotnet list package`. It
// is a package-level variable so tests can exercise scanDotnet's own wiring —
// the per-project reconcile scope above all — without dotnet installed. Without
// the seam nothing executed scanDotnet's body at all, and passing the wrong
// scope there (repo-wide, or after the cross-project dedupe) silently drops a
// real NuGet update while every test in the package still passes.
var runDotnetOutdatedFn = runDotnetOutdated

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

	targets := dotnetScanTargets(ctx, src, paths)
	if len(targets) == 0 {
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
	scanned := 0

	for _, target := range targets {
		projFile := filepath.Join(path, filepath.FromSlash(target.rel))
		// A project tracked at the ref but absent from the checkout has
		// nothing for dotnet to read. Skip it rather than fail the whole
		// ecosystem: the rest of the solution is still scannable. Say so,
		// though — an ecosystem reporting "all dependencies up to date" because
		// none of its projects were scannable is the one degradation depcheck
		// leaves no trace of anywhere.
		if _, statErr := os.Stat(projFile); statErr != nil {
			log.Printf("[depcheck] %s: %s is tracked at %s but absent from the checkout — skipping it",
				anvil, target.rel, src.Describe())
			continue
		}
		scanned++

		updates, err := runDotnetOutdatedFn(ctx, s.timeout, filepath.Dir(projFile), projFile)
		if err != nil {
			result.Error = fmt.Errorf("dotnet list package in %s: %w", projFile, err)
			return result
		}

		// A PackageReference pins a resolved version, so a stale one reported
		// off the checkout is replaced by what the source actually commits —
		// reconciled against THIS project's own pins, before the cross-project
		// dedupe below folds anything together.
		updates = reconcileWithCommitted(updates, pins.forScope(ctx, paths, target.scope), true)

		for _, u := range updates {
			if seen[u.Path] {
				continue
			}
			seen[u.Path] = true
			collected = append(collected, u)
		}
	}

	if scanned == 0 {
		log.Printf("[depcheck] %s: none of the %d NuGet project(s) tracked at %s are present in the checkout — nothing was scanned",
			anvil, len(targets), src.Describe())
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

// forScope folds the manifests in paths that apply to a scan target — the
// directories dotnetScanTargets resolved for it. A file that cannot be read is
// skipped: an incomplete map only ever means fewer entries are reconciled,
// never a wrong one.
func (p *msbuildPins) forScope(ctx context.Context, paths []string, scope []string) map[string]string {
	refs := map[string]string{}
	for _, manifest := range paths {
		if !isMSBuildManifest(manifest) || !msbuildAppliesTo(manifest, scope) {
			continue
		}
		for name, version := range p.read(ctx, manifest) {
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

// msbuildAppliesTo reports whether the pins in manifest apply to a scan target
// whose scope is the given directories.
//
// The scope matters because reconciliation DROPS an update whose committed pin
// is already the latest version. Folded repo-wide, one project in a monorepo
// that has been upgraded silences the identical update in every other project
// that has not — a real update dropped with no trace of it anywhere. So a
// sibling's manifest is out of scope, and only two things are in it:
//
//   - a manifest inside one of the scope's own trees (the project's .csproj,
//     and for a .sln the trees of the projects that solution references);
//   - a .props/.targets in an ANCESTOR directory of one of them, because
//     MSBuild imports Directory.Build.props and Directory.Packages.props by
//     walking up from the project — central package management pins live
//     nowhere else. The ancestor rule is by extension, not by file name: an
//     unrelated `version.props` above a project is imported by whatever imports
//     it, and depcheck cannot tell which those are, so it is read.
func msbuildAppliesTo(manifest string, scope []string) bool {
	dir := pathDir(manifest)
	directoryLevel := false
	switch strings.ToLower(filepath.Ext(manifest)) {
	case ".props", ".targets":
		directoryLevel = true
	}
	for _, root := range scope {
		if dirWithin(dir, root) {
			return true
		}
		if directoryLevel && dirWithin(root, dir) {
			return true
		}
	}
	return false
}

// isMSBuildManifest reports whether a path can hold NuGet package pins: any
// project file, and any .props/.targets by extension — the name is deliberately
// not inspected, since a repository is free to keep its central
// <PackageVersion> entries in a file called anything at all and import it from
// Directory.Build.props.
func isMSBuildManifest(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	case ".props", ".targets":
		return true
	}
	return false
}

// dotnetProjectExts are the project files `dotnet list package` runs against.
// The set is shared with isMSBuildManifest so the two halves of this ecosystem
// cannot disagree about which project types exist — reading an F# project's
// pins while never scanning the project itself is the shape that disagreement
// took.
var dotnetProjectExts = map[string]bool{".csproj": true, ".fsproj": true, ".vbproj": true}

// dotnetTarget is one project file to run `dotnet list package` against,
// together with the directories whose MSBuild manifests pin its packages.
type dotnetTarget struct {
	rel   string
	scope []string
}

// dotnetScanTargets selects the project files to scan and resolves each one's
// manifest scope: *.sln first, then any project file no solution references.
//
// A solution's scope is the set of directories of the projects it REFERENCES,
// read out of the .sln itself, rather than its own directory tree. That
// distinction is the whole point on the standard .NET layout, where the solution
// sits at the repository ROOT: the root directory is "", which dirWithin reports
// every directory as being within, so a tree-scoped root solution folds every
// manifest in the repository into its pin map — repo-wide reconciliation again,
// under a different name. Since reconciliation drops an update whose pin is
// already the latest, one out-of-solution project upstream has bumped then
// silences the same real update inside the solution, with no trace anywhere.
//
// A solution naming no project this can parse falls back to its own tree, which
// is all that can be said about it, and is what it has always meant.
func dotnetScanTargets(ctx context.Context, src manifestSource, paths []string) []dotnetTarget {
	var slnFiles, projectFiles []string
	for _, p := range paths {
		ext := strings.ToLower(filepath.Ext(p))
		switch {
		case ext == ".sln":
			slnFiles = append(slnFiles, p)
		case dotnetProjectExts[ext]:
			projectFiles = append(projectFiles, p)
		}
	}

	var targets []dotnetTarget
	covered := map[string]bool{}

	for _, sln := range slnFiles {
		referenced := slnReferencedProjects(ctx, src, sln)
		if len(referenced) == 0 {
			slnDir := pathDir(sln)
			for _, proj := range projectFiles {
				if dirWithin(pathDir(proj), slnDir) {
					covered[proj] = true
				}
			}
			targets = append(targets, dotnetTarget{rel: sln, scope: []string{slnDir}})
			continue
		}
		scope := make([]string, 0, len(referenced))
		for _, proj := range referenced {
			covered[proj] = true
			scope = append(scope, pathDir(proj))
		}
		targets = append(targets, dotnetTarget{rel: sln, scope: scope})
	}

	for _, proj := range projectFiles {
		if covered[proj] {
			continue
		}
		targets = append(targets, dotnetTarget{rel: proj, scope: []string{pathDir(proj)}})
	}

	return targets
}

// slnProjectEntry matches a solution's project entries:
//
//	Project("{FAE04EC0-...}") = "MyApp", "src\MyApp\MyApp.csproj", "{...}"
//
// The captured field is a path relative to the solution — except for a solution
// FOLDER, where it repeats the folder's name, which is why only entries naming a
// known project extension are kept.
var slnProjectEntry = regexp.MustCompile(`(?i)^Project\("\{[^}]*\}"\)\s*=\s*"[^"]*"\s*,\s*"([^"]+)"`)

// slnReferencedProjects returns the repo-relative paths of the projects a
// solution references. A solution that cannot be read returns nothing, which the
// caller reads as "fall back to the tree".
func slnReferencedProjects(ctx context.Context, src manifestSource, sln string) []string {
	data, err := src.Read(ctx, sln)
	if err != nil {
		return nil
	}
	dir := pathDir(sln)
	var refs []string
	for _, line := range strings.Split(string(data), "\n") {
		match := slnProjectEntry.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		// A .sln always spells its paths with backslashes, on every host.
		rel := strings.ReplaceAll(strings.TrimSpace(match[1]), "\\", "/")
		if !dotnetProjectExts[strings.ToLower(filepath.Ext(rel))] {
			continue
		}
		refs = append(refs, joinRepoPath(dir, rel))
	}
	return refs
}

// joinRepoPath resolves a solution-relative path against the solution's own
// directory, returning the repo-relative, forward-slashed form the rest of this
// package passes around. A path escaping the repository root keeps its leading
// "..", which simply matches nothing.
func joinRepoPath(dir, rel string) string {
	if dir == "" {
		return path.Clean(rel)
	}
	return path.Clean(dir + "/" + rel)
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
