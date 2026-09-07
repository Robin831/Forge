package changelog

import "strings"

// FragmentMatchesBead reports whether a changelog.d file NAME (not a path) is a
// changelog fragment for beadID.
//
// The rule, stated once because every reader of it — this package's
// ValidateFragmentExists behind `forge changelog validate`, and the daemon's
// stranded-branch completion probe — must answer the same question the same
// way: the name is <bead-id>.md, or the bead id followed by a "." or "-"
// delimiter and further segments of which the FIRST is alphabetic. That covers
// the language split some repositories use (<bead>.en.md + <bead>.nb.md) and
// the hyphen-delimited fragment KIND others add to it
// (<bead>-technical.en.md), in either order.
//
// Both decorations arrived from a live escalation. Matching only <bead>.md made
// the daemon read completed language-split work as incomplete and strand it in
// needs_human (Fhi.Metadata-15ed9); widening that to a dot only left the
// -technical pair — the whole fragment set a technical-only change carries, an
// existing example on main being Fhi.Metadata-1pl9l — failing the same way
// (Fhi.Metadata-hwbwz, 2026-09-07: the PR was opened by hand and passed all 12
// CI checks on the first run, so the escalation was a pure false negative).
//
// The alphabetic test on the first decorated segment is what keeps a DIFFERENT
// bead out, and it is load-bearing rather than tidiness: bd's hierarchical ids
// are literally <parent>.<n> (`bd create --parent`, 70 such pairs in this
// repository's own issues), so a child's changelog.d/<parent>.7.md is the
// parent id followed by the "." delimiter and would otherwise read as a
// fragment for the parent — the mirror of the false negative above, in which a
// parent's stranded branch inherits a merged child's fragment and reads as
// complete work. A language code and a fragment kind are alphabetic; a child's
// ordinal is not. A bare-character extension (<bead>1.md, <bead>1-technical.md)
// is excluded by the delimiter itself.
//
// This encodes the convention rather than reading it from the anvil's own
// changelog tooling, which is why Munin's "Check changelog updates" CI gate and
// Forge could disagree at all, and will again the next time a repository adds a
// fragment shape — Forge-3jyi5 weighs deriving the accepted shapes from the
// repository instead.
func FragmentMatchesBead(name, beadID string) bool {
	name = strings.TrimSpace(name)
	if beadID == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	stem, ok := strings.CutSuffix(name, ".md")
	if !ok {
		return false
	}
	if stem == beadID {
		return true
	}
	rest, ok := strings.CutPrefix(stem, beadID)
	if !ok {
		return false
	}
	if !strings.HasPrefix(rest, ".") && !strings.HasPrefix(rest, "-") {
		return false
	}
	return isAlphaSegment(firstFragmentSegment(rest[1:]))
}

// firstFragmentSegment returns the leading segment of a decorated fragment
// stem — everything up to the next "." or "-" delimiter.
func firstFragmentSegment(s string) string {
	if i := strings.IndexAny(s, ".-"); i >= 0 {
		return s[:i]
	}
	return s
}

// isAlphaSegment reports whether s is a non-empty run of ASCII letters, which
// is what a language code ("en", "nb", the "fr" of "fr-CA") and a fragment kind
// ("technical") are and a bd child bead's ordinal ("7") is not.
func isAlphaSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
