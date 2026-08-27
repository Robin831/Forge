package depcheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeSource_ReadMissingFileIsBlobNotFound(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644))

	src := worktreeSource{root: dir}
	data, err := src.Read(context.Background(), "go.mod")
	require.NoError(t, err)
	assert.Equal(t, "module x\n", string(data))

	// The same sentinel the ref source uses, so a scanner's absence check does
	// not have to know which source it was handed.
	_, err = src.Read(context.Background(), "package.json")
	assert.ErrorIs(t, err, ErrBlobNotFound)
}

func TestWalkWorktreePaths_SkipsUntrackedDirs(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"go.mod",
		"web/package.json",
		"web/node_modules/x/package.json",
		".workers/w1/go.mod",
		".worktrees/w2/go.mod",
		".previews/p1/package.json",
		"src/bin/Debug/App.csproj",
		"src/obj/App.csproj",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("{}"), 0o644))
	}

	assert.ElementsMatch(t, []string{"go.mod", "web/package.json"}, walkWorktreePaths(dir))
}

func TestPathDirAndDirWithin(t *testing.T) {
	assert.Equal(t, "", pathDir("go.mod"))
	assert.Equal(t, "src/App", pathDir("src/App/App.csproj"))

	// The repository root is "", which every directory sits under — the case a
	// plain prefix test spells as "/" and gets wrong.
	assert.True(t, dirWithin("src/App", ""))
	assert.True(t, dirWithin("", ""))
	assert.True(t, dirWithin("src/App", "src"))
	assert.False(t, dirWithin("srcOther", "src"))
	assert.False(t, dirWithin("tools", "src"))
}

func TestDotnetScanTargets_UnparseableSlnCoversItsTree(t *testing.T) {
	// A solution naming no project this can parse is all the .sln says, so it
	// keeps its historic meaning: it covers its own tree.
	src := stubSource{files: map[string]string{"MyApp.sln": "Microsoft Visual Studio Solution File\n"}}
	targets := dotnetScanTargets(context.Background(), "anvil", src,
		[]string{"MyApp.sln", "src/MyApp/MyApp.csproj", "README.md"})

	require.Len(t, targets, 1)
	assert.Equal(t, "MyApp.sln", targets[0].rel)
	assert.Equal(t, []scopeRoot{treeScope("")}, targets[0].scope)
}

// TestDotnetScanTargets_UnreadableSlnScansItsProjectsIndividually is the other
// half of that fallback, and the one it used to be confused with: a solution
// whose blob is missing (or whose read failed) says NOTHING about its projects,
// so claiming its tree on its behalf would — at the repository root, where the
// standard layout puts a .sln — fold every manifest in the repository into one
// pin map and let an out-of-solution project silence a real update inside it.
// The projects stand on their own instead, each with its own scope.
func TestDotnetScanTargets_UnreadableSlnScansItsProjectsIndividually(t *testing.T) {
	// The .sln is listed at the ref but its blob cannot be read.
	src := stubSource{files: map[string]string{
		"src/App/App.csproj": "<Project/>",
	}}
	targets := dotnetScanTargets(context.Background(), "anvil", src,
		[]string{"MyApp.sln", "src/App/App.csproj", "tools/Tool/Tool.csproj"})

	assert.Equal(t, []dotnetTarget{
		{rel: "src/App/App.csproj", scope: []scopeRoot{projectScope("src/App")}},
		{rel: "tools/Tool/Tool.csproj", scope: []scopeRoot{projectScope("tools/Tool")}},
	}, targets, "an unreadable solution is neither scanned nor allowed to cover anything")
}

func TestDotnetScanTargets_UncoveredProjectIsIncluded(t *testing.T) {
	src := stubSource{files: map[string]string{
		"src/App/App.sln": slnReferencing("App.csproj"),
	}}
	targets := dotnetScanTargets(context.Background(), "anvil", src,
		[]string{"src/App/App.sln", "src/App/App.csproj", "tools/Tool/Tool.csproj"})

	assert.Equal(t, []dotnetTarget{
		{rel: "src/App/App.sln", scope: []scopeRoot{projectScope("src/App")}},
		{rel: "tools/Tool/Tool.csproj", scope: []scopeRoot{projectScope("tools/Tool")}},
	}, targets)
}

// TestDotnetScanTargets_RootSlnScopesToItsOwnProjects is the failure a
// tree-scoped solution reintroduced: at the repository root a solution's tree is
// the whole repository, so every manifest in it — including a project the
// solution does not reference — was folded into its pin map, and an
// out-of-solution project upstream had bumped silenced the same real update
// inside the solution. Its scope is the projects it actually references, and the
// project it leaves out becomes a scan target of its own.
func TestDotnetScanTargets_RootSlnScopesToItsOwnProjects(t *testing.T) {
	src := stubSource{files: map[string]string{
		"MyApp.sln": slnReferencing("src\\App\\App.csproj", "src\\Lib\\Lib.fsproj"),
	}}
	targets := dotnetScanTargets(context.Background(), "anvil", src, []string{
		"MyApp.sln",
		"src/App/App.csproj",
		"src/Lib/Lib.fsproj",
		"tools/Tool/Tool.csproj",
	})

	assert.Equal(t, []dotnetTarget{
		{rel: "MyApp.sln", scope: []scopeRoot{projectScope("src/App"), projectScope("src/Lib")}},
		{rel: "tools/Tool/Tool.csproj", scope: []scopeRoot{projectScope("tools/Tool")}},
	}, targets, "the unreferenced project is neither in the solution's scope nor left unscanned")
}

// TestDotnetScanTargets_RootLevelProjectDoesNotScopeToTheRepository is the same
// failure by the other route: a solution and its project side by side at the
// repository root (what `dotnet new console` + `dotnet new sln` produces) put ""
// in the scope, and a root that is matched as a TREE takes every directory with
// it — including a test project the solution never references, whose already
// upgraded pin then drops the real update for the project that needs it.
func TestDotnetScanTargets_RootLevelProjectDoesNotScopeToTheRepository(t *testing.T) {
	src := stubSource{files: map[string]string{"MyApp.sln": slnReferencing("MyApp.csproj")}}
	targets := dotnetScanTargets(context.Background(), "anvil", src, []string{
		"MyApp.sln",
		"MyApp.csproj",
		"tests/Tests.csproj",
	})

	require.Len(t, targets, 2)
	assert.Equal(t, []scopeRoot{projectScope("")}, targets[0].scope)
	assert.False(t, msbuildAppliesTo("tests/Tests.csproj", targets[0].scope),
		"the out-of-solution project's pins must stay out of the root solution's scope")
	assert.True(t, msbuildAppliesTo("MyApp.csproj", targets[0].scope))
	assert.True(t, msbuildAppliesTo("Directory.Packages.props", targets[0].scope))
}

func TestSlnReferencedProjects_SkipsSolutionFolders(t *testing.T) {
	sln := `Microsoft Visual Studio Solution File, Format Version 12.00
Project("{2150E333-8FDC-42A3-9474-1DD2E32FD3B2}") = "Solution Items", "Solution Items", "{AAA}"
EndProject
Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "App", "src\App\App.csproj", "{BBB}"
EndProject
Project("{F2A71F9B-5D33-465A-A702-920D77279786}") = "Lib", "..\shared\Lib.fsproj", "{CCC}"
EndProject
`
	// Visual Studio and `dotnet new sln` write CRLF throughout, so that — not
	// the LF form every other fixture in this file uses — is the byte shape the
	// parser actually meets in production. The trailing \r is removed before the
	// line is matched, and nothing but this case says so: a later change to how
	// lines are split or anchored that stops tolerating it parses every real
	// solution to zero referenced projects, which is silently the documented
	// "names no project" fallback — at the repository root, the whole-repo pin
	// fold this scope exists to remove, with no error and no log line.
	for _, tc := range []struct {
		name string
		text string
	}{
		{"LF", sln},
		{"CRLF", strings.ReplaceAll(sln, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := stubSource{files: map[string]string{"nested/MyApp.sln": tc.text}}
			refs, err := slnReferencedProjects(context.Background(), src, "nested/MyApp.sln")
			require.NoError(t, err)
			assert.Equal(t, []string{"nested/src/App/App.csproj", "shared/Lib.fsproj"}, refs,
				"a solution folder names a label rather than a path, and a relative path resolves against the .sln")
		})
	}
}

// TestSlnReferencedProjects_ReadFailureIsNotAnEmptyList pins the distinction the
// tree fallback turns on: a solution that could not be read must not arrive at
// the caller looking like one that simply names no project.
func TestSlnReferencedProjects_ReadFailureIsNotAnEmptyList(t *testing.T) {
	refs, err := slnReferencedProjects(context.Background(), stubSource{}, "MyApp.sln")
	assert.Nil(t, refs)
	assert.ErrorIs(t, err, ErrBlobNotFound)
}

func TestNpmPackageFiles(t *testing.T) {
	paths := []string{"web/package.json", "package.json", "web/package.json.bak", "docs/README.md"}
	assert.Equal(t, []string{"package.json", "web/package.json"}, npmPackageFiles(paths))
}

// The two helpers below are the composition production performs itself — the
// paths worktreeSource reports, narrowed by the ecosystem's own selector — kept
// here so the discovery tests exercise the real path rather than a wrapper that
// exists only for them.

func npmProjectDirsIn(t *testing.T, root string) []string {
	t.Helper()
	paths, err := worktreeSource{root: root}.Paths(context.Background())
	require.NoError(t, err)
	var dirs []string
	for _, rel := range npmPackageFiles(paths) {
		dirs = append(dirs, filepath.Dir(filepath.Join(root, filepath.FromSlash(rel))))
	}
	return dirs
}

func dotnetProjectsIn(t *testing.T, root string) []string {
	t.Helper()
	src := worktreeSource{root: root}
	paths, err := src.Paths(context.Background())
	require.NoError(t, err)
	var files []string
	for _, target := range dotnetScanTargets(context.Background(), "test-anvil", src, paths) {
		files = append(files, filepath.Join(root, filepath.FromSlash(target.rel)))
	}
	return files
}

// stubSource serves a fixed set of repo-relative files, for the discovery tests
// that need a solution's CONTENTS without a checkout to put it in.
type stubSource struct{ files map[string]string }

func (s stubSource) Describe() string { return "stub" }

func (s stubSource) Paths(context.Context) ([]string, error) {
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (s stubSource) Read(_ context.Context, path string) ([]byte, error) {
	data, ok := s.files[path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, ErrBlobNotFound)
	}
	return []byte(data), nil
}

// slnReferencing builds the minimum solution text carrying the given
// solution-relative project paths (backslashed, as a .sln always spells them).
func slnReferencing(projects ...string) string {
	var b strings.Builder
	b.WriteString("Microsoft Visual Studio Solution File, Format Version 12.00\n")
	for i, proj := range projects {
		fmt.Fprintf(&b, "Project(\"{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}\") = \"P%d\", \"%s\", \"{%d}\"\nEndProject\n", i, proj, i)
	}
	return b.String()
}
