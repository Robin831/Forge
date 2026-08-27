package depcheck

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNpmProjectDiscovery(t *testing.T) {
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

	// Create package.json inside .previews (Kiln preview checkout — must be
	// skipped: its node_modules is a junction into the main checkout, and an
	// npm ci there deletes the main checkout's dependencies through the link)
	pv := filepath.Join(dir, ".previews", "some-bead", "client")
	require.NoError(t, os.MkdirAll(pv, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pv, "package.json"), []byte("{}"), 0o644))

	// Create package.json inside bin (should be skipped)
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "package.json"), []byte("{}"), 0o644))

	// Create package.json inside obj (should be skipped)
	obj := filepath.Join(dir, "obj")
	require.NoError(t, os.MkdirAll(obj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(obj, "package.json"), []byte("{}"), 0o644))

	dirs := npmProjectDirsIn(t, dir)
	assert.Len(t, dirs, 2)
	assert.Contains(t, dirs, dir)
	assert.Contains(t, dirs, sub)
}

func TestNpmProjectDiscovery_NoPackageJson(t *testing.T) {
	dir := t.TempDir()
	dirs := npmProjectDirsIn(t, dir)
	assert.Empty(t, dirs)
}

// TestRunNpmOutdated_CallsInstallFirst verifies that runNpmOutdated invokes
// npm ci/install (via runNpmInstallFn) before npm outdated to sync node_modules
// with the lock file. Uses runNpmCmdFn stub to keep the test hermetic.
func TestRunNpmOutdated_CallsInstallFirst(t *testing.T) {
	var calls []string

	origInstall := runNpmInstallFn
	origOutdated := runNpmOutdatedFn
	origCmd := runNpmCmdFn
	t.Cleanup(func() {
		runNpmInstallFn = origInstall
		runNpmOutdatedFn = origOutdated
		runNpmCmdFn = origCmd
	})

	runNpmInstallFn = func(_ context.Context, _ time.Duration, dir string) error {
		calls = append(calls, "install:"+dir)
		return nil
	}

	// Use the real runNpmOutdated so the ordering logic is exercised.
	runNpmOutdatedFn = origOutdated

	// Stub the npm command execution so no real npm binary is invoked.
	runNpmCmdFn = func(_ context.Context, _ time.Duration, dir string, args ...string) ([]byte, error) {
		calls = append(calls, "outdated:"+dir)
		return []byte("{}"), nil
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	s := &Scanner{timeout: 30 * time.Second}
	_ = s.scanNpm(context.Background(), "test", dir, worktreeSource{root: dir})

	require.GreaterOrEqual(t, len(calls), 2, "expected install and outdated calls")
	assert.Equal(t, "install:"+dir, calls[0], "npm install should be called before npm outdated")
	assert.Equal(t, "outdated:"+dir, calls[1], "npm outdated should be called after npm install")
}

// TestRunNpmOutdated_InstallFailureContinues verifies that if npm ci/install fails,
// runNpmOutdated still proceeds to run npm outdated (stale data is better than no data).
// This test exercises the real runNpmOutdated, stubbing only the command execution layer.
func TestRunNpmOutdated_InstallFailureContinues(t *testing.T) {
	origInstall := runNpmInstallFn
	origOutdated := runNpmOutdatedFn
	origCmd := runNpmCmdFn
	t.Cleanup(func() {
		runNpmInstallFn = origInstall
		runNpmOutdatedFn = origOutdated
		runNpmCmdFn = origCmd
	})

	// Force npm install to fail; runNpmOutdated should log/ignore this and
	// still attempt to run 'npm outdated'.
	runNpmInstallFn = func(_ context.Context, _ time.Duration, _ string) error {
		return fmt.Errorf("npm install failed: network error")
	}

	// Use the real runNpmOutdated so the install-failure-then-continue path is covered.
	runNpmOutdatedFn = origOutdated

	// Stub the underlying npm command execution so that 'npm outdated' succeeds
	// without requiring a real npm binary.
	runNpmCmdFn = func(_ context.Context, _ time.Duration, _ string, _ ...string) ([]byte, error) {
		return []byte(`{"foo":{"current":"1.0.0","latest":"1.0.1","type":"patch"}}`), nil
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test", dir, worktreeSource{root: dir})
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
	result := s.scanNpm(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
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
	result := s.scanNpm(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
	require.NotNil(t, result)

	assert.Empty(t, result.Patch, "patch entry should be superseded by the major bump")
	assert.Empty(t, result.Minor)
	assert.Len(t, result.Major, 1, "major bump should win over patch for the same package")
	assert.Equal(t, "lodash", result.Major[0].Path)
	assert.Equal(t, "5.0.0", result.Major[0].Latest)
}

// TestScanNpm_SkipsWhileKilnPreviewLive verifies that an anvil with a live Kiln
// preview gets no npm sync at all: `npm ci` deletes node_modules first, and the
// preview's worktree has that node_modules linked into it, so the delete would
// gut the main checkout out from under every worktree using it.
func TestScanNpm_SkipsWhileKilnPreviewLive(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	origInstall := runNpmInstallFn
	origOutdated := runNpmOutdatedFn
	origCmd := runNpmCmdFn
	t.Cleanup(func() {
		runNpmInstallFn = origInstall
		runNpmOutdatedFn = origOutdated
		runNpmCmdFn = origCmd
	})

	var npmCalls []string
	runNpmInstallFn = func(_ context.Context, _ time.Duration, d string) error {
		npmCalls = append(npmCalls, "install:"+d)
		return nil
	}
	runNpmCmdFn = func(_ context.Context, _ time.Duration, d string, args ...string) ([]byte, error) {
		npmCalls = append(npmCalls, "npm "+strings.Join(args, " ")+":"+d)
		return []byte("{}"), nil
	}

	var asked []string
	s := &Scanner{timeout: 30 * time.Second}
	s.SetPreviewLiveness(func(anvil string) string {
		asked = append(asked, anvil)
		return "Forge-prev"
	})

	result := s.scanNpm(context.Background(), "heimdall", dir, worktreeSource{root: dir})

	assert.Nil(t, result, "npm results should be skipped entirely, not reported from a tree we refused to sync")
	assert.Empty(t, npmCalls, "no npm command may run while a preview holds node_modules")
	assert.Equal(t, []string{"heimdall"}, asked, "liveness should be checked for the scanned anvil")
}

// TestScanNpm_SkipsWhenPreviewStartsMidScan verifies the re-check inside the
// per-project loop: a preview that comes up after the scan began still stops
// the sync before npm is spawned.
func TestScanNpm_SkipsWhenPreviewStartsMidScan(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	var outdatedCalls int
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, _ string) ([]ModuleUpdate, error) {
		outdatedCalls++
		return nil, nil
	}

	// No preview at the top of the scan; one is live by the time the loop is
	// about to spawn npm for the first project.
	var checks int
	s := &Scanner{timeout: 30 * time.Second}
	s.SetPreviewLiveness(func(string) string {
		checks++
		if checks == 1 {
			return ""
		}
		return "Forge-prev"
	})

	result := s.scanNpm(context.Background(), "heimdall", dir, worktreeSource{root: dir})

	assert.Nil(t, result, "a preview appearing mid-scan should still skip the npm half")
	assert.Zero(t, outdatedCalls, "npm outdated (and the npm ci it fronts) must not run")
	assert.Equal(t, 2, checks, "liveness is re-read immediately before the npm spawn")
}

// TestScanNpm_RunsWithoutLivePreview verifies the unchanged path: no callback
// installed, or a callback reporting no preview, both scan exactly as before.
func TestScanNpm_RunsWithoutLivePreview(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	var outdatedCalls int
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, _ string) ([]ModuleUpdate, error) {
		outdatedCalls++
		return []ModuleUpdate{{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"}}, nil
	}

	for _, tc := range []struct {
		name string
		fn   PreviewLivenessFunc
	}{
		{"nil callback", nil},
		{"no preview", func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outdatedCalls = 0
			s := &Scanner{timeout: 30 * time.Second}
			s.SetPreviewLiveness(tc.fn)

			result := s.scanNpm(context.Background(), "heimdall", dir, worktreeSource{root: dir})

			require.NotNil(t, result)
			assert.Equal(t, 1, outdatedCalls, "the npm scan should run exactly as before")
			assert.Len(t, result.Patch, 1)
		})
	}
}

// TestScanNpm_MonorepoSiblingDoesNotSilenceARealUpdate pins the scope of the
// npm reconcile. "app" still pins lodash at ^4.17.20 and is genuinely
// outdated; the sibling "lib" has already been bumped to ^4.17.21. Folding both
// package.json files into one map made the sibling's pin read as "upstream has
// already done this upgrade" and dropped app's real update entirely — in a
// monorepo, silently and with nothing left to notice it by.
func TestScanNpm_MonorepoSiblingDoesNotSilenceARealUpdate(t *testing.T) {
	dir := t.TempDir()

	manifests := map[string]string{
		"app": `{"dependencies":{"lodash":"^4.17.20"}}`,
		"lib": `{"dependencies":{"lodash":"^4.17.21"}}`,
	}
	for sub, manifest := range manifests {
		subDir := filepath.Join(dir, sub)
		require.NoError(t, os.MkdirAll(subDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "package.json"), []byte(manifest), 0o644))
	}

	// Only "app" is behind; "lib" is already at the latest, so npm reports
	// nothing for it.
	stubUpdates := map[string][]ModuleUpdate{
		filepath.Join(dir, "app"): {
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
		},
	}

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, d string) ([]ModuleUpdate, error) {
		return stubUpdates[d], nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
	require.NotNil(t, result)

	require.Len(t, result.Patch, 1, "app's own manifest still pins the old version, so its update is real")
	assert.Equal(t, "lodash", result.Patch[0].Path)
}

// TestScanNpm_DropsWhatTheProjectsOwnManifestAlreadyPins is the other half of
// the same scope: a project whose OWN committed package.json is already at the
// latest is reporting an update upstream has merged, and that entry is dropped.
func TestScanNpm_DropsWhatTheProjectsOwnManifestAlreadyPins(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies":{"lodash":"^4.17.21"}}`), 0o644))

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, _ string) ([]ModuleUpdate, error) {
		return []ModuleUpdate{
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
		}, nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir, worktreeSource{root: dir})
	require.NotNil(t, result)
	assert.Empty(t, result.Patch, "the checkout is behind what its own manifest commits")
}

// TestScanNpm_SkipsAProjectAbsentFromTheCheckout pins the fail-open guard that
// moving discovery to the tracking ref made necessary: a package.json the ref
// tracks need not exist on disk (a sparse or partial checkout, a directory
// deleted locally), and npm has nothing to read there. The rest of the scan
// still runs, and the skip leaves a log line rather than passing in silence.
func TestScanNpm_SkipsAProjectAbsentFromTheCheckout(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "app"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app", "package.json"),
		[]byte(`{"dependencies":{"lodash":"^4.17.20"}}`), 0o644))

	// The ref tracks a second workspace this checkout never materialized.
	src := stubSource{files: map[string]string{
		"app/package.json":   `{"dependencies":{"lodash":"^4.17.20"}}`,
		"ghost/package.json": `{"dependencies":{"lodash":"^4.17.20"}}`,
	}}

	var scannedDirs []string
	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, d string) ([]ModuleUpdate, error) {
		scannedDirs = append(scannedDirs, d)
		return []ModuleUpdate{{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"}}, nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir, src)
	require.NotNil(t, result)
	require.NoError(t, result.Error, "one absent project must not fail the whole ecosystem")

	assert.Equal(t, []string{filepath.Join(dir, "app")}, scannedDirs,
		"npm runs only where the manifest is actually present")
	require.Len(t, result.Patch, 1)
	assert.Equal(t, "lodash", result.Patch[0].Path)
}

// TestScanNpm_AbsentManifestInAnExistingDirectoryIsSkipped is the case a
// directory-level guard let through. Upstream adds web/frontend/package.json
// while the stale checkout already has web/frontend/ (holding something else),
// so the directory exists and the manifest does not. npm resolves its prefix by
// walking UP, so running there would reinstall the PARENT project and report the
// parent's packages as this project's — the cross-project bleed the per-project
// reconcile exists to prevent.
func TestScanNpm_AbsentManifestInAnExistingDirectoryIsSkipped(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies":{"lodash":"^4.17.20"}}`), 0o644))
	// The directory exists in the checkout; the manifest upstream added does not.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "web", "frontend"), 0o755))

	src := stubSource{files: map[string]string{
		"package.json":              `{"dependencies":{"lodash":"^4.17.20"}}`,
		"web/frontend/package.json": `{"dependencies":{"react":"^18.0.0"}}`,
	}}

	var scannedDirs []string
	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, d string) ([]ModuleUpdate, error) {
		scannedDirs = append(scannedDirs, d)
		return []ModuleUpdate{{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"}}, nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir, src)
	require.NotNil(t, result)
	assert.Equal(t, []string{dir}, scannedDirs,
		"a directory without the manifest is not a project npm can be run in")
}

// TestScanNpm_WhollyUnscannableAnvilReportsNothingAndSaysSo is the contract for
// the degenerate case: every tracked project missing reports empty, exactly as
// "everything is up to date" does, so it must at least leave a trace.
func TestScanNpm_WhollyUnscannableAnvilReportsNothingAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	src := stubSource{files: map[string]string{"web/package.json": `{"dependencies":{"lodash":"^4.17.20"}}`}}

	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, _ string) ([]ModuleUpdate, error) {
		t.Fatal("npm must not run for a project that is not in the checkout")
		return nil, nil
	}

	var logged bytes.Buffer
	restoreLog := captureLog(t, &logged)
	s := &Scanner{timeout: 30 * time.Second}
	result := s.scanNpm(context.Background(), "test-anvil", dir, src)
	restoreLog()

	require.NotNil(t, result)
	assert.Empty(t, result.Patch)
	assert.Empty(t, result.Minor)
	assert.Empty(t, result.Major)
	assert.Contains(t, logged.String(), "none of the 1 npm project(s)",
		"a scan that read nothing must not be indistinguishable from a clean one")
}

// captureLog redirects the standard logger into buf, returning the restore
// function so a test can assert on the output it produced.
func captureLog(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	restore := func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
	t.Cleanup(restore)
	return restore
}
