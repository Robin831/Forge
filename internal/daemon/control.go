package daemon

import (
	"sync/atomic"

	"github.com/Robin831/Forge/internal/pipeline"
)

// controlHandle satisfies the pipeline.ParkHandle contract so the pipeline
// goroutine can observe pause/resume requests for the bead it controls.
var _ pipeline.ParkHandle = (*controlHandle)(nil)

// steerMailboxSize bounds the buffered steer channel on each control handle.
// A small buffer lets the IPC/API layer enqueue a handful of steer messages
// without blocking even if the pipeline goroutine is momentarily busy between
// spawns; 16 is generous for interactive human-driven steering while keeping
// memory bounded. Sends beyond the buffer are dropped (non-blocking push).
const steerMailboxSize = 16

// DefaultResumeMessage is the prompt delivered to a paused bead's resumed Claude
// session when the human supplies no explicit resume message. The pause/resume
// mechanic reuses the steer resume path, so a resume-with-message is free; this
// constant provides the neutral default when the caller omits one.
const DefaultResumeMessage = "Continue with the task."

// controlHandle is the in-memory control surface for a single in-flight bead's
// pipeline. It is the shared foundation consumed by the RUNNING-spawn and
// BETWEEN-spawn steering features: the IPC/API layer looks up a handle by
// beadID and pushes a steer message into the mailbox. The pipeline goroutine is
// the sole consumer of the mailbox — it interrupts the currently running spawn
// itself (gracefully, via waitSmithWithSteer) without any pipeline-wide cancel,
// so a steer never tears down the pipeline context.
//
// A handle is created with an immutable workerID and registered only after the
// pending worker row is inserted and the bead is successfully claimed — just
// before the dispatchBead goroutine launches. There is a brief window between
// activeBeads.LoadOrStore and handle registration where no handle exists; the
// IPC/API layer handles this gracefully (lookupControlHandle returns false).
// Deregistered via releaseBeadSlot (deregisterControlHandle first, then
// activeBeads.Delete) so a new dispatch cannot register a handle that the old
// goroutine would then delete.
type controlHandle struct {
	// workerID is the DB worker row ID for the pipeline this handle controls.
	workerID string

	// steer is a buffered mailbox of steer messages destined for the running
	// (or next) spawn. Producers use pushSteer (non-blocking); consumers drain
	// it. Buffered so pushes don't block the IPC/API goroutine.
	steer chan string

	// liveSpawn is true while a Smith spawn is actively running and therefore
	// interruptible by a steer message (mode A). The pipeline goroutine flips it
	// via setLiveSpawn around each spawn's wait window; the IPC/API layer reads
	// it via hasLiveSpawn to label an incoming steer as mode A (interrupting a
	// live spawn) vs mode B (queued for the next spawn). Atomic so the reader
	// never blocks the pipeline goroutine.
	liveSpawn atomic.Bool

	// pause is a single-slot signal that a human requested the running spawn be
	// parked. The IPC/API layer pushes into it (non-blocking, via requestPause)
	// after validating the worker is in a pausable (running) state; the pipeline
	// goroutine selects on it alongside the running spawn to trigger the graceful
	// interrupt + park. Buffered to 1: a pause is a latching request, so a second
	// press while one is already pending is reported to the caller rather than
	// silently coalesced.
	pause chan struct{}

	// resume is a single-slot mailbox carrying the resume message a human supplied
	// (defaulting to DefaultResumeMessage). A pipeline goroutine parked after a
	// pause blocks on it to respawn `claude --resume <session>` with the message
	// as the new prompt and continue the pipeline loop.
	resume chan string
}

// newControlHandle builds a handle for the given worker with an empty steer
// mailbox and no interrupt wired yet.
func newControlHandle(workerID string) *controlHandle {
	return &controlHandle{
		workerID: workerID,
		steer:    make(chan string, steerMailboxSize),
		pause:    make(chan struct{}, 1),
		resume:   make(chan string, 1),
	}
}

// setLiveSpawn records whether a Smith spawn is currently running and therefore
// interruptible by a steer message. The pipeline goroutine calls it with true
// just before it begins waiting on a spawn and false once that wait returns.
// Safe to call concurrently with hasLiveSpawn.
func (h *controlHandle) setLiveSpawn(live bool) {
	h.liveSpawn.Store(live)
}

// hasLiveSpawn reports whether a Smith spawn is currently running. The IPC/API
// layer uses it to label an incoming steer as mode A (a live spawn will be
// interrupted and resumed) vs mode B (queued for the next spawn).
func (h *controlHandle) hasLiveSpawn() bool {
	return h.liveSpawn.Load()
}

// pushSteer enqueues a steer message without blocking. It returns false when
// the mailbox is full (the message is dropped rather than blocking the caller).
func (h *controlHandle) pushSteer(msg string) bool {
	select {
	case h.steer <- msg:
		return true
	default:
		return false
	}
}

// requestPause signals a pause request without blocking. It returns false when a
// pause is already pending (the single buffered slot is full), so the caller can
// report that a pause is already in flight rather than silently coalescing.
func (h *controlHandle) requestPause() bool {
	select {
	case h.pause <- struct{}{}:
		return true
	default:
		return false
	}
}

// requestResume delivers the resume message to a parked pipeline goroutine
// without blocking. It returns false when a resume is already pending (the
// single buffered slot is full).
func (h *controlHandle) requestResume(msg string) bool {
	select {
	case h.resume <- msg:
		return true
	default:
		return false
	}
}

// PauseRequested returns the channel that signals a pause request for this
// bead's pipeline. It satisfies the pipeline.ParkHandle contract: the pipeline
// goroutine selects on it (alongside the running spawn) to trigger the graceful
// interrupt + park.
func (h *controlHandle) PauseRequested() <-chan struct{} {
	return h.pause
}

// ResumeRequested returns the channel delivering the resume message to a parked
// pipeline. It satisfies the pipeline.ParkHandle contract: a pipeline goroutine
// parked after a pause blocks on it to respawn `claude --resume <session>`.
func (h *controlHandle) ResumeRequested() <-chan string {
	return h.resume
}

// registerControlHandle records the control handle for a bead so the IPC/API
// layer can look it up by beadID. Concurrency-safe via sync.Map.
func (d *Daemon) registerControlHandle(beadID string, h *controlHandle) {
	d.controlHandles.Store(beadID, h)
}

// deregisterControlHandle removes the control handle for a bead. Prefer
// releaseBeadSlot which pairs this with activeBeads.Delete in the correct order.
func (d *Daemon) deregisterControlHandle(beadID string) {
	d.controlHandles.Delete(beadID)
}

// releaseBeadSlot removes both the activeBeads reservation and the control
// handle for a bead unconditionally. Use this only in pre-dispatch error paths
// where no goroutine owns the slot yet. For goroutine cleanup or IPC stop
// handlers where a re-dispatch race is possible, use releaseBeadSlotIfOwner.
func (d *Daemon) releaseBeadSlot(beadID string) {
	d.deregisterControlHandle(beadID)
	d.activeBeads.Delete(beadID)
}

// releaseBeadSlotIfOwner removes the control handle and activeBeads reservation
// only if the currently stored handle is expectedCtrl. This prevents a finishing
// goroutine's deferred cleanup from deleting a handle registered by a new
// dispatch after re-dispatch.
func (d *Daemon) releaseBeadSlotIfOwner(beadID string, expectedCtrl *controlHandle) {
	if d.controlHandles.CompareAndDelete(beadID, expectedCtrl) {
		d.activeBeads.Delete(beadID)
	}
}

// releaseStoppedBeadSlot releases the in-memory bead slot from an IPC stop
// handler. It uses releaseBeadSlotIfOwner when a control handle is currently
// registered — so a re-dispatch that raced with the stop keeps its freshly
// registered handle — and falls back to the unconditional releaseBeadSlot when
// no handle is present. Shared by the stop_bead and queue_stop verbs so both
// stop paths release the slot identically.
func (d *Daemon) releaseStoppedBeadSlot(beadID string) {
	if ctrl, ok := d.lookupControlHandle(beadID); ok {
		d.releaseBeadSlotIfOwner(beadID, ctrl)
	} else {
		d.releaseBeadSlot(beadID)
	}
}

// lookupControlHandle returns the control handle for a bead, if one is
// currently registered. This is the accessor the IPC/API sibling uses to reach
// a running pipeline's steer mailbox and interrupt.
func (d *Daemon) lookupControlHandle(beadID string) (*controlHandle, bool) {
	v, ok := d.controlHandles.Load(beadID)
	if !ok {
		return nil, false
	}
	h, ok := v.(*controlHandle)
	if !ok {
		d.controlHandles.CompareAndDelete(beadID, v)
		return nil, false
	}
	return h, true
}

// pushSteer is a convenience that looks up the handle for beadID and pushes a
// steer message into its mailbox. Returns false when no handle is registered or
// the mailbox is full.
func (d *Daemon) pushSteer(beadID, msg string) bool {
	h, ok := d.lookupControlHandle(beadID)
	if !ok {
		return false
	}
	return h.pushSteer(msg)
}
