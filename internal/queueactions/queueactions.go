// Package queueactions hosts the shared business logic behind the queue
// resolution verbs: clarify, unclarify, retry, clear, stop.
//
// These functions are the single source of truth for the state mutations and
// audit-trail entries that back the CLI verbs (forge queue clarify, etc.) and
// the IPC handlers (queue_clarify, queue_unclarify, queue_retry, queue_clear,
// queue_stop). Daemon-specific orchestration that surrounds each action —
// shelling out to `bd`, refreshing in-memory caches, kicking the poller — is
// not part of this package; callers compose those on top of the action.
//
// Multi-forge safety is enforced inside each function: if the caller supplies
// a forge_id that does not match the QueueHandle's local forge_id, the action
// is refused with ErrForgeMismatch. A caller that omits forge_id retains the
// historical single-forge behaviour.
package queueactions

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/Robin831/Forge/internal/state"
)

// Params bundles the inputs common to all queue resolution actions. AnvilName
// is canonicalised by the caller before invocation. Note is the operator's
// free-form explanation, captured in the audit event.
type Params struct {
	BeadID    string
	ForgeID   string
	AnvilName string
	Note      string
}

// QueueHandle abstracts the daemon state operations that the shared actions
// rely on. The daemon implements this directly against state.DB; tests pass
// in a fake to verify behaviour without a real database.
type QueueHandle interface {
	// LocalForgeID returns the forge_id of the Forge instance handling the
	// request. Compared against Params.ForgeID for the multi-forge safety
	// check.
	LocalForgeID() string

	SetClarificationNeeded(beadID, anvil string, needed bool, reason string) error
	ClearNeedsAttention(beadID, anvil string) error
	GetRetry(beadID, anvil string) (*state.RetryRecord, error)
	ResetRetry(beadID, anvil string) error

	ActiveWorkerByBeadAndAnvil(beadID, anvil string) (*state.Worker, error)
	// PausedWorkerByBeadAndAnvil returns the worker parked by an operator pause
	// for a bead, or (nil, nil) when none is paused. Stop uses it so discarding a
	// paused bead (including one whose parked goroutine did not survive a daemon
	// restart) terminates the retained worker row and releases its worktree.
	PausedWorkerByBeadAndAnvil(beadID, anvil string) (*state.Worker, error)
	UpdateWorkerStatus(workerID string, status state.WorkerStatus) error

	LogEvent(typ state.EventType, message, beadID, anvil string) error
}

// Clarify marks a bead as needing human clarification before further dispatch.
// Mirrors the legacy set_clarification IPC handler. The note is required and
// captured both as the bead's clarification reason and in the audit event.
func Clarify(ctx context.Context, q QueueHandle, p Params) error {
	if err := validateCommon(p); err != nil {
		return err
	}
	reason := strings.TrimSpace(p.Note)
	if reason == "" {
		return ErrMissingReason
	}
	if err := checkForge(q, p); err != nil {
		return err
	}
	if err := q.SetClarificationNeeded(p.BeadID, p.AnvilName, true, reason); err != nil {
		return fmt.Errorf("set clarification: %w", err)
	}
	msg := fmt.Sprintf("Bead %s needs clarification: %s", p.BeadID, reason)
	if err := q.LogEvent(state.EventClarificationNeeded, msg, p.BeadID, p.AnvilName); err != nil {
		return fmt.Errorf("log event: %w", err)
	}
	return nil
}

// Unclarify clears the clarification_needed flag so a bead can be dispatched
// again. The note is optional; when supplied it is appended to the audit
// event so operators can see why the flag was lifted.
func Unclarify(ctx context.Context, q QueueHandle, p Params) error {
	if err := validateCommon(p); err != nil {
		return err
	}
	if err := checkForge(q, p); err != nil {
		return err
	}
	if err := q.SetClarificationNeeded(p.BeadID, p.AnvilName, false, ""); err != nil {
		return fmt.Errorf("clear clarification: %w", err)
	}
	if err := q.LogEvent(state.EventClarificationCleared, formatEvent("Clarification cleared for bead "+p.BeadID, p.Note), p.BeadID, p.AnvilName); err != nil {
		return fmt.Errorf("log event: %w", err)
	}
	return nil
}

// Retry resets the dispatch circuit breaker for a bead so the next poll
// cycle re-dispatches it. Only the bead-level reset lives here; PR-level
// retry (resetting bellows fix counts) remains in the daemon because it
// requires lifecycle and bellows references that have no place behind the
// shared interface.
//
// The returned bool is true when a circuit-breaker row with DispatchFailures>0
// was found and cleared; callers can use this to preserve the prior IPC
// response wording ("retry state reset" vs "retry reset").
func Retry(ctx context.Context, q QueueHandle, p Params) (bool, error) {
	if err := validateCommon(p); err != nil {
		return false, err
	}
	if err := checkForge(q, p); err != nil {
		return false, err
	}
	retry, err := q.GetRetry(p.BeadID, p.AnvilName)
	if err != nil {
		return false, fmt.Errorf("get retry state: %w", err)
	}
	hasCircuitBreaker := retry != nil && retry.DispatchFailures > 0
	if err := q.ResetRetry(p.BeadID, p.AnvilName); err != nil {
		if hasCircuitBreaker {
			return false, fmt.Errorf("reset retry state: %w", err)
		}
		// Pre-existing behaviour: a missing retry row is not fatal when there
		// was no circuit breaker to clear. Swallow and continue so the audit
		// event still fires.
	}
	base := fmt.Sprintf("Retry state reset for bead %s (manual)", p.BeadID)
	if !hasCircuitBreaker {
		base = fmt.Sprintf("Retry reset for bead %s (manual)", p.BeadID)
	}
	if err := q.LogEvent(state.EventRetryReset, formatEvent(base, p.Note), p.BeadID, p.AnvilName); err != nil {
		return false, fmt.Errorf("log event: %w", err)
	}
	return hasCircuitBreaker, nil
}

// Clear drops the needs-attention flags from a bead's retry row without
// re-dispatching it. Idempotent — safe on an already-clean bead.
func Clear(ctx context.Context, q QueueHandle, p Params) error {
	if err := validateCommon(p); err != nil {
		return err
	}
	if err := checkForge(q, p); err != nil {
		return err
	}
	if err := q.ClearNeedsAttention(p.BeadID, p.AnvilName); err != nil {
		return fmt.Errorf("clear needs-attention: %w", err)
	}
	msg := fmt.Sprintf("Needs-attention flags cleared for bead %s (manual)", p.BeadID)
	if err := q.LogEvent(state.EventRetryCleared, formatEvent(msg, p.Note), p.BeadID, p.AnvilName); err != nil {
		return fmt.Errorf("log event: %w", err)
	}
	return nil
}

// Stop terminates any running worker for the bead, marks the bead as needing
// clarification (preventing both auto and manual re-dispatch), and writes an
// audit event. The caller is responsible for any follow-up shell work — most
// notably `bd update --status=open --assignee=` to release the claim.
//
// The returned string is the worker ID that was signalled and marked failed,
// or empty if no active worker was found. Callers can log this to preserve
// operator visibility into which process was terminated.
func Stop(ctx context.Context, q QueueHandle, p Params) (string, error) {
	if err := validateCommon(p); err != nil {
		return "", err
	}
	if err := checkForge(q, p); err != nil {
		return "", err
	}
	reason := strings.TrimSpace(p.Note)
	if reason == "" {
		reason = "manually stopped"
	}
	reason = SanitizeControl(reason)

	var terminatedWorkerID string
	if w, err := q.ActiveWorkerByBeadAndAnvil(p.BeadID, p.AnvilName); err == nil && w != nil {
		if w.PID > 0 {
			signalWorker(w.PID)
		}
		_ = q.UpdateWorkerStatus(w.ID, state.WorkerFailed)
		terminatedWorkerID = w.ID
	} else if pw, err := q.PausedWorkerByBeadAndAnvil(p.BeadID, p.AnvilName); err == nil && pw != nil {
		// No active worker, but a paused (parked) worker exists — the operator is
		// discarding a paused bead. Transition it out of 'paused' so it is no
		// longer surfaced in Needs Attention or retained by worktree cleanup. Its
		// parked Claude spawn (if any) was already gracefully interrupted at pause
		// time, so there is normally no live PID; signal defensively when one is
		// recorded, in case the worker survived from before a restart.
		if pw.PID > 0 {
			signalWorker(pw.PID)
		}
		_ = q.UpdateWorkerStatus(pw.ID, state.WorkerFailed)
		terminatedWorkerID = pw.ID
	}

	if err := q.SetClarificationNeeded(p.BeadID, p.AnvilName, true, reason); err != nil {
		return "", fmt.Errorf("set clarification: %w", err)
	}

	msg := fmt.Sprintf("Bead %s stopped: %s", p.BeadID, reason)
	if err := q.LogEvent(state.EventBeadStopped, msg, p.BeadID, p.AnvilName); err != nil {
		return "", fmt.Errorf("log event: %w", err)
	}
	return terminatedWorkerID, nil
}

// validateCommon enforces the shared required fields (BeadID, AnvilName).
func validateCommon(p Params) error {
	if strings.TrimSpace(p.BeadID) == "" {
		return ErrMissingBeadID
	}
	if strings.TrimSpace(p.AnvilName) == "" {
		return ErrMissingAnvil
	}
	return nil
}

// checkForge enforces the multi-forge safety rule: if the caller supplied a
// forge_id, it must match the local forge. An empty caller forge_id preserves
// historical single-forge behaviour where the daemon implicitly owned every
// worker/bead in its state DB.
//
// When the caller supplies a forge_id but the local daemon has none configured,
// the request is rejected rather than silently accepted — an unconfigured local
// id is precisely when cross-forge clobbering is most likely and least visible.
func checkForge(q QueueHandle, p Params) error {
	if strings.TrimSpace(p.ForgeID) == "" {
		return nil
	}
	local := strings.TrimSpace(q.LocalForgeID())
	if local == "" {
		return fmt.Errorf("%w: caller=%q local=<unconfigured>", ErrForgeMismatch, p.ForgeID)
	}
	if p.ForgeID == local {
		return nil
	}
	return fmt.Errorf("%w: caller=%q local=%q", ErrForgeMismatch, p.ForgeID, local)
}

// formatEvent appends an operator-supplied note to the canonical event
// message so audit consumers see both the action and the human context.
func formatEvent(base, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return base
	}
	return base + " (note: " + note + ")"
}

// SanitizeControl strips control characters (except newline) from operator
// input that ends up in the bead's clarification_reason and event message.
// Exported so daemon log sites can apply the same transform and remain
// consistent with what the audit event records.
func SanitizeControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' {
			return -1
		}
		return r
	}, s)
}

// signalWorker delivers the platform-appropriate termination signal to a
// running worker process. SIGINT on POSIX gives Smith a chance to flush logs;
// Windows has no SIGINT so we fall back to Kill.
func signalWorker(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = proc.Kill()
		return
	}
	_ = proc.Signal(syscall.SIGINT)
}
