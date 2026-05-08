package forgechat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeBd records every invocation and returns canned responses keyed by the
// first arg ("create" / "close" / etc.). Each call to create returns the
// next id from createIDs (so the test can pre-stage the sequence) and then
// records it for later assertion.
type fakeBd struct {
	mu        sync.Mutex
	calls     [][]string
	createIDs []string
	idIdx     int
	failOn    map[int]error // index into createCalls -> error to return
	createN   int
}

func (f *fakeBd) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := append([]string{"@" + dir}, args...)
	f.calls = append(f.calls, rec)
	if len(args) == 0 {
		return nil, errors.New("fake bd called with no args")
	}
	switch args[0] {
	case "create":
		idx := f.createN
		f.createN++
		if err, ok := f.failOn[idx]; ok {
			return nil, err
		}
		if f.idIdx >= len(f.createIDs) {
			return nil, fmt.Errorf("fake bd: ran out of canned ids at create #%d", f.createN)
		}
		id := f.createIDs[f.idIdx]
		f.idIdx++
		return []byte(fmt.Sprintf(`{"id":"%s","title":"x"}`, id)), nil
	case "close":
		return []byte(`{"closed":true}`), nil
	default:
		return nil, fmt.Errorf("fake bd: unhandled subcommand %q", args[0])
	}
}

// argsFor returns every args slice recorded for invocations whose first arg
// is `verb`. Each returned slice starts with "@<dir>" so tests can also
// assert which anvil dir was used.
func (f *fakeBd) argsFor(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) >= 2 && c[1] == verb {
			out = append(out, c)
		}
	}
	return out
}

func anvilLookup(m map[string]string) AnvilLookup {
	return func(name string) (string, bool) {
		path, ok := m[name]
		return path, ok
	}
}

func TestMaterializeEmission_HappyPathCreatesAndWiresDeps(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p2", Anvil: "alpha", Title: "Two", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
		{ProposalID: "p1", Anvil: "alpha", Title: "One", Type: "feature", Priority: 1},
	}}
	bd := &fakeBd{createIDs: []string{"forge-aaa", "forge-bbb"}}
	res := MaterializeEmission(context.Background(), nil, env,
		anvilLookup(map[string]string{"alpha": "/anvils/alpha"}), bd.run)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("expected 2 created beads, got %d", len(res.Created))
	}
	// p1 must be created before p2 (topological order).
	if res.Created[0].ProposalID != "p1" {
		t.Errorf("expected p1 first, got order %s,%s",
			res.Created[0].ProposalID, res.Created[1].ProposalID)
	}
	// The second `bd create` (for p2) should pass --deps with p1's real id.
	creates := bd.argsFor("create")
	if len(creates) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(creates))
	}
	deps := strings.Join(creates[1], " ")
	if !strings.Contains(deps, "--deps forge-aaa") {
		t.Errorf("expected p2 create to pass --deps forge-aaa, got %q", deps)
	}
	// Both creates should use the anvil's directory.
	if creates[0][0] != "@/anvils/alpha" {
		t.Errorf("expected anvil dir on create, got %q", creates[0][0])
	}
}

func TestMaterializeEmission_RollbackOnFailure(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "One", Type: "task", Priority: 2},
		{ProposalID: "p2", Anvil: "alpha", Title: "Two", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
		{ProposalID: "p3", Anvil: "alpha", Title: "Three", Type: "task", Priority: 2, DependsOn: []string{"p2"}},
	}}
	// Simulate the third create failing.
	bd := &fakeBd{
		createIDs: []string{"forge-aaa", "forge-bbb"},
		failOn:    map[int]error{2: errors.New("simulated bd failure")},
	}
	res := MaterializeEmission(context.Background(), nil, env,
		anvilLookup(map[string]string{"alpha": "/anvils/alpha"}), bd.run)
	if res.Err == nil {
		t.Fatal("expected an error")
	}
	if !res.RolledBack {
		t.Fatal("expected RolledBack=true")
	}
	if len(res.Created) != 2 {
		t.Fatalf("expected 2 successful creates before failure, got %d", len(res.Created))
	}
	closes := bd.argsFor("close")
	if len(closes) != 2 {
		t.Fatalf("expected 2 rollback closes, got %d (%+v)", len(closes), closes)
	}
	// Closes should run in reverse order: p2 (forge-bbb) first, then p1 (forge-aaa).
	if !strings.Contains(strings.Join(closes[0], " "), "forge-bbb") {
		t.Errorf("expected first close to be forge-bbb, got %q", closes[0])
	}
	if !strings.Contains(strings.Join(closes[1], " "), "forge-aaa") {
		t.Errorf("expected second close to be forge-aaa, got %q", closes[1])
	}
	// Reason must mention rollback.
	if !strings.Contains(strings.Join(closes[0], " "), "rollback:") {
		t.Errorf("expected close reason to start with 'rollback:', got %q", closes[0])
	}
}

func TestMaterializeEmission_NilLookupIsRejected(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "a", Title: "x", Type: "task", Priority: 2},
	}}
	res := MaterializeEmission(context.Background(), nil, env, nil, (&fakeBd{}).run)
	if res.Err == nil {
		t.Fatal("expected error when lookup is nil")
	}
}

func TestMaterializeEmission_UnregisteredAnvilFails(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "ghost", Title: "x", Type: "task", Priority: 2},
	}}
	bd := &fakeBd{createIDs: []string{"forge-aaa"}}
	res := MaterializeEmission(context.Background(), nil, env,
		anvilLookup(map[string]string{"alpha": "/anvils/alpha"}), bd.run)
	if res.Err == nil {
		t.Fatal("expected error for unregistered anvil")
	}
	if len(bd.argsFor("create")) != 0 {
		t.Error("expected zero bd create calls for unregistered anvil")
	}
}
