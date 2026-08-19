// Package textfmt holds the small rendering helpers that have no home of their
// own but are wanted by more than one surface — the CLI, the daemon's log and
// event text, the web layer's messages and the PR comments questgiver posts.
//
// It exists because pluralization had been re-derived five times: three
// package-local plural helpers in two shapes (count-with-noun and bare
// suffix), a fourth spelled pluralS, and one open-coded if/else. None of them
// disagreed, which is exactly why the drift was invisible — the next surface
// wrote a sixth rather than finding one of the five.
package textfmt

import "strconv"

// Count renders a quantity with its noun — "1 quest", "3 quests" — so a number
// arrives labelled rather than bare. Only regular -s plurals are formed: every
// caller in Forge counts a noun it chose itself (quests, restarts, dependents,
// workers, beads), so an irregular one is a caller writing the two forms out,
// not a table living here.
func Count(n int, noun string) string {
	return strconv.Itoa(n) + " " + Noun(n, noun)
}

// Noun renders just the noun in the number Count would use, for the callers
// that already format the count themselves.
func Noun(n int, noun string) string {
	return noun + Suffix(n)
}

// Suffix is the plural marker alone — "" or "s" — for format strings that
// interpolate it next to a literal noun, as in "%d worker%s".
func Suffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
