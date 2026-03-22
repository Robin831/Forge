package ledger

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- Key binding definitions ----

// Shared bindings available across all views.
var (
	keyQuit    = key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit"))
	keyHelp    = key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help"))
	keyTab     = key.NewBinding(key.WithKeys("tab", "v"), key.WithHelp("tab/v", "switch view"))
	keyNew     = key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new bead"))
	keyEdit    = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit"))
	keyCloseB  = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "close"))
	keyReopen  = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reopen"))
	keyLabel   = key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "label"))
	keyPriorityB = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "priority"))
	keyComment      = key.NewBinding(key.WithKeys("C"), key.WithHelp("C", "comment"))
	keyNotes        = key.NewBinding(key.WithKeys("N"), key.WithHelp("N", "notes"))
	keyToggleClosed = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "toggle closed"))
	keyBackToAnvils = key.NewBinding(key.WithKeys("esc", "f"), key.WithHelp("esc/f", "back to anvils"))
	keyAssign  = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "assign"))
	keyAddDep  = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "add dep"))
	keyViewDeps = key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "view deps"))
	keyAI          = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "AI improve"))
	keyDepUpdate   = key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "update deps"))
	keyDetailPanel = key.NewBinding(key.WithKeys("\\"), key.WithHelp("\\", "toggle detail"))
	keySpace   = key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "select"))
	keyCtrlA   = key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "select all"))
	keyEscB    = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear"))
	keyBulkClose    = key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "bulk close"))
	keyBulkLabel    = key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("ctrl+l", "bulk label"))
	keyBulkPriority = key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("ctrl+p", "bulk priority"))

	// Navigation (shared)
	keyUp   = key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "up"))
	keyDown = key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "down"))

	// List-specific
	keySort = key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "sort"))

	// Kanban-specific navigation
	keyLaneLeft  = key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h/←", "prev lane"))
	keyLaneRight = key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l/→", "next lane"))
	keyMoveLeft  = key.NewBinding(key.WithKeys("H"), key.WithHelp("H", "move left"))
	keyMoveRight = key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "move right"))

	// Hierarchy-specific
	keyExpand = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "expand/collapse"))

	// Anvil picker-specific
	keyAnvilSelect = key.NewBinding(key.WithKeys("enter", " "), key.WithHelp("enter/space", "select anvil"))
)

// ---- Per-view KeyMap structs implementing help.KeyMap ----

// anvilsKeyMap provides keybindings for the anvil picker.
type anvilsKeyMap struct{}

func (anvilsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyUp, keyDown, keyAnvilSelect, keyHelp, keyQuit}
}

func (anvilsKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyUp, keyDown, keyAnvilSelect}, // Navigation
		{keyHelp, keyQuit},               // General
	}
}

// listKeyMap provides keybindings for the list view.
type listKeyMap struct{}

func (listKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyUp, keyDown, keySpace, keyNew, keyEdit, keyBackToAnvils, keyHelp, keyQuit}
}

func (listKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyUp, keyDown, keySpace, keyCtrlA, keyEscB},              // Navigation
		{keyNew, keyEdit, keyCloseB, keyReopen},                    // CRUD
		{keyLabel, keyPriorityB, keyComment, keyNotes, keyAssign},  // Metadata
		{keyAddDep, keyViewDeps},                                   // Dependencies
		{keyBulkClose, keyBulkLabel, keyBulkPriority},              // Bulk
		{keyAI, keyDepUpdate},                                      // AI / Updates
		{keyToggleClosed, keySort},                                 // Filters
		{keyTab, keyBackToAnvils, keyDetailPanel, keyHelp, keyQuit}, // General
	}
}

// kanbanKeyMap provides keybindings for the kanban view.
type kanbanKeyMap struct{}

func (kanbanKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyUp, keyDown, keyLaneLeft, keyLaneRight, keyNew, keyBackToAnvils, keyHelp, keyQuit}
}

func (kanbanKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyUp, keyDown, keyLaneLeft, keyLaneRight, keyMoveLeft, keyMoveRight}, // Navigation
		{keyNew, keyEdit, keyCloseB, keyReopen},                               // CRUD
		{keyPriorityB, keyComment, keyNotes, keyAssign},                       // Metadata
		{keyAddDep, keyViewDeps},                                              // Dependencies
		{keyBulkClose, keyBulkLabel, keyBulkPriority},                         // Bulk
		{keyAI, keyDepUpdate},                                                 // AI / Updates
		{keyToggleClosed},                                                     // Filters
		{keyTab, keyBackToAnvils, keyDetailPanel, keyHelp, keyQuit}, // General
	}
}

// hierarchyKeyMap provides keybindings for the hierarchy view.
type hierarchyKeyMap struct{}

func (hierarchyKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{keyUp, keyDown, keyExpand, keySpace, keyNew, keyBackToAnvils, keyHelp, keyQuit}
}

func (hierarchyKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{keyUp, keyDown, keyExpand, keySpace, keyCtrlA, keyEscB},                   // Navigation
		{keyNew, keyEdit, keyCloseB, keyReopen},                                    // CRUD
		{keyPriorityB, keyComment, keyNotes, keyAssign},                            // Metadata
		{keyAddDep, keyViewDeps},                                                   // Dependencies
		{keyBulkClose, keyBulkLabel, keyBulkPriority},                              // Bulk
		{keyAI, keyDepUpdate},                                                      // AI / Updates
		{keyToggleClosed},                                                          // Filters
		{keyTab, keyBackToAnvils, keyDetailPanel, keyHelp, keyQuit}, // General
	}
}

// ---- Help overlay state ----

// helpState manages the help overlay viewport and helper model.
type helpState struct {
	show    bool
	vp      viewport.Model
	vpReady bool
	helper  help.Model
}

func newHelpState() helpState {
	return helpState{helper: help.New()}
}

// categoryLabels are the display names for each FullHelp group, in order.
var categoryLabels = []string{
	"Navigation", "CRUD", "Metadata", "Dependencies",
	"Bulk", "AI / Updates", "Filters", "General",
}

// updateHelpOverlay handles key events when the help overlay is visible.
func (m *Model) updateHelpOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "?", "esc":
		m.helpSt.show = false
	case "j", "down":
		m.helpSt.vp.ScrollDown(1)
	case "k", "up":
		m.helpSt.vp.ScrollUp(1)
	case "pgdown", "ctrl+f":
		m.helpSt.vp.HalfPageDown()
	case "pgup", "ctrl+b":
		m.helpSt.vp.HalfPageUp()
	case "g":
		m.helpSt.vp.GotoTop()
	case "G":
		m.helpSt.vp.GotoBottom()
	}
	return m, nil
}

// renderHelpOverlay renders the full-screen help overlay with scrollable content.
func (m *Model) renderHelpOverlay() string {
	var km help.KeyMap
	switch m.view {
	case ViewAnvils:
		km = anvilsKeyMap{}
	case ViewKanban:
		km = kanbanKeyMap{}
	case ViewHierarchy:
		km = hierarchyKeyMap{}
	default:
		km = listKeyMap{}
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	catStyle   := lipgloss.NewStyle().Bold(true).Foreground(colorMuted)
	dimStyle   := lipgloss.NewStyle().Foreground(colorMuted)
	keyStyle   := lipgloss.NewStyle().Foreground(colorAccent)
	descStyle  := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Dark: "252", Light: "240"})

	var sb strings.Builder
	sb.WriteString(titleStyle.Render("⌨  Keybindings") + "\n\n")

	for i, group := range km.FullHelp() {
		label := "Other"
		if i < len(categoryLabels) {
			label = categoryLabels[i]
		}
		sb.WriteString(catStyle.Render(label+":") + "\n")
		for _, binding := range group {
			bh := binding.Help()
			fmt.Fprintf(&sb, "  %-20s %s\n",
				keyStyle.Render(bh.Key),
				descStyle.Render(bh.Desc),
			)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(dimStyle.Render("j/k: scroll  ?/esc: close"))

	content := sb.String()

	maxW := max(m.width-8, 40)
	maxW = min(maxW, m.width)
	maxH := max(m.height-6, 5)
	maxH = min(maxH, m.height)
	contentH := strings.Count(content, "\n") + 1

	vpH := min(contentH, maxH)
	if !m.helpSt.vpReady || m.helpSt.vp.Width != maxW || m.helpSt.vp.Height != vpH {
		m.helpSt.vp = viewport.New(maxW, vpH)
		m.helpSt.vpReady = true
	}
	m.helpSt.vp.Width = maxW
	m.helpSt.vp.Height = vpH
	m.helpSt.vp.SetContent(content)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Width(maxW).
		Render(m.helpSt.vp.View())

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
}

// renderFooter renders the context-sensitive help footer for the current view and state.
func (m *Model) renderFooter() string {
	footerStyle := lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)

	// Error note appended after footer text: show the last fetch error inline,
	// or prompt the user to open the event panel when errors are logged there.
	var errNote string
	if m.err != nil {
		errNote = lipgloss.NewStyle().Foreground(colorDanger).Render(fmt.Sprintf("  ⚠ %v", m.err))
	} else if m.hasEventErrors() {
		errNote = lipgloss.NewStyle().Foreground(colorDanger).Render("  ⚠ errors in activity log")
	}

	var footerText string

	switch {
	case m.helpSt.show:
		// Help overlay is visible — guide the user to scroll or close.
		footerText = "j/k: scroll  ?/esc: close"

	case m.list.sortForm != nil, m.activeForm != nil:
		// A huh form overlay is active.
		footerText = "enter: confirm  esc: cancel"

	case m.bulk.Count() > 0:
		// Bulk selection mode: show bulk operation shortcuts.
		h := m.helpSt.helper
		h.Width = max(m.mainPanelWidth()-4, 1)
		bulkBindings := []key.Binding{keyBulkClose, keyBulkLabel, keyBulkPriority, keyCtrlA, keyEscB}
		footerText = fmt.Sprintf("%d selected: ", m.bulk.Count()) + h.ShortHelpView(bulkBindings)

	default:
		// Normal mode: view-specific short bindings prefixed with the active view label.
		h := m.helpSt.helper
		var km help.KeyMap
		var viewLabel string
		switch m.view {
		case ViewAnvils:
			km = anvilsKeyMap{}
			viewLabel = "[Anvils]"
		case ViewKanban:
			km = kanbanKeyMap{}
			viewLabel = "[Kanban]"
		case ViewHierarchy:
			km = hierarchyKeyMap{}
			viewLabel = "[Hierarchy]"
		default:
			km = listKeyMap{}
			viewLabel = "[List]"
		}
		labelPrefix := viewLabel + "  "
		// footerStyle has Padding(0,2) which adds 4 chars; subtract label prefix too.
		h.Width = max(m.mainPanelWidth()-4-lipgloss.Width(labelPrefix), 1)
		footerText = labelPrefix + h.ShortHelpView(km.ShortHelp())
	}

	return footerStyle.Render(footerText) + errNote
}
