package selfdeploy

import "time"

// FailureReason classifies a self-deploy outcome that a human has to know
// about. The values are stable strings: they are persisted by the daemon and
// rendered in Hearth's Needs Attention list, so they must not be renamed
// casually (state.DeployReason* mirrors them).
type FailureReason string

const (
	// ReasonDrainTimeout: workers never drained, so the deploy was deferred.
	// Nothing was touched — the live binary is still the one that was running.
	ReasonDrainTimeout FailureReason = "drain_timeout"
	// ReasonSwapFailed: the new binary could not be moved into place. The
	// previous binary was restored, so the host still runs the old build.
	ReasonSwapFailed FailureReason = "swap_failed"
	// ReasonRestartFailed: the new binary was installed but the restart never
	// started. The previous binary is restored when one exists.
	ReasonRestartFailed FailureReason = "restart_failed"
	// ReasonRollbackFailed: the restart failed AND the previous binary could not
	// be put back. This is the worst state — the on-disk binary is the new,
	// never-started build while the running process is still the old one.
	ReasonRollbackFailed FailureReason = "rollback_failed"
	// ReasonPullBlocked: the fast-forward pull was refused by the checkout's own
	// state — a tree left mid-merge, a detached HEAD, a ref the pull cannot
	// lock. Nothing was touched; the live binary is still the one that was
	// running, and every later deploy fails the same way until an operator fixes
	// the checkout. It is escalated precisely because it does NOT clear on its
	// own: deferring quietly each time is how the running binary fell weeks
	// behind main with the evidence in the event log the whole time.
	ReasonPullBlocked FailureReason = "pull_blocked"
	// ReasonStashRetained: the deploy set local changes aside to fast-forward
	// and could not put them back, so they are in a stash it names rather than
	// in the working tree. Nothing was built or swapped. This is its own reason
	// rather than a flavour of ReasonPullBlocked because the remedy is unlike
	// any other deploy failure's — no later deploy restores those changes, and
	// the message is the only record of where they went.
	ReasonStashRetained FailureReason = "stash_retained"
)

// DeployEvent describes a self-deploy that ended somewhere other than "new
// binary live and restarting".
//
// It exists because the failure paths are otherwise silent: a rollback restores
// the previous binary and the daemon keeps running exactly as before, so the
// only evidence is a line in the event log. The result was that a merged fix
// could sit undeployed for days, discoverable only by diffing `forge version`
// against origin/main. Every field here answers one of the questions an
// operator asks at that point: what was I trying to run, what am I running now,
// why did it stop, and when did that happen.
type DeployEvent struct {
	// Reason classifies the failure. It is the only field consumers should
	// branch on.
	Reason FailureReason
	// AttemptedSHA is the commit the deploy tried to put live. Empty when the
	// SHA could not be resolved (it is diagnostic and never gates a deploy) or
	// when the deploy never got as far as pulling.
	AttemptedSHA string
	// RestoredSHA identifies the build that is live again after a rollback. It
	// is only meaningful when RolledBack is true, and may be empty when the
	// running build is unknown.
	RestoredSHA string
	// RolledBack reports whether the previous binary was actually put back. It
	// is false both when no rollback was needed (drain timeout) and when the
	// rollback itself failed (ReasonRollbackFailed).
	RolledBack bool
	// Detail is the human-readable failure text (the underlying error, or the
	// drain wait's summary of which workers held it up).
	Detail string
	// Unit is the systemd unit the deploy targeted.
	Unit string
	// BinaryPath is the live binary the deploy would have replaced.
	BinaryPath string
	// Timestamp is when the failure was observed, stamped from the Deployer's
	// clock so tests are deterministic.
	Timestamp time.Time
}

// Emitter turns deploy failures into operator-facing needs-attention items.
//
// Deliberately context-free, like Restarter: the emission points are the
// rollback and restart-failure paths, which run when the deploy context may
// already be cancelled (the daemon shutting itself down is how a restart is
// requested). Threading a context in would mean the one message an operator
// most needs to see is the one most likely to be dropped.
//
// Implementations must not block and must be safe to call from the deploy
// goroutine; errors are logged by the Deployer and never abort a rollback.
type Emitter interface {
	// EmitNeedsAttention records one needs-attention item for ev. It is called
	// once per failure MODE, not once per step: a restart failure that leads to
	// a rollback produces a single event. A deploy can legitimately raise two
	// where two things went wrong that need separate action — a pull blocked by
	// the checkout AND local changes it could not put back are different
	// problems with different remedies, and folding them into one row would
	// leave whichever was reported second invisible.
	EmitNeedsAttention(ev DeployEvent) error
	// ClearNeedsAttention resolves outstanding items for the given reasons, or
	// every reason when called with none. The Deployer calls it once the deploy
	// has progressed past the failure mode in question, so a deferral or a
	// rollback cannot linger in the list after a later deploy has superseded it.
	ClearNeedsAttention(reasons ...FailureReason) error
}
