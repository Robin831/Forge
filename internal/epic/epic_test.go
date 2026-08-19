package epic

import (
	"strings"
	"testing"
)

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

func TestValidBranchName(t *testing.T) {
	valid := []string{
		"feature/checkout-rewrite",
		"feature/Forge-abc1",
		"epic_1",
		"a",
		"release/2026.08",
	}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if !ValidBranchName(name) {
				t.Errorf("ValidBranchName(%q) = false, want true", name)
			}
		})
	}

	invalid := map[string]string{
		"empty":              "",
		"leading dash":       "--force",
		"single dash":        "-x",
		"dot dot":            "feature/../../x",
		"path traversal":     "../x",
		"leading slash":      "/feature/x",
		"trailing slash":     "feature/x/",
		"double slash":       "feature//x",
		"trailing dot":       "feature/x.",
		"lock suffix":        "feature/x.lock",
		"reflog syntax":      "feature/x@{1}",
		"space":              "feature/my branch",
		"tilde":              "feature/x~1",
		"caret":              "feature/x^",
		"colon":              "feature/x:y",
		"question mark":      "feature/x?",
		"asterisk":           "feature/x*",
		"open bracket":       "feature/x[y",
		"backslash":          `feature\x`,
		"control character":  "feature/x\ty",
		"component dot lead": "feature/.hidden",
	}
	for name, value := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if ValidBranchName(value) {
				t.Errorf("ValidBranchName(%q) = true, want false", value)
			}
		})
	}

	t.Run("over length", func(t *testing.T) {
		if ValidBranchName("feature/" + strings.Repeat("x", maxBranchNameLen)) {
			t.Error("an over-length name must be rejected")
		}
	})
}

// A label naming a branch git would reject (or read as a flag) must never reach
// git: the label still opts the parent in, but the branch falls back to the
// derived name rather than being handed on verbatim.
func TestBranchName_RejectsUnusableExplicitNames(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"flag-shaped", []string{"epic-branch:--force"}, "feature/Forge-abc"},
		{"path traversal", []string{"epic-branch:../../x"}, "feature/Forge-abc"},
		{"whitespace only", []string{"epic-branch:   "}, "feature/Forge-abc"},
		{"usable name still wins", []string{"epic-branch:feature/ok"}, "feature/ok"},
		{"first usable name wins", []string{"epic-branch:--force", "epic-branch:feature/ok"}, "feature/ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BranchName("Forge-abc", tt.labels); got != tt.want {
				t.Errorf("BranchName(Forge-abc, %v) = %q, want %q", tt.labels, got, tt.want)
			}
			if !IsOrchestrated(tt.labels) {
				t.Error("carrying the prefix is the opt-in, usable name or not")
			}
		})
	}
}

// Both opt-in forms are normalised the same way: leading whitespace must not
// make one of them silently invisible.
func TestIsOrchestrated_TrimsBothForms(t *testing.T) {
	for _, label := range []string{" crucible", "crucible ", " epic-branch:feature/x", "epic-branch:feature/x "} {
		t.Run(label, func(t *testing.T) {
			if !IsOrchestrated([]string{label}) {
				t.Errorf("IsOrchestrated([%q]) = false, want true", label)
			}
		})
	}
	if got := BranchName("Forge-abc", []string{" epic-branch:feature/x "}); got != "feature/x" {
		t.Errorf("BranchName with a padded label = %q, want feature/x", got)
	}
}

func TestIsIndependent(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"no labels", nil, false},
		{"unrelated labels", []string{"forgeReady", "ui"}, false},
		{"independent", []string{"independent"}, true},
		{"mixed case", []string{"Independent"}, true},
		{"padded", []string{" independent "}, true},
		{"among others", []string{"forgeReady", "independent"}, true},
		// The same near-miss rules the opt-in labels follow: containing the
		// word is not carrying the label.
		{"label merely containing independent", []string{"independent-ish"}, false},
		{"label prefixed", []string{"not-independent"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIndependent(tt.labels); got != tt.want {
				t.Errorf("IsIndependent(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

// One bead carrying both an opt-in and the opt-out is a contradiction, and it
// resolves toward "independent". The alternative leaves the family
// inconsistent: the bead's own dispatch path treats it as independent and runs
// it to main, while every child is still stamped with a feature branch nothing
// would then create.
func TestIsOrchestrated_IndependentWins(t *testing.T) {
	for _, labels := range [][]string{
		{"crucible", "independent"},
		{"independent", "crucible"},
		{"epic-branch:feature/x", "independent"},
	} {
		if IsOrchestrated(labels) {
			t.Errorf("IsOrchestrated(%v) = true, want false", labels)
		}
	}
}
