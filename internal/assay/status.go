package assay

import (
	"fmt"
	"sort"
	"strings"
)

// PassFailure names one deep pass that did not review the head and why. It is
// the contract every consumer of a partial run reads: the run record persists
// it, the worker status text names it, and the PR summary comment lists it, so
// all three answer "which passes did not look at this diff" from one source.
type PassFailure struct {
	// Name is the pass identifier ("logic", "repo-specific", …).
	Name string `json:"name"`
	// Reason is the short failure label — the provider result subtype where
	// there is one ("error_max_turns"), else a category from the Reason*
	// constants. May be empty when a foreign runner produced an error whose
	// cause could not be classified.
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
func DeriveStatus(total int, failed []PassFailure) RunStatus {
	switch {
	case len(failed) == 0:
		return RunStatusComplete
	case total > 0 && len(failed) >= total:
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
	return base + fmt.Sprintf(" (failed: %s)", formatFailedPasses(failed, " — "))
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
		formatFailedPassesParen(failed) +
		". The findings below are not a complete review of this diff."
}

// formatFailedPasses renders the failed passes as a comma-separated list. When
// every pass failed for the same reason the reason is stated once at the end
// ("logic, repo-specific — error_max_turns"); otherwise each pass carries its
// own ("logic — error_max_turns, repo-specific — rate_limited"). sep is the
// separator between a pass name and its reason.
func formatFailedPasses(failed []PassFailure, sep string) string {
	if len(failed) == 0 {
		return ""
	}
	names := passNames(failed)
	if reason, uniform := sharedReason(failed); uniform {
		if reason == "" {
			return strings.Join(names, ", ")
		}
		return strings.Join(names, ", ") + sep + reason
	}
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		if f.Reason == "" {
			parts = append(parts, f.Name)
			continue
		}
		parts = append(parts, f.Name+sep+f.Reason)
	}
	return strings.Join(parts, ", ")
}

// formatFailedPassesParen is formatFailedPasses with the reason in parentheses,
// which reads better inside the prose of the PR summary comment:
// "logic, repo-specific (error_max_turns)".
func formatFailedPassesParen(failed []PassFailure) string {
	if len(failed) == 0 {
		return ""
	}
	names := passNames(failed)
	if reason, uniform := sharedReason(failed); uniform {
		if reason == "" {
			return strings.Join(names, ", ")
		}
		return strings.Join(names, ", ") + " (" + reason + ")"
	}
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		if f.Reason == "" {
			parts = append(parts, f.Name)
			continue
		}
		parts = append(parts, f.Name+" ("+f.Reason+")")
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
	reasons := make([]string, 0, 1)
	for r := range seen {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	return reasons[0], true
}
