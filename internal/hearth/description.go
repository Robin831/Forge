package hearth

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// renderMarkdownDescription renders markdown content using glamour.
// Falls back to plain text if glamour rendering fails.
// width is the available content width in terminal columns.
func renderMarkdownDescription(content string, width int) string {
	if content == "" {
		return ""
	}
	if width < 20 {
		width = 20
	}

	style := "dark"
	if !lipgloss.HasDarkBackground() {
		style = "light"
	}

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return content
	}
	out, err := renderer.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimRight(out, "\n")
}

// descriptionViewerDimensions returns the (viewportWidth, viewportHeight) for
// the description viewer overlay, matching the log viewer sizing logic.
func (m *Model) descriptionViewerDimensions() (int, int) {
	viewerWidth := m.width - 8
	if viewerWidth < 40 {
		viewerWidth = 40
	}
	viewerHeight := m.height - 6
	if viewerHeight < 10 {
		viewerHeight = 10
	}
	vpWidth := viewerWidth - logViewerStyle.GetHorizontalFrameSize()
	// Fixed content lines: title + blank + blank + footer = 4
	vpHeight := viewerHeight - logViewerStyle.GetVerticalFrameSize() - 4
	if vpWidth < 1 {
		vpWidth = 1
	}
	if vpHeight < 1 {
		vpHeight = 1
	}
	return vpWidth, vpHeight
}

// openDescriptionViewer opens the description viewer overlay for the given bead.
// It renders the markdown content with glamour and initialises the scrollable viewport.
func (m *Model) openDescriptionViewer(beadID, title, description string) {
	vpWidth, vpHeight := m.descriptionViewerDimensions()
	m.descriptionViewerTitle = fmt.Sprintf("Description: %s", beadID)
	if title != "" {
		m.descriptionViewerTitle = fmt.Sprintf("Description: %s — %s", beadID, title)
	}
	m.descriptionViewerRaw = description
	m.descriptionViewerVP = viewport.New(vpWidth, vpHeight)
	if description == "" {
		m.descriptionViewerEmpty = true
		m.descriptionViewerVP.SetContent("")
	} else {
		m.descriptionViewerEmpty = false
		rendered := m.renderDescriptionViewerContent()
		m.descriptionViewerVP.SetContent(rendered)
	}
	m.showDescriptionViewer = true
}

// renderDescriptionViewerContent renders the raw markdown description for the
// current viewport width.
func (m *Model) renderDescriptionViewerContent() string {
	vpWidth, _ := m.descriptionViewerDimensions()
	return renderMarkdownDescription(m.descriptionViewerRaw, vpWidth)
}

// renderDescriptionViewer renders the description viewer overlay using the same
// style as the log viewer for visual consistency.
func (m *Model) renderDescriptionViewer() string {
	viewerWidth := m.width - 8
	if viewerWidth < 40 {
		viewerWidth = 40
	}
	viewerHeight := m.height - 6
	if viewerHeight < 10 {
		viewerHeight = 10
	}

	titleText := truncate(m.descriptionViewerTitle, viewerWidth-logViewerStyle.GetHorizontalFrameSize())

	var lines []string
	lines = append(lines, actionMenuTitleStyle.Render(titleText))
	lines = append(lines, "")

	if m.descriptionViewerEmpty {
		lines = append(lines, dimStyle.Render("(no description)"))
	} else {
		lines = append(lines, m.descriptionViewerVP.View())
	}

	lines = append(lines, "")
	scrollPct := int(m.descriptionViewerVP.ScrollPercent() * 100)
	lines = append(lines, dimStyle.Render(fmt.Sprintf("j/k/mouse: scroll • Esc: close  %d%%", scrollPct)))

	content := strings.Join(lines, "\n")
	return logViewerStyle.Width(viewerWidth).Height(viewerHeight).Render(content)
}
