package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
)

func guardDaemon() *Daemon {
	return &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func crucibleCfg(enabled bool) *config.Config {
	cfg := &config.Config{}
	cfg.Settings.CrucibleEnabled = enabled
	return cfg
}

func ownedIDs(owned map[string]crucibleOwner, anvil string, beads []poller.Bead) []string {
	var out []string
	for _, b := range beads {
		if _, ok := owned[anvil+"\x00"+b.ID]; ok {
			out = append(out, b.ID)
		}
	}
	return out
}

// A parent that opted in and its children land in the same poll batch: only the
// parent may dispatch, the Crucible runs the children itself.
func TestCrucibleOwnedChildren_SameBatch(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "child-1", Anvil: "repo", Parent: "parent-1"},
		{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, Blocks: []string{"child-1", "child-2"}},
		{ID: "child-2", Anvil: "repo", Parent: "parent-1"},
		{ID: "loner", Anvil: "repo"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(true), beads)

	assert.Equal(t, []string{"child-1", "child-2"}, ownedIDs(owned, "repo", beads))
}

// The inverted default: an unlabeled parent's children are ordinary beads and
// must keep dispatching independently.
func TestCrucibleOwnedChildren_UnlabeledParentOwnsNothing(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "parent-1", Anvil: "repo", IssueType: "epic", Blocks: []string{"child-1"}},
		{ID: "child-1", Anvil: "repo", Parent: "parent-1"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(true), beads)

	assert.Empty(t, owned)
}

// The opt-in label with the Crucible switched off: nobody creates the epic
// branch the poller stamped the children with, so they are withheld (and their
// parent escalated) rather than hard-failing in worktree.Create every cycle.
func TestCrucibleOwnedChildren_CrucibleDisabledWithholdsChildren(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, Blocks: []string{"child-1"}},
		{ID: "child-1", Anvil: "repo", Parent: "parent-1", EpicBranch: "feature/parent-1"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(false), beads)

	assert.Equal(t, []string{"child-1"}, ownedIDs(owned, "repo", beads))
	assert.Equal(t, crucibleOwner{ParentID: "parent-1", Disabled: true}, owned["repo\x00child-1"])
	assert.NotContains(t, owned, "repo\x00parent-1", "the parent still dispatches, and escalates itself")
}

// The parent is not in the batch (blocked, or already dispatched): the child is
// still recognisable by the epic branch the poller stamped it with.
func TestCrucibleOwnedChildren_CrucibleDisabledParentAbsent(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "child-1", Anvil: "repo", Parent: "parent-1", EpicBranch: "feature/parent-1"},
		{ID: "loner", Anvil: "repo"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(false), beads)

	assert.Equal(t, []string{"child-1"}, ownedIDs(owned, "repo", beads))
	assert.Equal(t, crucibleOwner{ParentID: "parent-1", Disabled: true}, owned["repo\x00child-1"])
}

// An unstamped bead with no orchestrated parent keeps dispatching with the
// Crucible off — the disabled case must not withhold ordinary work.
func TestCrucibleOwnedChildren_CrucibleDisabledLeavesPlainBeads(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "parent-1", Anvil: "repo", IssueType: "epic", Blocks: []string{"child-1"}},
		{ID: "child-1", Anvil: "repo", Parent: "parent-1"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(false), beads)

	assert.Empty(t, owned)
}

// A parent already mid-Crucible is in_progress and therefore absent from the
// batch; its children are matched through their own parent references.
func TestCrucibleOwnedChildren_ActiveCrucible(t *testing.T) {
	d := guardDaemon()
	d.crucibleStatuses.Store("repo/parent-1", crucible.Status{Phase: "dispatching"})

	beads := []poller.Bead{
		{ID: "child-1", Anvil: "repo", Parent: "parent-1"},
		{ID: "child-2", Anvil: "repo", Dependencies: []poller.BeadDep{
			{DependsOnID: "parent-1", Type: "parent-child"},
		}},
		{ID: "other-anvil-child", Anvil: "other", Parent: "parent-1"},
		{ID: "loner", Anvil: "repo"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(false), beads)

	assert.Equal(t, []string{"child-1", "child-2"}, ownedIDs(owned, "repo", beads))
	assert.False(t, owned["repo\x00child-1"].Disabled,
		"a running Crucible dispatches its own children, disabled setting or not")
	assert.NotContains(t, owned, "other\x00other-anvil-child",
		"an active Crucible in one anvil must not withhold a same-named parent's child in another")
}

// A plain depends_on sequencing edge is not a parent edge — sibling ordering
// must keep working in independent mode.
func TestCrucibleOwnedChildren_DependsOnIsNotOwnership(t *testing.T) {
	d := guardDaemon()
	d.crucibleStatuses.Store("repo/parent-1", crucible.Status{Phase: "dispatching"})

	beads := []poller.Bead{
		{ID: "sibling", Anvil: "repo", DependsOn: []string{"parent-1"}, Dependencies: []poller.BeadDep{
			{DependsOnID: "parent-1", Type: "depends_on"},
		}},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(true), beads)

	assert.Empty(t, owned)
}

// The parent itself is never withheld — it is the bead that starts the Crucible.
func TestCrucibleOwnedChildren_ParentNeverOwned(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "parent-1", Anvil: "repo", Labels: []string{"epic-branch:foo"}, Blocks: []string{"child-1"}},
		{ID: "child-1", Anvil: "repo", Parent: "parent-1"},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(true), beads)

	assert.NotContains(t, owned, "repo\x00parent-1")
	assert.Contains(t, owned, "repo\x00child-1")
}

// The stamped child names an unrelated upstream task through a blocks-typed
// sequencing edge before its real parent: attributing the stamp to the first
// candidate would freeze that task out of dispatch as needs_human.
func TestCrucibleOwnedChildren_StampAttributedToTheOptedInParent(t *testing.T) {
	d := guardDaemon()
	beads := []poller.Bead{
		{ID: "child-1", Anvil: "repo", EpicBranch: "feature/parent-1", EpicParent: "parent-1",
			Dependencies: []poller.BeadDep{
				{DependsOnID: "upstream-task", Type: "blocks"},
				{DependsOnID: "parent-1", Type: "parent-child"},
			}},
	}

	owned := d.crucibleOwnedChildren(crucibleCfg(false), beads)

	assert.Equal(t, crucibleOwner{ParentID: "parent-1", Disabled: true}, owned["repo\x00child-1"])
}

// Without the poller's record, only a candidate whose derived branch *is* the
// stamp may be named. An unrelated upstream task cannot satisfy that by
// accident, and naming nobody beats naming the wrong bead.
func TestStampingParent(t *testing.T) {
	tests := []struct {
		name string
		bead poller.Bead
		want string
	}{
		{
			name: "poller's record wins over edge order",
			bead: poller.Bead{ID: "c", EpicBranch: "feature/p", EpicParent: "p",
				Dependencies: []poller.BeadDep{{DependsOnID: "upstream", Type: "blocks"}}},
			want: "p",
		},
		{
			name: "unrecorded stamp falls back to the candidate whose name derives it",
			bead: poller.Bead{ID: "c", EpicBranch: "feature/p",
				Dependencies: []poller.BeadDep{
					{DependsOnID: "upstream", Type: "blocks"},
					{DependsOnID: "p", Type: "blocks"},
				}},
			want: "p",
		},
		{
			name: "no candidate derives the stamp: nobody is named",
			bead: poller.Bead{ID: "c", EpicBranch: "feature/checkout-rewrite",
				Dependencies: []poller.BeadDep{{DependsOnID: "upstream", Type: "blocks"}}},
			want: "",
		},
		{
			name: "no parent edges at all",
			bead: poller.Bead{ID: "c", EpicBranch: "feature/p"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stampingParent(tt.bead))
		})
	}
}

// needsHumanKeys returns the "beadID\x00anvil" keys currently flagged
// needs_human, in the form the dispatch loop's needsHumanSet uses.
func needsHumanKeys(t *testing.T, db *state.DB) map[string]string {
	t.Helper()
	recs, err := db.NeedsHumanBeads()
	require.NoError(t, err)
	out := make(map[string]string, len(recs))
	for _, r := range recs {
		out[r.BeadID+"\x00"+r.Anvil] = r.LastError
	}
	return out
}

func withholdDaemon(t *testing.T) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The escalation lands on the PARENT, not the withheld child: the parent is the
// bead carrying the label and the one an operator acts on. Flagging children
// would leave a needs_human row on every one of them to clear later.
func TestWithholdDisabledEpicChild_EscalatesTheParent(t *testing.T) {
	d := withholdDaemon(t)
	needsHuman := map[string]struct{}{}

	child := poller.Bead{ID: "child-1", Anvil: "repo", EpicBranch: "feature/parent-1"}
	d.withholdDisabledEpicChild(child, crucibleOwner{ParentID: "parent-1", Disabled: true}, needsHuman)

	flagged := needsHumanKeys(t, d.db)
	assert.Contains(t, flagged, "parent-1\x00repo")
	assert.NotContains(t, flagged, "child-1\x00repo", "the withheld child must not be flagged")
	assert.Contains(t, flagged["parent-1\x00repo"], "feature/parent-1",
		"the reason names the branch nothing creates")
	assert.Contains(t, needsHuman, "parent-1\x00repo",
		"the in-memory set is keyed beadID-first, matching the dispatch loop's needsHumanSet")
}

// Several children of the same parent escalate it exactly once per poll cycle.
func TestWithholdDisabledEpicChild_EscalatesOncePerParent(t *testing.T) {
	d := withholdDaemon(t)
	needsHuman := map[string]struct{}{}
	owner := crucibleOwner{ParentID: "parent-1", Disabled: true}

	d.withholdDisabledEpicChild(poller.Bead{ID: "child-1", Anvil: "repo", EpicBranch: "feature/parent-1"}, owner, needsHuman)
	first := needsHumanKeys(t, d.db)["parent-1\x00repo"]

	// A second child of the same parent, whose stamp would render a different
	// branch: if it re-marked, the reason would change.
	d.withholdDisabledEpicChild(poller.Bead{ID: "child-2", Anvil: "repo", EpicBranch: "feature/other"}, owner, needsHuman)

	assert.Len(t, needsHuman, 1)
	assert.Equal(t, first, needsHumanKeys(t, d.db)["parent-1\x00repo"],
		"the second child must not re-mark the parent")
}

// The same parent ID in two anvils is two escalations: the dedup key carries
// the anvil for the same reason the dispatch loop's does.
func TestWithholdDisabledEpicChild_PerAnvil(t *testing.T) {
	d := withholdDaemon(t)
	needsHuman := map[string]struct{}{}
	owner := crucibleOwner{ParentID: "parent-1", Disabled: true}

	d.withholdDisabledEpicChild(poller.Bead{ID: "child-1", Anvil: "repo-a", EpicBranch: "feature/parent-1"}, owner, needsHuman)
	d.withholdDisabledEpicChild(poller.Bead{ID: "child-1", Anvil: "repo-b", EpicBranch: "feature/parent-1"}, owner, needsHuman)

	flagged := needsHumanKeys(t, d.db)
	assert.Contains(t, flagged, "parent-1\x00repo-a")
	assert.Contains(t, flagged, "parent-1\x00repo-b")
}

// An owner whose parent could not be named logs and returns: nothing is marked,
// because there is no bead the escalation could truthfully name.
func TestWithholdDisabledEpicChild_UnknownParentMarksNothing(t *testing.T) {
	d := withholdDaemon(t)
	needsHuman := map[string]struct{}{}

	child := poller.Bead{ID: "child-1", Anvil: "repo", EpicBranch: "feature/checkout-rewrite"}
	d.withholdDisabledEpicChild(child, crucibleOwner{ParentID: "", Disabled: true}, needsHuman)

	assert.Empty(t, needsHumanKeys(t, d.db))
	assert.Empty(t, needsHuman)
}

// The child is withheld before its own EpicBranch is stamped (the parent named
// it through Blocks): the escalation still names the branch the parent derives.
func TestWithholdDisabledEpicChild_UnstampedChildNamesDerivedBranch(t *testing.T) {
	d := withholdDaemon(t)
	needsHuman := map[string]struct{}{}

	child := poller.Bead{ID: "child-1", Anvil: "repo"}
	d.withholdDisabledEpicChild(child, crucibleOwner{ParentID: "parent-1", Disabled: true}, needsHuman)

	assert.Contains(t, needsHumanKeys(t, d.db)["parent-1\x00repo"], "feature/parent-1")
}

func TestSummarizeIDs(t *testing.T) {
	assert.Equal(t, "", summarizeIDs(nil, 5))
	assert.Equal(t, "a, b", summarizeIDs([]string{"a", "b"}, 5))
	assert.Equal(t, "a, b, c", summarizeIDs([]string{"a", "b", "c"}, 3))
	assert.Equal(t, "a, b, c and 2 more", summarizeIDs([]string{"a", "b", "c", "d", "e"}, 3))
}
