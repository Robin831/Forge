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
	ViewHierarchy
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
	FormLabel
	FormPriority
	FormComment
	FormNotes
	FormAssign
	FormAddDep      // d key: pick a bead to add as a dependency
	FormViewDeps    // b key: view and optionally remove dependencies
	FormBulkLabel   // ctrl+l: set a label on all selected beads
	FormBulkPriority // ctrl+p: set priority on all selected beads
)

// Model is the top-level Bubbletea model for the Ledger TUI.
type Model struct {
	beads    []Bead
	loading  bool
	fetching bool // true while a FetchAllBeads call is in-flight
	err      error

	width  int
	height int

	view      ViewMode
	list      listState
	kanban    kanbanState
	hierarchy hierarchyState
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

	// Form field bindings for metadata operations
	formLabel       string // label name
	formLabelAction string // "add" or "remove"
	formNotes       string // comment/notes text
	formAssignee    string // assignee username
	formDepID       string // dep bead ID selected in dep forms; "dep:<id>" or "blocks:<id>" for removal

	// Toast notifications
	toasts      []toast
	nextToastID int

	// Bulk selection state for multi-select operations.
	bulk BulkState
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

		// Clear bulk selection on Escape (when no form or sort overlay is active).
		if msg.String() == "esc" && m.list.sortForm == nil && m.bulk.Count() > 0 {
			m.bulk.Clear()
			return m, nil
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
			case "p":
				return m, m.openPriorityForm()
			case "c":
				return m, m.openCommentForm()
			case "N":
				return m, m.openNotesForm()
			case "a":
				return m, m.openAssignForm()
			case "d":
				return m, m.openAddDepForm()
			case "b":
				return m, m.openDepViewerForm()
			// Bulk selection operations.
			case " ":
				if b := m.selectedBead(); b != nil {
					m.bulk.Toggle(b.ID)
				}
				return m, nil
			case "ctrl+a":
				m.selectAllVisible()
				return m, nil
			case "ctrl+x":
				if m.bulk.Count() == 0 {
					return m, nil
				}
				return m, BulkCloseCmd(m.anvils, m.beads, m.bulk.copySelected())
			case "ctrl+l":
				return m, m.openBulkLabelForm()
			case "ctrl+p":
				return m, m.openBulkPriorityForm()
			}
			// "l" opens label form only in list view; in kanban it navigates lanes.
			if msg.String() == "l" && m.view == ViewList {
				return m, m.openLabelForm()
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
		case ViewHierarchy:
			cmd := m.updateHierarchy(msg)
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
		m.refreshHierarchy()

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

	case DepAddedMsg:
		cmd := m.addToast(fmt.Sprintf("Added dep: %s → %s", msg.BeadID, msg.DepID), false)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case DepRemovedMsg:
		cmd := m.addToast(fmt.Sprintf("Removed dep: %s → %s", msg.BeadID, msg.DepID), false)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case BulkCloseResultMsg:
		m.bulk.Clear()
		var text string
		if msg.Failed == 0 {
			text = fmt.Sprintf("Closed %d beads", msg.Closed)
		} else {
			text = fmt.Sprintf("Closed %d beads, %d failed", msg.Closed, msg.Failed)
		}
		cmd := m.addToast(text, msg.Failed > 0)
		m.fetching = true
		return m, tea.Batch(cmd, FetchAllBeads(m.anvils, m.db))

	case BulkUpdatedMsg:
		m.bulk.Clear()
		var text string
		if msg.Failed == 0 {
			text = fmt.Sprintf("Updated %d beads", msg.Updated)
		} else {
			text = fmt.Sprintf("Updated %d beads, %d failed", msg.Updated, msg.Failed)
		}
		cmd := m.addToast(text, msg.Failed > 0)
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
	m.formLabel = ""
	m.formLabelAction = ""
	m.formNotes = ""
	m.formAssignee = ""
	m.formDepID = ""
}

// selectAllVisible marks all currently visible beads as selected.
// In list view this uses the sorted order; all other views use the full bead list.
func (m *Model) selectAllVisible() {
	switch m.view {
	case ViewList:
		sorted := sortBeads(m.beads, m.list.sortBy)
		m.bulk.SelectAll(sorted)
	default:
		m.bulk.SelectAll(m.beads)
	}
}

// parsePriority converts a priority string ("0"–"4") to an int, defaulting to 2.
func parsePriority(s string) int {
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "3":
		return 3
	case "4":
		return 4
	default:
		return 2
	}
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
		return NewBeadCmd(anvilPath, m.formTitle, m.formDescription, m.formType, parsePriority(m.formPriority))

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

	case FormLabel:
		if m.formTarget == nil || m.formLabel == "" {
			return nil
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return UpdateLabelCmd(anvilPath, m.formTarget.ID, m.formLabel, m.formLabelAction == "remove")

	case FormPriority:
		if m.formTarget == nil {
			return nil
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return UpdatePriorityCmd(anvilPath, m.formTarget.ID, parsePriority(m.formPriority))

	case FormComment:
		if m.formTarget == nil || m.formNotes == "" {
			return nil
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return AppendNotesCmd(anvilPath, m.formTarget.ID, m.formNotes)

	case FormNotes:
		if m.formTarget == nil || m.formNotes == "" {
			return nil
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return UpdateNotesCmd(anvilPath, m.formTarget.ID, m.formNotes)

	case FormAssign:
		if m.formTarget == nil {
			return nil
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return UpdateAssigneeCmd(anvilPath, m.formTarget.ID, m.formAssignee)

	case FormAddDep:
		if m.formTarget == nil || m.formDepID == "" {
			return nil
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		return AddDepCmd(anvilPath, m.formTarget.ID, m.formDepID)

	case FormBulkLabel:
		if m.formLabel == "" {
			return nil
		}
		return BulkLabelCmd(m.anvils, m.beads, m.bulk.copySelected(), m.formLabel, m.formLabelAction == "remove")

	case FormBulkPriority:
		return BulkPriorityCmd(m.anvils, m.beads, m.bulk.copySelected(), parsePriority(m.formPriority))

	case FormViewDeps:
		if m.formTarget == nil || m.formDepID == "" {
			return nil // "— done —" was selected or nothing selected
		}
		anvilPath, ok := m.anvils[m.formTarget.Anvil]
		if !ok {
			return func() tea.Msg {
				return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", m.formTarget.ID, m.formTarget.Anvil)}
			}
		}
		// formDepID encodes the removal direction:
		//   "dep:<depID>"    → this bead depends on depID; remove: bd dep remove <beadID> <depID>
		//   "blocks:<childID>" → childID depends on this bead; remove: bd dep remove <childID> <beadID>
		const depPrefix = "dep:"
		const blocksPrefix = "blocks:"
		switch {
		case len(m.formDepID) > len(depPrefix) && m.formDepID[:len(depPrefix)] == depPrefix:
			depID := m.formDepID[len(depPrefix):]
			return RemoveDepCmd(anvilPath, m.formTarget.ID, depID)
		case len(m.formDepID) > len(blocksPrefix) && m.formDepID[:len(blocksPrefix)] == blocksPrefix:
			childID := m.formDepID[len(blocksPrefix):]
			// Use the child bead's anvil path so bd dep remove runs in the correct repo.
			childAnvilPath := anvilPath
			for i := range m.beads {
				if m.beads[i].ID == childID {
					if p, ok := m.anvils[m.beads[i].Anvil]; ok {
						childAnvilPath = p
					}
					break
				}
			}
			return RemoveDepCmd(childAnvilPath, childID, m.formTarget.ID)
		}
		return nil
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
	case ViewHierarchy:
		flat := m.hierarchy.flat
		idx := m.hierarchy.vp.Selected()
		if idx >= 0 && idx < len(flat) {
			item := flat[idx]
			if !item.isDep && item.bead != nil {
				return item.bead
			}
			if item.isDep && item.depBead != nil {
				return item.depBead
			}
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

// openLabelForm creates and displays the label manager form for the selected bead.
func (m *Model) openLabelForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	m.formTarget = b
	m.formLabel = ""
	m.formLabelAction = "add"

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(fmt.Sprintf("Label for %s", b.ID)).
				Placeholder("label name").
				Value(&m.formLabel),
			huh.NewSelect[string]().
				Title("Action").
				Options(
					huh.NewOption("Add label", "add"),
					huh.NewOption("Remove label", "remove"),
				).
				Value(&m.formLabelAction),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormLabel
	return m.activeForm.Init()
}

// openPriorityForm creates and displays the priority changer form for the selected bead.
func (m *Model) openPriorityForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	m.formTarget = b
	m.formPriority = fmt.Sprintf("%d", b.Priority)

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Priority for %s", b.ID)).
				Options(
					huh.NewOption("P0 — Critical", "0"),
					huh.NewOption("P1 — High", "1"),
					huh.NewOption("P2 — Medium", "2"),
					huh.NewOption("P3 — Low", "3"),
					huh.NewOption("P4 — Backlog", "4"),
				).
				Value(&m.formPriority),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormPriority
	return m.activeForm.Init()
}

// openCommentForm creates and displays the comment textarea overlay for the selected bead.
func (m *Model) openCommentForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	m.formTarget = b
	m.formNotes = ""

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewText().
				Title(fmt.Sprintf("Add comment to %s", b.ID)).
				Value(&m.formNotes),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormComment
	return m.activeForm.Init()
}

// openNotesForm creates and displays the notes editor overlay for the selected bead.
func (m *Model) openNotesForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	m.formTarget = b
	m.formNotes = ""

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewText().
				Title(fmt.Sprintf("Edit notes for %s", b.ID)).
				Value(&m.formNotes),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormNotes
	return m.activeForm.Init()
}

// openAssignForm creates and displays the assignee input form for the selected bead.
func (m *Model) openAssignForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	m.formTarget = b
	m.formAssignee = b.Assignee

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(fmt.Sprintf("Assign %s", b.ID)).
				Placeholder("username (empty to unassign)").
				Value(&m.formAssignee),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormAssign
	return m.activeForm.Init()
}

// openAddDepForm opens a bead picker so the user can add a dependency to the
// selected bead. The picker is filtered to exclude the bead itself and any
// IDs it already depends on.
func (m *Model) openAddDepForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	// Build set of IDs to exclude: self + existing DependsOn entries.
	excluded := make(map[string]bool, len(b.DependsOn)+1)
	excluded[b.ID] = true
	for _, id := range b.DependsOn {
		excluded[id] = true
	}

	// Collect candidate beads from the same anvil, sorted by ID.
	var candidates []Bead
	for i := range m.beads {
		// Only allow dependencies within the same anvil as the selected bead.
		if m.beads[i].Anvil != b.Anvil {
			continue
		}
		if excluded[m.beads[i].ID] {
			continue
		}
		candidates = append(candidates, m.beads[i])
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})

	if len(candidates) == 0 {
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("%s already depends on all available beads", b.ID)}
		}
	}

	opts := make([]huh.Option[string], len(candidates))
	for i, c := range candidates {
		label := fmt.Sprintf("%-14s %s", c.ID, truncate(c.Title, 40))
		opts[i] = huh.NewOption(label, c.ID)
	}

	m.formTarget = b
	m.formDepID = candidates[0].ID

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Add dependency to %s (it will depend on…)", b.ID)).
				Options(opts...).
				Value(&m.formDepID),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormAddDep
	return m.activeForm.Init()
}

// openDepViewerForm opens a viewer showing the selected bead's dependency
// relationships (what it blocks, what blocks it). The user can optionally
// select a dependency to remove.
func (m *Model) openDepViewerForm() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}

	// Build a lookup map for resolving IDs to titles.
	byID := make(map[string]*Bead, len(m.beads))
	for i := range m.beads {
		byID[m.beads[i].ID] = &m.beads[i]
	}

	// Build options: DependsOn entries (this bead depends on them) + Blocks entries (they depend on this).
	var opts []huh.Option[string]

	for _, depID := range b.DependsOn {
		label := fmt.Sprintf("↓ depends on: %-14s", depID)
		if dep := byID[depID]; dep != nil {
			label = fmt.Sprintf("↓ depends on: %-14s  %s", depID, truncate(dep.Title, 30))
		}
		opts = append(opts, huh.NewOption(label, "dep:"+depID))
	}

	for _, childID := range b.Blocks {
		label := fmt.Sprintf("↑ blocks:     %-14s", childID)
		if child := byID[childID]; child != nil {
			label = fmt.Sprintf("↑ blocks:     %-14s  %s", childID, truncate(child.Title, 30))
		}
		opts = append(opts, huh.NewOption(label, "blocks:"+childID))
	}

	if len(opts) == 0 {
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("%s has no dependencies", b.ID)}
		}
	}

	// Append a no-op "done" option so the user can close without removing anything.
	opts = append(opts, huh.NewOption("— done (no removal) —", ""))

	m.formTarget = b
	m.formDepID = "" // default to "done"

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Dependencies for %s — select to remove", b.ID)).
				Options(opts...).
				Value(&m.formDepID),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormViewDeps
	return m.activeForm.Init()
}

// openBulkLabelForm opens a label input form for bulk label operations.
func (m *Model) openBulkLabelForm() tea.Cmd {
	if m.bulk.Count() == 0 {
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("no beads selected")}
		}
	}

	m.formLabel = ""
	m.formLabelAction = "add"

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(fmt.Sprintf("Label for %d selected beads", m.bulk.Count())).
				Placeholder("label name").
				Value(&m.formLabel),
			huh.NewSelect[string]().
				Title("Action").
				Options(
					huh.NewOption("Add label", "add"),
					huh.NewOption("Remove label", "remove"),
				).
				Value(&m.formLabelAction),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormBulkLabel
	return m.activeForm.Init()
}

// openBulkPriorityForm opens a priority selector form for bulk priority operations.
func (m *Model) openBulkPriorityForm() tea.Cmd {
	if m.bulk.Count() == 0 {
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("no beads selected")}
		}
	}

	m.formPriority = "2"

	m.activeForm = huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Priority for %d selected beads", m.bulk.Count())).
				Options(
					huh.NewOption("P0 — Critical", "0"),
					huh.NewOption("P1 — High", "1"),
					huh.NewOption("P2 — Medium", "2"),
					huh.NewOption("P3 — Low", "3"),
					huh.NewOption("P4 — Backlog", "4"),
				).
				Value(&m.formPriority),
		),
	).WithShowHelp(false).WithShowErrors(false)

	m.activeFormKind = FormBulkPriority
	return m.activeForm.Init()
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
	case ViewHierarchy:
		out = m.renderHierarchy()
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
