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

// cardContentLines is the number of content lines every kanban card emits.
// Keeping this fixed lets renderLane compute lane padding and scrolling correctly.
// A blank separator line is appended after each card by renderLane, making the
// total per-card height cardHeight = cardContentLines + 1.
const (
	cardContentLines = 3
	cardHeight       = cardContentLines + 1
)

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
		m.view = ViewHierarchy
		m.refreshHierarchy()
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

	// LaneInReview is read-only — HasPR is derived from Forge's state DB
	// (FetchAllBeads) and cannot be set via `bd update --status=...`.
	// Disallow moves into it to avoid temporary UI inconsistency.
	if targetLane == LaneInReview {
		return nil
	}

	// Optimistically move the bead in the UI.
	// Build a new slice to avoid mutating the original backing array.
	newLane := make([]Bead, 0, len(beads)-1)
	newLane = append(newLane, beads[:cursor]...)
	newLane = append(newLane, beads[cursor+1:]...)
	m.kanban.lanes[lane] = newLane
	m.kanban.laneVP[lane].ClampToTotal(len(m.kanban.lanes[lane]))
	b.Status = targetStatus
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
	// Reserve vertical space for chrome around the card area.
	const (
		headerLines     = 1 // top header row
		detailLines     = 3 // bottom detail preview
		footerLines     = 1 // footer help line
		laneHeaderLines = 1 // per-lane column header
	)
	chromeHeight := headerLines + detailLines + footerLines + laneHeaderLines
	cardAreaHeight := max(m.height-chromeHeight, 3)
	// visibleCards uses the package-level cardHeight constant (cardContentLines + 1 separator).
	visibleCards := max(cardAreaHeight/cardHeight, 1)

	// Render each lane
	var laneCols []string
	for i := range laneCount {
		col := m.renderLane(i, laneWidth, visibleCards, cardAreaHeight, i == m.kanban.activeLane)
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
	footer := footerStyle.Render("h/l: lane  j/k: card  H/L: move  n: new  e: edit  x: close  r: reopen  d: add dep  b: view deps  Tab: hierarchy  q: quit") + errNote

	return header + "\n" + board + "\n" + detail + "\n" + footer
}

// kanbanLaneWidth computes the width for each lane column.
func (m *Model) kanbanLaneWidth() int {
	w := max((m.width-5)/laneCount, 15)
	return w
}

// renderLane renders a single kanban lane column.
func (m *Model) renderLane(lane, width, visibleCards, areaHeight int, active bool) string {
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
		// renderCard always emits exactly cardContentLines lines.
		// Append an explicit blank separator to make each slot cardHeight lines.
		cards.WriteString(card)
		cards.WriteByte('\n')
		linesUsed += cardHeight
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
// It always emits exactly cardContentLines lines so that lane padding,
// scrolling, and column-height alignment remain correct.
func renderCard(b Bead, width int, selected, blocked bool) string {
	if width < 5 {
		width = 5
	}

	// Priority dot
	dot := priorityDot(b.Priority)

	// Line 1: dot + ID (always rendered)
	line1 := dot + " " + truncate(b.ID, width-3)

	// Line 2: first title line (always rendered)
	titleLines := wrapTitle(b.Title, width-1)
	line2 := titleLines[0]

	// Line 3: second title line if the title wrapped; otherwise anvil tag; otherwise blank.
	// This keeps the card at exactly cardContentLines (3) lines.
	line3 := ""
	if len(titleLines) > 1 {
		line3 = titleLines[1]
	} else if b.Anvil != "" {
		line3 = lipgloss.NewStyle().Foreground(colorMuted).Render("[" + truncate(b.Anvil, width-3) + "]")
	}

	content := line1 + "\n" + line2 + "\n" + line3

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

	l1 := line1.String()
	if lipgloss.Width(l1) > width {
		l1 = truncate(l1, width)
	}
	lines := []string{l1}
	if line2.Len() > 0 {
		l2 := line2.String()
		if lipgloss.Width(l2) > width {
			l2 = truncate(l2, width)
		}
		lines = append(lines, l2)
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
