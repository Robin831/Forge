package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIngot_HappyPath verifies that a successful pipeline run creates an ingot
// record and transitions it through init → smith → temper → warden → approved.
func TestIngot_HappyPath(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.WardenReviewer = approveWarden()

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Success)

	got, err := ingot.GetIngot(db.Conn(), "test-bead", "test-anvil")
	require.NoError(t, err)
	require.NotNil(t, got, "ingot record should exist after pipeline run")

	assert.Equal(t, "test-bead", got.BeadID)
	assert.Equal(t, "test-anvil", got.Anvil)
	assert.Equal(t, "Test bead", got.Title)
	assert.Equal(t, ingot.StatusApproved, got.Status)
	assert.NotEmpty(t, got.WorkerID)
	assert.NotEmpty(t, got.Branch)
}

// TestIngot_TemperResults verifies that temper step results are recorded as
// ingot test results.
func TestIngot_TemperResults(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		return &temper.Result{
			Passed:   true,
			Duration: 3 * time.Second,
			Steps: []temper.StepResult{
				{
					Name:     "build",
					Command:  "go build ./...",
					ExitCode: 0,
					Duration: 1 * time.Second,
					Passed:   true,
					Output:   "ok",
				},
				{
					Name:     "test",
					Command:  "go test ./...",
					ExitCode: 0,
					Duration: 2 * time.Second,
					Passed:   true,
					Optional: false,
					Output:   "PASS\nok  github.com/example 2.0s",
				},
			},
		}
	}
	params.WardenReviewer = approveWarden()

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Success)

	got, err := ingot.GetIngot(db.Conn(), "test-bead", "test-anvil")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.True(t, got.TemperPassed)
	assert.Empty(t, got.TemperFailedStep)
	assert.Greater(t, got.TemperDurationMs, 0)

	// Test results should be eager-loaded by GetIngot.
	require.Len(t, got.TestResults, 2)
	assert.Equal(t, "build", got.TestResults[0].StepName)
	assert.Equal(t, "go build ./...", got.TestResults[0].Command)
	assert.True(t, got.TestResults[0].Passed)
	assert.Equal(t, "test", got.TestResults[1].StepName)
}

// TestIngot_FailedPipeline verifies that a pipeline failure marks the ingot
// as failed.
func TestIngot_FailedPipeline(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	// Smith fails with non-zero exit code.
	params.SmithRunner = immediateSmith(&smith.Result{ExitCode: 1, ErrorOutput: "crashed"})

	outcome := Run(context.Background(), params)
	require.NotNil(t, outcome.Error)

	got, err := ingot.GetIngot(db.Conn(), "test-bead", "test-anvil")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, ingot.StatusFailed, got.Status)
}

// TestIngot_WardenReject_MarksFailed verifies that a hard warden rejection
// marks the ingot as failed.
func TestIngot_WardenReject_MarksFailed(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{
			Verdict: warden.VerdictReject,
			Summary: "Terrible code",
		}, nil
	}

	outcome := Run(context.Background(), params)
	assert.Equal(t, warden.VerdictReject, outcome.Verdict)

	got, err := ingot.GetIngot(db.Conn(), "test-bead", "test-anvil")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, ingot.StatusFailed, got.Status)
}

// TestIngot_NilDB_DoesNotPanic verifies that the pipeline runs without error
// even when DB is nil (ingot writes are silently skipped).
func TestIngot_NilDB_DoesNotPanic(t *testing.T) {
	params := Params{
		DB:        nil,
		AnvilName: "test-anvil",
		AnvilConfig: config.AnvilConfig{
			Path: t.TempDir(),
		},
		Bead: poller.Bead{
			ID:    "nil-db-bead",
			Title: "Nil DB test",
		},
		PromptBuilder:   prompt.NewBuilder(),
		WorktreeCreator: fakeWorktreeCreator(t),
		WorktreeRemover: noopRemover,
		SmithRunner:     immediateSmith(&smith.Result{ExitCode: 0}),
		TemperRunner:    passingTemper(),
		WardenReviewer:  approveWarden(),
		BeadReleaser:    func(_, _ string) error { return nil },
		Providers:       []provider.Provider{{Kind: provider.Claude}},
	}

	// Must not panic.
	outcome := Run(context.Background(), params)
	assert.True(t, outcome.Success)
}

// TestIngot_OutputTruncation verifies that long temper output is truncated to
// approximately 1000 characters in the ingot test result.
func TestIngot_OutputTruncation(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	longOutput := make([]byte, 2000)
	for i := range longOutput {
		longOutput[i] = 'x'
		if i > 0 && i%80 == 0 {
			longOutput[i] = '\n'
		}
	}

	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		return &temper.Result{
			Passed:   true,
			Duration: time.Second,
			Steps: []temper.StepResult{
				{
					Name:     "test",
					Command:  "go test ./...",
					ExitCode: 0,
					Duration: time.Second,
					Passed:   true,
					Output:   string(longOutput),
				},
			},
		}
	}
	params.WardenReviewer = approveWarden()

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Success)

	got, err := ingot.GetIngot(db.Conn(), "test-bead", "test-anvil")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.TestResults, 1)

	assert.LessOrEqual(t, len(got.TestResults[0].OutputSummary), 1000,
		"output should be truncated to ~1000 chars")
}

// TestTruncateOutput verifies the truncateOutput helper.
func TestTruncateOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxBytes int
		wantMax  int
	}{
		{"short unchanged", "hello", 100, 5},
		{"exact limit", "hello", 5, 5},
		{"truncated at newline", "line1\nline2\nline3", 12, 12},
		{"no newline", "abcdefghijklmnop", 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateOutput(tt.input, tt.maxBytes)
			assert.LessOrEqual(t, len(got), tt.wantMax)
		})
	}
}

// approveWarden returns a WardenReviewer that always approves.
func approveWarden() func(context.Context, string, string, string, string, string, *state.DB, ...provider.Provider) (*warden.ReviewResult, error) {
	return func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}
}
