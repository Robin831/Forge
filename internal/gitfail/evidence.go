package gitfail

import (
	"strings"

	"github.com/Robin831/Forge/internal/termtext"
	"github.com/Robin831/Forge/internal/textfmt"
)

// evidenceEllipsis marks a truncated evidence string. It is named because Bound
// budgets for its length rather than appending it on top of the bound it was
// given.
const evidenceEllipsis = "…"

// Sanitize renders git's own words for a failure, bounded and stripped of
// anything a terminal would interpret.
//
// The text is git's, and git's is partly the remote's (a server-side hook's
// rejection message is echoed verbatim), so it is text Forge did not write
// reaching a rendered needs-attention row and an activity-feed line —
// termtext.Line is what every such surface goes through. Newlines are the reason
// this is not just a trim: git writes one line per ref, and a feed row is one
// line.
func Sanitize(raw string, maxBytes int) string {
	clean := termtext.Line(strings.TrimSpace(raw))
	clean = strings.Join(strings.Fields(clean), " ")
	return Bound(clean, maxBytes)
}

// Bound truncates already-sanitized evidence to a byte bound.
//
// The marker is inside the bound, not on top of it: the bound is what a row and
// a feed line are sized in, so a cut that then appends three more bytes
// overshoots the very number it is enforcing. It is a parameter rather than a
// constant because a message that assembles several components gives git's words
// whatever the rest of them left of one total.
func Bound(s string, maxBytes int) string {
	if maxBytes <= len(evidenceEllipsis) {
		maxBytes = len(evidenceEllipsis) + 1
	}
	if len(s) <= maxBytes {
		return s
	}
	return textfmt.TruncateRunes(s, maxBytes-len(evidenceEllipsis)) + evidenceEllipsis
}
