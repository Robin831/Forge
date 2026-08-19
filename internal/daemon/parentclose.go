package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/epic"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
)

// maxParentCloseDepth bounds the walk up the parent chain after a child
// closes. A parent that closes may itself be the last open child of a
// grandparent, so the walk has to continue — but bd's hierarchy is data, and
// data can contain a cycle a single-shot visited-set would still traverse
// forever across recursive calls. Five levels is far past any nesting seen in
// practice (bd's own decomposition goes two deep) and costs one bd show plus
// one bd list per level.
const maxParentCloseDepth = 5

// bdShowRelation is one entry of the `dependencies` array `bd show --json`
// returns: the related bead, hydrated, plus the type of the edge. Note the
// field names differ from the `bd ready --json` form (`depends_on_id`/`type`,
// poller.BeadDep) — same relation, two spellings, so a payload from `bd show`
// cannot be unmarshalled straight into a poller.Bead.
type bdShowRelation struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	DependencyType string `json:"dependency_type"`
}

// bdShowBead is the subset of `bd show <id> --json` the parent auto-close
// reads: enough to find the bead's parents, and enough to decide whether a
// parent is closeable.
type bdShowBead struct {
	ID           string           `json:"id"`
	Status       string           `json:"status"`
	Parent       string           `json:"parent"`
	Labels       []string         `json:"labels"`
	Dependencies []bdShowRelation `json:"dependencies"`
}

// asPollerBead reshapes the show payload into the form poller.ParentCandidates
// consumes, so the "which beads may be my parent" precedence lives in exactly
// one place rather than being re-derived here against a second spelling of the
// same edges.
func (b bdShowBead) asPollerBead() poller.Bead {
	pb := poller.Bead{ID: b.ID, Parent: b.Parent, Labels: b.Labels}
	for _, dep := range b.Dependencies {
		pb.Dependencies = append(pb.Dependencies, poller.BeadDep{
			IssueID:     b.ID,
			DependsOnID: dep.ID,
			Type:        dep.DependencyType,
		})
	}
	return pb
}

// bdListChild is the subset of `bd list --parent <id> --json` needed to decide
// whether every child of a parent is closed.
type bdListChild struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Parent string `json:"parent"`
}

// isClosedBeadStatus reports bd's one terminal status. Anything else —
// including an empty string from a payload that did not carry the field — is
// read as "not closed", which is the direction that leaves a parent open.
func isClosedBeadStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "closed")
}

// maybeCloseParents closes the parents of a just-closed child when bd reports
// every one of their children as closed, walking up the chain for as long as
// each close makes the next parent complete.
//
// It is the independent-mode counterpart to the Crucible closing an
// orchestrated parent once the final PR exists (internal/crucible): with
// orchestration opt-in, the ordinary case is a parent whose children each
// merge on their own PR and whose parent then stays open forever.
//
// Every failure — bd unreachable, an unparseable payload, a status it cannot
// read, a child list that disagrees with the edge it came from — leaves the
// parent open. A parent left open costs one `bd close`; a parent closed over
// unfinished children costs the work queued behind it.
func (d *Daemon) maybeCloseParents(ctx context.Context, childID, anvil, anvilPath string) {
	if !d.cfg.Load().Settings.IsAutoCloseParentsEnabled() {
		return
	}
	d.closeCompleteParents(ctx, childID, anvil, anvilPath, maxParentCloseDepth)
}

// closeCompleteParents is maybeCloseParents' recursive half: the config gate is
// checked once by the caller, and depth bounds the walk.
func (d *Daemon) closeCompleteParents(ctx context.Context, childID, anvil, anvilPath string, depth int) {
	if depth <= 0 {
		d.logger.Warn("parent auto-close stopped at the depth limit",
			"bead", childID, "anvil", anvil, "max_depth", maxParentCloseDepth)
		return
	}
	if d.beadShower == nil || d.childLister == nil || ctx.Err() != nil {
		return
	}

	child, ok := d.showBeadForParentClose(anvilPath, childID)
	if !ok {
		return
	}
	for _, parentID := range poller.ParentCandidates(child.asPollerBead()) {
		if ctx.Err() != nil {
			return
		}
		if d.closeParentIfComplete(ctx, parentID, childID, anvil, anvilPath) {
			d.closeCompleteParents(ctx, parentID, anvil, anvilPath, depth-1)
		}
	}
}

// closeParentIfComplete closes one candidate parent when every gate passes,
// reporting whether the close actually landed.
//
// childID is the bead whose close triggered the check. It has to appear among
// the children bd reports for the parent: poller.ParentCandidates is
// deliberately loose (it also yields the depends-on side of a plain `blocks`
// sequencing edge, which is a predecessor and not a parent at all), and the
// membership check is what narrows it back to the hierarchy bd itself knows.
// Without it, finishing the last bead in a `blocks` chain would close the bead
// that merely came before it.
func (d *Daemon) closeParentIfComplete(ctx context.Context, parentID, childID, anvil, anvilPath string) bool {
	parent, ok := d.showBeadForParentClose(anvilPath, parentID)
	if !ok {
		return false
	}
	if isClosedBeadStatus(parent.Status) {
		return false
	}
	// An orchestrated parent belongs to the Crucible, which closes it when the
	// final PR is created. Closing it here would race that, and would close it
	// before the branch it owns has reached main.
	if epic.IsOrchestrated(parent.Labels) {
		d.logger.Debug("parent auto-close skipped: parent is orchestrated (Crucible owns its close)",
			"parent", parentID, "anvil", anvil)
		return false
	}

	children, err := d.listChildBeads(anvilPath, parentID)
	if err != nil {
		d.logger.Warn("parent auto-close skipped: could not list children",
			"parent", parentID, "anvil", anvil, "error", err)
		return false
	}
	if len(children) == 0 {
		return false
	}

	found := false
	open := 0
	for _, c := range children {
		if c.ID == childID {
			found = true
		}
		if !isClosedBeadStatus(c.Status) {
			open++
		}
	}
	if !found {
		d.logger.Debug("parent auto-close skipped: closed bead is not among the parent's children",
			"parent", parentID, "bead", childID, "anvil", anvil)
		return false
	}
	if open > 0 {
		d.logger.Debug("parent auto-close skipped: children still open",
			"parent", parentID, "anvil", anvil, "open_children", open, "total_children", len(children))
		return false
	}

	// Share the merge path's in-flight guard: two siblings closing at once must
	// not both decide to close the same parent.
	key := anvil + "\x00" + parentID
	if _, busy := d.beadCloseInFlight.LoadOrStore(key, true); busy {
		return false
	}
	defer d.beadCloseInFlight.Delete(key)

	reason := parentCloseReason(childID, len(children))
	closeCtx, cancel := context.WithTimeout(ctx, executil.BdTimeout())
	defer cancel()
	if err := d.closeBead(closeCtx, parentID, anvilPath, reason); err != nil && !isAlreadyClosedBdError(err) {
		d.logger.Warn("failed to auto-close parent bead", "parent", parentID, "anvil", anvil, "error", err)
		return false
	}

	d.logger.Info("parent bead auto-closed — every child is closed",
		"parent", parentID, "anvil", anvil, "children", len(children), "last_child", childID)
	_ = d.db.LogEvent(state.EventParentBeadAutoClosed,
		fmt.Sprintf("parent bead %s auto-closed: all %d child beads are closed (last: %s)",
			parentID, len(children), childID), parentID, anvil)
	return true
}

// parentCloseReason is the close reason recorded on the parent. It names the
// count and the child that completed it, because nothing else in the bead's
// history says why it closed with no PR of its own.
func parentCloseReason(childID string, children int) string {
	plural := "child beads"
	if children == 1 {
		plural = "child bead"
	}
	return fmt.Sprintf("all %d %s closed (last: %s)", children, plural, childID)
}

// showBeadForParentClose runs `bd show <id> --json` through the daemon's one
// bd-show seam and decodes it. The second return value is false for every
// failure — an unreadable bead is not an empty one.
func (d *Daemon) showBeadForParentClose(anvilPath, beadID string) (bdShowBead, bool) {
	out, stderr, err := d.beadShower(anvilPath, beadID)
	if err != nil {
		d.logger.Debug("parent auto-close: bd show failed", "bead", beadID, "error", err, "stderr", stderr)
		return bdShowBead{}, false
	}
	var bead bdShowBead
	if err := executil.DecodeJSON(out, &bead); err != nil {
		var beads []bdShowBead
		if arrErr := executil.DecodeJSON(out, &beads); arrErr != nil || len(beads) == 0 {
			d.logger.Debug("parent auto-close: failed to parse bd show", "bead", beadID, "error", err)
			return bdShowBead{}, false
		}
		bead = beads[0]
	}
	return bead, true
}

// listChildBeads returns the direct children bd records for a parent, closed
// ones included. The `bd show` payload cannot answer this: it reports the
// relation from the child's side only, and its `dependents` array is empty for
// a hierarchical parent.
//
// A failure is returned as an error rather than an empty slice — "this parent
// has no children" and "I could not ask" lead to opposite decisions.
func (d *Daemon) listChildBeads(anvilPath, parentID string) ([]bdListChild, error) {
	out, stderr, err := d.childLister(anvilPath, parentID)
	if err != nil {
		return nil, fmt.Errorf("bd list --parent %s: %w: %s", parentID, err, strings.TrimSpace(stderr))
	}
	var children []bdListChild
	if err := executil.DecodeJSON(out, &children); err != nil {
		return nil, fmt.Errorf("parsing bd list --parent %s: %w", parentID, err)
	}
	return children, nil
}
