package depupdate

import (
	"context"
	"testing"

	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubPeerDeps returns a peerDepFetcher replacement that returns pre-configured
// peer dependency maps keyed by "pkg@version".
func stubPeerDeps(data map[string]map[string]string) func(ctx context.Context, pkg, version string) map[string]string {
	return func(_ context.Context, pkg, version string) map[string]string {
		return data[pkg+"@"+version]
	}
}

func TestGroupUpdates_PeerDepMerging(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()

	// @vitejs/plugin-react@6.0.1 has peerDependencies: {"vite": "^8"}
	peerDepFetcher = stubPeerDeps(map[string]map[string]string{
		"@vitejs/plugin-react@6.0.1": {"vite": "^8"},
		"vite@8.0.0":                 nil,
	})

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Minor: []depcheck.ModuleUpdate{
			{Path: "vite", Current: "7.0.0", Latest: "8.0.0", Kind: "major"},
			{Path: "@vitejs/plugin-react", Current: "5.0.0", Latest: "6.0.1", Kind: "major"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	assert.Equal(t, "vite ecosystem", groups[0].Name)
	assert.Len(t, groups[0].Updates, 2)
	assert.Equal(t, "major", groups[0].Kind)
}

func TestGroupUpdates_TransitivePeerDeps(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()

	// A peers with B, B peers with C → all in one group.
	peerDepFetcher = stubPeerDeps(map[string]map[string]string{
		"pkg-a@2.0.0": {"pkg-b": "^2"},
		"pkg-b@2.0.0": {"pkg-c": "^2"},
		"pkg-c@2.0.0": nil,
	})

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Minor: []depcheck.ModuleUpdate{
			{Path: "pkg-a", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
			{Path: "pkg-b", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
			{Path: "pkg-c", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	assert.Len(t, groups[0].Updates, 3)
	// Root should be pkg-c (most depended on).
	assert.Equal(t, "pkg-c ecosystem", groups[0].Name)
}

func TestGroupUpdates_ScopeGrouping(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()
	peerDepFetcher = stubPeerDeps(nil) // no peer deps

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Patch: []depcheck.ModuleUpdate{
			{Path: "@tailwindcss/vite", Current: "1.0.0", Latest: "1.0.1", Kind: "patch"},
			{Path: "@tailwindcss/postcss", Current: "1.0.0", Latest: "1.0.1", Kind: "patch"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	assert.Equal(t, "@tailwindcss packages", groups[0].Name)
	assert.Len(t, groups[0].Updates, 2)
	assert.Equal(t, "patch", groups[0].Kind)
}

func TestGroupUpdates_StandaloneFallback(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()
	peerDepFetcher = stubPeerDeps(nil)

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Patch: []depcheck.ModuleUpdate{
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			{Path: "express", Current: "4.18.0", Latest: "4.18.2", Kind: "patch"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 2)

	names := map[string]bool{}
	for _, g := range groups {
		names[g.Name] = true
		assert.Len(t, g.Updates, 1)
	}
	assert.True(t, names["lodash"])
	assert.True(t, names["express"])
}

func TestGroupUpdates_NpmViewFailureTreatedAsStandalone(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()

	// All npm view calls fail (return nil).
	peerDepFetcher = func(_ context.Context, _, _ string) map[string]string {
		return nil
	}

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Minor: []depcheck.ModuleUpdate{
			{Path: "vite", Current: "7.0.0", Latest: "8.0.0", Kind: "major"},
			{Path: "react", Current: "18.0.0", Latest: "19.0.0", Kind: "major"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	// Each should be standalone since peer dep discovery failed.
	require.Len(t, groups, 2)
	for _, g := range groups {
		assert.Len(t, g.Updates, 1)
	}
}

func TestGroupUpdates_KindComputation(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()

	peerDepFetcher = stubPeerDeps(map[string]map[string]string{
		"pkg-a@2.0.0": {"pkg-b": "^1"},
		"pkg-b@1.1.0": nil,
	})

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Patch: []depcheck.ModuleUpdate{
			{Path: "pkg-b", Current: "1.0.0", Latest: "1.1.0", Kind: "minor"},
		},
		Major: []depcheck.ModuleUpdate{
			{Path: "pkg-a", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	assert.Equal(t, "major", groups[0].Kind)
}

func TestGroupUpdates_NonNpmSkipsPeerDeps(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()

	called := false
	peerDepFetcher = func(_ context.Context, _, _ string) map[string]string {
		called = true
		return nil
	}

	results := []*depcheck.CheckResult{
		{
			Ecosystem: "Go",
			Patch: []depcheck.ModuleUpdate{
				{Path: "github.com/foo/bar", Current: "v1.0.0", Latest: "v1.0.1", Kind: "patch"},
			},
		},
		{
			Ecosystem: "NuGet",
			Minor: []depcheck.ModuleUpdate{
				{Path: "Newtonsoft.Json", Current: "13.0.1", Latest: "13.0.3", Kind: "patch"},
			},
		},
	}

	groups := GroupUpdates(context.Background(), results)
	assert.False(t, called, "peerDepFetcher should not be called for non-npm ecosystems")
	require.Len(t, groups, 2)
}

func TestGroupUpdates_EmptyInput(t *testing.T) {
	groups := GroupUpdates(context.Background(), nil)
	assert.Nil(t, groups)

	groups = GroupUpdates(context.Background(), []*depcheck.CheckResult{})
	assert.Nil(t, groups)
}

func TestGroupUpdates_ErroredResultsSkipped(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()
	peerDepFetcher = stubPeerDeps(nil)

	results := []*depcheck.CheckResult{
		{
			Ecosystem: "npm",
			Error:     assert.AnError,
			Minor: []depcheck.ModuleUpdate{
				{Path: "should-not-appear", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
			},
		},
		{
			Ecosystem: "npm",
			Patch: []depcheck.ModuleUpdate{
				{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			},
		},
	}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	assert.Equal(t, "lodash", groups[0].Name)
}

func TestGroupUpdates_MixedPeerAndScope(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()

	// vite + plugin are peer deps; tailwindcss packages are scope-grouped.
	peerDepFetcher = stubPeerDeps(map[string]map[string]string{
		"@vitejs/plugin-react@6.0.0": {"vite": "^8"},
		"vite@8.0.0":                 nil,
		"@tailwindcss/vite@1.0.1":    nil,
		"@tailwindcss/postcss@1.0.1": nil,
		"lodash@4.17.21":             nil,
	})

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Minor: []depcheck.ModuleUpdate{
			{Path: "vite", Current: "7.0.0", Latest: "8.0.0", Kind: "major"},
			{Path: "@vitejs/plugin-react", Current: "5.0.0", Latest: "6.0.0", Kind: "major"},
		},
		Patch: []depcheck.ModuleUpdate{
			{Path: "@tailwindcss/vite", Current: "1.0.0", Latest: "1.0.1", Kind: "patch"},
			{Path: "@tailwindcss/postcss", Current: "1.0.0", Latest: "1.0.1", Kind: "patch"},
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 3)

	names := map[string]bool{}
	for _, g := range groups {
		names[g.Name] = true
	}
	assert.True(t, names["vite ecosystem"])
	assert.True(t, names["@tailwindcss packages"])
	assert.True(t, names["lodash"])
}

func TestExtractScope(t *testing.T) {
	tests := []struct {
		path  string
		scope string
	}{
		{"@tailwindcss/vite", "tailwindcss"},
		{"@vitejs/plugin-react", "vitejs"},
		{"lodash", ""},
		{"react-dom", ""},
		{"@scope", ""},    // malformed
		{"@scope/", ""},   // malformed: has slash but empty name, treated as no scope
		{"github.com/foo/bar", ""}, // Go module path, no @ prefix
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.scope, extractScope(tt.path))
		})
	}
}

func TestWorstKind(t *testing.T) {
	tests := []struct {
		name    string
		updates []depcheck.ModuleUpdate
		want    string
	}{
		{"all patch", []depcheck.ModuleUpdate{{Kind: "patch"}, {Kind: "patch"}}, "patch"},
		{"minor wins", []depcheck.ModuleUpdate{{Kind: "patch"}, {Kind: "minor"}}, "minor"},
		{"major wins", []depcheck.ModuleUpdate{{Kind: "patch"}, {Kind: "minor"}, {Kind: "major"}}, "major"},
		{"single major", []depcheck.ModuleUpdate{{Kind: "major"}}, "major"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, worstKind(tt.updates))
		})
	}
}

func TestGroupUpdates_SourceDirPreserved(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()
	peerDepFetcher = stubPeerDeps(nil)

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Patch: []depcheck.ModuleUpdate{
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch", SourceDir: "/repo/web"},
			{Path: "express", Current: "4.18.0", Latest: "4.18.2", Kind: "patch", SourceDir: "/repo/web"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 2)
	for _, g := range groups {
		assert.Equal(t, "/repo/web", g.SourceDir, "SourceDir should be preserved for standalone group %q", g.Name)
	}
}

func TestGroupUpdates_SourceDirPreservedInScopeGroup(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()
	peerDepFetcher = stubPeerDeps(nil)

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Patch: []depcheck.ModuleUpdate{
			{Path: "@tailwindcss/vite", Current: "1.0.0", Latest: "1.0.1", Kind: "patch", SourceDir: "/repo/web"},
			{Path: "@tailwindcss/postcss", Current: "1.0.0", Latest: "1.0.1", Kind: "patch", SourceDir: "/repo/web"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	assert.Equal(t, "@tailwindcss packages", groups[0].Name)
	assert.Equal(t, "/repo/web", groups[0].SourceDir)
}

func TestGroupUpdates_SingleScopedPackageIsStandalone(t *testing.T) {
	orig := peerDepFetcher
	defer func() { peerDepFetcher = orig }()
	peerDepFetcher = stubPeerDeps(nil)

	results := []*depcheck.CheckResult{{
		Ecosystem: "npm",
		Patch: []depcheck.ModuleUpdate{
			{Path: "@types/node", Current: "18.0.0", Latest: "18.0.1", Kind: "patch"},
		},
	}}

	groups := GroupUpdates(context.Background(), results)
	require.Len(t, groups, 1)
	// Single scoped package should be standalone, not a scope group.
	assert.Equal(t, "@types/node", groups[0].Name)
}
