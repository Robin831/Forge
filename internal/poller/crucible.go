package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Robin831/Forge/internal/executil"
)

// ResolveBlocks enriches ready beads with their blocks (children) field by
// calling `bd show <id> --json` for each bead. Lookups are run concurrently
// to avoid adding sequential latency when there are many beads.
//
// This is needed because `bd ready --json` may not include the blocks field.
// All bead types are checked — any bead (feature, task, etc.) can have
// children that need to be resolved.
func ResolveBlocks(ctx context.Context, beads []Bead, anvilPaths map[string]string) {
	type result struct {
		index  int
		blocks []string
	}

	// Identify beads that need resolution.
	needed := make([]int, 0, len(beads))
	for i := range beads {
		b := &beads[i]
		if len(b.Blocks) > 0 {
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
	Status         string `json:"status"`
}

// bdShowResponse is the subset of `bd show --json` output we need to extract
// the blocks (children) of a bead. bd returns "dependents" as an array of
// objects rather than a flat "blocks" string array.
type bdShowResponse struct {
	Dependents []bdShowDependent `json:"dependents"`
}

// OpenChildren asks bd directly for the children of a bead that are not yet
// closed — the "blocks" and "parent-child" dependents `bd show` reports.
//
// It exists because a bead's reconstructed Blocks field answers a narrower
// question: pollAnvil builds it from the beads present in *this poll batch*, so
// an empty Blocks means "no children are ready", which a child blocked on an
// unrelated dependency also produces. Deciding an epic has nothing left to
// orchestrate needs the wider answer, and only bd has it.
//
// An error is returned rather than an empty slice when bd cannot be reached:
// "no children" and "cannot tell" lead to opposite decisions.
func OpenChildren(ctx context.Context, beadID, anvilPath string) ([]string, error) {
	cmd, cancel := executil.BdCommand(ctx, "show", beadID, "--json")
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w: %s", beadID, err, strings.TrimSpace(stderr.String()))
	}

	open, err := parseOpenChildren(output)
	if err != nil {
		return nil, fmt.Errorf("parsing bd show %s: %w", beadID, err)
	}
	return open, nil
}

// parseOpenChildren is OpenChildren minus the subprocess: the filtering of one
// `bd show --json` payload down to the not-yet-closed children.
//
// It is separate because both filters encode an assumption about bd's output
// that only a test can hold still. The dependency-type filter is what keeps a
// plain "depends on" edge from counting as a child — the same distinction
// ParentCandidates makes in the other direction — and the status filter reads
// "closed" as bd's one terminal status, matching every other status comparison
// in Forge (beadclose.go, the merge reconciler). If bd ever reports a finished
// child as something else, an epic whose work is done would be held on every
// poll instead of dispatching, so the vocabulary is pinned in a test rather
// than assumed.
//
// Malformed output is an error, never an empty slice: the caller reads "no
// children" as permission to merge the parent to main.
func parseOpenChildren(output []byte) ([]string, error) {
	var resp bdShowResponse
	if err := json.Unmarshal(unwrapJSONArray(output), &resp); err != nil {
		return nil, err
	}

	var open []string
	for _, dep := range resp.Dependents {
		if dep.DependencyType != "blocks" && dep.DependencyType != "parent-child" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(dep.Status), "closed") {
			continue
		}
		open = append(open, dep.ID)
	}
	return open, nil
}

// lookupBlocks fetches a bead's details and extracts the IDs of beads it blocks.
func lookupBlocks(ctx context.Context, beadID, anvilPath string) []string {
	cmd, cancel := executil.BdCommand(ctx, "show", beadID, "--json")
	defer cancel()
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
		if dep.DependencyType == "blocks" && dep.Status != "closed" {
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
