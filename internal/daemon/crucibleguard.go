package daemon

import (
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/epic"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
)

// crucibleOwner records why a bead in this poll batch is not the dispatch
// loop's to run, and which parent claimed it.
type crucibleOwner struct {
	// ParentID is the opted-in parent that owns this bead's work. It is empty
	// when no candidate could be *verified* as the one that opted in — the
	// escalation names a bead, so a guess would name the wrong one (see
	// stampingParent). A withheld child with no named owner is still withheld;
	// it just escalates nobody.
	ParentID string

	// Disabled marks the child of a parent that opted into orchestration while
	// settings.crucible_enabled is false. Nobody creates the epic branch in
	// that case, so dispatching the child is a guaranteed hard failure in
	// worktree.Create ("base branch not found on origin") — it is withheld and
	// the parent is escalated instead. False means an actual Crucible owns the
	// bead and will dispatch it itself.
	Disabled bool
}

// crucibleOwnedChildren returns the "anvil\x00beadID" keys of beads in this
// poll batch whose work is not the dispatch loop's: the children of an
// orchestrated parent — one about to start a Crucible in this same cycle, one
// already running one, or one that opted in while the Crucible is switched off.
//
// Without this the dispatch loop happily dispatched a parent and its children
// in the same cycle — the Crucible then ran each child a second time, on the
// feature branch, against a bead another worker already held. Ordering does not
// save us either, since the batch is sorted by priority, so the set is computed
// in one pre-pass over the whole batch before any dispatch.
//
// The opt-in is what makes a parent an owner, NOT settings.crucible_enabled: a
// child stamped with `feature/<parent-id>` fails the same way whether the
// Crucible is off or merely has not reached it, and the setting being off is
// the case the operator has to be told about rather than the case that quietly
// burns the child's dispatch circuit breaker (Forge-fblf).
//
// A parent that has not opted in (no "crucible"/"epic-branch:" label) is never
// an owner: its children are independent beads and dispatching them is the
// whole point.
func (d *Daemon) crucibleOwnedChildren(cfg *config.Config, beads []poller.Bead) map[string]crucibleOwner {
	owned := make(map[string]crucibleOwner)
	enabled := cfg != nil && cfg.Settings.CrucibleEnabled

	// owner of a parent key: the parent's own id, plus whether a Crucible is
	// actually running for it (in which case the children are dispatched by
	// that Crucible, not withheld for the operator).
	type parentOwner struct {
		id     string
		active bool
	}
	orchestrating := make(map[string]parentOwner)

	// Parents already orchestrating. These are typically not in the batch at
	// all (the parent is in_progress), so their children are matched through
	// the child's own parent references below.
	d.crucibleStatuses.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok {
			// crucibleStatuses is keyed "<anvil>/<parentID>".
			if anvil, parentID, found := strings.Cut(k, "/"); found {
				orchestrating[anvil+"\x00"+parentID] = parentOwner{id: parentID, active: true}
			}
		}
		return true
	})

	// Parents in this batch that opted into orchestration.
	for _, b := range beads {
		if !crucible.IsCrucibleCandidate(b) {
			continue
		}
		key := b.Anvil + "\x00" + b.ID
		if prev, ok := orchestrating[key]; ok && prev.active {
			continue
		}
		orchestrating[key] = parentOwner{id: b.ID}
		// The parent's reconstructed Blocks name its children directly.
		for _, childID := range b.Blocks {
			owned[b.Anvil+"\x00"+childID] = crucibleOwner{ParentID: b.ID, Disabled: !enabled}
		}
	}

	// The other direction: a child that names an orchestrating parent through
	// its own parent field or a blocks/parent-child dependency. This is what
	// catches children of a parent that is already mid-Crucible and therefore
	// absent from the batch.
	for _, b := range beads {
		key := b.Anvil + "\x00" + b.ID
		if _, self := orchestrating[key]; self {
			continue
		}
		if _, already := owned[key]; already {
			continue
		}
		for _, parentID := range poller.ParentCandidates(b) {
			if parent, ok := orchestrating[b.Anvil+"\x00"+parentID]; ok {
				owned[key] = crucibleOwner{ParentID: parent.id, Disabled: !enabled && !parent.active}
				break
			}
		}
	}

	if enabled {
		return owned
	}

	// Crucible off: a child whose parent is in neither the batch nor the
	// status map still carries the parent's branch, stamped by the poller
	// because the parent opted in. That branch has no creator, so the bead is
	// withheld on the strength of the stamp alone.
	//
	// Naming the parent is a separate question from withholding the child, and
	// the answer is stampingParent's, not ParentCandidates'[0] — see there.
	for _, b := range beads {
		key := b.Anvil + "\x00" + b.ID
		if b.EpicBranch == "" {
			continue
		}
		if _, self := orchestrating[key]; self {
			continue
		}
		if _, already := owned[key]; already {
			continue
		}
		owned[key] = crucibleOwner{ParentID: stampingParent(b), Disabled: true}
	}

	return owned
}

// stampingParent returns the bead whose opt-in produced this child's
// EpicBranch stamp, or "" when that cannot be established.
//
// It is deliberately not "the first parent candidate": ParentCandidates returns
// the Parent field followed by every blocks/parent-child edge in order, and
// `bd dep add` sequencing edges are blocks-typed, so an ordinary upstream task
// routinely precedes the labeled parent in that list. Attributing the stamp to
// it would flag an unrelated — possibly ready — bead as needs_human, freezing it
// out of dispatch until an operator runs `forge queue clear`, while the parent
// that actually opted in is never escalated.
//
// The authoritative answer is the one ResolveEpicBranches recorded when it
// stamped the child (EpicParent: the candidate whose labels it verified). The
// branch-name fallback covers a bead stamped by some other path: a candidate
// only qualifies when the stamp is exactly the name its ID derives, which no
// unrelated upstream task can satisfy by accident. An explicitly named branch
// ("epic-branch:<name>") does not derive from any ID, so an unrecorded stamp of
// that shape names nobody — and naming nobody is the right answer, since the
// alternative is naming the wrong bead.
func stampingParent(b poller.Bead) string {
	if b.EpicParent != "" {
		return b.EpicParent
	}
	for _, candidate := range poller.ParentCandidates(b) {
		if epic.BranchName(candidate, nil) == b.EpicBranch {
			return candidate
		}
	}
	return ""
}

// crucibleDisabledReason is the operator-facing sentence for the one
// misconfiguration this file and dispatchBead both report: a parent opted into
// epic orchestration while settings.crucible_enabled is false. Both views —
// the parent refusing to dispatch and each child being withheld — read the same
// wording from here, so rewording one cannot drift from the other.
func crucibleDisabledReason(parentID, branch string) string {
	return fmt.Sprintf("bead %s opts into epic orchestration (%q or epic-branch:<name>) but "+
		"settings.crucible_enabled is false — nothing creates %s, so this parent is not dispatched "+
		"and its children are held back. Enable the Crucible to orchestrate the epic, or remove the "+
		"label so this parent and its children dispatch independently to main",
		parentID, epic.CrucibleLabel, branch)
}

// summarizeIDs renders a bounded, comma-separated list of bead IDs for an
// operator-facing message. An epic can carry dozens of children and the reason
// text lands in a Needs Attention row; naming the first few and counting the
// rest keeps that row readable.
func summarizeIDs(ids []string, limit int) string {
	if len(ids) <= limit {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:limit], ", "), len(ids)-limit)
}

// escalateOrchestratedParent is the one way an opted-in parent stops being
// dispatched: a Needs Attention entry naming what to do, instead of a run.
//
// Every caller is a case where the parent could be run as an ordinary bead but
// must not be — its label is what routes its children, and a standalone dispatch
// merges the parent to main without un-routing them. The pending worker row is
// failed first, before the bd/DB writes: it counts toward dispatch capacity
// until something closes it, and a leaked row costs a smith slot for the life of
// the daemon.
func (d *Daemon) escalateOrchestratedParent(bead poller.Bead, claimWorkerID, reason string) {
	if claimWorkerID != "" {
		if err := d.db.UpdateWorkerStatus(claimWorkerID, state.WorkerFailed); err != nil {
			d.logger.Warn("failed to mark pending worker failed after escalating an orchestrated parent",
				"bead", bead.ID, "worker", claimWorkerID, "error", err)
		}
	}
	if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); err != nil {
		d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
	}
	d.recordDispatchFailure(bead.ID, bead.Anvil, reason, true)
}

// withholdDisabledEpicChild reports a child held back because its parent opted
// into orchestration while settings.crucible_enabled is false, and escalates
// the parent — once — so the misconfiguration is visible rather than a queue
// that silently stops moving.
//
// The escalation lands on the parent, not the child: the parent is the bead
// carrying the label and the bead an operator has to act on (enable the
// Crucible, or drop the label so the whole family dispatches to main). Marking
// the children would leave a flag behind on every one of them for a
// `forge queue clear` to undo later.
//
// needsHuman is the poll cycle's needs-human snapshot; it is updated in place
// so several children of the same parent escalate it only once.
func (d *Daemon) withholdDisabledEpicChild(bead poller.Bead, owner crucibleOwner, needsHuman map[string]struct{}) {
	d.logger.Warn("withholding epic child: its parent opted into orchestration but settings.crucible_enabled is false",
		"bead", bead.ID, "anvil", bead.Anvil, "parent", owner.ParentID, "base", bead.EpicBranch)

	if owner.ParentID == "" {
		return
	}
	// beadID first, anvil second: this key is the dispatch loop's needsHumanSet
	// form (daemon.go: bead.ID+"\x00"+bead.Anvil), NOT the owned map's
	// anvil-first form above. Reversing it back would break the once-per-parent
	// dedup silently — the lookup would simply never hit.
	key := owner.ParentID + "\x00" + bead.Anvil
	if _, already := needsHuman[key]; already {
		return
	}

	reason := crucibleDisabledReason(owner.ParentID, epicBranchOrDefault(bead, owner.ParentID))
	if err := d.db.MarkNeedsHuman(owner.ParentID, bead.Anvil, reason); err != nil {
		d.logger.Error("failed to mark epic parent as needs_human", "bead", owner.ParentID, "error", err)
		return
	}
	needsHuman[key] = struct{}{}
}

// epicBranchOrDefault names the branch the withheld child would have based on:
// the branch the poller stamped it with, falling back to the derived name for
// the parent when the stamp is missing.
func epicBranchOrDefault(bead poller.Bead, parentID string) string {
	if bead.EpicBranch != "" {
		return bead.EpicBranch
	}
	return epic.BranchName(parentID, nil)
}
