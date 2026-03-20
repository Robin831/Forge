package ledger

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
