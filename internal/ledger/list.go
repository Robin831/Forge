package ledger

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// SortField determines how beads are sorted in the list view.
type SortField int

const (
	SortPriority  SortField = iota // default
	SortStatus
	SortUpdatedAt
)

func (s SortField) String() string {
	switch s {
	case SortPriority:
		return "Priority"
	case SortStatus:
		return "Status"
	case SortUpdatedAt:
		return "Updated"
	default:
		return "Priority"
	}
}

// listState holds list-view-specific state.
type listState struct {
	vp       scrollViewport
	sortBy   SortField
	sortForm *huh.Form // non-nil while the sort selector is open
}

// Column widths for the table layout.
const (
	colPriority = 4
	colID       = 14
	colStatus   = 13
	colAnvil    = 14
	colLabels   = 14
	colAssignee = 12
	// Title gets the remainder.
)

// sortBeads returns a sorted copy of the bead slice based on the given field.
func sortBeads(beads []Bead, field SortField) []Bead {
	sorted := make([]Bead, len(beads))
	copy(sorted, beads)
	sort.SliceStable(sorted, func(i, j int) bool {
		switch field {
		case SortStatus:
			return statusOrder(sorted[i].Status) < statusOrder(sorted[j].Status)
		case SortUpdatedAt:
			ti := sorted[i].UpdatedAt
			tj := sorted[j].UpdatedAt
			if ti == nil && tj == nil {
				return false
			}
			if ti == nil {
				return false
			}
			if tj == nil {
				return true
			}
			return ti.After(*tj)
		default: // SortPriority
			return sorted[i].Priority < sorted[j].Priority
		}
	})
	return sorted
}

// statusOrder returns a numeric ordering for status values so that active
// statuses appear first: in_progress=0, open=1, closed=2.
func statusOrder(s string) int {
	switch s {
	case "in_progress":
		return 0
	case "open":
		return 1
	case "closed":
		return 2
	default:
		return 3
	}
}

// statusColor returns the color for a given bead status.
func statusColor(status string) lipgloss.AdaptiveColor {
	switch status {
	case "open":
		return colorInfo
	case "in_progress":
		return colorWarning
	case "closed":
		return colorMuted
	default:
		return colorDanger // blocked or unknown
	}
}

// updateList handles key messages for the list view.
func (m *Model) updateList(msg tea.KeyMsg) tea.Cmd {
	// If the sort selector form is active, delegate to it.
	if m.list.sortForm != nil {
		form, cmd := m.list.sortForm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.list.sortForm = f
		}
		if m.list.sortForm.State == huh.StateCompleted {
			m.processSortResult()
			m.list.sortForm = nil
		} else if m.list.sortForm.State == huh.StateAborted {
			m.list.sortForm = nil
			return nil
		}
		return cmd
	}

	switch msg.String() {
	case "j", "down":
		m.list.vp.ScrollDown(len(m.beads))
	case "k", "up":
		m.list.vp.ScrollUp()
	case "S":
		return m.openSortSelector()
	}
	return nil
}

// openSortSelector creates a huh.Form for choosing the sort field and returns
// the tea.Cmd from its Init call so Bubbletea can schedule it.
func (m *Model) openSortSelector() tea.Cmd {
	var choice string
	switch m.list.sortBy {
	case SortPriority:
		choice = "priority"
	case SortStatus:
		choice = "status"
	case SortUpdatedAt:
		choice = "updated"
	}

	m.list.sortForm = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Sort by").
				Options(
					huh.NewOption("Priority", "priority"),
					huh.NewOption("Status", "status"),
					huh.NewOption("Updated", "updated"),
				).
				Value(&choice),
		),
	).WithShowHelp(false).WithShowErrors(false)

	cmd := m.list.sortForm.Init()

	// Store a pointer to choice so we can read the result in processSortResult.
	m.sortChoice = &choice

	return cmd
}

// processSortResult reads the sort form result and applies it.
func (m *Model) processSortResult() {
	if m.sortChoice == nil {
		return
	}
	switch *m.sortChoice {
	case "priority":
		m.list.sortBy = SortPriority
	case "status":
		m.list.sortBy = SortStatus
	case "updated":
		m.list.sortBy = SortUpdatedAt
	}
	m.sortChoice = nil
}

// renderList renders the bead list as a table.
func (m *Model) renderList() string {
	sorted := sortBeads(m.beads, m.list.sortBy)
	total := len(sorted)

	// Header
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 2)
	header := headerStyle.Render(fmt.Sprintf("⚒ Forge Ledger — %d beads (sorted by %s)", total, m.list.sortBy))

	// Column header
	colHeaderStyle := lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Padding(0, 2)
	titleWidth := m.titleColumnWidth()
	colHeader := colHeaderStyle.Render(
		padRight("Pri", colPriority) +
			padRight("ID", colID) +
			padRight("Title", titleWidth) +
			padRight("Status", colStatus) +
			padRight("Anvil", colAnvil) +
			padRight("Labels", colLabels) +
			padRight("Assignee", colAssignee),
	)

	// Compute available height for rows: total height - header(1) - col header(1) - footer(1) - padding(2)
	rowsHeight := max(m.height-5, 1)

	m.list.vp.ClampToTotal(total)
	m.list.vp.AdjustViewport(rowsHeight, total)
	start, end := m.list.vp.VisibleRange(rowsHeight, total)

	var rows strings.Builder
	for i := start; i < end; i++ {
		b := sorted[i]
		selected := i == m.list.vp.Selected()

		row := m.renderBeadRow(b, titleWidth, selected)
		rows.WriteString(row)
		if i < end-1 {
			rows.WriteByte('\n')
		}
	}

	// Footer
	footerStyle := lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)
	var errNote string
	if m.err != nil {
		errNote = lipgloss.NewStyle().Foreground(colorDanger).Render(fmt.Sprintf("  ⚠ %v", m.err))
	}
	footer := footerStyle.Render("j/k: navigate  S: sort  q: quit") + errNote

	out := header + "\n" + colHeader + "\n" + rows.String() + "\n" + footer

	// Overlay the sort selector if active.
	if m.list.sortForm != nil {
		formView := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(1, 2).
			Render(m.list.sortForm.View())
		out = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, formView,
			lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
	}

	return out
}

// titleColumnWidth computes the width available for the title column.
func (m *Model) titleColumnWidth() int {
	fixed := colPriority + colID + colStatus + colAnvil + colLabels + colAssignee + 4 // 4 for padding
	return max(m.width-fixed, 10)
}

// renderBeadRow renders a single bead as a table row.
func (m *Model) renderBeadRow(b Bead, titleWidth int, selected bool) string {
	sc := statusColor(b.Status)

	// Priority
	priStr := fmt.Sprintf("P%d", b.Priority)
	pri := padRight(priStr, colPriority)

	// ID
	id := padRight(truncate(b.ID, colID-1), colID)

	// Title — truncated to fit
	title := padRight(truncate(b.Title, titleWidth-1), titleWidth)

	// Status
	statusStr := b.Status
	if statusStr == "in_progress" {
		statusStr = "in_prog"
	}
	status := padRight(statusStr, colStatus)

	// Anvil
	anvil := padRight(truncate(b.Anvil, colAnvil-1), colAnvil)

	// Labels — join and truncate
	labelStr := strings.Join(b.Labels, ",")
	labels := padRight(truncate(labelStr, colLabels-1), colLabels)

	// Assignee
	assignee := padRight(truncate(b.Assignee, colAssignee-1), colAssignee)

	row := pri + id + title + status + anvil + labels + assignee

	if selected {
		return lipgloss.NewStyle().
			Bold(true).
			Background(lipgloss.AdaptiveColor{Dark: "237", Light: "254"}).
			Foreground(sc).
			Padding(0, 2).
			Render(row)
	}

	return lipgloss.NewStyle().
		Foreground(sc).
		Padding(0, 2).
		Render(row)
}

// truncate shortens s to maxLen runes, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	return string(runes[:maxLen-1]) + "…"
}

// padRight pads s with spaces so the total rune count equals width.
func padRight(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
