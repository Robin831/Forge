package poller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractEpicBranch_ExplicitLabel(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
		Labels:    []string{"epic-branch:feature/depcheck"},
	}
	assert.Equal(t, "feature/depcheck", ExtractEpicBranch(b))
}

func TestExtractEpicBranch_DefaultConvention(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
		Labels:    []string{"some-other-label"},
	}
	assert.Equal(t, "epic/epic-1", ExtractEpicBranch(b))
}

func TestExtractEpicBranch_NoLabels(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
	}
	assert.Equal(t, "epic/epic-1", ExtractEpicBranch(b))
}

func TestExtractEpicBranch_NotEpic(t *testing.T) {
	b := Bead{
		ID:        "task-1",
		IssueType: "task",
		Labels:    []string{"epic-branch:feature/foo"},
	}
	// Even with the label, non-epic beads should return the explicit branch
	// when the label is present.
	assert.Equal(t, "feature/foo", ExtractEpicBranch(b))
}

func TestExtractEpicBranch_NotEpicNoLabel(t *testing.T) {
	b := Bead{
		ID:        "task-1",
		IssueType: "task",
	}
	assert.Equal(t, "", ExtractEpicBranch(b))
}

func TestIsEpicBead(t *testing.T) {
	tests := []struct {
		issueType string
		want      bool
	}{
		{"epic", true},
		{"Epic", true},
		{"EPIC", true},
		{"feature", false},
		{"task", false},
		{"bug", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.issueType, func(t *testing.T) {
			b := Bead{IssueType: tt.issueType}
			assert.Equal(t, tt.want, IsEpicBead(b))
		})
	}
}

func TestExtractEpicBranch_CaseInsensitiveLabel(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
		Labels:    []string{"Epic-Branch:feature/my-epic"},
	}
	assert.Equal(t, "feature/my-epic", ExtractEpicBranch(b))
}

func TestSanitizeBeadID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Forge-n1g", "Forge-n1g"},
		{"my bead", "my-bead"},
		{"bead:123", "bead-123"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeBeadID(tt.input))
		})
	}
}
