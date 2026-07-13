package pipeline

import (
	"context"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/provider"
)

// DefaultResumeMessage is the prompt delivered to a parked bead's resumed Claude
// session when the operator supplies no explicit resume message. The pause/park/
// resume mechanic reuses the steer resume path, so a resume-with-message is free;
// this constant is the neutral default when the caller omits one.
//
// The daemon IPC layer applies the same default when accepting a resume_bead
// request; this pipeline-side copy is a defensive fallback so a parked pipeline
// never resumes with an empty prompt regardless of how the resume signal was
// delivered.
const DefaultResumeMessage = "Continue with the task."

// ParkRecord captures the state a paused pipeline needs to resume mid-flow. It is
// the authoritative record type owned by this package and consumed by downstream
// pause/resume sub-tasks (e.g. persistence and UI): when a pause interrupts an
// in-flight Smith spawn, the pipeline records the interrupted session and its
// loop position here so a later resume can respawn `claude --resume <SessionID>`
// and continue the pipeline loop from where it parked rather than from the top.
type ParkRecord struct {
	// SessionID is the Claude session_id captured from the spawn that the pause
	// interrupted. Resuming replays `claude --resume <SessionID>` so the model
	// picks up its prior context. Empty when the provider reported no session
	// (e.g. a non-Claude provider); the resume then folds the message into a
	// fresh prompt instead.
	SessionID string

	// Iteration is the pipeline loop iteration that was in flight when the pause
	// fired. It records where in the Smith→Temper→Warden loop the pipeline was
	// parked so a resume continues mid-flow.
	Iteration int

	// Provider is the AI provider that owns SessionID. A provider session is
	// provider-bound, so the resume must respawn with this exact provider even
	// if the pipeline's active provider index has since advanced to a fallback.
	Provider provider.Provider
}

// ResumeSession seeds a pipeline to re-enter a bead that was paused before a
// daemon restart. Unlike ParkRecord (which the running pipeline builds when a
// live pause fires), a ResumeSession is reconstructed by the daemon from the
// persisted paused worker row (session_id + model) and passed into Params so the
// pipeline resumes the recorded Claude session on its first iteration, reusing
// the retained worktree, rather than starting fresh. It reuses the same steer
// respawn machinery as an in-process resume.
type ResumeSession struct {
	// SessionID is the persisted Claude session_id to resume via
	// `claude --resume <SessionID>`. Empty means no session was captured (e.g. a
	// non-Claude provider); the pipeline then folds Message into a fresh prompt.
	SessionID string

	// Provider is the AI provider that owns SessionID. A provider session is
	// provider-bound, so the resume must respawn with this exact provider.
	Provider provider.Provider

	// Message is the resume prompt delivered to the resumed session. Empty is
	// substituted with DefaultResumeMessage.
	Message string
}

// ParkHandle is the registry-handle contract between the daemon's control
// registry and the pipeline goroutine for the pause/park/resume mechanic. It is
// the authoritative contract owned by this package: the daemon's per-bead control
// handle satisfies it, and downstream sub-tasks build on it.
//
// The pipeline observes two edges through this handle:
//   - PauseRequested fires when an operator asks the running spawn to be parked.
//     The pipeline gracefully group-SIGINTs the current spawn (reusing the steer
//     interrupt path, with NO failure marking), records a ParkRecord, marks the
//     worker paused, and blocks on ResumeRequested.
//   - ResumeRequested delivers the resume message (already defaulted to
//     DefaultResumeMessage by the IPC layer when omitted). Receiving it unblocks
//     the parked pipeline, which respawns the recorded session and continues.
//
// A nil ParkHandle (or nil channels) disables the mechanic: the pipeline runs
// exactly as it did before, never parking. The interrupt itself is owned by the
// pipeline (it already holds the spawn), so the handle only carries the pause and
// resume signals.
type ParkHandle interface {
	// PauseRequested returns the channel that signals a pause request. A receive
	// means the operator asked to park the running spawn.
	PauseRequested() <-chan struct{}

	// ResumeRequested returns the channel delivering the resume message for a
	// parked pipeline. The message may be empty; the pipeline substitutes
	// DefaultResumeMessage in that case.
	ResumeRequested() <-chan string
}

// timeoutGate owns the pipeline's smith-timeout context and lets its deadline be
// rebuilt (extended) after a pause without leaking the previous cancel func. The
// cancel of each derived context is stored on the struct and released when the
// next deadline is set or when close() runs, so exactly one live timeout context
// exists at a time.
type timeoutGate struct {
	base   context.Context
	cancel context.CancelFunc
}

// set cancels the previous timeout context (if any) and derives a fresh one from
// the base context with the given deadline, returning it for the pipeline to use.
func (g *timeoutGate) set(deadline time.Time) context.Context {
	if g.cancel != nil {
		g.cancel()
	}
	ctx, cancel := context.WithDeadline(g.base, deadline)
	g.cancel = cancel
	return ctx
}

// close releases the current timeout context. Safe to call when none is set.
func (g *timeoutGate) close() {
	if g.cancel != nil {
		g.cancel()
	}
}

// pauseClock accumulates the wall-clock time a pipeline spends parked across one
// or more pause/resume cycles so the smith-timeout deadline can be advanced by
// exactly that amount ("pausing the deadline"). It is deliberately tiny and pure
// (no time.Now inside) so the timeout-extension math is unit-testable: callers
// feed it measured pause durations and ask it to project an extended deadline
// from the pipeline's original deadline.
type pauseClock struct {
	// total is the sum of all completed pause durations. Non-positive additions
	// are ignored so a clock skew (resume timestamp before pause timestamp) can
	// never shorten the budget.
	total time.Duration
}

// add records a single completed pause of duration d. Non-positive durations are
// ignored.
func (c *pauseClock) add(d time.Duration) {
	if d > 0 {
		c.total += d
	}
}

// Total returns the accumulated paused duration recorded so far.
func (c *pauseClock) Total() time.Duration { return c.total }

// extend projects the extended deadline from the pipeline's original deadline by
// adding the total accumulated pause. Recomputing from the fixed original
// deadline (rather than mutating a running deadline) keeps the operation
// idempotent across repeated resumes. A zero original deadline (no smith timeout
// in effect) is returned unchanged.
func (c *pauseClock) extend(original time.Time) time.Time {
	return extendDeadlineForPause(original, c.total)
}

// extendDeadlineForPause advances a smith-timeout deadline by the wall-clock time
// a pipeline spent parked, so time spent paused does not count against the smith
// timeout budget. It is the pure, unit-testable core of the "pause the deadline"
// behavior:
//
//   - A zero deadline means no timeout is in effect and is returned unchanged.
//   - A non-positive pausedFor cannot shorten the budget and is a no-op.
//   - Otherwise the deadline moves forward by exactly pausedFor.
func extendDeadlineForPause(deadline time.Time, pausedFor time.Duration) time.Time {
	if deadline.IsZero() || pausedFor <= 0 {
		return deadline
	}
	return deadline.Add(pausedFor)
}

// parkUntilResume blocks the pipeline goroutine on the park handle's resume
// signal after a pause. It returns the resume message (trimmed and defaulted to
// DefaultResumeMessage when the operator omitted one) and true once a resume
// arrives, or ("", false) when either context is cancelled or the handle/channel
// is nil. On the false path the caller leaves the worker paused and exits without
// marking it failed.
//
// Two contexts are watched so a parked pipeline unblocks promptly on daemon
// shutdown WITHOUT the smith timeout ever tripping the park:
//   - ctx is the pipeline's cancellable base (IPC stop/interrupt). It carries NO
//     smith deadline, so a long pause cannot expire it.
//   - shutdownCtx signals daemon shutdown. It lets the drain path unblock a
//     parked goroutine that would otherwise wait indefinitely on ctx. A nil
//     shutdownCtx disables this second edge (used by tests/callers without one).
func parkUntilResume(ctx, shutdownCtx context.Context, h ParkHandle) (string, bool) {
	if h == nil {
		return "", false
	}
	resumeCh := h.ResumeRequested()
	if resumeCh == nil {
		return "", false
	}
	var shutdownDone <-chan struct{}
	if shutdownCtx != nil {
		shutdownDone = shutdownCtx.Done()
	}
	select {
	case <-ctx.Done():
		return "", false
	case <-shutdownDone:
		return "", false
	case msg, ok := <-resumeCh:
		if !ok {
			return "", false
		}
		msg = strings.TrimSpace(msg)
		if msg == "" {
			msg = DefaultResumeMessage
		}
		return msg, true
	}
}
