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
			name: "valid json",
			data: `{"id":"Forge-abc1","title":"Test","status":"open"}`,
			want: "Forge-abc1",
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
