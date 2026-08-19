package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

const autoCloseAnvil = "forge"

// autoCloseHarness wires a Daemon whose bd show / bd close calls are served
// from in-memory fixtures, so the grouping-parent gates can be exercised
// without a beads database.
type autoCloseHarness struct {
	d *Daemon

	mu       sync.Mutex
	beads    map[string]string // bead ID → `bd show --json` payload
	showErr  map[string]error  // bead ID → error the show hook returns
	closeErr error

	shown  []string
	closed []struct{ id, reason string }
}

func newAutoCloseHarness(t *testing.T) *autoCloseHarness {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	h := &autoCloseHarness{
		beads:   map[string]string{},
		showErr: map[string]error{},
	}
	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			autoCloseAnvil: {Path: t.TempDir()},
		},
	})
	d.beadShower = func(_, beadID string) ([]byte, string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.shown = append(h.shown, beadID)
		if err := h.showErr[beadID]; err != nil {
			return nil, "boom", err
		}
		payload, ok := h.beads[beadID]
		if !ok {
			return nil, "not found", fmt.Errorf("unknown bead %s", beadID)
		}
		return []byte(payload), "", nil
	}
	d.parentCloser = func(_, beadID, reason string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closeErr != nil {
			return h.closeErr
		}
		h.closed = append(h.closed, struct{ id, reason string }{beadID, reason})
		return nil
	}
	h.d = d
	return h
}

func (h *autoCloseHarness) anvilPath() string {
	return h.d.cfg.Load().Anvils[autoCloseAnvil].Path
}

func (h *autoCloseHarness) run(childID string) {
	h.d.maybeCloseGroupingParent(childID, autoCloseAnvil, h.anvilPath())
}

func (h *autoCloseHarness) closedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.closed))
	for _, c := range h.closed {
		out = append(out, c.id)
	}
	return out
}

// childPayload renders a child bead that names parentID both ways bd can.
func childPayload(id, parentID string) string {
	return fmt.Sprintf(`{"id":%q,"status":"closed","parent":%q,
	 "dependencies":[{"issue_id":%q,"depends_on_id":%q,"type":"parent-child"}]}`,
		id, parentID, id, parentID)
}

// parentPayload renders a parent bead with the given labels and children,
// where each child is "id:status".
func parentPayload(id, status string, labels []string, children ...string) string {
	labelJSON := "[]"
	if len(labels) > 0 {
		labelJSON = `["` + strings.Join(labels, `","`) + `"]`
	}
	deps := make([]string, 0, len(children))
	for _, c := range children {
		parts := strings.SplitN(c, ":", 2)
		deps = append(deps, fmt.Sprintf(`{"id":%q,"dependency_type":"parent-child","status":%q}`, parts[0], parts[1]))
	}
	return fmt.Sprintf(`{"id":%q,"status":%q,"labels":%s,"dependents":[%s]}`,
		id, status, labelJSON, strings.Join(deps, ","))
}

func TestMaybeCloseGroupingParent_ClosesWhenAllChildrenClosed(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed", "Forge-c2:closed")

	h.run("Forge-c1")

	require.Len(t, h.closed, 1)
	assert.Equal(t, "Forge-p1", h.closed[0].id)
	assert.Contains(t, h.closed[0].reason, "Forge-c1")
	assert.Contains(t, h.closed[0].reason, "Forge-c2")
	assert.Contains(t, h.closed[0].reason, "all 2 children closed")

	events, err := h.d.db.RecentEvents(10)
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Type == state.EventBeadAutoClosed && e.BeadID == "Forge-p1" {
			found = true
			assert.Contains(t, e.Message, "auto-closed")
		}
	}
	assert.True(t, found, "expected a bead_auto_closed event for the parent")
}

func TestMaybeCloseGroupingParent_KeepsParentWithOpenChild(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed", "Forge-c2:open")

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs(), "a parent with an open child must stay open")
}

func TestMaybeCloseGroupingParent_SkipsOrchestratedParent(t *testing.T) {
	for _, label := range []string{"crucible", "epic-branch:feature/x"} {
		t.Run(label, func(t *testing.T) {
			h := newAutoCloseHarness(t)
			h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
			h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", []string{label}, "Forge-c1:closed")

			h.run("Forge-c1")

			assert.Empty(t, h.closedIDs(), "the Crucible owns an opted-in parent")
		})
	}
}

func TestMaybeCloseGroupingParent_SkipsAlreadyClosedParent(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "closed", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs(), "an already-closed parent must not be closed again")
}

func TestMaybeCloseGroupingParent_NoParent(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed"}`

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Equal(t, []string{"Forge-c1"}, h.shown, "a parentless child must not trigger further bd lookups")
}

func TestMaybeCloseGroupingParent_ParentWithNoChildren(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	// A parent reporting no children at all is a relation read from the wrong
	// side or a stale index — never grounds for a close.
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil)

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
}

func TestMaybeCloseGroupingParent_IgnoresNonChildEdges(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	// The only dependent is a plain "depends on" edge: a sequencing relation,
	// not a child, so the parent has no children to have finished.
	h.beads["Forge-p1"] = `{"id":"Forge-p1","status":"open","dependents":[{"id":"Forge-x","dependency_type":"depends-on","status":"open"}]}`

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
}

func TestMaybeCloseGroupingParent_BdShowFailuresAreSwallowed(t *testing.T) {
	t.Run("child lookup fails", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.showErr["Forge-c1"] = errors.New("dolt is wedged")

		h.run("Forge-c1")

		assert.Empty(t, h.closedIDs())
	})

	t.Run("parent lookup fails", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
		h.showErr["Forge-p1"] = errors.New("dolt is wedged")

		h.run("Forge-c1")

		assert.Empty(t, h.closedIDs())
	})

	t.Run("unparseable parent payload", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
		h.beads["Forge-p1"] = "not json at all"

		h.run("Forge-c1")

		assert.Empty(t, h.closedIDs())
	})

	t.Run("bd close fails", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
		h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed")
		h.closeErr = errors.New("bd close: exit status 1")

		h.run("Forge-c1")

		assert.Empty(t, h.closedIDs())
	})
}

func TestMaybeCloseGroupingParent_ArrayWrappedPayload(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = "[" + childPayload("Forge-c1", "Forge-p1") + "]"
	h.beads["Forge-p1"] = "[" + parentPayload("Forge-p1", "open", nil, "Forge-c1:closed") + "]"

	h.run("Forge-c1")

	assert.Equal(t, []string{"Forge-p1"}, h.closedIDs())
}

// A child reached through a `blocks` dependency edge rather than the parent
// field resolves the same way: it is the edge the poller reads as a parent.
func TestMaybeCloseGroupingParent_ResolvesParentViaBlocksEdge(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-p1","type":"blocks"}]}`
	h.beads["Forge-p1"] = `{"id":"Forge-p1","status":"open","dependents":[{"id":"Forge-c1","dependency_type":"blocks","status":"closed"}]}`

	h.run("Forge-c1")

	assert.Equal(t, []string{"Forge-p1"}, h.closedIDs())
}

// Only one parent is closed per child close: the first candidate that passes
// every gate wins, and the trailing sequencing edges are left alone.
func TestMaybeCloseGroupingParent_ClosesAtMostOneParent(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-p2","type":"blocks"}]}`
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed")
	h.beads["Forge-p2"] = parentPayload("Forge-p2", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Equal(t, []string{"Forge-p1"}, h.closedIDs())
}

// An orchestrated first candidate is skipped rather than ending the walk: the
// Crucible owning one bead says nothing about the next candidate.
func TestMaybeCloseGroupingParent_OrchestratedCandidateDoesNotBlockTheNext(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-p2","type":"blocks"}]}`
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", []string{"crucible"}, "Forge-c1:closed")
	h.beads["Forge-p2"] = parentPayload("Forge-p2", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Equal(t, []string{"Forge-p2"}, h.closedIDs())
}

func TestMaybeCloseGroupingParent_NoHooksWired(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.d.parentCloser = nil
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Empty(t, h.shown, "without a closer there is nothing to look up")
}

func TestAutoCloseParentDetail(t *testing.T) {
	assert.Equal(t, "auto-closed: all 1 child closed (Forge-a)",
		autoCloseParentDetail([]string{"Forge-a"}))
	assert.Equal(t, "auto-closed: all 2 children closed (Forge-a, Forge-b)",
		autoCloseParentDetail([]string{"Forge-a", "Forge-b"}))

	many := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, fmt.Sprintf("Forge-%02d", i))
	}
	detail := autoCloseParentDetail(many)
	assert.Contains(t, detail, "all 12 children closed")
	assert.Contains(t, detail, "+4 more")
	assert.NotContains(t, detail, "Forge-08")
}

// The merge path is the only caller: a child close that lands there must run
// the auto-close, and must still report success when the auto-close does not.
func TestCloseMergedBead_RunsGroupingParentAutoClose(t *testing.T) {
	newDaemon := func(t *testing.T, closeParentErr error) (*Daemon, *[]string) {
		t.Helper()
		d := newCloseTestDaemon(t, &flakyBdCloser{})
		var closed []string
		payloads := map[string]string{
			"Forge-c1": childPayload("Forge-c1", "Forge-p1"),
			"Forge-p1": parentPayload("Forge-p1", "open", nil, "Forge-c1:closed"),
		}
		d.beadShower = func(_, beadID string) ([]byte, string, error) {
			payload, ok := payloads[beadID]
			if !ok {
				return nil, "not found", fmt.Errorf("unknown bead %s", beadID)
			}
			return []byte(payload), "", nil
		}
		d.parentCloser = func(_, beadID, _ string) error {
			if closeParentErr != nil {
				return closeParentErr
			}
			closed = append(closed, beadID)
			return nil
		}
		return d, &closed
	}

	t.Run("closes the parent", func(t *testing.T) {
		d, closed := newDaemon(t, nil)
		err := d.closeMergedBead(context.Background(), "Forge-c1", closeTestAnvil,
			anvilPathFor(t, d), "PR #7 merged", 7, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"Forge-p1"}, *closed)
	})

	t.Run("a failed parent close never fails the merge close", func(t *testing.T) {
		d, closed := newDaemon(t, errors.New("bd close: exit status 1"))
		err := d.closeMergedBead(context.Background(), "Forge-c1", closeTestAnvil,
			anvilPathFor(t, d), "PR #7 merged", 7, nil)
		require.NoError(t, err)
		assert.Empty(t, *closed)
	})
}
