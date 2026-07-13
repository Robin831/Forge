package pipeline

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTemper_InfraFailure_RetriedOnceThenEscalates verifies that a Temper
// failure classified as infra is re-run once WITHOUT invoking Smith and, when
// it persists, escalates to needs-attention with the classification in the
// error — rather than looping Smith on a phantom failure.
func TestTemper_InfraFailure_RetriedOnceThenEscalates(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	var smithCalls, temperCalls int32
	origSmith := params.SmithRunner
	params.SmithRunner = func(ctx context.Context, wt, promptText, logDir string, pv provider.Provider, flags []string) (*smith.Process, error) {
		atomic.AddInt32(&smithCalls, 1)
		return origSmith(ctx, wt, promptText, logDir, pv, flags)
	}
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		atomic.AddInt32(&temperCalls, 1)
		return &temper.Result{
			Passed:         false,
			FailedStep:     "test",
			Classification: temper.ClassificationInfra,
			Summary:        "infra failure: signal: killed",
		}
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.NeedsHuman, "persistent infra failure must escalate to needs_human")
	assert.False(t, outcome.Success)
	require.Error(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "infra", "escalation error must carry the classification")
	assert.Equal(t, int32(1), atomic.LoadInt32(&smithCalls), "Smith must run exactly once — never looped on infra failure")
	assert.Equal(t, int32(2), atomic.LoadInt32(&temperCalls), "Temper must run twice (initial + one retry without Smith)")
}

// TestTemper_TimeoutFailure_RecoversOnRetry verifies that a Temper failure
// classified as timeout is retried once without Smith and, when the retry
// passes, the pipeline proceeds to Warden and succeeds.
func TestTemper_TimeoutFailure_RecoversOnRetry(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	var smithCalls, temperCalls int32
	origSmith := params.SmithRunner
	params.SmithRunner = func(ctx context.Context, wt, promptText, logDir string, pv provider.Provider, flags []string) (*smith.Process, error) {
		atomic.AddInt32(&smithCalls, 1)
		return origSmith(ctx, wt, promptText, logDir, pv, flags)
	}
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		if atomic.AddInt32(&temperCalls, 1) == 1 {
			return &temper.Result{
				Passed:         false,
				FailedStep:     "test",
				Classification: temper.ClassificationTimeout,
				Summary:        "step exceeded its deadline",
			}
		}
		return &temper.Result{Passed: true}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "ok"}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success, "a timeout that clears on retry should let the pipeline succeed")
	assert.False(t, outcome.NeedsHuman)
	assert.Equal(t, int32(1), atomic.LoadInt32(&smithCalls), "Smith must run exactly once — the retry does not re-invoke Smith")
	assert.Equal(t, int32(2), atomic.LoadInt32(&temperCalls), "Temper must run twice (initial timeout + passing retry)")
}

// TestTemper_CancellationInfraFailure_AbortsWithoutEscalation verifies that when
// the pipeline context is cancelled (daemon shutdown / IPC interrupt) the
// resulting SIGKILL — which Temper classifies as infra — does NOT trigger a
// retry-without-smith or escalate the bead to needs_human. The pipeline must
// abort cleanly with the context error so the bead can be retried after restart.
func TestTemper_CancellationInfraFailure_AbortsWithoutEscalation(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var temperCalls int32
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		atomic.AddInt32(&temperCalls, 1)
		// Simulate the daemon shutting down while the step was running: the
		// context is cancelled and the step is killed by SIGKILL, which Temper
		// reports as an infra failure.
		cancel()
		return &temper.Result{
			Passed:         false,
			FailedStep:     "test",
			Classification: temper.ClassificationInfra,
			Summary:        "infra failure: signal: killed",
		}
	}

	outcome := Run(ctx, params)

	assert.False(t, outcome.NeedsHuman, "a cancellation-induced infra failure must NOT escalate to needs_human")
	assert.False(t, outcome.Success)
	require.Error(t, outcome.Error)
	assert.ErrorIs(t, outcome.Error, context.Canceled, "aborted pipeline must surface the context cancellation")
	assert.Equal(t, int32(1), atomic.LoadInt32(&temperCalls), "Temper must NOT be retried once the context is cancelled")
}
