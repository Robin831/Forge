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
	Type     string // "smith", "warden", "temper", "cifix", "reviewfix"
	LastLog  string // Last line from the worker log
	PID      int    // Process ID for kill
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

	// Data source for polling
	data *DataSource

	// Callback for killing a worker (set by the caller)
	OnKill func(workerID string, pid int)

	// State
	focused        Panel
	queueScroll    int
	workerScroll   int
	eventScroll    int
	eventAutoScroll bool  // true = follow new events
	prevEventCount int   // track event count for auto-scroll
	width          int
	height         int
	ready          bool
}

// NewModel creates a new Hearth TUI model.
// Pass nil for DataSource to run in display-only mode (no polling).
func NewModel(ds *DataSource) Model {
	return Model{
		focused:         PanelQueue,
		data:            ds,
		eventAutoScroll: true,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.SetWindowTitle("The Forge — Hearth")}

	// Start the data tick cycle and do an initial fetch
	if m.data != nil {
		cmds = append(cmds, Tick())
		cmds = append(cmds, FetchAll(m.data.Ctx, m.data.DB, m.data.Anvils))
	}

	return tea.Batch(cmds...)
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
			// Disable auto-scroll when user manually scrolls events
			if m.focused == PanelEvents {
				m.eventAutoScroll = false
			}

		case "k", "up":
			m.scrollUp()
			if m.focused == PanelEvents {
				m.eventAutoScroll = false
			}

		case "f", "F":
			// Toggle follow mode (auto-scroll) for events
			if m.focused == PanelEvents {
				m.eventAutoScroll = !m.eventAutoScroll
				if m.eventAutoScroll {
					m.eventScroll = 0
				}
			}

		case "K":
			// Kill selected worker
			if m.focused == PanelWorkers && len(m.workers) > 0 &&
				m.workerScroll < len(m.workers) {
				w := m.workers[m.workerScroll]
				if m.OnKill != nil && w.PID > 0 {
					m.OnKill(w.ID, w.PID)
				}
			}
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
		// Auto-scroll to bottom if enabled and new events arrived
		if m.eventAutoScroll && len(msg.Items) > m.prevEventCount {
			if len(msg.Items) > 0 {
				m.eventScroll = 0 // Events are newest-first from DB
			}
		}
		m.prevEventCount = len(msg.Items)

	case TickMsg:
		// On each tick, refresh all panels and schedule the next tick
		if m.data != nil {
			return m, tea.Batch(
				Tick(),
				FetchAll(m.data.Ctx, m.data.DB, m.data.Anvils),
			)
		}
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
		"Tab: switch panel • j/k: scroll • K: kill worker • f: follow events • q: quit",
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
			anvil := dimStyle.Render(item.Anvil)
			line := fmt.Sprintf("%s %s %s %s", priority, item.BeadID, anvil, truncate(item.Title, width-28))
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
			typeIcon := workerTypeIcon(item.Type)
			mainLine := fmt.Sprintf("%s %s %s %s %s",
				status, typeIcon, item.BeadID,
				dimStyle.Render(item.Anvil), item.Duration)
			if i == m.workerScroll {
				mainLine = selectedStyle.Render(mainLine)
			}
			lines = append(lines, mainLine)
			// Show last log line for the selected worker
			if i == m.workerScroll && item.LastLog != "" {
				lines = append(lines, dimStyle.Render("  "+truncate(item.LastLog, width-6)))
			}
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

	scrollIndicator := ""
	if !m.eventAutoScroll {
		scrollIndicator = dimStyle.Render(" ⏸")
	}
	title := panelTitleStyle.Render(fmt.Sprintf("Events (%d)%s", len(m.events), scrollIndicator))

	var lines []string
	lines = append(lines, title)

	if len(m.events) == 0 {
		lines = append(lines, dimStyle.Render("No events"))
	} else {
		visible := visibleItems(m.eventScroll, len(m.events), height-3)
		for i := visible.start; i < visible.end; i++ {
			item := m.events[i]
			beadTag := ""
			if item.BeadID != "" {
				beadTag = dimStyle.Render("["+item.BeadID+"] ")
			}
			line := fmt.Sprintf("%s %s %s%s",
				dimStyle.Render(item.Timestamp),
				eventTypeStyle(item.Type),
				beadTag,
				truncate(item.Message, width-30))
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

// KillWorkerMsg requests killing the selected worker by PID.
type KillWorkerMsg struct {
	WorkerID string
	PID      int
}

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

// workerTypeIcon returns a short icon for the worker type.
func workerTypeIcon(t string) string {
	switch t {
	case "smith":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render("⚒")
	case "warden":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Render("⛨")
	case "temper":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("🔥")
	case "cifix":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("🔧")
	case "reviewfix":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Render("📝")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("?")
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
