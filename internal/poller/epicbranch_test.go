package poller

import (
	"context"
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
	// Non-epic, non-feature beads without an epic-branch label should not assume a default branch.
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

func TestExtractEpicBranch_FeatureDefault(t *testing.T) {
	b := Bead{
		ID:        "feat-42",
		IssueType: "feature",
	}
	// ExtractEpicBranch preserves legacy behavior: non-epic beads without
	// an explicit label return empty. Use ExtractParentBranch for defaults.
	assert.Equal(t, "", ExtractEpicBranch(b))
}

func TestExtractParentBranch_FeatureDefault(t *testing.T) {
	b := Bead{
		ID:        "feat-42",
		IssueType: "feature",
	}
	// ExtractParentBranch returns a default feature/ branch for non-epics.
	assert.Equal(t, "feature/feat-42", ExtractParentBranch(b))
}

func TestExtractEpicBranch_CaseInsensitiveLabel(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
		Labels:    []string{"Epic-Branch:feature/my-epic"},
	}
	assert.Equal(t, "feature/my-epic", ExtractEpicBranch(b))
}

// mockLookup returns a lookup function that resolves parentID to branch via the
// provided map. IDs not in the map return "".
func mockLookup(epicMap map[string]string) func(ctx context.Context, parentID, anvilPath string) string {
	return func(_ context.Context, parentID, _ string) string {
		return epicMap[parentID]
	}
}

func TestResolveEpicBranches_BlocksBased(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	epicBranchLookupFunc = mockLookup(map[string]string{
		"epic-1": "feature/my-epic",
	})

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Blocks: []string{"epic-1"}},
	}
	paths := map[string]string{"repo": "/tmp/repo"}

	ResolveEpicBranches(context.Background(), beads, paths)

	assert.Equal(t, "feature/my-epic", beads[0].EpicBranch)
}

func TestResolveEpicBranches_BlocksNotEpic(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	epicBranchLookupFunc = mockLookup(map[string]string{
		// "other-task" is not an epic, so lookup returns ""
	})

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Blocks: []string{"other-task"}},
	}
	paths := map[string]string{"repo": "/tmp/repo"}

	ResolveEpicBranches(context.Background(), beads, paths)

	assert.Equal(t, "", beads[0].EpicBranch)
}

// TestIsEpicParentBead is a regression test for Forge-t6y9: the guard logic
// inside lookupEpicBranch must reject regular tasks/features that merely have
// dependents, returning false so their PR targets main instead of a feature
// branch. Before the fix this check did not exist and any bead with blockers
// could mistakenly be treated as an epic parent.
func TestIsEpicParentBead(t *testing.T) {
	tests := []struct {
		name      string
		issueType string
		labels    []string
		want      bool
	}{
		// Forge-t6y9 regression: "gnf7" is a regular task with its own blockers —
		// it must NOT qualify as an epic parent even though it has dependents.
		{"task with blockers (Forge-t6y9)", "task", nil, false},
		{"task with unrelated labels", "task", []string{"priority:high"}, false},
		{"feature without label", "feature", nil, false},
		// Legitimate epic parents.
		{"epic type", "epic", nil, true},
		{"epic type uppercase", "EPIC", nil, true},
		{"explicit epic-branch label on task", "task", []string{"epic-branch:feature/foo"}, true},
		{"explicit epic-branch label on feature", "feature", []string{"epic-branch:feature/bar"}, true},
		{"case-insensitive label", "task", []string{"Epic-Branch:feature/baz"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isEpicParentBead(tt.issueType, tt.labels))
		})
	}
}

// TestResolveEpicBranches_RegularDependencyChainNeverUsesFeatureBranch verifies
// the ResolveEpicBranches routing layer: when lookupEpicBranch (correctly)
// returns "" for a non-epic bead, no EpicBranch is set on the child.
func TestResolveEpicBranches_RegularDependencyChainNeverUsesFeatureBranch(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	epicBranchLookupFunc = mockLookup(map[string]string{
		// "gnf7" is a regular task that has children but is NOT an epic — ignore its feature branch
		"gnf7": "",
	})

	beads := []Bead{
		{ID: "w3j2", Anvil: "repo", Blocks: []string{"gnf7"}},
	}
	paths := map[string]string{"repo": "/tmp/repo"}

	ResolveEpicBranches(context.Background(), beads, paths)

	assert.Equal(t, "", beads[0].EpicBranch, "regular blocked-by dependency must not route PR to a feature branch")
}

func TestResolveEpicBranches_BlocksCached(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	callCount := 0
	epicBranchLookupFunc = func(_ context.Context, parentID, _ string) string {
		callCount++
		if parentID == "epic-1" {
			return "epic/epic-1"
		}
		return ""
	}

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Blocks: []string{"epic-1"}},
		{ID: "task-2", Anvil: "repo", Blocks: []string{"epic-1"}},
	}
	paths := map[string]string{"repo": "/tmp/repo"}

	ResolveEpicBranches(context.Background(), beads, paths)

	assert.Equal(t, "epic/epic-1", beads[0].EpicBranch)
	assert.Equal(t, "epic/epic-1", beads[1].EpicBranch)
	assert.Equal(t, 1, callCount, "lookup should be called once due to caching")
}

func TestResolveEpicBranches_ParentTakesPrecedence(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	epicBranchLookupFunc = mockLookup(map[string]string{
		"epic-parent": "feature/parent-epic",
		"epic-blocks": "feature/blocks-epic",
	})

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Parent: "epic-parent", Blocks: []string{"epic-blocks"}},
	}
	paths := map[string]string{"repo": "/tmp/repo"}

	ResolveEpicBranches(context.Background(), beads, paths)

	assert.Equal(t, "feature/parent-epic", beads[0].EpicBranch, "parent path should take precedence over blocks")
}

func TestResolveEpicBranches_MultipleBlocksFirstEpicWins(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	epicBranchLookupFunc = mockLookup(map[string]string{
		"not-epic": "",
		"epic-2":   "feature/second",
	})

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Blocks: []string{"not-epic", "epic-2"}},
	}
	paths := map[string]string{"repo": "/tmp/repo"}

	ResolveEpicBranches(context.Background(), beads, paths)

	assert.Equal(t, "feature/second", beads[0].EpicBranch)
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
