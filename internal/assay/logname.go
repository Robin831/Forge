package assay

import "strings"

// LogStage is the leading segment of every Assay session log filename. It is
// what the bead Logs panel maps to the "assay" stage label, and it is
// deliberately unchanged from the pre-run-key naming so files written before
// this scheme still resolve to the same stage.
const LogStage = "assay"

// PassLogPrefix builds the smith.SpawnOptions.LogPrefix for one pass session,
// producing a log file named
//
//	assay-<runKey>-<pass>-<ts>-<seq>.log
//
// A single Assay run writes six of these — one triage plus five deep passes —
// and before the run key and the pass name were in the filename the panel had
// no way to tell one run's six sessions from six separate runs, nor which of
// them produced the run's findings.
//
// The run key is always all-digits (the daemon derives it from the run's start
// time) and a pass name never is, which is what keeps the reader's parse
// unambiguous for both this format and the older assay-<ts>-<seq>.log. An
// empty or unusable run key degrades to assay-<pass>-…: the session is still
// named by its pass, it just is not grouped into a run.
func PassLogPrefix(runKey, pass string) string {
	parts := []string{LogStage}
	if k := sanitizeLogSegment(runKey); k != "" && isAllDigits(k) {
		parts = append(parts, k)
	}
	if p := sanitizeLogSegment(pass); p != "" {
		parts = append(parts, p)
	}
	return strings.Join(parts, "-")
}

// sanitizeLogSegment reduces a value to the lowercase alphanumeric-and-dash
// alphabet the filename parse assumes. Everything else — path separators, dots,
// underscores, anything that would introduce a component or a second field
// separator — is dropped, and leading/trailing dashes are trimmed so a segment
// can never collapse two others together. Pass names come from a fixed table
// in this package, but the prefix reaches a filesystem path, so it is screened
// rather than trusted.
func sanitizeLogSegment(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}

// isAllDigits reports whether s is non-empty and consists only of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
