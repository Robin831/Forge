package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

func backstopDaemon(t *testing.T) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func insertBackstopWorker(t *testing.T, d *Daemon, id string, status state.WorkerStatus) {
	t.Helper()
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        id,
		BeadID:    "Forge-52bp",
		Anvil:     "repo",
		Status:    status,
		Phase:     "smith",
		StartedAt: time.Now(),
	}))
}

func backstopStatus(t *testing.T, d *Daemon, id string) state.WorkerStatus {
	t.Helper()
	got, err := d.db.GetWorkerStatus(id)
	require.NoError(t, err)
	return got
}

// The backstop's whole job: a row still claiming live work when its dispatch
// exits is failed, and a row an ordinary path already finalised — or one
// deliberately handed off live to Bellows (monitoring/detached) or to a cold
// resume (paused) — is left exactly as it stands.
//
// The exempt half is what makes the defer safe to install unconditionally: the
// success path returns from finalizePipeline with the row on 'monitoring', so a
// backstop that read only "not terminal" would report every successful dispatch
// as a failure and take the PR off Bellows' radar.
func TestTerminateAbandonedWorker(t *testing.T) {
	cases := []struct {
		status     state.WorkerStatus
		wantFailed bool
	}{
		{state.WorkerPending, true},
		{state.WorkerRunning, true},
		{state.WorkerReviewing, true},
		{state.WorkerStalled, true},
		{state.WorkerDone, false},
		{state.WorkerFailed, false},
		{state.WorkerPartial, false},
		{state.WorkerTimeout, false},
		{state.WorkerKilled, false},
		{state.WorkerMonitoring, false},
		{state.WorkerDetached, false},
		{state.WorkerPaused, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			d := backstopDaemon(t)
			insertBackstopWorker(t, d, "w-1", tc.status)

			d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

			want := tc.status
			if tc.wantFailed {
				want = state.WorkerFailed
			}
			assert.Equal(t, want, backstopStatus(t, d, "w-1"))
		})
	}
}

// A backstop that fires is a gap in the finalisation, not an outcome of the
// work, so it says so in the activity feed under its own event type — naming
// the status it replaced, which is the only clue to which exit path was missing
// its update.
func TestTerminateAbandonedWorkerRecordsTheGap(t *testing.T) {
	d := backstopDaemon(t)
	insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

	d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

	events, err := d.db.RecentEvents(20)
	require.NoError(t, err)
	var found *state.Event
	for i := range events {
		if events[i].Type == state.EventWorkerAbandoned {
			found = &events[i]
			break
		}
	}
	require.NotNil(t, found, "no worker_abandoned event was recorded")
	assert.Equal(t, "Forge-52bp", found.BeadID)
	assert.Contains(t, found.Message, "w-1")
	assert.Contains(t, found.Message, string(state.WorkerRunning))

	// A row that was already terminal is not news.
	d2 := backstopDaemon(t)
	insertBackstopWorker(t, d2, "w-2", state.WorkerDone)
	d2.terminateAbandonedWorker("w-2", "Forge-52bp", "repo")
	quiet, err := d2.db.RecentEvents(20)
	require.NoError(t, err)
	for _, e := range quiet {
		assert.NotEqual(t, state.EventWorkerAbandoned, e.Type,
			"the backstop announced a row it did not touch")
	}
}

// Called twice — as it is whenever an explicit termination and the defer both
// run — the second call is a no-op rather than a second event.
func TestTerminateAbandonedWorkerIsIdempotent(t *testing.T) {
	d := backstopDaemon(t)
	insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

	d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")
	d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

	assert.Equal(t, state.WorkerFailed, backstopStatus(t, d, "w-1"))

	events, err := d.db.RecentEvents(20)
	require.NoError(t, err)
	count := 0
	for _, e := range events {
		if e.Type == state.EventWorkerAbandoned {
			count++
		}
	}
	assert.Equal(t, 1, count, "the second call announced the same gap again")
}

// An empty worker ID (no claim row was ever inserted) and a row the bellows
// sweep has already deleted are both nothing to finalise. Neither may create a
// row or raise an event.
func TestTerminateAbandonedWorkerToleratesAMissingRow(t *testing.T) {
	d := backstopDaemon(t)

	d.terminateAbandonedWorker("", "Forge-52bp", "repo")
	d.terminateAbandonedWorker("gone", "Forge-52bp", "repo")

	events, err := d.db.RecentEvents(20)
	require.NoError(t, err)
	assert.Empty(t, events)

	_, err = d.db.GetWorkerStatus("gone")
	assert.ErrorIs(t, err, state.ErrWorkerNotFound)
}

// Installed as a defer it also covers the exit no explicit update can: a panic
// unwinding the dispatch goroutine.
func TestTerminateAbandonedWorkerRunsOnAPanicUnwind(t *testing.T) {
	d := backstopDaemon(t)
	insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

	func() {
		defer func() { _ = recover() }()
		defer d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")
		panic("smith exploded mid-pipeline")
	}()

	assert.Equal(t, state.WorkerFailed, backstopStatus(t, d, "w-1"))
}

// dispatchBead has roughly two dozen exits — early aborts, gotos, the pipeline
// error handler, the success path — and cannot be driven end to end from a test
// (it spawns claude in a real worktree). What CAN be pinned is the one property
// that makes the backstop cover all of them: the defer is registered before the
// function's first exit, so no return, goto or panic can precede it.
func TestDispatchBeadRegistersTheBackstopBeforeItsFirstExit(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	require.NoError(t, err)

	const signature = "func (d *Daemon) dispatchBead("
	start := strings.Index(string(src), signature)
	require.GreaterOrEqual(t, start, 0, "dispatchBead not found — has it been renamed?")
	body := string(src)[start:]

	deferAt := strings.Index(body, "defer d.terminateAbandonedWorker(claimWorkerID")
	require.GreaterOrEqual(t, deferAt, 0,
		"dispatchBead no longer installs the abandoned-worker backstop; every exit it has can leak a worker row claiming a dead Smith")

	for _, exit := range []string{"\n\t\treturn", "\n\t\tgoto ", "\n\treturn"} {
		if at := strings.Index(body, exit); at >= 0 {
			assert.Less(t, deferAt, at,
				"the backstop defer is registered after an exit (%q) in dispatchBead", strings.TrimSpace(exit))
		}
	}
}
