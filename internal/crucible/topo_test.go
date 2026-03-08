package crucible

import (
	"testing"

	"github.com/Robin831/Forge/internal/poller"
)

func TestTopoSort_Empty(t *testing.T) {
	sorted, err := TopoSort(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sorted) != 0 {
		t.Fatalf("expected empty, got %d", len(sorted))
	}
}

func TestTopoSort_Single(t *testing.T) {
	beads := []poller.Bead{{ID: "a"}}
	sorted, err := TopoSort(beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sorted) != 1 || sorted[0].ID != "a" {
		t.Fatalf("expected [a], got %v", ids(sorted))
	}
}

func TestTopoSort_NoDeps(t *testing.T) {
	beads := []poller.Bead{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	sorted, err := TopoSort(beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sorted) != 3 {
		t.Fatalf("expected 3, got %d", len(sorted))
	}
}

func TestTopoSort_LinearChain(t *testing.T) {
	// c depends on b, b depends on a → order: a, b, c
	beads := []poller.Bead{
		{ID: "c", DependsOn: []string{"b"}},
		{ID: "a"},
		{ID: "b", DependsOn: []string{"a"}},
	}
	sorted, err := TopoSort(beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := ids(sorted)
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("expected [a b c], got %v", got)
	}
}

func TestTopoSort_DiamondDeps(t *testing.T) {
	// d depends on b and c; b and c depend on a
	beads := []poller.Bead{
		{ID: "d", DependsOn: []string{"b", "c"}},
		{ID: "b", DependsOn: []string{"a"}},
		{ID: "c", DependsOn: []string{"a"}},
		{ID: "a"},
	}
	sorted, err := TopoSort(beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := ids(sorted)
	if got[0] != "a" {
		t.Fatalf("expected a first, got %v", got)
	}
	if got[3] != "d" {
		t.Fatalf("expected d last, got %v", got)
	}
}

func TestTopoSort_ExternalDepsIgnored(t *testing.T) {
	// b depends on a (in group) and ext (external, not in group)
	beads := []poller.Bead{
		{ID: "b", DependsOn: []string{"a", "ext"}},
		{ID: "a"},
	}
	sorted, err := TopoSort(beads)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := ids(sorted)
	if got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected [a b], got %v", got)
	}
}

func TestTopoSort_Cycle(t *testing.T) {
	beads := []poller.Bead{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	}
	_, err := TopoSort(beads)
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestTopoSort_SelfCycle(t *testing.T) {
	beads := []poller.Bead{
		{ID: "a", DependsOn: []string{"a"}},
	}
	_, err := TopoSort(beads)
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func ids(beads []poller.Bead) []string {
	out := make([]string, len(beads))
	for i, b := range beads {
		out[i] = b.ID
	}
	return out
}
