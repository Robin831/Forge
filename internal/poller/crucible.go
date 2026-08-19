package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Robin831/Forge/internal/epic"
	"github.com/Robin831/Forge/internal/executil"
)

// ResolveBlocks enriches ready beads with their blocks (children) field by
// calling `bd show <id> --json` for each bead. Lookups are run concurrently
// to avoid adding sequential latency when there are many beads.
//
// This is needed because `bd ready --json` may not include the blocks field.
// All bead types are checked — any bead (feature, task, etc.) can have
// children that need to be resolved.
//
// Each lookup pays for executil.BdIncludeDependentsFlag, which bd documents as
// "may be slow on hub beads" because it streams every dependent's record rather
// than a count — and this is the widest fan-out of it in Forge, one goroutine
// per ready bead with no Blocks. It is bounded by the ready-bead count of a
// single poll cycle, so it is left unlimited; the cost is recorded on
// executil.BdShowDependentsArgs.
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
// returned by `bd show --json`. Only entries whose dependency_type
// isChildDependency accepts ("blocks" or "parent-child") indicate children of
// the bead.
//
// It deliberately carries no Labels field. A dependents entry is an edge
// summary, not a bead record: `bd show --json` (verified against bd 1.1.2)
// reports id, title, status, priority, issue_type, timestamps and
// dependency_type there, and nothing else — labels appear only on a bead's own
// top-level record. Reading dep.Labels would therefore have been permanently
// nil, making the "independent" opt-out silently inert on exactly the two paths
// that decide whether a parent still has an epic to orchestrate. The labels are
// resolved by a second lookup instead (resolveIndependent).
type bdShowDependent struct {
	ID             string `json:"id"`
	DependencyType string `json:"dependency_type"`
	Status         string `json:"status"`
}

// bdShowResponse is the subset of `bd show --json` output we need to extract
// the blocks (children) of a bead. bd returns "dependents" as an array of
// objects rather than a flat "blocks" string array.
//
// The array is only there when the show was run with
// executil.BdIncludeDependentsFlag: bd (1.1.2) reports `dependent_count`
// unflagged and omits `dependents` entirely, so an unflagged show decodes into
// a zero-value struct here and every reader concludes the bead has no children.
// Both readers therefore go through executil.BdShowDependents.
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
// Children that opted out of the epic ("independent") are not counted: their
// work never lands on the parent's feature branch, so an open one says nothing
// about whether the epic still has something to orchestrate.
//
// An error is returned rather than an empty slice when bd cannot be reached:
// "no children" and "cannot tell" lead to opposite decisions.
func OpenChildren(ctx context.Context, beadID, anvilPath string) ([]string, error) {
	cmd, cancel := executil.BdShowDependents(ctx, beadID)
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		err = executil.ClassifyBdShowError(err, stderr.String())
		return nil, fmt.Errorf("bd show %s: %w: %s", beadID, err, strings.TrimSpace(stderr.String()))
	}

	candidates, err := parseOpenChildren(output)
	if err != nil {
		return nil, fmt.Errorf("parsing bd show %s: %w", beadID, err)
	}
	return dropIndependent(ctx, candidates, anvilPath), nil
}

// parseOpenChildren is OpenChildren minus the subprocess: the filtering of one
// `bd show --json` payload down to the not-yet-closed children. The per-child
// opt-out is NOT applied here — it needs labels this payload does not carry, so
// OpenChildren applies it afterwards via dropIndependent.
//
// It is separate because both filters encode an assumption about bd's output
// that only a test can hold still. The dependency-type filter is what keeps a
// plain "depends on" edge from counting as a child — the same distinction
// ParentCandidates makes in the other direction — and the status filter reads
// "closed" as bd's one terminal status, matching every other status comparison
// in Forge (beadclose.go, the merge reconciler). If bd ever reports a finished
// child as something else, an epic whose work is done would be held on every
// poll instead of dispatching, so the vocabulary is pinned in a test rather
// than assumed. Both halves of it — isChildDependency and isClosedStatus — are
// shared with lookupBlocks, so neither the set of edges that makes a dependent
// a child nor the status that makes it finished can be read one way by one
// reader of this payload and another way by the other.
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
		if !isChildDependency(dep.DependencyType) {
			continue
		}
		if isClosedStatus(dep.Status) {
			continue
		}
		open = append(open, dep.ID)
	}
	return open, nil
}

// isClosedStatus reads bd's one terminal status, trimmed and case-insensitively.
// Both readers of a dependents payload call it: lookupBlocks used to compare
// `!= "closed"` verbatim while parseOpenChildren folded and trimmed, so a
// dependent reported as "Closed" was a child to one gate and not to the other —
// the same per-path divergence the label normalisation exists to prevent.
func isClosedStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "closed")
}

// isChildDependency reads the dependency types that make a dependents entry a
// child. It is the other half of the shared vocabulary, and for the same
// reason: lookupBlocks used to count only "blocks" while parseOpenChildren
// counted "blocks" and "parent-child", so a family linked purely by
// parent-child edges was held open by OpenChildren (the parent escalates rather
// than dispatching) while its Blocks stayed empty — IsCrucibleCandidate never
// fired, and the epic could not orchestrate its way out of the hold. The wider
// pair is the correct one at both sites: it is what pollAnvil reconstructs
// Blocks from when child and parent are in the same poll batch, so the fallback
// lookup now answers the way the batch path already did.
//
// "depends_on" is deliberately excluded at every site: it is a sequencing
// constraint, and reading it as a child edge would have the Crucible adopt
// downstream beads.
func isChildDependency(depType string) bool {
	return depType == "blocks" || depType == "parent-child"
}

// lookupBlocks fetches a bead's details and extracts the IDs of beads it blocks.
//
// Children that opted out of the epic ("independent") are excluded: Blocks is
// what every orchestration gate reads as "children to orchestrate"
// (IsCrucibleCandidate, crucibleOwnedChildren, the Crucible's own child count),
// and an opted-out child is none of those — the same exclusion pollAnvil applies
// when it rebuilds Blocks from a poll batch. The labels come from a second
// lookup rather than from the dependents payload, which does not carry them.
func lookupBlocks(ctx context.Context, beadID, anvilPath string) []string {
	cmd, cancel := executil.BdShowDependents(ctx, beadID)
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		// Classified before logging: this site swallows its error and returns
		// nil, which every orchestration gate reads as "no children". A bd too
		// old for the flag would therefore be indistinguishable from an epic
		// with nothing in it, so the log line has to name the cause.
		log.Printf("lookupBlocks: bd show %s failed: %v: %s",
			beadID, executil.ClassifyBdShowError(err, stderr.String()), stderr.String())
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
		if isChildDependency(dep.DependencyType) && !isClosedStatus(dep.Status) {
			blocks = append(blocks, dep.ID)
		}
	}
	return dropIndependent(ctx, blocks, anvilPath)
}

// dropIndependent removes the children that carry the "independent" label from
// a list of child IDs read out of a `bd show` dependents array.
//
// It exists because that array is an edge summary: bd reports a dependent's id,
// title, status, priority, issue_type and dependency_type — never its labels,
// which live only on a bead's own record. Both readers of the array
// (parseOpenChildren via OpenChildren, and lookupBlocks) therefore have to ask
// bd a second time, and they ask through this one function so the two cannot
// answer the question differently. crucible.FetchChildren needs no equivalent:
// it already fetches each child's own record and reads the labels off it.
//
// The lookup is one subprocess for the whole list — `bd show a b c --json`
// returns an array of full records — so a parent with children costs one extra
// bd call per poll and a bead with none costs nothing.
//
// A child whose labels cannot be read is kept, which is the conservative
// direction at both call sites and is why the two can share this function: for
// OpenChildren keeping it holds the parent for an operator rather than closing
// an epic whose children are still open, and for lookupBlocks keeping it leaves
// the parent a Crucible candidate rather than dropping a child that was never
// opted out. Neither error path can invent an opt-out that was not there.
func dropIndependent(ctx context.Context, childIDs []string, anvilPath string) []string {
	if len(childIDs) == 0 {
		return childIDs
	}

	independent, err := resolveIndependent(ctx, childIDs, anvilPath)
	if err != nil {
		log.Printf("dropIndependent: cannot read labels for %v: %v (treating them as ordinary children)", childIDs, err)
		return childIDs
	}

	kept := make([]string, 0, len(childIDs))
	for _, id := range childIDs {
		if independent[id] {
			continue
		}
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// resolveIndependent reads the labels of each named bead from its own record and
// reports which of them carry the "independent" opt-out.
//
// `bd show` takes several ids and answers with an array of records, so this is
// one subprocess however many children there are. A bead missing from the answer
// is absent from the map and so counts as an ordinary child — the same
// conservative direction dropIndependent takes for an outright failure.
//
// The ids are values Forge did not write (they come out of a dolt database that
// syncs through the git remote), so they go to bd as `--id=<id>` flags rather
// than positionally — executil.BdShowArgs is the one builder that decides that,
// shared with the dependents-array shape, so an id that would otherwise be read
// as a flag is named explicitly here and there alike.
func resolveIndependent(ctx context.Context, beadIDs []string, anvilPath string) (map[string]bool, error) {
	if len(executil.BdShowIDArgs(beadIDs...)) == 0 {
		return map[string]bool{}, nil
	}

	cmd, cancel := executil.BdCommand(ctx, executil.BdShowArgs(beadIDs...)...)
	defer cancel()
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w: %s", strings.Join(beadIDs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return parseIndependent(output)
}

// parseIndependent is resolveIndependent minus the subprocess: the reading of a
// `bd show <ids...> --json` payload down to "which of these opted out".
//
// bd answers a multi-id show with a JSON array, and a single-id show with an
// array of one — unwrapJSONArray is no help here (it collapses to the first
// element), so the array form is decoded as an array and a bare object is
// accepted as the one-element case.
func parseIndependent(output []byte) (map[string]bool, error) {
	type record struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}

	trimmed := bytes.TrimSpace(output)
	var records []record
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &records); err != nil {
			return nil, err
		}
	} else {
		var one record
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, err
		}
		records = []record{one}
	}

	out := make(map[string]bool, len(records))
	for _, r := range records {
		if r.ID != "" && epic.IsIndependent(r.Labels) {
			out[r.ID] = true
		}
	}
	return out, nil
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
