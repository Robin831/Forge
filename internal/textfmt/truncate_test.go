package textfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunesKeepsShortStringsWhole(t *testing.T) {
	if got := TruncateRunes("abc", 10); got != "abc" {
		t.Fatalf("TruncateRunes = %q, want %q", got, "abc")
	}
	if got := TruncateRunes("abc", 3); got != "abc" {
		t.Fatalf("a string exactly at the bound must be untouched, got %q", got)
	}
}

// A cut that lands inside a multi-byte rune must move back to its start, never
// leave a fragment that renders as U+FFFD.
func TestTruncateRunesCutsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes per rune
	for maxBytes := 0; maxBytes <= len(s); maxBytes++ {
		got := TruncateRunes(s, maxBytes)
		if len(got) > maxBytes {
			t.Fatalf("TruncateRunes(_, %d) returned %d bytes", maxBytes, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateRunes(_, %d) = %q, which is not valid UTF-8", maxBytes, got)
		}
	}
}

func TestTruncateRunesRejectsNonPositiveBounds(t *testing.T) {
	if got := TruncateRunes("abc", 0); got != "" {
		t.Fatalf("a zero bound holds nothing, got %q", got)
	}
	if got := TruncateRunes("abc", -1); got != "" {
		t.Fatalf("a negative bound holds nothing, got %q", got)
	}
}
