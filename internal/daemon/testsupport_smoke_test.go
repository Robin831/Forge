package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

// TestHarnessSmoke exercises the fixture itself rather than any daemon
// behaviour: the sub-tasks that assert on behaviour land after this one, and
// until they do an unexercised fixture is a file that compiles and is never run
// — which is exactly how a harness comes to be wrong on the day it is first
// needed. Every helper is touched here, so a change that breaks one fails now.
func TestHarnessSmoke(t *testing.T) {
	e := newTestDaemon(t, withPollInterval(10*time.Millisecond), withWorkerLimit(2))

	// The daemon was constructed with what the options asked for, and it is
	// running as the generation the database stamps.
	cfg := e.d.config()
	assert.Equal(t, 10*time.Millisecond, cfg.Settings.PollInterval)
	assert.Equal(t, 2, cfg.Settings.MaxTotalSmiths)
	assert.NotEmpty(t, e.generation())

	// A real, migrated database on disk — not :memory: — is what the daemon
	// holds open.
	require.FileExists(t, e.dbPath)

	// Seeding writes rows the production insert path cannot: a leaked row from
	// a lifetime that ended, and a paused one of the running generation.
	e.seedBead("Forge-smoke1", "in_progress")
	leaked := e.seedWorkerRow(workerRow{
		BeadID:     "Forge-smoke1",
		Status:     state.WorkerRunning,
		Phase:      "smith",
		Generation: "gen-a-lifetime-that-ended",
		Heartbeat:  time.Now().Add(-time.Hour),
	})
	parked := e.seedWorkerRow(workerRow{
		BeadID:     "Forge-smoke2",
		Status:     state.WorkerPaused,
		Phase:      "smith",
		Generation: e.generation(),
		Heartbeat:  time.Now(),
	})

	// The ownership columns came back exactly as planted — the property the
	// whole fixture rests on.
	own, err := e.db.WorkerOwnershipOf(leaked.ID)
	require.NoError(t, err)
	assert.Equal(t, "gen-a-lifetime-that-ended", own.Generation)
	assert.False(t, own.HeartbeatAt().IsZero(), "a planted heartbeat must be readable back")

	own, err = e.db.WorkerOwnershipOf(parked.ID)
	require.NoError(t, err)
	assert.Equal(t, e.generation(), own.Generation)

	// waitForStatus observes a transition rather than sleeping past it: the
	// status is written by a loop running under the env, so the assertion also
	// covers startLoop and the wait in stop.
	e.waitForStatus(t, "Forge-smoke1", string(state.WorkerRunning), time.Second)
	e.startLoop(func(ctx context.Context) {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return
		}
		_, _ = e.conn().Exec(`UPDATE workers SET status = ? WHERE id = ?`,
			string(state.WorkerDone), leaked.ID)
	})
	e.waitForStatus(t, "Forge-smoke1", string(state.WorkerDone), 2*time.Second)
	e.waitForWorkerStatus(t, parked.ID, state.WorkerPaused, time.Second)

	// A real child process, killed out from under the daemon and reaped, so
	// "gone" is true of a pid rather than of a zombie that still answers a
	// liveness probe.
	pid, cmd := e.spawnDummyWorker(t)
	require.NotNil(t, cmd)
	require.Greater(t, pid, 0)
	e.waitFor(t, func() bool { return processAlive(pid) }, "the dummy worker to be running")
	e.killWorker(pid)
	assert.False(t, processAlive(pid), "killWorker must leave no live process behind")

	// The capture sink records level, message and attributes, which is what
	// makes a WARN assertion an assertion about the WARN and not about a
	// formatted line that happens to contain a word.
	e.d.logger.Warn("harness smoke warning", "worker", leaked.ID, "count", 3)
	require.True(t, e.hasLog(slog.LevelWarn, "harness smoke warning"))
	assert.False(t, e.hasLog(slog.LevelError, "harness smoke warning"),
		"a WARN must not match at ERROR")

	rec, ok := e.findLog(slog.LevelWarn, "harness smoke warning")
	require.True(t, ok)
	worker, ok := rec.Attr("worker")
	require.True(t, ok, "attributes must survive capture")
	assert.Equal(t, leaked.ID, worker)

	e.waitForLog(t, slog.LevelWarn, "harness smoke warning")

	// The ownership loops start and shut down on the env's context. stop is
	// idempotent, so calling it here and again from the registered cleanup is
	// not a double close — and it fails the test if either loop ignores its
	// context.
	e.start()
	e.stop()

	t.Run("two generations over one database", func(t *testing.T) {
		// The condition the ownership columns exist for, produced rather than
		// asserted: one daemon writes a row, that lifetime ends, and a second
		// daemon comes up over the same file. withGeneration names the two
		// lifetimes and withDBPath is what makes them share a database.
		first := newTestDaemon(t,
			withGeneration("gen-first-lifetime"),
			withLogLevel(slog.LevelWarn))

		first.seedBead("Forge-smoke3", "in_progress")
		row := first.seedWorkerRow(workerRow{
			BeadID:     "Forge-smoke3",
			Status:     state.WorkerRunning,
			Phase:      "smith",
			Generation: first.generation(),
			Heartbeat:  time.Now(),
		})

		// Owned by the lifetime that wrote it, on that lifetime's own reading.
		own, err := first.db.WorkerOwnershipOf(row.ID)
		require.NoError(t, err)
		assert.Equal(t, first.generation(), own.Generation)

		// withLogLevel raises the sink's floor, so a record below it is
		// dropped rather than recorded — which is what keeps a level
		// assertion an assertion about the level.
		first.d.logger.Info("below the capture floor")
		assert.False(t, first.hasLog(slog.LevelInfo, "below the capture floor"))
		first.d.logger.Warn("at the capture floor")
		assert.True(t, first.hasLog(slog.LevelWarn, "at the capture floor"))

		// The restart. The second daemon opens the first one's database and
		// reads the same row.
		second := newTestDaemon(t,
			withDBPath(first.dbPath),
			withGeneration("gen-second-lifetime"))
		require.Equal(t, first.dbPath, second.dbPath)

		own, err = second.db.WorkerOwnershipOf(row.ID)
		require.NoError(t, err)
		assert.Equal(t, "gen-first-lifetime", own.Generation,
			"the row must survive the restart carrying the generation that wrote it")
		assert.NotEqual(t, second.generation(), own.Generation,
			"a row of an ended lifetime must not read as owned by the running one")

		// And the second lifetime's own rows are its own, so the two are
		// distinguishable in both directions rather than everything reading
		// as foreign.
		second.seedBead("Forge-smoke4", "in_progress")
		mine := second.seedWorkerRow(workerRow{
			BeadID:     "Forge-smoke4",
			Status:     state.WorkerRunning,
			Phase:      "smith",
			Generation: second.generation(),
			Heartbeat:  time.Now(),
		})
		own, err = second.db.WorkerOwnershipOf(mine.ID)
		require.NoError(t, err)
		assert.Equal(t, second.generation(), own.Generation)
	})
}
