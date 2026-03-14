package depcheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindDotnetProjects_SlnPreferred(t *testing.T) {
	dir := t.TempDir()

	// Create solution file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "MyApp.sln"), []byte(""), 0o644))

	// Create csproj in subdirectory (covered by sln)
	sub := filepath.Join(dir, "src", "MyApp")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "MyApp.csproj"), []byte(""), 0o644))

	files := findDotnetProjects(dir)
	// Should only return the sln, not the csproj under it
	assert.Len(t, files, 1)
	assert.Equal(t, filepath.Join(dir, "MyApp.sln"), files[0])
}

func TestFindDotnetProjects_CsprojOnly(t *testing.T) {
	dir := t.TempDir()

	// Create csproj without a sln
	sub := filepath.Join(dir, "tools", "MyTool")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "MyTool.csproj"), []byte(""), 0o644))

	files := findDotnetProjects(dir)
	assert.Len(t, files, 1)
	assert.Equal(t, filepath.Join(sub, "MyTool.csproj"), files[0])
}

func TestFindDotnetProjects_SkipsBinObj(t *testing.T) {
	dir := t.TempDir()

	// Create csproj in bin (should be skipped)
	bin := filepath.Join(dir, "bin", "Debug")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "App.csproj"), []byte(""), 0o644))

	files := findDotnetProjects(dir)
	assert.Empty(t, files)
}

func TestFindDotnetProjects_Empty(t *testing.T) {
	dir := t.TempDir()
	files := findDotnetProjects(dir)
	assert.Empty(t, files)
}

func TestFindDotnetProjects_SkipsWorktrees(t *testing.T) {
	dir := t.TempDir()

	// Create a .sln inside .worktrees (should be skipped)
	wt := filepath.Join(dir, ".worktrees", "feature-branch")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "App.sln"), []byte(""), 0o644))

	// Create a .csproj inside .worktrees (should be skipped)
	wtSub := filepath.Join(wt, "src", "App")
	require.NoError(t, os.MkdirAll(wtSub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wtSub, "App.csproj"), []byte(""), 0o644))

	files := findDotnetProjects(dir)
	assert.Empty(t, files, ".worktrees contents should not be returned")
}
