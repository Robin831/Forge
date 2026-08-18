package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"log"

	"github.com/Robin831/Forge/internal/epic"
	"github.com/Robin831/Forge/internal/executil"
)

// epicBranchLookupFunc is the function used to look up epic branch names.
// It defaults to lookupEpicBranch but can be replaced in tests.
var epicBranchLookupFunc = lookupEpicBranch

// SetEpicBranchLookupForTest overrides the epic-branch lookup function and
// returns a restore function. It lets packages outside poller (e.g. daemon)
// exercise epic-branch resolution without shelling out to bd. Intended for
// tests only; always defer the returned restore.
func SetEpicBranchLookupForTest(fn func(ctx context.Context, parentID, anvilPath string) string) (restore func()) {
	orig := epicBranchLookupFunc
	epicBranchLookupFunc = fn
	return func() { epicBranchLookupFunc = orig }
}

// ResolveEpicBranches enriches beads whose parent has opted into epic
// orchestration with that parent's branch name. It discovers the parent
// relationship two ways:
//
//  1. Parent field: child.Parent names the parent bead.
//  2. Dependencies: a "blocks" or "parent-child" dependency entry whose
//     depends_on_id names the parent — the same edge pollAnvil uses to
//     reconstruct a parent's children, read from the child's side.
//
// The raw "blocks" JSON field is deliberately NOT consulted: pollAnvil clears
// it and rebuilds it with the inverted meaning ("my children"), so reading it
// here as "beads I block" walked the graph the wrong way.
//
// A parent that has not opted in resolves to "", which leaves the child with an
// empty EpicBranch — worktree from main, PR to main, like any standalone bead.
//
// It calls `bd show <id> --json` for each unique candidate parent, caching
// results to avoid duplicate calls.
func ResolveEpicBranches(ctx context.Context, beads []Bead, anvilPaths map[string]string) {
	// Cache lookups: "anvil:beadID" → resolved branch (empty string = not an
	// orchestrated parent)
	cache := make(map[string]string)

	for i := range beads {
		b := &beads[i]

		anvilPath, ok := anvilPaths[b.Anvil]
		if !ok {
			continue
		}

		for _, parentID := range ParentCandidates(*b) {
			cacheKey := b.Anvil + ":" + parentID
			branch, cached := cache[cacheKey]
			if !cached {
				branch = epicBranchLookupFunc(ctx, parentID, anvilPath)
				cache[cacheKey] = branch
			}
			if branch != "" {
				b.EpicBranch = branch
				break
			}
		}
	}
}

// ParentCandidates returns the bead IDs that may be this bead's parent, in
// precedence order: the explicit parent field first, then the depends-on side
// of any "blocks"/"parent-child" dependency entry — the same edges pollAnvil
// walks to reconstruct a parent's children, read from the child's side.
// Self-references and duplicates are dropped.
//
// It is exported because the daemon needs the same answer when deciding
// whether a bead's work belongs to a Crucible that is already running.
func ParentCandidates(b Bead) []string {
	var out []string
	seen := map[string]bool{b.ID: true}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}

	add(b.Parent)
	for _, dep := range b.Dependencies {
		if dep.Type == "blocks" || dep.Type == "parent-child" {
			add(dep.DependsOnID)
		}
	}
	return out
}

// parentBeadResponse is used to unmarshal the bead fields returned by
// `bd show <id> --json` during epic branch lookup.
type parentBeadResponse struct {
	Bead
}

// lookupEpicBranch fetches a candidate parent bead and returns its shared
// branch name — but only when the parent has explicitly opted into epic
// orchestration (see epic.IsOrchestrated). Ordinary parents, including beads
// with `issue_type: epic`, return "" so their children run as independent
// beads that PR to main.
func lookupEpicBranch(ctx context.Context, parentID, anvilPath string) string {
	cmd, cancel := executil.BdCommand(ctx, "show", parentID, "--json")
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("lookupEpicBranch: bd show %s failed: %v: %s", parentID, err, stderr.String())
		return ""
	}

	// bd show --json may return an array with a single element: [{...}]
	output = unwrapJSONArray(output)

	var resp parentBeadResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		log.Printf("lookupEpicBranch: failed to unmarshal bd show %s output: %v", parentID, err)
		return ""
	}

	if !isEpicParentBead(resp.Labels) {
		return ""
	}

	return ExtractParentBranch(resp.Bead)
}

// isEpicParentBead reports whether a bead has opted into epic orchestration.
// Only the "crucible" label or an explicit "epic-branch:" label qualify — a
// bead's issue type does not, which is what makes children of an ordinary
// (even epic-typed) parent independent by default.
func isEpicParentBead(labels []string) bool {
	return epic.IsOrchestrated(labels)
}

// IsOrchestratedParent reports whether a bead has opted into epic
// orchestration via its labels. It is the one gate every epic/Crucible code
// path shares.
func IsOrchestratedParent(b Bead) bool {
	return epic.IsOrchestrated(b.Labels)
}

// ExtractParentBranch returns the shared branch name for a parent bead: the
// branch named by an "epic-branch:<name>" label, else the derived
// "feature/<bead-id>". It delegates to epic.BranchName so the poller and the
// Crucible cannot derive different names for the same parent.
//
// It derives a name for any bead; callers gate on IsOrchestratedParent first.
func ExtractParentBranch(b Bead) string {
	return epic.BranchName(b.ID, b.Labels)
}

// sanitizeBeadID converts a bead ID to a safe branch name component.
func sanitizeBeadID(id string) string {
	return epic.SanitizeID(id)
}
