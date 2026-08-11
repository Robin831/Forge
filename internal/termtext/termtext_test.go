package termtext

import (
	"strings"
	"testing"
	"unicode"
)

// TestLine covers the sequence set both surfaces depend on. The cases are the
// union of what Hearth's title stripper and Assay's reason stripper each used to
// handle alone: whichever one was weaker decided how much of a hostile string
// reached the terminal, which is why there is now one of these.
func TestLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text is untouched", "triage failed", "triage failed"},
		{"empty", "", ""},
		{"csi colour", "\x1b[31mred\x1b[0m", "red"},
		{"csi cursor movement and erase", "a\x1b[2A\x1b[Kb", "ab"},
		// The private-parameter bytes < = > are inside the CSI parameter range
		// (0x30–0x3F). A class of only [0-9;:?] failed to match these, and the
		// lone ESC then went to the non-printable pass, leaving "[>0c" visible.
		{"csi private parameter bytes", "a\x1b[>0cb", "ab"},
		{"csi private mode set", "a\x1b[=5hb", "ab"},
		{"csi with intermediate bytes", "a\x1b[?1;2 qb", "ab"},
		{"osc terminated by BEL", "a\x1b]0;pwned\ab", "ab"},
		{"osc terminated by ST", "a\x1b]0;pwned\x1b\\b", "ab"},
		{"dcs string", "a\x1bPq#0;2;0;0;0\x1b\\b", "ab"},
		{"apc string", "a\x1b_Gf=100\x1b\\b", "ab"},
		{"bare two-byte escape", "a\x1bMb", "ab"},
		{"lone control runes", "a\x07\x08\x00b", "ab"},
		{"line breaks become spaces", "line\none", "line one"},
		{"crlf becomes two spaces", "line\r\none", "line  one"},
		{"tabs become spaces", "tab\there", "tab here"},
		{"zero width space is dropped", "a​b", "ab"},
		{"non-ascii text survives", "café — naïve ✅", "café — naïve ✅"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Line(tt.input)
			if got != tt.want {
				t.Errorf("Line(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestLineLeavesNothingExecutable is the property the callers actually rely on:
// whatever goes in, what comes out carries no escape byte and no non-printable
// rune, so it can add characters to a row and never commands to the terminal.
func TestLineLeavesNothingExecutable(t *testing.T) {
	hostile := []string{
		"\x1b[31mRED\x1b[0m\x1b[2A\x1b[Kspoofed",
		"\x1b]0;pwned\apost",
		"\x1b[>0c\x1b[=5h\x1b[?25l",
		"\x1bPq\x1b\\\x1b^priv\x1b\\\x1b_apc\x1b\\",
		"multi\nline\r\nreason\twith\ttabs",
		"\x1b[",
		"\x1b",
	}
	for _, in := range hostile {
		got := Line(in)
		if strings.ContainsRune(got, '\x1b') {
			t.Errorf("Line(%q) kept an escape byte: %q", in, got)
		}
		for _, r := range got {
			if !unicode.IsPrint(r) {
				t.Errorf("Line(%q) kept non-printable %q: %q", in, r, got)
			}
		}
	}
}
