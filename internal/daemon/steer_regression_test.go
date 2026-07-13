package daemon

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// steerPipelineParams wires a pipeline.Params exactly the way dispatchBead does
// for the steering path: SteerCh, SpawnLive, and ParkHandle all come from the
// real control handle. This is the production wiring the earlier steer_ipc_test
// stubbed out (with a no-op setInterrupt), which is why the pipeline-cancelling
// bug shipped untested. Callers layer their own Smith/Warden stubs on top.
func steerPipelineParams(t *testing.T, d *Daemon, bead, workerID string, ctrl *controlHandle) pipeline.Params {
	t.Helper()
	return pipeline.Params{
		DB:            d.db,
		AnvilName:     "test-anvil",
		AnvilConfig:   config.AnvilConfig{Path: t.TempDir()},
		Bead:          poller.Bead{ID: bead, Title: "steer regression bead"},
		WorkerID:      workerID,
		PromptBuilder: prompt.NewBuilder(),
		WorktreeCreator: func(_ context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
			return &worktree.Worktree{
				BeadID:    beadID,
				AnvilPath: anvilPath,
				Path:      t.TempDir(),
				Branch:    "forge/" + beadID,
			}, nil
		},
		WorktreeRemover: func(_ context.Context, _ string, _ *worktree.Worktree) {},
		TemperRunner: func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
			return &temper.Result{Passed: true}
		},
		EmptyDiffChecker:  func(_, _ string) bool { return false },
		BeadReleaser:      func(_, _ string) error { return nil },
		SteerNoteAppender: func(_, _, _ string) error { return nil },
		Providers:         []provider.Provider{{Kind: provider.Claude}},

		// Production steering wiring under test.
		SteerCh:    ctrl.steer,
		SpawnLive:  ctrl.setLiveSpawn,
		ParkHandle: ctrl,
	}
}

// TestSteer_ProductionWiring_ModeA_ResumeSpawnBornLive is the core regression
// for Forge-bpd2. It drives the REAL pipeline loop with the REAL control-handle
// wiring and delivers a steer through the REAL handleSteerBead IPC handler while
// a Smith spawn is live (mode A). It asserts the previous defect is gone: the
// resume spawn is born with a NON-cancelled context (the old fireInterrupt →
// pipeline-ctx cancel made it "context canceled" and failed the bead), reuses
// the interrupted spawn's session_id, and the pipeline proceeds through
// Temper → Warden to completion without failing the worker.
func TestSteer_ProductionWiring_ModeA_ResumeSpawnBornLive(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-STEER-A"
	const workerID = "w-steer-a"

	ctrl := newControlHandle(workerID)
	d.registerControlHandle(bead, ctrl)

	// The initial spawn stays running (Done() open) until interrupted. It has
	// already captured a session_id from the stream, as a live Claude spawn would.
	running := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}

	var resumeCalls int32
	var resumeCtxErr error
	var resumeSession string

	params := steerPipelineParams(t, d, bead, workerID, ctrl)
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(running), nil
	}
	params.SmithInterrupter = func(proc *smith.Process) { proc.Interrupt(0) }
	params.SmithResumeRunner = func(ctx context.Context, _, _, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		// Capturing ctx.Err() here is the crux of the regression: the old wiring
		// cancelled this context via fireInterrupt, so the resume was born dead.
		resumeCtxErr = ctx.Err()
		resumeSession = sessionID
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	// Mirror dispatchBead: a cancellable, deadline-free pipeline context.
	pipelineCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	outcomeCh := make(chan *pipeline.Outcome, 1)
	go func() { outcomeCh <- pipeline.Run(pipelineCtx, params) }()

	// Once a spawn is live, deliver the steer through the real IPC handler.
	require.Eventually(t, ctrl.hasLiveSpawn, 2*time.Second, 2*time.Millisecond, "a spawn should go live")

	payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: bead, Message: "also update the README"})
	resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
	require.Equal(t, "ok", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "mode A", "a steer while a spawn is live must be labelled mode A")

	var outcome *pipeline.Outcome
	select {
	case outcome = <-outcomeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not complete after the steer")
	}

	require.NoError(t, outcome.Error, "steering must not error the pipeline")
	assert.True(t, outcome.Success, "pipeline must complete through temper/warden after the steer")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the steered session must be resumed exactly once")
	assert.NoError(t, resumeCtxErr, "the resume spawn must be born with a NON-cancelled context (regression)")
	assert.Equal(t, "sess-1", resumeSession, "resume must reuse the interrupted spawn's session_id")

	w, err := d.db.GetWorker(workerID)
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "steering must not fail the bead")
}

// TestSteer_ProductionWiring_ModeB_QueuesWithoutAbort exercises a steer that
// arrives while Temper/Warden runs — no spawn is live — through the real IPC
// handler and real pipeline wiring. It asserts the message is labelled mode B,
// the pipeline is NOT aborted, and the queued steer is applied on the next spawn
// (a resume born with a live context) so the bead completes rather than failing.
func TestSteer_ProductionWiring_ModeB_QueuesWithoutAbort(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-STEER-B"
	const workerID = "w-steer-b"

	ctrl := newControlHandle(workerID)
	d.registerControlHandle(bead, ctrl)

	var wardenCalls int32
	var resumeCalls int32
	var resumeCtxErr error
	var steerResp ipc.Response

	params := steerPipelineParams(t, d, bead, workerID, ctrl)
	// Iteration 1's fresh spawn completes normally and reports a session_id.
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}), nil
	}
	params.SmithResumeRunner = func(ctx context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		resumeCtxErr = ctx.Err()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		if atomic.AddInt32(&wardenCalls, 1) == 1 {
			// No spawn is live during Warden — delivering the steer now exercises
			// mode B through the real handler. (Runs on the pipeline goroutine,
			// which is the Run() caller here, so this is single-threaded.)
			assert.False(t, ctrl.hasLiveSpawn(), "no spawn should be live during Warden (mode B)")
			payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: bead, Message: "prioritise the caching layer"})
			steerResp = d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
			return &warden.ReviewResult{Verdict: warden.VerdictRequestChanges, Summary: "address the nil pointer"}, nil
		}
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	pipelineCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run synchronously: the steer is delivered from within the Warden stub on
	// this same goroutine, so no concurrency is needed.
	outcome := pipeline.Run(pipelineCtx, params)

	require.Equal(t, "ok", steerResp.Type, "the mode B steer must be accepted")
	assert.Contains(t, steerMsg(t, steerResp), "mode B", "a steer with no live spawn must be labelled mode B")

	require.NoError(t, outcome.Error, "a mode B steer must not abort the pipeline")
	assert.True(t, outcome.Success, "pipeline must complete after applying the queued steer")
	assert.Equal(t, 2, outcome.Iterations, "the queued steer must drive a second iteration")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the queued steer must be applied on the next spawn")
	assert.NoError(t, resumeCtxErr, "the mode B resume spawn must be born with a non-cancelled context")

	w, err := d.db.GetWorker(workerID)
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "a mode B steer must not fail the bead")
}
