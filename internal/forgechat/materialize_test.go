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

// TestMaterializeEmission_RollbackSurvivesCancelledParentContext is the
// regression test for the "rollback uses caller's possibly-cancelled context"
// bug. We arrange the parent ctx to already be cancelled by the time
// rollback runs (mimicking an HTTP client disconnect that aborts mid-flight)
// and assert that every previously-created bead is still closed.
func TestMaterializeEmission_RollbackSurvivesCancelledParentContext(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "One", Type: "task", Priority: 2},
		{ProposalID: "p2", Anvil: "alpha", Title: "Two", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
	}}

	parentCtx, cancelParent := context.WithCancel(context.Background())

	// On the second create: cancel the parent ctx, then return an error.
	// This simulates "the HTTP request was aborted while bd was working"
	// — exactly the case the original code regressed on, where rollback
	// would inherit the cancelled ctx and silently no-op.
	bd := &cancelOnSecondBd{
		canned: []string{"forge-aaa"},
		cancel: cancelParent,
		failOn: 1,
		err:    errors.New("cancelled mid-create"),
	}

	res := MaterializeEmission(parentCtx, nil, env,
		anvilLookup(map[string]string{"alpha": "/anvils/alpha"}), bd.run)
	if res.Err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if !res.RolledBack {
		t.Fatal("expected RolledBack=true even when parent ctx was cancelled")
	}
	if len(res.Created) != 1 {
		t.Fatalf("expected one create to have succeeded before cancel, got %d", len(res.Created))
	}
	closes := bd.argsFor("close")
	if len(closes) != 1 {
		t.Fatalf("expected one rollback close to run despite cancelled parent ctx, got %d (%+v)", len(closes), closes)
	}
	if !strings.Contains(strings.Join(closes[0], " "), "forge-aaa") {
		t.Errorf("expected close to target forge-aaa, got %q", closes[0])
	}
	if res.RollbackError != nil {
		t.Errorf("rollback should have completed cleanly, got %v", res.RollbackError)
	}
}

// cancelOnSecondBd is a fakeBd variant that cancels a context just before
// returning the configured error on a chosen create call.
type cancelOnSecondBd struct {
	mu      sync.Mutex
	calls   [][]string
	canned  []string
	idx     int
	createN int
	failOn  int
	err     error
	cancel  context.CancelFunc
}

func (f *cancelOnSecondBd) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{"@" + dir}, args...))
	if len(args) == 0 {
		return nil, errors.New("fake bd: no args")
	}
	switch args[0] {
	case "create":
		idx := f.createN
		f.createN++
		if idx == f.failOn {
			// Cancel the parent ctx, then fail. The next call (rollback)
			// will see ctx.Err() != nil unless the materializer detaches it.
			if f.cancel != nil {
				f.cancel()
			}
			return nil, f.err
		}
		if f.idx >= len(f.canned) {
			return nil, fmt.Errorf("fake bd: out of canned ids")
		}
		id := f.canned[f.idx]
		f.idx++
		return []byte(fmt.Sprintf(`{"id":"%s"}`, id)), nil
	case "close":
		// Refuse to honour a cancelled ctx — that's exactly what would
		// happen with the real bd subprocess, which is the behaviour we're
		// regression-testing for.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []byte(`{"closed":true}`), nil
	default:
		return nil, fmt.Errorf("fake bd: unhandled %q", args[0])
	}
}

func (f *cancelOnSecondBd) argsFor(verb string) [][]string {
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

func TestMaterializeEmission_EmptyAnvilPathRejected(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "x", Type: "task", Priority: 2},
	}}
	bd := &fakeBd{createIDs: []string{"forge-aaa"}}
	// Lookup returns ok=true but with an empty path — should be rejected
	// rather than running bd in the daemon's cwd.
	lookup := func(name string) (string, bool) { return "", true }
	res := MaterializeEmission(context.Background(), nil, env, lookup, bd.run)
	if res.Err == nil {
		t.Fatal("expected error when lookup returns empty path")
	}
	if len(bd.argsFor("create")) != 0 {
		t.Errorf("expected zero bd calls when path is empty, got %d", len(bd.argsFor("create")))
	}
}

func TestMaterializeEmission_EmptyEnvelopeFails(t *testing.T) {
	bd := &fakeBd{}
	res := MaterializeEmission(context.Background(), nil, nil,
		anvilLookup(map[string]string{}), bd.run)
	if res.Err == nil {
		t.Fatal("expected error for nil envelope")
	}
	res = MaterializeEmission(context.Background(), nil, &EmissionEnvelope{},
		anvilLookup(map[string]string{}), bd.run)
	if res.Err == nil {
		t.Fatal("expected error for empty envelope")
	}
}

func TestMaterializeEmission_MultiAnvilHappyPath(t *testing.T) {
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "A", Type: "task", Priority: 2},
		{ProposalID: "p2", Anvil: "beta", Title: "B", Type: "feature", Priority: 1},
	}}
	bd := &fakeBd{createIDs: []string{"alpha-1", "beta-1"}}
	res := MaterializeEmission(context.Background(), nil, env,
		anvilLookup(map[string]string{"alpha": "/anvils/alpha", "beta": "/anvils/beta"}), bd.run)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Created) != 2 {
		t.Fatalf("expected 2 creates, got %d", len(res.Created))
	}
	// Each bd create must run in its bead's anvil directory so the right
	// dolt database is targeted.
	dirs := map[string]string{}
	for _, c := range bd.argsFor("create") {
		// args layout: ["@<dir>", "create", "--title", "X", ...]
		var title string
		for i := 1; i < len(c)-1; i++ {
			if c[i] == "--title" {
				title = c[i+1]
				break
			}
		}
		dirs[title] = c[0]
	}
	if dirs["A"] != "@/anvils/alpha" {
		t.Errorf("bead A should run in alpha dir, got %q", dirs["A"])
	}
	if dirs["B"] != "@/anvils/beta" {
		t.Errorf("bead B should run in beta dir, got %q", dirs["B"])
	}
}

func TestParseCreatedID_AcceptsObjectAndArray(t *testing.T) {
	id, err := parseCreatedID([]byte(`{"id":"forge-aaa","title":"x"}`))
	if err != nil {
		t.Fatalf("object form: %v", err)
	}
	if id != "forge-aaa" {
		t.Errorf("object form: got %q", id)
	}

	id, err = parseCreatedID([]byte(`[{"id":"forge-bbb"}]`))
	if err != nil {
		t.Fatalf("array form: %v", err)
	}
	if id != "forge-bbb" {
		t.Errorf("array form: got %q", id)
	}
}

func TestParseCreatedID_ToleratesTrailingDiagnostics(t *testing.T) {
	// Some bd builds emit follow-up diagnostic lines after the JSON object
	// (orphan detection, sync hints, etc.). The decoder must ignore them.
	out := []byte("{\"id\":\"forge-aaa\",\"title\":\"x\"}\nWARN: sync skipped\n")
	id, err := parseCreatedID(out)
	if err != nil {
		t.Fatalf("trailing diagnostics: %v", err)
	}
	if id != "forge-aaa" {
		t.Errorf("got %q", id)
	}
}

func TestParseCreatedID_RejectsMissingID(t *testing.T) {
	if _, err := parseCreatedID([]byte(`{"title":"x"}`)); err == nil {
		t.Fatal("expected error when bd output has no id")
	}
	if _, err := parseCreatedID([]byte(`[]`)); err == nil {
		t.Fatal("expected error when bd output is an empty array")
	}
	if _, err := parseCreatedID([]byte(`not json at all`)); err == nil {
		t.Fatal("expected error when output is unparseable")
	}
}

func TestMaterializeEmission_DefaultsBlankTypeToTask(t *testing.T) {
	// If a caller skips ValidateEmission (which normalises blank Type to
	// "task"), buildCreateArgs must still pass a sensible --type to bd.
	env := &EmissionEnvelope{Beads: []BeadProposal{
		{ProposalID: "p1", Anvil: "alpha", Title: "x", Type: "", Priority: 2},
	}}
	bd := &fakeBd{createIDs: []string{"forge-aaa"}}
	res := MaterializeEmission(context.Background(), nil, env,
		anvilLookup(map[string]string{"alpha": "/anvils/alpha"}), bd.run)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	creates := bd.argsFor("create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(creates))
	}
	joined := strings.Join(creates[0], " ")
	if !strings.Contains(joined, "--type task") {
		t.Errorf("expected blank type to default to task, got args %q", joined)
	}
}
