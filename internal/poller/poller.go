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
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	IssueType   string   `json:"issue_type"`
	Assignee    string   `json:"assignee"`
	Parent      string   `json:"parent"`
	Labels      []string `json:"labels"`

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
