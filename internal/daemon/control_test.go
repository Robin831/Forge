package daemon

import (
	"testing"
)

// TestControlHandleLifecycle exercises the full register → lookup → steer →
// live-spawn → deregister cycle that the IPC/API steering features build on.
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

	h := newControlHandle("worker-1")
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

	// A fresh handle has no live spawn (mode B).
	if got.hasLiveSpawn() {
		t.Fatal("expected no live spawn on a fresh handle")
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

	// Marking a spawn live flips the mode-A indicator; clearing restores mode B.
	got.setLiveSpawn(true)
	if !got.hasLiveSpawn() {
		t.Fatal("expected a live spawn after setLiveSpawn(true)")
	}
	got.setLiveSpawn(false)
	if got.hasLiveSpawn() {
		t.Fatal("expected no live spawn after setLiveSpawn(false)")
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

// TestReleaseBeadSlot verifies that releaseBeadSlot removes both the
// control handle and the activeBeads reservation.
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

// TestReleaseBeadSlotIfOwner verifies that releaseBeadSlotIfOwner only removes
// the slot when the stored handle matches, preventing a finishing goroutine from
// deleting a handle registered by a new dispatch.
func TestReleaseBeadSlotIfOwner(t *testing.T) {
	d := &Daemon{}
	const bead = "Forge-owner"

	h1 := newControlHandle("worker-old")
	d.activeBeads.Store(bead, true)
	d.registerControlHandle(bead, h1)

	// Simulate stop_bead releasing and a new dispatch registering h2.
	d.releaseBeadSlot(bead)
	h2 := newControlHandle("worker-new")
	d.activeBeads.Store(bead, true)
	d.registerControlHandle(bead, h2)

	// Old goroutine's deferred cleanup: must NOT remove h2.
	d.releaseBeadSlotIfOwner(bead, h1)

	if _, ok := d.lookupControlHandle(bead); !ok {
		t.Fatal("new handle should survive old goroutine's conditional cleanup")
	}
	if _, ok := d.activeBeads.Load(bead); !ok {
		t.Fatal("activeBeads slot should survive old goroutine's conditional cleanup")
	}

	// Owner cleanup should work when the handle matches.
	d.releaseBeadSlotIfOwner(bead, h2)
	if _, ok := d.lookupControlHandle(bead); ok {
		t.Fatal("handle should be removed when owner matches")
	}
	if _, ok := d.activeBeads.Load(bead); ok {
		t.Fatal("activeBeads should be removed when owner matches")
	}
}

// TestControlHandleLiveSpawn verifies the live-spawn indicator that
// handleSteerBead uses to label a steer mode A (live spawn) vs mode B (queued).
// A fresh handle reports no live spawn; the flag tracks setLiveSpawn.
func TestControlHandleLiveSpawn(t *testing.T) {
	h := newControlHandle("worker-3")
	if h.hasLiveSpawn() {
		t.Fatal("a fresh handle must report no live spawn (mode B)")
	}

	h.setLiveSpawn(true)
	if !h.hasLiveSpawn() {
		t.Fatal("hasLiveSpawn should report true while a spawn is live (mode A)")
	}

	h.setLiveSpawn(false) // pipeline left the spawn wait window
	if h.hasLiveSpawn() {
		t.Fatal("hasLiveSpawn should report false once the spawn wait returns")
	}
}
