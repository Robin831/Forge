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

// TestResolveEpicBranches_RegularDependencyChainNeverUsesFeatureBranch is a
// regression test for Forge-t6y9: when bead A blocks bead B (regular
// dependency, not parent-child), A's PR must target main, not feature/B.
// Even if B itself has its own blockers, it should not be treated as an epic
// parent unless it is explicitly typed as "epic" or has an epic-branch label.
func TestResolveEpicBranches_RegularDependencyChainNeverUsesFeatureBranch(t *testing.T) {
	orig := epicBranchLookupFunc
	defer func() { epicBranchLookupFunc = orig }()

	// Simulate the buggy scenario: "gnf7" happens to have its own blockers,
	// so before the fix lookupEpicBranch would return "feature/gnf7" for it.
	// After the fix it must return "" because gnf7 is not an epic.
	epicBranchLookupFunc = mockLookup(map[string]string{
		// "gnf7" is a regular task that has children but is NOT an epic — no branch
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
