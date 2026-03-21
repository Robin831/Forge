package depupdate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Preview tests ----

func TestPreview_ReturnsOneResultPerGroup(t *testing.T) {
	groups := makeUpdateGroups(3)
	results := Preview(groups)
	require.Len(t, results, 3)
}

func TestPreview_AllResultsNotApplied(t *testing.T) {
	groups := makeUpdateGroups(4)
	results := Preview(groups)
	for _, r := range results {
		assert.False(t, r.Applied, "Preview should never mark a group as applied")
		assert.NoError(t, r.Err)
	}
}

func TestPreview_PreservesGroupOrder(t *testing.T) {
	groups := makeUpdateGroups(3)
	results := Preview(groups)
	for i, r := range results {
		assert.Equal(t, groups[i].Name, r.Group.Name)
	}
}

func TestPreview_EmptyGroups(t *testing.T) {
	results := Preview(nil)
	require.Empty(t, results)
}

// ---- Scan tests ----

func TestScan_CancelledContext_ReturnsEmptyReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	anvils := []Anvil{
		{Name: "repo1", Path: t.TempDir()},
	}
	reports, err := Scan(ctx, anvils, Options{})
	require.NoError(t, err, "Scan should not propagate context cancellation as an error")
	// The cancelled context should prevent the scan from running.
	assert.Empty(t, reports)
}

func TestScan_EmptyAnvils_ReturnsEmpty(t *testing.T) {
	reports, err := Scan(context.Background(), nil, Options{})
	require.NoError(t, err)
	assert.Empty(t, reports)
}

func TestScan_NilDB_DoesNotPanic(t *testing.T) {
	anvil := Anvil{
		Name: "test",
		Path: t.TempDir(), // no go.mod / package.json → scanners return nil
		DB:   nil,
	}
	// Should not panic even with a nil DB (depcheck.ScanAnvilDeps does not use DB).
	require.NotPanics(t, func() {
		_, _ = Scan(context.Background(), []Anvil{anvil}, Options{})
	})
}

// ---- Apply tests ----

func TestApply_CancelledContext_AllResultsHaveError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	groups := []UpdateGroup{
		{
			Name:      "pkg-a",
			Kind:      "patch",
			Ecosystem: "Go",
			Updates: []depcheck.ModuleUpdate{
				{Path: "example.com/a", Current: "v1.0.0", Latest: "v1.0.1", Kind: "patch"},
			},
		},
	}
	results, err := Apply(ctx, t.TempDir(), config.AnvilConfig{}, groups)
	require.NoError(t, err, "Apply never returns a non-nil error at the slice level")
	require.Len(t, results, 1)
	assert.False(t, results[0].Applied)
	assert.ErrorIs(t, results[0].Err, context.Canceled)
}

func TestApply_UnknownEcosystem_ResultHasError(t *testing.T) {
	dir := initTestGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")

	groups := []UpdateGroup{
		{
			Name:      "pkg-a",
			Kind:      "patch",
			Ecosystem: "unknown-ecosystem",
			Updates: []depcheck.ModuleUpdate{
				{Path: "pkg-a", Current: "1.0.0", Latest: "1.0.1", Kind: "patch"},
			},
		},
	}
	results, err := Apply(context.Background(), dir, config.AnvilConfig{}, groups)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.False(t, results[0].Applied)
	assert.Error(t, results[0].Err)
}

func TestApply_ReturnsOneResultPerGroup(t *testing.T) {
	dir := initTestGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")

	groups := []UpdateGroup{
		{Name: "a", Kind: "patch", Ecosystem: "unknown-a", Updates: []depcheck.ModuleUpdate{{Path: "a", Current: "1.0.0", Latest: "1.0.1"}}},
		{Name: "b", Kind: "patch", Ecosystem: "unknown-b", Updates: []depcheck.ModuleUpdate{{Path: "b", Current: "1.0.0", Latest: "1.0.1"}}},
	}
	results, err := Apply(context.Background(), dir, config.AnvilConfig{}, groups)
	require.NoError(t, err)
	require.Len(t, results, 2, "Apply must return exactly one Result per input group")
}
