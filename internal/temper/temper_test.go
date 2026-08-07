package temper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSummary_IncludesFailedStepOutput(t *testing.T) {
	r := &Result{
		Steps: []StepResult{
			{Name: "build", Passed: true, Duration: 2_000_000_000},
			{Name: "test", Passed: false, Duration: 5_000_000_000, Output: "--- FAIL: TestFoo (0.01s)\n    foo_test.go:42: expected 42 got 0\nFAIL\n"},
		},
		FailedStep: "test",
	}
	summary := buildSummary(r)

	assert.Contains(t, summary, "[PASS] build")
	assert.Contains(t, summary, "[FAIL] test")
	assert.Contains(t, summary, "foo_test.go:42: expected 42 got 0")
	assert.Contains(t, summary, "Failed at step: test")
}

func TestBuildSummary_TruncatesLongOutput(t *testing.T) {
	// Use distinct head/tail markers so we can verify the tail is preserved
	// and the head (which precedes the truncation point) is dropped.
	head := "HEAD_MARKER_" + strings.Repeat("h", 100)
	tail := strings.Repeat("t", 100) + "_TAIL_MARKER"
	longOutput := head + strings.Repeat("x", maxStepOutputLen) + tail
	r := &Result{
		Steps: []StepResult{
			{Name: "test", Passed: false, Duration: 1_000_000_000, Output: longOutput},
		},
		FailedStep: "test",
	}
	summary := buildSummary(r)

	assert.Contains(t, summary, "... (truncated)")
	// The tail of the output should be preserved (most relevant errors are at the end)
	assert.Contains(t, summary, "_TAIL_MARKER")
	// The head should have been truncated away
	assert.NotContains(t, summary, "HEAD_MARKER_")
}

func TestBuildSummary_NoOutputForPassingSteps(t *testing.T) {
	r := &Result{
		Steps: []StepResult{
			{Name: "build", Passed: true, Duration: 1_000_000_000, Output: "build output here"},
			{Name: "test", Passed: true, Duration: 2_000_000_000, Output: "ok  ./..."},
		},
		Passed: true,
	}
	summary := buildSummary(r)

	assert.NotContains(t, summary, "build output here")
	assert.NotContains(t, summary, "ok  ./...")
	assert.Contains(t, summary, "All required checks passed")
}

func TestBuildSummary_IncludesOptionalWarnOutput(t *testing.T) {
	r := &Result{
		Steps: []StepResult{
			{Name: "build", Passed: true, Duration: 1_000_000_000},
			{Name: "lint", Passed: false, Optional: true, Duration: 1_000_000_000, Output: "foo.go:10: unused variable x"},
			{Name: "test", Passed: true, Duration: 2_000_000_000},
		},
		Passed: true,
	}
	summary := buildSummary(r)

	assert.Contains(t, summary, "[WARN] lint")
	assert.Contains(t, summary, "unused variable x")
	assert.Contains(t, summary, "All required checks passed")
}

// goModDir creates a temp directory containing a go.mod so detectSteps
// recognises the project as Go.
func goModDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644))
	return dir
}

func stepNames(steps []Step) []string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return names
}

func TestDefaultConfigWithRace_IncludesRaceStep(t *testing.T) {
	dir := goModDir(t)
	opts := &DetectOptions{DisableGolangciLint: true}

	cfg := DefaultConfigWithRace(dir, opts, true)

	assert.True(t, cfg.GoRaceDetection)
	names := stepNames(cfg.Steps)
	assert.Contains(t, names, "race", "expected a 'race' step when GoRaceDetection is true")

	// Verify the race step has the expected command and args.
	for _, s := range cfg.Steps {
		if s.Name == "race" {
			assert.Equal(t, "go", s.Command)
			assert.Equal(t, []string{"test", "-race", "-short", "./..."}, s.Args)
			assert.Equal(t, 10*time.Minute, s.Timeout)
			return
		}
	}
	t.Fatal("race step not found despite being in step names")
}

func TestLoadAnvilConfig_ReturnsNilWhenFileAbsent(t *testing.T) {
	dir := t.TempDir()

	cfg, err := LoadAnvilConfig(dir)

	assert.NoError(t, err)
	assert.Nil(t, cfg, "should return nil config when .forge/temper.yaml does not exist")
}

func TestLoadAnvilConfig_ParsesValidYAML(t *testing.T) {
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, "temper.yaml"), []byte("go_race_detection: true\n"), 0o644))

	cfg, err := LoadAnvilConfig(dir)

	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.GoRaceDetection)
	assert.True(t, *cfg.GoRaceDetection)
}

func TestLoadAnvilConfig_ReturnsErrorForCorruptYAML(t *testing.T) {
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, "temper.yaml"), []byte("{{not valid yaml"), 0o644))

	cfg, err := LoadAnvilConfig(dir)

	assert.Error(t, err, "corrupt YAML should return an error, not be silently swallowed")
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "failed to parse config")
}

func TestLoadAnvilConfig_ReturnsErrorForUnreadableFile(t *testing.T) {
	// Make temper.yaml a directory so os.ReadFile fails with a non-ENOENT
	// error on all platforms (including Windows where path-not-found is
	// treated as ENOENT by os.IsNotExist).
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, ".forge")
	require.NoError(t, os.MkdirAll(filepath.Join(forgeDir, "temper.yaml"), 0o755))

	cfg, err := LoadAnvilConfig(dir)

	assert.Error(t, err, "non-ENOENT read errors should be returned, not swallowed")
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "failed to read config")
}

func TestDetectSteps_NodeAtRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	steps := detectSteps(dir, nil, false)
	names := stepNames(steps)
	assert.Contains(t, names, "lint")
	assert.Contains(t, names, "test")
	// Root node steps should have empty Dir.
	for _, s := range steps {
		if s.Name == "lint" || s.Name == "test" {
			assert.Empty(t, s.Dir, "root node step should have empty Dir")
		}
	}
}

func TestDetectSteps_AppendsVitestWorkerCapWhenScriptUsesVitest(t *testing.T) {
	// Reproduces the Hytte OOM scenario: a Vite/Vitest frontend whose test:run
	// script invokes vitest. Temper must cap worker concurrency to 1 thread so
	// uncapped Vitest workers cannot push the host past its RAM limit.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
        "scripts": {
            "test:run": "vitest run",
            "lint": "eslint ."
        }
    }`), 0o644))

	steps := detectSteps(dir, nil, false)

	var testStep *Step
	for i := range steps {
		if steps[i].Name == "test" {
			testStep = &steps[i]
			break
		}
	}
	require.NotNil(t, testStep, "expected a test step for the Node project")

	assert.Equal(t, "npm", testStep.Command)
	assert.Contains(t, testStep.Args, "--",
		"vitest args must be passed after `--` so npm forwards them to the script")
	assert.Contains(t, testStep.Args, "--pool=threads")
	assert.Contains(t, testStep.Args, "--poolOptions.threads.maxThreads=1")
	assert.Contains(t, testStep.Args, "--poolOptions.threads.minThreads=1")

	// The leading "run test:run" prefix must still be intact so the existing
	// npm invocation continues to work.
	require.GreaterOrEqual(t, len(testStep.Args), 2)
	assert.Equal(t, "run", testStep.Args[0])
	assert.Equal(t, "test:run", testStep.Args[1])
}

func TestDetectSteps_DoesNotAppendVitestCapWhenScriptDoesNotUseVitest(t *testing.T) {
	// A non-Vitest test script (e.g. jest, mocha) must not get the Vitest-
	// specific flags appended — those flags would either be ignored or
	// reported as unknown options by other runners.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
        "scripts": {
            "test:run": "jest --ci"
        }
    }`), 0o644))

	steps := detectSteps(dir, nil, false)

	var testStep *Step
	for i := range steps {
		if steps[i].Name == "test" {
			testStep = &steps[i]
			break
		}
	}
	require.NotNil(t, testStep, "expected a test step for the Node project")

	assert.Equal(t, []string{"run", "test:run"}, testStep.Args,
		"non-vitest scripts must not have vitest cap flags appended")
}

func TestDetectSteps_DoesNotAppendVitestCapWhenScriptMissing(t *testing.T) {
	// An empty/missing scripts block must not get the Vitest cap — there is
	// no way to know what the script will do, so default to the previous
	// behaviour of just running `npm run test:run`.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))

	steps := detectSteps(dir, nil, false)

	var testStep *Step
	for i := range steps {
		if steps[i].Name == "test" {
			testStep = &steps[i]
			break
		}
	}
	require.NotNil(t, testStep)
	assert.Equal(t, []string{"run", "test:run"}, testStep.Args)
}

func TestConfigFromSteps_OverridePreservesUserCommandUnchanged(t *testing.T) {
	// Acceptance criterion from the bead: a per-anvil temper.steps override
	// must pass through verbatim — Forge must not silently inject the Vitest
	// worker cap into a user-supplied command.
	userArgs := []string{"run", "test:run"}
	cfg := ConfigFromSteps([]config.TemperStepConfig{
		{Name: "test", Command: "npm", Args: userArgs},
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 1)
	assert.Equal(t, "npm", cfg.Steps[0].Command)
	assert.Equal(t, userArgs, cfg.Steps[0].Args,
		"user-supplied Args must be preserved verbatim; auto-detect cap must not leak in")
}

func TestCommandInvokesVitest(t *testing.T) {
	cases := []struct {
		script string
		want   bool
	}{
		{"vitest", true},
		{"vitest run", true},
		{"vitest run --coverage", true},
		{"npx vitest run", true},
		{"node_modules/.bin/vitest run", true},
		{"./node_modules/.bin/vitest", true},
		{"vitest.cmd run", true},
		{"jest --ci", false},
		{"mocha --reporter spec", false},
		{"", false},
		{"echo vitestic", false}, // substring must not match
		{"echo not-vitest", false},
	}
	for _, c := range cases {
		got := commandInvokesVitest(c.script)
		assert.Equal(t, c.want, got, "commandInvokesVitest(%q)", c.script)
	}
}

func TestScriptUsesVitest_MissingPackageJSON(t *testing.T) {
	// No package.json at all — must safely return false rather than panicking
	// or returning an error to callers.
	dir := t.TempDir()
	assert.False(t, scriptUsesVitest(dir, "test:run"))
}

func TestScriptUsesVitest_CorruptPackageJSON(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{not valid json"), 0o644))
	assert.False(t, scriptUsesVitest(dir, "test:run"),
		"corrupt JSON must not crash detection; safe default is `not vitest`")
}

func TestDetectSteps_NodeInSubdirectory(t *testing.T) {
	dir := t.TempDir()
	// Go at root, Node in web/
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "web"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "web", "package.json"), []byte("{}"), 0o644))

	opts := &DetectOptions{DisableGolangciLint: true}
	steps := detectSteps(dir, opts, false)
	names := stepNames(steps)

	// Should have Go steps
	assert.Contains(t, names, "build")
	assert.Contains(t, names, "vet")
	assert.Contains(t, names, "test")

	// Should have prefixed Node steps with Dir set
	assert.Contains(t, names, "web:lint")
	assert.Contains(t, names, "web:test")
	for _, s := range steps {
		if s.Name == "web:lint" || s.Name == "web:test" {
			assert.Equal(t, "web", s.Dir)
		}
	}
}

func TestDetectSteps_MultipleNodeSubdirs(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"web", "client"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, sub), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, sub, "package.json"), []byte("{}"), 0o644))
	}

	steps := detectSteps(dir, nil, false)
	names := stepNames(steps)

	// Both subdirs detected, no root
	assert.NotContains(t, names, "lint")
	assert.Contains(t, names, "web:lint")
	assert.Contains(t, names, "web:test")
	assert.Contains(t, names, "client:lint")
	assert.Contains(t, names, "client:test")
}

func TestDetectSteps_NodeRootAndSubdir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "frontend"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "frontend", "package.json"), []byte("{}"), 0o644))

	steps := detectSteps(dir, nil, false)
	names := stepNames(steps)

	// Both root and subdirectory should be detected
	assert.Contains(t, names, "lint")
	assert.Contains(t, names, "frontend:lint")
}

func TestConfigFromCommands_AllCommands(t *testing.T) {
	cfg := ConfigFromCommands("make build", "make test", "make lint", false)
	require.NotNil(t, cfg)
	assert.Len(t, cfg.Steps, 3)

	names := stepNames(cfg.Steps)
	assert.Equal(t, []string{"build", "lint", "test"}, names)

	// Verify command splitting
	assert.Equal(t, "make", cfg.Steps[0].Command)
	assert.Equal(t, []string{"build"}, cfg.Steps[0].Args)
	assert.Equal(t, "make", cfg.Steps[1].Command)
	assert.Equal(t, []string{"lint"}, cfg.Steps[1].Args)
	assert.Equal(t, "make", cfg.Steps[2].Command)
	assert.Equal(t, []string{"test"}, cfg.Steps[2].Args)

	// Lint should be optional
	assert.False(t, cfg.Steps[0].Optional, "build should not be optional")
	assert.True(t, cfg.Steps[1].Optional, "lint should be optional")
	assert.False(t, cfg.Steps[2].Optional, "test should not be optional")
}

func TestConfigFromCommands_OnlyBuild(t *testing.T) {
	cfg := ConfigFromCommands("cargo build", "", "", false)
	require.NotNil(t, cfg)
	assert.Len(t, cfg.Steps, 1)
	assert.Equal(t, "build", cfg.Steps[0].Name)
	assert.Equal(t, "cargo", cfg.Steps[0].Command)
	assert.Equal(t, []string{"build"}, cfg.Steps[0].Args)
}

func TestConfigFromCommands_OnlyTest(t *testing.T) {
	cfg := ConfigFromCommands("", "pytest -v", "", false)
	require.NotNil(t, cfg)
	assert.Len(t, cfg.Steps, 1)
	assert.Equal(t, "test", cfg.Steps[0].Name)
	assert.Equal(t, "pytest", cfg.Steps[0].Command)
	assert.Equal(t, []string{"-v"}, cfg.Steps[0].Args)
}

func TestConfigFromCommands_AllEmpty(t *testing.T) {
	cfg := ConfigFromCommands("", "", "", false)
	assert.Nil(t, cfg, "all empty commands should return nil")
}

func TestConfigFromCommands_SingleWordCommand(t *testing.T) {
	cfg := ConfigFromCommands("", "pytest", "", false)
	require.NotNil(t, cfg)
	assert.Equal(t, "pytest", cfg.Steps[0].Command)
	assert.Empty(t, cfg.Steps[0].Args)
}

func TestConfigFromSteps_PassesVerifyCleanThrough(t *testing.T) {
	cfg := ConfigFromSteps([]config.TemperStepConfig{
		{
			Name:        "build",
			Command:     "npm",
			Args:        []string{"run", "build"},
			VerifyClean: []string{"web/dist", "web/static"},
		},
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 1)
	assert.Equal(t, []string{"web/dist", "web/static"}, cfg.Steps[0].VerifyClean,
		"VerifyClean should be carried from TemperStepConfig into the temper Step")
}

func TestDetectEmbeddedBundles_DetectsHearthLayout(t *testing.T) {
	dir := t.TempDir()
	// Hearth 2.0 layout: frontend source + committed dist with index.html.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "web", "frontend"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "web", "frontend", "package.json"), []byte("{}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "web", "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "web", "dist", "index.html"), []byte("<html></html>"), 0o644))

	bundles := detectEmbeddedBundles(dir)
	require.Len(t, bundles, 1)
	assert.Equal(t, "hearth", bundles[0].Name)
	assert.Equal(t, "internal/web/frontend", bundles[0].FrontendDir)
	assert.Equal(t, "internal/web/dist", bundles[0].DistDir)
}

func TestDetectEmbeddedBundles_RequiresBothPaths(t *testing.T) {
	// Only frontend exists, no dist — should not match.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "web", "frontend"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "web", "frontend", "package.json"), []byte("{}"), 0o644))

	assert.Empty(t, detectEmbeddedBundles(dir),
		"detection requires both a frontend package.json and a dist/index.html")

	// Only dist exists, no frontend — also should not match.
	dir2 := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir2, "internal", "web", "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "internal", "web", "dist", "index.html"), []byte(""), 0o644))

	assert.Empty(t, detectEmbeddedBundles(dir2))
}

func TestEmbeddedBundle_Steps_Shape(t *testing.T) {
	eb := embeddedBundle{Name: "hearth", FrontendDir: "internal/web/frontend", DistDir: "internal/web/dist"}
	steps := eb.Steps()
	require.Len(t, steps, 3, "expected conflict-marker scan + install + build steps")

	assert.Equal(t, "hearth-frontend-conflict-markers", steps[0].Name)
	assert.Empty(t, steps[0].Command, "scan-only step must have no Command")
	assert.Equal(t, []string{"internal/web/dist"}, steps[0].VerifyNoConflictMarkers)

	assert.Equal(t, "hearth-frontend-install", steps[1].Name)
	assert.Equal(t, "npm", steps[1].Command)
	assert.Equal(t, []string{"install", "--no-audit", "--no-fund"}, steps[1].Args)
	assert.Equal(t, "internal/web/frontend", steps[1].Dir)
	assert.Empty(t, steps[1].VerifyClean, "install step does not verify clean (it may touch package-lock or node_modules)")

	assert.Equal(t, "hearth-frontend-build", steps[2].Name)
	assert.Equal(t, "npm", steps[2].Command)
	assert.Equal(t, []string{"run", "build"}, steps[2].Args)
	assert.Equal(t, "internal/web/frontend", steps[2].Dir)
	assert.Equal(t, []string{"internal/web/dist"}, steps[2].VerifyClean,
		"build step must verify dist remains clean — that's the whole point of the check")

	// Install and build should share the same Paths filter so they only run
	// when frontend src or build config actually changed in the diff.
	assert.Equal(t, steps[1].Paths, steps[2].Paths)
	assert.NotEmpty(t, steps[1].Paths)
	assert.Contains(t, steps[1].Paths, "internal/web/frontend/src/**")
}

func TestDetectSteps_IncludesEmbeddedBundleForHearthLayout(t *testing.T) {
	dir := t.TempDir()
	// Go at root + Hearth 2.0 frontend pattern.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "web", "frontend"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "web", "frontend", "package.json"), []byte("{}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "web", "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "web", "dist", "index.html"), []byte("<html></html>"), 0o644))

	opts := &DetectOptions{DisableGolangciLint: true}
	steps := detectSteps(dir, opts, false)
	names := stepNames(steps)

	assert.Contains(t, names, "hearth-frontend-conflict-markers")
	assert.Contains(t, names, "hearth-frontend-install")
	assert.Contains(t, names, "hearth-frontend-build")
}

// gitAvailable returns true when a `git` binary is on PATH so tests that
// shell out to git can be skipped on minimal CI images.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// initGitRepo bootstraps a minimal git repo in dir with one initial commit so
// `git status --porcelain` and `git diff` have a HEAD to compare against.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"-c", "commit.gpgsign=false", "commit", "-m", "init", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Strip the git repo-location env vars so the parent test environment
		// doesn't leak (same strip set the production helpers use).
		cmd.Env = executil.CleanGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestRun_VerifyClean_FailsWhenStepDirtiesArtifact(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	// Commit a "dist" file so git tracks it.
	distDir := filepath.Join(dir, "dist")
	require.NoError(t, os.MkdirAll(distDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(distDir, "bundle.js"), []byte("v1"), 0o644))
	initGitRepo(t, dir)

	// Step "rebuild" simulates a build that overwrites the committed dist
	// output with different bytes — exactly the stale-bundle scenario.
	cmdName := "sh"
	args := []string{"-c", "echo v2 > dist/bundle.js"}
	if runtime.GOOS == "windows" {
		t.Skip("uses sh; skip on Windows")
	}

	cfg := Config{Steps: []Step{{
		Name:        "rebuild",
		Command:     cmdName,
		Args:        args,
		Timeout:     30 * time.Second,
		VerifyClean: []string{"dist"},
	}}}

	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	assert.False(t, res.Passed, "VerifyClean should convert success to failure when dist diverges")
	assert.Equal(t, "rebuild", res.FailedStep)
	assert.Contains(t, res.Steps[0].Output, "stale relative to the current source")
}

func TestRun_VerifyClean_PassesWhenStepLeavesArtifactClean(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	distDir := filepath.Join(dir, "dist")
	require.NoError(t, os.MkdirAll(distDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(distDir, "bundle.js"), []byte("v1"), 0o644))
	initGitRepo(t, dir)

	// "rebuild" is a no-op (true) — it leaves the worktree exactly as
	// committed, which is the same shape as a successful idempotent rebuild.
	cmdName := "true"
	if runtime.GOOS == "windows" {
		// Windows lacks `true`; use cmd's noop equivalent.
		cmdName = "cmd"
	}
	args := []string(nil)
	if runtime.GOOS == "windows" {
		args = []string{"/c", "exit", "0"}
	}

	cfg := Config{Steps: []Step{{
		Name:        "rebuild",
		Command:     cmdName,
		Args:        args,
		Timeout:     30 * time.Second,
		VerifyClean: []string{"dist"},
	}}}

	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	assert.True(t, res.Passed, "step should pass when committed artifacts match a fresh build")
	assert.Empty(t, res.FailedStep)
}

// readTemperLog returns the contents of the single temper-*.log file written to
// <dir>/.forge-logs, failing the test if zero or more than one is present.
func readTemperLog(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".forge-logs"))
	require.NoError(t, err)
	var matches []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "temper-") && strings.HasSuffix(e.Name(), ".log") {
			matches = append(matches, e.Name())
		}
	}
	require.Len(t, matches, 1, "expected exactly one temper-*.log file")
	data, err := os.ReadFile(filepath.Join(dir, ".forge-logs", matches[0]))
	require.NoError(t, err)
	return string(data)
}

func TestWriteTemperLog_ContainsFullPerStepOutput(t *testing.T) {
	dir := t.TempDir()
	// A large output that would be truncated in the ingot OutputSummary must be
	// preserved in full in the temper log.
	longOutput := strings.Repeat("build error line\n", 500)
	result := &Result{
		Steps: []StepResult{
			{Name: "build", Command: "go build ./...", ExitCode: 0, Output: "ok\n", Duration: time.Second, Passed: true},
			{Name: "test", Command: "go test ./...", ExitCode: 1, Output: longOutput, Duration: 3 * time.Second, Passed: false},
		},
		Passed:     false,
		FailedStep: "test",
		Duration:   4 * time.Second,
	}

	writeTemperLog(dir, result)

	logContent := readTemperLog(t, dir)
	assert.Contains(t, logContent, "Overall: FAIL (first failing step: test)")
	assert.Contains(t, logContent, "go build ./...")
	assert.Contains(t, logContent, "go test ./...")
	assert.Contains(t, logContent, "Exit:     1")
	// The FULL, untruncated output must be present.
	assert.Contains(t, logContent, longOutput)
	assert.GreaterOrEqual(t, len(logContent), len(longOutput))
}

// TestRun_WritesTemperLogToForgeLogs verifies the temper log lands in the
// worktree's .forge-logs directory (where preserveWorktreeLogs later finds it)
// as a side effect of a normal Run.
func TestRun_WritesTemperLogToForgeLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses echo semantics; skip on Windows")
	}
	dir := t.TempDir()
	cfg := Config{Steps: []Step{{
		Name:    "greet",
		Command: "echo",
		Args:    []string{"hello-from-temper"},
		Timeout: 30 * time.Second,
	}}}

	res := Run(context.Background(), dir, cfg, nil, "Forge-test", "test")
	require.NotNil(t, res)
	require.True(t, res.Passed)

	logContent := readTemperLog(t, dir)
	assert.Contains(t, logContent, "greet")
	assert.Contains(t, logContent, "hello-from-temper")
	assert.Contains(t, logContent, "PASS")
}

func TestConfigFromCommands_Timeouts(t *testing.T) {
	cfg := ConfigFromCommands("make build", "make test", "make lint", false)
	require.NotNil(t, cfg)

	for _, s := range cfg.Steps {
		switch s.Name {
		case "build":
			assert.Equal(t, 3*time.Minute, s.Timeout)
		case "lint":
			assert.Equal(t, 3*time.Minute, s.Timeout)
		case "test":
			assert.Equal(t, 5*time.Minute, s.Timeout)
		}
	}
}

func TestDefaultConfigWithRace_ExcludesRaceStepWhenDisabled(t *testing.T) {
	dir := goModDir(t)
	opts := &DetectOptions{DisableGolangciLint: true}

	cfg := DefaultConfigWithRace(dir, opts, false)

	assert.False(t, cfg.GoRaceDetection)
	names := stepNames(cfg.Steps)
	assert.NotContains(t, names, "race", "should not have 'race' step when GoRaceDetection is false")
	// Should still have the standard Go steps.
	assert.Contains(t, names, "build")
	assert.Contains(t, names, "vet")
	assert.Contains(t, names, "test")
}

func TestConfigFromCommands_LintRequired_True(t *testing.T) {
	cfg := ConfigFromCommands("", "", "make lint", true)
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 1)
	assert.Equal(t, "lint", cfg.Steps[0].Name)
	assert.False(t, cfg.Steps[0].Optional, "lint should not be optional when lintRequired is true")
}

func TestConfigFromCommands_LintRequired_False_PreservesDefault(t *testing.T) {
	cfg := ConfigFromCommands("", "", "make lint", false)
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 1)
	assert.Equal(t, "lint", cfg.Steps[0].Name)
	assert.True(t, cfg.Steps[0].Optional, "lint should be optional when lintRequired is false")
}

func TestConfigFromCommands_LintRequired_EmptyLint_NoStep(t *testing.T) {
	cfg := ConfigFromCommands("make build", "", "", true)
	require.NotNil(t, cfg)
	for _, s := range cfg.Steps {
		assert.NotEqual(t, "lint", s.Name, "no lint step should be added when lint command is empty")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestConfigFromSteps_BasicOrdering(t *testing.T) {
	steps := []config.TemperStepConfig{
		{Name: "install", Command: "npm", Args: []string{"ci"}},
		{Name: "lint", Command: "npm", Args: []string{"run", "lint"}},
		{Name: "test", Command: "npm", Args: []string{"run", "test:run"}},
	}
	cfg := ConfigFromSteps(steps)
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 3)
	assert.Equal(t, "install", cfg.Steps[0].Name)
	assert.Equal(t, "lint", cfg.Steps[1].Name)
	assert.Equal(t, "test", cfg.Steps[2].Name)
	assert.Equal(t, "npm", cfg.Steps[0].Command)
	assert.Equal(t, []string{"ci"}, cfg.Steps[0].Args)
}

func TestConfigFromSteps_RequiredDefault_True(t *testing.T) {
	steps := []config.TemperStepConfig{
		{Name: "build", Command: "make", Args: []string{"build"}},
	}
	cfg := ConfigFromSteps(steps)
	require.NotNil(t, cfg)
	assert.False(t, cfg.Steps[0].Optional, "step without required field should not be optional")
}

func TestConfigFromSteps_RequiredFalse_Optional(t *testing.T) {
	steps := []config.TemperStepConfig{
		{Name: "mypy", Command: "mypy", Args: []string{"src"}, Required: boolPtr(false)},
	}
	cfg := ConfigFromSteps(steps)
	require.NotNil(t, cfg)
	assert.True(t, cfg.Steps[0].Optional, "step with required: false should be optional")
}

func TestConfigFromSteps_RespectsDir(t *testing.T) {
	steps := []config.TemperStepConfig{
		{Name: "lint", Command: "npm", Args: []string{"run", "lint"}, Dir: "web"},
	}
	cfg := ConfigFromSteps(steps)
	require.NotNil(t, cfg)
	assert.Equal(t, "web", cfg.Steps[0].Dir)
}

func TestConfigFromSteps_RespectsTimeout(t *testing.T) {
	steps := []config.TemperStepConfig{
		{Name: "test", Command: "pytest", Timeout: 10 * time.Minute},
	}
	cfg := ConfigFromSteps(steps)
	require.NotNil(t, cfg)
	assert.Equal(t, 10*time.Minute, cfg.Steps[0].Timeout)
}

func TestConfigFromSteps_DefaultTimeout(t *testing.T) {
	steps := []config.TemperStepConfig{
		{Name: "build", Command: "make"},
	}
	cfg := ConfigFromSteps(steps)
	require.NotNil(t, cfg)
	assert.Equal(t, 5*time.Minute, cfg.Steps[0].Timeout)
}

func TestConfigFromSteps_EmptyList_ReturnsNil(t *testing.T) {
	cfg := ConfigFromSteps([]config.TemperStepConfig{})
	assert.Nil(t, cfg, "empty steps slice should return nil")

	cfg = ConfigFromSteps(nil)
	assert.Nil(t, cfg, "nil steps slice should return nil")
}

func TestConfigFromSteps_RequiredByDefault(t *testing.T) {
	// This test verifies that ConfigFromSteps marks steps as required by default.
	// It does not execute Temper's Run loop.
	cfg := ConfigFromSteps([]config.TemperStepConfig{
		{Name: "pass", Command: "true"},
		{Name: "fail", Command: "false"},
		{Name: "should-not-run", Command: "true"},
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 3)
	// All steps are required (default) — verify Optional is false.
	for _, s := range cfg.Steps {
		assert.False(t, s.Optional, "all steps should be required by default")
	}
}

func TestConfigFromSteps_PropagatesPaths(t *testing.T) {
	cfg := ConfigFromSteps([]config.TemperStepConfig{
		{Name: "lint", Command: "npm", Args: []string{"run", "lint"}, Paths: []string{"client/**"}},
		{Name: "build", Command: "dotnet", Args: []string{"build"}},
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 2)
	assert.Equal(t, []string{"client/**"}, cfg.Steps[0].Paths)
	assert.Nil(t, cfg.Steps[1].Paths)
}

func TestMatchesChangedFiles_EmptyPatterns_AlwaysMatches(t *testing.T) {
	assert.True(t, matchesChangedFiles(nil, []string{"foo.go"}))
	assert.True(t, matchesChangedFiles([]string{}, []string{"foo.go"}))
}

func TestMatchesChangedFiles_NoFiles_NoMatch(t *testing.T) {
	assert.False(t, matchesChangedFiles([]string{"**/*.go"}, nil))
	assert.False(t, matchesChangedFiles([]string{"**/*.go"}, []string{}))
}

func TestMatchesChangedFiles_DoublestarGlob(t *testing.T) {
	files := []string{"client/src/app.tsx", "client/package.json", "README.md"}

	assert.True(t, matchesChangedFiles([]string{"client/**"}, files))
	assert.False(t, matchesChangedFiles([]string{"api/**"}, files))
}

func TestMatchesChangedFiles_ExtensionGlob(t *testing.T) {
	files := []string{"internal/temper/temper.go", "internal/config/config.go"}

	assert.True(t, matchesChangedFiles([]string{"**/*.go"}, files))
	assert.False(t, matchesChangedFiles([]string{"**/*.cs"}, files))
}

func TestMatchesChangedFiles_MultiplePatterns(t *testing.T) {
	files := []string{"docs/README.md"}

	// Second pattern matches
	assert.True(t, matchesChangedFiles([]string{"api/**", "docs/**"}, files))
	// Neither matches
	assert.False(t, matchesChangedFiles([]string{"api/**", "client/**"}, files))
}

func TestMatchesChangedFiles_ExactFile(t *testing.T) {
	files := []string{"go.mod", "internal/temper/temper.go"}
	assert.True(t, matchesChangedFiles([]string{"go.mod"}, files))
	assert.False(t, matchesChangedFiles([]string{"go.sum"}, files))
}

func TestBuildSummary_SkippedStep(t *testing.T) {
	r := &Result{
		Steps: []StepResult{
			{Name: "client-lint", Passed: true, Skipped: true},
			{Name: "api-build", Passed: true, Duration: 2_000_000_000},
		},
		Passed: true,
	}
	summary := buildSummary(r)

	assert.Contains(t, summary, "[SKIP] client-lint")
	assert.Contains(t, summary, "[PASS] api-build")
	assert.Contains(t, summary, "All required checks passed")
}

func TestRun_SkipsStepWhenPathsDontMatch(t *testing.T) {
	cfg := Config{
		Steps: []Step{
			{Name: "always-run", Command: "echo", Args: []string{"hello"}, Timeout: 5 * time.Second},
			{Name: "client-only", Command: "echo", Args: []string{"client"}, Timeout: 5 * time.Second, Paths: []string{"client/**"}},
			{Name: "api-only", Command: "echo", Args: []string{"api"}, Timeout: 5 * time.Second, Paths: []string{"api/**"}},
		},
		ChangedFiles: []string{"api/main.go", "api/handler.go"},
	}

	result := Run(context.Background(), t.TempDir(), cfg, nil, "test-bead", "test-anvil")

	require.Len(t, result.Steps, 3)
	// "always-run" has no paths — runs normally
	assert.False(t, result.Steps[0].Skipped)
	assert.True(t, result.Steps[0].Passed)
	// "client-only" has paths that don't match — skipped
	assert.True(t, result.Steps[1].Skipped)
	assert.True(t, result.Steps[1].Passed)
	// "api-only" has paths that match — runs normally
	assert.False(t, result.Steps[2].Skipped)
	assert.True(t, result.Steps[2].Passed)

	assert.True(t, result.Passed)
}

func TestIsDestructiveNpmInstall_Matches(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		want    bool
	}{
		{"npm", []string{"ci"}, true},
		{"npm", []string{"clean-install"}, true},
		{"npm.cmd", []string{"ci"}, true},
		{"npm.exe", []string{"ci"}, true},
		{"/usr/bin/npm", []string{"ci"}, true},
		{"npm", []string{"install"}, false},
		{"npm", []string{"run", "build"}, false},
		{"npm", []string{}, false},
		{"node", []string{"ci"}, false},
		{"yarn", []string{"install"}, false},
		{"npx", []string{"ci"}, false},
	}
	for _, tt := range tests {
		step := Step{Command: tt.command, Args: tt.args}
		got := isDestructiveNpmInstall(step)
		assert.Equal(t, tt.want, got, "isDestructiveNpmInstall(%q, %v)", tt.command, tt.args)
	}
}

func TestIsNodeModulesLinked_RealDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755))

	assert.False(t, isNodeModulesLinked(dir), "real directory should not be detected as linked")
}

func TestIsNodeModulesLinked_Linked(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(target, "node_modules"), 0o755))
	createTestDirLink(t, filepath.Join(target, "node_modules"), filepath.Join(dir, "node_modules"))

	assert.True(t, isNodeModulesLinked(dir), "symlink/junction should be detected as linked")
}

func TestIsNodeModulesLinked_Missing(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, isNodeModulesLinked(dir), "missing node_modules should return false")
}

func TestResolveStepDir(t *testing.T) {
	assert.Equal(t, "/work", resolveStepDir("/work", ""))
	assert.Equal(t, filepath.Join("/work", "web"), resolveStepDir("/work", "web"))
	assert.Equal(t, "/absolute/path", resolveStepDir("/work", "/absolute/path"))
}

func TestRun_BlocksDestructiveNpmWithJunction(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(target, "node_modules"), 0o755))
	createTestDirLink(t, filepath.Join(target, "node_modules"), filepath.Join(dir, "node_modules"))

	cfg := Config{
		Steps: []Step{
			{Name: "install", Command: "npm", Args: []string{"ci"}, Timeout: 5 * time.Second},
			{Name: "build", Command: "echo", Args: []string{"ok"}, Timeout: 5 * time.Second},
		},
	}

	result := Run(context.Background(), dir, cfg, nil, "test-bead", "test-anvil")

	require.Len(t, result.Steps, 2)
	assert.True(t, result.Steps[0].Skipped, "npm ci should be skipped when node_modules is a symlink")
	assert.True(t, result.Steps[0].Passed, "skipped step should count as passed")
	assert.Contains(t, result.Steps[0].Output, "Blocked")
	assert.False(t, result.Steps[1].Skipped, "subsequent steps should still run")
	assert.True(t, result.Passed)
}

func TestRun_AllowsNpmCiWithRealNodeModules(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755))

	cfg := Config{
		Steps: []Step{
			{Name: "install", Command: "npm", Args: []string{"ci"}, Timeout: 5 * time.Second},
		},
	}

	result := Run(context.Background(), dir, cfg, nil, "test-bead", "test-anvil")

	require.Len(t, result.Steps, 1)
	assert.False(t, result.Steps[0].Skipped, "npm ci should not be skipped with real node_modules")
}

func TestRun_VerifyNoConflictMarkers_FailsWhenMarkerPresent(t *testing.T) {
	dir := t.TempDir()
	distDir := filepath.Join(dir, "internal", "web", "dist", "assets")
	require.NoError(t, os.MkdirAll(distDir, 0o755))
	// Reproduces the production incident on 2026-05-11: a rebase Smith
	// committed a JS bundle whose first line was the 8-char conflict marker
	// emitted by git for some recursive-merge strategies. The freshness
	// rebuild missed it because the Paths filter skipped npm run build.
	bundle := "<<<<<<<< HEAD:internal/web/dist/assets/index-BdlG5RPS.js\n" +
		"(function(){console.log('one');})();\n" +
		"========\n" +
		"(function(){console.log('two');})();\n" +
		">>>>>>>> main:internal/web/dist/assets/index-BdlG5RPS.js\n"
	require.NoError(t, os.WriteFile(filepath.Join(distDir, "index-BdlG5RPS.js"), []byte(bundle), 0o644))

	cfg := Config{Steps: []Step{{
		Name:                    "conflict-markers",
		VerifyNoConflictMarkers: []string{"internal/web/dist"},
	}}}

	res := Run(context.Background(), dir, cfg, nil, "Forge-x2o9-test", "test-anvil")
	require.NotNil(t, res)
	require.Len(t, res.Steps, 1)
	assert.False(t, res.Passed, "scan must fail when conflict markers are present")
	assert.Equal(t, "conflict-markers", res.FailedStep)
	output := res.Steps[0].Output
	assert.Contains(t, output, "internal/web/dist/assets/index-BdlG5RPS.js:1")
	assert.Contains(t, output, "internal/web/dist/assets/index-BdlG5RPS.js:3")
	assert.Contains(t, output, "internal/web/dist/assets/index-BdlG5RPS.js:5")
}

func TestRun_VerifyNoConflictMarkers_PassesOnCleanDist(t *testing.T) {
	dir := t.TempDir()
	distDir := filepath.Join(dir, "internal", "web", "dist", "assets")
	require.NoError(t, os.MkdirAll(distDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(distDir, "index.js"),
		[]byte("(function(){console.log('hello');})();\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "web", "dist", "index.html"),
		[]byte("<!doctype html><html></html>"), 0o644))

	cfg := Config{Steps: []Step{{
		Name:                    "conflict-markers",
		VerifyNoConflictMarkers: []string{"internal/web/dist"},
	}}}

	res := Run(context.Background(), dir, cfg, nil, "Forge-x2o9-test", "test-anvil")
	require.NotNil(t, res)
	assert.True(t, res.Passed, "scan must pass on a clean dist")
	require.Len(t, res.Steps, 1)
	assert.True(t, res.Steps[0].Passed)
}

func TestRun_VerifyNoConflictMarkers_RunsRegardlessOfPathsFilter(t *testing.T) {
	// Critical regression guard: the scan must catch markers in committed
	// dist even when no source change matches the surrounding Paths filter.
	// In the production incident the freshness rebuild was skipped because
	// the rebase Smith only touched files under the dist directory itself —
	// none of the FrontendDir/src/** paths matched, so install + build
	// (and their VerifyClean check) silently skipped, and the markers shipped.
	dir := t.TempDir()
	distDir := filepath.Join(dir, "internal", "web", "dist", "assets")
	require.NoError(t, os.MkdirAll(distDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(distDir, "index.js"),
		[]byte("<<<<<<< HEAD\nalert(1);\n=======\nalert(2);\n>>>>>>> other\n"), 0o644))

	cfg := Config{
		Steps: []Step{{
			Name: "conflict-markers",
			// Path filter that would normally skip a code-driven step
			// (no source files match this glob). The scan-only branch
			// must ignore Paths and run anyway.
			Paths:                   []string{"internal/web/frontend/src/**"},
			VerifyNoConflictMarkers: []string{"internal/web/dist"},
		}},
		// Mimic the production diff: only dist/ assets changed, no source.
		ChangedFiles: []string{"internal/web/dist/assets/index.js"},
	}

	res := Run(context.Background(), dir, cfg, nil, "Forge-x2o9-test", "test-anvil")
	require.NotNil(t, res)
	require.Len(t, res.Steps, 1)
	assert.False(t, res.Steps[0].Skipped, "scan-only step must not be gated by Paths filter")
	assert.False(t, res.Passed, "markers in dist must fail the run even when no source files changed")
	assert.Equal(t, "conflict-markers", res.FailedStep)
}

func TestRun_VerifyNoConflictMarkers_HandlesMissingPath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Steps: []Step{{
		Name:                    "conflict-markers",
		VerifyNoConflictMarkers: []string{"internal/web/dist"},
	}}}
	res := Run(context.Background(), dir, cfg, nil, "Forge-x2o9-test", "test-anvil")
	require.NotNil(t, res)
	require.Len(t, res.Steps, 1)
	assert.True(t, res.Steps[0].Passed, "absent pathspec must be treated as nothing-to-scan, not an error")
}

func TestScanFileForConflictMarkers_HandlesLongMinifiedLine(t *testing.T) {
	// Realistic minified JS bundles routinely exceed the bufio default
	// 64 KiB line limit. The bumped buffer must accommodate them so the
	// scan doesn't error out on perfectly clean files.
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.js")
	// 256 KiB single line — well over the default 64 KiB, well under the
	// 8 MiB bumped ceiling.
	long := strings.Repeat("a", 256*1024)
	require.NoError(t, os.WriteFile(path, []byte("<<<<<<< HEAD\n"+long+"\n"), 0o644))

	hits, err := scanFileForConflictMarkers(path, dir)
	require.NoError(t, err)
	require.Len(t, hits, 1, "marker on line 1 must be reported; the long line on line 2 must not error")
	assert.Equal(t, "bundle.js:1", hits[0])
}

func TestLineHasConflictMarkerPrefix(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"<<<<<<< HEAD", true},
		{"<<<<<<<< HEAD:foo.js", true},
		{"=======", true},
		{"======== more", true},
		{">>>>>>> main", true},
		{">>>>>>>> main:foo.js", true},
		// Below the 7-char threshold.
		{"<<<<<< short", false},
		{"====== short", false},
		{">>>>>> short", false},
		// Marker characters not at the start of the line.
		{" <<<<<<< HEAD", false},
		{"// =======", false},
		// Legitimate code with marker chars but not in a run.
		{"const a = 1; const b = 2;", false},
		{"a >>> 3", false},
		{"", false},
	}
	for _, c := range cases {
		got := lineHasConflictMarkerPrefix([]byte(c.line))
		assert.Equal(t, c.want, got, "line=%q", c.line)
	}
}

func TestEmbeddedBundle_Steps_IncludesUnconditionalConflictMarkerScan(t *testing.T) {
	eb := embeddedBundle{Name: "hearth", FrontendDir: "internal/web/frontend", DistDir: "internal/web/dist"}
	steps := eb.Steps()
	require.Len(t, steps, 3, "expected conflict-marker + install + build steps")

	assert.Equal(t, "hearth-frontend-conflict-markers", steps[0].Name)
	assert.Empty(t, steps[0].Command, "scan-only step must have no Command")
	assert.Equal(t, []string{"internal/web/dist"}, steps[0].VerifyNoConflictMarkers)
	assert.Empty(t, steps[0].Paths,
		"conflict-marker scan must NOT have a Paths filter — it must run even when no source change touches the dist rebuild paths")
}

func TestConfigFromSteps_PassesVerifyNoConflictMarkersThrough(t *testing.T) {
	cfg := ConfigFromSteps([]config.TemperStepConfig{
		{
			Name:                    "conflict-markers",
			VerifyNoConflictMarkers: []string{"internal/web/dist"},
		},
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.Steps, 1)
	assert.Equal(t, []string{"internal/web/dist"}, cfg.Steps[0].VerifyNoConflictMarkers)
}

func TestRun_NilChangedFiles_NeverSkips(t *testing.T) {
	cfg := Config{
		Steps: []Step{
			{Name: "with-paths", Command: "echo", Args: []string{"ok"}, Timeout: 5 * time.Second, Paths: []string{"nonexistent/**"}},
		},
		ChangedFiles: nil, // unknown — should not skip
	}

	result := Run(context.Background(), t.TempDir(), cfg, nil, "test-bead", "test-anvil")

	require.Len(t, result.Steps, 1)
	assert.False(t, result.Steps[0].Skipped, "step should not be skipped when ChangedFiles is nil")
	assert.True(t, result.Steps[0].Passed)
}
