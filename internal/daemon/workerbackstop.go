package daemon

import (
	"errors"
	"fmt"

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
// cold resume, see state.WorkerStatus.NeedsTerminalBackstop — is untouched.
func (d *Daemon) terminateAbandonedWorker(workerID, beadID, anvil string) {
	if workerID == "" {
		return
	}

	// Read first only to name the status in the log and the event, and to skip
	// the write in the overwhelmingly common case where a path already
	// finalised the row. The UPDATE below is what actually decides.
	status, err := d.db.GetWorkerStatus(workerID)
	if err != nil {
		if !errors.Is(err, state.ErrWorkerNotFound) {
			d.logger.Warn("could not read worker status at dispatch exit",
				"bead", beadID, "worker", workerID, "error", err)
		}
		return
	}
	if !status.NeedsTerminalBackstop() {
		return
	}

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

	reason := fmt.Sprintf("Worker %s left in %q when its dispatch exited — marked failed", workerID, string(status))
	d.logger.Warn("dispatch exited without finalising its worker row; marked failed",
		"bead", beadID, "anvil", anvil, "worker", workerID, "status", string(status))
	if err := d.db.LogEvent(state.EventWorkerAbandoned, reason, beadID, anvil); err != nil {
		d.logger.Warn("failed to log abandoned worker event",
			"bead", beadID, "worker", workerID, "error", err)
	}
}
