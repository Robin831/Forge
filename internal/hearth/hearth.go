// Package hearth provides The Forge's TUI dashboard using Bubbletea.
//
// The TUI has three panels in a horizontal layout:
//   - Queue (left): Pending beads from anvils
//   - Workers (center): Active Smith processes
//   - Event Log (right): Recent events from the state DB
//
// Tab switches focus between panels, j/k scrolls the focused panel,
// q quits the app.
package hearth

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Panel identifies a TUI panel.
type Panel int

const (
	PanelQueue   Panel = iota
	PanelWorkers
	PanelEvents
)

// QueueItem represents a bead in the queue panel.
type QueueItem struct {
	BeadID   string
	Title    string
	Anvil    string
	Priority int
	Status   string
}

// WorkerItem represents a worker in the workers panel.
type WorkerItem struct {
	ID       string
	BeadID   string
	Anvil    string
	Status   string
	Duration string
	CostUSD  float64
}

// EventItem represents an event in the event log panel.
type EventItem struct {
	Timestamp string
	Type      string
	Message   string
	BeadID    string
}

// Model is the Bubbletea model for the Hearth TUI.
type Model struct {
	// Panels
	queue   []QueueItem
	workers []WorkerItem
	events  []EventItem

	// State
	focused      Panel
	queueScroll  int
	workerScroll int
	eventScroll  int
	width        int
	height       int
	ready        bool
}

// NewModel creates a new Hearth TUI model.
func NewModel() Model {
	return Model{
		focused: PanelQueue,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.SetWindowTitle("The Forge — Hearth")
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "tab":
			m.focused = (m.focused + 1) % 3

		case "shift+tab":
			m.focused = (m.focused + 2) % 3

		case "j", "down":
			m.scrollDown()

		case "k", "up":
			m.scrollUp()
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

	case UpdateQueueMsg:
		m.queue = msg.Items

	case UpdateWorkersMsg:
		m.workers = msg.Items

	case UpdateEventsMsg:
		m.events = msg.Items
	}

	return m, nil
}

// View implements tea.Model.
func (m Model) View() string {
	if !m.ready {
		return "Initializing The Forge..."
	}

	// Calculate panel widths (roughly thirds)
	panelWidth := (m.width - 4) / 3 // 4 for borders/gaps
	if panelWidth < 20 {
		panelWidth = 20
	}
	contentHeight := m.height - 4 // header + footer

	// Build panels
	queuePanel := m.renderQueue(panelWidth, contentHeight)
	workerPanel := m.renderWorkers(panelWidth, contentHeight)
	eventPanel := m.renderEvents(panelWidth, contentHeight)

	// Header
	header := headerStyle.Width(m.width).Render("🔥 The Forge — Hearth Dashboard")

	// Join panels horizontally
	panels := lipgloss.JoinHorizontal(lipgloss.Top,
		queuePanel,
		workerPanel,
		eventPanel,
	)

	// Footer
	footer := footerStyle.Width(m.width).Render(
		"Tab: switch panel • j/k: scroll • q: quit",
	)

	return lipgloss.JoinVertical(lipgloss.Left, header, panels, footer)
}

// scrollDown scrolls the focused panel down.
func (m *Model) scrollDown() {
	switch m.focused {
	case PanelQueue:
		if m.queueScroll < len(m.queue)-1 {
			m.queueScroll++
		}
	case PanelWorkers:
		if m.workerScroll < len(m.workers)-1 {
			m.workerScroll++
		}
	case PanelEvents:
		if m.eventScroll < len(m.events)-1 {
			m.eventScroll++
		}
	}
}

// scrollUp scrolls the focused panel up.
func (m *Model) scrollUp() {
	switch m.focused {
	case PanelQueue:
		if m.queueScroll > 0 {
			m.queueScroll--
		}
	case PanelWorkers:
		if m.workerScroll > 0 {
			m.workerScroll--
		}
	case PanelEvents:
		if m.eventScroll > 0 {
			m.eventScroll--
		}
	}
}

// renderQueue renders the queue panel.
func (m Model) renderQueue(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelQueue {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render(fmt.Sprintf("Queue (%d)", len(m.queue)))

	var lines []string
	lines = append(lines, title)

	if len(m.queue) == 0 {
		lines = append(lines, dimStyle.Render("No pending beads"))
	} else {
		visible := visibleItems(m.queueScroll, len(m.queue), height-3)
		for i := visible.start; i < visible.end; i++ {
			item := m.queue[i]
			priority := priorityStyle(item.Priority)
			line := fmt.Sprintf("%s %s %s", priority, item.BeadID, truncate(item.Title, width-20))
			if i == m.queueScroll {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderWorkers renders the workers panel.
func (m Model) renderWorkers(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelWorkers {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render(fmt.Sprintf("Workers (%d)", len(m.workers)))

	var lines []string
	lines = append(lines, title)

	if len(m.workers) == 0 {
		lines = append(lines, dimStyle.Render("No active workers"))
	} else {
		visible := visibleItems(m.workerScroll, len(m.workers), height-3)
		for i := visible.start; i < visible.end; i++ {
			item := m.workers[i]
			status := workerStatusStyle(item.Status)
			line := fmt.Sprintf("%s %s %s", status, item.BeadID, item.Duration)
			if i == m.workerScroll {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderEvents renders the event log panel.
func (m Model) renderEvents(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelEvents {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render(fmt.Sprintf("Events (%d)", len(m.events)))

	var lines []string
	lines = append(lines, title)

	if len(m.events) == 0 {
		lines = append(lines, dimStyle.Render("No events"))
	} else {
		visible := visibleItems(m.eventScroll, len(m.events), height-3)
		for i := visible.start; i < visible.end; i++ {
			item := m.events[i]
			line := fmt.Sprintf("%s %s %s", dimStyle.Render(item.Timestamp), eventTypeStyle(item.Type), truncate(item.Message, width-25))
			if i == m.eventScroll {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// --- Messages for updating panel data ---

// UpdateQueueMsg updates the queue panel.
type UpdateQueueMsg struct{ Items []QueueItem }

// UpdateWorkersMsg updates the workers panel.
type UpdateWorkersMsg struct{ Items []WorkerItem }

// UpdateEventsMsg updates the event log panel.
type UpdateEventsMsg struct{ Items []EventItem }

// --- Styles ---

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("208")).
			Align(lipgloss.Center).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Align(lipgloss.Center).
			Padding(0, 1)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	focusedPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("208")).
				Padding(0, 1)

	panelTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("255")).
			MarginBottom(1)

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("208"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
)

// priorityStyle returns a colored priority indicator.
func priorityStyle(p int) string {
	colors := map[int]string{
		0: "196", // red (critical)
		1: "208", // orange (high)
		2: "226", // yellow (medium)
		3: "75",  // blue (low)
		4: "240", // gray (backlog)
	}
	color, ok := colors[p]
	if !ok {
		color = "240"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(fmt.Sprintf("P%d", p))
}

// workerStatusStyle returns a colored status indicator.
func workerStatusStyle(status string) string {
	switch status {
	case "running":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Render("●")
	case "reviewing":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("◐")
	case "done":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Render("✓")
	case "failed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("○")
	}
}

// eventTypeStyle returns a styled event type.
func eventTypeStyle(t string) string {
	switch {
	case strings.Contains(t, "pass") || strings.Contains(t, "done") || strings.Contains(t, "merged"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Render(t)
	case strings.Contains(t, "fail") || strings.Contains(t, "reject") || strings.Contains(t, "error"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(t)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Render(t)
	}
}

// --- Helpers ---

type visibleRange struct {
	start, end int
}

func visibleItems(scroll, total, viewHeight int) visibleRange {
	if viewHeight <= 0 {
		viewHeight = 10
	}
	start := scroll
	end := start + viewHeight
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	return visibleRange{start: start, end: end}
}

func truncate(s string, maxLen int) string {
	if maxLen < 4 {
		maxLen = 4
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
