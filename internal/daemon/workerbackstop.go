package daemon

import (
	"errors"
	"fmt"

	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
)

// terminateAbandonedWorker is the dispatch goroutine's exit backstop: whatever
// path a dispatch took out of the goroutine, the claim worker row it inserted
// must not be left claiming a live Smith.
//
// It exists because the finalisation is otherwise scattered. Roughly thirty
// UpdateWorkerStatus call sites across internal/daemon and internal/pipeline
// each terminate the row for the exit they own, and an exit nobody wrote one
// for — the pipeline's abort when Temper is killed by a cancelled context is
// the one that was found — returns with the row still 'running'. That row then
// counts against max_total_smiths (state.ActiveDispatchWorkers) forever, holds
// a panel open in Hearth and the dashboard for a process that is gone, and is
// only ever cleared by hand. A backstop at the one place every exit passes
// through covers all of them uniformly, including a panic unwinding the stack.
//
// It is idempotent with every one of those call sites, the
// preDispatchRemoteBranchCheck termination included: the decision is made in
// state.FailWorkerIfUnfinished's own WHERE clause, so a row an ordinary path
// already finalised — or deliberately handed off live to Bellows or to a
// cold resume, see state.WorkerBackstopState.NeedsTerminalBackstop — is
// untouched.
func (d *Daemon) terminateAbandonedWorker(workerID, beadID, anvil string) {
	if workerID == "" {
		return
	}

	// Read first only to name the status in the log and the event, and to skip
	// the write in the overwhelmingly common case where a path already
	// finalised the row. The UPDATE below is what actually decides, and the
	// read is advisory in both directions: a row that is GONE has nothing to
	// finalise, but a read that merely FAILED says nothing about the row, so
	// the conditional write still runs. Returning there would let one errored
	// SELECT reopen the exact gap the backstop exists to close, and it would
	// do so on a database busy enough that a dispatch is likelier than usual
	// to have exited badly. The status is read together with the prev_status
	// the watchdog masked, because a 'stalled' row is only abandoned when the
	// status underneath it was.
	row, err := d.db.GetWorkerBackstopState(workerID)
	switch {
	case errors.Is(err, state.ErrWorkerNotFound):
		return
	case err != nil:
		d.logger.Warn("could not read worker status at dispatch exit; attempting termination anyway",
			"bead", beadID, "worker", workerID, "error", err)
		row = state.WorkerBackstopState{}
	case !row.NeedsTerminalBackstop():
		return
	}
	status := row.Status

	failed, err := d.db.FailWorkerIfUnfinished(workerID)
	if err != nil {
		d.logger.Warn("could not terminate an abandoned worker row at dispatch exit",
			"bead", beadID, "worker", workerID, "status", string(status), "error", err)
		return
	}
	if !failed {
		// An ordinary path landed a terminal status between the read and the
		// write. That is the backstop working, not a failure.
		return
	}

	// An unread status is reported as unknown rather than as the empty string,
	// which would read as a row carrying no status at all.
	observed := string(status)
	if observed == "" {
		observed = "unknown"
	}
	reason := fmt.Sprintf("Worker %s left in %q when its dispatch exited — marked failed", workerID, observed)
	d.logger.Warn("dispatch exited without finalising its worker row; marked failed",
		"bead", beadID, "anvil", anvil, "worker", workerID, "status", observed)
	if err := d.db.LogEvent(state.EventWorkerAbandoned, reason, beadID, anvil); err != nil {
		d.logger.Warn("failed to log abandoned worker event",
			"bead", beadID, "worker", workerID, "error", err)
	}
}

// finalizeCrucibleWorker terminates the parent claim row a Crucible run leaves
// behind, at the exit that owns it.
//
// dispatchBead flips the row to 'running' before crucible.Run and the Crucible
// itself never moves it, so every exit of that block returns with the row still
// claiming live work. The backstop above would catch them — that is the leak it
// was written for — but it would catch them as ABANDONED: every successful epic
// would be recorded 'failed' and would emit worker_abandoned, an event
// documented to name a finalisation nobody wrote. An expected exit emitting it
// on every run is how a signal like that stops being read.
//
// The status is the caller's because only the caller knows how the run ended,
// and it is written unconditionally: unlike the backstop, this is the path that
// owns the row, so there is no concurrent finaliser to lose a race with.
func (d *Daemon) finalizeCrucibleWorker(workerID string, bead poller.Bead, status state.WorkerStatus) {
	if workerID == "" {
		return
	}
	if err := d.db.UpdateWorkerStatus(workerID, status); err != nil {
		d.logger.Warn("failed to finalise the crucible parent worker row",
			"bead", bead.ID, "anvil", bead.Anvil, "worker", workerID, "status", string(status), "error", err)
	}
}

// crucibleWorkerStatus is the terminal status the parent claim row takes for a
// finished Crucible run.
//
// 'done' and not 'monitoring' on success: the Crucible has already closed the
// parent bead behind its final PR, which Bellows picks up through its own row,
// so nothing would ever move a monitoring row here and it would hold a live
// panel open for a dispatch that finished.
//
// Anything that is not a success is 'failed', which is a claim about the ROW
// and not about the Crucible: an error, a paused run, and a Result that reports
// neither an error nor success are alike in that no epic reached its final PR,
// and the one honest reading of a row whose dispatch ended without succeeding
// is that it did not succeed. Deriving it from the result rather than from
// which branch the caller took is what lets that call be unconditional, so an
// exit nobody wrote a finalisation for cannot exist.
func crucibleWorkerStatus(result *crucible.Result) state.WorkerStatus {
	if result != nil && result.Error == nil && result.Success {
		return state.WorkerDone
	}
	return state.WorkerFailed
}
