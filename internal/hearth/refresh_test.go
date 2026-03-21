package hearth

import (
	"testing"
)

func TestFindBeadIndexByID(t *testing.T) {
	items := []QueueItem{
		{BeadID: "Forge-aaa"},
		{BeadID: "Forge-bbb"},
		{BeadID: "Forge-ccc"},
	}

	if got := findBeadIndexByID(items, "Forge-bbb"); got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
	if got := findBeadIndexByID(items, "Forge-aaa"); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
	if got := findBeadIndexByID(items, "Forge-ccc"); got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
	if got := findBeadIndexByID(items, "Forge-zzz"); got != -1 {
		t.Errorf("expected -1 for missing id, got %d", got)
	}
	if got := findBeadIndexByID(nil, "Forge-aaa"); got != -1 {
		t.Errorf("expected -1 for nil slice, got %d", got)
	}
}

func TestCaptureFocusedBeadID(t *testing.T) {
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "Forge-111", Anvil: "test"},
		{BeadID: "Forge-222", Anvil: "test"},
	}
	m.queueNavItems = []queueNavItem{
		{isAnvil: true, anvilName: "test", beadIdx: -1},
		{isAnvil: false, beadIdx: 0},
		{isAnvil: false, beadIdx: 1},
	}

	// Cursor on bead at nav index 1 → should capture Forge-111
	m.queueVP.cursor = 1
	m.captureFocusedBeadID()
	if m.focusedBeadID != "Forge-111" {
		t.Errorf("expected Forge-111, got %q", m.focusedBeadID)
	}

	// Cursor on bead at nav index 2 → should capture Forge-222
	m.queueVP.cursor = 2
	m.captureFocusedBeadID()
	if m.focusedBeadID != "Forge-222" {
		t.Errorf("expected Forge-222, got %q", m.focusedBeadID)
	}

	// Cursor on anvil header → focusedBeadID should not change
	m.queueVP.cursor = 0
	m.captureFocusedBeadID()
	if m.focusedBeadID != "Forge-222" {
		t.Errorf("expected focusedBeadID unchanged (Forge-222), got %q", m.focusedBeadID)
	}
}

func TestCaptureFocusedBeadIDEmptyNav(t *testing.T) {
	m := NewModel(nil)
	// No panic when navItems is empty
	m.captureFocusedBeadID()
	if m.focusedBeadID != "" {
		t.Errorf("expected empty focusedBeadID, got %q", m.focusedBeadID)
	}
}

func TestCaptureFocusedBeadIDCursorOutOfBounds(t *testing.T) {
	m := NewModel(nil)
	m.queue = []QueueItem{{BeadID: "Forge-abc", Anvil: "test"}}
	m.queueNavItems = []queueNavItem{{isAnvil: false, beadIdx: 0}}
	m.queueVP.cursor = 99 // out of bounds
	m.captureFocusedBeadID()
	if m.focusedBeadID != "" {
		t.Errorf("expected empty focusedBeadID for out-of-bounds cursor, got %q", m.focusedBeadID)
	}
}
