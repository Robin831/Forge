package daemon

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worker"
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

// escalationDaemon is withholdDaemon plus a config, which escalation needs:
// recordDispatchFailure releases the bead's claim. No anvils are configured, so
// that release logs and returns instead of shelling out to bd — the same
// hermetic seam emptyDiffDaemon uses.
func escalationDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := withholdDaemon(t)
	d.cfg.Store(&config.Config{})
	return d
}

func pendingWorker(t *testing.T, d *Daemon, workerID, beadID, anvil string) {
	t.Helper()
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    beadID,
		Anvil:     anvil,
		Branch:    "forge/" + beadID,
		Status:    state.WorkerPending,
		StartedAt: time.Now(),
	}))
}

// The invariant escalateOrchestratedParent's doc comment is built on: the
// pending worker row is closed out. It counts toward dispatch capacity until
// something does, so a leaked row costs a smith slot for the life of the daemon
// — one per escalated epic misconfiguration.
func TestEscalateOrchestratedParent_ClosesTheWorkerRow(t *testing.T) {
	d := escalationDaemon(t)
	pendingWorker(t, d, "w-1", "parent-1", "repo")

	before, err := worker.DispatchActiveCount(d.db, "repo")
	require.NoError(t, err)
	require.Equal(t, 1, before, "the pending claim row occupies a smith slot")

	d.escalateOrchestratedParent(poller.Bead{ID: "parent-1", Anvil: "repo"}, "w-1", "epic on hold: children not ready")

	w, err := d.db.GetWorker("w-1")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerFailed, w.Status)

	after, err := worker.DispatchActiveCount(d.db, "repo")
	require.NoError(t, err)
	assert.Equal(t, 0, after, "the slot must be released, not leaked")
}

// The other two writes: the bead is flagged for the operator with the given
// reason, and the attempt is recorded as a dispatch failure (escalate=true, so
// the claim is released too).
func TestEscalateOrchestratedParent_FlagsTheBead(t *testing.T) {
	d := escalationDaemon(t)
	pendingWorker(t, d, "w-1", "parent-1", "repo")

	d.escalateOrchestratedParent(poller.Bead{ID: "parent-1", Anvil: "repo"}, "w-1", "epic on hold: children not ready")

	assert.Equal(t, "epic on hold: children not ready", needsHumanKeys(t, d.db)["parent-1\x00repo"])

	r, err := d.db.GetRetry("parent-1", "repo")
	require.NoError(t, err)
	assert.True(t, r.NeedsHuman)
	assert.Equal(t, 1, r.DispatchFailures, "the escalation is recorded as a dispatch failure")
}

// A worker row that cannot be updated — no claim was taken, or the row is gone —
// must not cost the escalation: the flag is what the operator sees, and skipping
// it would leave the parent silently undispatched.
func TestEscalateOrchestratedParent_WorkerUpdateNeverBlocksTheFlag(t *testing.T) {
	for _, workerID := range []string{"", "w-missing"} {
		t.Run("worker="+workerID, func(t *testing.T) {
			d := escalationDaemon(t)

			d.escalateOrchestratedParent(poller.Bead{ID: "parent-1", Anvil: "repo"}, workerID, "epic on hold: children not ready")

			assert.Contains(t, needsHumanKeys(t, d.db), "parent-1\x00repo")
		})
	}
}

// An empty claim ID means no worker row was inserted; nothing must be invented
// for it.
func TestEscalateOrchestratedParent_NoClaimTouchesNoWorker(t *testing.T) {
	d := escalationDaemon(t)
	pendingWorker(t, d, "w-1", "other-bead", "repo")

	d.escalateOrchestratedParent(poller.Bead{ID: "parent-1", Anvil: "repo"}, "", "epic on hold: children not ready")

	w, err := d.db.GetWorker("w-1")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerPending, w.Status, "an unrelated worker row is untouched")
}

// The three-way choice dispatchBead makes for an opted-in parent that is not a
// Crucible candidate. Each outcome is a different failure if it fires wrongly:
// dispatching orphans the children, escalating on a bd blip freezes the epic,
// deferring forever hides a real misconfiguration.
func TestDecideOrchestratedParent(t *testing.T) {
	parent := poller.Bead{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}}

	t.Run("bd error defers, with no reason to record", func(t *testing.T) {
		action, reason := decideOrchestratedParent(parent, nil, errors.New("dolt is down"), true)
		assert.Equal(t, parentDefer, action)
		assert.Empty(t, reason)
	})

	t.Run("a bd error wins over children it may have returned", func(t *testing.T) {
		action, _ := decideOrchestratedParent(parent, []string{"child-1"}, errors.New("dolt is down"), true)
		assert.Equal(t, parentDefer, action, "a partial answer is not an answer")
	})

	t.Run("no open children runs the ordinary pipeline", func(t *testing.T) {
		action, reason := decideOrchestratedParent(parent, nil, nil, true)
		assert.Equal(t, parentRunNormally, action)
		assert.Empty(t, reason)
	})

	t.Run("open children hold the parent and name them", func(t *testing.T) {
		action, reason := decideOrchestratedParent(parent, []string{"child-1", "child-2"}, nil, true)
		assert.Equal(t, parentHold, action)
		assert.Contains(t, reason, "child-1")
		assert.Contains(t, reason, "feature/parent-1", "the reason names the branch the children are routed to")
		assert.True(t, strings.HasPrefix(reason, epicHoldPrefix), "the hold must be self-clearing")
	})

	t.Run("the Crucible being off owns the wording", func(t *testing.T) {
		action, reason := decideOrchestratedParent(parent, []string{"child-1"}, nil, false)
		assert.Equal(t, parentHold, action)
		assert.Equal(t, crucibleDisabledReason("parent-1", "feature/parent-1"), reason,
			"promising a Crucible run that cannot happen is the wrong remedy")
	})

	t.Run("no children, Crucible off: still an ordinary bead", func(t *testing.T) {
		action, _ := decideOrchestratedParent(parent, nil, nil, false)
		assert.Equal(t, parentRunNormally, action,
			"nothing is routed anywhere, so the setting does not matter")
	})

	t.Run("an explicit branch label is the branch named", func(t *testing.T) {
		labeled := poller.Bead{ID: "parent-2", Anvil: "repo", Labels: []string{"epic-branch:checkout-rewrite"}}
		_, reason := decideOrchestratedParent(labeled, []string{"child-1"}, nil, true)
		assert.Contains(t, reason, "checkout-rewrite")
	})
}

// The reversal this PR exists for: a schematic decline against an explicit
// opt-in escalates. It used to clear the parent's own EpicBranch and dispatch it
// standalone — which strips the routing from one bead of a family whose children
// the poller already stamped, merging the parent to main while they base on a
// branch no Crucible creates.
func TestSchematicDeclineReason(t *testing.T) {
	parent := poller.Bead{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, EpicBranch: "feature/parent-1"}

	t.Run("a confirmed check does not escalate", func(t *testing.T) {
		assert.Empty(t, schematicDeclineReason(parent, true, "these children share a schema migration"))
	})

	t.Run("a decline escalates and names both remedies", func(t *testing.T) {
		reason := schematicDeclineReason(parent, false, "the children touch unrelated packages")
		require.NotEmpty(t, reason)
		assert.Contains(t, reason, "the children touch unrelated packages")
		assert.Contains(t, reason, "feature/parent-1")
		assert.Contains(t, reason, "schematic_enabled")
		assert.False(t, strings.HasPrefix(reason, epicHoldPrefix),
			"a label contradicting its own check is not self-clearing: only an operator resolves it")
	})

	t.Run("the parent's routing is left alone", func(t *testing.T) {
		_ = schematicDeclineReason(parent, false, "independent")
		assert.Equal(t, "feature/parent-1", parent.EpicBranch,
			"clearing the parent's stamp while its children keep theirs is the bug")
	})

	t.Run("an empty check reason still reads as a sentence", func(t *testing.T) {
		assert.Contains(t, schematicDeclineReason(parent, false, "   "), "no reason given")
	})
}

// The check's Reason is free text from an AI session whose inputs include bead
// content Forge did not write, and it now lands in a persisted, terminal-rendered
// Needs Attention row. Escape sequences must not survive that trip.
func TestSchematicDeclineReason_SanitizesTheModelsText(t *testing.T) {
	parent := poller.Bead{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}}

	reason := schematicDeclineReason(parent, false, "independent\x1b[2J\x1b[1;31mATTENTION: all clear\nsecond line\r\n")

	assert.NotContains(t, reason, "\x1b")
	assert.NotContains(t, reason, "\n")
	assert.NotContains(t, reason, "\r")
	assert.Contains(t, reason, "independent")
	assert.Contains(t, reason, "second line", "stripping must not swallow the text itself")
}

// A model that answers with three paragraphs must not own the Needs Attention
// panel.
func TestSanitizeEscalationDetail_Bounded(t *testing.T) {
	detail := sanitizeEscalationDetail(strings.Repeat("verbose ", 200))

	assert.LessOrEqual(t, len([]rune(detail)), escalationDetailMax+1, "bounded, plus the ellipsis")
	assert.True(t, strings.HasSuffix(detail, "…"))
}

// The self-healing half of the hold: the conditions it is raised for (children
// not ready, the Crucible switched off) resolve on their own, and needs_human
// does not. Without this the epic deadlocks — the parent is skipped on the stale
// flag while its now-ready children are still withheld on its behalf.
func TestClearResolvedEpicHold(t *testing.T) {
	d := escalationDaemon(t)
	require.NoError(t, d.db.MarkNeedsHuman("parent-1", "repo", crucibleDisabledReason("parent-1", "feature/parent-1")))

	// The child is ready this cycle, so the parent is a Crucible candidate again.
	beads := []poller.Bead{
		{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, Blocks: []string{"child-1"}},
		{ID: "child-1", Anvil: "repo", Parent: "parent-1"},
	}
	needsHuman := map[string]struct{}{"parent-1\x00repo": {}}

	d.clearResolvedEpicHold(crucibleCfg(true), beads, needsHuman)

	assert.NotContains(t, needsHumanKeys(t, d.db), "parent-1\x00repo", "the hold is withdrawn")
	assert.NotContains(t, needsHuman, "parent-1\x00repo",
		"and dropped from this cycle's snapshot, so the same poll dispatches the epic")
}

// The conditions that have not resolved keep their hold.
func TestClearResolvedEpicHold_LeavesUnresolvedHolds(t *testing.T) {
	held := crucibleDisabledReason("parent-1", "feature/parent-1")

	t.Run("the Crucible is still off", func(t *testing.T) {
		d := escalationDaemon(t)
		require.NoError(t, d.db.MarkNeedsHuman("parent-1", "repo", held))
		beads := []poller.Bead{
			{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, Blocks: []string{"child-1"}},
		}
		needsHuman := map[string]struct{}{"parent-1\x00repo": {}}

		d.clearResolvedEpicHold(crucibleCfg(false), beads, needsHuman)

		assert.Contains(t, needsHumanKeys(t, d.db), "parent-1\x00repo")
		assert.Contains(t, needsHuman, "parent-1\x00repo")
	})

	t.Run("still no child is ready", func(t *testing.T) {
		d := escalationDaemon(t)
		require.NoError(t, d.db.MarkNeedsHuman("parent-1", "repo", held))
		// No Blocks: nothing of this epic is ready in this batch, which is the
		// state the hold was raised for.
		beads := []poller.Bead{{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}}}
		needsHuman := map[string]struct{}{"parent-1\x00repo": {}}

		d.clearResolvedEpicHold(crucibleCfg(true), beads, needsHuman)

		assert.Contains(t, needsHumanKeys(t, d.db), "parent-1\x00repo")
	})
}

// Only the holds this file raises are withdrawn. A parent flagged for something
// else — a schematic decline, an exhausted circuit breaker, an operator's own
// mark — keeps its flag, or "self-clearing" becomes a way to lose one.
func TestClearResolvedEpicHold_LeavesOtherReasonsAlone(t *testing.T) {
	d := escalationDaemon(t)
	parent := poller.Bead{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, Blocks: []string{"child-1"}}
	require.NoError(t, d.db.MarkNeedsHuman("parent-1", "repo", schematicDeclineReason(parent, false, "independent")))

	needsHuman := map[string]struct{}{"parent-1\x00repo": {}}
	d.clearResolvedEpicHold(crucibleCfg(true), []poller.Bead{parent}, needsHuman)

	assert.Contains(t, needsHumanKeys(t, d.db), "parent-1\x00repo")
	assert.Contains(t, needsHuman, "parent-1\x00repo")
}

// An unflagged bead is not queried at all, and a nil config is not dereferenced.
func TestClearResolvedEpicHold_NoOps(t *testing.T) {
	d := escalationDaemon(t)
	beads := []poller.Bead{{ID: "parent-1", Anvil: "repo", Labels: []string{"crucible"}, Blocks: []string{"child-1"}}}

	d.clearResolvedEpicHold(nil, beads, map[string]struct{}{"parent-1\x00repo": {}})
	d.clearResolvedEpicHold(crucibleCfg(true), beads, map[string]struct{}{})

	assert.Empty(t, needsHumanKeys(t, d.db))
}
