package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

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

// ResolveEpicBranches enriches beads that have a parent epic with the epic's
// branch name. It calls `bd show <parent-id> --json` for each unique parent
// to discover the branch, caching results to avoid duplicate calls.
func ResolveEpicBranches(ctx context.Context, beads []Bead, anvilPaths map[string]string) {
	// Cache parent lookups: parentID → resolved branch (empty string = not an epic)
	cache := make(map[string]string)

	for i := range beads {
		b := &beads[i]
		if b.Parent == "" {
			continue
		}

		anvilPath, ok := anvilPaths[b.Anvil]
		if !ok {
			continue
		}

		cacheKey := b.Anvil + ":" + b.Parent
		if branch, cached := cache[cacheKey]; cached {
			b.EpicBranch = branch
			continue
		}

		branch := lookupEpicBranch(ctx, b.Parent, anvilPath)
		cache[cacheKey] = branch
		b.EpicBranch = branch
	}
}

// lookupEpicBranch fetches a parent bead's details and extracts the epic
// branch name. Returns empty string if the parent is not an epic or has no
// branch configured.
func lookupEpicBranch(ctx context.Context, parentID, anvilPath string) string {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "show", parentID, "--json"))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var parent Bead
	if err := json.Unmarshal(output, &parent); err != nil {
		return ""
	}

	// Only resolve for epic-type beads
	if !strings.EqualFold(parent.IssueType, "epic") {
		return ""
	}

	return ExtractEpicBranch(parent)
}

// ExtractEpicBranch extracts the epic branch name from a bead's labels.
// It looks for a label with the "epic-branch:" prefix. If none is found
// and the bead is an epic, it returns the default "epic/<bead-id>".
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

	// Default convention: epic/<bead-id>
	if strings.EqualFold(b.IssueType, "epic") {
		return DefaultEpicBranchPrefix + sanitizeBeadID(b.ID)
	}
	return ""
}

// IsEpicBead returns true if the bead is an epic type.
func IsEpicBead(b Bead) bool {
	return strings.EqualFold(b.IssueType, "epic")
}

// sanitizeBeadID converts a bead ID to a safe branch name component.
func sanitizeBeadID(id string) string {
	r := strings.NewReplacer(
		" ", "-",
		":", "-",
		"\\", "-",
	)
	return r.Replace(id)
}

