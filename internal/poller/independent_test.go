package poller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
)

// The per-child opt-out has to survive the poll it is discovered in: the label
// is durable, ForceIndependent is not (json:"-"), so pollAnvil re-derives the
// flag on every pass. Driven through a real `bd ready` subprocess rather than a
// re-implementation of the loop, since the derivation being in the *eligible*
// path — after the assignee and clarification filters — is part of what is
// being asserted.
func TestPollAnvil_DerivesForceIndependentFromTheLabel(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[
  {"id":"child-1","status":"open","parent":"parent-1","labels":["independent"]},
  {"id":"child-2","status":"open","parent":"parent-1","labels":["forgeReady"]}
]
JSON`)

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	beads, err := p.PollSingle(context.Background(), "repo")
	require.NoError(t, err)
	require.Len(t, beads, 2)

	byID := map[string]Bead{}
	for _, b := range beads {
		byID[b.ID] = b
	}
	assert.True(t, byID["child-1"].ForceIndependent, "the labeled child dispatches standalone")
	assert.False(t, byID["child-2"].ForceIndependent, "its sibling is orchestrated as usual")
}

// The label is matched the way every other epic label is: trimmed and
// case-insensitively, so a padded or capitalised value cannot opt out through
// one code path and not another.
func TestPollAnvil_ForceIndependentLabelIsNormalised(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[{"id":"child-1","status":"open","labels":[" Independent "]}]
JSON`)

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	beads, err := p.PollSingle(context.Background(), "repo")
	require.NoError(t, err)
	require.Len(t, beads, 1)

	assert.True(t, beads[0].ForceIndependent)
}

// Blocks is the "children to orchestrate" signal every epic gate reads, so an
// opted-out child must not appear in it — otherwise a parent whose only ready
// child is independent starts a Crucible with nothing to put on its branch.
func TestPollAnvil_IndependentChildIsNotInTheParentsBlocks(t *testing.T) {
	withFakeBd(t, `cat <<'JSON'
[
  {"id":"parent-1","status":"open","labels":["crucible"]},
  {"id":"child-1","status":"open","parent":"parent-1","labels":["independent"]},
  {"id":"child-2","status":"open","dependencies":[{"depends_on_id":"parent-1","type":"blocks"}]}
]
JSON`)

	p := New(map[string]config.AnvilConfig{"repo": {Path: t.TempDir()}})
	beads, err := p.PollSingle(context.Background(), "repo")
	require.NoError(t, err)

	var parent Bead
	for _, b := range beads {
		if b.ID == "parent-1" {
			parent = b
		}
	}
	sort.Strings(parent.Blocks)
	assert.Equal(t, []string{"child-2"}, parent.Blocks,
		"an independent child is not counted among the children the epic orchestrates")
}

// The regression the json:"-" tag invites: a bead that has been through the
// queue cache (or any other JSON round-trip) arrives with the flag cleared, so
// nothing may depend on the flag alone. IsIndependentBead reads the label too,
// which is what every epic gate calls.
func TestIsIndependentBead_SurvivesAJSONRoundTrip(t *testing.T) {
	original := Bead{ID: "child-1", Labels: []string{"independent"}, ForceIndependent: true}

	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	var restored Bead
	require.NoError(t, json.Unmarshal(encoded, &restored))

	assert.False(t, restored.ForceIndependent, "the flag is json:\"-\" — it does not survive")
	assert.True(t, IsIndependentBead(restored), "but the label does, and that is what is read")
}

// The two forms reach a bead by different routes and neither implies the other:
// a force-run bead carries no label, a labeled bead restored from cache carries
// no flag.
func TestIsIndependentBead(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want bool
	}{
		{"neither", Bead{ID: "b"}, false},
		{"label only", Bead{ID: "b", Labels: []string{"independent"}}, true},
		{"flag only (manual run independently)", Bead{ID: "b", ForceIndependent: true}, true},
		{"both", Bead{ID: "b", Labels: []string{"INDEPENDENT"}, ForceIndependent: true}, true},
		{"unrelated labels", Bead{ID: "b", Labels: []string{"forgeReady", "crucible"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsIndependentBead(tt.bead))
		})
	}
}

// The stamp is what routes a bead onto the epic branch, so the whole of the
// opt-out on this side is refusing to stamp it: worktree from main, PR to main.
func TestResolveEpicBranches_IndependentChildIsNotStamped(t *testing.T) {
	restore := SetEpicBranchLookupForTest(func(_ context.Context, parentID, _ string) string {
		if parentID == "parent-1" {
			return "feature/parent-1"
		}
		return ""
	})
	defer restore()

	beads := []Bead{
		{ID: "child-1", Anvil: "repo", Parent: "parent-1", Labels: []string{"independent"}},
		{ID: "child-2", Anvil: "repo", Parent: "parent-1"},
	}
	ResolveEpicBranches(context.Background(), beads, map[string]string{"repo": "/tmp/repo"})

	assert.Empty(t, beads[0].EpicBranch, "the opted-out child PRs to main")
	assert.Empty(t, beads[0].EpicParent)
	assert.Equal(t, "feature/parent-1", beads[1].EpicBranch, "its sibling is routed as usual")
}

// An open independent child says nothing about whether the epic still has work
// for its feature branch, so it must not hold the parent: OpenChildren is what
// decides between dispatching an opted-in parent and escalating it.
func TestParseOpenChildren_IndependentChildDoesNotCount(t *testing.T) {
	output := `{"dependents":[
		{"id":"child-1","dependency_type":"blocks","status":"open","labels":["independent"]},
		{"id":"child-2","dependency_type":"blocks","status":"open","labels":["forgeReady"]}]}`

	open, err := parseOpenChildren([]byte(output))

	require.NoError(t, err)
	assert.Equal(t, []string{"child-2"}, open)
}

// The whole family opted out: the epic has nothing left to orchestrate, which
// is the answer that lets the parent run the ordinary pipeline to main.
func TestParseOpenChildren_AllIndependentIsEmpty(t *testing.T) {
	output := `{"dependents":[
		{"id":"child-1","dependency_type":"blocks","status":"open","labels":["independent"]},
		{"id":"child-2","dependency_type":"parent-child","status":"open","labels":["Independent"]}]}`

	open, err := parseOpenChildren([]byte(output))

	require.NoError(t, err)
	assert.Empty(t, open)
}

// A dependent bd reports without any labels is read as an ordinary child. That
// is the conservative direction: it holds the parent for an operator instead of
// closing an epic whose children are still open.
func TestParseOpenChildren_MissingLabelsAreAnOrdinaryChild(t *testing.T) {
	open, err := parseOpenChildren([]byte(`{"dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"}]}`))

	require.NoError(t, err)
	assert.Equal(t, []string{"child-1"}, open)
}
