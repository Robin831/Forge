package daemon

import (
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/state"
)

// pidNamesRunningSession reports whether a worker row in this phase records the
// pid of the session that phase is CURRENTLY running — the one question that
// makes a dead pid evidence of anything.
//
// The pid column is written at two places in the pipeline and nowhere else: the
// schematic's OnSpawn and each Smith spawn (internal/pipeline). It is never
// cleared, so a row that has moved on to `temper` or `warden` still carries the
// pid of the Smith that finished before it — a dead process by design, exactly
// as a `monitoring` row's is. Reading THAT as an abandoned worker would fail
// every pipeline whose test suite outruns stale_interval, and — because
// 'failed' is terminal where 'stalled' is not — hand its max_total_smiths slot
// to a second dispatch while it was still working.
//
// So the check is confined to the two phases that record a pid, which are also
// the ones the leak is about: they hold a dispatch slot. The lifecycle phases
// record a Smith pid of their own but hold no slot, and they keep running past
// their session (verify, push, resolve threads), which is the same window
// `temper` is — a dead pid there would be read the same wrong way.
//
// A daemon RESTART needs none of this: shutdown's orphan sweep already fails
// every worker row whose recorded process is gone, whatever its phase, because
// at that point no goroutine survives to contradict it.
func pidNamesRunningSession(phase string) bool {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "smith", "schematic":
		return true
	default:
		return false
	}
}

// terminateDeadStaleWorker is the liveness half of stale detection: a worker
// row that has gone silent AND whose recorded process no longer exists is
// marked failed rather than stalled, and it reports whether it did.
//
// The two conditions are one symptom with opposite remedies. 'stalled' is a
// mask and not an ending — MarkWorkerStalled saves the status underneath it and
// the recovery pass restores it when the log goes live again — so it is the
// right answer for a session that has stopped WRITING and the wrong one for a
// session that has stopped EXISTING: the writes that would clear the mask can
// only come from the process that is gone, so nothing ever moves the row again.
// Meanwhile it keeps counting against max_total_smiths
// (state.ActiveDispatchWorkers counts 'stalled'), which on a small forge is how
// one crashed session stops dispatch entirely with nothing in the log but the
// same "limit reached" line every poll.
//
// Every step that cannot answer leaves the ordinary stalled path in place,
// because the failure directions are not symmetric: a row wrongly left stalled
// is recovered by the operator or by the process resuming, while a live worker
// wrongly marked failed has its slot handed to a second dispatch for the same
// bead.
func (d *Daemon) terminateDeadStaleWorker(w state.Worker) bool {
	// No pid recorded is not evidence of a dead process: a 'pending' row stalls
	// on age before its dispatch ever spawned anything, and a row written by a
	// path that stamps the pid later has simply not been stamped yet. Nothing
	// is established, so the row is left to the stalled path — the plain
	// "unknown means unknown" reading, and the one that keeps this check from
	// failing every pending worker on a slow host.
	if w.PID <= 0 {
		return false
	}
	// A pid is only evidence about the phase that recorded it; everywhere else
	// the row carries a session that finished on purpose.
	if !pidNamesRunningSession(w.Phase) {
		return false
	}
	if processAlive(w.PID) {
		return false
	}

	// The decision is made in FailWorkerIfUnfinished's own WHERE clause rather
	// than from w.Status, which was read when StalledWorkers ran: between that
	// read and this write an ordinary path can land a terminal status, and a
	// row deliberately handed off live — monitoring to Bellows, paused for a
	// cold resume — must be left exactly as it is. A monitoring row is
	// precisely the case a pid test alone gets wrong, since its Smith process
	// is SUPPOSED to be gone.
	failed, err := d.db.FailWorkerIfUnfinished(w.ID)
	if err != nil {
		d.logger.Warn("could not mark a stale worker failed after finding its process gone; falling back to stalled",
			"worker", w.ID, "bead", w.BeadID, "anvil", w.Anvil, "pid", w.PID, "error", err)
		return false
	}
	if !failed {
		// The row is exempt (a monitoring handoff, an operator pause) or was
		// finalised between the two statements. Either way this pass has
		// nothing to say about it; fall through so an exempt row keeps the
		// behaviour it had before this check existed.
		return false
	}

	d.logger.Warn("marking worker as failed — its process is gone",
		"worker", w.ID, "bead", w.BeadID, "anvil", w.Anvil,
		"phase", w.Phase, "status", string(w.Status), "pid", w.PID)
	reason := fmt.Sprintf("Worker %s marked failed — process %d is no longer running", w.ID, w.PID)
	if err := d.db.LogEvent(state.EventWorkerProcessGone, reason, w.BeadID, w.Anvil); err != nil {
		d.logger.Warn("failed to log the process-gone event",
			"worker", w.ID, "bead", w.BeadID, "error", err)
	}
	return true
}
