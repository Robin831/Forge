// Package ledger provides an interactive TUI for browsing and managing beads
// across all registered Forge anvils.
package ledger

import (
	"fmt"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/state"
)

// refreshInterval controls how often the ledger auto-refreshes bead data.
const refreshInterval = 30 * time.Second

// ViewMode determines which screen the Ledger is showing.
type ViewMode int

const (
	ViewList ViewMode = iota
	ViewKanban
)

// tickMsg triggers a periodic data refresh.
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

// FormKind tracks which overlay form is active.
type FormKind int

const (
	FormNone FormKind = iota
	FormNewBead
	FormEditBead
	FormCloseBead
)

// Model is the top-level Bubbletea model for the Ledger TUI.
type Model struct {
	beads    []Bead
	loading  bool
	fetching bool // true while a FetchAllBeads call is in-flight
	err      error

	width  int
	height int

	view   ViewMode
	list   listState
	kanban kanbanState
	// sortChoice holds the pointer used by the huh sort selector form.
	sortChoice *string

	anvils map[string]string // name → path
	db     *state.DB

	// Form overlay state
	activeForm     *huh.Form
	activeFormKind FormKind
	formTarget     *Bead // bead being edited/closed (nil for new)

	// Form field bindings for new/edit bead
	formTitle       string
	formDescription string
	formType        string
	formPriority    string
	formAnvil       string
	formReason      string // close reason

	// Toast notifications
	toasts      []toast
	nextToastID int
}

// NewModel creates a new Ledger model.
func NewModel(anvils map[string]string, db *state.DB) *Model {
	return &Model{
		anvils:  anvils,
		db:      db,
		loading: true,
		view:    ViewList,
	}
}

// Init schedules the initial data fetch and periodic refresh tick.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), FetchAllBeads(m.anvils, m.db))
}

// Update handles incoming messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle toast dismissals first — they apply regardless of form state.
	if dismiss, ok := msg.(toastDismissMsg); ok {
		m.dismissToast(dismiss.id)
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// When a form overlay is active, route key events to the form handler.
		if m.activeForm != nil {
			return m.updateForm(msg)
		}
		// Global quit key: always handle ctrl+c.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		// Global quit key "q" (only when sort form is not open).
		if m.list.sortForm == nil {
			switch msg.String() {
			case "q":
				return m, tea.Quit
			}
		}

		// CRUD key bindings (available in both views when no form is open).
		if m.list.sortForm == nil {
			switch msg.String() {
			case "n":
				return m, m.openNewBeadForm()
			case "e":
				return m, m.openEditBeadForm()
			case "x":
				return m, m.openCloseBeadForm()
			case "r":
				return m, m.reopenSelectedBead()
			}
		}

		// Delegate to view-specific handler.
		switch m.view {
		case ViewList:
			cmd := m.updateList(msg)
			return m, cmd
		case ViewKanban:
			cmd := m.updateKanban(msg)
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case UpdateBeadsMsg:
		m.loading = false
		m.fetching = false
		m.beads = msg.Beads
		m.err = msg.Err
		m.list.vp.ClampToTotal(len(m.beads))
		m.refreshKanbanLanes()

	case moveBeadMsg:
		if msg.Err != nil {
			m.err = msg.Err
		}
		m.fetching = true
		return m, FetchAllBeads(m.anvils, m.db)

	case BeadCreatedMsg:
		label := msg.ID
		if label == "" {
			label = "bead"
		}
		cmd := m.addToast(fmt.Sprintf("Created %s", label), false)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case BeadUpdatedMsg:
		cmd := m.addToast(fmt.Sprintf("Updated %s", msg.ID), false)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case BeadClosedMsg:
		cmd := m.addToast(fmt.Sprintf("Closed %s", msg.ID), false)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case BeadReopenedMsg:
		cmd := m.addToast(fmt.Sprintf("Reopened %s", msg.ID), false)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case ActionErrorMsg:
		cmd := m.addToast(msg.Err.Error(), true)
		return m, cmd

	case tickMsg:
		if m.fetching {
			return m, tickCmd()
		}
		m.fetching = true
		return m, tea.Batch(tickCmd(), FetchAllBeads(m.anvils, m.db))
	}
	return m, nil
}

// updateForm handles input when a form overlay is active.
func (m *Model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		if k.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if k.Type == tea.KeyEsc {
			m.clearForm()
			return m, nil
		}
	}

	cmd := m.driveHuhForm(&m.activeForm, msg)

	if m.activeForm.State == huh.StateCompleted {
		actionCmd := m.executeFormAction()
		m.clearForm()
		if cmd != nil {
			return m, tea.Batch(cmd, actionCmd)
		}
		return m, actionCmd
	}
	if m.activeForm.State == huh.StateAborted {
		m.clearForm()
		return m, cmd
	}

	return m, cmd
}

// clearForm resets the form overlay state.
func (m *Model) clearForm() {
	m.activeForm = nil
	m.activeFormKind = FormNone
	m.formTarget = nil
	m.formTitle = ""
	m.formDescription = ""
	m.formType = ""
	m.formPriority = ""
	m.formAnvil = ""
	m.formReason = ""
}

// executeFormAction dispatches the appropriate bd command based on the active form.
func (m *Model) executeFormAction() tea.Cmd {
	switch m.activeFormKind {
	case FormNewBead:
		anvilPath, ok := m.anvils[m.formAnvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil: %s", m.formAnvil)}
			}
		}
		pri := 2
		switch m.formPriority {
		case "0":
			pri = 0
		case "1":
			pri = 1
		case "2":
			pri = 2
		case "3":
			pri = 3
		case "4":
			pri = 4
		}
		return NewBeadCmd(anvilPath, m.formTitle, m.formDescription, m.formType, pri)

	case FormEditBead:
		if m.formTarget == nil {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("no form target for edit bead action")}
			}
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return EditBeadCmd(anvilPath, m.formTarget.ID, m.formTitle, m.formDescription)

	case FormCloseBead:
		if m.formTarget == nil {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("no form target for close bead action")}
			}
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return CloseBeadCmd(anvilPath, m.formTarget.ID, m.formReason)
	}
	return nil
}

// selectedBead returns a pointer to the currently selected bead, or nil.
func (m *Model) selectedBead() *Bead {
	switch m.view {
	case ViewList:
		sorted := sortBeads(m.beads, m.list.sortBy)
		idx := m.list.vp.Selected()
		if idx >= 0 && idx < len(sorted) {
			return &sorted[idx]
		}
	case ViewKanban:
		lane := m.kanban.activeLane
		beads := m.kanban.lanes[lane]
		idx := m.kanban.laneVP[lane].Selected()
		if idx >= 0 && idx < len(beads) {
			return &beads[idx]
		}
	}
	return nil
}

// openNewBeadForm creates and displays the new bead form.
func (m *Model) openNewBeadForm() tea.Cmd {
	// Build anvil options from registered anvils.
	anvilNames := make([]string, 0, len(m.anvils))
	for name := range m.anvils {
		anvilNames = append(anvilNames, name)
	}
	sort.Strings(anvilNames)
	if len(anvilNames) == 0 {
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("no anvils registered")}
		}
	}

	m.formType = "task"
	m.formPriority = "2"
	m.formAnvil = anvilNames[0]

	anvilOpts := make([]huh.Option[string], len(anvilNames))
	for i, n := range anvilNames {
		anvilOpts[i] = huh.NewOption(n, n)
	}

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Title").
				Value(&m.formTitle),
			huh.NewText().
				Title("Description").
				Value(&m.formDescription),
			huh.NewSelect[string]().
				Title("Type").
				Options(
					huh.NewOption("Task", "task"),
					huh.NewOption("Bug", "bug"),
					huh.NewOption("Feature", "feature"),
					huh.NewOption("Epic", "epic"),
				).
				Value(&m.formType),
			huh.NewSelect[string]().
				Title("Priority").
				Options(
					huh.NewOption("P0 — Critical", "0"),
					huh.NewOption("P1 — High", "1"),
					huh.NewOption("P2 — Medium", "2"),
					huh.NewOption("P3 — Low", "3"),
					huh.NewOption("P4 — Backlog", "4"),
				).
				Value(&m.formPriority),
			huh.NewSelect[string]().
				Title("Anvil").
				Options(anvilOpts...).
				Value(&m.formAnvil),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormNewBead
	return m.activeForm.Init()
}

// openEditBeadForm creates and displays the edit bead form for the selected bead.
func (m *Model) openEditBeadForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	m.formTarget = b
	m.formTitle = b.Title
	m.formDescription = b.Description

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Title").
				Value(&m.formTitle),
			huh.NewText().
				Title("Description").
				Value(&m.formDescription),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormEditBead
	return m.activeForm.Init()
}

// openCloseBeadForm creates and displays the close bead form for the selected bead.
func (m *Model) openCloseBeadForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}
	if b.Status == "closed" {
		return nil
	}

	m.formTarget = b
	m.formReason = ""

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(fmt.Sprintf("Close %s — Reason (optional)", b.ID)).
				Value(&m.formReason),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormCloseBead
	return m.activeForm.Init()
}

// reopenSelectedBead dispatches a reopen command for the selected bead.
func (m *Model) reopenSelectedBead() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}
	if b.Status != "closed" {
		return nil
	}

	anvilPath, ok := m.anvils[b.Anvil]
	if !ok {
		return nil
	}
	return ReopenBeadCmd(anvilPath, b.ID)
}

// driveHuhForm updates a huh form and returns the resulting command for
// bubbletea's runtime to execute. This is the standard huh embedding pattern —
// bubbletea handles cmd execution and delivers resulting messages back through
// Update, avoiding goroutine leaks from manual sync driving.
func (m *Model) driveHuhForm(form **huh.Form, msg tea.Msg) tea.Cmd {
	f, cmd := (*form).Update(msg)
	if f != nil {
		if hf, ok := f.(*huh.Form); ok {
			*form = hf
		}
	}
	return cmd
}

// View renders the current state.
func (m *Model) View() string {
	if m.loading {
		titleStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent).
			Padding(1, 2)
		return titleStyle.Render("⚒ Forge Ledger — Loading beads...")
	}

	var out string
	switch m.view {
	case ViewKanban:
		out = m.renderKanban()
	default:
		out = m.renderList()
	}

	// Overlay active form.
	if m.activeForm != nil {
		formView := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(1, 2).
			Render(m.activeForm.View())
		out = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, formView,
			lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
	}

	// Overlay toasts at the bottom using ANSI-aware compositor.
	toastView := m.renderToasts()
	if toastView != "" {
		out = placeToastsOverlay(m.width, m.height, 1, toastView, out)
	}

	return out
}
