package ledger

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const (
	// eventPanelContentH is the number of content rows visible in the event panel.
	eventPanelContentH = 5
	// eventRetentionCap is the maximum number of events retained in memory.
	eventRetentionCap = 100
)

// EventLevel indicates the severity of a log entry.
type EventLevel int

const (
	EventInfo  EventLevel = iota
	EventWarn
	EventError
)

// EventEntry is one entry in the Ledger's in-memory event/error log.
type EventEntry struct {
	Timestamp string
	Level     EventLevel
	Message   string
}

// eventPanelH returns the number of terminal rows consumed by the event panel
// when it is visible: 1 separator + 1 title + eventPanelContentH content rows.
// Returns 0 when the panel is hidden.
func (m *Model) eventPanelH() int {
	if !m.showEventPanel {
		return 0
	}
	return 2 + eventPanelContentH
}

// hasEventErrors reports whether any EventError entries exist in the log.
func (m *Model) hasEventErrors() bool {
	for _, e := range m.eventLog {
		if e.Level == EventError {
			return true
		}
	}
	return false
}

// addEvent appends an event to the in-memory log. Old entries are evicted
// when eventRetentionCap is reached (FIFO).
func (m *Model) addEvent(level EventLevel, msg string) {
	m.eventLog = append(m.eventLog, EventEntry{
		Timestamp: time.Now().Format("15:04:05"),
		Level:     level,
		Message:   msg,
	})
	if len(m.eventLog) > eventRetentionCap {
		m.eventLog = m.eventLog[len(m.eventLog)-eventRetentionCap:]
	}
}

// renderEventPanel renders the event/error panel. The panel consists of a
// separator line, a title bar, and eventPanelContentH content rows showing
// the most recent log entries (newest at the bottom).
func (m *Model) renderEventPanel() string {
	sepStyle   := lipgloss.NewStyle().Foreground(colorMuted)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	dimStyle   := lipgloss.NewStyle().Foreground(colorMuted)
	tsStyle    := lipgloss.NewStyle().Foreground(colorMuted)
	infoStyle  := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Dark: "252", Light: "240"})
	warnStyle  := lipgloss.NewStyle().Foreground(colorWarning)
	errStyle   := lipgloss.NewStyle().Foreground(colorDanger)

	sepW := max(m.width-2, 1)
	sep := sepStyle.Render(strings.Repeat("─", sepW))

	count := fmt.Sprintf(" (%d)", len(m.eventLog))
	title := titleStyle.Render("⚡ Activity") +
		dimStyle.Render(count) +
		dimStyle.Render("  E: hide")

	// Build all rendered lines from the event log, word-wrapping long messages.
	innerW := max(m.width-4, 10) // 2-char left padding + 2-char right margin
	var allLines []string
	for _, e := range m.eventLog {
		var icon string
		var msgStyle lipgloss.Style
		switch e.Level {
		case EventError:
			icon = "✗"
			msgStyle = errStyle
		case EventWarn:
			icon = "⚠"
			msgStyle = warnStyle
		default:
			icon = "·"
			msgStyle = infoStyle
		}
		prefix := tsStyle.Render(e.Timestamp) + " " + icon + " "
		prefixW := lipgloss.Width(prefix)
		available := max(innerW-prefixW, 10)

		words := strings.Fields(e.Message)
		var line strings.Builder
		lineW := 0
		first := true
		for _, word := range words {
			ww := lipgloss.Width(word)
			if lineW > 0 && lineW+1+ww > available {
				if first {
					allLines = append(allLines, "  "+prefix+msgStyle.Render(line.String()))
					first = false
				} else {
					allLines = append(allLines, "  "+strings.Repeat(" ", prefixW)+msgStyle.Render(line.String()))
				}
				line.Reset()
				lineW = 0
			}
			if lineW > 0 {
				line.WriteByte(' ')
				lineW++
			}
			line.WriteString(word)
			lineW += ww
		}
		if line.Len() > 0 || len(words) == 0 {
			if first {
				allLines = append(allLines, "  "+prefix+msgStyle.Render(line.String()))
			} else {
				allLines = append(allLines, "  "+strings.Repeat(" ", prefixW)+msgStyle.Render(line.String()))
			}
		}
	}

	// Show the most recent eventPanelContentH lines (newest at bottom).
	if len(allLines) > eventPanelContentH {
		allLines = allLines[len(allLines)-eventPanelContentH:]
	}
	// Pad with empty lines at the top to fill the panel to its fixed height.
	for len(allLines) < eventPanelContentH {
		allLines = append([]string{""}, allLines...)
	}

	var sb strings.Builder
	sb.WriteString(sep)
	sb.WriteByte('\n')
	sb.WriteString(title)
	for _, l := range allLines {
		sb.WriteByte('\n')
		sb.WriteString(l)
	}
	return sb.String()
}

// placeEventPanelOverlay composites the event panel over the bottom rows of
// the background string using the ANSI-aware compositor. The panel spans the
// full terminal width from the left edge.
func placeEventPanelOverlay(width, height int, panel, background string) string {
	panelLines := strings.Split(panel, "\n")
	bgLines := strings.Split(background, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}
	panelH := len(panelLines)
	startY := max(height-panelH, 0)
	placeOverlayAt(0, startY, width, panelLines, bgLines)
	return strings.Join(bgLines[:height], "\n")
}
