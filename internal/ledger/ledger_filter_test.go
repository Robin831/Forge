package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilteredBeadsAnvilFilter(t *testing.T) {
	m := &Model{
		beads: []Bead{
			{ID: "a", Anvil: "forge", Status: "open"},
			{ID: "b", Anvil: "heimdall", Status: "open"},
			{ID: "c", Anvil: "forge", Status: "in_progress"},
		},
		showClosed: true,
	}

	// No filter: all beads returned.
	got := m.filteredBeads()
	assert.Len(t, got, 3)

	// Filter by "forge": only forge beads returned.
	m.selectedAnvil = "forge"
	got = m.filteredBeads()
	assert.Len(t, got, 2)
	for _, b := range got {
		assert.Equal(t, "forge", b.Anvil)
	}

	// Filter by "heimdall": only heimdall beads returned.
	m.selectedAnvil = "heimdall"
	got = m.filteredBeads()
	assert.Len(t, got, 1)
	assert.Equal(t, "b", got[0].ID)
}

func TestFilteredBeadsClosedHidden(t *testing.T) {
	m := &Model{
		beads: []Bead{
			{ID: "open1", Status: "open"},
			{ID: "closed1", Status: "closed"},
			{ID: "wip", Status: "in_progress"},
		},
		showClosed: false,
	}

	got := m.filteredBeads()
	assert.Len(t, got, 2)
	for _, b := range got {
		assert.NotEqual(t, "closed", b.Status)
	}
}

func TestFilteredBeadsClosedShown(t *testing.T) {
	m := &Model{
		beads: []Bead{
			{ID: "open1", Status: "open"},
			{ID: "closed1", Status: "closed"},
		},
		showClosed: true,
	}

	got := m.filteredBeads()
	assert.Len(t, got, 2)
}


func TestFilterHintNoFilters(t *testing.T) {
	m := &Model{showClosed: true}
	assert.Equal(t, "", m.filterHint())
}

func TestFilterHintAnvilOnly(t *testing.T) {
	m := &Model{
		selectedAnvil: "forge",
		showClosed:  true,
	}
	hint := m.filterHint()
	assert.Contains(t, hint, "[forge]")
}

func TestFilterHintClosedCount(t *testing.T) {
	m := &Model{
		showClosed: false,
		beads: []Bead{
			{ID: "a", Status: "open"},
			{ID: "b", Status: "closed"},
			{ID: "c", Status: "closed"},
		},
	}
	hint := m.filterHint()
	assert.Contains(t, hint, "+2 closed")
}

func TestFilterHintBothFilters(t *testing.T) {
	m := &Model{
		selectedAnvil: "forge",
		showClosed:  false,
		beads: []Bead{
			{ID: "a", Anvil: "forge", Status: "open"},
			{ID: "b", Anvil: "forge", Status: "closed"},
			{ID: "c", Anvil: "heimdall", Status: "closed"},
		},
	}
	hint := m.filterHint()
	assert.Contains(t, hint, "[forge]")
	// Only 1 closed bead in the "forge" anvil.
	assert.Contains(t, hint, "+1 closed")
}

func TestFilterHintClosedCountZero(t *testing.T) {
	m := &Model{
		showClosed: false,
		beads: []Bead{
			{ID: "a", Status: "open"},
		},
	}
	hint := m.filterHint()
	assert.Equal(t, "", hint, "no hint when there are no hidden closed beads")
}
