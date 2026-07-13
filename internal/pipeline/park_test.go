package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeParkHandle is a test double for the ParkHandle contract. The pause and
// resume channels are driven directly by the test to simulate operator pause/
// resume requests.
type fakeParkHandle struct {
	pause  chan struct{}
	resume chan string
}

func (h *fakeParkHandle) PauseRequested() <-chan struct{} { return h.pause }
func (h *fakeParkHandle) ResumeRequested() <-chan string  { return h.resume }

// TestPause_ParkAndResume exercises the pause/park/resume mechanic end-to-end: a
// running Smith spawn is interrupted by a pause request, its captured session_id
// and iteration are recorded in a park record, the worker transitions to paused,
// the goroutine parks (does NOT exit) awaiting a resume, and no failure is
// marked. On resume with no explicit message, the parked session is respawned via
// `claude --resume <session_id>` with the default 'Continue with the task.'
// message and the pipeline continues from the parked iteration through
// Temper → Warden to success.
func TestPause_ParkAndResume(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	// The initial spawn stays "running" until interrupted. It has already
	// captured a session_id from the stream (as a real Claude spawn would).
	runningResult := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(runningResult), nil
	}

	// A pause is pending while the initial spawn runs; the resume channel stays
	// empty until the test observes the park and sends a resume.
	ph := &fakeParkHandle{
		pause:  make(chan struct{}, 1),
		resume: make(chan string, 1),
	}
	ph.pause <- struct{}{}
	params.ParkHandle = ph

	var interruptCalls int32
	params.SmithInterrupter = func(proc *smith.Process) {
		atomic.AddInt32(&interruptCalls, 1)
		proc.Interrupt(0) // unblock the running stub deterministically
	}

	// Record the resume invocation so we can assert the session and message.
	var mu sync.Mutex
	var gotSessionID, gotResumeMsg string
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, resumeMsg, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		mu.Lock()
		gotSessionID = sessionID
		gotResumeMsg = resumeMsg
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}

	// The resume iteration (iteration 2) produced a diff; Warden approves.
	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	// Run the pipeline in the background so the test can observe the park and
	// then release it via resume.
	done := make(chan *Outcome, 1)
	go func() { done <- Run(context.Background(), params) }()

	// The pipeline must park the worker in the paused status (proving both the
	// park record was constructed and the goroutine did not exit).
	require.Eventually(t, func() bool {
		w, err := db.GetWorker("test-worker")
		return err == nil && w.Status == state.WorkerPaused
	}, 2*time.Second, 5*time.Millisecond, "pipeline should park the worker in paused status")

	// While parked, the pipeline must NOT have returned an outcome.
	select {
	case <-done:
		t.Fatal("pipeline exited while it should be parked awaiting resume")
	default:
	}

	// A paused worker must never be marked failed.
	w, err := db.GetWorker("test-worker")
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "a parked worker must not be marked failed")

	// Resume with no explicit message → the default prompt is used.
	ph.resume <- ""

	var outcome *Outcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not resume within the deadline")
	}

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success, "pipeline should succeed after resume")
	assert.Equal(t, 2, outcome.Iterations, "the resume must continue as a second iteration")
	assert.EqualValues(t, 1, atomic.LoadInt32(&interruptCalls), "the running spawn must be interrupted exactly once")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the parked session must be resumed exactly once")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "sess-1", gotSessionID, "resume must reuse the session_id captured from the paused spawn")
	assert.Equal(t, DefaultResumeMessage, gotResumeMsg, "an empty resume message must default to DefaultResumeMessage")

	// The final worker status must not be failed (the interrupt/park path never
	// marks failure).
	w, err = db.GetWorker("test-worker")
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "pause/resume must not mark the worker failed")

	// A bead_paused and a bead_resumed event must each be recorded exactly once.
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	var pausedCount, resumedCount int
	for _, e := range events {
		switch e.Type {
		case state.EventBeadPaused:
			pausedCount++
		case state.EventBeadResumed:
			resumedCount++
		}
	}
	assert.Equal(t, 1, pausedCount, "exactly one bead_paused event must be recorded")
	assert.Equal(t, 1, resumedCount, "exactly one bead_resumed event must be recorded")
}

// TestPause_ResumeWithExplicitMessage verifies that a resume message supplied by
// the operator is delivered verbatim as the resume prompt (rather than the
// default).
func TestPause_ResumeWithExplicitMessage(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	runningResult := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(runningResult), nil
	}

	ph := &fakeParkHandle{
		pause:  make(chan struct{}, 1),
		resume: make(chan string, 1),
	}
	ph.pause <- struct{}{}
	params.ParkHandle = ph
	params.SmithInterrupter = func(proc *smith.Process) { proc.Interrupt(0) }

	var mu sync.Mutex
	var gotResumeMsg string
	params.SmithResumeRunner = func(_ context.Context, _, resumeMsg, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		mu.Lock()
		gotResumeMsg = resumeMsg
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-2", ResultSubtype: "success"}), nil
	}
	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	done := make(chan *Outcome, 1)
	go func() { done <- Run(context.Background(), params) }()

	require.Eventually(t, func() bool {
		w, err := db.GetWorker("test-worker")
		return err == nil && w.Status == state.WorkerPaused
	}, 2*time.Second, 5*time.Millisecond, "pipeline should park the worker")

	ph.resume <- "focus on the retry logic"

	var outcome *Outcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not resume within the deadline")
	}

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "focus on the retry logic", gotResumeMsg, "an explicit resume message must be delivered verbatim")
}

// TestPause_CancelWhileParkedDoesNotFail verifies that if the pipeline context is
// cancelled while the goroutine is parked (e.g. daemon shutdown), the pipeline
// exits without marking the worker failed and leaves it in the paused status.
func TestPause_CancelWhileParkedDoesNotFail(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	runningResult := &smith.Result{ExitCode: 0, SessionID: "sess-1", ResultSubtype: "success"}
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewRunningProcessForTest(runningResult), nil
	}

	ph := &fakeParkHandle{
		pause:  make(chan struct{}, 1),
		resume: make(chan string, 1),
	}
	ph.pause <- struct{}{}
	params.ParkHandle = ph
	params.SmithInterrupter = func(proc *smith.Process) { proc.Interrupt(0) }

	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Outcome, 1)
	go func() { done <- Run(ctx, params) }()

	require.Eventually(t, func() bool {
		w, err := db.GetWorker("test-worker")
		return err == nil && w.Status == state.WorkerPaused
	}, 2*time.Second, 5*time.Millisecond, "pipeline should park the worker")

	// Cancel while parked — the pipeline must exit without resuming.
	cancel()

	var outcome *Outcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not return after cancellation while parked")
	}

	require.Error(t, outcome.Error, "a cancellation while parked should surface an error")
	assert.EqualValues(t, 0, atomic.LoadInt32(&resumeCalls), "a cancelled park must not respawn")

	// The worker must remain paused, NOT failed.
	w, err := db.GetWorker("test-worker")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerPaused, w.Status, "a cancelled park must leave the worker paused, not failed")
}
