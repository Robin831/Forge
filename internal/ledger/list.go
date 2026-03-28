package ledger

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

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
	colExt      = 6  // compact GitHub issue number, e.g. "#42"
	colLabels   = 14
	colAssignee = 12
	// Title gets the remainder.
)

// sortBeads returns a sorted copy of the bead slice based on the given field.
func sortBeads(beads []Bead, field SortField) []Bead {
	sorted := make([]Bead, len(beads))
	copy(sorted, beads)
	sort.SliceStable(sorted, func(i, j int) bool {
		a := sorted[i]
		b := sorted[j]

		// Primary key comparison based on the selected sort field.
		switch field {
		case SortStatus:
			si := statusOrder(a.Status)
			sj := statusOrder(b.Status)
			if si != sj {
				return si < sj
			}
		case SortUpdatedAt:
			if updatedAfterDesc(a.UpdatedAt, b.UpdatedAt) {
				return true
			}
			if updatedAfterDesc(b.UpdatedAt, a.UpdatedAt) {
				return false
			}
		default: // SortPriority
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
		}

		// Secondary: UpdatedAt descending (newest first, nil treated as oldest).
		if updatedAfterDesc(a.UpdatedAt, b.UpdatedAt) {
			return true
		}
		if updatedAfterDesc(b.UpdatedAt, a.UpdatedAt) {
			return false
		}

		// Tertiary: ID ascending, then Title ascending for deterministic ordering.
		if a.ID != b.ID {
			return a.ID < b.ID
		}

		return a.Title < b.Title
	})
	return sorted
}

// updatedAfterDesc reports whether ti should come before tj when sorting by
// UpdatedAt in descending order. Non-nil timestamps are considered more
// recent than nil (nil is treated as the oldest).
func updatedAfterDesc(ti, tj *time.Time) bool {
	if ti == nil && tj == nil {
		return false
	}
	if ti == nil {
		return false
	}
	if tj == nil {
		return true
	}
	if ti.Equal(*tj) {
		return false
	}
	return ti.After(*tj)
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
			m.sortChoice = nil
			return cmd
		}
		return cmd
	}

	switch msg.String() {
	case "j", "down":
		m.list.vp.ScrollDown(m.filteredBeadsCount())
	case "k", "up":
		m.list.vp.ScrollUp()
	case "S":
		return m.openSortSelector()
	case "tab", "v":
		m.cycleView()
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
	sorted := sortBeads(m.filteredBeads(), m.list.sortBy)
	total := len(sorted)

	// Header
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 2)
	header := headerStyle.Render(fmt.Sprintf("⚒ Forge Ledger — List  %d beads (sorted by %s)%s", total, m.list.sortBy, m.filterHint()))

	// Column header — when bulk mode is active the data rows have a checkboxWidth
	// prefix and a narrower title column, so the header must match.
	// Optional columns (Labels, Assignee) are omitted on narrow terminals.
	colHeaderStyle := lipgloss.NewStyle().Bold(true).Foreground(colorMuted).Padding(0, 2)
	titleWidth := m.titleColumnWidth()
	hideLabels, hideAssignee := m.hiddenListCols()
	headerPrefix := ""
	headerTitleWidth := titleWidth
	if m.bulk.Count() > 0 {
		headerPrefix = strings.Repeat(" ", checkboxWidth)
		headerTitleWidth = max(titleWidth-checkboxWidth, 4)
	}
	colHeaderRow := headerPrefix +
		padRight("Pri", colPriority) +
		padRight("ID", colID) +
		padRight("Title", headerTitleWidth) +
		padRight("Status", colStatus) +
		padRight("Anvil", colAnvil)
	if !hideLabels {
		colHeaderRow += padRight("Ext", colExt)
		colHeaderRow += padRight("Labels", colLabels)
	}
	if !hideAssignee {
		colHeaderRow += padRight("Assignee", colAssignee)
	}
	colHeader := colHeaderStyle.Render(colHeaderRow)

	// Compute available height for rows: total height - header(1) - col header(1) - footer(1)
	// - event panel (always visible) - border overhead (when detail panel shown)
	// - 1 for the newline between footer and event panel separator.
	borderOverhead := 0
	if m.detailPanelW() > 0 {
		borderOverhead = 2
	}
	rowsHeight := max(m.height-4-borderOverhead-m.eventPanelH(), 1)

	m.list.vp.ClampToTotal(total)
	m.list.vp.AdjustViewport(rowsHeight, total)
	start, end := m.list.vp.VisibleRange(rowsHeight, total)

	var rows strings.Builder
	if total == 0 {
		// Empty state: show an informative message instead of an empty table body.
		emptyStyle := lipgloss.NewStyle().Foreground(colorMuted).Italic(true).Padding(1, 2)
		var emptyMsg string
		if !m.showClosed && len(m.beads) > 0 {
			// Beads exist but some are hidden by the closed filter.
			emptyMsg = "No beads match the current filter"
		} else if m.selectedAnvil != "" && len(m.beads) == 0 {
			// Anvil selected but it has no beads at all.
			emptyMsg = "No beads found in this anvil"
		} else if m.selectedAnvil != "" {
			// Anvil selected; beads were filtered out by the closed toggle.
			emptyMsg = "No beads match the current filter"
		} else {
			emptyMsg = "No beads found"
		}
		rows.WriteString(emptyStyle.Render(emptyMsg))
	} else {
		for i := start; i < end; i++ {
			b := sorted[i]
			selected := i == m.list.vp.Selected()

			row := m.renderBeadRow(b, titleWidth, selected)
			rows.WriteString(row)
			if i < end-1 {
				rows.WriteByte('\n')
			}
		}
	}

	footer := m.renderFooter()

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

// hiddenListCols returns which optional columns should be hidden to give the
// title more space on narrow terminals.
func (m *Model) hiddenListCols() (hideLabels, hideAssignee bool) {
	w := m.mainPanelWidth()
	hideAssignee = w < narrowDropAssigneeWidth
	hideLabels = w < narrowDropLabelsWidth
	return
}

// titleColumnWidth computes the width available for the title column.
// Optional columns (Ext, Labels, Assignee) are excluded when the terminal is narrow.
func (m *Model) titleColumnWidth() int {
	hideLabels, hideAssignee := m.hiddenListCols()
	fixed := colPriority + colID + colStatus + colAnvil + 4 // 4 for padding
	if !hideLabels {
		fixed += colExt
		fixed += colLabels
	}
	if !hideAssignee {
		fixed += colAssignee
	}
	return max(m.mainPanelWidth()-fixed, 10)
}

// checkboxWidth is the visual width of the checkbox prefix (e.g. "[✓] ").
const checkboxWidth = 4

// renderBeadRow renders a single bead as a table row.
func (m *Model) renderBeadRow(b Bead, titleWidth int, selected bool) string {
	sc := statusColor(b.Status)

	// Checkbox prefix — shown whenever any bead is selected.
	var checkPrefix string
	if m.bulk.Count() > 0 {
		if m.bulk.IsSelected(b.ID) {
			checkPrefix = "[✓] "
		} else {
			checkPrefix = "[ ] "
		}
		// Shrink title column to make room for the checkbox.
		titleWidth = max(titleWidth-checkboxWidth, 4)
	}

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

	// Optional columns: hidden on narrow terminals to give more title space.
	hideLabels, hideAssignee := m.hiddenListCols()

	var extPart, labelsPart, assigneePart string
	if !hideLabels {
		extStr := formatExternalRef(b.ExternalRef)
		extPart = padRight(truncate(extStr, colExt-1), colExt)
		labelStr := strings.Join(b.Labels, ",")
		labelsPart = padRight(truncate(labelStr, colLabels-1), colLabels)
	}
	if !hideAssignee {
		assigneePart = padRight(truncate(b.Assignee, colAssignee-1), colAssignee)
	}

	row := checkPrefix + pri + id + title + status + anvil + extPart + labelsPart + assigneePart

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

// formatExternalRef converts an external reference to a compact display string.
// Full GitHub issue URLs (e.g. https://github.com/org/repo/issues/42) are
// shortened to "#42". The gh-N shorthand format is also converted to "#N".
// Other formats are returned verbatim.
func formatExternalRef(ref string) string {
	if ref == "" {
		return ""
	}
	// Handle full GitHub issue URLs: must be github.com with /issues/<number> path.
	if strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://") {
		if u, err := url.Parse(ref); err == nil && u.Hostname() == "github.com" {
			segments := strings.Split(strings.Trim(u.Path, "/"), "/")
			// Path must be: <owner>/<repo>/issues/<number>
			if len(segments) >= 4 && segments[len(segments)-2] == "issues" {
				numSeg := segments[len(segments)-1]
				if numSeg != "" && strings.IndexFunc(numSeg, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
					return "#" + numSeg
				}
			}
		}
		return ref
	}
	// Handle gh-N shorthand.
	if after, ok := strings.CutPrefix(ref, "gh-"); ok && after != "" {
		return "#" + after
	}
	return ref
}

// truncate shortens s so its visual width is at most maxLen columns, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}

	// If already fits within the available visual width, return as-is.
	if lipgloss.Width(s) <= maxLen {
		return s
	}

	ellipsis := "…"
	ellipsisWidth := lipgloss.Width(ellipsis)

	// For very small widths, just show an ellipsis to indicate truncation.
	if maxLen <= ellipsisWidth {
		return ellipsis
	}

	var (
		builder      strings.Builder
		currentWidth int
	)

	for _, r := range s {
		runeStr := string(r)
		rw := lipgloss.Width(runeStr)

		// Leave room for the ellipsis at the end.
		if currentWidth+rw+ellipsisWidth > maxLen {
			break
		}

		builder.WriteString(runeStr)
		currentWidth += rw
	}

	return builder.String() + ellipsis
}

// padRight pads s with spaces so the total visual width equals width.
func padRight(s string, width int) string {
	currentWidth := lipgloss.Width(s)
	if currentWidth >= width {
		return s
	}

	padding := width - currentWidth
	return s + strings.Repeat(" ", padding)
}
