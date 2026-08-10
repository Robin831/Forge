package assay

import (
	"fmt"
	"strings"
)

// PassFailure names one deep pass that did not review the head and why. It is
// the contract every consumer of a partial run reads: the run record persists
// it, the worker status text names it, and the PR summary comment lists it, so
// all three answer "which passes did not look at this diff" from one source.
type PassFailure struct {
	// Name is the pass identifier ("logic", "repo-specific", …). Always a bare
	// label — classifyPassError constrains it for the same reason it constrains
	// Reason: both are rendered into the public PR comment.
	Name string `json:"name"`
	// Reason is the short failure label — the provider result subtype where
	// there is one ("error_max_turns"), else a category from the Reason*
	// constants. May be empty when a foreign runner produced an error whose
	// cause could not be classified. Always a bare label — classifyPassError
	// constrains it, since it is rendered into the public PR comment.
	Reason string `json:"reason"`
}

// RunStatus is the outcome of a whole Assay run over one head SHA.
//
// The distinction that matters is Partial: a run where some passes reviewed the
// diff and others never did. Reporting that as a bare failure hides findings
// that are real; reporting it as a success is worse — it presents an incomplete
// review as full coverage. Partial says exactly which of the two happened.
type RunStatus string

const (
	// RunStatusComplete means every deep pass reviewed the head.
	RunStatusComplete RunStatus = "complete"
	// RunStatusPartial means at least one, but not every, deep pass reviewed
	// the head. The findings that exist are real but do not cover the diff.
	RunStatusPartial RunStatus = "partial"
	// RunStatusFailed means no deep pass reviewed the head.
	RunStatusFailed RunStatus = "failed"
)

// DeriveStatus maps the pass tally onto a run status. total is the number of
// deep passes attempted; failed lists the ones that did not produce findings.
// It is the single place the three-way decision is made — everything else reads
// the status it returns rather than re-deriving it from a pass-error count.
//
// A run that attempted no deep passes at all is Failed, not Complete: nothing
// looked at the diff, and "complete: 0 of 0 passes completed" would present
// that as a clean review. The deep-pass set is static today, so this only
// matters if it ever becomes filterable — at which point the conservative
// answer is the one that must already be wired in.
func DeriveStatus(total int, failed []PassFailure) RunStatus {
	switch {
	case total <= 0:
		return RunStatusFailed
	case len(failed) == 0:
		return RunStatusComplete
	case len(failed) >= total:
		return RunStatusFailed
	default:
		return RunStatusPartial
	}
}

// RenderStatusText renders the one-line coverage status, e.g.
//
//	partial: 3 of 5 passes completed (failed: logic, repo-specific — error_max_turns)
//
// It is what the daemon logs, what the assay_partial event message carries, and
// what the PR findings panel shows beside the run's status pill. The worker row
// itself carries only the status ("partial") — the tally lives in these three.
//
// Passes sharing a reason are grouped behind a single reason; mixed reasons are
// rendered as "name — reason" pairs. A complete run reads
// "complete: 5 of 5 passes completed".
func RenderStatusText(status RunStatus, completed, total int, failed []PassFailure) string {
	base := fmt.Sprintf("%s: %d of %d passes completed", status, completed, total)
	if len(failed) == 0 {
		return base
	}
	return base + fmt.Sprintf(" (failed: %s)", formatFailedPasses(failed, reasonDash))
}

// PartialCoverageNote renders the caveat line the PR summary comment carries
// when passes are missing from a run: an explicit statement of which passes did
// not review this head, so a short findings list is not mistaken for a clean
// bill of health. Returns "" when nothing failed.
func PartialCoverageNote(failed []PassFailure) string {
	if len(failed) == 0 {
		return ""
	}
	return "⚠️ **Partial coverage** — these Assay passes did not review this head: " +
		formatFailedPasses(failed, reasonParen) +
		". The findings below are not a complete review of this diff."
}

// reasonDash and reasonParen are the two ways a reason is attached to the pass
// names it belongs to: a dash for the one-line status text, parentheses for the
// prose of the PR summary comment ("logic, repo-specific (error_max_turns)").
func reasonDash(names, reason string) string  { return names + " — " + reason }
func reasonParen(names, reason string) string { return names + " (" + reason + ")" }

// formatFailedPasses renders the failed passes as a comma-separated list. When
// every pass failed for the same reason the reason is stated once at the end
// ("logic, repo-specific — error_max_turns"); otherwise each pass carries its
// own ("logic — error_max_turns, repo-specific — rate_limited"). attach joins a
// name (or the joined list of them) to a reason, so the grouping logic stays in
// one place and the log form and the PR-comment form cannot drift apart.
func formatFailedPasses(failed []PassFailure, attach func(names, reason string) string) string {
	if len(failed) == 0 {
		return ""
	}
	names := passNames(failed)
	if reason, uniform := sharedReason(failed); uniform {
		if reason == "" {
			return strings.Join(names, ", ")
		}
		return attach(strings.Join(names, ", "), reason)
	}
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		if f.Reason == "" {
			parts = append(parts, f.Name)
			continue
		}
		parts = append(parts, attach(f.Name, f.Reason))
	}
	return strings.Join(parts, ", ")
}

// passNames returns the failed pass names in their given order.
func passNames(failed []PassFailure) []string {
	names := make([]string, 0, len(failed))
	for _, f := range failed {
		names = append(names, f.Name)
	}
	return names
}

// sharedReason reports the single reason every failed pass carries, and whether
// they in fact all share one.
func sharedReason(failed []PassFailure) (string, bool) {
	seen := map[string]bool{}
	for _, f := range failed {
		seen[f.Reason] = true
	}
	if len(seen) != 1 {
		return "", false
	}
	for r := range seen {
		return r, true
	}
	return "", false
}
