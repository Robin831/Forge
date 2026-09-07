//go:build !windows

package daemon

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

// reapedPID runs a trivial child to completion and returns its pid — a pid that
// named a real process and no longer names one. It is reaped by cmd.Run, so it
// is not a zombie, which kill(pid, 0) would still report as alive.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Run(), "could not run the throwaway child")
	pid := cmd.Process.Pid
	require.Positive(t, pid)
	require.False(t, processAlive(pid), "the child should be gone once Run has reaped it")
	return pid
}

// TestCheckStaleWorkers_DeadPIDFails is the recovery this bead exists for: a
// silent worker whose process is gone is marked failed rather than stalled, so
// the dispatch slot it holds (ActiveDispatchWorkers counts 'stalled') is
// released within one detector interval instead of being pinned until an
// operator clears it by hand.
func TestCheckStaleWorkers_DeadPIDFails(t *testing.T) {
	d, db, insertSilent := staleLivenessFixture(t)

	insertSilent(state.Worker{
		ID: "w-dead", BeadID: "BD-dead", Anvil: "anvil-1",
		Status: state.WorkerRunning, Phase: "smith", PID: reapedPID(t),
	})

	d.checkStaleWorkers(staleLivenessInterval)

	w, err := db.GetWorker("w-dead")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerFailed, w.Status,
		"a silent worker whose process is gone must be failed, not stalled")

	// The row must no longer occupy a dispatch slot, which is the whole point
	// of choosing 'failed' over 'stalled'.
	active, err := db.ActiveDispatchWorkers()
	require.NoError(t, err)
	for _, a := range active {
		assert.NotEqual(t, "w-dead", a.ID, "a failed row must not hold a dispatch slot")
	}

	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	var found bool
	for _, ev := range events {
		if ev.Type == state.EventWorkerProcessGone {
			found = true
		}
	}
	assert.True(t, found, "the transition must be visible in the activity feed")
}

// TestCheckStaleWorkers_DeadPIDInTemperStillStalls is the regression this
// check must not become. The pid column is written only by the schematic and
// Smith spawns and never cleared, so a row that has moved on to `temper`
// carries the pid of the Smith that finished before it — dead by design. A
// build or test suite that outruns stale_interval would otherwise be reported
// as an abandoned worker and, because 'failed' is terminal where 'stalled' is
// not, have its dispatch slot handed to a second worker while it was still
// running.
func TestCheckStaleWorkers_DeadPIDInTemperStillStalls(t *testing.T) {
	d, db, insertSilent := staleLivenessFixture(t)

	insertSilent(state.Worker{
		ID: "w-temper", BeadID: "BD-temper", Anvil: "anvil-1",
		Status: state.WorkerRunning, Phase: "temper", PID: reapedPID(t),
	})

	d.checkStaleWorkers(staleLivenessInterval)

	w, err := db.GetWorker("w-temper")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerStalled, w.Status,
		"a row in a phase that does not run the session its pid names must keep the recoverable mask")
}

// TestCheckStaleWorkers_DeadPIDSparesAMonitoringHandoff is the case a pid test
// alone gets wrong. A pipeline flips its own row to 'monitoring' at warden
// approval and hands the PR to Bellows; the Smith process behind that row is
// SUPPOSED to be gone. Failing it would report every successful dispatch whose
// row went quiet as a dead worker, so the write is refused (it goes through
// FailWorkerIfUnfinished, which exempts the handoff statuses) and the row falls
// through to the ordinary stalled path it had before this check existed.
func TestCheckStaleWorkers_DeadPIDSparesAMonitoringHandoff(t *testing.T) {
	d, db, insertSilent := staleLivenessFixture(t)

	insertSilent(state.Worker{
		ID: "w-monitoring", BeadID: "BD-mon", Anvil: "anvil-1",
		Status: state.WorkerMonitoring, Phase: "smith", PID: reapedPID(t),
	})

	d.checkStaleWorkers(staleLivenessInterval)

	row, err := db.GetWorkerBackstopState("w-monitoring")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerStalled, row.Status,
		"a monitoring handoff must not be failed just because its Smith process has exited")
	assert.Equal(t, state.WorkerMonitoring, row.PrevStatus,
		"the masked status must still be restorable by the recovery pass")
}
