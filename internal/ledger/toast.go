package ledger

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const (
	// toastDuration is how long a toast notification stays visible.
	toastDuration = 4 * time.Second

	// maxToasts is the maximum number of toasts shown simultaneously.
	maxToasts = 3

	// toastMaxWidth caps the message length to prevent overly wide toasts.
	toastMaxWidth = 60
)

// toast is a temporary notification displayed at the bottom of the Ledger TUI.
type toast struct {
	id      int
	message string
	isError bool
}

// toastDismissMsg fires when a toast's auto-dismiss timer expires.
type toastDismissMsg struct{ id int }

// scheduleToastDismiss returns a Cmd that fires toastDismissMsg after toastDuration.
func scheduleToastDismiss(id int) tea.Cmd {
	return tea.Tick(toastDuration, func(time.Time) tea.Msg {
		return toastDismissMsg{id: id}
	})
}

var (
	toastSuccessStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("82")).
				Padding(0, 1).
				Foreground(lipgloss.Color("255"))

	toastErrorStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("196")).
			Padding(0, 1).
			Foreground(lipgloss.Color("255"))
)

// addToast appends a toast and returns a dismiss command.
func (m *Model) addToast(message string, isError bool) tea.Cmd {
	t := toast{
		id:      m.nextToastID,
		message: message,
		isError: isError,
	}
	m.nextToastID++
	m.toasts = append(m.toasts, t)
	if len(m.toasts) > maxToasts {
		m.toasts = m.toasts[len(m.toasts)-maxToasts:]
	}
	return scheduleToastDismiss(t.id)
}

// dismissToast removes the toast with the given ID.
func (m *Model) dismissToast(id int) {
	for i, t := range m.toasts {
		if t.id == id {
			m.toasts = append(m.toasts[:i], m.toasts[i+1:]...)
			return
		}
	}
}

// renderToasts renders the active toasts stacked vertically.
func (m *Model) renderToasts() string {
	if len(m.toasts) == 0 {
		return ""
	}
	parts := make([]string, len(m.toasts))
	for i, t := range m.toasts {
		text := truncateToast(t.message, toastMaxWidth)
		if t.isError {
			parts[i] = toastErrorStyle.Render(text)
		} else {
			parts[i] = toastSuccessStyle.Render(text)
		}
	}
	return strings.Join(parts, "\n")
}

// placeToastsOverlay places the toast overlay at the bottom-center of the
// background, positioned above the last footerH rows.
// It uses an ANSI-aware compositor to preserve background content and styling.
func placeToastsOverlay(width, height, footerH int, overlay, background string) string {
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

	startY := height - footerH - overlayHeight
	if startY < 0 {
		startY = 0
	}
	startX := (width - overlayWidth) / 2
	if startX < 0 {
		startX = 0
	}

	placeOverlayAt(startX, startY, overlayWidth, overlayLines, bgLines)
	return strings.Join(bgLines[:height], "\n")
}

// placeOverlayAt composites overlayLines into bgLines starting at (startX, startY),
// preserving background content to the right of the overlay. It is ANSI-aware:
// escape sequences in the background lines are skipped during visual-column math.
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

// visualToRuneIndex returns the rune index in s corresponding to visual column
// col, skipping ANSI CSI escape sequences and using cell-width-aware counting.
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

// ansiEscapeLen returns the number of runes consumed by an ANSI CSI escape
// sequence starting at runes[i], or 0 if no escape sequence starts there.
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

// truncateToast truncates s so its visual width does not exceed maxWidth.
func truncateToast(s string, maxWidth int) string {
	if lipgloss.Width(s) <= maxWidth {
		return s
	}
	suffix := "..."
	limit := maxWidth - lipgloss.Width(suffix)
	if limit <= 0 {
		return suffix
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > limit {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + suffix
}
