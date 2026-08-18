package daemon

import (
	"strings"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/poller"
)

// crucibleOwnedChildren returns the set of "anvil\x00beadID" keys for beads in
// this poll batch whose work belongs to a Crucible: the children of an
// orchestrated parent that is either about to start a Crucible in this same
// cycle or is already running one.
//
// Without this the dispatch loop happily dispatched a parent and its children
// in the same cycle — the Crucible then ran each child a second time, on the
// feature branch, against a bead another worker already held. Ordering does not
// save us either, since the batch is sorted by priority, so the set is computed
// in one pre-pass over the whole batch before any dispatch.
//
// A parent that has not opted in (no "crucible"/"epic-branch:" label) is never
// in the set: its children are independent beads and dispatching them is the
// whole point.
func (d *Daemon) crucibleOwnedChildren(cfg *config.Config, beads []poller.Bead) map[string]struct{} {
	owned := make(map[string]struct{})

	// Parents already orchestrating. These are typically not in the batch at
	// all (the parent is in_progress), so their children are matched through
	// the child's own parent references below.
	orchestrating := make(map[string]struct{})
	d.crucibleStatuses.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok {
			// crucibleStatuses is keyed "<anvil>/<parentID>".
			if anvil, parentID, found := strings.Cut(k, "/"); found {
				orchestrating[anvil+"\x00"+parentID] = struct{}{}
			}
		}
		return true
	})

	// Parents in this batch that will take the Crucible path.
	if cfg != nil && cfg.Settings.CrucibleEnabled {
		for _, b := range beads {
			if crucible.IsCrucibleCandidate(b) {
				orchestrating[b.Anvil+"\x00"+b.ID] = struct{}{}
				// The parent's reconstructed Blocks name its children directly.
				for _, childID := range b.Blocks {
					owned[b.Anvil+"\x00"+childID] = struct{}{}
				}
			}
		}
	}

	if len(orchestrating) == 0 {
		return owned
	}

	// The other direction: a child that names an orchestrating parent through
	// its own parent field or a blocks/parent-child dependency. This is what
	// catches children of a parent that is already mid-Crucible and therefore
	// absent from the batch.
	for _, b := range beads {
		if _, self := orchestrating[b.Anvil+"\x00"+b.ID]; self {
			continue
		}
		for _, parentID := range poller.ParentCandidates(b) {
			if _, ok := orchestrating[b.Anvil+"\x00"+parentID]; ok {
				owned[b.Anvil+"\x00"+b.ID] = struct{}{}
				break
			}
		}
	}

	return owned
}
