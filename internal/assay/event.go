package assay

import (
	"fmt"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/termtext"
)

// eventReasonMax caps the failure reason rendered into a terminal event
// message. A run error can carry a provider's whole stderr; the activity feed
// is one line per event, so the reason is trimmed to its first line and then to
// this many runes. The full text is on the run record and in the daemon log.
const eventReasonMax = 160

// RunEvent is everything the activity feed needs to close out one Assay run:
// the coverage tally, what the run found, what it cost and how long it took.
//
// It exists because the feed's terminal event must say the same numbers the
// daemon log line says. Both are rendered from the run's own record, so a
// review that reports "5/5 passes, 7 findings" in the log cannot report
// anything else in Hearth.
type RunEvent struct {
	// PRNumber is the GitHub PR the run reviewed.
	PRNumber int
	// Status is the run's three-way coverage outcome.
	Status RunStatus
	// CompletedPasses / TotalPasses are the pass tally behind Status. A zero
	// TotalPasses means the run died before the deep passes were attempted
	// (a failed triage, an unfetchable diff), and no tally is rendered.
	CompletedPasses int
	TotalPasses     int
	// FailedPasses names the passes that did not review the head.
	FailedPasses []PassFailure
	// Findings is the size of the final, aggregated finding set.
	Findings int
	// CostUSD is what the run was billed — including a run that failed, which
	// is still a billed run.
	CostUSD float64
	// Duration is the run's wall-clock time.
	Duration time.Duration
	// ShadowMode reports whether the run posted nothing on the PR.
	ShadowMode bool
	// Reason is the failure cause, rendered for failed runs only.
	Reason string
	// SkippedReason names why a run that did not fail nonetheless reviewed
	// nothing — an unreviewable diff, or a repeat push with no reviewable
	// delta. It is rendered in place of the findings count, because "0
	// findings" is precisely how a skip would otherwise read.
	SkippedReason string
}

// Message renders the one-line terminal event message for a finished run:
//
//	Assay PR #347: complete — 5/5 passes, 7 findings ($2.80, 152s)
//	Assay PR #347: partial — 3/5 passes (failed: logic — error_max_turns), 4 findings ($1.20, 90s)
//	Assay PR #347: failed — all assay deep passes failed ($0.40, 30s)
//
// A shadow run appends "(shadow — findings in panel only)" wherever it produced
// coverage: a shadow review posts nothing on the PR by design, so without that
// clause the operator has no way to tell a silent-by-design run from one whose
// findings simply never arrived. A failed run carries no findings, so the
// clause would only mislead there and is left off.
//
// Every part of the message Forge did not write itself — the failure reason and
// the failed-pass names/reasons — goes through trimEventReason on the way in,
// so the row is safe to render whatever a producer put in those fields.
func (e RunEvent) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Assay PR #%d: %s", e.PRNumber, e.Status)

	details := make([]string, 0, 3)
	if e.TotalPasses > 0 {
		tally := fmt.Sprintf("%d/%d passes", e.CompletedPasses, e.TotalPasses)
		if len(e.FailedPasses) > 0 {
			tally += fmt.Sprintf(" (failed: %s)", formatFailedPasses(sanitizeFailedPasses(e.FailedPasses), reasonDash))
		}
		details = append(details, tally)
	}
	switch {
	case e.Status == RunStatusFailed:
		if reason := trimEventReason(e.Reason); reason != "" {
			details = append(details, reason)
		}
	case e.SkippedReason != "":
		// A skipped run is complete and reviewed nothing. Reporting its empty
		// finding set as "0 findings" is the one rendering that reads like a
		// clean review, so the row names the skip instead.
		details = append(details, "skipped: "+trimEventReason(e.SkippedReason))
	default:
		details = append(details, fmt.Sprintf("%d findings", e.Findings))
	}
	if len(details) > 0 {
		b.WriteString(" — " + strings.Join(details, ", "))
	}

	fmt.Fprintf(&b, " ($%.2f, %ds)", e.CostUSD, int(e.Duration.Round(time.Second).Seconds()))
	// The shadow clause explains an absent PR comment on a run that produced
	// coverage. A skipped run produced none, so the clause would only offer a
	// second explanation for the same silence.
	if e.ShadowMode && e.Status != RunStatusFailed && e.SkippedReason == "" {
		b.WriteString(" (shadow — findings in panel only)")
	}
	return b.String()
}

// trimEventReason reduces a failure cause to one bounded, printable line.
//
// Provider errors arrive multi-line and can be arbitrarily long, so anything
// past the first line (or past eventReasonMax) is dropped with an ellipsis
// rather than allowed to push the numbers off the row. They are also the one
// thing in the message Forge did not write: the text can quote provider output
// shaped by the diff under review, and Hearth renders a feed row through
// lipgloss without sanitizing it. So escape sequences and control runes are
// stripped too — otherwise a crafted reason could repaint or spoof rows in the
// operator's activity feed rather than merely occupy one.
func trimEventReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if nl := strings.IndexAny(reason, "\r\n"); nl != -1 {
		reason = strings.TrimSpace(reason[:nl])
	}
	reason = strings.TrimSpace(sanitizeEventReason(reason))
	runes := []rune(reason)
	if len(runes) > eventReasonMax {
		return strings.TrimSpace(string(runes[:eventReasonMax])) + "…"
	}
	return reason
}

// sanitizeEventReason removes ANSI escape sequences and any remaining
// non-printable rune, so what reaches the feed can only add characters to a row
// and never commands to the terminal drawing it.
//
// It is the shared internal/termtext stripper rather than a local one: Hearth
// strips bead titles with the same helper, and two implementations of this
// meant the surface with the weaker coverage decided how much of a sequence
// reached the screen.
func sanitizeEventReason(s string) string {
	return termtext.Line(s)
}

// sanitizeFailedPasses puts the failed-pass list through the same trimming the
// failure reason gets, so the safety property the feed row depends on is local
// to Message() instead of an invariant every producer of a PassFailure has to
// keep.
//
// Today a pass name is an engine constant and a reason is a provider result
// subtype, so nothing here is currently hostile. But run errors already carry
// raw provider text, and the day a PassError reason is populated the same way,
// this is the only thing between it and a lipgloss row.
func sanitizeFailedPasses(failed []PassFailure) []PassFailure {
	if len(failed) == 0 {
		return nil
	}
	out := make([]PassFailure, 0, len(failed))
	for _, f := range failed {
		out = append(out, PassFailure{
			Name:   trimEventReason(f.Name),
			Reason: trimEventReason(f.Reason),
		})
	}
	return out
}
