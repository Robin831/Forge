package daemon

import (
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/epic"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/termtext"
)

// Auto-close of a plain grouping parent.
//
// Epic orchestration is opt-in (the "crucible" label), so the default for a
// parent bead with children is independent dispatch: every child runs the
// ordinary pipeline and PRs to main. In the orchestrated case the Crucible
// closes the parent itself once the final PR merges — independent mode had no
// equivalent, so a parent filed purely to group work stayed open forever after
// its last child closed.
//
// This is that equivalent, and it is deliberately the *narrow* one: it runs
// only off a child close that a merge produced, it never touches a parent the
// Crucible owns, and every question it asks bd that comes back unanswered
// resolves to "leave the parent open". Leaving a parent open costs one manual
// `bd close`; closing one early loses the grouping while work is still open.

// maxNamedAutoClosedChildren bounds the child IDs named in the close reason and
// the event message. The reason is persisted on the bead, so an epic with fifty
// children must not write fifty IDs into it; the count carries the rest.
const maxNamedAutoClosedChildren = 8

// autoCloseReasonPrefix labels the persisted `bd close` reason, where the
// detail stands alone. The event message supplies its own framing, so the
// prefix lives here rather than inside the shared detail.
const autoCloseReasonPrefix = "auto-closed: "

// autoCloseVerdict is what one candidate reports back to the candidate walk in
// maybeCloseGroupingParent.
type autoCloseVerdict int

const (
	// autoCloseTryNext: this candidate is not the parent the just-closed child
	// belongs to, so the walk moves on.
	autoCloseTryNext autoCloseVerdict = iota

	// autoCloseDone: the walk ends here. Either the parent closed, or the
	// candidate could not be read at all (leaving the relationship unknown —
	// unproven is not the same as ruled out), or the candidate is this child's
	// parent and this caller is simply not the one closing it — it is already
	// closed, the Crucible owns it, a sibling is closing it right now, it
	// still has open children, or bd refused. "Identified but not closable by
	// me" must end the walk just as firmly as a close: the trailing candidates
	// are `blocks` sequencing edges that only look like parents from the
	// child's side, so falling through to them closes a bead outside the
	// relationship that justified acting at all.
	autoCloseDone
)

// maybeCloseGroupingParent closes the grouping parent of a just-closed child
// when that child was the last one open.
//
// It is called from the merge-close path (closeMergedBead) after the child's
// own close has landed. Every failure is logged and swallowed: the merge path
// must not report a failure because a courtesy close did not happen.
func (d *Daemon) maybeCloseGroupingParent(childID, anvil, anvilPath string) {
	if d.beadShower == nil || d.parentCloser == nil {
		return
	}

	child, ok := d.showBeadRelations(anvilPath, childID, "child")
	if !ok {
		return
	}

	// ParentCandidates orders the explicit parent field ahead of the
	// dependency edges, which is the order to try them in: the first candidate
	// that is an open, unorchestrated parent with children is the one this
	// child belongs to.
	//
	// At most one parent is closed per child close, and the walk stops at the
	// first candidate that turns out to be this child's parent — closed or
	// not — or that could not be read at all, leaving the relationship
	// unknown. A bead has one parent; once it is found, the trailing
	// candidates are sequencing edges and nothing that happens to the parent
	// makes them closable.
	for _, parentID := range poller.ParentCandidates(child.Bead) {
		if d.closeGroupingParentIfComplete(parentID, childID, anvil, anvilPath) == autoCloseDone {
			return
		}
	}
}

// closeGroupingParentIfComplete runs the gates for one candidate parent and
// closes it when they all pass. Its verdict tells the walk whether to continue.
func (d *Daemon) closeGroupingParentIfComplete(parentID, childID, anvil, anvilPath string) autoCloseVerdict {
	parent, ok := d.showBeadRelations(anvilPath, parentID, "parent")
	if !ok {
		// A candidate bd cannot answer for may well be the real parent, and
		// the next candidate is only safe to consider once this one has been
		// ruled out. An unanswered question ends the walk rather than pushing
		// it one edge further out.
		return autoCloseDone
	}

	children, open := parent.children()
	if len(children) == 0 {
		// A parent that reports no children at all is not the parent this
		// child just finished — a stale index or a relation read from the
		// wrong side. Never close on the strength of an empty list.
		return autoCloseTryNext
	}
	if !containsBeadID(children, childID) {
		// Children, but not this one: another bead's parent, or a sequencing
		// predecessor that happens to have children of its own.
		d.logger.Debug("grouping parent auto-close skipped: candidate does not list this child",
			"candidate", parentID, "child", childID, "anvil", anvil)
		return autoCloseTryNext
	}

	// From here the candidate is this child's parent, so every remaining exit
	// ends the walk whether or not the close happens in this call.
	if epic.IsOrchestrated(parent.Labels) {
		// The Crucible owns an opted-in parent and closes it after the final
		// PR merges. Closing it here would race that. This is the "identified
		// but not closable by me" case: the parent is found, so the trailing
		// candidates are sequencing edges and the walk must not fall through
		// to them. (An orchestrated candidate that does NOT list this child
		// already returned autoCloseTryNext above — being orchestrated says
		// nothing about a bead that is not this child's parent.)
		d.logger.Debug("grouping parent auto-close skipped: parent is orchestrated",
			"parent", parentID, "child", childID, "anvil", anvil)
		return autoCloseDone
	}
	if strings.EqualFold(strings.TrimSpace(parent.Status), "closed") {
		return autoCloseDone
	}
	if len(open) > 0 {
		d.logger.Debug("grouping parent stays open: children still open",
			"parent", parentID, "child", childID, "anvil", anvil,
			"open_children", len(open), "total_children", len(children))
		return autoCloseDone
	}

	// Collapse the sibling race: two children whose PRs merge in the same
	// cycle both see a fully closed sibling set. The loser is done — the close
	// is happening, just not on this goroutine.
	key := anvil + "\x00" + parentID
	if _, busy := d.parentAutoCloseInFlight.LoadOrStore(key, true); busy {
		return autoCloseDone
	}
	defer d.parentAutoCloseInFlight.Delete(key)

	// The IDs come from bd, so they are text Forge did not write on their way
	// to a persisted close reason and a rendered activity feed.
	detail := termtext.Line(autoCloseParentDetail(children))
	if err := d.parentCloser(anvilPath, parentID, autoCloseReasonPrefix+detail); err != nil {
		if !isAlreadyClosedBdError(err) {
			d.logger.Warn("failed to auto-close grouping parent; leaving it open",
				"parent", parentID, "child", childID, "anvil", anvil, "error", err)
		}
		return autoCloseDone
	}

	d.logger.Info("auto-closed grouping parent (all children closed)",
		"parent", parentID, "last_child", childID, "anvil", anvil, "children", len(children))
	_ = d.db.LogEvent(state.EventBeadAutoClosed,
		fmt.Sprintf("Parent %s auto-closed: %s", termtext.Line(parentID), detail), parentID, anvil)
	return autoCloseDone
}

// containsBeadID reports whether ids holds beadID, tolerating the case and
// padding differences bd's own output shows between fields.
func containsBeadID(ids []string, beadID string) bool {
	want := strings.TrimSpace(beadID)
	for _, id := range ids {
		if strings.EqualFold(strings.TrimSpace(id), want) {
			return true
		}
	}
	return false
}

// autoCloseParentDetail renders the body of the close reason, naming the
// children so the closed parent records *why* it closed rather than just that
// something did it. The caller supplies the framing (autoCloseReasonPrefix on
// the bd reason, "Parent <id> auto-closed:" in the event).
func autoCloseParentDetail(children []string) string {
	named := children
	suffix := ""
	if len(named) > maxNamedAutoClosedChildren {
		named = named[:maxNamedAutoClosedChildren]
		suffix = fmt.Sprintf(", +%d more", len(children)-maxNamedAutoClosedChildren)
	}
	plural := "children"
	if len(children) == 1 {
		plural = "child"
	}
	return fmt.Sprintf("all %d %s closed (%s%s)",
		len(children), plural, strings.Join(named, ", "), suffix)
}

// beadRelations is the slice of `bd show --json` this path reads: the bead
// itself (for its labels, status and the edges pointing at its parents) plus
// its dependents (the edges pointing at its children).
type beadRelations struct {
	poller.Bead
	Dependents []beadDependent `json:"dependents"`
}

type beadDependent struct {
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
	Status         string `json:"status"`
}

// children returns the bead's child IDs and, separately, the ones that are not
// yet closed. The dependency-type filter is the same one poller.OpenChildren
// applies: a plain "depends on" edge is a sequencing relation, not a child.
func (b beadRelations) children() (all, open []string) {
	for _, dep := range b.Dependents {
		if dep.DependencyType != "blocks" && dep.DependencyType != "parent-child" {
			continue
		}
		if dep.ID == "" {
			continue
		}
		all = append(all, dep.ID)
		if !strings.EqualFold(strings.TrimSpace(dep.Status), "closed") {
			open = append(open, dep.ID)
		}
	}
	return all, open
}

// showBeadRelations reads one bead through the injectable bd-show hook. role
// only labels the debug line, so a failed lookup says which side of the
// relation could not be read.
func (d *Daemon) showBeadRelations(anvilPath, beadID, role string) (beadRelations, bool) {
	out, stderr, err := d.beadShower(anvilPath, beadID)
	if err != nil {
		d.logger.Debug("grouping parent auto-close: bd show failed",
			"bead", beadID, "role", role, "error", err, "stderr", stderr)
		return beadRelations{}, false
	}

	rel, err := decodeBdShow[beadRelations](out)
	if err != nil {
		d.logger.Debug("grouping parent auto-close: failed to parse bd show",
			"bead", beadID, "role", role, "error", err)
		return beadRelations{}, false
	}
	return rel, true
}
