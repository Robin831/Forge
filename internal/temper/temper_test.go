package temper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
