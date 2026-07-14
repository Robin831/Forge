package daemon

import (
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedResumableWorker inserts a worker row and stamps its session_id/model so
// ResumableWorkerByBeadID (which filters on branch != '' AND session_id != '')
// can find it. Passing an empty branch/session lets a test exercise the
// negative filters.
func seedResumableWorker(t *testing.T, db *state.DB, id, beadID, anvil, branch, session, model string) {
	t.Helper()
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID:        id,
		BeadID:    beadID,
		Anvil:     anvil,
		Branch:    branch,
		Status:    state.WorkerPaused,
		StartedAt: time.Now(),
	}))
	// UpdateWorkerSession sets both session_id and model in one write.
	require.NoError(t, db.UpdateWorkerSession(id, session, model))
}

// configureResumeAnvil registers a single anvil pointing at dir so the resume
// entrypoint can resolve a path past its anvil-lookup guard.
func configureResumeAnvil(t *testing.T, d *Daemon, anvil, dir string) {
	t.Helper()
	cfg := d.cfg.Load()
	updated := *cfg
	updated.Anvils = map[string]config.AnvilConfig{anvil: {Path: dir}}
	d.cfg.Store(&updated)
}

// TestResumeBeadWithMessage_Guards exercises every pre-dispatch guard/failure
// branch of ResumeBeadWithMessage. Each case returns before the dispatch
// goroutine is spawned, so no worktree/Smith machinery is required.
func TestResumeBeadWithMessage_Guards(t *testing.T) {
	t.Run("empty bead_id is rejected", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		id, err := d.ResumeBeadWithMessage("   ", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "bead_id is required")
	})

	t.Run("live pipeline is rejected (use resume, not resume-with-message)", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		// A registered control handle means a live pipeline owns the worktree.
		d.registerControlHandle("BD-LIVE", newControlHandle("worker-live"))
		id, err := d.ResumeBeadWithMessage("BD-LIVE", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "already has a live pipeline")
	})

	t.Run("no resumable worker row is rejected", func(t *testing.T) {
		d, _ := newQueueActionDaemon(t, "forge-a")
		id, err := d.ResumeBeadWithMessage("BD-NONE", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "no resumable worker row")
	})

	t.Run("worker missing session_id is not resumable (filtered out)", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		// Branch present but no session_id: ResumableWorkerByBeadID filters it
		// out, so the bead has no resumable row.
		seedResumableWorker(t, db, "w-nosess", "BD-NOSESS", "anvil-1", "forge/BD-NOSESS", "", "opus")
		id, err := d.ResumeBeadWithMessage("BD-NOSESS", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "no resumable worker row")
	})

	t.Run("resume preconditions fail when anvil was never recorded", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		// branch + session_id present (so the row is selected) but anvil empty,
		// so ResumeState reports the missing precondition.
		seedResumableWorker(t, db, "w-noanvil", "BD-NOANVIL", "", "forge/BD-NOANVIL", "sess-1", "opus")
		id, err := d.ResumeBeadWithMessage("BD-NOANVIL", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "cannot be resumed")
	})

	t.Run("unknown anvil is rejected", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		// Fully resumable worker, but no matching anvil in config.
		seedResumableWorker(t, db, "w-unk", "BD-UNKANVIL", "ghost-anvil", "forge/BD-UNKANVIL", "sess-1", "opus")
		id, err := d.ResumeBeadWithMessage("BD-UNKANVIL", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "not found or has no path")
	})

	t.Run("bead already in flight is rejected without clobbering the reservation", func(t *testing.T) {
		d, db := newQueueActionDaemon(t, "forge-a")
		configureResumeAnvil(t, d, "anvil-1", t.TempDir())
		seedResumableWorker(t, db, "w-inflight", "BD-INFLIGHT", "anvil-1", "forge/BD-INFLIGHT", "sess-1", "opus")
		// A concurrent dispatch already reserved the slot.
		d.activeBeads.Store("BD-INFLIGHT", true)

		id, err := d.ResumeBeadWithMessage("BD-INFLIGHT", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "already in flight")

		// The pre-existing reservation must survive the rejected call.
		_, ok := d.activeBeads.Load("BD-INFLIGHT")
		assert.True(t, ok, "existing in-flight reservation must not be deleted by a rejected resume")
	})

	t.Run("bead fetch failure releases the reserved slot", func(t *testing.T) {
		dir := t.TempDir()
		// Install a bd stub that fails on `bd show`, so crucible.FetchBead errors.
		stubBd(t, dir)
		d, db := newQueueActionDaemon(t, "forge-a")
		configureResumeAnvil(t, d, "anvil-1", dir)
		seedResumableWorker(t, db, "w-fetchfail", "BD-FETCHFAIL", "anvil-1", "forge/BD-FETCHFAIL", "sess-1", "opus")

		id, err := d.ResumeBeadWithMessage("BD-FETCHFAIL", "msg")
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), "failed to fetch bead")

		// The slot reserved just before the fetch must be released so the poller
		// can re-dispatch the bead later.
		_, ok := d.activeBeads.Load("BD-FETCHFAIL")
		assert.False(t, ok, "in-flight slot must be released when bead fetch fails")
		// No stale control handle should linger either.
		_, hasCtrl := d.lookupControlHandle("BD-FETCHFAIL")
		assert.False(t, hasCtrl, "control handle must not linger after a failed fetch")
	})
}
