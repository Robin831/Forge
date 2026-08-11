package daemon

import (
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// enginePassFailures converts the persisted failed-pass list back into the
// engine's form. It is the inverse of statePassFailures and exists for the same
// boundary reason: the state package cannot import the engine, so the terminal
// event — which renders from the run record, not from the review result — has
// to cross back over to reuse the engine's one failed-pass renderer.
func enginePassFailures(failed []state.AssayPassFailure) []assay.PassFailure {
	if len(failed) == 0 {
		return nil
	}
	out := make([]assay.PassFailure, 0, len(failed))
	for _, f := range failed {
		out = append(out, assay.PassFailure{Name: f.Name, Reason: f.Reason})
	}
	return out
}

// assayEventStatus reads the persisted status back as the engine's. Anything
// that is not a recognised complete/partial value is failed: a run whose status
// never got set died before the engine returned, and reporting that as a
// completion would be the one wrong answer.
func assayEventStatus(status string) assay.RunStatus {
	switch status {
	case state.AssayStatusComplete:
		return assay.RunStatusComplete
	case state.AssayStatusPartial:
		return assay.RunStatusPartial
	default:
		return assay.RunStatusFailed
	}
}

// assayTerminalEvent maps a finished run onto the one event type that closes it
// out. It goes through assayEventStatus so the event type and the status word
// the message leads with are decided by a single mapping and cannot disagree.
func assayTerminalEvent(status string) state.EventType {
	switch assayEventStatus(status) {
	case assay.RunStatusComplete:
		return state.EventAssayCompleted
	case assay.RunStatusPartial:
		return state.EventAssayPartial
	default:
		return state.EventAssayFailed
	}
}

// assayRunEventMessage renders the feed message for a finished run. It reads
// the run record rather than the review result because the record is the one
// thing both terminal paths produce — a run that failed before the engine
// returned has no result at all — and because the record is what the daemon
// already logs, so the two cannot report different numbers.
func assayRunEventMessage(run *state.AssayRun) string {
	reason := run.Error
	if reason == "" {
		reason = run.SkippedReason
	}
	if reason == "" {
		reason = "no passes reviewed the head"
	}
	return assay.RunEvent{
		PRNumber:        run.PRNumber,
		Status:          assayEventStatus(run.Status),
		CompletedPasses: run.CompletedPasses,
		TotalPasses:     run.TotalPasses,
		FailedPasses:    enginePassFailures(run.FailedPasses),
		Findings:        run.FindingsCount,
		CostUSD:         run.CostUSD,
		Duration:        time.Duration(run.DurationMs) * time.Millisecond,
		ShadowMode:      run.ShadowMode,
		Reason:          reason,
	}.Message()
}

// emitAssayTerminalEvent logs exactly one terminal event for a finished Assay
// run — completed, partial or failed. It is called from the single place a run
// finishes, which is what guarantees the 1:1 pairing with the pr_review_needed
// that opened it: every dispatched review resolves in the feed, and the common
// clean case (which used to emit nothing at all) says so with the same numbers
// the daemon log line carries.
//
// A failure to write the event is logged and swallowed. The review itself has
// already happened; losing its feed row must not take the run down with it.
func (d *Daemon) emitAssayTerminalEvent(run *state.AssayRun, beadID string) {
	if run == nil {
		return
	}
	evt := assayTerminalEvent(run.Status)
	if err := d.db.LogEvent(evt, assayRunEventMessage(run), beadID, run.Anvil); err != nil {
		d.logger.Warn("failed to log Assay terminal event",
			"event", string(evt), "pr", run.PRNumber, "bead", beadID, "error", err)
	}
}
