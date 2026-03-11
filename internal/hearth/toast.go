package hearth

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const (
	// toastDuration is how long a toast notification stays visible before auto-dismissal.
	toastDuration = 4 * time.Second

	// maxToasts is the maximum number of toasts shown simultaneously.
	maxToasts = 3

	// toastMaxWidth caps the message length to prevent overly wide toasts.
	toastMaxWidth = 60
)

// toast is a temporary notification displayed at the bottom of the Hearth TUI.
type toast struct {
	id      int
	message string
	isError bool
}

// toastDismissMsg fires when a toast's auto-dismiss timer expires.
type toastDismissMsg struct{ id int }

// scheduleToastDismiss returns a Cmd that fires toastDismissMsg{id} after toastDuration.
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

// truncateToVisualWidth truncates s so its visual width (counting wide chars as
// 2 columns) does not exceed maxWidth, then appends suffix. If the string fits
// within maxWidth it is returned unchanged (suffix not added).
func truncateToVisualWidth(s string, maxWidth int, suffix string) string {
	if lipgloss.Width(s) <= maxWidth {
		return s
	}
	limit := maxWidth - lipgloss.Width(suffix)
	if limit <= 0 {
		return suffix
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > limit {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + suffix
}

// renderToasts renders the active toasts stacked vertically (newest last, so
// newest appears at the bottom closest to the footer).
func (m *Model) renderToasts() string {
	if len(m.toasts) == 0 {
		return ""
	}
	parts := make([]string, len(m.toasts))
	for i, t := range m.toasts {
		text := truncateToVisualWidth(t.message, toastMaxWidth, "...")
		if t.isError {
			parts[i] = toastErrorStyle.Render(text)
		} else {
			parts[i] = toastSuccessStyle.Render(text)
		}
	}
	return strings.Join(parts, "\n")
}

// toastEventKey returns a fingerprint string for an EventItem, used to detect
// which events are new since the last poll cycle.
func toastEventKey(ev EventItem) string {
	return ev.Timestamp + "\x00" + ev.Type + "\x00" + ev.Message
}

// toastForEvent returns the toast message, error flag, and true if the event
// type warrants a toast notification. Returns ("", false, false) otherwise.
func toastForEvent(ev EventItem) (message string, isError bool, ok bool) {
	msg := ev.Message
	if msg == "" {
		msg = ev.BeadID
	}
	// Trim to first line only — event messages can include multi-line details.
	if nl := strings.IndexByte(msg, '\n'); nl != -1 {
		msg = msg[:nl]
	}

	switch ev.Type {
	case "warden_pass":
		return "✓ " + firstOf(msg, "Review passed"), false, true

	case "pr_created":
		return "✓ " + firstOf(msg, "PR created"), false, true

	case "pr_merged":
		return "✓ " + firstOf(msg, "PR merged"), false, true

	case "bead_closed":
		beadMsg := "Bead closed"
		if ev.BeadID != "" {
			beadMsg = ev.BeadID + " closed"
		}
		return "✓ " + firstOf(msg, beadMsg), false, true

	case "smith_failed":
		return "✗ " + firstOf(msg, "Smith failed"), true, true

	case "pr_merge_failed":
		return "✗ " + firstOf(msg, "PR merge failed"), true, true

	case "lifecycle_exhausted":
		beadMsg := "Needs attention"
		if ev.BeadID != "" {
			beadMsg = ev.BeadID + " needs attention"
		}
		return "⚠ " + firstOf(msg, beadMsg), true, true

	case "crucible_complete":
		return "✓ " + firstOf(msg, "Crucible complete"), false, true
	}

	return "", false, false
}

// firstOf returns s if non-empty, otherwise fallback.
func firstOf(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// placeToastsOverlay places the overlay string at the bottom-center of the
// background, positioned just above the footer (last footerH rows).
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

	// Position above the footer
	startY := height - footerH - overlayHeight
	if startY < 0 {
		startY = 0
	}
	startX := (width - overlayWidth) / 2
	if startX < 0 {
		startX = 0
	}

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

	return strings.Join(bgLines[:height], "\n")
}
