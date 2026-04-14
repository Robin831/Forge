package temper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
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
