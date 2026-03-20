package ledger

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// ansiEscapeLen returns the length (in runes) of a CSI escape sequence
// starting at runes[i], or 0 if there is no escape at that position.
func ansiEscapeLen(runes []rune, i int) int {
	if i >= len(runes) || runes[i] != '\x1b' {
		return 0
	}
	if i+1 >= len(runes) || runes[i+1] != '[' {
		return 0
	}
	j := i + 2
	for j < len(runes) {
		r := runes[j]
		j++
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return j - i
		}
	}
	return 0
}

// visualToRuneIndex returns the rune index in s that corresponds to visual
// column col, skipping ANSI CSI escape sequences and using cell-width-aware
// counting so double-width runes are handled correctly.
func visualToRuneIndex(s string, col int) int {
	runes := []rune(s)
	visual := 0
	i := 0
	for i < len(runes) {
		if visual >= col {
			return i
		}
		if n := ansiEscapeLen(runes, i); n > 0 {
			i += n
			continue
		}
		visual += runewidth.RuneWidth(runes[i])
		i++
	}
	return i
}

// placeOverlayAt composites overlayLines onto bgLines at position (startX, startY).
func placeOverlayAt(startX, startY, overlayWidth int, overlayLines, bgLines []string) {
	for i, overlayLine := range overlayLines {
		bgIdx := startY + i
		if bgIdx >= len(bgLines) {
			break
		}
		bgLine := bgLines[bgIdx]
		bgRunes := []rune(bgLine)
		olRunes := []rune(overlayLine)

		bgCutStart := visualToRuneIndex(bgLine, startX)

		var result []rune
		result = append(result, bgRunes[:bgCutStart]...)
		for lipgloss.Width(string(result)) < startX {
			result = append(result, ' ')
		}
		result = append(result, olRunes...)
		bgCutEnd := visualToRuneIndex(bgLine, startX+overlayWidth)
		if bgCutEnd < len(bgRunes) {
			result = append(result, bgRunes[bgCutEnd:]...)
		}
		bgLines[bgIdx] = string(result)
	}
}

// placeOverlay centers an overlay on a background of the given dimensions.
func placeOverlay(width, height int, overlay, background string) string {
	overlayLines := strings.Split(overlay, "\n")
	bgLines := strings.Split(background, "\n")

	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}

	overlayHeight := len(overlayLines)
	overlayWidth := 0
	for _, l := range overlayLines {
		if w := lipgloss.Width(l); w > overlayWidth {
			overlayWidth = w
		}
	}

	startY := (height - overlayHeight) / 2
	startX := (width - overlayWidth) / 2
	if startY < 0 {
		startY = 0
	}
	if startX < 0 {
		startX = 0
	}

	placeOverlayAt(startX, startY, overlayWidth, overlayLines, bgLines)
	return strings.Join(bgLines[:height], "\n")
}
