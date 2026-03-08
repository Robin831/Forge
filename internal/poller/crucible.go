package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// ResolveBlocks enriches ready beads with their blocks (children) field by
// calling `bd show <id> --json` for each bead. Results are cached per poll
// cycle to avoid duplicate calls.
//
// This is needed because `bd ready --json` may not include the blocks field.
// Only beads that are not already epic-type are checked (epics use their own
// flow via ResolveEpicBranches).
func ResolveBlocks(ctx context.Context, beads []Bead, anvilPaths map[string]string) {
	cache := make(map[string][]string) // "anvil:beadID" → blocks

	for i := range beads {
		b := &beads[i]

		// Skip beads that already have blocks populated.
		if len(b.Blocks) > 0 {
			continue
		}

		// Skip epic beads — they have their own dispatch flow.
		if IsEpicBead(*b) {
			continue
		}

		anvilPath, ok := anvilPaths[b.Anvil]
		if !ok {
			continue
		}

		cacheKey := b.Anvil + ":" + b.ID
		if blocks, cached := cache[cacheKey]; cached {
			b.Blocks = blocks
			continue
		}

		blocks := lookupBlocks(ctx, b.ID, anvilPath)
		cache[cacheKey] = blocks
		b.Blocks = blocks
	}
}

// lookupBlocks fetches a bead's details and extracts the blocks field.
func lookupBlocks(ctx context.Context, beadID, anvilPath string) []string {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "show", beadID, "--json"))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		log.Printf("lookupBlocks: bd show %s failed: %v: %s", beadID, err, stderr.String())
		return nil
	}

	var bead Bead
	if err := json.Unmarshal(output, &bead); err != nil {
		return nil
	}

	return bead.Blocks
}
