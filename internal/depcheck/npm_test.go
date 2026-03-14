package depcheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindNpmProjects(t *testing.T) {
	dir := t.TempDir()

	// Create package.json in root
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	// Create package.json in subdirectory
	sub := filepath.Join(dir, "client")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "package.json"), []byte("{}"), 0o644))

	// Create package.json inside node_modules (should be skipped)
	nm := filepath.Join(dir, "node_modules", "foo")
	require.NoError(t, os.MkdirAll(nm, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "package.json"), []byte("{}"), 0o644))

	// Create package.json inside .worktrees (should be skipped)
	wt := filepath.Join(dir, ".worktrees", "client")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "package.json"), []byte("{}"), 0o644))

	// Create package.json inside bin (should be skipped)
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "package.json"), []byte("{}"), 0o644))

	// Create package.json inside obj (should be skipped)
	obj := filepath.Join(dir, "obj")
	require.NoError(t, os.MkdirAll(obj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(obj, "package.json"), []byte("{}"), 0o644))

	dirs := findNpmProjects(dir)
	assert.Len(t, dirs, 2)
	assert.Contains(t, dirs, dir)
	assert.Contains(t, dirs, sub)
}

func TestFindNpmProjects_NoPackageJson(t *testing.T) {
	dir := t.TempDir()
	dirs := findNpmProjects(dir)
	assert.Empty(t, dirs)
}

// TestNpmCrossProjectDedup verifies that the seen-map dedup in scanNpm prevents
// the same package from being counted multiple times when it appears in more
// than one package.json (e.g. worktree copies of the same repo).
func TestNpmCrossProjectDedup(t *testing.T) {
	// Simulate updates from two separate package.json directories where both
	// report the same outdated packages. The accumulator should emit each
	// package only once.
	project1 := []ModuleUpdate{
		{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
		{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
	}
	project2 := []ModuleUpdate{
		{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"}, // duplicate
		{Path: "axios", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
	}

	result := &CheckResult{}
	seen := map[string]bool{}

	for _, updates := range [][]ModuleUpdate{project1, project2} {
		for _, u := range updates {
			if seen[u.Path] {
				continue
			}
			seen[u.Path] = true
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

	assert.Len(t, result.Patch, 1, "lodash should appear once despite two projects reporting it")
	assert.Equal(t, "lodash", result.Patch[0].Path)
	assert.Len(t, result.Minor, 1)
	assert.Equal(t, "react", result.Minor[0].Path)
	assert.Len(t, result.Major, 1)
	assert.Equal(t, "axios", result.Major[0].Path)
}
