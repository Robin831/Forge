package pipeline

import (
	"context"
	"strings"

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

// parkUntilResume blocks the pipeline goroutine on the park handle's resume
// signal after a pause. It returns the resume message (trimmed and defaulted to
// DefaultResumeMessage when the operator omitted one) and true once a resume
// arrives, or ("", false) when the context is cancelled (e.g. daemon shutdown)
// or the handle/channel is nil. On the false path the caller leaves the worker
// paused and exits without marking it failed.
func parkUntilResume(ctx context.Context, h ParkHandle) (string, bool) {
	if h == nil {
		return "", false
	}
	resumeCh := h.ResumeRequested()
	if resumeCh == nil {
		return "", false
	}
	select {
	case <-ctx.Done():
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
