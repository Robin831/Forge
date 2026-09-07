package daemon

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Robin831/Forge/internal/state"
)

// pidNamesRunningSession reports whether a worker row in this phase records the
// pid of the session that phase is CURRENTLY running — the one question that
// makes a dead pid evidence of anything.
//
// The claim is about the PHASE and not about which call sites write the column:
// several do (the pipeline's schematic and Smith spawns, the daemon's own
// schematic check, the Crucible, and the quench/burnish/rebase fix workers),
// and what they have in common is that each stamps the pid of the session its
// own phase is running. The column is never cleared, so a row that has moved on
// to `temper` or `warden` still carries the pid of the Smith that finished
// before it — a dead process by design, exactly as a `monitoring` row's is.
// Reading THAT as an abandoned worker would fail every pipeline whose test
// suite outruns stale_interval, and — because 'failed' is terminal where
// 'stalled' is not — hand its max_total_smiths slot to a second dispatch while
// it was still working.
//
// So the check is confined to the two phases whose pid names the session they
// are in, which are also the ones the leak is about: they hold a dispatch slot.
// The lifecycle phases record a Smith pid of their own but hold no slot, and
// they keep running past their session (verify, push, resolve threads), which
// is the same window `temper` is — a dead pid there would be read the same
// wrong way.
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

// deadPIDWatch remembers, from one stale-detection pass to the next, which
// worker rows were seen naming a process that no longer exists.
//
// It exists because a row can legitimately read as "phase smith, pid dead" for
// a moment while it is entirely healthy: the pipeline flips the phase to
// `smith` BEFORE the spawn it is about to make writes its pid, so on every
// review iteration past the first — and on every resume — the row carries the
// previous, already-exited Smith's pid for the width of a process launch, with
// a log that a long temper/warden phase has already left stale. A detector tick
// landing in that window sees the exact condition this feature fails a worker
// on, and failing is terminal: the live pipeline's row is ended underneath it
// and its dispatch slot handed to a second worker for the same bead.
//
// Requiring the SAME dead pid on two consecutive passes closes it without the
// detector having to know anything about pipeline internals. The window is one
// spawn wide and the passes are stale_interval/2 apart at their fastest, so a
// genuinely dead session is failed one pass later than before (still within a
// detector interval or two of the crash) while a row in the pre-spawn window is
// spared: by the next pass it names the pid the spawn wrote, and a different
// pid starts the count again rather than confirming the old one.
//
// The zero value is usable; the map is created on first use.
type deadPIDWatch struct {
	mu sync.Mutex
	// seen holds, per worker id, the dead pid observed on the previous pass. An
	// entry is dropped rather than kept whenever the row stops presenting the
	// condition — its pid reads alive, it names a different pid, or it has left
	// the stale set altogether — so the two sightings are consecutive
	// observations of one condition and not two unrelated ones.
	seen map[string]int
}

// confirm records that worker id was observed naming the dead pid, and reports
// whether the same pid was already recorded on the previous pass.
func (w *deadPIDWatch) confirm(id string, pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = make(map[string]int)
	}
	prev, ok := w.seen[id]
	w.seen[id] = pid
	return ok && prev == pid
}

// forget drops a worker's sighting. Called for every row this pass did not find
// dead, so a single confirmation can never be assembled out of two sightings
// with a healthy pass between them.
func (w *deadPIDWatch) forget(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, id)
}

// retain drops the sightings of every worker outside the given set — the rows
// that are no longer stale at all, and the ones this pass has just failed.
// Without it the map grows with every worker the daemon ever runs, and a row
// that goes quiet again much later would be failed on its first sighting on the
// strength of an observation from another episode.
func (w *deadPIDWatch) retain(ids map[string]struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id := range w.seen {
		if _, keep := ids[id]; !keep {
			delete(w.seen, id)
		}
	}
}

// presentsDeadPID reports whether a worker row currently shows the condition
// the liveness check acts on — a phase whose pid names the session it is in,
// and a process that no longer exists — and keeps the watch in step with the
// answer: anything that is not that condition drops the row's sighting, so two
// sightings can never be assembled across a pass that saw something else.
//
// Every step that cannot answer says no, because the failure directions are not
// symmetric: a row wrongly left stalled is recovered by the operator or by the
// process resuming, while a live worker wrongly marked failed has its slot
// handed to a second dispatch for the same bead.
func (d *Daemon) presentsDeadPID(w state.Worker) bool {
	// No pid recorded is not evidence of a dead process: a 'pending' row stalls
	// on age before its dispatch ever spawned anything, and a row written by a
	// path that stamps the pid later has simply not been stamped yet. Nothing
	// is established, so the row is left to the stalled path — the plain
	// "unknown means unknown" reading, and the one that keeps this check from
	// failing every pending worker on a slow host.
	if w.PID <= 0 {
		d.deadPIDs.forget(w.ID)
		return false
	}
	// A pid is only evidence about the phase that recorded it; everywhere else
	// the row carries a session that finished on purpose.
	if !pidNamesRunningSession(w.Phase) {
		d.deadPIDs.forget(w.ID)
		return false
	}
	if processAlive(w.PID) {
		d.deadPIDs.forget(w.ID)
		return false
	}
	return true
}

// noteStaleWorkerLiveness is the first half of the two-pass rule: it records
// what this pass sees of a newly-silent worker's process and acts on none of
// it. A row that has only just gone silent gets the recoverable 'stalled' mask
// whatever its pid says, because THIS is the tick a healthy pipeline's
// pre-spawn window lands on; only a later pass, over a row that is still
// stalled and still names the same dead pid, may end it.
func (d *Daemon) noteStaleWorkerLiveness(w state.Worker) {
	if !d.presentsDeadPID(w) {
		return
	}
	d.deadPIDs.confirm(w.ID, w.PID)
	d.logger.Info("a newly stalled worker's recorded process is gone; confirming on the next detector pass before failing it",
		"worker", w.ID, "bead", w.BeadID, "anvil", w.Anvil,
		"phase", w.Phase, "pid", w.PID)
}

// terminateDeadStaleWorker is the liveness half of stale detection: a worker
// row that has gone silent AND whose recorded process no longer exists — on two
// consecutive passes — is marked failed rather than stalled, and it reports
// whether it did.
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
	if !d.presentsDeadPID(w) {
		return false
	}
	// One sighting is not the condition: the pipeline sets phase=smith before
	// the spawn that will write the new pid, so a healthy row in that window
	// presents exactly this. The second consecutive sighting of the SAME pid on
	// a LATER pass is what tells a launch in progress from a session that is
	// gone.
	if !d.deadPIDs.confirm(w.ID, w.PID) {
		d.logger.Info("a stale worker's recorded process is gone; confirming on the next detector pass before failing it",
			"worker", w.ID, "bead", w.BeadID, "anvil", w.Anvil,
			"phase", w.Phase, "pid", w.PID)
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
