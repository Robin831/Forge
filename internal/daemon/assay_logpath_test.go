package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

// newAssayLogPathDaemon builds a minimal Daemon with a fresh state DB, enough
// to exercise assayLogPathRecorder against real worker rows.
func newAssayLogPathDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return d, db
}

func insertAssayWorker(t *testing.T, db *state.DB, id string) {
	t.Helper()
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID:        id,
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		Status:    state.WorkerRunning,
		Phase:     "assay",
		PRNumber:  713,
		StartedAt: time.Now(),
	}))
}

// Regression: Assay inserted its worker row without a log path and never
// recorded one, so the Hearth live panel had nothing to stream and rendered an
// empty transcript for the whole run.
func TestAssayLogPathRecorderRecordsFirstPassLog(t *testing.T) {
	d, db := newAssayLogPathDaemon(t)
	insertAssayWorker(t, db, "w-assay-1")

	rec := d.assayLogPathRecorder("w-assay-1")
	require.NotNil(t, rec)
	rec("/wt/.forge-logs/assay-100-1.log")

	workers, err := db.ActiveWorkers()
	require.NoError(t, err)
	require.Len(t, workers, 1)
	require.Equal(t, "/wt/.forge-logs/assay-100-1.log", workers[0].LogPath)
}

// The deep passes fan out concurrently, so the recorder must latch the first
// log it sees rather than letting the panel flip between passes mid-run.
func TestAssayLogPathRecorderLatchesFirstLogUnderConcurrency(t *testing.T) {
	d, db := newAssayLogPathDaemon(t)
	insertAssayWorker(t, db, "w-assay-2")

	rec := d.assayLogPathRecorder("w-assay-2")
	rec("/wt/.forge-logs/assay-triage.log")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec("/wt/.forge-logs/assay-deep.log")
		}(i)
	}
	wg.Wait()

	workers, err := db.ActiveWorkers()
	require.NoError(t, err)
	require.Len(t, workers, 1)
	require.Equal(t, "/wt/.forge-logs/assay-triage.log", workers[0].LogPath)
}

// The Burnish-coordination path runs Assay without its own worker row; there is
// nothing to point at a log, and the recorder must not touch other rows.
func TestAssayLogPathRecorderNilWithoutWorkerID(t *testing.T) {
	d, db := newAssayLogPathDaemon(t)
	insertAssayWorker(t, db, "w-assay-3")

	require.Nil(t, d.assayLogPathRecorder(""))

	workers, err := db.ActiveWorkers()
	require.NoError(t, err)
	require.Len(t, workers, 1)
	require.Empty(t, workers[0].LogPath)
}
