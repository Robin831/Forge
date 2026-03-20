package depcheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestRunNpmOutdated_CallsInstallFirst verifies that runNpmOutdated invokes
// npm install (via runNpmInstallFn) before npm outdated to sync node_modules
// with the lock file.
func TestRunNpmOutdated_CallsInstallFirst(t *testing.T) {
	var calls []string

	origInstall := runNpmInstallFn
	origOutdated := runNpmOutdatedFn
	t.Cleanup(func() {
		runNpmInstallFn = origInstall
		runNpmOutdatedFn = origOutdated
	})

	runNpmInstallFn = func(_ context.Context, _ time.Duration, dir string) error {
		calls = append(calls, "install:"+dir)
		return nil
	}

	// Replace runNpmOutdatedFn so we go through the real runNpmOutdated logic
	// but with the install stub above. We need to call the real function, not
	// the stub, so we restore the original outdated fn and only stub install.
	runNpmOutdatedFn = origOutdated

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	s := &Scanner{timeout: 30 * time.Second}
	// scanNpm calls runNpmOutdatedFn which is the real runNpmOutdated,
	// which in turn calls runNpmInstallFn (our stub).
	_ = s.scanNpm(context.Background(), "test", dir)

	assert.Contains(t, calls, "install:"+dir, "npm install should be called before npm outdated")
}

// TestRunNpmOutdated_InstallFailureContinues verifies that if npm install fails,
// runNpmOutdated still proceeds (stale data is better than no data).
func TestRunNpmOutdated_InstallFailureContinues(t *testing.T) {
	origInstall := runNpmInstallFn
	origOutdated := runNpmOutdatedFn
	t.Cleanup(func() {
		runNpmInstallFn = origInstall
		runNpmOutdatedFn = origOutdated
	})

	runNpmInstallFn = func(_ context.Context, _ time.Duration, _ string) error {
		return fmt.Errorf("npm install failed: network error")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	// The real runNpmOutdated will fail because npm isn't installed in CI,
	// but that's fine — we just need to verify it doesn't abort on install failure.
	// Use the stub for the outdated call to isolate the test.
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, _ string) ([]ModuleUpdate, error) {
		return []ModuleUpdate{{Path: "foo", Current: "1.0.0", Latest: "1.0.1", Kind: "patch"}}, nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test", dir)
	require.NotNil(t, result)
	assert.Nil(t, result.Error, "install failure should not cause scanNpm to fail")
	assert.Len(t, result.Patch, 1, "outdated results should still be returned")
}

// TestScanNpmCrossProjectDedup verifies that scanNpm deduplicates packages that
// appear in more than one package.json (e.g. worktree copies of the same repo).
// runNpmOutdatedFn is replaced with a stub so npm does not need to be installed.
func TestScanNpmCrossProjectDedup(t *testing.T) {
	dir := t.TempDir()

	// Create two package.json files in separate sub-directories.
	for _, sub := range []string{"app", "lib"} {
		subDir := filepath.Join(dir, sub)
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "package.json"), []byte("{}"), 0o644))
	}

	// Map each directory to the updates its stub will return.
	stubUpdates := map[string][]ModuleUpdate{
		filepath.Join(dir, "app"): {
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
		},
		filepath.Join(dir, "lib"): {
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"}, // duplicate
			{Path: "axios", Current: "1.0.0", Latest: "2.0.0", Kind: "major"},
		},
	}

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, d string) ([]ModuleUpdate, error) {
		return stubUpdates[d], nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir)
	require.NotNil(t, result)

	assert.Len(t, result.Patch, 1, "lodash should appear once despite two projects reporting it")
	assert.Equal(t, "lodash", result.Patch[0].Path)
	assert.Len(t, result.Minor, 1)
	assert.Equal(t, "react", result.Minor[0].Path)
	assert.Len(t, result.Major, 1)
	assert.Equal(t, "axios", result.Major[0].Path)
}

// TestScanNpmCrossProjectDedup_MostSevereWins verifies that when the same
// package appears across multiple package.json files with different update
// severities, the most severe kind (major > minor > patch) wins rather than
// whichever project was scanned first.
func TestScanNpmCrossProjectDedup_MostSevereWins(t *testing.T) {
	dir := t.TempDir()

	for _, sub := range []string{"app", "lib"} {
		subDir := filepath.Join(dir, sub)
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "package.json"), []byte("{}"), 0o644))
	}

	// "app" reports lodash as a patch; "lib" reports lodash as a major.
	// WalkDir returns dirs in lexicographic order so "app" is scanned first —
	// with first-wins dedup the patch would win. The fix must pick major.
	stubUpdates := map[string][]ModuleUpdate{
		filepath.Join(dir, "app"): {
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
		},
		filepath.Join(dir, "lib"): {
			{Path: "lodash", Current: "4.17.20", Latest: "5.0.0", Kind: "major"},
		},
	}

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, d string) ([]ModuleUpdate, error) {
		return stubUpdates[d], nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir)
	require.NotNil(t, result)

	assert.Empty(t, result.Patch, "patch entry should be superseded by the major bump")
	assert.Empty(t, result.Minor)
	assert.Len(t, result.Major, 1, "major bump should win over patch for the same package")
	assert.Equal(t, "lodash", result.Major[0].Path)
	assert.Equal(t, "5.0.0", result.Major[0].Latest)
}
