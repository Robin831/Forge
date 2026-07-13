package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWaitSmithWithSteer_DeterministicOrderings covers the unambiguous race
// orderings for waitSmithWithSteer: normal completion with no signal, a steer
// interrupting a live spawn, and a pause interrupting a live spawn. In every
// case a value that was consumed from a channel must be propagated, and the
// process is interrupted only while it is still running.
func TestWaitSmithWithSteer_DeterministicOrderings(t *testing.T) {
	// interrupt closes the running stub's done channel so Wait() can return.
	interrupt := func(p *smith.Process) { p.Interrupt(0) }

	tests := []struct {
		name          string
		running       bool // spawn still running at select time
		queueSteer    string
		queuePause    bool
		wantSteer     string
		wantPaused    bool
		wantInterrupt bool
	}{
		{
			name:          "completion with no signal queued",
			running:       false,
			wantSteer:     "",
			wantPaused:    false,
			wantInterrupt: false,
		},
		{
			name:          "steer interrupts a live spawn (mode A)",
			running:       true,
			queueSteer:    "go left",
			wantSteer:     "go left",
			wantPaused:    false,
			wantInterrupt: true,
		},
		{
			name:          "pause interrupts a live spawn",
			running:       true,
			queuePause:    true,
			wantSteer:     "",
			wantPaused:    true,
			wantInterrupt: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}
			var proc *smith.Process
			if tt.running {
				proc = smith.NewRunningProcessForTest(result)
			} else {
				proc = smith.NewProcessForTest(result)
			}

			var steerCh chan string
			if tt.queueSteer != "" {
				steerCh = make(chan string, 1)
				steerCh <- tt.queueSteer
			}
			var pauseCh chan struct{}
			if tt.queuePause {
				pauseCh = make(chan struct{}, 1)
				pauseCh <- struct{}{}
			}

			var interruptCalls int32
			gotResult, gotSteer, gotPaused := waitSmithWithSteer(proc, steerCh, pauseCh, func(p *smith.Process) {
				atomic.AddInt32(&interruptCalls, 1)
				interrupt(p)
			})

			require.NotNil(t, gotResult)
			assert.Equal(t, tt.wantSteer, gotSteer)
			assert.Equal(t, tt.wantPaused, gotPaused)
			if tt.wantInterrupt {
				assert.EqualValues(t, 1, atomic.LoadInt32(&interruptCalls), "a live spawn must be interrupted exactly once")
			} else {
				assert.EqualValues(t, 0, atomic.LoadInt32(&interruptCalls), "a finished spawn must not be interrupted")
			}
		})
	}
}

// TestWaitSmithWithSteer_SteerNeverConsumedThenDropped exercises the
// signal-then-done vs done-then-signal race where the spawn is ALREADY complete
// and a steer is simultaneously available. Because select picks a ready case at
// random, this repeats many times and asserts the invariant that once a steer is
// received from the channel it is always returned — never consumed and silently
// discarded in favour of "prefer completion". (The pre-fix code drained the
// channel and returned an empty steer, losing the operator's course-correction.)
func TestWaitSmithWithSteer_SteerNeverConsumedThenDropped(t *testing.T) {
	const iterations = 500
	for i := 0; i < iterations; i++ {
		// A completed process: Done() is already closed, so the outer select may
		// fire on either proc.Done() or steerCh.
		proc := smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"})
		steerCh := make(chan string, 1)
		steerCh <- "go left"

		var interrupted bool
		_, steer, paused := waitSmithWithSteer(proc, steerCh, nil, func(*smith.Process) { interrupted = true })

		require.False(t, paused)
		if steer == "" {
			// Completion won the race and no steer was consumed — the message
			// must still be queued for the between-spawns (mode B) drain.
			require.Len(t, steerCh, 1, "an unconsumed steer must remain in the mailbox, never silently drained")
		} else {
			require.Equal(t, "go left", steer, "a consumed steer must be returned verbatim")
			require.Len(t, steerCh, 0)
			require.False(t, interrupted, "an already-finished spawn must not be interrupted")
		}
	}
}

// TestWaitSmithWithSteer_PauseNeverConsumedThenDropped is the pause counterpart:
// with the spawn already complete and a pause simultaneously available, a pause
// that is received must always be reported (paused=true) so the worker parks
// rather than silently completing while the operator was told the pause
// succeeded.
func TestWaitSmithWithSteer_PauseNeverConsumedThenDropped(t *testing.T) {
	const iterations = 500
	for i := 0; i < iterations; i++ {
		proc := smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"})
		pauseCh := make(chan struct{}, 1)
		pauseCh <- struct{}{}

		_, steer, paused := waitSmithWithSteer(proc, nil, pauseCh, func(*smith.Process) {})

		require.Empty(t, steer)
		if !paused {
			// Completion won and the pause was not consumed — it must stay
			// pending so a between-spawns drain can still honour it.
			require.Len(t, pauseCh, 1, "an unconsumed pause must remain pending, never silently drained")
		} else {
			require.Len(t, pauseCh, 0, "a consumed pause must be reported so the worker parks")
		}
	}
}

// TestPause_BetweenSpawns_ParksAtWardenApproval verifies bug #2: a pause enqueued
// while Warden runs — a window with no live spawn for waitSmithWithSteer to
// observe — must NOT be dropped when Warden approves and the bead would otherwise
// complete. The pipeline parks instead, and resuming continues the recorded
// session for a further iteration.
func TestPause_BetweenSpawns_ParksAtWardenApproval(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	// Iteration 1's spawn completes normally with a session_id; nothing is live
	// to interrupt while Temper/Warden run afterwards.
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}), nil
	}
	params.EmptyDiffChecker = func(_, _ string) bool { return false }

	ph := &fakeParkHandle{pause: make(chan struct{}, 1), resume: make(chan string, 1)}
	params.ParkHandle = ph

	var wardenCalls int32
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		if atomic.AddInt32(&wardenCalls, 1) == 1 {
			// Operator pauses while the first review is in flight, then Warden
			// approves. The acknowledged pause must survive the approval.
			ph.pause <- struct{}{}
		}
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	var resumeSession string
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		resumeSession = sessionID
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}

	done := make(chan *Outcome, 1)
	go func() { done <- Run(context.Background(), params) }()

	require.Eventually(t, func() bool {
		w, err := db.GetWorker("test-worker")
		return err == nil && w.Status == state.WorkerPaused
	}, 2*time.Second, 5*time.Millisecond, "an acknowledged between-spawns pause must park the bead at Warden approval instead of completing")

	select {
	case <-done:
		t.Fatal("pipeline completed despite a pending pause — the pause was silently dropped")
	default:
	}

	ph.resume <- ""
	var outcome *Outcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not resume within the deadline")
	}

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success, "pipeline should succeed after the parked pause resumes")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the parked session must be resumed exactly once")
	assert.Equal(t, "sess-1", resumeSession, "resume must reuse the session captured before the pause")

	w, err := db.GetWorker("test-worker")
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "a between-spawns pause must never mark the worker failed")
}

// TestPause_BetweenSpawns_DuringTemper_ParksAtApproval covers the same
// signal-loss class from the Temper window: a pause enqueued while Temper runs
// (no live spawn) is honoured at the following Warden approval rather than
// discarded at completion.
func TestPause_BetweenSpawns_DuringTemper_ParksAtApproval(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}), nil
	}
	params.EmptyDiffChecker = func(_, _ string) bool { return false }

	ph := &fakeParkHandle{pause: make(chan struct{}, 1), resume: make(chan string, 1)}
	params.ParkHandle = ph

	var temperCalls int32
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		if atomic.AddInt32(&temperCalls, 1) == 1 {
			ph.pause <- struct{}{}
		}
		return &temper.Result{Passed: true}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		assert.Equal(t, "sess-1", sessionID)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}

	done := make(chan *Outcome, 1)
	go func() { done <- Run(context.Background(), params) }()

	require.Eventually(t, func() bool {
		w, err := db.GetWorker("test-worker")
		return err == nil && w.Status == state.WorkerPaused
	}, 2*time.Second, 5*time.Millisecond, "a pause enqueued during Temper must park the bead, not be dropped at completion")

	ph.resume <- ""
	select {
	case outcome := <-done:
		require.NoError(t, outcome.Error)
		assert.True(t, outcome.Success)
		assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls))
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not resume within the deadline")
	}
}

// TestSteer_BetweenSpawns_AppliedAtWardenApproval verifies that a steer enqueued
// while the final Warden review runs — arriving just as the bead would complete
// — is applied on a further iteration (resuming the last session with the steer)
// rather than silently discarded, satisfying the "never dropped" contract for
// steers as well as pauses.
func TestSteer_BetweenSpawns_AppliedAtWardenApproval(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}), nil
	}
	params.EmptyDiffChecker = func(_, _ string) bool { return false }

	steer := make(chan string, 1)
	params.SteerCh = steer

	var wardenCalls int32
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		if atomic.AddInt32(&wardenCalls, 1) == 1 {
			// Steer arrives while the first review is in flight, then Warden
			// approves. The steer must apply on a further iteration.
			steer <- "also update the docs"
		}
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	var resumeMsg, resumeSession string
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, steerMsg, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		resumeMsg = steerMsg
		resumeSession = sessionID
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success)
	assert.Equal(t, 2, outcome.Iterations, "a steer pending at approval must apply on a further iteration, not be dropped")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the last session must be resumed exactly once")
	assert.Equal(t, "sess-1", resumeSession, "the steer must resume the last completed session")
	assert.Contains(t, resumeMsg, "also update the docs", "the resume prompt must carry the steer message")
}
