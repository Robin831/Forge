package depcheck

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Explorer's layout (2026-10-20): samples/HostileHost pins Stiler from a private
// feed the anvil cannot read, and `dotnet list package` on it failed the whole
// .NET scan with an empty stderr. The test hosts are not shipped, so the default
// exclude keeps them out of every ecosystem's scan.
func TestDefaultExcludeDropsSampleHostsFromTheDotnetScan(t *testing.T) {
	src := excludingSource{manifestSource: stubSource{files: map[string]string{
		"Fhi.Munin.Explorer.slnx":                                       "<Solution />",
		"samples/HostileHost/HostileHost.csproj":                        "<Project />",
		"samples/LegacyHost/LegacyHost.csproj":                          "<Project />",
		"samples/ModernHost/ModernHost.csproj":                          "<Project />",
		"src/Fhi.Munin.Explorer/Fhi.Munin.Explorer.csproj":              "<Project />",
		"test/Fhi.Munin.Explorer.Tests/Fhi.Munin.Explorer.Tests.csproj": "<Project />",
	}}, globs: DefaultExclude}
	paths, err := src.Paths(context.Background())
	require.NoError(t, err)
	var scanned []string
	for _, target := range dotnetScanTargets(context.Background(), "explorer", src, paths) {
		scanned = append(scanned, target.rel)
	}
	assert.ElementsMatch(t, []string{
		"src/Fhi.Munin.Explorer/Fhi.Munin.Explorer.csproj",
		"test/Fhi.Munin.Explorer.Tests/Fhi.Munin.Explorer.Tests.csproj",
	}, scanned)
}

func TestExcludedMatchesRepoRelativeGlobs(t *testing.T) {
	assert.True(t, excluded("samples/HostileHost/HostileHost.csproj", DefaultExclude))
	assert.True(t, excluded("samples/web/package.json", DefaultExclude), "every ecosystem, npm too")
	assert.False(t, excluded("src/samples/Real.csproj", DefaultExclude), "only the top-level samples folder")
	assert.True(t, excluded("tools/gen/package.json", []string{"./tools/**"}), "a leading ./ is tolerated")
	assert.False(t, excluded("src/App.csproj", []string{"[unclosed"}), "a malformed glob matches nothing")
}

func TestPerAnvilExcludesAddToTheDefault(t *testing.T) {
	s := &Scanner{}
	s.UpdateAnvilExcludes(map[string][]string{"explorer": {"tools/**"}})
	assert.Equal(t, []string{"samples/**", "tools/**"}, s.excludesFor("explorer"))
	assert.Equal(t, []string{"samples/**"}, s.excludesFor("munin"), "an anvil with none set keeps the default")
	// The scanner keeps its own copy: a later change to the caller's slice does not reach a running scan.
	in := map[string][]string{"x": {"a/**"}}
	s.UpdateAnvilExcludes(in)
	in["x"][0] = "b/**"
	assert.Equal(t, []string{"samples/**", "a/**"}, s.excludesFor("x"))
}

// The on-demand scan (ScanAnvilDeps, FindOrCreateBeadID) reads the working tree
// directly; it gets the same excludes as the scheduled scan (Copilot on Forge #951).
func TestOnDemandWorktreeSourceGetsTheExcludes(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"samples/HostileHost/HostileHost.csproj", "src/App/App.csproj", "tools/gen/package.json"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte("<Project />"), 0o644))
	}
	s := &Scanner{}
	s.UpdateAnvilExcludes(map[string][]string{"explorer": {"tools/**"}})
	paths, err := s.withExcludes("explorer", worktreeSource{root: root}).Paths(context.Background())
	require.NoError(t, err)
	assert.Contains(t, paths, "src/App/App.csproj")
	for _, p := range paths {
		assert.NotContains(t, p, "samples/", "the default exclude applies to the working tree too")
		assert.NotContains(t, p, "tools/", "and so do the anvil's own globs")
	}
}
