package daemon

import "sync"

// steerMailboxSize bounds the buffered steer channel on each control handle.
// A small buffer lets the IPC/API layer enqueue a handful of steer messages
// without blocking even if the pipeline goroutine is momentarily busy between
// spawns; 16 is generous for interactive human-driven steering while keeping
// memory bounded. Sends beyond the buffer are dropped (non-blocking push).
const steerMailboxSize = 16

// controlHandle is the in-memory control surface for a single in-flight bead's
// pipeline. It is the shared foundation consumed by the RUNNING-spawn and
// BETWEEN-spawn steering features: the IPC/API layer looks up a handle by
// beadID, pushes a steer message into the mailbox, and/or triggers the
// interrupt to stop the currently running spawn.
//
// A handle is created with an immutable workerID and registered only after the
// pending worker row is inserted and the bead is successfully claimed — just
// before the dispatchBead goroutine launches. There is a brief window between
// activeBeads.LoadOrStore and handle registration where no handle exists; the
// IPC/API layer handles this gracefully (lookupControlHandle returns false).
// Deregistered via releaseBeadSlot (activeBeads.Delete first, then
// deregisterControlHandle) so the handle remains accessible for the full
// duration the bead is marked in-flight.
type controlHandle struct {
	// workerID is the DB worker row ID for the pipeline this handle controls.
	workerID string

	// steer is a buffered mailbox of steer messages destined for the running
	// (or next) spawn. Producers use pushSteer (non-blocking); consumers drain
	// it. Buffered so pushes don't block the IPC/API goroutine.
	steer chan string

	// mu guards interrupt, which is (re)wired as the pipeline enters and leaves
	// the phase where a spawn is actually cancellable.
	mu sync.Mutex

	// interrupt, when non-nil, stops the currently running spawn (typically a
	// context cancel func for the pipeline/smith subprocess). It is nil while no
	// spawn is cancellable (e.g. before the pipeline context is created).
	interrupt func()
}

// newControlHandle builds a handle for the given worker with an empty steer
// mailbox and no interrupt wired yet.
func newControlHandle(workerID string) *controlHandle {
	return &controlHandle{
		workerID: workerID,
		steer:    make(chan string, steerMailboxSize),
	}
}

// setInterrupt wires (or clears, with nil) the func that stops the currently
// running spawn. Safe to call concurrently with fireInterrupt.
func (h *controlHandle) setInterrupt(fn func()) {
	h.mu.Lock()
	h.interrupt = fn
	h.mu.Unlock()
}

// fireInterrupt invokes the wired interrupt func, if any. It returns true when
// an interrupt was actually triggered, false when none is currently wired.
func (h *controlHandle) fireInterrupt() bool {
	h.mu.Lock()
	fn := h.interrupt
	h.mu.Unlock()
	if fn == nil {
		return false
	}
	fn()
	return true
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
// handle for a bead. The two deletes are separate operations, not a single
// atomic update, but ordering is intentional: activeBeads.Delete first, then
// deregisterControlHandle, so the handle remains accessible for the full
// duration the bead is marked in-flight. Idempotent — safe to call even if no
// handle was registered (sync.Map.Delete is a no-op for absent keys).
func (d *Daemon) releaseBeadSlot(beadID string) {
	d.activeBeads.Delete(beadID)
	d.deregisterControlHandle(beadID)
}

// lookupControlHandle returns the control handle for a bead, if one is
// currently registered. This is the accessor the IPC/API sibling uses to reach
// a running pipeline's steer mailbox and interrupt.
func (d *Daemon) lookupControlHandle(beadID string) (*controlHandle, bool) {
	v, ok := d.controlHandles.Load(beadID)
	if !ok {
		return nil, false
	}
	return v.(*controlHandle), true
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

// triggerInterrupt is a convenience that looks up the handle for beadID and
// fires its interrupt. Returns false when no handle is registered or no
// interrupt is currently wired (no spawn is cancellable right now).
func (d *Daemon) triggerInterrupt(beadID string) bool {
	h, ok := d.lookupControlHandle(beadID)
	if !ok {
		return false
	}
	return h.fireInterrupt()
}
