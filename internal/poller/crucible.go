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

// bdShowDependent represents a single entry in the "dependents" array
// returned by `bd show --json`. Only entries with dependency_type "blocks"
// indicate children of the bead.
type bdShowDependent struct {
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
}

// bdShowResponse is the subset of `bd show --json` output we need to extract
// the blocks (children) of a bead. bd returns "dependents" as an array of
// objects rather than a flat "blocks" string array.
type bdShowResponse struct {
	Dependents []bdShowDependent `json:"dependents"`
}

// lookupBlocks fetches a bead's details and extracts the IDs of beads it blocks.
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

	// bd show --json may return an array with a single element or a bare object.
	output = unwrapJSONArray(output)

	var resp bdShowResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		log.Printf("lookupBlocks: failed to parse bd show %s: %v", beadID, err)
		return nil
	}

	var blocks []string
	for _, dep := range resp.Dependents {
		if dep.DependencyType == "blocks" {
			blocks = append(blocks, dep.ID)
		}
	}
	return blocks
}

// unwrapJSONArray strips a wrapping JSON array if the output is `[{...}]`,
// returning just `{...}`. bd show --json returns an array with one element.
func unwrapJSONArray(data []byte) []byte {
	data = bytes.TrimSpace(data)
	if len(data) > 1 && data[0] == '[' {
		// Find the first '{' and last '}'
		start := bytes.IndexByte(data, '{')
		end := bytes.LastIndexByte(data, '}')
		if start >= 0 && end > start {
			return data[start : end+1]
		}
	}
	return data
}
