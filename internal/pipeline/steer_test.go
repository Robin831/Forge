package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSteer_InterruptAndResume exercises steer mode A end-to-end: a running
// Smith spawn is interrupted by a steer message, its captured session_id is
// preserved, the session is resumed with the steering message as the new
// prompt, the respawn counts as a second pipeline iteration, the worker is NOT
// marked failed, and the pipeline continues through Temper → Warden.
func TestSteer_InterruptAndResume(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	// The initial spawn stays "running" until interrupted. It has already
	// captured a session_id from the stream (as a real Claude spawn would).
	runningResult := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(runningResult), nil
	}

	// One buffered steer message, delivered while the initial spawn runs.
	steer := make(chan string, 1)
	steer <- "please also update the README"
	params.SteerCh = steer

	var interruptCalls int32
	params.SmithInterrupter = func(proc *smith.Process) {
		atomic.AddInt32(&interruptCalls, 1)
		proc.Interrupt(0) // unblock the running stub deterministically
	}

	// Record the resume invocation so we can assert the session and message.
	var mu sync.Mutex
	var gotSessionID, gotSteerMsg string
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, steerMsg, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		mu.Lock()
		gotSessionID = sessionID
		gotSteerMsg = steerMsg
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}

	// The resume iteration (iteration 2) produced a diff; Warden approves.
	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success, "pipeline should succeed after steer resume")
	assert.Equal(t, 2, outcome.Iterations, "the resume must count as a second pipeline iteration")
	assert.EqualValues(t, 1, atomic.LoadInt32(&interruptCalls), "running spawn must be interrupted exactly once")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the session must be resumed exactly once")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "sess-1", gotSessionID, "resume must reuse the session_id captured from the interrupted spawn")
	assert.Equal(t, "please also update the README", gotSteerMsg, "the steering message must be delivered as the resume prompt")

	// The interrupt must not have marked the worker failed.
	w, err := db.GetWorker(outcome.WorkerID)
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "steer interrupt must not mark the worker failed")
}

// TestSteer_MaxIterationsBlocksRespawn verifies that a steer interrupt on the
// final allowed iteration does not respawn and escalates for human attention.
func TestSteer_MaxIterationsBlocksRespawn(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.MaxIterations = 1

	runningResult := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(runningResult), nil
	}

	steer := make(chan string, 1)
	steer <- "steer once"
	params.SteerCh = steer
	params.SmithInterrupter = func(proc *smith.Process) { proc.Interrupt(0) }

	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.Success)
	assert.True(t, outcome.NeedsHuman, "steer at max iterations should escalate to needs_human")
	assert.EqualValues(t, 0, atomic.LoadInt32(&resumeCalls), "must not respawn when already at max iterations")
	assert.Equal(t, 1, outcome.Iterations)
	require.Error(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "max_pipeline_iterations")
}

// TestSteer_NoSessionID_Escalates verifies that a steer interrupt of a spawn
// with no captured session_id (e.g. a non-Claude provider) cannot resume and
// escalates instead of respawning.
func TestSteer_NoSessionID_Escalates(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	runningResult := &smith.Result{ExitCode: 0, SessionID: ""}
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(runningResult), nil
	}

	steer := make(chan string, 1)
	steer <- "steer me"
	params.SteerCh = steer
	params.SmithInterrupter = func(proc *smith.Process) { proc.Interrupt(0) }

	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.Success)
	assert.True(t, outcome.NeedsHuman)
	assert.EqualValues(t, 0, atomic.LoadInt32(&resumeCalls), "must not resume without a session_id")
	require.Error(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "session_id")
}
