// Package ledger provides an interactive TUI for browsing and managing beads
// across all registered Forge anvils.
package ledger

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

// refreshInterval controls how often the ledger auto-refreshes bead data.
const refreshInterval = 30 * time.Second

// Minimum terminal dimensions required for the Ledger TUI to render correctly.
const (
	minTermWidth  = 80
	minTermHeight = 24
)

// Narrow-terminal thresholds for adaptive column layout.
const (
	// Below this width, kanban degrades from 4 lanes to 2 visible lanes.
	narrowKanbanWidth = 100
	// Below this width, the Assignee column is hidden in list view.
	narrowDropAssigneeWidth = 100
	// Below this width, both Labels and Assignee columns are hidden in list view.
	narrowDropLabelsWidth = 90
)

// ViewMode determines which screen the Ledger is showing.
type ViewMode int

const (
	ViewList ViewMode = iota
	ViewKanban
	ViewHierarchy
	ViewAnvils // top-level anvil picker — shown on startup
)

// anvilState holds scroll state for the anvil picker.
type anvilState struct {
	vp scrollViewport
}

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

	anvils       map[string]string            // name → path
	anvilConfigs map[string]config.AnvilConfig // per-anvil configuration (for dep updates)
	db           *state.DB

	// Anvil picker state — used in ViewAnvils mode.
	anvilSt       anvilState
	selectedAnvil string // name of the anvil whose beads are currently shown; "" = none

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

	// Event panel state — persistent log of recent operations and errors.
	eventLog       []EventEntry
	showEventPanel bool

	// Filter state — persists across view switches.
	showClosed bool // whether to show closed beads (default false; toggle with 'c')

	// Bulk selection state for multi-select operations.
	bulk BulkState

	// AI improvement overlay state.
	aiOverlay       aiOverlayState
	aiTarget        *Bead
	aiResult        aiImprovementResult
	aiSpinFrame     int
	aiApprovalFocus int // 0 = Accept, 1 = Reject
	aiRunID         int // incremented each time a run starts; used to discard stale completions

	// Help overlay state.
	helpSt helpState

	// Detail panel state — persistent bead detail shown on the right side.
	showDetailPanel bool

}

// NewModel creates a new Ledger model. anvilConfigs is the per-anvil
// configuration map from forge.yaml (may be nil).
func NewModel(anvils map[string]string, anvilConfigs map[string]config.AnvilConfig, db *state.DB) *Model {
	return &Model{
		anvils:          anvils,
		anvilConfigs:    anvilConfigs,
		db:              db,
		loading:         false,
		view:            ViewAnvils,
		showClosed:      false,
		helpSt:          newHelpState(),
		showDetailPanel: true, // auto-shown on wide terminals (≥minWidthForDetailPanel)
	}
}

// filteredBeads returns m.beads with the current anvil filter and closed
// visibility applied.
func (m *Model) filteredBeads() []Bead {
	var result []Bead
	for _, b := range m.beads {
		if m.selectedAnvil != "" && b.Anvil != m.selectedAnvil {
			continue
		}
		if !m.showClosed && b.Status == "closed" {
			continue
		}
		result = append(result, b)
	}
	return result
}

// filteredBeadsCount returns the count of beads matching the current filters
// without allocating a slice.
func (m *Model) filteredBeadsCount() int {
	count := 0
	for _, b := range m.beads {
		if m.selectedAnvil != "" && b.Anvil != m.selectedAnvil {
			continue
		}
		if !m.showClosed && b.Status == "closed" {
			continue
		}
		count++
	}
	return count
}

// sortedAnvilNames returns the registered anvil names in alphabetical order.
func (m *Model) sortedAnvilNames() []string {
	names := make([]string, 0, len(m.anvils))
	for name := range m.anvils {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// cycleView advances the view mode: List → Kanban → Hierarchy → List.
func (m *Model) cycleView() {
	switch m.view {
	case ViewList:
		m.view = ViewKanban
		m.refreshKanbanLanes()
	case ViewKanban:
		m.view = ViewHierarchy
		m.refreshHierarchy()
	default: // ViewHierarchy
		m.view = ViewList
	}
}


// filterHint returns a compact display string reflecting the active filters,
// e.g. "  [forge]  +5 closed". Returns "" when no filters are active.
func (m *Model) filterHint() string {
	var sb strings.Builder
	if m.selectedAnvil != "" {
		sb.WriteString(fmt.Sprintf("  [%s]", m.selectedAnvil))
	}
	if !m.showClosed {
		closedCount := 0
		for _, b := range m.beads {
			if b.Status == "closed" && (m.selectedAnvil == "" || b.Anvil == m.selectedAnvil) {
				closedCount++
			}
		}
		if closedCount > 0 {
			sb.WriteString(fmt.Sprintf("  +%d closed", closedCount))
		}
	}
	return sb.String()
}

// refreshBeads returns a Cmd that re-fetches beads for the current context:
// when a specific anvil is selected it fetches only that anvil; otherwise
// returns nil (no anvil is active, e.g. we are in ViewAnvils mode).
func (m *Model) refreshBeads() tea.Cmd {
	if m.selectedAnvil != "" {
		path, ok := m.anvils[m.selectedAnvil]
		if !ok {
			return nil
		}
		return FetchAnvilBeads(m.selectedAnvil, path, m.db)
	}
	return nil
}

// goBackToAnvils switches the view back to the top-level anvil picker.
func (m *Model) goBackToAnvils() (tea.Model, tea.Cmd) {
	m.view = ViewAnvils
	m.selectedAnvil = ""
	m.loading = false
	m.fetching = false
	m.beads = nil
	m.bulk.Clear()
	return m, nil
}

// enterAnvil selects the given anvil and loads its beads.
func (m *Model) enterAnvil(name string) tea.Cmd {
	path, ok := m.anvils[name]
	if !ok {
		return nil
	}
	m.selectedAnvil = name
	m.view = ViewList
	m.loading = true
	m.fetching = true
	m.beads = nil
	return FetchAnvilBeads(name, path, m.db)
}

// updateAnvilList handles key events when the anvil picker is active.
func (m *Model) updateAnvilList(msg tea.KeyMsg) tea.Cmd {
	names := m.sortedAnvilNames()
	switch msg.String() {
	case "j", "down":
		m.anvilSt.vp.ScrollDown(len(names))
	case "k", "up":
		m.anvilSt.vp.ScrollUp()
	case "enter", " ":
		idx := m.anvilSt.vp.Selected()
		if idx >= 0 && idx < len(names) {
			return m.enterAnvil(names[idx])
		}
	}
	return nil
}

// Init schedules the periodic refresh tick. Bead data is fetched lazily when
// the user selects an anvil from the top-level ViewAnvils screen.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(tea.SetWindowTitle("The Forge — Ledger"), tickCmd())
}

// Update handles incoming messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle toast dismissals first — they apply regardless of form state.
	if dismiss, ok := msg.(toastDismissMsg); ok {
		m.dismissToast(dismiss.id)
		return m, nil
	}

	switch msg := msg.(type) {
	case aiSpinnerTickMsg:
		if m.aiOverlay == aiOverlaySpinner {
			m.aiSpinFrame++
			return m, aiSpinnerTickCmd()
		}
		return m, nil

	case aiImprovementDoneMsg:
		// Ignore completions from a previous run (e.g. user pressed esc and started a new one).
		if msg.runID != m.aiRunID || m.aiOverlay == aiOverlayNone {
			return m, nil
		}
		if msg.err != nil {
			m.addEvent(EventError, fmt.Sprintf("AI improve failed: %s", msg.err))
			m.aiOverlay = aiOverlayNone
			m.aiTarget = nil
			return m, m.addToast(fmt.Sprintf("AI improve failed: %s", msg.err), true)
		}
		m.aiResult = msg.result
		m.aiOverlay = aiOverlayApproval
		m.aiApprovalFocus = 0
		return m, nil

	case tea.KeyMsg:
		// When the AI overlay is active, route key events to the AI overlay handler.
		if m.aiOverlay != aiOverlayNone {
			return m.updateAIOverlay(msg)
		}
		// When a form overlay is active, route key events to the form handler.
		if m.activeForm != nil {
			return m.updateForm(msg)
		}
		// When the help overlay is active, route key events to the help handler.
		if m.helpSt.show {
			return m.updateHelpOverlay(msg)
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

		// In ViewAnvils mode, handle help toggle then delegate remaining keys.
		if m.view == ViewAnvils {
			if msg.String() == "?" {
				m.helpSt.show = true
				m.helpSt.vpReady = false
				return m, nil
			}
			cmd := m.updateAnvilList(msg)
			return m, cmd
		}

		// Clear bulk selection on Escape (when no form or sort overlay is active).
		if msg.String() == "esc" && m.list.sortForm == nil && m.bulk.Count() > 0 {
			m.bulk.Clear()
			return m, nil
		}

		// ESC (when no bulk selection and no sort form) returns to anvil picker.
		if msg.String() == "esc" && m.list.sortForm == nil && m.bulk.Count() == 0 {
			return m.goBackToAnvils()
		}

		// CRUD key bindings (available in bead views when no form is open).
		if m.list.sortForm == nil {
			switch msg.String() {
			case "?":
				m.helpSt.show = true
				m.helpSt.vpReady = false
				return m, nil
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
				// 'c' toggles visibility of closed beads.
				m.showClosed = !m.showClosed
				filtered := m.filteredBeads()
				m.list.vp.ClampToTotal(len(filtered))
				m.refreshKanbanLanes()
				m.refreshHierarchy()
				return m, nil
			case "C":
				return m, m.openCommentForm()
			case "N":
				return m, m.openNotesForm()
			case "a":
				return m, m.openAssignForm()
			case "d":
				return m, m.openAddDepForm()
			case "b":
				return m, m.openDepViewerForm()
			case "i":
				return m, m.startAIImprovement()
			case "\\":
				m.showDetailPanel = !m.showDetailPanel
				return m, nil
			// Bulk selection operations.
			case " ":
				// Dep sub-rows in hierarchy view are display-only; skip bulk toggle for them.
				if b := m.selectedBead(); b != nil && !m.isFocusedDepRow() {
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
			case "f":
				// 'f' goes back to the anvil picker from a bead view.
				return m.goBackToAnvils()
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

	case tea.MouseMsg:
		// Only handle scroll wheel events; ignore clicks and motion.
		if msg.Action != tea.MouseActionPress {
			break
		}
		// Don't let scroll events mutate underlying state while a blocking overlay
		// (form, AI overlay, or sort selector) is active.
		overlayActive := m.activeForm != nil ||
			m.aiOverlay != aiOverlayNone ||
			m.list.sortForm != nil
		if overlayActive {
			break
		}
		switch msg.Button {
		case tea.MouseButtonWheelDown:
			if m.helpSt.show {
				m.helpSt.vp.ScrollDown(1)
				break
			}
			switch m.view {
			case ViewAnvils:
				m.anvilSt.vp.ScrollDown(len(m.sortedAnvilNames()))
			case ViewList:
				m.list.vp.ScrollDown(m.filteredBeadsCount())
			case ViewKanban:
				lane := m.kanban.activeLane
				m.kanban.laneVP[lane].ScrollDown(len(m.kanban.lanes[lane]))
			case ViewHierarchy:
				m.hierarchy.vp.ScrollDown(len(m.hierarchy.flat))
			}
		case tea.MouseButtonWheelUp:
			if m.helpSt.show {
				m.helpSt.vp.ScrollUp(1)
				break
			}
			switch m.view {
			case ViewAnvils:
				m.anvilSt.vp.ScrollUp()
			case ViewList:
				m.list.vp.ScrollUp()
			case ViewKanban:
				m.kanban.laneVP[m.kanban.activeLane].ScrollUp()
			case ViewHierarchy:
				m.hierarchy.vp.ScrollUp()
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case UpdateBeadsMsg:
		m.loading = false
		m.fetching = false
		m.beads = msg.Beads
		m.err = msg.Err
		if msg.Err != nil {
			m.addEvent(EventError, fmt.Sprintf("Refresh error: %v", msg.Err))
		}
		m.list.vp.ClampToTotal(len(m.filteredBeads()))
		m.refreshKanbanLanes()
		m.refreshHierarchy()

	case moveBeadMsg:
		if msg.Err != nil {
			m.addEvent(EventError, fmt.Sprintf("Move failed: %v", msg.Err))
			m.err = msg.Err
			cmd := m.addToast(fmt.Sprintf("Move failed: %v", msg.Err), true)
			m.fetching = true
			return m, tea.Batch(cmd, m.refreshBeads())
		}
		m.fetching = true
		return m, m.refreshBeads()

	case BeadCreatedMsg:
		label := msg.ID
		if label == "" {
			label = "bead"
		}
		m.addEvent(EventInfo, fmt.Sprintf("Created %s", label))
		cmd := m.addToast(fmt.Sprintf("Created %s", label), false)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case BeadUpdatedMsg:
		m.addEvent(EventInfo, fmt.Sprintf("Updated %s", msg.ID))
		cmd := m.addToast(fmt.Sprintf("Updated %s", msg.ID), false)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case BeadClosedMsg:
		m.addEvent(EventInfo, fmt.Sprintf("Closed %s", msg.ID))
		cmd := m.addToast(fmt.Sprintf("Closed %s", msg.ID), false)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case BeadReopenedMsg:
		m.addEvent(EventInfo, fmt.Sprintf("Reopened %s", msg.ID))
		cmd := m.addToast(fmt.Sprintf("Reopened %s", msg.ID), false)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case DepAddedMsg:
		m.addEvent(EventInfo, fmt.Sprintf("Added dep: %s → %s", msg.BeadID, msg.DepID))
		cmd := m.addToast(fmt.Sprintf("Added dep: %s → %s", msg.BeadID, msg.DepID), false)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case DepRemovedMsg:
		m.addEvent(EventInfo, fmt.Sprintf("Removed dep: %s → %s", msg.BeadID, msg.DepID))
		cmd := m.addToast(fmt.Sprintf("Removed dep: %s → %s", msg.BeadID, msg.DepID), false)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case BulkCloseResultMsg:
		m.bulk.Clear()
		var text string
		if msg.Failed == 0 {
			text = fmt.Sprintf("Closed %d beads", msg.Closed)
			m.addEvent(EventInfo, text)
		} else {
			text = fmt.Sprintf("Closed %d beads, %d failed", msg.Closed, msg.Failed)
			m.addEvent(EventWarn, text)
		}
		cmd := m.addToast(text, msg.Failed > 0)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case BulkUpdatedMsg:
		m.bulk.Clear()
		var text string
		if msg.Failed == 0 {
			text = fmt.Sprintf("Updated %d beads", msg.Updated)
			m.addEvent(EventInfo, text)
		} else {
			text = fmt.Sprintf("Updated %d beads, %d failed", msg.Updated, msg.Failed)
			m.addEvent(EventWarn, text)
		}
		cmd := m.addToast(text, msg.Failed > 0)
		m.fetching = true
		return m, tea.Batch(cmd, m.refreshBeads())

	case ActionErrorMsg:
		m.addEvent(EventError, msg.Err.Error())
		cmd := m.addToast(msg.Err.Error(), true)
		return m, cmd

	case tickMsg:
		if m.fetching {
			return m, tickCmd()
		}
		// In ViewAnvils mode no beads are loaded, so skip the periodic fetch.
		if m.view == ViewAnvils {
			return m, tickCmd()
		}
		m.fetching = true
		return m, tea.Batch(tickCmd(), m.refreshBeads())

	default:
		// Forward unrecognised messages (e.g. huh internal focus/blur
		// commands returned by Form.Init) to the active form so that
		// fields initialise correctly and Enter/Tab work as expected.
		if m.activeForm != nil {
			return m.updateForm(msg)
		}
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
		pendingMsg := m.pendingToastForForm()
		actionCmd := m.executeFormAction()
		m.clearForm()
		var pendingCmd tea.Cmd
		if actionCmd != nil {
			pendingCmd = m.addToast(pendingMsg, false)
		}
		if cmd != nil {
			return m, tea.Batch(cmd, pendingCmd, actionCmd)
		}
		return m, tea.Batch(pendingCmd, actionCmd)
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

// pendingToastForForm returns the "in progress" message to show immediately
// when a form is submitted, before the bd command completes. Must be called
// before clearForm() so that formTarget and activeFormKind are still set.
func (m *Model) pendingToastForForm() string {
	id := "bead"
	if m.formTarget != nil && m.formTarget.ID != "" {
		id = m.formTarget.ID
	}
	switch m.activeFormKind {
	case FormNewBead:
		return "Creating bead..."
	case FormEditBead:
		return fmt.Sprintf("Updating %s...", id)
	case FormCloseBead:
		return fmt.Sprintf("Closing %s...", id)
	case FormLabel:
		if m.formLabelAction == "remove" {
			return fmt.Sprintf("Removing label from %s...", id)
		}
		return fmt.Sprintf("Adding label to %s...", id)
	case FormPriority:
		return fmt.Sprintf("Updating priority on %s...", id)
	case FormComment:
		return fmt.Sprintf("Adding comment to %s...", id)
	case FormNotes:
		return fmt.Sprintf("Updating notes on %s...", id)
	case FormAssign:
		return fmt.Sprintf("Updating assignee on %s...", id)
	case FormAddDep:
		return fmt.Sprintf("Adding dependency to %s...", id)
	case FormViewDeps:
		return fmt.Sprintf("Removing dependency from %s...", id)
	case FormBulkLabel:
		return fmt.Sprintf("Updating labels on %d bead(s)...", m.bulk.Count())
	case FormBulkPriority:
		return fmt.Sprintf("Updating priority on %d bead(s)...", m.bulk.Count())
	default:
		return "Processing..."
	}
}

// isFocusedDepRow reports whether the cursor is currently on a dependency
// sub-row (→ arrow item) in the hierarchy view. These rows are display-only
// and should not participate in bulk selection.
func (m *Model) isFocusedDepRow() bool {
	if m.view != ViewHierarchy {
		return false
	}
	idx := m.hierarchy.vp.Selected()
	if idx >= 0 && idx < len(m.hierarchy.flat) {
		return m.hierarchy.flat[idx].isDep
	}
	return false
}

// selectAllVisible marks all currently visible beads as selected.
// List view uses the sorted order; hierarchy view iterates the flattened visible
// tree and skips hidden (collapsed) beads and dependency sub-rows; kanban uses
// the full bead list (all beads are shown across lanes).
func (m *Model) selectAllVisible() {
	switch m.view {
	case ViewList:
		sorted := sortBeads(m.filteredBeads(), m.list.sortBy)
		m.bulk.SelectAll(sorted)
	case ViewHierarchy:
		// Only select beads that are actually visible in the flattened tree.
		// Dep sub-rows (isDep=true) are display-only and excluded.
		var visible []Bead
		for _, item := range m.hierarchy.flat {
			if !item.isDep && item.bead != nil {
				visible = append(visible, *item.bead)
			}
		}
		m.bulk.SelectAll(visible)
	default:
		m.bulk.SelectAll(m.filteredBeads())
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
	case ViewAnvils:
		return nil
	case ViewList:
		sorted := sortBeads(m.filteredBeads(), m.list.sortBy)
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

// renderTooSmall renders a full-screen message when the terminal is below the
// minimum required dimensions.
func (m *Model) renderTooSmall() string {
	msg := fmt.Sprintf(
		"Terminal too small (%dx%d)\nMinimum required: %dx%d",
		m.width, m.height, minTermWidth, minTermHeight,
	)
	style := lipgloss.NewStyle().
		Foreground(colorWarning).
		Bold(true).
		Padding(1, 2)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, style.Render(msg))
}

// View renders the current state.
func (m *Model) View() string {
	// Enforce minimum terminal size before rendering anything else.
	if m.width > 0 && m.height > 0 && (m.width < minTermWidth || m.height < minTermHeight) {
		return m.renderTooSmall()
	}

	if m.loading {
		titleStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent).
			Padding(1, 2)
		dimStyle := lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)
		lines := []string{
			titleStyle.Render(fmt.Sprintf("⚒ Forge Ledger — Loading beads for %s...", m.selectedAnvil)),
			"",
			dimStyle.Render("  Fetching open beads..."),
			dimStyle.Render("  Fetching in-progress beads..."),
			dimStyle.Render("  Fetching recently closed beads..."),
			dimStyle.Render("  Enriching with PR data..."),
			"",
			dimStyle.Render("  This may take a moment on slow database connections."),
		}
		return strings.Join(lines, "\n")
	}

	var out string
	switch m.view {
	case ViewAnvils:
		out = m.renderAnvilList()
	case ViewKanban:
		out = m.renderKanban()
	case ViewHierarchy:
		out = m.renderHierarchy()
	default:
		out = m.renderList()
	}

	// Pad the main content to fill the available height so the activity
	// panel is always anchored at the bottom of the bordered panel.
	if m.detailPanelW() > 0 {
		targetLines := max(m.height-2-m.eventPanelH()-1, 1) // -2 border, -1 joining newline
		contentLines := strings.Count(out, "\n") + 1
		for contentLines < targetLines {
			out += "\n"
			contentLines++
		}
	}

	// Always append the activity panel below the main view content.
	out = out + "\n" + m.renderEventPanel()

	// Composite the detail panel on the right side when it is visible and no
	// full-screen overlay is blocking the main view. The sort-selector form
	// rendered inside renderList() also expands to full width, so we skip
	// compositing in that case too.
	blockingOverlay := m.activeForm != nil ||
		m.aiOverlay != aiOverlayNone ||
		m.helpSt.show ||
		m.list.sortForm != nil
	if m.detailPanelW() > 0 && !blockingOverlay {
		detailStr := m.renderDetailPanel()
		// Wrap the main panel in a rounded border to match the detail panel.
		innerH := max(m.height-2, 1) // -2 for top+bottom border
		clipped := clipLines(out, innerH)
		mainBorder := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Width(m.mainPanelWidth()).
			Height(innerH)
		mainStr := mainBorder.Render(clipped)
		// Clip both rendered panels to exactly m.height lines so they align.
		mainStr = clipLines(mainStr, m.height)
		detailStr = clipLines(detailStr, m.height)
		out = lipgloss.JoinHorizontal(lipgloss.Top, mainStr, detailStr)
	}

	eventPanelVisible := true // always visible

	// Overlay active form. Replaces out entirely with a full-screen placement,
	// hiding the event panel underneath.
	if m.activeForm != nil {
		formView := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(1, 2).
			Render(m.activeForm.View())
		out = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, formView,
			lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
		eventPanelVisible = false
	}

	// Overlay AI improvement spinner or approval.
	switch m.aiOverlay {
	case aiOverlaySpinner:
		out = m.renderAISpinnerOverlay()
		eventPanelVisible = false
	case aiOverlayApproval:
		out = m.renderAIApprovalOverlay()
		eventPanelVisible = false
	}

	// Overlay help screen on top of everything else.
	if m.helpSt.show {
		out = m.renderHelpOverlay()
		eventPanelVisible = false
	}

	// Overlay toasts at the bottom using ANSI-aware compositor.
	// When the event panel is visible, place toasts above it.
	toastView := m.renderToasts()
	if toastView != "" {
		footerH := 1
		if eventPanelVisible {
			footerH = m.eventPanelH()
		}
		out = placeToastsOverlay(m.width, m.height, footerH, toastView, out)
	}

	return out
}

// renderAnvilList renders the top-level anvil picker screen.
func (m *Model) renderAnvilList() string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 2)
	title := titleStyle.Render("⚒ Forge Ledger — Select Anvil")

	panelW := m.mainPanelWidth()
	if panelW <= 0 {
		panelW = 80
	}

	footer := m.renderFooter()
	footerH := strings.Count(footer, "\n") + 1
	viewH := m.height - 2 - footerH // title line + blank line + footer
	if viewH < 1 {
		viewH = 1
	}

	names := m.sortedAnvilNames()
	m.anvilSt.vp.AdjustViewport(viewH, len(names))
	start, end := m.anvilSt.vp.VisibleRange(viewH, len(names))

	selectedStyle := lipgloss.NewStyle().
		Background(lipgloss.AdaptiveColor{Dark: "237", Light: "252"}).
		Foreground(colorAccent).
		Bold(true)
	cursorStyle := lipgloss.NewStyle().Foreground(colorAccent)
	dimStyle := lipgloss.NewStyle().Foreground(colorMuted)

	itemWidth := panelW - 4
	if itemWidth < 1 {
		itemWidth = 1
	}

	var rows []string
	for i := start; i < end; i++ {
		name := names[i]
		path := m.anvils[name]
		selected := i == m.anvilSt.vp.Selected()

		cursor := "  "
		if selected {
			cursor = cursorStyle.Render("▶ ")
		}
		prefix := fmt.Sprintf("%s%-18s  ", cursor, name)
		prefixWidth := lipgloss.Width(prefix)
		pathWidth := itemWidth - prefixWidth
		if pathWidth < 0 {
			pathWidth = 0
		}
		line := prefix + dimStyle.Render(truncate(path, pathWidth))

		if selected {
			// Pad line to fill the row width so the background color spans the full width.
			lineWidth := lipgloss.Width(line)
			if lineWidth < itemWidth {
				line += strings.Repeat(" ", itemWidth-lineWidth)
			}
			rows = append(rows, selectedStyle.Render(line))
		} else {
			rows = append(rows, line)
		}
	}

	if len(names) == 0 {
		rows = []string{
			dimStyle.Padding(0, 2).Render("No anvils registered. Run: forge anvil add <name> <path>"),
		}
	}

	body := strings.Join(rows, "\n")
	return title + "\n\n" + body + "\n" + footer
}
