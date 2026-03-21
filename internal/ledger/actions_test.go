package ledger

import (
	"testing"
)

func TestExtractIDFromJSON(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "valid json object",
			data: `{"id":"Forge-abc1","title":"Test","status":"open"}`,
			want: "Forge-abc1",
		},
		{
			name: "json array wrapper",
			data: `[{"id":"Forge-xyz9","title":"Test","status":"open"}]`,
			want: "Forge-xyz9",
		},
		{
			name: "empty array",
			data: `[]`,
			want: "",
		},
		{
			name: "empty json",
			data: `{}`,
			want: "",
		},
		{
			name: "invalid json",
			data: `not json`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractIDFromJSON([]byte(tt.data))
			if got != tt.want {
				t.Errorf("extractIDFromJSON(%q) = %q, want %q", tt.data, got, tt.want)
			}
		})
	}
}

func TestSelectedBeadNil(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		view:   ViewList,
	}
	// No beads loaded — selectedBead should return nil.
	if b := m.selectedBead(); b != nil {
		t.Error("expected nil bead when list is empty")
	}
}

func TestReopenGuardsNonClosed(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		beads: []Bead{
			{ID: "test-1", Title: "Open bead", Status: "open", Anvil: "test"},
		},
		view: ViewList,
	}
	// reopenSelectedBead should return nil for a non-closed bead.
	cmd := m.reopenSelectedBead()
	if cmd != nil {
		t.Error("expected nil cmd for non-closed bead")
	}
}

func TestCloseGuardsClosed(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		beads: []Bead{
			{ID: "test-1", Title: "Closed bead", Status: "closed", Anvil: "test"},
		},
		view: ViewList,
	}
	// openCloseBeadForm should return nil for an already-closed bead.
	cmd := m.openCloseBeadForm()
	if cmd != nil {
		t.Error("expected nil cmd for already-closed bead")
	}
}

// TestOpenAddDepFormExcludesSelfAndExisting verifies that openAddDepForm
// excludes the selected bead itself and any beads already in its DependsOn list.
func TestOpenAddDepFormExcludesSelfAndExisting(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		beads: []Bead{
			{ID: "test-1", Title: "Selected", Status: "open", Anvil: "test", DependsOn: []string{"test-3"}},
			{ID: "test-2", Title: "Eligible", Status: "open", Anvil: "test"},
			{ID: "test-3", Title: "Already dep", Status: "open", Anvil: "test"},
		},
		view: ViewList,
	}
	cmd := m.openAddDepForm()
	if cmd == nil {
		t.Fatal("expected a non-nil cmd (form init)")
	}
	if m.activeFormKind != FormAddDep {
		t.Errorf("activeFormKind = %v, want FormAddDep", m.activeFormKind)
	}
	// Only test-2 should be a candidate; formDepID defaults to first candidate.
	if m.formDepID != "test-2" {
		t.Errorf("formDepID = %q, want %q (self and existing dep should be excluded)", m.formDepID, "test-2")
	}
	if m.formTarget == nil || m.formTarget.ID != "test-1" {
		t.Error("formTarget should be the selected bead")
	}
}

// TestOpenAddDepFormSameAnvilOnly verifies that openAddDepForm only offers beads
// from the same anvil as the selected bead, not those from other anvils.
func TestOpenAddDepFormSameAnvilOnly(t *testing.T) {
	m := &Model{
		anvils: map[string]string{
			"anvil-a": "/tmp/a",
			"anvil-b": "/tmp/b",
		},
		beads: []Bead{
			{ID: "a-1", Title: "Selected", Status: "open", Anvil: "anvil-a"},
			{ID: "a-2", Title: "Same anvil", Status: "open", Anvil: "anvil-a"},
			{ID: "b-1", Title: "Other anvil", Status: "open", Anvil: "anvil-b"},
		},
		view: ViewList,
	}
	cmd := m.openAddDepForm()
	if cmd == nil {
		t.Fatal("expected a non-nil cmd (form init)")
	}
	if m.activeFormKind != FormAddDep {
		t.Errorf("activeFormKind = %v, want FormAddDep", m.activeFormKind)
	}
	// Only a-2 (same anvil) should be a candidate; b-1 must be excluded.
	if m.formDepID != "a-2" {
		t.Errorf("formDepID = %q, want %q (cross-anvil bead should be excluded)", m.formDepID, "a-2")
	}
}

// TestOpenAddDepFormAllExcluded verifies that openAddDepForm returns an error
// command when all beads are already excluded (self or existing deps).
func TestOpenAddDepFormAllExcluded(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		beads: []Bead{
			{ID: "test-1", Title: "Selected", Status: "open", Anvil: "test", DependsOn: []string{"test-2"}},
			{ID: "test-2", Title: "Already dep", Status: "open", Anvil: "test"},
		},
		view: ViewList,
	}
	cmd := m.openAddDepForm()
	if cmd == nil {
		t.Fatal("expected a non-nil error cmd")
	}
	msg := cmd()
	if _, ok := msg.(ActionErrorMsg); !ok {
		t.Errorf("expected ActionErrorMsg, got %T", msg)
	}
}

// TestOpenDepViewerFormDefaultDoneSelection verifies that openDepViewerForm
// initialises formDepID to "" (the "done" no-op option) by default.
func TestOpenDepViewerFormDefaultDoneSelection(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		beads: []Bead{
			{
				ID: "test-1", Title: "Selected", Status: "open", Anvil: "test",
				DependsOn: []string{"test-2"},
				Blocks:    []string{"test-3"},
			},
			{ID: "test-2", Title: "Dep", Status: "open", Anvil: "test"},
			{ID: "test-3", Title: "Child", Status: "open", Anvil: "test"},
		},
		view: ViewList,
	}
	cmd := m.openDepViewerForm()
	if cmd == nil {
		t.Fatal("expected a non-nil cmd (form init)")
	}
	if m.activeFormKind != FormViewDeps {
		t.Errorf("activeFormKind = %v, want FormViewDeps", m.activeFormKind)
	}
	// Default selection should be "" (done / no removal).
	if m.formDepID != "" {
		t.Errorf("formDepID = %q, want empty string (default done selection)", m.formDepID)
	}
}

// TestOpenDepViewerFormNoDeps verifies that openDepViewerForm returns an error
// command when the selected bead has no dependencies or blocks.
func TestOpenDepViewerFormNoDeps(t *testing.T) {
	m := &Model{
		anvils: map[string]string{"test": "/tmp/test"},
		beads: []Bead{
			{ID: "test-1", Title: "Lonely bead", Status: "open", Anvil: "test"},
		},
		view: ViewList,
	}
	cmd := m.openDepViewerForm()
	if cmd == nil {
		t.Fatal("expected a non-nil error cmd")
	}
	msg := cmd()
	if _, ok := msg.(ActionErrorMsg); !ok {
		t.Errorf("expected ActionErrorMsg, got %T", msg)
	}
}
