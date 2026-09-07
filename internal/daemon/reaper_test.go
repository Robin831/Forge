package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

func reaperDaemon(t *testing.T) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	d := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.workerGeneration = "gen-current"
	db.SetDaemonGeneration(d.workerGeneration)
	return d
}

func insertReaperWorker(t *testing.T, d *Daemon, id string, status state.WorkerStatus, phase string, pid int) {
	t.Helper()
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        id,
		BeadID:    "Forge-g1j7",
		Anvil:     "repo",
		Status:    status,
		Phase:     phase,
		PID:       pid,
		StartedAt: time.Now(),
	}))
}

// backdate rewrites a row's generation, heartbeat and start time directly, which
// is how a test stands up "a row a previous daemon lifetime left behind" without
// running a second daemon.
func backdate(t *testing.T, d *Daemon, id, generation string, lastSeen time.Time) {
	t.Helper()
	beat := ""
	if !lastSeen.IsZero() {
		beat = lastSeen.Format(time.RFC3339Nano)
	}
	_, err := d.db.Conn().Exec(
		`UPDATE workers SET daemon_generation = ?, heartbeat_at = ?, started_at = ? WHERE id = ?`,
		generation, beat, lastSeen.Format(time.RFC3339Nano), id)
	require.NoError(t, err)
}

func statusOf(t *testing.T, d *Daemon, id string) state.WorkerStatus {
	t.Helper()
	s, err := d.db.GetWorkerStatus(id)
	require.NoError(t, err)
	return s
}

// The decision table, made without a daemon: the registry vetoes first, the
// generation decides on its own, and only a row of the running generation is
// aged against its heartbeat.
func TestJudgeLeak(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	grace := 3 * time.Minute
	fresh := now.Add(-30 * time.Second)
	stale := now.Add(-10 * time.Minute)

	beat := func(at time.Time) string { return at.Format(time.RFC3339Nano) }

	tests := []struct {
		name       string
		candidate  state.LeakCandidate
		registered []string
		wantLeaked bool
	}{
		{
			name:       "a registered row is never reaped, whatever the columns say",
			candidate:  state.LeakCandidate{ID: "w1", Generation: "gen-old", Heartbeat: beat(stale), StartedAt: stale},
			registered: []string{"w1"},
			wantLeaked: false,
		},
		{
			name:       "a prior generation with nothing running it is leaked",
			candidate:  state.LeakCandidate{ID: "w2", Generation: "gen-old", Heartbeat: beat(fresh), StartedAt: fresh},
			wantLeaked: true,
		},
		{
			name:       "an unrecorded generation is read the same way",
			candidate:  state.LeakCandidate{ID: "w3", Generation: "", Heartbeat: "", StartedAt: fresh},
			wantLeaked: true,
		},
		{
			name:       "this generation, unregistered, fresh heartbeat: left alone",
			candidate:  state.LeakCandidate{ID: "w4", Generation: "gen-current", Heartbeat: beat(fresh), StartedAt: fresh},
			wantLeaked: false,
		},
		{
			name:       "this generation, unregistered, stale heartbeat: leaked",
			candidate:  state.LeakCandidate{ID: "w5", Generation: "gen-current", Heartbeat: beat(stale), StartedAt: stale},
			wantLeaked: true,
		},
		{
			name:       "this generation, registered, stale heartbeat: left alone",
			candidate:  state.LeakCandidate{ID: "w6", Generation: "gen-current", Heartbeat: beat(stale), StartedAt: stale},
			registered: []string{"w6"},
			wantLeaked: false,
		},
		{
			name:       "this generation with no heartbeat ages against its insertion",
			candidate:  state.LeakCandidate{ID: "w7", Generation: "gen-current", StartedAt: stale},
			wantLeaked: true,
		},
		{
			name:       "this generation with nothing to age against is left for a later pass",
			candidate:  state.LeakCandidate{ID: "w8", Generation: "gen-current"},
			wantLeaked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live := map[string]struct{}{}
			for _, id := range tc.registered {
				live[id] = struct{}{}
			}
			got := judgeLeak(tc.candidate, "gen-current", live, grace, now)
			assert.Equal(t, tc.wantLeaked, got.leaked)
			assert.NotEmpty(t, got.reason, "every verdict must say why, since the reason is the whole content of the WARN")
		})
	}
}

// The regression this bead exists to avoid, driven end to end: a paused row from
// a dead daemon generation with a pid that no longer exists is exactly what
// every ownership signal calls leaked, and it must survive untouched because
// its session is resumable in place.
func TestReapLeavesAPausedRowFromADeadGenerationAlone(t *testing.T) {
	d := reaperDaemon(t)

	insertReaperWorker(t, d, "paused-resumable", state.WorkerPaused, "smith", 2157387)
	backdate(t, d, "paused-resumable", "gen-previous", time.Now().Add(-48*time.Hour))

	require.NoError(t, d.reapLeakedWorkers(context.Background()))
	assert.Equal(t, state.WorkerPaused, statusOf(t, d, "paused-resumable"))
}

// The leak the reaper exists to close: rows a previous daemon lifetime left
// behind, ended regardless of what their pid says — including one whose pid
// number reads alive, which is what a PID-based sweep can never reach.
func TestReapEndsRowsFromAPreviousDaemonLifetime(t *testing.T) {
	d := reaperDaemon(t)

	insertReaperWorker(t, d, "leaked-running", state.WorkerRunning, "smith", 424242)
	insertReaperWorker(t, d, "leaked-live-pid", state.WorkerRunning, "smith", 1) // pid 1 is always alive
	insertReaperWorker(t, d, "leaked-pending", state.WorkerPending, "", 0)
	insertReaperWorker(t, d, "monitoring", state.WorkerMonitoring, "bellows", 0)
	insertReaperWorker(t, d, "done", state.WorkerDone, "smith", 0)
	for _, id := range []string{"leaked-running", "leaked-live-pid", "leaked-pending", "monitoring", "done"} {
		backdate(t, d, id, "gen-previous", time.Now().Add(-2*time.Hour))
	}

	require.NoError(t, d.reapLeakedWorkers(context.Background()))

	assert.Equal(t, state.WorkerFailed, statusOf(t, d, "leaked-running"))
	assert.Equal(t, state.WorkerFailed, statusOf(t, d, "leaked-live-pid"),
		"a live pid is not evidence of life — the number may simply have been reused")
	assert.Equal(t, state.WorkerFailed, statusOf(t, d, "leaked-pending"))
	assert.Equal(t, state.WorkerMonitoring, statusOf(t, d, "monitoring"),
		"a bellows handoff row outlives every daemon lifetime by design")
	assert.Equal(t, state.WorkerDone, statusOf(t, d, "done"))

	events, err := d.db.RecentEvents(50)
	require.NoError(t, err)
	leaked := 0
	for _, e := range events {
		if e.Type == state.EventWorkerLeaked {
			leaked++
		}
	}
	assert.Equal(t, 3, leaked, "each reaped row is announced once")
}

// Within one daemon lifetime the registry is what spares a row, and it spares
// it even when the heartbeat has fallen arbitrarily far behind — a loaded host
// must not be able to end live work.
func TestReapSparesRegisteredWorkersOfTheRunningGeneration(t *testing.T) {
	d := reaperDaemon(t)

	insertReaperWorker(t, d, "held", state.WorkerRunning, "smith", 0)
	insertReaperWorker(t, d, "unheld", state.WorkerRunning, "smith", 0)
	for _, id := range []string{"held", "unheld"} {
		backdate(t, d, id, d.workerGeneration, time.Now().Add(-2*time.Hour))
	}

	release := d.trackWorker("held")
	require.NoError(t, d.reapLeakedWorkers(context.Background()))
	assert.Equal(t, state.WorkerRunning, statusOf(t, d, "held"))
	assert.Equal(t, state.WorkerFailed, statusOf(t, d, "unheld"))

	// Once released, the same row is reapable on the next pass — the release is
	// what makes it so, which is why every owner holds it through teardown.
	release()
	require.NoError(t, d.reapLeakedWorkers(context.Background()))
	assert.Equal(t, state.WorkerFailed, statusOf(t, d, "held"))
}

// A row of the running generation that nothing has registered is still left
// alone until the grace window has passed, because the common reading of that
// state is an owner that has just registered rather than one that never will.
func TestReapWaitsOutTheGraceWindowWithinOneGeneration(t *testing.T) {
	d := reaperDaemon(t)

	insertReaperWorker(t, d, "just-inserted", state.WorkerRunning, "smith", 0)
	require.NoError(t, d.reapLeakedWorkers(context.Background()))
	assert.Equal(t, state.WorkerRunning, statusOf(t, d, "just-inserted"),
		"a freshly inserted row of this generation is not a leak")

	backdate(t, d, "just-inserted", d.workerGeneration, time.Now().Add(-workerHeartbeatGrace-time.Minute))
	require.NoError(t, d.reapLeakedWorkers(context.Background()))
	assert.Equal(t, state.WorkerFailed, statusOf(t, d, "just-inserted"))
}

// The heartbeat loop moves the column for registered rows and only for them, so
// a registered row of the running generation never ages into the fallback the
// registry already spared it from.
func TestHeartbeatRefreshesRegisteredWorkersAndStopsWithTheirRelease(t *testing.T) {
	d := reaperDaemon(t)
	insertReaperWorker(t, d, "beating", state.WorkerRunning, "smith", 0)
	backdate(t, d, "beating", d.workerGeneration, time.Now().Add(-time.Hour))

	before, err := d.db.WorkerOwnershipOf("beating")
	require.NoError(t, err)

	release := d.trackWorker("beating")
	n, err := d.db.HeartbeatWorkers(d.liveWorkers.snapshot())
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	after, err := d.db.WorkerOwnershipOf("beating")
	require.NoError(t, err)
	assert.NotEqual(t, before.Heartbeat, after.Heartbeat)
	assert.WithinDuration(t, time.Now(), after.HeartbeatAt(), time.Minute)

	// After the release the id leaves the set, so nothing refreshes it and the
	// row ages out exactly as an unowned one should.
	release()
	assert.Empty(t, d.liveWorkers.snapshot())
	n, err = d.db.HeartbeatWorkers(d.liveWorkers.snapshot())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// The heartbeat loop stamps once BEFORE its first tick, so the invariant the
// reaper rests on — a registered row is heartbeated within the grace window —
// is established by the loop itself rather than borrowed from the insert that
// happens to stamp heartbeat_at today. Without it the first 30 seconds of a
// daemon lifetime rest on a property of a different function.
func TestHeartbeatStampsBeforeItsFirstTick(t *testing.T) {
	d := reaperDaemon(t)
	insertReaperWorker(t, d, "beating", state.WorkerRunning, "smith", 0)
	backdate(t, d, "beating", d.workerGeneration, time.Now().Add(-time.Hour))
	defer d.trackWorker("beating")()

	before, err := d.db.WorkerOwnershipOf("beating")
	require.NoError(t, err)

	// A context already cancelled: the loop returns on its first select, so
	// anything observed here happened ahead of the ticker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.runWorkerHeartbeat(ctx)

	after, err := d.db.WorkerOwnershipOf("beating")
	require.NoError(t, err)
	assert.NotEqual(t, before.Heartbeat, after.Heartbeat,
		"the heartbeat loop left the row unstamped until its first tick")
	assert.WithinDuration(t, time.Now(), after.HeartbeatAt(), time.Minute)
}

// The registry counts holders rather than flagging them: a resume that overlaps
// its predecessor's teardown owns the same row twice, and the first release must
// not drop the second owner's registration.
func TestLiveWorkerRegistryCountsOverlappingHolders(t *testing.T) {
	var r liveWorkerRegistry

	assert.False(t, r.has("w"))
	first := r.hold("w")
	second := r.hold("w")
	assert.True(t, r.has("w"))

	first()
	assert.True(t, r.has("w"), "the second holder still owns the row")
	first() // idempotent: a defer that somehow runs twice must not drop a live hold
	assert.True(t, r.has("w"))

	second()
	assert.False(t, r.has("w"))

	// An empty id registers nothing and releases nothing.
	r.hold("")()
	assert.Empty(t, r.snapshot())
}

func TestLiveWorkerRegistryIsConcurrencySafe(t *testing.T) {
	var r liveWorkerRegistry
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := r.hold("shared")
			_ = r.snapshot()
			_ = r.has("shared")
			release()
		}()
	}
	wg.Wait()
	assert.Empty(t, r.snapshot())
}

// Two daemon lifetimes never share a generation, even when the pid is recycled
// — which is the case that would let a restarted daemon adopt its predecessor's
// abandoned rows as its own and never reap them.
func TestDaemonGenerationsDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		g := newDaemonGeneration()
		require.NotEmpty(t, g)
		assert.False(t, seen[g], "generation %s was minted twice", g)
		seen[g] = true
	}
}
