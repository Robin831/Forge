package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewModelInitialState(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil)

	assert.True(t, m.loading, "NewModel must start in loading state")
	assert.Equal(t, ViewList, m.view, "NewModel must default to list view")
	assert.True(t, m.showClosed, "NewModel must show closed beads by default")
	assert.NotNil(t, m.anvils, "anvils map must be initialised")
}

func TestNewModelInitReturnsCmd(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil)
	cmd := m.Init()
	require.NotNil(t, cmd, "Init() must return a non-nil command (batch of tick + fetch)")
}

func TestNewModelViewLoadingNonEmpty(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil)
	m.width = 100
	m.height = 30

	out := m.View()
	assert.NotEmpty(t, out, "View() on a new model must produce non-empty output")
	assert.Contains(t, out, "Loading", "View() in loading state must mention loading")
}

func TestNewModelViewNilAnvils(t *testing.T) {
	// NewModel with nil anvils must not panic and must still produce output.
	m := NewModel(nil, nil)
	m.width = 100
	m.height = 30

	assert.NotPanics(t, func() {
		out := m.View()
		assert.NotEmpty(t, out)
	})
}

func TestNewModelViewAfterBeadsLoaded(t *testing.T) {
	m := NewModel(map[string]string{"test": "/tmp"}, nil)
	m.width = 100
	m.height = 30

	// Simulate a successful bead fetch arriving.
	m.loading = false
	m.beads = []Bead{
		{ID: "test-1", Title: "Hello world", Status: "open", Anvil: "test"},
	}
	m.refreshHierarchy()

	out := m.View()
	assert.NotEmpty(t, out)
	// In list view (the default), the bead ID must be visible.
	assert.Contains(t, out, "test-1")
}
