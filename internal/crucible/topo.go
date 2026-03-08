// Package crucible orchestrates parent beads with children on feature branches.
//
// The Crucible is where separate beads are melted together into a single unified PR.
// It auto-detects parent-child bead relationships, creates a feature branch,
// dispatches children in topological order through the pipeline, merges their PRs,
// and produces a final PR against main.
package crucible

import (
	"errors"
	"fmt"

	"github.com/Robin831/Forge/internal/poller"
)

// ErrCycle is returned when the children contain a dependency cycle.
var ErrCycle = errors.New("dependency cycle detected among children")

// TopoSort sorts beads topologically based on their DependsOn relationships.
// Only intra-group dependencies are considered — deps pointing to beads outside
// the input set are ignored. Returns ErrCycle if there's a cycle.
func TopoSort(beads []poller.Bead) ([]poller.Bead, error) {
	if len(beads) == 0 {
		return beads, nil
	}

	// Single bead: fast path unless it depends on itself.
	if len(beads) == 1 {
		for _, dep := range beads[0].DependsOn {
			if dep == beads[0].ID {
				return nil, fmt.Errorf("%w: bead %s depends on itself", ErrCycle, beads[0].ID)
			}
		}
		return beads, nil
	}

	// Build a set of IDs in the input for fast membership checks.
	idSet := make(map[string]struct{}, len(beads))
	byID := make(map[string]poller.Bead, len(beads))
	for _, b := range beads {
		idSet[b.ID] = struct{}{}
		byID[b.ID] = b
	}

	// Build in-degree counts and adjacency list (only intra-group edges).
	// Edge: dep → bead (dep must come before bead).
	inDegree := make(map[string]int, len(beads))
	dependents := make(map[string][]string) // depID → list of beads that depend on it

	for _, b := range beads {
		if _, exists := inDegree[b.ID]; !exists {
			inDegree[b.ID] = 0
		}
		for _, dep := range b.DependsOn {
			if _, inGroup := idSet[dep]; !inGroup {
				continue // external dependency, ignore
			}
			inDegree[b.ID]++
			dependents[dep] = append(dependents[dep], b.ID)
		}
	}

	// Kahn's algorithm: start with nodes that have no intra-group dependencies.
	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	var sorted []poller.Bead
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, byID[id])

		for _, dependent := range dependents[id] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	if len(sorted) != len(beads) {
		return nil, fmt.Errorf("%w: resolved %d of %d children", ErrCycle, len(sorted), len(beads))
	}

	return sorted, nil
}
