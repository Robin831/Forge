package daemon

import (
	"fmt"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
)

// PauseReason identifies what caused the daemon-wide dispatch pause. It is a
// string rather than an int so an unknown value round-trips through the IPC
// status payload intact and can be rendered verbatim by an older client.
type PauseReason string

const (
	// PauseReasonNone is the reason of an unpaused daemon.
	PauseReasonNone PauseReason = ""
	// PauseReasonManual is an operator pause (pause_dispatch / forge pause).
	PauseReasonManual PauseReason = ipc.PauseReasonManual
	// PauseReasonSelfDeploy is the transient pause runSelfDeploy takes while it
	// drains active workers before rebuilding and restarting the daemon. It is
	// deliberately never persisted: a daemon that restarts mid-deploy must come
	// back unpaused rather than inherit a drain that is no longer running.
	PauseReasonSelfDeploy PauseReason = ipc.PauseReasonSelfDeploy
)

// pauseState is the atomic snapshot of the dispatch pause switch. Flag and
// reason live in one immutable value so a reader can never observe a pause with
// a stale (or missing) reason, which is exactly the bug that made a self-deploy
// drain read as "PAUSED (manual)".
type pauseState struct {
	// Paused suspends new dispatch. Running workers are untouched and finish
	// normally; manual `forge queue run` dispatch remains allowed.
	Paused bool
	// Reason is why dispatch is paused. Always non-empty when Paused is true.
	Reason PauseReason
	// Detail is optional reason-specific context for the status line, e.g. the
	// drain budget of a self-deploy pause.
	Detail string
}

// setDispatchPaused replaces the dispatch pause state. A pause is always given
// a reason: an empty one is normalized to manual, matching how a persisted
// pause written by an older Forge is interpreted on restore. Resuming clears
// the reason and detail along with the flag.
func (d *Daemon) setDispatchPaused(paused bool, reason PauseReason, detail string) {
	if !paused {
		reason, detail = PauseReasonNone, ""
	} else if reason == PauseReasonNone {
		reason = PauseReasonManual
	}
	d.dispatchPause.Store(&pauseState{Paused: paused, Reason: reason, Detail: detail})
}

// dispatchPauseState returns the current pause snapshot. The zero value (never
// paused since startup) reads as unpaused.
func (d *Daemon) dispatchPauseState() pauseState {
	if ps := d.dispatchPause.Load(); ps != nil {
		return *ps
	}
	return pauseState{}
}

// dispatchIsPaused reports whether new dispatch is currently suspended.
func (d *Daemon) dispatchIsPaused() bool {
	return d.dispatchPauseState().Paused
}

// pauseForSelfDeploy pauses dispatch for a self-deploy's bounded drain and
// returns the function that undoes it. maxDrainWait is recorded as the pause
// detail so status can say how long the drain may last.
//
// The returned restore takes whether the restart has already been requested.
// Two exemptions are deliberate:
//   - a pause that predates the deploy belongs to the operator, so it stays —
//     and is restored with its own reason, so a manual pause is never
//     relabelled as a self-deploy drain (or the reverse);
//   - once the restart has been requested the binary swap has already happened,
//     and resuming would let a worker start in the seconds before systemd stops
//     the process.
func (d *Daemon) pauseForSelfDeploy(maxDrainWait time.Duration) func(restartRequested bool) {
	wasPaused := d.dispatchPauseState()
	d.setDispatchPaused(true, PauseReasonSelfDeploy, fmt.Sprintf("max %s", maxDrainWait))
	return func(restartRequested bool) {
		switch {
		case restartRequested:
			return
		case wasPaused.Paused:
			d.setDispatchPaused(true, wasPaused.Reason, wasPaused.Detail)
		default:
			d.setDispatchPaused(false, PauseReasonNone, "")
		}
	}
}

// pluralS returns the plural suffix for a count, for status strings like
// "waiting on 2 workers".
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
