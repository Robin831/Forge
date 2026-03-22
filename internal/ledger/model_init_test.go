package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewModelInitialState(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil, nil)

	assert.False(t, m.loading, "NewModel must not be in loading state (anvil view needs no fetch)")
	assert.Equal(t, ViewAnvils, m.view, "NewModel must default to anvil picker view")
	assert.False(t, m.showClosed, "NewModel must hide closed beads by default")
	assert.NotNil(t, m.anvils, "anvils map must be initialised")
}

func TestNewModelInitReturnsCmd(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil, nil)
	cmd := m.Init()
	require.NotNil(t, cmd, "Init() must return a non-nil command (tick)")
}

func TestNewModelViewAnvilPicker(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil, nil)
	m.width = 100
	m.height = 30

	out := m.View()
	assert.NotEmpty(t, out, "View() on a new model must produce non-empty output")
	assert.Contains(t, out, "Anvil", "View() in anvil-picker state must show anvil picker")
	assert.Contains(t, out, "test", "View() must list the registered anvil")
}

func TestNewModelViewNilAnvils(t *testing.T) {
	// NewModel with nil anvils must not panic and must still produce output.
	m := NewModel(nil, nil, nil)
	m.width = 100
	m.height = 30

	assert.NotPanics(t, func() {
		out := m.View()
		assert.NotEmpty(t, out)
	})
}

func TestNewModelViewAfterAnvilSelected(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil, nil)
	m.width = 100
	m.height = 30

	// Simulate entering an anvil and receiving beads.
	m.selectedAnvil = "test"
	m.view = ViewList
	m.loading = false
	m.beads = []Bead{
		{ID: "test-1", Title: "Hello world", Status: "open", Anvil: "test"},
	}
	m.refreshHierarchy()

	out := m.View()
	assert.NotEmpty(t, out)
	// In list view, the bead ID must be visible.
	assert.Contains(t, out, "test-1")
}
