// Package poller provides the BeadPoller which discovers ready work across
// registered anvils by invoking 'bd ready --json' in each anvil directory.
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"bytes"
	"os/exec"
	"sort"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
)

// Bead represents an issue returned by 'bd ready --json'.
// Only the fields Forge needs are extracted.
type Bead struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    int    `json:"priority"`
	IssueType   string `json:"issue_type"`
	Assignee    string `json:"assignee"`
	Parent      string `json:"parent"`

	// Forge-injected: which anvil this bead belongs to
	Anvil string `json:"-"`
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

	// Poll each anvil (sequential for now; can be parallelized later)
	for name, anvil := range p.anvils {
		beads, err := pollAnvil(ctx, name, anvil)
		results = append(results, AnvilResult{
			Name:  name,
			Beads: beads,
			Err:   err,
		})
	}

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

	// Filter out beads that are already assigned (claimed by another agent)
	var unassigned []Bead
	for _, b := range beads {
		if b.Assignee == "" {
			unassigned = append(unassigned, b)
		}
	}

	return unassigned, nil
}

// PollSingle polls a single anvil by name and returns its beads.
func (p *BeadPoller) PollSingle(ctx context.Context, name string) ([]Bead, error) {
	anvil, ok := p.anvils[name]
	if !ok {
		return nil, fmt.Errorf("anvil %q not found", name)
	}
	return pollAnvil(ctx, name, anvil)
}
