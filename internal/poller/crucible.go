package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// ResolveBlocks enriches ready beads with their blocks (children) field by
// calling `bd show <id> --json` for each bead. Lookups are run concurrently
// to avoid adding sequential latency when there are many beads.
//
// This is needed because `bd ready --json` may not include the blocks field.
// Only beads that are not already epic-type are checked (epics use their own
// flow via ResolveEpicBranches).
func ResolveBlocks(ctx context.Context, beads []Bead, anvilPaths map[string]string) {
	type result struct {
		index  int
		blocks []string
	}

	// Identify beads that need resolution and collect unique lookups.
	type lookupKey struct {
		anvil  string
		beadID string
	}
	needed := make([]int, 0, len(beads))
	for i := range beads {
		b := &beads[i]
		if len(b.Blocks) > 0 {
			continue
		}
		if IsEpicBead(*b) {
			continue
		}
		if _, ok := anvilPaths[b.Anvil]; !ok {
			continue
		}
		needed = append(needed, i)
	}

	if len(needed) == 0 {
		return
	}

	results := make([]result, len(needed))
	var wg sync.WaitGroup
	wg.Add(len(needed))

	for j, i := range needed {
		j, i := j, i // capture loop vars
		go func() {
			defer wg.Done()
			b := beads[i]
			anvilPath := anvilPaths[b.Anvil]
			blocks := lookupBlocks(ctx, b.ID, anvilPath)
			results[j] = result{index: i, blocks: blocks}
		}()
	}

	wg.Wait()

	for _, r := range results {
		beads[r.index].Blocks = r.blocks
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
