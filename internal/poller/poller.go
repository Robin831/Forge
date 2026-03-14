// Package poller provides the BeadPoller which discovers ready work across
// registered anvils by invoking 'bd ready --json' in each anvil directory.
package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
)

// Bead represents an issue returned by 'bd ready --json'.
// Only the fields Forge needs are extracted.
type Bead struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Priority     int       `json:"priority"`
	IssueType    string    `json:"issue_type"`
	Assignee     string    `json:"assignee"`
	Parent       string    `json:"parent"`
	Labels       []string  `json:"labels"`
	Blocks         []string  `json:"blocks"`          // Bead IDs that this bead blocks (children)
	DependsOn      []string  `json:"depends_on"`     // Bead IDs that this bead depends on
	Dependencies   []BeadDep `json:"dependencies"`   // Detailed dependency info from bd
	DependentCount int       `json:"dependent_count"` // Number of beads that depend on this bead

	// Forge-injected: which anvil this bead belongs to
	Anvil string `json:"-"`
	// Forge-injected: epic branch name resolved from parent epic's labels.
	// When set, this bead should branch from and PR to this branch instead of main.
	EpicBranch string `json:"-"`
	// Forge-injected: when true, dispatch as standalone — skip epic and crucible detection.
	ForceIndependent bool `json:"-"`
}

// BeadDep represents a dependency entry in the bd JSON output.
type BeadDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"` // "parent-child", "blocks", etc.
}

// AnvilResult holds the poll result for a single anvil.
type AnvilResult struct {
	Name  string
	Beads []Bead
	Err   error
}

// BeadPoller polls registered anvils for ready beads.
type BeadPoller struct {
	anvils map[string]config.AnvilConfig
}

// New creates a BeadPoller for the given anvil configurations.
func New(anvils map[string]config.AnvilConfig) *BeadPoller {
	return &BeadPoller{anvils: anvils}
}

// Poll runs 'bd ready --json' in each anvil directory, merges results,
// and returns them sorted by priority (lowest number = highest priority).
// Errors per-anvil are collected but do not stop other anvils from being polled.
func (p *BeadPoller) Poll(ctx context.Context) ([]Bead, []AnvilResult) {
	results := make([]AnvilResult, 0, len(p.anvils))

	// Poll all anvils concurrently and merge results.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, anvil := range p.anvils {
		wg.Add(1)
		go func(name string, anvil config.AnvilConfig) {
			defer wg.Done()
			beads, err := pollAnvil(ctx, name, anvil)
			mu.Lock()
			results = append(results, AnvilResult{
				Name:  name,
				Beads: beads,
				Err:   err,
			})
			mu.Unlock()
		}(name, anvil)
	}
	wg.Wait()

	// Merge all beads into a unified queue
	var all []Bead
	for _, r := range results {
		all = append(all, r.Beads...)
	}

	// Sort by priority (ascending), then by ID for stability
	sort.Slice(all, func(i, j int) bool {
		if all[i].Priority != all[j].Priority {
			return all[i].Priority < all[j].Priority
		}
		return all[i].ID < all[j].ID
	})

	return all, results
}

// pollAnvil runs 'bd ready --json' in an anvil directory and parses the output.
func pollAnvil(ctx context.Context, name string, anvil config.AnvilConfig) ([]Bead, error) {
	// Build command with timeout
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "ready", "--json"))
	cmd.Dir = anvil.Path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd ready in %s (%s): %w: %s", name, anvil.Path, err, stderr.String())
	}

	var beads []Bead
	if err := json.Unmarshal(output, &beads); err != nil {
		return nil, fmt.Errorf("parsing bd ready output for %s: %w", name, err)
	}

	// Tag each bead with the anvil name
	for i := range beads {
		beads[i].Anvil = name
	}

	// Build Blocks lists from children's Parent references and Dependencies.
	// bd ready returns children with a "parent" field and/or a "dependencies"
	// array, but the parent bead itself may not include a "blocks" array.
	// We reconstruct it here so that IsCrucibleCandidate can detect parents
	// with children.
	beadIdx := make(map[string]int, len(beads))
	for i := range beads {
		beadIdx[beads[i].ID] = i
	}
	blocksSet := make(map[string]map[string]bool) // parent ID -> set of child IDs
	addBlock := func(parentID, childID string) {
		if _, ok := beadIdx[parentID]; !ok {
			return
		}
		if blocksSet[parentID] == nil {
			blocksSet[parentID] = make(map[string]bool)
		}
		blocksSet[parentID][childID] = true
	}
	for _, b := range beads {
		// Parent field directly links child to parent
		if b.Parent != "" {
			addBlock(b.Parent, b.ID)
		}
		// Dependencies array: only "blocks" and "parent-child" types indicate
		// a parent-child relationship. "depends_on" is a sequencing constraint
		// and must NOT be treated as a Blocks edge — doing so would cause the
		// crucible to incorrectly adopt downstream beads as children.
		for _, dep := range b.Dependencies {
			if dep.DependsOnID != "" && dep.DependsOnID != b.ID &&
				(dep.Type == "blocks" || dep.Type == "parent-child") {
				addBlock(dep.DependsOnID, b.ID)
			}
		}
	}
	for parentID, children := range blocksSet {
		idx := beadIdx[parentID]
		// Merge with any existing Blocks from the JSON
		existing := make(map[string]bool, len(beads[idx].Blocks))
		for _, id := range beads[idx].Blocks {
			existing[id] = true
		}
		for childID := range children {
			if !existing[childID] {
				beads[idx].Blocks = append(beads[idx].Blocks, childID)
			}
		}
	}

	// Filter Blocks to only include IDs present in this poll batch.
	// The JSON "blocks" field means "beads I block" (child→parent), but
	// IsCrucibleCandidate needs parent→children. When a child has
	// blocks=[parentID] and the parent is NOT in the ready results (it's
	// blocked), the child would be misidentified as a crucible parent.
	// Only keep Blocks entries that point to beads in the current results.
	for i := range beads {
		if len(beads[i].Blocks) == 0 {
			continue
		}
		var valid []string
		for _, id := range beads[i].Blocks {
			if _, ok := beadIdx[id]; ok {
				valid = append(valid, id)
			}
		}
		beads[i].Blocks = valid
	}

	// Filter out beads that are already assigned (claimed by another agent)
	// or tagged as needing clarification.
	var eligible []Bead
	for _, b := range beads {
		if b.Assignee != "" {
			continue
		}
		if hasClarificationTag(b.Labels) {
			continue
		}
		eligible = append(eligible, b)
	}

	return eligible, nil
}

// hasClarificationTag returns true if the tags list contains the
// "clarification-needed" or "clarification_needed" tag (case-insensitive).
func hasClarificationTag(tags []string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, "clarification-needed") || strings.EqualFold(t, "clarification_needed") {
			return true
		}
	}
	return false
}

// PollSingle polls a single anvil by name and returns its beads.
func (p *BeadPoller) PollSingle(ctx context.Context, name string) ([]Bead, error) {
	anvil, ok := p.anvils[name]
	if !ok {
		return nil, fmt.Errorf("anvil %q not found", name)
	}
	return pollAnvil(ctx, name, anvil)
}

// PollInProgress runs 'bd list --status=in_progress --json' in each anvil directory
// concurrently. It returns all in-progress beads, merged and sorted by priority, along
// with per-anvil results so callers can distinguish "no in-progress beads" from
// "bd list failed" and log errors accordingly.
func (p *BeadPoller) PollInProgress(ctx context.Context) ([]Bead, []AnvilResult) {
	results := make([]AnvilResult, 0, len(p.anvils))

	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, anvil := range p.anvils {
		wg.Add(1)
		go func(name string, anvil config.AnvilConfig) {
			defer wg.Done()
			beads, err := pollInProgressAnvil(ctx, name, anvil)
			mu.Lock()
			results = append(results, AnvilResult{Name: name, Beads: beads, Err: err})
			mu.Unlock()
		}(name, anvil)
	}
	wg.Wait()

	var all []Bead
	for _, r := range results {
		all = append(all, r.Beads...)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Priority != all[j].Priority {
			return all[i].Priority < all[j].Priority
		}
		return all[i].ID < all[j].ID
	})
	return all, results
}

// pollInProgressAnvil runs 'bd list --status=in_progress --json' in one anvil directory.
func pollInProgressAnvil(ctx context.Context, name string, anvil config.AnvilConfig) ([]Bead, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "list", "--status=in_progress", "--json"))
	cmd.Dir = anvil.Path

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list --status=in_progress in %s (%s): %w: %s", name, anvil.Path, err, stderr.String())
	}

	var beads []Bead
	if err := json.Unmarshal(output, &beads); err != nil {
		return nil, fmt.Errorf("parsing bd list output for %s: %w", name, err)
	}

	for i := range beads {
		beads[i].Anvil = name
	}
	return beads, nil
}
