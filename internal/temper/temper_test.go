package temper

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	// Create output longer than maxStepOutputLen
	longOutput := strings.Repeat("x", maxStepOutputLen+500)
	r := &Result{
		Steps: []StepResult{
			{Name: "test", Passed: false, Duration: 1_000_000_000, Output: longOutput},
		},
		FailedStep: "test",
	}
	summary := buildSummary(r)

	assert.Contains(t, summary, "...(truncated)")
	// The tail of the output should be preserved (most relevant errors are at the end)
	assert.Contains(t, summary, strings.Repeat("x", 100))
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
