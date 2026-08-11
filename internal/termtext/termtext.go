// Package termtext reduces text Forge did not write — a provider's stderr, a
// bead title, anything quoted back from an agent or a repository — to something
// that can only add characters to a terminal row and never commands to the
// terminal drawing it.
//
// It exists so there is one stripper rather than one per surface. Hearth
// renders bead titles through lipgloss and the activity feed renders Assay
// failure reasons the same way; both are untrusted, and two hand-rolled
// strippers with different coverage meant the weaker one silently decided how
// much of an escape sequence reached the screen.
package termtext

import (
	"regexp"
	"strings"
	"unicode"
)

// ansiEscape matches the escape sequences untrusted text can carry: CSI
// (colour, cursor movement, erase), the string sequences OSC/DCS/PM/APC/SOS
// (title and clipboard writes, terminated by BEL or ST) and the bare two-byte
// forms. Matching the whole sequence rather than the lone ESC is what keeps a
// stripped "\x1b[31m" from leaving a visible "[31m" behind in the row.
//
// The CSI parameter class is the full 0x30–0x3F range — digits, ':', ';' and
// the private-parameter bytes '<', '=', '>', '?' — so a private-mode sequence
// like "\x1b[>0c" (secondary DA) is matched whole rather than falling through
// to leave "[>0c" as literal residue.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9:;<=>?]*[ -/]*[@-~]" +
	"|\x1b[\\]P^_X][^\a\x1b]*(?:\a|\x1b\\\\)?" +
	"|\x1b[@-Z\\\\-_]")

// Line reduces untrusted text to a single printable line: escape sequences are
// removed whole, line breaks and tabs become spaces, and any remaining
// non-printable rune is dropped.
//
// Line breaks become spaces rather than being dropped because both callers
// render into a single row — a title or a feed message — where a swallowed
// newline would run two words together, and a surviving one would let the text
// claim rows it was never given. Tabs are spaced for the same reason.
//
// Line does not bound length; callers that render into a fixed width truncate
// themselves, and they do it after this so a cut cannot land inside a sequence
// this would otherwise have removed.
func Line(s string) string {
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}
