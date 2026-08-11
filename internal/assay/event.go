package assay

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
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
func (e RunEvent) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Assay PR #%d: %s", e.PRNumber, e.Status)

	details := make([]string, 0, 3)
	if e.TotalPasses > 0 {
		tally := fmt.Sprintf("%d/%d passes", e.CompletedPasses, e.TotalPasses)
		if len(e.FailedPasses) > 0 {
			tally += fmt.Sprintf(" (failed: %s)", formatFailedPasses(e.FailedPasses, reasonDash))
		}
		details = append(details, tally)
	}
	if e.Status == RunStatusFailed {
		if reason := trimEventReason(e.Reason); reason != "" {
			details = append(details, reason)
		}
	} else {
		details = append(details, fmt.Sprintf("%d findings", e.Findings))
	}
	if len(details) > 0 {
		b.WriteString(" — " + strings.Join(details, ", "))
	}

	fmt.Fprintf(&b, " ($%.2f, %ds)", e.CostUSD, int(e.Duration.Round(time.Second).Seconds()))
	if e.ShadowMode && e.Status != RunStatusFailed {
		b.WriteString(" (shadow — findings in panel only)")
	}
	return b.String()
}

// ansiEscape matches the escape sequences a provider's stderr can carry: CSI
// (colour, cursor movement, erase), OSC (title and clipboard writes, terminated
// by BEL or ST) and the bare two-byte forms. Matching the whole sequence rather
// than the lone ESC is what keeps a stripped "\x1b[31m" from leaving a visible
// "[31m" behind in the row.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;:?]*[ -/]*[@-~]" +
	"|\x1b[\\]P^_X][^\a\x1b]*(?:\a|\x1b\\\\)?" +
	"|\x1b[@-Z\\\\-_]")

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
// and never commands to the terminal drawing it. Tabs become spaces (a tab is
// not printable, but dropping it would run two words together).
func sanitizeEventReason(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}
