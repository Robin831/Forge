package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/state"
)

// resetSpy is a bellowsMonitorIface double that records only the calls this
// file cares about: the snapshot clear a reattach must trigger.
type resetSpy struct {
	bellowsMonitorIface
	resets []string
}

func (s *resetSpy) ResetPRState(anvil string, prNumber int) {
	s.resets = append(s.resets, anvil+"/"+strconv.Itoa(prNumber))
}

// newDetachTestDaemon builds a Daemon over a temp state.db with one anvil.
func newDetachTestDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:         db,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:     context.Background(),
		reqTracker: *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{"munin": {Path: dir}},
	})
	return d, db
}

// insertDetachPR records an open PR and returns its row id.
func insertDetachPR(t *testing.T, db *state.DB, number int, beadID string) int {
	t.Helper()
	pr := &state.PR{
		Number:    number,
		Anvil:     "munin",
		BeadID:    beadID,
		Branch:    "forge/" + beadID,
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NotZero(t, pr.ID)
	return pr.ID
}

// TestActionBlockedByDetach is the dispatch guard's truth table. A detached PR
// refuses the worker-spawning actions and nothing else: manual verbs still run
// (detach means "stop automatic work", not "brick the PR"), and the post-merge
// bookkeeping still runs (a muted PR still merges, and a merged bead left open
// blocks its dependents).
func TestActionBlockedByDetach(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	detachedID := insertDetachPR(t, db, 11, "TEST-detached")
	insertDetachPR(t, db, 12, "TEST-attached")
	require.NoError(t, db.UpdatePRBellowsDetached(detachedID, true))

	cases := []struct {
		name string
		req  lifecycle.ActionRequest
		want bool
	}{
		{"detached + automatic CI fix", lifecycle.ActionRequest{Action: lifecycle.ActionFixCI, Anvil: "munin", PRNumber: 11}, true},
		{"detached + automatic review fix", lifecycle.ActionRequest{Action: lifecycle.ActionFixReview, Anvil: "munin", PRNumber: 11}, true},
		{"detached + automatic rebase", lifecycle.ActionRequest{Action: lifecycle.ActionRebase, Anvil: "munin", PRNumber: 11}, true},
		{"detached + automatic assay", lifecycle.ActionRequest{Action: lifecycle.ActionAssayReview, Anvil: "munin", PRNumber: 11}, true},
		{"detached + manual assay run", lifecycle.ActionRequest{Action: lifecycle.ActionAssayReview, Anvil: "munin", PRNumber: 11, IsManual: true}, false},
		{"detached + manual CI fix", lifecycle.ActionRequest{Action: lifecycle.ActionFixCI, Anvil: "munin", PRNumber: 11, IsManual: true}, false},
		{"detached + close bead", lifecycle.ActionRequest{Action: lifecycle.ActionCloseBead, Anvil: "munin", PRNumber: 11}, false},
		{"detached + cleanup", lifecycle.ActionRequest{Action: lifecycle.ActionCleanup, Anvil: "munin", PRNumber: 11}, false},
		{"attached + automatic CI fix", lifecycle.ActionRequest{Action: lifecycle.ActionFixCI, Anvil: "munin", PRNumber: 12}, false},
		{"unknown PR fails open", lifecycle.ActionRequest{Action: lifecycle.ActionFixCI, Anvil: "munin", PRNumber: 99}, false},
		{"no PR number fails open", lifecycle.ActionRequest{Action: lifecycle.ActionFixCI, Anvil: "munin"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, d.actionBlockedByDetach(tc.req))
		})
	}
}

// TestHandleLifecycleAction_DetachedPRIsNoOp pins that the guard fires before
// any work is scheduled: the bead is never registered in flight, so a detached
// PR leaves no residue behind for the next cycle to trip over.
func TestHandleLifecycleAction_DetachedPRIsNoOp(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	id := insertDetachPR(t, db, 21, "TEST-noop")
	require.NoError(t, db.UpdatePRBellowsDetached(id, true))

	d.handleLifecycleAction(context.Background(), lifecycle.ActionRequest{
		Action:   lifecycle.ActionFixReview,
		Anvil:    "munin",
		PRNumber: 21,
		BeadID:   "TEST-noop",
		Branch:   "forge/TEST-noop",
	})

	_, inFlight := d.activeBeads.Load("TEST-noop")
	assert.False(t, inFlight, "detached PR must not claim the bead slot")
}

// TestDrainPendingAction_DropsDetachedAndKeepsDraining is the parked-action
// half. An action parked while the PR was attached must not slip through by
// being drained after the detach — and dropping it must not strand the actions
// parked behind it, which is why the drain path carries its own guard rather
// than relying on handleLifecycleAction's (that one returns before the
// goroutine whose deferred drain would continue the chain).
func TestDrainPendingAction_DropsDetachedAndKeepsDraining(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	detachedID := insertDetachPR(t, db, 31, "TEST-drain")
	require.NoError(t, db.UpdatePRBellowsDetached(detachedID, true))
	insertDetachPR(t, db, 32, "TEST-drain")

	// The second action is a cleanup on the still-attached PR: it needs no
	// worktree and no claude session, and the review-fix bookkeeping row it
	// deletes is the observable proof that it actually ran.
	_, err := db.RecordReviewFixDispatch("munin", 32, "deadbeef")
	require.NoError(t, err)

	// Both are parked under one bead: a CI fix on the detached PR (drained
	// first by priority) and a cleanup on the PR that is still attached.
	d.parkPendingAction("TEST-drain", lifecycle.ActionRequest{
		Action: lifecycle.ActionFixCI, Anvil: "munin", PRNumber: 31, BeadID: "TEST-drain",
	})
	d.parkPendingAction("TEST-drain", lifecycle.ActionRequest{
		Action: lifecycle.ActionCleanup, Anvil: "munin", PRNumber: 32, BeadID: "TEST-drain",
	})

	d.drainPendingAction(context.Background(), "TEST-drain")
	d.wg.Wait()

	_, stillParked := d.pendingActions.Load("TEST-drain")
	assert.False(t, stillParked, "the parked set must be fully drained")

	row, err := db.GetReviewFixDispatch("munin", 32)
	require.NoError(t, err)
	assert.Nil(t, row, "the action parked behind the detached one must still run")
}

// TestPRAction_DetachBellows covers the verb end to end: the flag is persisted,
// the in-flight fix worker for that PR is stopped, and a worker on another PR
// is left alone.
func TestPRAction_DetachBellows(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	prID := insertDetachPR(t, db, 41, "TEST-detach")
	insertDetachPR(t, db, 42, "TEST-other")

	// PID 0: killWorkerProcess has no process to signal and marks the row
	// failed, which is the observable outcome the verb owes the operator.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-burnish", BeadID: "TEST-detach", Anvil: "munin", Branch: "forge/TEST-detach",
		Status: state.WorkerRunning, Phase: "burnish", PRNumber: 41, StartedAt: time.Now(),
	}))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-other", BeadID: "TEST-other", Anvil: "munin", Branch: "forge/TEST-other",
		Status: state.WorkerRunning, Phase: "quench", PRNumber: 42, StartedAt: time.Now(),
	}))

	payload, _ := json.Marshal(ipc.PRActionPayload{
		Action: "detach_bellows", PRID: prID, PRNumber: 41, Anvil: "munin", BeadID: "TEST-detach",
	})
	resp := d.handleIPC(ipc.Command{Type: "pr_action", Payload: payload})
	require.Equal(t, "ok", resp.Type)

	pr, err := db.GetPRByID(prID)
	require.NoError(t, err)
	assert.True(t, pr.BellowsDetached, "detach must persist prs.bellows_detached")

	d.wg.Wait()

	killed, err := db.GetWorker("w-burnish")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerFailed, killed.Status, "the PR's in-flight fix worker must be stopped")
	spared, err := db.GetWorker("w-other")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerRunning, spared.Status, "another PR's worker must be left alone")
}

// TestPRAction_DetachBellows_NoWorkerIsSuccess — "nothing running" is what the
// operator asked for, not an error.
func TestPRAction_DetachBellows_NoWorkerIsSuccess(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	prID := insertDetachPR(t, db, 51, "TEST-quiet")

	payload, _ := json.Marshal(ipc.PRActionPayload{
		Action: "detach_bellows", PRID: prID, PRNumber: 51, Anvil: "munin", BeadID: "TEST-quiet",
	})
	resp := d.handleIPC(ipc.Command{Type: "pr_action", Payload: payload})
	require.Equal(t, "ok", resp.Type)
	d.wg.Wait()

	pr, err := db.GetPRByID(prID)
	require.NoError(t, err)
	assert.True(t, pr.BellowsDetached)
}

// TestPRAction_DetachBellows_UnresolvablePRRefused — reporting success without
// writing the flag would leave the operator watching Forge keep working the PR.
func TestPRAction_DetachBellows_UnresolvablePRRefused(t *testing.T) {
	d, _ := newDetachTestDaemon(t)

	payload, _ := json.Marshal(ipc.PRActionPayload{
		Action: "detach_bellows", PRNumber: 404, Anvil: "munin",
	})
	resp := d.handleIPC(ipc.Command{Type: "pr_action", Payload: payload})
	require.Equal(t, "error", resp.Type)
}

// TestPRAction_ReattachBellows clears the flag and drops bellows' cached
// snapshot, so the problems that outlived the mute are re-detected as fresh
// transitions rather than swallowed as state it has already seen.
func TestPRAction_ReattachBellows(t *testing.T) {
	d, db := newDetachTestDaemon(t)
	prID := insertDetachPR(t, db, 61, "TEST-reattach")
	require.NoError(t, db.UpdatePRBellowsDetached(prID, true))
	spy := &resetSpy{}
	d.bellowsMonitor = spy

	payload, _ := json.Marshal(ipc.PRActionPayload{
		Action: "reattach_bellows", PRID: prID, PRNumber: 61, Anvil: "munin", BeadID: "TEST-reattach",
	})
	resp := d.handleIPC(ipc.Command{Type: "pr_action", Payload: payload})
	require.Equal(t, "ok", resp.Type)

	pr, err := db.GetPRByID(prID)
	require.NoError(t, err)
	assert.False(t, pr.BellowsDetached, "reattach must clear prs.bellows_detached")
	assert.Equal(t, []string{"munin/61"}, spy.resets, "reattach must clear the cached snapshot")

	// The automatic loop is open again.
	assert.False(t, d.actionBlockedByDetach(lifecycle.ActionRequest{
		Action: lifecycle.ActionFixCI, Anvil: "munin", PRNumber: 61,
	}))
}

// compile-time assurance the spy still satisfies the interface the daemon holds.
var _ bellowsMonitorIface = (*resetSpy)(nil)
