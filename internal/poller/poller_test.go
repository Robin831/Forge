package poller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBead_JSONParsing(t *testing.T) {
	raw := `[
		{"id":"forge-1","title":"Fix bug","priority":1,"status":"open","issue_type":"bug","labels":["urgent"]},
		{"id":"forge-2","title":"Add feature","priority":2,"status":"open","issue_type":"feature","assignee":"alice"}
	]`

	var beads []Bead
	require.NoError(t, json.Unmarshal([]byte(raw), &beads))

	assert.Len(t, beads, 2)
	assert.Equal(t, "forge-1", beads[0].ID)
	assert.Equal(t, "Fix bug", beads[0].Title)
	assert.Equal(t, 1, beads[0].Priority)
	assert.Equal(t, []string{"urgent"}, beads[0].Labels)
	assert.Equal(t, "alice", beads[1].Assignee)
}

func TestBead_UnmarshalJSON_Tags(t *testing.T) {
	jsonData := `[
		{
			"id": "BD-1",
			"title": "Test Bead",
			"status": "ready",
			"priority": 1,
			"labels": ["forge-auto", "bug"]
		},
		{
			"id": "BD-2",
			"title": "Another Bead",
			"priority": 2
		}
	]`

	var beads []Bead
	require.NoError(t, json.Unmarshal([]byte(jsonData), &beads))
	assert.Len(t, beads, 2)

	assert.Equal(t, "BD-1", beads[0].ID)
	assert.Equal(t, []string{"forge-auto", "bug"}, beads[0].Labels)

	assert.Equal(t, "BD-2", beads[1].ID)
	assert.Nil(t, beads[1].Labels)
}

func TestBead_AnvilFieldNotInJSON(t *testing.T) {
	// Anvil is injected at runtime and should not appear in JSON output
	b := Bead{ID: "x", Anvil: "myrepo"}
	data, err := json.Marshal(b)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "myrepo")
}

func TestSortBeadsByPriority(t *testing.T) {
	// Replicate the sort.Slice logic used in Poll to verify ordering
	beads := []Bead{
		{ID: "z-high", Priority: 3},
		{ID: "a-critical", Priority: 0},
		{ID: "b-high", Priority: 3},
		{ID: "m-medium", Priority: 2},
		{ID: "x-low", Priority: 4},
		{ID: "c-urgent", Priority: 1},
	}

	sort.Slice(beads, func(i, j int) bool {
		if beads[i].Priority != beads[j].Priority {
			return beads[i].Priority < beads[j].Priority
		}
		return beads[i].ID < beads[j].ID
	})

	assert.Equal(t, "a-critical", beads[0].ID) // priority 0
	assert.Equal(t, "c-urgent", beads[1].ID)   // priority 1
	assert.Equal(t, "m-medium", beads[2].ID)   // priority 2
	assert.Equal(t, "b-high", beads[3].ID)     // priority 3, ID "b" < "z"
	assert.Equal(t, "z-high", beads[4].ID)     // priority 3, ID "z"
	assert.Equal(t, "x-low", beads[5].ID)      // priority 4
}

func TestSortBeadsByPriority_StableByID(t *testing.T) {
	// When priorities are equal, sort is stable by ID (ascending)
	beads := []Bead{
		{ID: "forge-z", Priority: 2},
		{ID: "forge-a", Priority: 2},
		{ID: "forge-m", Priority: 2},
	}

	sort.Slice(beads, func(i, j int) bool {
		if beads[i].Priority != beads[j].Priority {
			return beads[i].Priority < beads[j].Priority
		}
		return beads[i].ID < beads[j].ID
	})

	assert.Equal(t, "forge-a", beads[0].ID)
	assert.Equal(t, "forge-m", beads[1].ID)
	assert.Equal(t, "forge-z", beads[2].ID)
}

func TestNew(t *testing.T) {
	p := New(nil)
	assert.NotNil(t, p)

	p2 := New(map[string]config.AnvilConfig{})
	assert.NotNil(t, p2)
}

func TestPollSingle_UnknownAnvil(t *testing.T) {
	p := New(nil)
	_, err := p.PollSingle(context.Background(), "nonexistent")
	assert.ErrorContains(t, err, "not found")
}

func TestHasClarificationTag(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want bool
	}{
		{"nil tags", nil, false},
		{"empty tags", []string{}, false},
		{"unrelated tags", []string{"urgent", "bug"}, false},
		{"exact hyphen", []string{"clarification-needed"}, true},
		{"exact underscore", []string{"clarification_needed"}, true},
		{"case insensitive", []string{"Clarification-Needed"}, true},
		{"upper case", []string{"CLARIFICATION_NEEDED"}, true},
		{"among other tags", []string{"bug", "clarification-needed", "p1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasClarificationTag(tt.tags))
		})
	}
}

// TestBlocksReconstruction_OnlyBlocksType verifies that the Blocks reconstruction
// in pollAnvil only considers "blocks" and "parent-child" dependency types,
// not "depends_on". A depends_on relationship is a sequencing constraint, not a
// parent-child edge. Treating it as Blocks would cause the crucible to adopt
// downstream beads as children.
func TestBlocksReconstruction_OnlyBlocksType(t *testing.T) {
	beads := []Bead{
		{
			ID: "parent-1",
			Dependencies: []BeadDep{
				{DependsOnID: "child-a", Type: "blocks"},
			},
		},
		{
			ID:     "child-a",
			Parent: "parent-1",
		},
		{
			ID: "child-b",
			Dependencies: []BeadDep{
				{DependsOnID: "parent-1", Type: "blocks"},
			},
		},
		{
			ID: "downstream",
			Dependencies: []BeadDep{
				{DependsOnID: "parent-1", Type: "depends_on"},
			},
		},
	}

	// Simulate the Blocks reconstruction logic from pollAnvil.
	beadIdx := make(map[string]int, len(beads))
	for i := range beads {
		beadIdx[beads[i].ID] = i
	}
	blocksSet := make(map[string]map[string]bool)
	addBlock := func(parentID, childID string) {
		if _, ok := beadIdx[parentID]; !ok {
			return
		}
		if blocksSet[parentID] == nil {
			blocksSet[parentID] = make(map[string]bool)
		}
		blocksSet[parentID][childID] = true
	}
	for _, b := range beads {
		if b.Parent != "" {
			addBlock(b.Parent, b.ID)
		}
		for _, dep := range b.Dependencies {
			if dep.DependsOnID != "" && dep.DependsOnID != b.ID &&
				(dep.Type == "blocks" || dep.Type == "parent-child") {
				addBlock(dep.DependsOnID, b.ID)
			}
		}
	}
	for parentID, children := range blocksSet {
		idx := beadIdx[parentID]
		for childID := range children {
			beads[idx].Blocks = append(beads[idx].Blocks, childID)
		}
	}

	// parent-1 should have child-a and child-b as Blocks, but NOT downstream
	sort.Strings(beads[0].Blocks)
	assert.Equal(t, []string{"child-a", "child-b"}, beads[0].Blocks,
		"parent-1 Blocks should include blocks/parent-child deps only, not depends_on")

	// downstream should not appear in any Blocks list
	assert.Empty(t, beads[3].Blocks, "downstream should have no Blocks")
}

// TestPoll_MultipleAnvils verifies that Poll collects results from all anvils
// concurrently. Anvil paths point to temp directories where 'bd ready' will
// fail, but all anvils must still be represented in the returned results.
// Running with -race will detect any data-race in the concurrent poll loop.
func TestPoll_MultipleAnvils(t *testing.T) {
	anvils := map[string]config.AnvilConfig{
		"anvil-a": {Path: t.TempDir()},
		"anvil-b": {Path: t.TempDir()},
		"anvil-c": {Path: t.TempDir()},
	}

	p := New(anvils)
	beads, results := p.Poll(context.Background())

	// All three anvils must be represented in results (errors are fine)
	assert.Len(t, results, len(anvils))

	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.Name] = true
		// Each failed anvil should carry a non-nil error
		assert.Error(t, r.Err, "expected error for anvil %s", r.Name)
	}
	assert.True(t, seen["anvil-a"], "anvil-a missing from results")
	assert.True(t, seen["anvil-b"], "anvil-b missing from results")
	assert.True(t, seen["anvil-c"], "anvil-c missing from results")

	// No beads expected since all polls failed
	assert.Empty(t, beads)
}
