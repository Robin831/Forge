// Package hearth provides The Forge's TUI dashboard using Bubbletea.
//
// The TUI has three panels in a vertical split layout:
//   - Queue (top left): Pending beads from anvils
//   - Workers (top right): Active Smith processes
//   - Event Log (bottom): Recent events from the state DB
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
	PanelQueue Panel = iota
	PanelWorkers
	PanelEvents
	PanelAttention

	// Event panel rendering constants
	eventPanelInteriorPadding = 4
	eventPanelMinWidth        = 1
	eventTimestampWidth       = 9  // "HH:MM:SS "
	eventMsgMinWidth          = 20 // Minimum width before msg moves to next line

	// Number of panels in the tab cycle.
	panelCount = 4
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
	ID            string
	BeadID        string
	Anvil         string
	Status        string
	Duration      string
	CostUSD       float64
	Type          string   // "smith", "warden", "temper", "cifix", "reviewfix"
	LastLog       string   // Last line from the worker log
	PID           int      // Process ID for kill
	LogPath       string   // Path to the worker's log file
	ActivityLines []string // Recent parsed activity from the log (tool calls, thinking, text)
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
	queue     []QueueItem
	workers   []WorkerItem
	events    []EventItem
	attention []AttentionItem

	// Data source for polling
	data *DataSource

	// Callback for killing a worker (set by the caller)
	OnKill func(workerID string, pid int)

	// Callback for resetting a bead to open (set by the caller)
	OnResetBead func(beadID, anvil string)

	// State
	focused          Panel
	queueScroll      int
	workerScroll     int
	eventScroll      int
	attentionScroll  int
	eventAutoScroll  bool // true = follow new events
	prevEventCount   int  // track event count for auto-scroll
	width            int
	height           int
	ready            bool

	// Event rendering cache
	eventLinesCache        []string
	eventWidthCache        int
	eventSelectedIdxCache  int
	eventCountCache        int
	eventRevision          int // incremented on every UpdateEventsMsg to detect content changes
	eventRevisionCache     int
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
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.SetWindowTitle("The Forge — Hearth")}

	// Start the data tick cycle and do an initial fetch
	if m.data != nil {
		cmds = append(cmds, Tick())
		cmds = append(cmds, FetchAll(m.data.Ctx, m.data.DB, m.data.Anvils))
	}

	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "tab":
			m.focused = (m.focused + 1) % panelCount

		case "shift+tab":
			m.focused = (m.focused + panelCount - 1) % panelCount

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

		case "R":
			// Reset selected attention item to open
			if m.focused == PanelAttention && len(m.attention) > 0 &&
				m.attentionScroll < len(m.attention) {
				item := m.attention[m.attentionScroll]
				if m.OnResetBead != nil {
					m.OnResetBead(item.BeadID, item.Anvil)
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
		m.eventRevision++
		// Auto-scroll to bottom if enabled and new events arrived
		if m.eventAutoScroll && len(msg.Items) > m.prevEventCount {
			if len(msg.Items) > 0 {
				m.eventScroll = 0 // Events are newest-first from DB
			}
		}
		m.prevEventCount = len(msg.Items)

	case UpdateAttentionMsg:
		m.attention = msg.Items
		// Clamp scroll position if items were removed
		if m.attentionScroll >= len(m.attention) && len(m.attention) > 0 {
			m.attentionScroll = len(m.attention) - 1
		}

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
func (m *Model) View() string {
	if !m.ready {
		return "Initializing The Forge..."
	}

	queueWidth, workerWidth, eventWidth := m.getPanelWidths()
	contentHeight := m.height - 4 // header + footer
	if contentHeight < 0 {
		contentHeight = 0
	}

	// Split content height: top 3 panels get ~65%, attention panel gets ~35%.
	// Minimum 4 rows for the attention panel.
	attnHeight := contentHeight * 35 / 100
	if attnHeight < 4 {
		attnHeight = 4
	}
	if attnHeight > contentHeight {
		attnHeight = contentHeight
	}
	topHeight := contentHeight - attnHeight

	// Build top panels
	queuePanel := m.renderQueue(queueWidth, topHeight)
	workerPanel := m.renderWorkers(workerWidth, topHeight)
	eventPanel := m.renderEvents(eventWidth, topHeight)

	// Build attention panel (full width)
	attnPanel := m.renderAttention(m.width, attnHeight)

	// Header
	header := headerStyle.Width(m.width).Render("🔥 The Forge — Hearth Dashboard")

	// Footer
	footer := footerStyle.Width(m.width).Render(
		"Tab: switch panel • j/k: scroll • K: kill worker • R: reset bead • f: follow events • q: quit",
	)

	// Final assembly
	topSection := lipgloss.JoinHorizontal(lipgloss.Top, queuePanel, workerPanel, eventPanel)
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		topSection,
		attnPanel,
		footer,
	)
}

func (m *Model) getPanelWidths() (queueWidth, workerWidth, eventWidth int) {
	remainingWidth := m.width - 4 // 4 for borders/gaps
	if remainingWidth < 0 {
		remainingWidth = 0
	}
	queueWidth = remainingWidth / 4
	workerWidth = remainingWidth / 4
	eventWidth = remainingWidth - queueWidth - workerWidth
	return
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
		_, _, eventWidth := m.getPanelWidths()
		totalLines := m.eventTotalLineCount(eventWidth)
		if m.eventScroll < totalLines-1 {
			m.eventScroll++
		}
	case PanelAttention:
		if m.attentionScroll < len(m.attention)-1 {
			m.attentionScroll++
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
	case PanelAttention:
		if m.attentionScroll > 0 {
			m.attentionScroll--
		}
	}
}

// renderQueue renders the queue panel.
func (m *Model) renderQueue(width, height int) string {
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

// renderWorkers splits the center Workers panel into two vertical sub-panels:
// top shows the worker list, bottom shows the live activity log for the
// selected worker. Uses lipgloss.JoinVertical for the split.
func (m *Model) renderWorkers(width, height int) string {
	// For very small panels, give all space to the list.
	// Otherwise enforce a minimum of 5 rows so renderWorkerList has enough room.
	listHeight := height * 6 / 10
	if height < 5 {
		listHeight = height
	} else {
		listHeight = max(listHeight, 5)
	}
	activityHeight := height - listHeight

	top := m.renderWorkerList(width, listHeight)
	bottom := m.renderWorkerActivity(width, activityHeight)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// renderWorkerList renders the top sub-panel: the list of active workers.
func (m *Model) renderWorkerList(width, height int) string {
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
		// height-2 (borders) - 2 (title + margin) = height-4
		visible := visibleItems(m.workerScroll, len(m.workers), height-4)
		for i := visible.start; i < visible.end; i++ {
			item := m.workers[i]
			status := workerStatusStyle(item.Status)
			phase := phaseTag(item.Type)
			mainLine := fmt.Sprintf("%s %s %s %s %s",
				status, phase, item.BeadID,
				dimStyle.Render(item.Anvil), item.Duration)
			if i == m.workerScroll {
				mainLine = selectedStyle.Render(mainLine)
			}
			lines = append(lines, mainLine)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderWorkerActivity renders the bottom sub-panel: a live activity log
// for the currently selected worker, parsed from its stream-json log file.
func (m *Model) renderWorkerActivity(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelWorkers {
		style = focusedPanelStyle.Width(width)
	}

	title := activityPanelTitleStyle.Render("Live Activity")

	var lines []string
	lines = append(lines, title)

	var activityLines []string
	if len(m.workers) > 0 && m.workerScroll < len(m.workers) {
		activityLines = m.workers[m.workerScroll].ActivityLines
	}

	if len(activityLines) == 0 {
		lines = append(lines, dimStyle.Render("No activity"))
	} else {
		// height-2 (borders) - 2 (title + margin) = height-4
		maxVisible := height - 4
		if maxVisible < 1 {
			maxVisible = 1
		}
		start := len(activityLines) - maxVisible
		if start < 0 {
			start = 0
		}
		for _, entry := range activityLines[start:] {
			lines = append(lines, truncate(entry, width-4))
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderEvents renders the event log panel with word-wrapped messages.
func (m *Model) renderEvents(width, height int) string {
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

	contentHeight := height - 3 // title + border rows

	if len(m.events) == 0 {
		lines = append(lines, dimStyle.Render("No events"))
	} else {
		allLines := m.renderAllEventLines(width)
		visible := visibleItems(m.eventScroll, len(allLines), contentHeight)
		for i := visible.start; i < visible.end; i++ {
			lines = append(lines, allLines[i])
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderAllEventLines flattens all events into a single slice of rendered lines.
// It uses eventLineCount for the selection-mapping pass to avoid a double full render.
// Caches the results to avoid redundant work.
func (m *Model) renderAllEventLines(width int) []string {
	// Calculate what the selected event index WOULD be.
	selectedEventIdx := -1
	cumulative := 0
	for i, event := range m.events {
		count := m.eventLineCount(event, width)
		if selectedEventIdx == -1 && cumulative+count > m.eventScroll {
			selectedEventIdx = i
		}
		cumulative += count
	}

	// Check if cache is valid.
	if m.eventWidthCache == width &&
		m.eventCountCache == len(m.events) &&
		m.eventSelectedIdxCache == selectedEventIdx &&
		m.eventRevisionCache == m.eventRevision &&
		m.eventLinesCache != nil {
		return m.eventLinesCache
	}

	// Cache invalid, perform full render.
	var allLines []string
	for i, event := range m.events {
		allLines = append(allLines, m.renderEventLines(event, i == selectedEventIdx, width)...)
	}

	// Update cache
	m.eventLinesCache = allLines
	m.eventWidthCache = width
	m.eventCountCache = len(m.events)
	m.eventSelectedIdxCache = selectedEventIdx
	m.eventRevisionCache = m.eventRevision

	return allLines
}

type eventLayout struct {
	interiorWidth int
	prefixVisLen  int
	msgWidth      int
	beadTag       string
}

func (m *Model) getEventLayout(item EventItem, panelWidth int) eventLayout {
	beadTag := ""
	if item.BeadID != "" {
		beadTag = "[" + item.BeadID + "] "
	}

	// Interior width: subtract border (1 each side = 2) + padding (1 each side = 2) = 4 total
	interiorWidth := panelWidth - eventPanelInteriorPadding
	if interiorWidth < eventPanelMinWidth {
		interiorWidth = eventPanelMinWidth
	}

	// Visual prefix length: "HH:MM:SS "(9) + type + " " + beadTag
	prefixVisLen := eventTimestampWidth + len(item.Type) + 1 + len(beadTag)
	msgWidth := interiorWidth - prefixVisLen

	return eventLayout{
		interiorWidth: interiorWidth,
		prefixVisLen:  prefixVisLen,
		msgWidth:      msgWidth,
		beadTag:       beadTag,
	}
}

// eventTotalLineCount returns the total number of rendered lines across all events
// without allocating styled strings. Used by scrollDown for cheap bounds checking.
func (m *Model) eventTotalLineCount(width int) int {
	if m.eventWidthCache == width && m.eventCountCache == len(m.events) && m.eventRevisionCache == m.eventRevision && m.eventLinesCache != nil {
		return len(m.eventLinesCache)
	}

	total := 0
	for _, event := range m.events {
		total += m.eventLineCount(event, width)
	}
	return total
}

// eventLineCount returns the number of lines renderEventLines would produce for item
// without performing any string formatting or allocation beyond wrap counting.
func (m *Model) eventLineCount(item EventItem, width int) int {
	layout := m.getEventLayout(item, width)

	var n int
	if layout.msgWidth < eventMsgMinWidth {
		// header line + wrapped message lines
		n = 1 + wordWrapCount(item.Message, layout.interiorWidth-2)
	} else {
		n = wordWrapCount(item.Message, layout.msgWidth)
	}
	if n > maxEventLines {
		n = maxEventLines
	}
	return n
}

// renderEventLines renders a single event as one or more wrapped lines.
// The timestamp and event type stay on the first line; the message body wraps
// onto continuation lines if it exceeds the available width.
// At most maxEventLines lines are produced to prevent long error messages from
// overflowing into adjacent hearth panels.
const maxEventLines = 3

func (m *Model) renderEventLines(item EventItem, selected bool, panelWidth int) []string {
	layout := m.getEventLayout(item, panelWidth)

	capWrapped := func(wrapped []string) []string {
		if len(wrapped) > maxEventLines {
			wrapped = append(wrapped[:maxEventLines-1], "…")
		}
		return wrapped
	}

	var lines []string
	if layout.msgWidth < eventMsgMinWidth {
		// Prefix is too wide for the current panel width, put message on its own lines
		header := fmt.Sprintf("%s %s %s",
			dimStyle.Render(item.Timestamp),
			eventTypeStyle(item.Type),
			dimStyle.Render(layout.beadTag))
		if selected {
			header = selectedStyle.Render(header)
		}
		lines = append(lines, header)

		// Message starts on next line, indented slightly
		wrapped := capWrapped(wordWrap(item.Message, layout.interiorWidth-2))
		for _, part := range wrapped {
			line := "  " + dimStyle.Render(part)
			if selected {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	} else {
		// Message starts on the same line as the prefix
		wrapped := capWrapped(wordWrap(item.Message, layout.msgWidth))
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}

		indent := strings.Repeat(" ", layout.prefixVisLen)
		for i, part := range wrapped {
			var line string
			if i == 0 {
				line = fmt.Sprintf("%s %s %s%s",
					dimStyle.Render(item.Timestamp),
					eventTypeStyle(item.Type),
					dimStyle.Render(layout.beadTag),
					part)
			} else {
				line = indent + dimStyle.Render(part)
			}
			if selected {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// renderAttention renders the full-width "Needs Human Attention" panel at the
// bottom of the dashboard. It lists beads with needs_human=1 (exhausted retries)
// or lifecycle_exhausted events, along with the reason and a suggested remediation.
func (m *Model) renderAttention(width, height int) string {
	style := panelStyle.Width(width - 4) // subtract borders
	if m.focused == PanelAttention {
		style = focusedPanelStyle.Width(width - 4)
	}

	count := len(m.attention)
	badge := ""
	if count > 0 {
		badge = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render(fmt.Sprintf(" (%d)", count))
	}
	title := attentionTitleStyle.Render("Needs Human Attention") + badge

	var lines []string
	lines = append(lines, title)

	if count == 0 {
		lines = append(lines, dimStyle.Render("No beads need attention"))
	} else {
		// Each item occupies 2 rows: main line + suggestion line.
		// height - 2 (borders) - 2 (title + margin) = height-4 available rows.
		maxVisible := (height - 4) / 2
		if maxVisible < 1 {
			maxVisible = 1
		}
		visible := visibleItems(m.attentionScroll, count, maxVisible)
		for i := visible.start; i < visible.end; i++ {
			item := m.attention[i]
			selected := i == m.attentionScroll && m.focused == PanelAttention

			reasonStr := attentionReasonStyle(item.Reason)
			mainLine := fmt.Sprintf("%s  %s  %s  %s",
				lipgloss.NewStyle().Bold(true).Render(item.BeadID),
				dimStyle.Render(item.Anvil),
				reasonStr,
				dimStyle.Render(item.UpdatedAt),
			)
			suggLine := "  " + dimStyle.Render("→ "+item.Suggestion)

			if selected {
				mainLine = selectedStyle.Render(item.BeadID+"  "+item.Anvil+"  "+item.Reason+"  "+item.UpdatedAt) + "  " + dimStyle.Render("[R: reset to open]")
				suggLine = "  " + selectedStyle.Render("→ "+item.Suggestion)
			}
			lines = append(lines, mainLine)
			lines = append(lines, suggLine)
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

	activityPanelTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("245")).
			MarginBottom(1)

	attentionTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("196")).
				MarginBottom(1)
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
	case "monitoring":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("○")
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

// phaseTag returns a colored [phase] tag for the active pipeline component.
// Colors: smith=yellow, temper=cyan, warden=magenta, bellows=blue, idle=gray.
func phaseTag(phase string) string {
	switch phase {
	case "smith":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("[smith]")
	case "temper":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Render("[temper]")
	case "warden":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("201")).Render("[warden]")
	case "bellows":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("[bellows]")
	case "cifix":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("[cifix]")
	case "reviewfix":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Render("[reviewfix]")
	case "rebase":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render("[rebase]")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("[idle]")
	}
}

// attentionReasonStyle returns a colored reason string for the attention panel.
func attentionReasonStyle(reason string) string {
	switch {
	case strings.Contains(reason, "CI fix"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(reason)
	case strings.Contains(reason, "Review fix"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Render(reason)
	case strings.Contains(reason, "Rebase"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Render(reason)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render(reason)
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

// wordWrapCount returns the number of lines wordWrap would produce without
// allocating the line strings. Mirrors the logic in wordWrap exactly.
func wordWrapCount(s string, maxWidth int) int {
	if maxWidth < 1 {
		maxWidth = 1
	}

	count := 0
	paragraphs := strings.Split(s, "\n")
	for _, pStr := range paragraphs {
		if pStr == "" {
			if len(paragraphs) > 1 {
				count++
			}
			continue
		}

		p := []rune(pStr)
		for len(p) > maxWidth {
			breakAt := -1
			for i := maxWidth; i >= maxWidth/2; i-- {
				if i < len(p) && p[i] == ' ' {
					breakAt = i
					break
				}
			}
			if breakAt == -1 {
				breakAt = maxWidth
			}
			count++
			p = p[breakAt:]
			for len(p) > 0 && p[0] == ' ' {
				p = p[1:]
			}
		}
		if len(p) > 0 {
			count++
		}
	}

	if count == 0 {
		return 1 // wordWrap returns [""] for empty input
	}
	return count
}

// wordWrap splits s into lines of at most maxWidth runes (by rune count, not
// display width), preferring to break at spaces. Newlines in s are preserved
// as hard line breaks; leading indentation on each input line is kept intact.
func wordWrap(s string, maxWidth int) []string {
	if maxWidth < 1 {
		maxWidth = 1
	}

	var result []string
	paragraphs := strings.Split(s, "\n")
	for _, pStr := range paragraphs {
		if strings.TrimSpace(pStr) == "" {
			if len(paragraphs) > 1 {
				result = append(result, "")
			}
			continue
		}

		p := []rune(pStr)
		for len(p) > maxWidth {
			breakAt := -1
			// Scan backwards from maxWidth looking for a space to break at.
			// Include position maxWidth itself (the char just past the limit)
			// so that a space landing exactly there produces a clean line.
			end := maxWidth
			if end >= len(p) {
				end = len(p) - 1
			}
			for i := end; i >= maxWidth/2; i-- {
				if p[i] == ' ' {
					breakAt = i
					break
				}
			}
			if breakAt == -1 {
				breakAt = maxWidth
			}
			result = append(result, string(p[:breakAt]))
			p = p[breakAt:]
			// Consume the space we broke at.
			for len(p) > 0 && p[0] == ' ' {
				p = p[1:]
			}
		}
		if len(p) > 0 {
			result = append(result, string(p))
		}
	}

	if len(result) == 0 {
		return []string{""}
	}
	return result
}
