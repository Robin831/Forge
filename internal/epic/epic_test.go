package epic

import "testing"

func TestIsOrchestrated(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"no labels", nil, false},
		{"unrelated labels", []string{"ui", "priority:high"}, false},
		// The bead's issue type is not an input here at all — an epic-typed
		// parent with no label is not orchestrated.
		{"crucible label", []string{"crucible"}, true},
		{"crucible label mixed case", []string{"Crucible"}, true},
		{"crucible label padded", []string{" crucible "}, true},
		{"crucible among others", []string{"ui", "crucible", "p1"}, true},
		{"label merely containing crucible", []string{"crucible-ish"}, false},
		{"label prefixed", []string{"not-crucible"}, false},
		{"epic-branch label", []string{"epic-branch:feature/foo"}, true},
		{"epic-branch label mixed case", []string{"Epic-Branch:feature/foo"}, true},
		{"epic-branch label with no name still opts in", []string{"epic-branch:"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOrchestrated(tt.labels); got != tt.want {
				t.Errorf("IsOrchestrated(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestExplicitBranch(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"none", []string{"crucible"}, ""},
		{"named", []string{"epic-branch:feature/foo"}, "feature/foo"},
		{"padded name", []string{"epic-branch:  feature/foo  "}, "feature/foo"},
		{"mixed case prefix", []string{"Epic-Branch:foo"}, "foo"},
		{"empty name ignored", []string{"epic-branch:"}, ""},
		{"first non-empty wins", []string{"epic-branch:", "epic-branch:bar"}, "bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExplicitBranch(tt.labels); got != tt.want {
				t.Errorf("ExplicitBranch(%v) = %q, want %q", tt.labels, got, tt.want)
			}
		})
	}
}

func TestBranchName(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		labels []string
		want   string
	}{
		{"derived", "Forge-abc1", nil, "feature/Forge-abc1"},
		{"derived with crucible label", "Forge-abc1", []string{"crucible"}, "feature/Forge-abc1"},
		{"explicit label wins", "Forge-abc1", []string{"crucible", "epic-branch:foo"}, "foo"},
		{"id sanitised", "my epic:1", nil, "feature/my-epic-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BranchName(tt.id, tt.labels); got != tt.want {
				t.Errorf("BranchName(%q, %v) = %q, want %q", tt.id, tt.labels, got, tt.want)
			}
		})
	}
}

func TestSanitizeID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Forge-n1g", "Forge-n1g"},
		{"my bead", "my-bead"},
		{"bead:123", "bead-123"},
		{"a/b", "a-b"},
		{`a\b`, "a-b"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := SanitizeID(tt.in); got != tt.want {
				t.Errorf("SanitizeID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
