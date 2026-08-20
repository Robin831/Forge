package temper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stepPaths indexes the detected steps by name for glob assertions.
func stepPaths(steps []Step) map[string][]string {
	out := make(map[string][]string, len(steps))
	for _, s := range steps {
		out[s.Name] = s.Paths
	}
	return out
}

// wouldRun reports the names of the steps that Run would execute for the given
// changed-file list, applying the same gate Run applies (see the Paths check in
// Run). Scan-only steps have no Command and always run.
func wouldRun(steps []Step, changed []string) []string {
	var names []string
	for _, s := range steps {
		if s.Command == "" && len(s.VerifyNoConflictMarkers) > 0 {
			names = append(names, s.Name)
			continue
		}
		if len(s.Paths) > 0 && changed != nil && !matchesChangedFiles(s.Paths, changed) {
			continue
		}
		names = append(names, s.Name)
	}
	return names
}

func TestDetectSteps_GoStepsCarryDefaultPaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644))

	paths := stepPaths(detectSteps(dir, &DetectOptions{DisableGolangciLint: true}, true))

	for _, name := range []string{"build", "vet", "test", "race"} {
		require.Contains(t, paths, name)
		assert.Equal(t, goStepPaths(dir), paths[name], "step %q must carry the Go default globs", name)
		assert.Subset(t, paths[name], defaultGoPaths, "step %q must carry the static Go globs", name)
	}
	// The globs must cover a Go file anywhere, the module files, and the
	// fixture trees tests read — a change to any of them changes what `go
	// build`/`go test` produce.
	for _, f := range []string{"main.go", "internal/x/y.go", "go.mod", "go.sum", "internal/x/testdata/case.json"} {
		assert.True(t, matchesChangedFiles(defaultGoPaths, []string{f}), "%q must match the Go globs", f)
	}
	// The lint config decides what the lint step reports, so it is in the set.
	assert.True(t, matchesChangedFiles(defaultGoPaths, []string{".golangci.yml"}))
	assert.False(t, matchesChangedFiles(defaultGoPaths, []string{"docs/architecture.md"}))
}

func TestDetectSteps_DotnetStepsCarryDefaultPaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "App.sln"), []byte(""), 0o644))

	paths := stepPaths(detectSteps(dir, nil, false))

	for _, name := range []string{"build", "test"} {
		require.Contains(t, paths, name)
		assert.Equal(t, defaultDotnetPaths, paths[name], "step %q must carry the .NET default globs", name)
	}
	for _, f := range []string{
		"src/Api/Program.cs",
		"src/Api/Api.csproj",
		"App.sln",
		"Directory.Build.props",
		"build/common.targets",
		"src/Api/appsettings.Development.json",
		"src/Web/Pages/Index.razor",
		"src/Web/Views/Home.cshtml",
		"global.json",
		"tests/Api.Tests/TestData/payload.xml",
		"tests/Api.Tests/Fixtures/response.txt",
	} {
		assert.True(t, matchesChangedFiles(defaultDotnetPaths, []string{f}), "%q must match the .NET globs", f)
	}
	assert.False(t, matchesChangedFiles(defaultDotnetPaths, []string{"README.md"}))
}

func TestDetectSteps_NodeSubdirGatedOnItsOwnTree(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "web"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "web", "package.json"), []byte("{}"), 0o644))

	paths := stepPaths(detectSteps(dir, nil, false))

	assert.Equal(t, []string{"web/**"}, paths["web:lint"])
	assert.Equal(t, []string{"web/**"}, paths["web:test"])
}

// TestDetectSteps_NodeAtRootIsUngated documents the deliberate exception: a
// Node project at the repository root has no directory boundary separating its
// inputs from the rest of the repo, so there is no conservative subset to gate
// on and the steps keep running unconditionally.
func TestDetectSteps_NodeAtRootIsUngated(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	paths := stepPaths(detectSteps(dir, nil, false))

	assert.Nil(t, paths["lint"])
	assert.Nil(t, paths["test"])
}

// TestDetectSteps_MixedReactDotnetRepo is the acceptance case from Forge-bxdg:
// a React frontend and a .NET backend in one repository, auto-detected with no
// per-anvil temper config at all. A one-line frontend change must not drag the
// whole .NET build and test suite along with it, and vice versa.
func TestDetectSteps_MixedReactDotnetRepo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "App.sln"), []byte(""), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", "Api"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "Api", "Api.csproj"), []byte(""), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "client"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client", "package.json"), []byte("{}"), 0o644))

	steps := detectSteps(dir, nil, false)
	require.ElementsMatch(t, []string{"build", "test", "client:lint", "client:test"}, stepNames(steps))

	frontendOnly := wouldRun(steps, []string{"client/src/App.tsx"})
	assert.ElementsMatch(t, []string{"client:lint", "client:test"}, frontendOnly,
		"a frontend-only diff must not run dotnet build/test")

	backendOnly := wouldRun(steps, []string{"src/Api/Program.cs"})
	assert.ElementsMatch(t, []string{"build", "test"}, backendOnly,
		"a backend-only diff must not run the npm steps")

	both := wouldRun(steps, []string{"src/Api/Program.cs", "client/src/App.tsx"})
	assert.ElementsMatch(t, []string{"build", "test", "client:lint", "client:test"}, both)

	// The unknown case: no changed-file list at all still runs everything.
	assert.ElementsMatch(t, []string{"build", "test", "client:lint", "client:test"}, wouldRun(steps, nil))
}

// TestRun_AutoDetectedStepsSkipWhenNothingMatches pins the end-to-end behaviour
// through Run itself: gating applies to auto-detected steps (which now declare
// Paths), the skipped steps carry the path-filter reason, and a run whose every
// step was gated out still passes.
func TestRun_AutoDetectedStepsSkipWhenNothingMatches(t *testing.T) {
	cfg := Config{
		Steps: []Step{
			{Name: "build", Command: "echo", Args: []string{"go"}, Timeout: 5 * time.Second, Paths: defaultGoPaths},
			{Name: "dotnet-build", Command: "echo", Args: []string{"dotnet"}, Timeout: 5 * time.Second, Paths: defaultDotnetPaths},
		},
		ChangedFiles: []string{"client/src/App.tsx"},
	}

	result := Run(context.Background(), t.TempDir(), cfg, nil, "test-bead", "test-anvil")

	require.Len(t, result.Steps, 2)
	for _, s := range result.Steps {
		assert.True(t, s.Skipped, "step %q should be skipped", s.Name)
		assert.Equal(t, SkipReasonPathFilter, s.SkipReason)
	}
	assert.True(t, result.Passed)
	assert.Equal(t, []string{"build", "dotnet-build"}, PathSkippedStepNames(result))
}

// TestPathSkippedStepNames_ExcludesBlockedSteps keeps the two reasons a step can
// fail to run apart: only changed-file gating is reported as gating, so a
// blocked npm install is never mistaken for a diff that did not need it.
func TestPathSkippedStepNames_ExcludesBlockedSteps(t *testing.T) {
	r := &Result{Steps: []StepResult{
		{Name: "gated", Skipped: true, SkipReason: SkipReasonPathFilter},
		{Name: "blocked", Skipped: true, SkipReason: SkipReasonBlockedInstall},
		{Name: "ran", Passed: true},
	}}

	assert.Equal(t, []string{"gated"}, PathSkippedStepNames(r))
	assert.Nil(t, PathSkippedStepNames(nil))
}
