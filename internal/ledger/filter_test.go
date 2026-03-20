package ledger

import (
	"testing"
)

func TestFilterState_Matches_TextSearch(t *testing.T) {
	f := newFilterState()
	f.text = "auth"

	tests := []struct {
		name  string
		bead  Bead
		match bool
	}{
		{"match in ID", Bead{ID: "auth-123", Title: "Something"}, true},
		{"match in title", Bead{ID: "xyz", Title: "Fix auth middleware"}, true},
		{"match in description", Bead{ID: "xyz", Title: "Fix bug", Description: "The auth module fails"}, true},
		{"match in label", Bead{ID: "xyz", Title: "Fix bug", Labels: []string{"security", "auth"}}, true},
		{"case insensitive", Bead{ID: "xyz", Title: "AUTH service down"}, true},
		{"no match", Bead{ID: "xyz", Title: "Fix database", Description: "DB is slow"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := f.Matches(tt.bead); got != tt.match {
				t.Errorf("Matches() = %v, want %v", got, tt.match)
			}
		})
	}
}

func TestFilterState_Matches_AnvilFilter(t *testing.T) {
	f := newFilterState()
	f.anvil = "forge"

	if !f.Matches(Bead{ID: "a", Anvil: "Forge"}) {
		t.Error("expected case-insensitive anvil match")
	}
	if f.Matches(Bead{ID: "a", Anvil: "heimdall"}) {
		t.Error("expected no match for different anvil")
	}
}

func TestFilterState_Matches_StatusFilter(t *testing.T) {
	f := newFilterState()
	f.status = "open"

	if !f.Matches(Bead{ID: "a", Status: "open"}) {
		t.Error("expected match for open status")
	}
	if f.Matches(Bead{ID: "a", Status: "closed"}) {
		t.Error("expected no match for closed status")
	}
}

func TestFilterState_Matches_PriorityFilter(t *testing.T) {
	f := newFilterState()
	f.priority = 1

	if !f.Matches(Bead{ID: "a", Priority: 1}) {
		t.Error("expected match for priority 1")
	}
	if f.Matches(Bead{ID: "a", Priority: 2}) {
		t.Error("expected no match for priority 2")
	}
}

func TestFilterState_Matches_LabelFilter(t *testing.T) {
	f := newFilterState()
	f.label = "urgent"

	if !f.Matches(Bead{ID: "a", Labels: []string{"Urgent", "backend"}}) {
		t.Error("expected case-insensitive label match")
	}
	if f.Matches(Bead{ID: "a", Labels: []string{"backend"}}) {
		t.Error("expected no match without matching label")
	}
}

func TestFilterState_Matches_Combined(t *testing.T) {
	f := newFilterState()
	f.text = "fix"
	f.anvil = "forge"

	// Matches both criteria
	if !f.Matches(Bead{ID: "a", Title: "Fix bug", Anvil: "forge"}) {
		t.Error("expected match for combined criteria")
	}
	// Text matches but anvil doesn't
	if f.Matches(Bead{ID: "a", Title: "Fix bug", Anvil: "heimdall"}) {
		t.Error("expected no match when anvil differs")
	}
	// Anvil matches but text doesn't
	if f.Matches(Bead{ID: "a", Title: "Add feature", Anvil: "forge"}) {
		t.Error("expected no match when text doesn't match")
	}
}

func TestFilterState_FilterBeads(t *testing.T) {
	f := newFilterState()
	f.text = "auth"

	beads := []Bead{
		{ID: "1", Title: "Auth service"},
		{ID: "2", Title: "Database fix"},
		{ID: "3", Title: "Auth middleware"},
	}

	got := f.FilterBeads(beads)
	if len(got) != 2 {
		t.Fatalf("FilterBeads() returned %d beads, want 2", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "3" {
		t.Errorf("FilterBeads() returned wrong beads: %v", got)
	}
}

func TestFilterState_FilterBeads_NoFilter(t *testing.T) {
	f := newFilterState()
	beads := []Bead{{ID: "1"}, {ID: "2"}}
	got := f.FilterBeads(beads)
	if len(got) != 2 {
		t.Errorf("FilterBeads() with no filter returned %d beads, want 2", len(got))
	}
}

func TestFilterState_HasActiveFilter(t *testing.T) {
	f := newFilterState()
	if f.HasActiveFilter() {
		t.Error("new filter should not be active")
	}
	f.text = "search"
	if !f.HasActiveFilter() {
		t.Error("filter with text should be active")
	}
	f.text = ""
	f.anvil = "forge"
	if !f.HasActiveFilter() {
		t.Error("filter with anvil should be active")
	}
}

func TestFilterState_Summary(t *testing.T) {
	f := newFilterState()
	f.text = "auth"
	if got := f.Summary(); got != "auth" {
		t.Errorf("Summary() = %q, want %q", got, "auth")
	}

	f.anvil = "forge"
	if got := f.Summary(); got != "auth | Anvil: forge" {
		t.Errorf("Summary() = %q, want %q", got, "auth | Anvil: forge")
	}
}

func TestFilterState_Clear(t *testing.T) {
	f := newFilterState()
	f.text = "something"
	f.anvil = "forge"
	f.status = "open"
	f.priority = 1
	f.label = "urgent"
	f.active = true

	f.clear()

	if f.active || f.text != "" || f.anvil != "" || f.status != "" || f.priority != -1 || f.label != "" {
		t.Error("clear() did not reset all fields")
	}
}
