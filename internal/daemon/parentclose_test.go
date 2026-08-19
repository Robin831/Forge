package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

// fakeBead is one row of the fake beads database backing the parent
// auto-close tests: the fields bd reports that the close path reads.
type fakeBead struct {
	status string
	labels []string
	parent string // hierarchical parent, as `bd list --parent` reports it
	// blocks names beads this one merely depends on in sequence. bd renders
	// such an edge exactly like a parent-child one in `bd show`, which is what
	// makes the membership check in closeParentIfComplete load-bearing.
	blocks []string
}

// fakeBd stands in for the three bd invocations the parent auto-close makes:
// `bd show`, `bd list --parent` and `bd close`.
type fakeBd struct {
	mu      sync.Mutex
	beads   map[string]*fakeBead
	showErr map[string]error
	listErr map[string]error
	badShow map[string]bool // return unparseable output for this bead
	closes  []string
	reasons map[string]string
	lists   []string
}

func newFakeBd(beads map[string]*fakeBead) *fakeBd {
	return &fakeBd{
		beads:   beads,
		showErr: map[string]error{},
		listErr: map[string]error{},
		badShow: map[string]bool{},
		reasons: map[string]string{},
	}
}

func (f *fakeBd) show(_, beadID string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.showErr[beadID]; err != nil {
		return nil, "boom", err
	}
	if f.badShow[beadID] {
		return []byte("not json at all"), "", nil
	}
	b, ok := f.beads[beadID]
	if !ok {
		return nil, "no such bead", errors.New("bd show: not found")
	}
	entry := map[string]any{
		"id":     beadID,
		"status": b.status,
		"labels": b.labels,
	}
	var deps []map[string]any
	if b.parent != "" {
		entry["parent"] = b.parent
		// Real bd emits the hierarchy on the child's side twice: the parent
		// field and a parent-child dependency edge.
		deps = append(deps, map[string]any{
			"id": b.parent, "status": f.statusLocked(b.parent), "dependency_type": "parent-child",
		})
	}
	for _, id := range b.blocks {
		deps = append(deps, map[string]any{
			"id": id, "status": f.statusLocked(id), "dependency_type": "blocks",
		})
	}
	entry["dependencies"] = deps
	// bd show --json wraps the bead in a single-element array.
	raw, _ := json.Marshal([]any{entry})
	return raw, "", nil
}

func (f *fakeBd) statusLocked(id string) string {
	if b, ok := f.beads[id]; ok {
		return b.status
	}
	return ""
}

func (f *fakeBd) list(_, parentID string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, parentID)
	if err := f.listErr[parentID]; err != nil {
		return nil, "boom", err
	}
	children := []map[string]any{}
	for id, b := range f.beads {
		if b.parent == parentID {
			children = append(children, map[string]any{"id": id, "status": b.status, "parent": parentID})
		}
	}
	raw, _ := json.Marshal(children)
	return raw, "", nil
}

func (f *fakeBd) close(_ context.Context, beadID, _, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, beadID)
	f.reasons[beadID] = reason
	if b, ok := f.beads[beadID]; ok {
		b.status = "closed"
	}
	return nil
}

func (f *fakeBd) closed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.closes))
	copy(out, f.closes)
	return out
}

func (f *fakeBd) listCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.lists))
	copy(out, f.lists)
	return out
}

// newParentCloseDaemon wires a Daemon against the fake bd with a real state DB.
func newParentCloseDaemon(t *testing.T, bd *fakeBd) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{closeTestAnvil: {Path: t.TempDir()}},
	})
	d.beadShower = bd.show
	d.childLister = bd.list
	d.beadCloser = bd.close
	return d
}

func (d *Daemon) runParentClose(t *testing.T, childID string) {
	t.Helper()
	d.maybeCloseParents(context.Background(), childID, closeTestAnvil, anvilPathFor(t, d))
}

// The headline case: the last open child closes, so the parent closes with a
// reason naming the count and the child that completed it.
func TestMaybeCloseParents_ClosesParentWhenAllChildrenClosed(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "closed", parent: "parent"},
		"child-2": {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "child-2")

	assert.Equal(t, []string{"parent"}, bd.closed())
	assert.Equal(t, "all 2 child beads closed (last: child-2)", bd.reasons["parent"])

	events, err := d.db.RecentEvents(10)
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Type == state.EventParentBeadAutoClosed {
			found = true
			assert.Contains(t, e.Message, "parent")
			assert.Equal(t, "parent", e.BeadID)
		}
	}
	assert.True(t, found, "expected a parent_bead_auto_closed event")
}

// A sibling still open leaves the parent alone — the whole point of the check.
func TestMaybeCloseParents_LeavesParentOpenWithOpenSibling(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "in_progress", parent: "parent"},
		"child-2": {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "child-2")
	assert.Empty(t, bd.closed())
}

// Fail open: a child list that cannot be read is not an empty child list.
func TestMaybeCloseParents_FailsOpenOnListError(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "closed", parent: "parent"},
	})
	bd.listErr["parent"] = errors.New("dolt: i/o timeout")
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "child-1")
	assert.Empty(t, bd.closed())
}

// Fail open: an unreadable or unparseable parent is not a closeable one.
func TestMaybeCloseParents_FailsOpenOnParentShowFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeBd)
	}{
		{"show errors", func(f *fakeBd) { f.showErr["parent"] = errors.New("bd show: connection refused") }},
		{"show unparseable", func(f *fakeBd) { f.badShow["parent"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := newFakeBd(map[string]*fakeBead{
				"parent":  {status: "open"},
				"child-1": {status: "closed", parent: "parent"},
			})
			tc.setup(bd)
			d := newParentCloseDaemon(t, bd)
			d.runParentClose(t, "child-1")
			assert.Empty(t, bd.closed())
		})
	}
}

// Fail open at the first hop too: an unreadable child yields no candidates, so
// nothing is closed and bd is never asked for a child list.
func TestMaybeCloseParents_FailsOpenOnChildShowFailure(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "closed", parent: "parent"},
	})
	bd.showErr["child-1"] = errors.New("bd show: connection refused")
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "child-1")
	assert.Empty(t, bd.closed())
	assert.Empty(t, bd.listCalls())
}

// An orchestrated parent is the Crucible's to close, when its final PR exists.
func TestMaybeCloseParents_SkipsOrchestratedParent(t *testing.T) {
	for _, label := range []string{"crucible", "epic-branch:feature/x"} {
		t.Run(label, func(t *testing.T) {
			bd := newFakeBd(map[string]*fakeBead{
				"parent":  {status: "open", labels: []string{label}},
				"child-1": {status: "closed", parent: "parent"},
			})
			d := newParentCloseDaemon(t, bd)
			d.runParentClose(t, "child-1")
			assert.Empty(t, bd.closed())
		})
	}
}

// An already-closed parent is never closed a second time: a redundant bd close
// is a spurious Dolt commit on every merge.
func TestMaybeCloseParents_SkipsClosedParent(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "closed"},
		"child-1": {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "child-1")
	assert.Empty(t, bd.closed())
}

// A bead with no parent asks bd nothing beyond its own show.
func TestMaybeCloseParents_NoParentIsANoOp(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{"lonely": {status: "closed"}})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "lonely")
	assert.Empty(t, bd.closed())
	assert.Empty(t, bd.listCalls())
}

// The membership check: a `blocks` sequencing edge makes its predecessor a
// parent CANDIDATE, but bd's hierarchy does not list the closing bead as its
// child — so finishing the last bead of a chain must not close the one before
// it, whose own children are all closed.
func TestMaybeCloseParents_IgnoresBlocksPredecessor(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"earlier":     {status: "open"},
		"earlier-kid": {status: "closed", parent: "earlier"},
		"later":       {status: "closed", blocks: []string{"earlier"}},
	})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "later")
	assert.Empty(t, bd.closed())
}

// Closing a parent can complete its own parent, so the walk continues up.
func TestMaybeCloseParents_WalksUpTheChain(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"grandparent": {status: "open"},
		"parent":      {status: "open", parent: "grandparent"},
		"child-1":     {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "child-1")
	assert.Equal(t, []string{"parent", "grandparent"}, bd.closed())
}

// A cycle in bd's parent data must not spin forever.
func TestMaybeCloseParents_BoundedByDepthLimit(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"a": {status: "open", parent: "b"},
		"b": {status: "open", parent: "a"},
	})
	d := newParentCloseDaemon(t, bd)
	d.runParentClose(t, "a")
	assert.LessOrEqual(t, len(bd.closed()), maxParentCloseDepth)
}

// Two siblings closing at once must not both close the parent.
func TestMaybeCloseParents_ConcurrentSiblingsCloseParentOnce(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "closed", parent: "parent"},
		"child-2": {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	path := anvilPathFor(t, d)

	var wg sync.WaitGroup
	for _, child := range []string{"child-1", "child-2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			d.maybeCloseParents(context.Background(), id, closeTestAnvil, path)
		}(child)
	}
	wg.Wait()
	assert.Equal(t, []string{"parent"}, bd.closed())
}

// The opt-out is checked before any bd call, so disabling it costs nothing.
func TestMaybeCloseParents_DisabledByConfig(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	off := false
	cfg := d.cfg.Load()
	cfg.Settings.AutoCloseParents = &off
	d.cfg.Store(cfg)

	d.runParentClose(t, "child-1")
	assert.Empty(t, bd.closed())
	assert.Empty(t, bd.listCalls())
}

// Nothing runs when the seams are missing (a Daemon built by a test that never
// wired bd), rather than panicking on a nil call.
func TestMaybeCloseParents_NoSeamsIsANoOp(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "closed", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	d.childLister = nil
	d.runParentClose(t, "child-1")
	assert.Empty(t, bd.closed())
}

// The merge path is the trigger: closing a child after its PR merges closes the
// parent it completed, in the same call.
func TestCloseMergedBead_ClosesCompletedParent(t *testing.T) {
	bd := newFakeBd(map[string]*fakeBead{
		"parent":  {status: "open"},
		"child-1": {status: "open", parent: "parent"},
	})
	d := newParentCloseDaemon(t, bd)
	require.NoError(t, d.closeMergedBead(context.Background(), "child-1", closeTestAnvil,
		anvilPathFor(t, d), "PR #12 merged", 12, nil))
	assert.Equal(t, []string{"child-1", "parent"}, bd.closed())
}
