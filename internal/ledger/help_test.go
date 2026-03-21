package ledger

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// newTestModel returns a minimal Model suitable for Update-level tests.
func newTestModel() *Model {
	m := &Model{
		anvils:  map[string]string{"test": "/tmp/test"},
		view:    ViewList,
		width:   80,
		height:  24,
		helpSt:  newHelpState(),
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

	// `q` should NOT close the overlay (it is quit in normal mode; help uses ?/esc only).
	m = sendKey(m, "q")
	if !m.helpSt.show {
		t.Error("`q` should not close the help overlay; only `?` and `esc` should")
	}
}
