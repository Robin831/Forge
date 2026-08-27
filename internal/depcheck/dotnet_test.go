package depcheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDotnetProjectDiscovery_SlnPreferred(t *testing.T) {
	dir := t.TempDir()

	// Create solution file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "MyApp.sln"), []byte(""), 0o644))

	// Create csproj in subdirectory (covered by sln)
	sub := filepath.Join(dir, "src", "MyApp")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "MyApp.csproj"), []byte(""), 0o644))

	files := dotnetProjectsIn(t, dir)
	// Should only return the sln, not the csproj under it
	assert.Len(t, files, 1)
	assert.Equal(t, filepath.Join(dir, "MyApp.sln"), files[0])
}

func TestDotnetProjectDiscovery_CsprojOnly(t *testing.T) {
	dir := t.TempDir()

	// Create csproj without a sln
	sub := filepath.Join(dir, "tools", "MyTool")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "MyTool.csproj"), []byte(""), 0o644))

	files := dotnetProjectsIn(t, dir)
	assert.Len(t, files, 1)
	assert.Equal(t, filepath.Join(sub, "MyTool.csproj"), files[0])
}

func TestDotnetProjectDiscovery_SkipsBinObj(t *testing.T) {
	dir := t.TempDir()

	// Create csproj in bin (should be skipped)
	bin := filepath.Join(dir, "bin", "Debug")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "App.csproj"), []byte(""), 0o644))

	files := dotnetProjectsIn(t, dir)
	assert.Empty(t, files)
}

func TestDotnetProjectDiscovery_Empty(t *testing.T) {
	dir := t.TempDir()
	files := dotnetProjectsIn(t, dir)
	assert.Empty(t, files)
}

func TestDotnetProjectDiscovery_SkipsWorktrees(t *testing.T) {
	dir := t.TempDir()

	// Create a .sln inside .worktrees (should be skipped)
	wt := filepath.Join(dir, ".worktrees", "feature-branch")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "App.sln"), []byte(""), 0o644))

	// Create a .csproj inside .worktrees (should be skipped)
	wtSub := filepath.Join(wt, "src", "App")
	require.NoError(t, os.MkdirAll(wtSub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wtSub, "App.csproj"), []byte(""), 0o644))

	files := dotnetProjectsIn(t, dir)
	assert.Empty(t, files, ".worktrees contents should not be returned")
}

// TestMSBuildAppliesTo covers the scope of the NuGet reconcile: a project's own
// directory and the directory-level props above it, never a sibling's pins.
func TestMSBuildAppliesTo(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		scope    []scopeRoot
		want     bool
	}{
		{"the project's own file", "src/App/App.csproj", []scopeRoot{projectScope("src/App")}, true},
		{"props beside the project", "src/App/Directory.Build.props", []scopeRoot{projectScope("src/App")}, true},
		// MSBuild imports by walking UP from a project, so a manifest below one
		// is either never imported or belongs to a different project — whose
		// already-upgraded pin is exactly what must not silence this one.
		{"a project nested under the project", "src/App/sub/Nested.csproj", []scopeRoot{projectScope("src/App")}, false},
		{"a sibling project", "src/Other/Other.csproj", []scopeRoot{projectScope("src/App")}, false},
		{"an ancestor Directory.Packages.props", "Directory.Packages.props", []scopeRoot{projectScope("src/App")}, true},
		{"an intermediate Directory.Build.props", "src/Directory.Build.props", []scopeRoot{projectScope("src/App")}, true},
		{"a sibling's props", "src/Other/Other.props", []scopeRoot{projectScope("src/App")}, false},
		// A project at the repository root has scope "", which dirWithin
		// reports every directory as within: matched as a tree it would take
		// the whole repository, which is the repo-wide fold under another name.
		{"a root-level project's own file", "MyApp.csproj", []scopeRoot{projectScope("")}, true},
		{"root-level props", "Directory.Packages.props", []scopeRoot{projectScope("")}, true},
		{"a project elsewhere in the repository", "tests/Tests.csproj", []scopeRoot{projectScope("")}, false},
		{"props under another directory", "tests/Tests.props", []scopeRoot{projectScope("")}, false},
		// A solution's scope is the several projects it references, and a
		// project outside all of them stays outside — which is what a
		// root-level solution scoped to its own tree could not express.
		{"a second project of the same solution", "src/Lib/Lib.csproj", []scopeRoot{projectScope("src/App"), projectScope("src/Lib")}, true},
		{"a project the solution does not reference", "tools/Tool/Tool.csproj", []scopeRoot{projectScope("src/App"), projectScope("src/Lib")}, false},
		{"props above one of the solution's projects", "src/Directory.Packages.props", []scopeRoot{projectScope("src/App"), projectScope("src/Lib")}, true},
		// Only the fallback for a solution naming no parseable project mints a
		// tree root, and there the subtree is the point.
		{"a project under a tree root", "src/App/App.csproj", []scopeRoot{treeScope("src")}, true},
		{"a project outside a tree root", "tools/Tool/Tool.csproj", []scopeRoot{treeScope("src")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, msbuildAppliesTo(tc.manifest, tc.scope))
		})
	}
}

// TestIsMSBuildManifestReadsTheExtensionSets is the invariant the doc on
// dotnetProjectExts asserts: the project types this ecosystem SCANS and the
// ones whose pins it READS are one set, so neither can grow without the other.
// It was previously only claimed — isMSBuildManifest restated the list — which
// is how .fsproj came to have its pins read while the project itself was never
// scanned.
func TestIsMSBuildManifestReadsTheExtensionSets(t *testing.T) {
	for ext := range dotnetProjectExts {
		assert.True(t, isMSBuildManifest("src/App/App"+ext), ext)
		assert.True(t, isMSBuildManifest("src/App/App"+strings.ToUpper(ext)), "extensions are matched case-insensitively")
	}
	for ext := range msbuildDirectoryExts {
		assert.True(t, isMSBuildManifest("Directory.Build"+ext), ext)
		assert.False(t, dotnetProjectExts[ext], "a directory-level import is not a project to scan")
	}
	assert.False(t, isMSBuildManifest("src/App/App.sln"))
	assert.False(t, isMSBuildManifest("README.md"))
}

// TestMSBuildPins_ExcludesASiblingsPins is the failure the scope exists to
// prevent, at the level the decision is made: folded repo-wide, the sibling's
// already-upgraded Serilog pin reads as "upstream has done this" and drops the
// real update this project still needs.
func TestMSBuildPins_ExcludesASiblingsPins(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Directory.Packages.props": `<Project><ItemGroup><PackageVersion Include="Shared" Version="1.0.0" /></ItemGroup></Project>`,
		"src/App/App.csproj":       `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.0" /></ItemGroup></Project>`,
		"src/Other/Other.csproj":   `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.1" /></ItemGroup></Project>`,
	})

	src := worktreeSource{root: dir}
	paths, err := src.Paths(context.Background())
	require.NoError(t, err)

	pins := newMSBuildPins(src)
	assert.Equal(t, map[string]string{"Serilog": "3.1.0", "Shared": "1.0.0"},
		pins.forScope(context.Background(), paths, []scopeRoot{projectScope("src/App")}))
	assert.Equal(t, map[string]string{"Serilog": "3.1.1", "Shared": "1.0.0"},
		pins.forScope(context.Background(), paths, []scopeRoot{projectScope("src/Other")}))
}

// TestScanDotnet_MonorepoSiblingDoesNotSilenceARealUpdate is the npm scan-level
// case for .NET, over the wiring that connects scanDotnet to the scope helpers:
// "App" still pins Serilog at 3.1.0 and is genuinely outdated while the sibling
// "Other" has already been bumped. Passing the wrong scope on that one line —
// repo-wide, a sibling's directory, or reconciling after the cross-project
// dedupe — drops App's real update, and every helper-level test still passes.
func TestScanDotnet_MonorepoSiblingDoesNotSilenceARealUpdate(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"src/App/App.csproj":     `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.0" /></ItemGroup></Project>`,
		"src/Other/Other.csproj": `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.1" /></ItemGroup></Project>`,
	})

	// Only App's checkout is behind; Other is already at the latest, so dotnet
	// reports nothing for it.
	stubOutdated(t, map[string][]ModuleUpdate{
		filepath.Join(dir, "src", "App", "App.csproj"): {
			{Path: "Serilog", Current: "3.1.0", Latest: "3.1.1", Kind: "patch"},
		},
	})

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanDotnet(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
	require.NotNil(t, result)
	require.NoError(t, result.Error)

	require.Len(t, result.Patch, 1, "App's own project file still pins the old version, so its update is real")
	assert.Equal(t, "Serilog", result.Patch[0].Path)
}

// TestScanDotnet_RootLevelSolutionIsNotSilencedByAProjectOutsideIt is the same
// failure on the standard .NET layout, which is the one that hides it: the
// solution and its project sit at the repository ROOT, so the scope root is ""
// — the directory dirWithin reports every other directory as being within. A
// test project the solution never references has already been bumped to 4.2.0
// upstream; matched as a tree, its pin lands in the solution's map and drops
// MyApp's real, un-applied update with no bead and no log line.
func TestScanDotnet_RootLevelSolutionIsNotSilencedByAProjectOutsideIt(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"MyApp.sln":          slnReferencing("MyApp.csproj"),
		"MyApp.csproj":       `<Project><ItemGroup><PackageReference Include="Serilog" Version="4.1.0" /></ItemGroup></Project>`,
		"tests/Tests.csproj": `<Project><ItemGroup><PackageReference Include="Serilog" Version="4.2.0" /></ItemGroup></Project>`,
	})

	stubOutdated(t, map[string][]ModuleUpdate{
		filepath.Join(dir, "MyApp.sln"): {
			{Path: "Serilog", Current: "4.1.0", Latest: "4.2.0", Kind: "minor"},
		},
	})

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanDotnet(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
	require.NotNil(t, result)
	require.NoError(t, result.Error)

	require.Len(t, result.Minor, 1, "the solution's own project still pins 4.1.0, so its update is real")
	assert.Equal(t, "Serilog", result.Minor[0].Path)
}

// TestScanDotnet_DropsWhatTheProjectsOwnManifestAlreadyPins is the other half of
// the same scope, and the reason a bead is not filed twice for merged work.
func TestScanDotnet_DropsWhatTheProjectsOwnManifestAlreadyPins(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"src/App/App.csproj": `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.1" /></ItemGroup></Project>`,
	})

	stubOutdated(t, map[string][]ModuleUpdate{
		filepath.Join(dir, "src", "App", "App.csproj"): {
			{Path: "Serilog", Current: "3.1.0", Latest: "3.1.1", Kind: "patch"},
		},
	})

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanDotnet(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
	require.NotNil(t, result)
	assert.Empty(t, result.Patch, "the checkout is behind what the project's own file commits")
}

// TestScanDotnet_SkipsAProjectAbsentFromTheCheckout pins the fail-open guard:
// discovery comes from the tracking ref while dotnet runs in the checkout, so a
// project the ref tracks may not be on disk. The rest of the scan still runs.
func TestScanDotnet_SkipsAProjectAbsentFromTheCheckout(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"src/App/App.csproj": `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.0" /></ItemGroup></Project>`,
	})

	appProj := filepath.Join(dir, "src", "App", "App.csproj")
	stubOutdated(t, map[string][]ModuleUpdate{
		appProj: {{Path: "Serilog", Current: "3.1.0", Latest: "3.1.1", Kind: "patch"}},
	})

	// The ref tracks a second project this checkout never materialized.
	src := stubSource{files: map[string]string{
		"src/App/App.csproj":     `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.0" /></ItemGroup></Project>`,
		"src/Ghost/Ghost.csproj": `<Project />`,
	}}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanDotnet(context.Background(), "test-anvil", dir, src)
	require.NotNil(t, result)
	require.NoError(t, result.Error, "one absent project must not fail the whole ecosystem")
	require.Len(t, result.Patch, 1)
	assert.Equal(t, "Serilog", result.Patch[0].Path)
}

// stubOutdated replaces the `dotnet list package` seam with a fixed answer per
// project file, and asserts nothing else is invoked.
func stubOutdated(t *testing.T, byProject map[string][]ModuleUpdate) {
	t.Helper()
	orig := runDotnetOutdatedFn
	t.Cleanup(func() { runDotnetOutdatedFn = orig })
	runDotnetOutdatedFn = func(_ context.Context, _ time.Duration, _, projFile string) ([]ModuleUpdate, error) {
		return byProject[projFile], nil
	}
}
