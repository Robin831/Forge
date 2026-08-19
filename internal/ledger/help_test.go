package ledger

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sendMouseWheel(m *Model, button tea.MouseButton) *Model {
	result, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: button})
	if next, ok := result.(*Model); ok {
		return next
	}
	return m
}

// newTestModel returns a minimal Model suitable for Update-level tests.
func newTestModel() *Model {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		view:   ViewList,
		width:  80,
		height: 24,
		helpSt: newHelpState(),
	}
	return m
}

func sendKey(m *Model, key string) *Model {
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	if next, ok := result.(*Model); ok {
		return next
	}
	return m
}

func sendSpecialKey(m *Model, kt tea.KeyType) *Model {
	result, _ := m.Update(tea.KeyMsg{Type: kt})
	if next, ok := result.(*Model); ok {
		return next
	}
	return m
}

// TestHelpOverlayToggle verifies that `?` opens the help overlay and `?`/`esc` closes it.
func TestHelpOverlayToggle(t *testing.T) {
	m := newTestModel()

	if m.helpSt.show {
		t.Fatal("help overlay should be hidden initially")
	}

	// Press `?` to open.
	m = sendKey(m, "?")
	if !m.helpSt.show {
		t.Error("help overlay should be visible after pressing `?`")
	}

	// Press `?` again to close.
	m = sendKey(m, "?")
	if m.helpSt.show {
		t.Error("help overlay should be hidden after pressing `?` a second time")
	}

	// Open again, then close with `esc`.
	m = sendKey(m, "?")
	if !m.helpSt.show {
		t.Error("help overlay should be visible after pressing `?`")
	}
	m = sendSpecialKey(m, tea.KeyEsc)
	if m.helpSt.show {
		t.Error("help overlay should be hidden after pressing `esc`")
	}
}

// TestHelpOverlayConsumesKeys verifies that navigation keys are consumed by
// the help overlay and do not leak through to the underlying list view.
func TestHelpOverlayConsumesKeys(t *testing.T) {
	m := newTestModel()
	m.beads = []Bead{
		{ID: "test-1", Title: "First", Status: "open", Anvil: "test"},
		{ID: "test-2", Title: "Second", Status: "open", Anvil: "test"},
	}
	m.refreshHierarchy()

	// Open help overlay.
	m = sendKey(m, "?")
	if !m.helpSt.show {
		t.Fatal("help overlay must be open")
	}

	// `j` navigation key should not close the overlay or move the underlying list.
	before := m.list.vp.Selected()
	m = sendKey(m, "j")
	if !m.helpSt.show {
		t.Error("`j` should not close the help overlay")
	}
	if m.list.vp.Selected() != before {
		t.Error("`j` should not move the underlying list while help is open")
	}
}

// TestMouseWheelScrollsList verifies that wheel down/up moves the list cursor.
func TestMouseWheelScrollsList(t *testing.T) {
	m := newTestModel()
	m.view = ViewList
	m.beads = []Bead{
		{ID: "a", Title: "A", Status: "open", Anvil: "test"},
		{ID: "b", Title: "B", Status: "open", Anvil: "test"},
		{ID: "c", Title: "C", Status: "open", Anvil: "test"},
	}

	initial := m.list.vp.Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelDown)
	if m.list.vp.Selected() <= initial {
		t.Error("wheel down should advance the list cursor")
	}

	after := m.list.vp.Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelUp)
	if m.list.vp.Selected() >= after {
		t.Error("wheel up should retreat the list cursor")
	}
}

// TestMouseWheelScrollsKanban verifies that wheel down/up moves the kanban lane cursor.
func TestMouseWheelScrollsKanban(t *testing.T) {
	m := newTestModel()
	m.view = ViewKanban
	m.beads = []Bead{
		{ID: "a", Title: "A", Status: "open", Anvil: "test"},
		{ID: "b", Title: "B", Status: "open", Anvil: "test"},
	}
	m.refreshKanbanLanes()

	lane := m.kanban.activeLane
	initial := m.kanban.laneVP[lane].Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelDown)
	if m.kanban.laneVP[lane].Selected() <= initial {
		t.Error("wheel down should advance the kanban lane cursor")
	}

	after := m.kanban.laneVP[lane].Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelUp)
	if m.kanban.laneVP[lane].Selected() >= after {
		t.Error("wheel up should retreat the kanban lane cursor")
	}
}

// TestMouseWheelScrollsHierarchy verifies that wheel down/up moves the hierarchy cursor.
func TestMouseWheelScrollsHierarchy(t *testing.T) {
	m := newTestModel()
	m.view = ViewHierarchy
	m.beads = []Bead{
		{ID: "a", Title: "A", Status: "open", Anvil: "test"},
		{ID: "b", Title: "B", Status: "open", Anvil: "test"},
		{ID: "c", Title: "C", Status: "open", Anvil: "test"},
	}
	m.refreshHierarchy()

	initial := m.hierarchy.vp.Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelDown)
	if m.hierarchy.vp.Selected() <= initial {
		t.Error("wheel down should advance the hierarchy cursor")
	}

	after := m.hierarchy.vp.Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelUp)
	if m.hierarchy.vp.Selected() >= after {
		t.Error("wheel up should retreat the hierarchy cursor")
	}
}

// TestMouseWheelHelpOverlayDoesNotMoveList verifies that when the help overlay
// is visible, wheel events scroll the help viewport but do NOT change the
// underlying list/kanban/hierarchy selection.
func TestMouseWheelHelpOverlayDoesNotMoveList(t *testing.T) {
	m := newTestModel()
	m.view = ViewList
	m.beads = []Bead{
		{ID: "a", Title: "A", Status: "open", Anvil: "test"},
		{ID: "b", Title: "B", Status: "open", Anvil: "test"},
		{ID: "c", Title: "C", Status: "open", Anvil: "test"},
	}
	m.refreshHierarchy()

	// Open the help overlay.
	m = sendKey(m, "?")
	if !m.helpSt.show {
		t.Fatal("help overlay must be open")
	}

	before := m.list.vp.Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelDown)
	if m.list.vp.Selected() != before {
		t.Error("wheel down with help overlay open should not move the list cursor")
	}

	m = sendMouseWheel(m, tea.MouseButtonWheelUp)
	if m.list.vp.Selected() != before {
		t.Error("wheel up with help overlay open should not move the list cursor")
	}
}

// TestMouseWheelIgnoredWhenOverlayActive verifies that wheel events are ignored
// while an overlay (AI, update, sort form) is active, preventing selection
// changes behind the overlay.
func TestMouseWheelIgnoredWhenOverlayActive(t *testing.T) {
	m := newTestModel()
	m.view = ViewList
	m.beads = []Bead{
		{ID: "a", Title: "A", Status: "open", Anvil: "test"},
		{ID: "b", Title: "B", Status: "open", Anvil: "test"},
	}

	// Simulate an active AI overlay.
	m.aiOverlay = aiOverlaySpinner

	before := m.list.vp.Selected()
	m = sendMouseWheel(m, tea.MouseButtonWheelDown)
	if m.list.vp.Selected() != before {
		t.Error("wheel down should be ignored when an overlay is active")
	}
}
