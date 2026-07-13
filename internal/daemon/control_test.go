package daemon

import (
	"sync/atomic"
	"testing"
)

// TestControlHandleLifecycle exercises the full register → lookup → steer →
// interrupt → deregister cycle that the IPC/API steering features build on.
func TestControlHandleLifecycle(t *testing.T) {
	d := &Daemon{}
	const bead = "Forge-qchh"

	// Not registered yet: lookup and the convenience accessors report absence.
	if _, ok := d.lookupControlHandle(bead); ok {
		t.Fatal("expected no handle before registration")
	}
	if d.pushSteer(bead, "early") {
		t.Fatal("pushSteer should fail when no handle is registered")
	}
	if d.triggerInterrupt(bead) {
		t.Fatal("triggerInterrupt should fail when no handle is registered")
	}

	// Register a handle with an interrupt that flips a sentinel.
	var interrupted atomic.Bool
	h := newControlHandle("worker-1")
	h.setInterrupt(func() { interrupted.Store(true) })
	d.registerControlHandle(bead, h)

	got, ok := d.lookupControlHandle(bead)
	if !ok {
		t.Fatal("expected handle after registration")
	}
	if got != h {
		t.Fatal("lookup returned a different handle than was registered")
	}
	if got.workerID != "worker-1" {
		t.Fatalf("workerID = %q, want %q", got.workerID, "worker-1")
	}

	// Push a steer message and read it back off the mailbox.
	if !d.pushSteer(bead, "go left") {
		t.Fatal("pushSteer should succeed for a registered handle")
	}
	select {
	case msg := <-h.steer:
		if msg != "go left" {
			t.Fatalf("steer message = %q, want %q", msg, "go left")
		}
	default:
		t.Fatal("expected a steer message in the mailbox")
	}

	// Trigger the interrupt and verify the sentinel flipped.
	if !d.triggerInterrupt(bead) {
		t.Fatal("triggerInterrupt should succeed for a registered handle with an interrupt")
	}
	if !interrupted.Load() {
		t.Fatal("interrupt func was not invoked")
	}

	// Deregister and confirm lookup once again reports absence.
	d.deregisterControlHandle(bead)
	if _, ok := d.lookupControlHandle(bead); ok {
		t.Fatal("expected no handle after deregistration")
	}
}

// TestControlHandlePushSteerFullMailbox verifies pushSteer is non-blocking and
// reports false once the buffered mailbox is full instead of blocking.
func TestControlHandlePushSteerFullMailbox(t *testing.T) {
	h := newControlHandle("worker-2")
	for i := 0; i < steerMailboxSize; i++ {
		if !h.pushSteer("m") {
			t.Fatalf("push %d should succeed within buffer capacity", i)
		}
	}
	if h.pushSteer("overflow") {
		t.Fatal("pushSteer should return false when the mailbox is full")
	}
}

// TestReleaseBeadSlot verifies that releaseBeadSlot atomically removes both the
// activeBeads reservation and the control handle, and that the handle is still
// accessible while the bead is in activeBeads (i.e. activeBeads is deleted first).
func TestReleaseBeadSlot(t *testing.T) {
	d := &Daemon{}
	const bead = "Forge-release"

	d.activeBeads.Store(bead, true)
	h := newControlHandle("worker-r")
	d.registerControlHandle(bead, h)

	if _, ok := d.activeBeads.Load(bead); !ok {
		t.Fatal("expected bead in activeBeads before release")
	}
	if _, ok := d.lookupControlHandle(bead); !ok {
		t.Fatal("expected handle before release")
	}

	d.releaseBeadSlot(bead)

	if _, ok := d.activeBeads.Load(bead); ok {
		t.Fatal("expected bead removed from activeBeads after release")
	}
	if _, ok := d.lookupControlHandle(bead); ok {
		t.Fatal("expected handle removed after release")
	}
}

// TestControlHandleInterruptCleared verifies that clearing the interrupt (as the
// pipeline does when it returns) makes fireInterrupt a no-op returning false.
func TestControlHandleInterruptCleared(t *testing.T) {
	h := newControlHandle("worker-3")
	if h.fireInterrupt() {
		t.Fatal("fireInterrupt should return false when no interrupt is wired")
	}

	var called atomic.Bool
	h.setInterrupt(func() { called.Store(true) })
	h.setInterrupt(nil) // pipeline returned; interrupt cleared

	if h.fireInterrupt() {
		t.Fatal("fireInterrupt should return false after the interrupt is cleared")
	}
	if called.Load() {
		t.Fatal("cleared interrupt func must not be invoked")
	}
}
