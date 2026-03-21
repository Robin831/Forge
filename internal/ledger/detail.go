package ledger

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// detailPanelFixedW is the fixed column width of the bead detail side panel
// (including its left border).
const detailPanelFixedW = 38

// minWidthForDetailPanel is the minimum terminal width at which the detail
// panel is auto-shown. Below this threshold the full width goes to the main
// view regardless of showDetailPanel.
const minWidthForDetailPanel = 120

// detailPanelW returns the width allocated to the detail panel, or 0 when the
// panel is hidden (either because showDetailPanel is false or the terminal is
// too narrow).
func (m *Model) detailPanelW() int {
	if !m.showDetailPanel || m.width < minWidthForDetailPanel {
		return 0
	}
	return detailPanelFixedW
}

// mainPanelWidth returns the width available for the main view. When the detail
// panel is visible this is the terminal width minus the detail panel width.
// Otherwise it equals the full terminal width.
func (m *Model) mainPanelWidth() int {
	d := m.detailPanelW()
	if d == 0 {
		return m.width
	}
	return m.width - d
}

// detailPanelBaseStyle is the base lipgloss style for the detail side panel,
// used both for rendering and for deriving the horizontal frame overhead.
func detailPanelBaseStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		BorderLeft(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(colorMuted).
		Padding(0, 1)
}

// renderDetailPanel renders the bead detail side panel as a fixed-width block
// of height m.height. A left border acts as the visual divider from the main
// panel.
func (m *Model) renderDetailPanel() string {
	w := m.detailPanelW()
	if w <= 0 {
		return ""
	}

	base := detailPanelBaseStyle()
	// Derive the inner content width from the style so the offset never
	// drifts out of sync with the style definition.
	frameW := base.GetHorizontalFrameSize()
	innerW := max(w-frameW, 4)

	b := m.selectedBead()

	var content strings.Builder
	if b == nil {
		emptyStyle := lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
		content.WriteString(emptyStyle.Render("No bead selected"))
	} else {
		renderBeadDetailContent(&content, b, innerW)
	}

	return base.
		Width(w - frameW).
		Height(m.height).
		Render(content.String())
}

// renderBeadDetailContent writes the full bead detail text into sb.
func renderBeadDetailContent(sb *strings.Builder, b *Bead, innerW int) {
	keyStyle    := lipgloss.NewStyle().Foreground(colorMuted).Bold(true)
	mutedStyle  := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Dark: "252", Light: "240"})
	accentStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	sc          := statusColor(b.Status)
	statusStyle := lipgloss.NewStyle().Foreground(sc)

	// ID header
	sb.WriteString(accentStyle.Render(truncate(b.ID, innerW)))
	sb.WriteByte('\n')

	// Title (word-wrapped, bold)
	titleStyle := lipgloss.NewStyle().Bold(true)
	for _, line := range wrapDetailText(b.Title, innerW) {
		sb.WriteString(titleStyle.Render(line))
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')

	// Metadata fields
	writeDetailField(sb, keyStyle, "Status", statusStyle.Render(b.Status), innerW)
	writeDetailField(sb, keyStyle, "Priority", fmt.Sprintf("P%d", b.Priority), innerW)
	if b.IssueType != "" {
		writeDetailField(sb, keyStyle, "Type", b.IssueType, innerW)
	}
	writeDetailField(sb, keyStyle, "Anvil", b.Anvil, innerW)
	if b.Assignee != "" {
		writeDetailField(sb, keyStyle, "Assignee", "@"+b.Assignee, innerW)
	}
	if len(b.Labels) > 0 {
		writeDetailField(sb, keyStyle, "Labels", strings.Join(b.Labels, ", "), innerW)
	}
	if b.HasPR {
		writeDetailField(sb, keyStyle, "PR", "open", innerW)
	}
	if b.UpdatedAt != nil {
		writeDetailField(sb, keyStyle, "Updated", b.UpdatedAt.Format("2006-01-02"), innerW)
	}
	if b.ClosedAt != nil {
		writeDetailField(sb, keyStyle, "Closed", b.ClosedAt.Format("2006-01-02"), innerW)
	}

	// Dependencies
	if len(b.DependsOn) > 0 {
		sb.WriteByte('\n')
		sb.WriteString(keyStyle.Render("Depends on:"))
		sb.WriteByte('\n')
		for _, dep := range b.DependsOn {
			sb.WriteString(mutedStyle.Render("  • " + truncate(dep, innerW-4)))
			sb.WriteByte('\n')
		}
	}
	if len(b.Blocks) > 0 {
		sb.WriteByte('\n')
		sb.WriteString(keyStyle.Render("Blocks:"))
		sb.WriteByte('\n')
		for _, bl := range b.Blocks {
			sb.WriteString(mutedStyle.Render("  • " + truncate(bl, innerW-4)))
			sb.WriteByte('\n')
		}
	}

	// Description
	if b.Description != "" {
		sb.WriteByte('\n')
		sb.WriteString(keyStyle.Render(strings.Repeat("─", min(innerW, 20))))
		sb.WriteByte('\n')
		sb.WriteString(keyStyle.Render("Description:"))
		sb.WriteByte('\n')
		for _, line := range wrapDetailText(b.Description, innerW) {
			sb.WriteString(mutedStyle.Render(line))
			sb.WriteByte('\n')
		}
	}
}

// writeDetailField renders a "Label: value" metadata line into sb, truncating
// the value to fit within innerW.
func writeDetailField(sb *strings.Builder, keyStyle lipgloss.Style, label, value string, innerW int) {
	key := label + ": "
	keyW := lipgloss.Width(key)
	valW := max(innerW-keyW, 1)
	sb.WriteString(keyStyle.Render(key))
	sb.WriteString(truncate(value, valW))
	sb.WriteByte('\n')
}

// wrapDetailText wraps text at word boundaries to fit within the given visual
// width, preserving paragraph breaks (blank lines between paragraphs).
func wrapDetailText(text string, width int) []string {
	if width <= 0 || text == "" {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var result []string
	for _, paragraph := range strings.Split(text, "\n") {
		if paragraph == "" {
			result = append(result, "")
			continue
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			result = append(result, "")
			continue
		}
		var line strings.Builder
		lineW := 0
		for _, word := range words {
			ww := lipgloss.Width(word)
			if lineW > 0 && lineW+1+ww > width {
				result = append(result, line.String())
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
		if line.Len() > 0 {
			result = append(result, line.String())
		}
	}
	return result
}
