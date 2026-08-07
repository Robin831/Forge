package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
)

// EpicBranchLabelPrefix is the label prefix used to define an epic's feature
// branch. A label "epic-branch:feature/depcheck" means the epic uses
// "feature/depcheck" as its shared branch.
const EpicBranchLabelPrefix = "epic-branch:"

// DefaultEpicBranchPrefix is the branch name prefix used when an epic bead
// has no explicit epic-branch label. The branch name is derived as
// "epic/<epic-id>".
const DefaultEpicBranchPrefix = "epic/"

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

// ResolveEpicBranches enriches beads that belong to an epic with the epic's
// branch name. It discovers the epic relationship via two paths:
//
//  1. Parent field: child.Parent is set to an epic bead ID (legacy).
//  2. Blocks field: child.Blocks contains an epic-type bead ID, meaning the
//     child blocks the epic in the dependency graph. This is the preferred
//     approach because beads with a parent set are hidden from `bd ready`.
//
// It calls `bd show <id> --json` for each unique candidate, caching results
// to avoid duplicate calls.
func ResolveEpicBranches(ctx context.Context, beads []Bead, anvilPaths map[string]string) {
	// Cache lookups: "anvil:beadID" → resolved branch (empty string = not an epic)
	cache := make(map[string]string)

	for i := range beads {
		b := &beads[i]

		anvilPath, ok := anvilPaths[b.Anvil]
		if !ok {
			continue
		}

		// Path 1: explicit parent field (legacy)
		if b.Parent != "" {
			cacheKey := b.Anvil + ":" + b.Parent
			if branch, cached := cache[cacheKey]; cached {
				b.EpicBranch = branch
				continue
			}
			branch := epicBranchLookupFunc(ctx, b.Parent, anvilPath)
			cache[cacheKey] = branch
			b.EpicBranch = branch
			continue
		}

		// Path 2: check if any bead this one blocks is an epic.
		// A child that blocks an epic should be routed through the epic's
		// feature branch without needing the parent field set.
		if len(b.Blocks) > 0 {
			for _, blockedID := range b.Blocks {
				cacheKey := b.Anvil + ":" + blockedID
				if branch, cached := cache[cacheKey]; cached {
					if branch != "" {
						b.EpicBranch = branch
						break
					}
					continue
				}
				branch := epicBranchLookupFunc(ctx, blockedID, anvilPath)
				cache[cacheKey] = branch
				if branch != "" {
					b.EpicBranch = branch
					break
				}
			}
		}
	}
}

// parentBeadResponse is used to unmarshal the bead fields returned by
// `bd show <id> --json` during epic branch lookup.
type parentBeadResponse struct {
	Bead
}

// lookupEpicBranch fetches a parent bead's details and extracts the feature
// branch name. It only returns a branch for beads that have an explicit
// epic-branch label or are of type "epic". Regular dependency relationships
// (e.g. task blocks task) never generate a feature branch — only intentional
// epic parents do.
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

	// Guard: only return a branch for beads that are explicitly typed as "epic"
	// or carry an "epic-branch:" label. Regular tasks/features with dependents
	// are NOT epic parents — that misdetection was the source of Forge-t6y9
	// where task A blocks task B caused A's PR to incorrectly target
	// feature/B instead of main.
	if !isEpicParentBead(resp.IssueType, resp.Labels) {
		return ""
	}

	return ExtractParentBranch(resp.Bead)
}

// isEpicParentBead reports whether a bead qualifies as an epic parent that
// should route child PRs through a shared feature branch. Only beads with
// IssueType "epic" or an explicit "epic-branch:" label qualify. Regular
// tasks or features that happen to have dependents do NOT qualify — that
// distinction is what the Forge-t6y9 fix enforces.
func isEpicParentBead(issueType string, labels []string) bool {
	for _, label := range labels {
		if strings.HasPrefix(strings.ToLower(label), strings.ToLower(EpicBranchLabelPrefix)) {
			return true
		}
	}
	return strings.EqualFold(issueType, "epic")
}

// DefaultFeatureBranchPrefix is the branch name prefix used for non-epic
// parent beads (e.g. features) that have no explicit epic-branch label.
const DefaultFeatureBranchPrefix = "feature/"

// ExtractParentBranch extracts the shared feature branch name from a parent
// bead's labels. It looks for a label with the "epic-branch:" prefix. If none
// is found, it returns a default branch: "epic/<bead-id>" for epics, or
// "feature/<bead-id>" for other types (e.g. features with children).
func ExtractParentBranch(b Bead) string {
	for _, label := range b.Labels {
		if strings.HasPrefix(strings.ToLower(label), strings.ToLower(EpicBranchLabelPrefix)) {
			branch := strings.TrimPrefix(label, label[:len(EpicBranchLabelPrefix)])
			branch = strings.TrimSpace(branch)
			if branch != "" {
				return branch
			}
		}
	}

	// Default convention based on type.
	if strings.EqualFold(b.IssueType, "epic") {
		return DefaultEpicBranchPrefix + sanitizeBeadID(b.ID)
	}
	return DefaultFeatureBranchPrefix + sanitizeBeadID(b.ID)
}

// ExtractEpicBranch is a backward-compatible wrapper that preserves the
// legacy semantics used by older callers. It mirrors the label parsing
// logic of ExtractParentBranch, but for non-epic beads without an explicit
// epic-branch label it returns the empty string instead of a default feature
// branch. New code should prefer ExtractParentBranch.
//
// Deprecated: Use ExtractParentBranch instead.
func ExtractEpicBranch(b Bead) string {
	for _, label := range b.Labels {
		if strings.HasPrefix(strings.ToLower(label), strings.ToLower(EpicBranchLabelPrefix)) {
			branch := strings.TrimPrefix(label, label[:len(EpicBranchLabelPrefix)])
			branch = strings.TrimSpace(branch)
			if branch != "" {
				return branch
			}
		}
	}

	// Legacy default: only epics get an implicit default branch. Non-epic
	// beads without an explicit label return the empty string.
	if strings.EqualFold(b.IssueType, "epic") {
		return DefaultEpicBranchPrefix + sanitizeBeadID(b.ID)
	}
	return ""
}

// IsEpicBead returns true if the bead is an epic type. This is used by the
// daemon for the legacy epic branch creation path. For Crucible candidacy,
// use crucible.IsCrucibleCandidate which checks for children (Blocks) instead.
func IsEpicBead(b Bead) bool {
	return strings.EqualFold(b.IssueType, "epic")
}

// sanitizeBeadID converts a bead ID to a safe branch name component.
// Slashes are replaced so the result does not create unexpected path segments
// when used as "epic/<id>" (matching worktree.sanitizePath behaviour).
func sanitizeBeadID(id string) string {
	r := strings.NewReplacer(
		" ", "-",
		":", "-",
		"\\", "-",
		"/", "-",
	)
	return r.Replace(id)
}
