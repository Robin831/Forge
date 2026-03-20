package ledger

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Lane indices for the kanban board.
const (
	LaneOpen       = 0
	LaneInProgress = 1
	LaneInReview   = 2
	LaneClosed     = 3
	laneCount      = 4
)

// laneTitle returns the display title for a lane index.
func laneTitle(lane int) string {
	switch lane {
	case LaneOpen:
		return "Open"
	case LaneInProgress:
		return "In Progress"
	case LaneInReview:
		return "In Review"
	case LaneClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

// laneColor returns the accent color for a lane header.
func laneColor(lane int) lipgloss.AdaptiveColor {
	switch lane {
	case LaneOpen:
		return colorInfo
	case LaneInProgress:
		return colorWarning
	case LaneInReview:
		return lipgloss.AdaptiveColor{Dark: "42", Light: "28"} // green
	case LaneClosed:
		return colorMuted
	default:
		return colorMuted
	}
}

// kanbanState holds kanban-view-specific state.
type kanbanState struct {
	activeLane int                       // 0-3
	lanes      [laneCount][]Bead         // beads per lane
	laneVP     [laneCount]scrollViewport // per-lane scroll state
}

// moveBeadMsg is returned after a bd update command finishes.
type moveBeadMsg struct {
	Err error
}

// assignLane determines which kanban lane a bead belongs to.
func assignLane(b Bead) int {
	if b.Status == "closed" {
		return LaneClosed
	}
	if b.HasPR && b.Status != "closed" {
		return LaneInReview
	}
	if b.Status == "in_progress" {
		return LaneInProgress
	}
	return LaneOpen
}

// populateLanes distributes beads into kanban lanes.
// Closed beads are filtered to the last 7 days.
func populateLanes(beads []Bead) [laneCount][]Bead {
	var lanes [laneCount][]Bead
	cutoff := time.Now().AddDate(0, 0, -7)

	for _, b := range beads {
		lane := assignLane(b)
		if lane == LaneClosed {
			if b.ClosedAt != nil && b.ClosedAt.Before(cutoff) {
				continue
			}
			if b.ClosedAt == nil && (b.UpdatedAt == nil || b.UpdatedAt.Before(cutoff)) {
				continue
			}
		}
		lanes[lane] = append(lanes[lane], b)
	}
	return lanes
}

// isBlocked reports whether a bead has unmet dependencies.
func isBlocked(b Bead) bool {
	return len(b.DependsOn) > 0
}

// laneStatusForIndex returns the bd status string for a target lane.
func laneStatusForIndex(lane int) string {
	switch lane {
	case LaneOpen:
		return "open"
	case LaneInProgress:
		return "in_progress"
	case LaneInReview:
		return "in_progress" // no "in_review" status; keep in_progress
	case LaneClosed:
		return "closed"
	default:
		return "open"
	}
}

// updateKanban handles key messages for the kanban view.
func (m *Model) updateKanban(msg tea.KeyMsg) tea.Cmd {
	lane := m.kanban.activeLane
	total := len(m.kanban.lanes[lane])

	switch msg.String() {
	case "h", "left":
		if m.kanban.activeLane > 0 {
			m.kanban.activeLane--
		}
	case "l", "right":
		if m.kanban.activeLane < laneCount-1 {
			m.kanban.activeLane++
		}
	case "j", "down":
		m.kanban.laneVP[lane].ScrollDown(total)
	case "k", "up":
		m.kanban.laneVP[lane].ScrollUp()
	case "H": // Shift+H: move bead left
		return m.moveBeadToLane(lane - 1)
	case "L": // Shift+L: move bead right
		return m.moveBeadToLane(lane + 1)
	case "tab":
		m.view = ViewList
	}
	return nil
}

// moveBeadToLane moves the currently selected bead to the target lane.
func (m *Model) moveBeadToLane(targetLane int) tea.Cmd {
	if targetLane < 0 || targetLane >= laneCount {
		return nil
	}
	lane := m.kanban.activeLane
	beads := m.kanban.lanes[lane]
	if len(beads) == 0 {
		return nil
	}
	cursor := m.kanban.laneVP[lane].Selected()
	if cursor >= len(beads) {
		return nil
	}
	b := beads[cursor]
	targetStatus := laneStatusForIndex(targetLane)

	// Look up the anvil path for this bead.
	anvilPath, ok := m.anvils[b.Anvil]
	if !ok {
		return nil
	}

	// Optimistically move the bead in the UI.
	m.kanban.lanes[lane] = append(beads[:cursor], beads[cursor+1:]...)
	m.kanban.laneVP[lane].ClampToTotal(len(m.kanban.lanes[lane]))
	b.Status = targetStatus
	if targetLane == LaneInReview {
		b.HasPR = true
	}
	m.kanban.lanes[targetLane] = append(m.kanban.lanes[targetLane], b)

	// Shell out to bd in the background.
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var args []string
		if targetStatus == "closed" {
			args = []string{"close", b.ID}
		} else {
			args = []string{"update", b.ID, "--status=" + targetStatus}
		}
		_, err := bdExec(ctx, anvilPath, args...)
		return moveBeadMsg{Err: err}
	}
}

// refreshKanbanLanes repopulates the kanban lanes from the current bead list.
func (m *Model) refreshKanbanLanes() {
	m.kanban.lanes = populateLanes(m.beads)
	for i := range laneCount {
		m.kanban.laneVP[i].ClampToTotal(len(m.kanban.lanes[i]))
	}
}

// renderKanban renders the kanban board view.
func (m *Model) renderKanban() string {
	totalBeads := 0
	for i := range laneCount {
		totalBeads += len(m.kanban.lanes[i])
	}

	// Header
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 2)
	header := headerStyle.Render(fmt.Sprintf("⚒ Forge Ledger — Kanban (%d beads)", totalBeads))

	// Lane dimensions
	laneWidth := m.kanbanLaneWidth()
	// Available height: total - header(1) - detail(3) - footer(1) - lane_header(1)
	cardAreaHeight := max(m.height-6, 3)
	// Each card is 4 lines tall (priority+ID, title line 1, title line 2/anvil, blank separator).
	cardHeight := 4
	visibleCards := max(cardAreaHeight/cardHeight, 1)

	// Render each lane
	var laneCols []string
	for i := range laneCount {
		col := m.renderLane(i, laneWidth, visibleCards, cardHeight, cardAreaHeight, i == m.kanban.activeLane)
		laneCols = append(laneCols, col)
	}

	board := lipgloss.JoinHorizontal(lipgloss.Top, laneCols...)

	// Detail preview for selected card
	detail := m.renderKanbanDetail(laneWidth)

	// Footer
	footerStyle := lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)
	var errNote string
	if m.err != nil {
		errNote = lipgloss.NewStyle().Foreground(colorDanger).Render(fmt.Sprintf("  ⚠ %v", m.err))
	}
	footer := footerStyle.Render("h/l: lane  j/k: card  H/L: move bead  Tab: list  q: quit") + errNote

	return header + "\n" + board + "\n" + detail + "\n" + footer
}

// kanbanLaneWidth computes the width for each lane column.
func (m *Model) kanbanLaneWidth() int {
	w := max((m.width-5)/laneCount, 15)
	return w
}

// renderLane renders a single kanban lane column.
func (m *Model) renderLane(lane, width, visibleCards, cardHeight, areaHeight int, active bool) string {
	beads := m.kanban.lanes[lane]
	total := len(beads)

	// Lane header
	title := fmt.Sprintf(" %s (%d)", laneTitle(lane), total)
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(laneColor(lane)).
		Width(width).
		Align(lipgloss.Center)
	if active {
		headerStyle = headerStyle.
			Underline(true)
	}
	laneHeader := headerStyle.Render(truncate(title, width-2))

	// Adjust viewport for visible cards
	m.kanban.laneVP[lane].AdjustViewport(visibleCards, total)
	start, end := m.kanban.laneVP[lane].VisibleRange(visibleCards, total)

	var cards strings.Builder
	linesUsed := 0
	for idx := start; idx < end; idx++ {
		b := beads[idx]
		selected := active && idx == m.kanban.laneVP[lane].Selected()
		blocked := isBlocked(b)
		card := renderCard(b, width-2, selected, blocked)
		if linesUsed > 0 {
			cards.WriteByte('\n')
			linesUsed++
		}
		cards.WriteString(card)
		linesUsed += cardHeight - 1 // card itself is cardHeight-1 lines + 1 separator
	}

	// Pad remaining height so all lanes are the same height
	for linesUsed < areaHeight {
		cards.WriteByte('\n')
		linesUsed++
	}

	laneStyle := lipgloss.NewStyle().Width(width)
	if active {
		laneStyle = laneStyle.BorderLeft(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(colorAccent)
	}

	return laneStyle.Render(laneHeader + "\n" + cards.String())
}

// renderCard renders a single bead card for the kanban board.
func renderCard(b Bead, width int, selected, blocked bool) string {
	if width < 5 {
		width = 5
	}

	// Priority dot
	dot := priorityDot(b.Priority)

	// Line 1: dot + ID
	line1 := dot + " " + truncate(b.ID, width-3)

	// Line 2-3: title (up to 2 lines)
	titleLines := wrapTitle(b.Title, width-1)

	// Anvil tag
	anvilTag := ""
	if b.Anvil != "" {
		anvilTag = lipgloss.NewStyle().Foreground(colorMuted).Render("[" + truncate(b.Anvil, width-3) + "]")
	}

	var cardLines []string
	cardLines = append(cardLines, line1)
	cardLines = append(cardLines, titleLines...)
	if anvilTag != "" {
		cardLines = append(cardLines, anvilTag)
	}

	content := strings.Join(cardLines, "\n")

	style := lipgloss.NewStyle().
		Width(width).
		PaddingLeft(1)

	if blocked {
		style = style.
			BorderLeft(true).
			BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(colorDanger)
	}

	if selected {
		style = style.
			Bold(true).
			Background(lipgloss.AdaptiveColor{Dark: "237", Light: "254"})
	}

	return style.Render(content)
}

// priorityDot returns a colored dot character based on priority level.
func priorityDot(priority int) string {
	var color lipgloss.AdaptiveColor
	switch {
	case priority <= 0:
		color = colorDanger
	case priority == 1:
		color = colorWarning
	case priority == 2:
		color = colorInfo
	default:
		color = colorMuted
	}
	return lipgloss.NewStyle().Foreground(color).Render("●")
}

// wrapTitle wraps a title string into at most 2 lines at the given width.
func wrapTitle(title string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	if lipgloss.Width(title) <= width {
		return []string{title}
	}

	// Split at word boundaries, fill up to 2 lines.
	words := strings.Fields(title)
	var line1, line2 strings.Builder
	onLine1 := true

	for _, w := range words {
		if onLine1 {
			candidate := line1.String()
			if candidate != "" {
				candidate += " "
			}
			candidate += w
			if lipgloss.Width(candidate) <= width {
				if line1.Len() > 0 {
					line1.WriteByte(' ')
				}
				line1.WriteString(w)
			} else {
				if line1.Len() == 0 {
					// Single word longer than width.
					line1.WriteString(truncate(w, width))
					onLine1 = false
					continue
				}
				onLine1 = false
				// Start line 2 with this word.
				line2.WriteString(w)
			}
		} else {
			candidate := line2.String()
			if candidate != "" {
				candidate += " "
			}
			candidate += w
			if lipgloss.Width(candidate) <= width {
				if line2.Len() > 0 {
					line2.WriteByte(' ')
				}
				line2.WriteString(w)
			} else {
				// Truncate what we have.
				break
			}
		}
	}

	lines := []string{line1.String()}
	if line2.Len() > 0 {
		lines = append(lines, truncate(line2.String(), width))
	}
	return lines
}

// renderKanbanDetail renders the 3-line detail preview at the bottom.
func (m *Model) renderKanbanDetail(_ int) string {
	lane := m.kanban.activeLane
	beads := m.kanban.lanes[lane]
	if len(beads) == 0 {
		return strings.Repeat("\n", 2) // 3 empty lines
	}

	cursor := m.kanban.laneVP[lane].Selected()
	if cursor >= len(beads) {
		return strings.Repeat("\n", 2)
	}
	b := beads[cursor]

	detailWidth := max(m.width-4, 20)
	style := lipgloss.NewStyle().Foreground(colorInfo).Padding(0, 2)
	mutedStyle := lipgloss.NewStyle().Foreground(colorMuted)

	// Line 1: full title
	line1 := truncate(b.Title, detailWidth)

	// Line 2: description snippet
	desc := strings.ReplaceAll(b.Description, "\n", " ")
	line2 := mutedStyle.Render(truncate(desc, detailWidth))

	// Line 3: status, priority, assignee, labels
	var parts []string
	parts = append(parts, fmt.Sprintf("Status: %s", b.Status))
	parts = append(parts, fmt.Sprintf("P%d", b.Priority))
	if b.Assignee != "" {
		parts = append(parts, fmt.Sprintf("@%s", b.Assignee))
	}
	if len(b.Labels) > 0 {
		parts = append(parts, strings.Join(b.Labels, ","))
	}
	if isBlocked(b) {
		parts = append(parts, lipgloss.NewStyle().Foreground(colorDanger).Render("BLOCKED"))
	}
	line3 := mutedStyle.Render(truncate(strings.Join(parts, " | "), detailWidth))

	return style.Render(line1 + "\n" + line2 + "\n" + line3)
}
