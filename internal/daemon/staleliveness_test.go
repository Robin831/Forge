package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

// staleLivenessFixture builds a daemon over a temp state.db and returns it with
// a helper that inserts a worker whose log has been silent far longer than the
// stale interval the tests use.
func staleLivenessFixture(t *testing.T) (*Daemon, *state.DB, func(w state.Worker) state.Worker) {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{})

	insertSilent := func(w state.Worker) state.Worker {
		t.Helper()
		logFile := filepath.Join(tmpDir, w.ID+".log")
		require.NoError(t, os.WriteFile(logFile, []byte("log"), 0o644))
		old := time.Now().Add(-20 * time.Minute)
		require.NoError(t, os.Chtimes(logFile, old, old))
		w.LogPath = logFile
		if w.StartedAt.IsZero() {
			w.StartedAt = time.Now().Add(-25 * time.Minute)
		}
		require.NoError(t, db.InsertWorker(&w))
		return w
	}
	return d, db, insertSilent
}

const staleLivenessInterval = 5 * time.Minute

// TestCheckStaleWorkers_LivePIDStillStalls pins the unchanged half: a silent
// worker whose process is still running is a worker that may resume, so it
// keeps the recoverable 'stalled' mask.
func TestCheckStaleWorkers_LivePIDStillStalls(t *testing.T) {
	d, db, insertSilent := staleLivenessFixture(t)

	// The test process itself is the one pid guaranteed to be alive.
	insertSilent(state.Worker{
		ID: "w-live", BeadID: "BD-live", Anvil: "anvil-1",
		Status: state.WorkerRunning, Phase: "smith", PID: os.Getpid(),
	})

	d.checkStaleWorkers(staleLivenessInterval)

	w, err := db.GetWorker("w-live")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerStalled, w.Status,
		"a silent worker whose process is alive must stay stalled, not be failed")
}

// TestCheckStaleWorkers_NoPIDStillStalls covers the row that records no pid at
// all — a pending claim whose dispatch never spawned anything. Nothing is
// established about a process there, so the ordinary stalled path must stand
// rather than every pending worker being failed on a slow host.
func TestCheckStaleWorkers_NoPIDStillStalls(t *testing.T) {
	d, db, insertSilent := staleLivenessFixture(t)

	insertSilent(state.Worker{
		ID: "w-nopid", BeadID: "BD-nopid", Anvil: "anvil-1",
		Status: state.WorkerRunning, Phase: "smith", PID: 0,
	})

	d.checkStaleWorkers(staleLivenessInterval)

	w, err := db.GetWorker("w-nopid")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerStalled, w.Status,
		"a worker row with no recorded pid must keep the stalled behaviour")
}

// TestProcessAlive_RejectsNonPositivePIDs is the guard on the syscall the
// liveness check is built from: kill(0, 0) addresses the caller's own process
// group and kill(-1, 0) every process it may signal, both of which succeed —
// so without the guard a worker row recording no pid would read as one holding
// a live process, which is exactly the row this feature must be able to fail.
func TestProcessAlive_RejectsNonPositivePIDs(t *testing.T) {
	assert.False(t, processAlive(0), "pid 0 is not a process")
	assert.False(t, processAlive(-1), "a negative pid is not a process")
	assert.True(t, processAlive(os.Getpid()), "the running test process must read as alive")
}
