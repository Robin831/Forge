package daemon

import (
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/worker"
)

// Wedge RECOVERY: the "recovery works" half of the epic.
//
// Each test here produces one way a worker row is left claiming work that is
// gone, and asserts the same observable pair on the other side of it: the row
// reaches a TERMINAL status, and the next poll DISPATCHES a bead that was
// waiting behind it. The second half is the one that matters — a row marked
// failed while the global slot stays spoken for is the same wedge under a
// different status — so every test runs the real dispatch gate
// (state.ActiveDispatchWorkers, through worker.DispatchActiveWorkers in
// pollAndDispatch) against max_total_smiths = 1 and reads the answer off the
// database, not off a counter the test keeps.
//
// The three take three different production paths on purpose, because "a
// wedged worker" is not one condition:
//
//   - a Smith process killed under a LIVE daemon is recovered by the stale
//     detector's liveness escalation (checkStaleWorkers -> presentsDeadPID ->
//     terminateDeadStaleWorker, staleliveness.go);
//   - a pipeline that ABORTS — Temper killed by an expired deadline is the
//     exit the backstop was written for — is recovered by the dispatch
//     goroutine's own exit defer (terminateAbandonedWorker, workerbackstop.go);
//   - a row left behind by a daemon that is no longer running is recovered by
//     the leaked-worker reaper (reapLeakedWorker/judgeLeak, reaper.go).
//
// The sibling paused-survival test is the "recovery does not overreach" half.
// The predicate the two must not contradict each other about is judgeLeak, and
// both read it rather than restating it: leakVerdictFor below runs the shipped
// function over a real state.db row, so this file asserting that it FIRES for a
// leaked row and DECLINES for a live-but-silent one, and the sibling asserting
// that it declines for a paused one, are three answers from one decision table.

const (
	// wedgedBead is the bead whose worker row is the wedge under test.
	wedgedBead = "Forge-wedged"

	// waitingBead is what the stubbed `bd ready` offers on every poll. It is
	// never dispatched while the wedge holds the single global slot, and its
	// worker row appearing is the whole evidence that the slot came back.
	waitingBead = "Forge-waiting"

	// recoveryStaleInterval is the stale_interval the detector passes run at.
	// The seeded rows start an hour in the past with no log file, so they are
	// silent by any threshold; a minute is simply a value a real deployment
	// might hold, rather than one tuned to make the test pass.
	recoveryStaleInterval = time.Minute
)

// newRecoveryEnv is a harness env wired for the one question these tests ask:
// does a bead waiting behind a wedged worker get dispatched once the wedge is
// cleared.
//
// max_total_smiths is 1, so the wedged row holds the ONLY slot and a dispatch
// is unambiguous evidence that it was released — with a larger ceiling a poll
// would dispatch whether or not the wedge cleared. The anvil path is the env's
// own temp directory and `bd` is a stub on PATH, which is what makes
// pollAndDispatch reachable without a beads database; the dispatch it then
// makes aborts almost immediately (the directory is not a git checkout), and
// that is fine, because the assertion is that a dispatch HAPPENED — the pending
// worker row insertPendingWorker writes before the goroutine is launched.
func newRecoveryEnv(t *testing.T) *testEnv {
	t.Helper()

	e := newTestDaemon(t, withWorkerLimit(1))
	e.reconfigure(func(c *config.Config) {
		// A positive crucible poll interval is what makes a FAST poll legal:
		// pollAndDispatch promotes every poll to a full one below it, and a
		// full poll runs the anvil-health probe against a real dolt database
		// this test has no business reaching.
		c.Settings.CruciblePollInterval = time.Minute
		c.Settings.StaleInterval = recoveryStaleInterval
		c.Anvils = map[string]config.AnvilConfig{
			testAnvil: {Path: e.dir, MaxSmiths: 1, AutoDispatch: "all"},
		}
	})
	stubBdOnPath(t, e.dir,
		`[{"id": "`+waitingBead+`", "title": "waiting bead", "status": "ready", "priority": 1}]`)

	// Registered after the env's own cleanups, so it runs BEFORE them: a
	// dispatch goroutine still unwinding when the temp directory is removed
	// and the database closed is a test failing on its own teardown.
	t.Cleanup(func() { drainDispatch(t, e) })
	return e
}

// drainDispatch waits for the dispatch goroutines pollAndDispatch launched.
// Bounded and reported rather than left to hang, on the same argument as the
// harness's own stop.
func drainDispatch(t *testing.T, e *testEnv) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		e.d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Errorf("dispatch goroutines did not finish within 30s")
	}
}

// dispatched reports whether the bead has a worker row at all, which is what
// makes it the dispatch assertion: insertPendingWorker writes the row inside
// the poll loop, before the goroutine is launched and before anything the
// pipeline does can fail, so its presence says a dispatch was made and its
// absence says the gate refused one.
func dispatched(t *testing.T, e *testEnv, beadID string) bool {
	t.Helper()
	_, err := e.beadWorkerStatus(beadID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	default:
		t.Fatalf("reading the worker row for %s: %v", beadID, err)
		return false
	}
}

// slotHolders is the dispatch gate's own reading of who is occupying
// max_total_smiths — worker.DispatchActiveWorkers over state.db, the exact call
// pollAndDispatch makes before it dispatches anything.
func slotHolders(t *testing.T, e *testEnv) []state.Worker {
	t.Helper()
	holders, err := worker.DispatchActiveWorkers(e.db)
	require.NoError(t, err, "reading the dispatch gate's active workers")
	return holders
}

// leakVerdictFor runs the reaper's shipped predicate over a seeded row, so a
// test states what judgeLeak (reaper.go) says about that row rather than
// restating the rule in its own words.
//
// It reports whether the row was a CANDIDATE at all: state.LeakCandidates is
// the status half of the decision (WorkerBackstopState.NeedsTerminalBackstop as
// SQL, which is where 'paused' is excluded), so a row missing from the set is
// already exempt and judgeLeak never sees it.
func leakVerdictFor(t *testing.T, e *testEnv, workerID string) (leakVerdict, bool) {
	t.Helper()

	candidates, err := e.db.LeakCandidates()
	require.NoError(t, err, "reading leak candidates")

	live := make(map[string]struct{})
	for _, id := range e.d.liveWorkers.snapshot() {
		live[id] = struct{}{}
	}
	for _, c := range candidates {
		if c.ID == workerID {
			return judgeLeak(c, e.d.workerGeneration, live, workerHeartbeatGrace, time.Now()), true
		}
	}
	return leakVerdict{}, false
}

// eventLogged reports whether an event of the given type was recorded for a
// bead — the operator-facing half of a recovery, and the half a status column
// cannot carry.
func eventLogged(t *testing.T, e *testEnv, kind state.EventType, beadID string) bool {
	t.Helper()
	events, err := e.db.RecentEvents(200)
	require.NoError(t, err)
	for _, ev := range events {
		if ev.Type == kind && ev.BeadID == beadID {
			return true
		}
	}
	return false
}

// TestKilledWorkerReachesTerminalStatusAndDispatchResumes drives the condition
// an operator sees when a Smith session is killed under a running daemon: a
// real child process, SIGKILLed and reaped, with its worker row still recorded
// 'running' and still holding the only dispatch slot.
//
// The recovery is the stale detector's liveness escalation, and it takes TWO
// passes by design (staleliveness.go): the pipeline flips a row's phase to
// `smith` before the spawn that writes the new pid, so one sighting of a dead
// pid is also what a perfectly healthy worker looks like for the width of a
// process launch. The test therefore asserts the intermediate state as well —
// after the first pass the row is 'stalled' and the slot is STILL held, because
// state.ActiveDispatchWorkers counts 'stalled' — so a regression that failed
// the row on the first sighting would fail here rather than pass faster.
func TestKilledWorkerReachesTerminalStatusAndDispatchResumes(t *testing.T) {
	e := newRecoveryEnv(t)

	// A real process, so "the row outlived its process" is a fact about a pid
	// rather than about a number the test invented.
	pid, _ := e.spawnDummyWorker(t)
	require.True(t, processAlive(pid), "the dummy worker must be running before it is killed")

	e.seedBead(wedgedBead, "in_progress")
	row := e.seedWorkerRow(workerRow{
		BeadID: wedgedBead,
		Status: state.WorkerRunning,
		Phase:  "smith",
		PID:    pid,
		// An hour old with no log file: silent by any stale_interval, which is
		// what the detector reads. Nothing here depends on the age being an
		// hour rather than a minute.
		StartedAt: time.Now().Add(-time.Hour),
		// Of the RUNNING generation with a fresh heartbeat, which is the
		// honest description of a row whose daemon is still alive — and which
		// keeps the reaper out of this test entirely (asserted below), so what
		// recovers the row is the path this test is about.
		Generation: e.generation(),
		Heartbeat:  time.Now(),
	})

	verdict, candidate := leakVerdictFor(t, e, row.ID)
	require.True(t, candidate, "a running row must reach the reaper's predicate at all")
	require.False(t, verdict.leaked,
		"the reaper must not claim this row (its daemon is alive): %s", verdict.reason)

	// The wedge: one row, one slot, and the waiting bead refused.
	require.Len(t, slotHolders(t, e), 1, "the seeded row must hold the only dispatch slot")
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, waitingBead),
		"a bead must not be dispatched while the wedged row holds max_total_smiths")

	// The kill. killWorker reaps as well as signals, so the pid is genuinely
	// gone rather than a zombie that still answers a liveness probe.
	e.killWorker(pid)
	require.False(t, processAlive(pid), "the killed worker process must be gone")

	status, err := e.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	require.Equal(t, state.WorkerRunning, status,
		"nothing observes the kill on its own: the row is still claiming a Smith that no longer exists")

	// First pass: the sighting is recorded and the recoverable mask goes on.
	e.d.checkStaleWorkers(recoveryStaleInterval)

	status, err = e.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	assert.Equal(t, state.WorkerStalled, status,
		"the first pass must mask the row, not end it — this is the tick a healthy pre-spawn window lands on")
	assert.Len(t, slotHolders(t, e), 1, "'stalled' still occupies a dispatch slot")
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, waitingBead),
		"a stalled row must keep holding the slot: the two-pass rule delays recovery, it does not skip it")

	// Second pass: same row, still silent, still naming the same gone process.
	e.d.checkStaleWorkers(recoveryStaleInterval)

	status, err = e.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	require.Equal(t, state.WorkerFailed, status,
		"a stalled row whose process is confirmed gone must reach a terminal status")
	assert.True(t, eventLogged(t, e, state.EventWorkerProcessGone, wedgedBead),
		"the recovery must be announced in the activity feed, not only in the status column")
	assert.True(t, e.hasLog(slog.LevelWarn, "marking worker as failed — its process is gone"))

	// The half that matters: the slot came back and the queue moved.
	assert.Empty(t, slotHolders(t, e), "a terminal row must stop occupying a dispatch slot")
	e.d.pollAndDispatch(e.ctx, false)
	assert.True(t, dispatched(t, e, waitingBead),
		"the bead waiting behind the killed worker must be dispatched once its slot is released")
}

// TestTemperDeadlineAbortReleasesSlot covers the exit the dispatch-goroutine
// backstop was written for: a Temper step killed by its own expired deadline,
// leaving the pipeline to unwind through a return nobody wrote a status update
// for (see workerbackstop.go, which names this case).
//
// The deadline is REAL — a step whose command outlives its timeout, killed by
// temper.Run — rather than a premise the test asserts, because the load-bearing
// claim is what Temper does NOT do: it kills the step, classifies the failure
// and returns, and the worker row is still 'running' afterwards. That row is
// the wedge, and terminateAbandonedWorker is what ends it.
//
// The row carries no pid, which is not a shortcut: workers.pid names the last
// process the row's PHASE spawned, so at phase `temper` it is the Smith that
// finished on purpose. This recovery cannot rest on pid evidence and the test
// is arranged so that it cannot accidentally appear to.
func TestTemperDeadlineAbortReleasesSlot(t *testing.T) {
	e := newRecoveryEnv(t)

	e.seedBead(wedgedBead, "in_progress")
	row := e.seedWorkerRow(workerRow{
		BeadID:     wedgedBead,
		Status:     state.WorkerRunning,
		Phase:      "temper",
		PID:        0,
		StartedAt:  time.Now().Add(-time.Minute),
		Generation: e.generation(),
		Heartbeat:  time.Now(),
	})

	require.Len(t, slotHolders(t, e), 1, "the seeded row must hold the only dispatch slot")
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, waitingBead),
		"a bead must not be dispatched while the verifying worker holds max_total_smiths")

	// A verification step that outruns its deadline. The command is this test
	// binary re-executed as the sleeping helper (the harness's own dummy
	// worker), which is a real process group Temper has to kill rather than a
	// command that happens to be slow.
	t.Setenv(helperProcessEnv, "1")
	result := temper.Run(e.ctx, e.dir, temper.Config{
		Steps: []temper.Step{{
			Name:    "test",
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestHelperSleep$"},
			Timeout: 150 * time.Millisecond,
		}},
	}, e.db, wedgedBead, testAnvil)

	require.NotNil(t, result)
	require.Len(t, result.Steps, 1)
	require.False(t, result.Passed, "a step killed on its deadline must not report a verdict")
	assert.True(t, result.Steps[0].Terminated,
		"the step must be recorded as killed by Forge rather than as a command that exited")
	assert.Equal(t, temper.ClassificationTimeout, result.Steps[0].Classification)

	// The wedge itself: Temper ends the step and says nothing about the worker
	// row, so the abort leaves it claiming a pipeline that is no longer running.
	status, err := e.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	require.Equal(t, state.WorkerRunning, status,
		"Temper must not be what ends the row — if it did, the backstop below would be asserting nothing")
	require.Len(t, slotHolders(t, e), 1)
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, waitingBead),
		"the aborted pipeline's row still holds the slot until its dispatch exits")

	// The dispatch goroutine returning. This is the call dispatchBead defers
	// above every one of its returns (`defer d.terminateAbandonedWorker(...)`,
	// daemon.go), invoked here because reaching it through a real pipeline run
	// would need a git worktree and a provider session, neither of which a
	// package test may have.
	e.d.terminateAbandonedWorker(row.ID, wedgedBead, testAnvil)

	status, err = e.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	require.Equal(t, state.WorkerFailed, status,
		"a dispatch that exited without finalising its row must leave it terminal")
	assert.True(t, eventLogged(t, e, state.EventWorkerAbandoned, wedgedBead))
	assert.True(t, e.hasLog(slog.LevelWarn, "dispatch exited without finalising its worker row"))

	assert.Empty(t, slotHolders(t, e), "a terminal row must stop occupying a dispatch slot")
	e.d.pollAndDispatch(e.ctx, false)
	assert.True(t, dispatched(t, e, waitingBead),
		"the bead waiting behind the aborted verification must be dispatched once its slot is released")
}

// TestReaperClearsLeakedNonTerminalRowOnStartup is the crash/restart case: the
// row is planted BEFORE the daemon's loops exist, carrying a generation this
// lifetime never issued, a heartbeat nothing has refreshed and a pid whose
// process is gone — which is what a SIGKILLed, OOM-killed or rebooted daemon
// leaves behind and what no defer inside a running daemon can cover.
//
// The pid is dead here and dead for nothing: judgeLeak never reads it (see the
// top of reaper.go for why a pid answers this question in neither direction).
// It is killed so the fixture describes the real condition rather than a
// half of it.
func TestReaperClearsLeakedNonTerminalRowOnStartup(t *testing.T) {
	e := newRecoveryEnv(t)

	pid, _ := e.spawnDummyWorker(t)
	e.killWorker(pid)
	require.False(t, processAlive(pid))

	e.seedBead(wedgedBead, "in_progress")
	row := e.seedWorkerRow(workerRow{
		BeadID:    wedgedBead,
		Status:    state.WorkerRunning,
		Phase:     "smith",
		PID:       pid,
		StartedAt: time.Now().Add(-time.Hour),
		// The two ownership columns, planted verbatim: a lifetime that ended,
		// and a heartbeat well past workerHeartbeatGrace. Either alone is the
		// condition; together they are what a restart actually leaves.
		Generation: "gen-a-lifetime-that-ended",
		Heartbeat:  time.Now().Add(-time.Hour),
	})

	// The daemon comes up over that database. Ownership loops first, as
	// Daemon.Run wires them, so the row is judged by a daemon that is running
	// rather than by a bare function call.
	e.start()

	verdict, candidate := leakVerdictFor(t, e, row.ID)
	require.True(t, candidate, "a running row must reach the reaper's predicate at all")
	require.True(t, verdict.leaked,
		"a row from a lifetime that ended, registered by nothing, must read as leaked")

	require.Len(t, slotHolders(t, e), 1, "the leaked row holds the only dispatch slot until it is reaped")
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, waitingBead),
		"a bead must not be dispatched while a leaked row holds max_total_smiths")

	// The startup pass — the one Daemon.Run makes before dispatch begins, so
	// the rows a previous lifetime left behind stop holding slots on the first
	// poll rather than ten minutes into it.
	require.NoError(t, e.d.reapLeakedWorkers(e.ctx))

	status, err := e.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	require.Equal(t, state.WorkerFailed, status,
		"a leaked non-terminal row must be ended by the startup pass")
	assert.True(t, eventLogged(t, e, state.EventWorkerLeaked, wedgedBead))
	assert.True(t, e.hasLog(slog.LevelWarn, "reaped a leaked worker row"))

	assert.Empty(t, slotHolders(t, e), "a terminal row must stop occupying a dispatch slot")
	e.d.pollAndDispatch(e.ctx, false)
	assert.True(t, dispatched(t, e, waitingBead),
		"the bead waiting behind the leaked row must be dispatched once its slot is released")
}
