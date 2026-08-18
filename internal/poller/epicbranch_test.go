package poller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractParentBranch_ExplicitLabel(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
		Labels:    []string{"epic-branch:feature/depcheck"},
	}
	assert.Equal(t, "feature/depcheck", ExtractParentBranch(b))
}

// The derived name is "feature/<id>" for every parent, epic-typed or not —
// the same name the Crucible builds. The old "epic/<id>" derivation for epic
// types disagreed with it, so an independently dispatched child hard-failed
// with "base branch not found on origin".
func TestExtractParentBranch_DerivedNameMatchesCrucible(t *testing.T) {
	for _, issueType := range []string{"epic", "feature", "task", ""} {
		t.Run(issueType, func(t *testing.T) {
			b := Bead{ID: "epic-1", IssueType: issueType}
			assert.Equal(t, "feature/epic-1", ExtractParentBranch(b))
		})
	}
}

func TestExtractParentBranch_CaseInsensitiveLabel(t *testing.T) {
	b := Bead{
		ID:        "epic-1",
		IssueType: "epic",
		Labels:    []string{"Epic-Branch:feature/my-epic"},
	}
	assert.Equal(t, "feature/my-epic", ExtractParentBranch(b))
}

// TestIsOrchestratedParent covers the inverted default: orchestration is opt-in
// via the "crucible" label (or an explicit "epic-branch:<name>"), and
// issue_type: epic alone no longer opts in.
func TestIsOrchestratedParent(t *testing.T) {
	tests := []struct {
		name      string
		issueType string
		labels    []string
		want      bool
	}{
		// Not opted in — children dispatch independently to main.
		{"epic type alone no longer opts in", "epic", nil, false},
		{"epic type with unrelated labels", "epic", []string{"priority:high"}, false},
		{"task with blockers (Forge-t6y9)", "task", nil, false},
		{"feature without label", "feature", nil, false},
		{"label that merely contains crucible", "task", []string{"crucible-ish"}, false},
		// Opted in.
		{"crucible label", "task", []string{"crucible"}, true},
		{"crucible label uppercase", "epic", []string{"Crucible"}, true},
		{"crucible label among others", "feature", []string{"ui", "crucible"}, true},
		{"explicit epic-branch label on task", "task", []string{"epic-branch:feature/foo"}, true},
		{"explicit epic-branch label on feature", "feature", []string{"epic-branch:feature/bar"}, true},
		{"case-insensitive epic-branch label", "task", []string{"Epic-Branch:feature/baz"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Bead{ID: "b-1", IssueType: tt.issueType, Labels: tt.labels}
			assert.Equal(t, tt.want, IsOrchestratedParent(b))
		})
	}
}

// mockLookup returns a lookup function that resolves parentID to branch via the
// provided map. IDs not in the map return "".
func mockLookup(epicMap map[string]string) func(ctx context.Context, parentID, anvilPath string) string {
	return func(_ context.Context, parentID, _ string) string {
		return epicMap[parentID]
	}
}

func TestResolveEpicBranches_ParentField(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"epic-1": "feature/my-epic",
	}))()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Parent: "epic-1"},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "feature/my-epic", beads[0].EpicBranch)
}

// A "blocks"/"parent-child" dependency names the parent from the child's side —
// the same edge pollAnvil walks to rebuild a parent's children.
func TestResolveEpicBranches_DependencyEdge(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"epic-1": "feature/my-epic",
	}))()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Dependencies: []BeadDep{
			{IssueID: "task-1", DependsOnID: "epic-1", Type: "parent-child"},
		}},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "feature/my-epic", beads[0].EpicBranch)
}

// Regression for the inverted-meaning bug: pollAnvil overwrites Blocks with
// "my children", so ResolveEpicBranches must not read it as "beads I block".
// A parent whose child happens to be an orchestrated parent itself must not
// inherit the child's branch.
func TestResolveEpicBranches_IgnoresBlocksField(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"child-1": "feature/child-epic",
	}))()

	beads := []Bead{
		{ID: "parent-1", Anvil: "repo", Blocks: []string{"child-1"}},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "", beads[0].EpicBranch,
		"Blocks means 'my children' after pollAnvil — it must never resolve a parent branch")
}

// A parent that has not opted in resolves to "" — the child flows through the
// normal pipeline: worktree from main, PR to main.
func TestResolveEpicBranches_UnlabeledParentLeavesChildIndependent(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		// lookupEpicBranch returns "" for a parent without the opt-in label.
	}))()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Parent: "epic-1"},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "", beads[0].EpicBranch)
}

func TestResolveEpicBranches_Cached(t *testing.T) {
	callCount := 0
	defer SetEpicBranchLookupForTest(func(_ context.Context, parentID, _ string) string {
		callCount++
		if parentID == "epic-1" {
			return "feature/epic-1"
		}
		return ""
	})()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Parent: "epic-1"},
		{ID: "task-2", Anvil: "repo", Parent: "epic-1"},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "feature/epic-1", beads[0].EpicBranch)
	assert.Equal(t, "feature/epic-1", beads[1].EpicBranch)
	assert.Equal(t, 1, callCount, "lookup should be called once due to caching")
}

func TestResolveEpicBranches_ParentTakesPrecedence(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"epic-parent": "feature/parent-epic",
		"epic-dep":    "feature/dep-epic",
	}))()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Parent: "epic-parent", Dependencies: []BeadDep{
			{IssueID: "task-1", DependsOnID: "epic-dep", Type: "blocks"},
		}},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "feature/parent-epic", beads[0].EpicBranch,
		"the parent field should take precedence over a dependency edge")
}

func TestResolveEpicBranches_FirstOrchestratedDepWins(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"not-epic": "",
		"epic-2":   "feature/second",
	}))()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Dependencies: []BeadDep{
			{IssueID: "task-1", DependsOnID: "not-epic", Type: "blocks"},
			{IssueID: "task-1", DependsOnID: "epic-2", Type: "blocks"},
		}},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "feature/second", beads[0].EpicBranch)
}

// A plain "depends_on" sequencing dependency is not a parent edge, so sibling
// ordering never routes a bead onto a feature branch.
func TestResolveEpicBranches_DependsOnIsNotAParentEdge(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"sibling-a": "feature/sibling-a",
	}))()

	beads := []Bead{
		{ID: "sibling-b", Anvil: "repo", DependsOn: []string{"sibling-a"}, Dependencies: []BeadDep{
			{IssueID: "sibling-b", DependsOnID: "sibling-a", Type: "depends_on"},
		}},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "", beads[0].EpicBranch)
}

func TestParentCandidates(t *testing.T) {
	b := Bead{
		ID:     "child",
		Parent: "p1",
		Blocks: []string{"my-child"},
		Dependencies: []BeadDep{
			{DependsOnID: "p1", Type: "parent-child"}, // duplicate of Parent
			{DependsOnID: "p2", Type: "blocks"},
			{DependsOnID: "seq", Type: "depends_on"}, // not a parent edge
			{DependsOnID: "child", Type: "blocks"},   // self-reference
		},
	}
	assert.Equal(t, []string{"p1", "p2"}, ParentCandidates(b))
}

// The stamp records which candidate produced it. A sequencing `blocks` edge can
// precede the real parent in ParentCandidates, so re-deriving the answer later
// from edge order names the wrong bead — the daemon's Crucible guard reads
// EpicParent instead.
func TestResolveEpicBranches_RecordsTheResolvedParent(t *testing.T) {
	defer SetEpicBranchLookupForTest(mockLookup(map[string]string{
		"epic-1": "feature/checkout-rewrite",
	}))()

	beads := []Bead{
		{ID: "task-1", Anvil: "repo", Dependencies: []BeadDep{
			{DependsOnID: "upstream-task", Type: "blocks"},
			{DependsOnID: "epic-1", Type: "parent-child"},
		}},
		{ID: "task-2", Anvil: "repo", Parent: "not-an-epic"},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Equal(t, "feature/checkout-rewrite", beads[0].EpicBranch)
	assert.Equal(t, "epic-1", beads[0].EpicParent,
		"the recorded parent is the one whose labels resolved, not the first edge")
	assert.Empty(t, beads[1].EpicParent, "an unresolved child records no parent")
}
