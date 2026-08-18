package daemon

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/poller"
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
