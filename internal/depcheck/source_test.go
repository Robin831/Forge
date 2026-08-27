package depcheck

import (
	"context"
	"os"
	"path/filepath"
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

func TestDotnetProjectPaths_RootSlnCoversNestedCsproj(t *testing.T) {
	paths := []string{"MyApp.sln", "src/MyApp/MyApp.csproj", "README.md"}
	assert.Equal(t, []string{"MyApp.sln"}, dotnetProjectPaths(paths))
}

func TestDotnetProjectPaths_UncoveredCsprojIsIncluded(t *testing.T) {
	paths := []string{"src/App/App.sln", "src/App/App.csproj", "tools/Tool/Tool.csproj"}
	assert.Equal(t, []string{"src/App/App.sln", "tools/Tool/Tool.csproj"}, dotnetProjectPaths(paths))
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
		dirs = append(dirs, localDir(root, rel))
	}
	return dirs
}

func dotnetProjectsIn(t *testing.T, root string) []string {
	t.Helper()
	paths, err := worktreeSource{root: root}.Paths(context.Background())
	require.NoError(t, err)
	rels := dotnetProjectPaths(paths)
	files := make([]string, 0, len(rels))
	for _, rel := range rels {
		files = append(files, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return files
}
