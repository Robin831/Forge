package depcheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
// tree and the directory-level props above it, never a sibling's pins.
func TestMSBuildAppliesTo(t *testing.T) {
	cases := []struct {
		name       string
		manifest   string
		projectDir string
		want       bool
	}{
		{"the project's own file", "src/App/App.csproj", "src/App", true},
		{"a file nested under the project", "src/App/sub/Nested.csproj", "src/App", true},
		{"a sibling project", "src/Other/Other.csproj", "src/App", false},
		{"an ancestor Directory.Packages.props", "Directory.Packages.props", "src/App", true},
		{"an intermediate Directory.Build.props", "src/Directory.Build.props", "src/App", true},
		{"a sibling's props", "src/Other/Other.props", "src/App", false},
		{"anything at all, for a root-level solution", "src/Other/Other.csproj", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, msbuildAppliesTo(tc.manifest, tc.projectDir))
		})
	}
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
		pins.forProject(context.Background(), paths, "src/App"))
	assert.Equal(t, map[string]string{"Serilog": "3.1.1", "Shared": "1.0.0"},
		pins.forProject(context.Background(), paths, "src/Other"))
}
