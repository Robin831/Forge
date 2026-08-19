package daemon

import "github.com/Robin831/Forge/internal/poller"

// Reasons a dispatch bypasses every epic/crucible gate. They are strings rather
// than a bool so the daemon log names which bypass fired.
const (
	skipReasonIndependent = "independent"
	skipReasonResume      = "restart-resume"
)

// skipsEpicRouting reports whether a bead about to be dispatched goes straight
// to the normal pipeline — worktree from main, PR to main — without consulting
// any epic or Crucible gate, and why.
//
// Independent beads: either an operator ran this child standalone
// (ForceIndependent), or the bead carries the "independent" label, which carves
// it out of its parent's epic permanently. Both mean the same thing here, so
// both read the one predicate — poller.IsIndependentBead — rather than the
// dispatch path checking only the flag, which is json:"-" and therefore absent
// on any bead that came back through the queue cache.
//
// Restart-resume dispatches: a paused bead is mid-flow in a normal
// Smith→Temper→Warden loop, never an epic/crucible parent, and its worktree
// already exists.
//
// The independent check is deliberately first: a bead that is both is one an
// operator already routed away from its epic, and resuming it must not put it
// back.
func skipsEpicRouting(bead poller.Bead, resuming bool) (string, bool) {
	if poller.IsIndependentBead(bead) {
		return skipReasonIndependent, true
	}
	if resuming {
		return skipReasonResume, true
	}
	return "", false
}
