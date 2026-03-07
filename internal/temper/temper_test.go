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
