package textfmt

import "unicode/utf8"

// TruncateRunes returns at most maxBytes bytes of s, cutting back to the last
// rune boundary rather than through a multi-byte sequence.
//
// The bound is in bytes because that is what the surfaces reaching for it are
// sized in — a persisted row, a feed line, a prompt section — while the text is
// routinely not Forge's own (git's output, a remote's rejection message, a
// model's answer) and so is neither ASCII by construction nor safe to
// re-encode. A plain slice at a byte index leaves a half-written rune at the
// end, which renders as a replacement character at best.
//
// No marker is appended: a caller that wants an ellipsis budgets for it itself,
// since only the caller knows whether its bound covers the marker.
func TruncateRunes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
