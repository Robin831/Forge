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

	mu        sync.Mutex
	beads     map[string]string // bead ID → `bd show --json` payload
	showErr   map[string]error  // bead ID → error the show hook returns
	closeErr  error
	closeHook func(beadID string) // called inside parentCloser, outside the lock

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
		hook, closeErr := h.closeHook, h.closeErr
		h.mu.Unlock()
		// The hook runs unlocked so a test can park a goroutine inside the
		// close while a sibling goroutine keeps reading fixtures.
		if hook != nil {
			hook(beadID)
		}
		if closeErr != nil {
			return closeErr
		}
		h.mu.Lock()
		defer h.mu.Unlock()
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

// shownIDs is the list of beads bd show was called for, in order.
func (h *autoCloseHarness) shownIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.shown...)
}

// autoClosedEvents returns the bead_auto_closed event messages recorded so far.
func (h *autoCloseHarness) autoClosedEvents(t *testing.T) []string {
	t.Helper()
	events, err := h.d.db.RecentEvents(50)
	require.NoError(t, err)
	var out []string
	for _, e := range events {
		if e.Type == state.EventBeadAutoClosed {
			out = append(out, e.Message)
		}
	}
	return out
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
	assert.Equal(t, "auto-closed: all 2 children closed (Forge-c1, Forge-c2)", h.closed[0].reason)

	require.Equal(t,
		[]string{"Parent Forge-p1 auto-closed: all 2 children closed (Forge-c1, Forge-c2)"},
		h.autoClosedEvents(t),
		"the event supplies its own framing, so the detail must not repeat the prefix")
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
	// Either hook missing takes the same early return: the lookup hook is as
	// load-bearing as the closer.
	for _, name := range []string{"parentCloser", "beadShower"} {
		t.Run(name, func(t *testing.T) {
			h := newAutoCloseHarness(t)
			if name == "parentCloser" {
				h.d.parentCloser = nil
			} else {
				h.d.beadShower = nil
			}
			h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
			h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed")

			h.run("Forge-c1")

			assert.Empty(t, h.closedIDs())
			assert.Empty(t, h.shownIDs(), "without both hooks there is nothing to look up")
		})
	}
}

// A candidate that lists children, but not the one that just closed, is some
// other bead's parent: the walk moves past it to the real one.
func TestMaybeCloseGroupingParent_SkipsCandidateThatDoesNotListTheChild(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-p2","type":"blocks"}]}`
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-other:closed")
	h.beads["Forge-p2"] = parentPayload("Forge-p2", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Equal(t, []string{"Forge-p2"}, h.closedIDs())
}

// A parent someone already closed ends the walk. Continuing would evaluate the
// trailing `blocks` sequencing candidates and close a bead that was never this
// child's parent.
func TestMaybeCloseGroupingParent_ClosedParentEndsTheWalk(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-x","type":"blocks"}]}`
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "closed", nil, "Forge-c1:closed")
	h.beads["Forge-x"] = parentPayload("Forge-x", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
	assert.NotContains(t, h.shownIDs(), "Forge-x",
		"the sequencing candidate behind a closed parent must not even be looked at")
}

// The same holds while the parent is merely *not yet* closable: an open
// sibling is a reason to stop, not a reason to look one edge further out.
func TestMaybeCloseGroupingParent_OpenSiblingEndsTheWalk(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-x","type":"blocks"}]}`
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed", "Forge-c2:open")
	h.beads["Forge-x"] = parentPayload("Forge-x", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
	assert.NotContains(t, h.shownIDs(), "Forge-x")
}

// A `bd close` that reports the parent as already closed is the world state
// the walk was heading for, so it ends there: no event, no second candidate.
func TestMaybeCloseGroupingParent_AlreadyClosedCloseErrorEndsTheWalk(t *testing.T) {
	for _, msg := range []string{
		"bd close Forge-p1: issue Forge-p1 is already closed",
		"bd close: ALREADY_CLOSED",
	} {
		t.Run(msg, func(t *testing.T) {
			h := newAutoCloseHarness(t)
			h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
			 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-x","type":"blocks"}]}`
			h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed")
			h.beads["Forge-x"] = parentPayload("Forge-x", "open", nil, "Forge-c1:closed")
			h.closeErr = errors.New(msg)

			h.run("Forge-c1")

			assert.Empty(t, h.closedIDs())
			assert.Empty(t, h.autoClosedEvents(t), "nothing closed here, so nothing to announce")
			assert.NotContains(t, h.shownIDs(), "Forge-x",
				"an already-closed parent is handled, not a reason to try a sequencing edge")
		})
	}
}

// A generic bd failure is the parent's problem, not the next candidate's: the
// walk ends at the parent it identified.
func TestMaybeCloseGroupingParent_FailedCloseEndsTheWalk(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-x","type":"blocks"}]}`
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed")
	h.beads["Forge-x"] = parentPayload("Forge-x", "open", nil, "Forge-c1:closed")
	h.closeErr = errors.New("bd close: exit status 1")

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
	assert.NotContains(t, h.shownIDs(), "Forge-x")
}

// An unreadable candidate leaves the relationship unknown, and an unanswered
// question is never grounds for closing the bead one edge further out.
func TestMaybeCloseGroupingParent_UnreadableCandidateEndsTheWalk(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = `{"id":"Forge-c1","status":"closed","parent":"Forge-p1",
	 "dependencies":[{"issue_id":"Forge-c1","depends_on_id":"Forge-x","type":"blocks"}]}`
	h.showErr["Forge-p1"] = errors.New("dolt is wedged")
	h.beads["Forge-x"] = parentPayload("Forge-x", "open", nil, "Forge-c1:closed")

	h.run("Forge-c1")

	assert.Empty(t, h.closedIDs())
	assert.NotContains(t, h.shownIDs(), "Forge-x")
}

// Two siblings whose PRs merge in the same cycle both reach the parent with a
// fully closed sibling set. Exactly one close must land — and the loser must
// end its walk there rather than falling through to a sequencing candidate.
func TestMaybeCloseGroupingParent_ConcurrentSiblingsCloseOnce(t *testing.T) {
	h := newAutoCloseHarness(t)
	for _, child := range []string{"Forge-c1", "Forge-c2"} {
		h.beads[child] = fmt.Sprintf(`{"id":%q,"status":"closed","parent":"Forge-p1",
		 "dependencies":[{"issue_id":%q,"depends_on_id":"Forge-x","type":"blocks"}]}`, child, child)
	}
	h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed", "Forge-c2:closed")
	// The trailing candidate: open, and its own dependents all closed, so it
	// would pass every gate if the loser ever got to it.
	h.beads["Forge-x"] = parentPayload("Forge-x", "open", nil, "Forge-c1:closed", "Forge-c2:closed")

	// The winner parks inside bd close, which guarantees the other goroutine
	// meets the in-flight guard rather than a finished close.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var park sync.Once
	h.closeHook = func(string) {
		// Only the first close parks. A regression that let the loser walk on
		// to Forge-x must fail the assertions below rather than deadlock here.
		parked := false
		park.Do(func() { parked = true })
		if !parked {
			return
		}
		entered <- struct{}{}
		<-release
	}

	done := make(chan struct{}, 2)
	for _, child := range []string{"Forge-c1", "Forge-c2"} {
		go func() {
			defer func() { done <- struct{}{} }()
			h.run(child)
		}()
	}

	<-entered // one goroutine holds the in-flight key and is inside bd close
	<-done    // the other has finished its whole walk
	close(release)
	<-done

	assert.Equal(t, []string{"Forge-p1"}, h.closedIDs(), "the parent closes exactly once")
	assert.NotContains(t, h.shownIDs(), "Forge-x",
		"the losing sibling must stop at the parent, not walk on to a sequencing edge")
	assert.Len(t, h.autoClosedEvents(t), 1)
}

// bd's own output capitalizes and pads status strings in places, so the gates
// compare them case- and whitespace-insensitively.
func TestMaybeCloseGroupingParent_StatusComparisonIsCaseAndSpaceTolerant(t *testing.T) {
	t.Run("padded uppercase parent status counts as closed", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
		h.beads["Forge-p1"] = parentPayload("Forge-p1", " CLOSED ", nil, "Forge-c1:closed")

		h.run("Forge-c1")

		assert.Empty(t, h.closedIDs(), "an already-closed parent must not be closed again")
	})

	t.Run("padded uppercase child status counts as closed", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
		h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1: CLOSED ")

		h.run("Forge-c1")

		assert.Equal(t, []string{"Forge-p1"}, h.closedIDs())
	})

	t.Run("padded child status that is not closed keeps the parent open", func(t *testing.T) {
		h := newAutoCloseHarness(t)
		h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
		h.beads["Forge-p1"] = parentPayload("Forge-p1", "open", nil, "Forge-c1:closed", "Forge-c2: OPEN ")

		h.run("Forge-c1")

		assert.Empty(t, h.closedIDs())
	})
}

// bd IDs are whatever the Dolt database carries, and they land in a persisted
// close reason and a rendered activity feed — so they are stripped like any
// other text Forge did not write.
func TestMaybeCloseGroupingParent_SanitizesBdSuppliedText(t *testing.T) {
	h := newAutoCloseHarness(t)
	h.beads["Forge-c1"] = childPayload("Forge-c1", "Forge-p1")
	// Written out rather than built by parentPayload: the escape has to
	// reach the decoder as JSON's own \u001b, not Go's \x1b.
	h.beads["Forge-p1"] = `{"id":"Forge-p1","status":"open","labels":[],"dependents":[{"id":"Forge-c1","dependency_type":"parent-child","status":"closed"},{"id":"Forge-\u001b[31mc2","dependency_type":"parent-child","status":"closed"}]}`

	h.run("Forge-c1")

	require.Len(t, h.closed, 1)
	assert.NotContains(t, h.closed[0].reason, "\u001b")
	assert.Contains(t, h.closed[0].reason, "Forge-c2",
		"the sequence is removed whole, leaving no visible residue")

	events := h.autoClosedEvents(t)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0], "\u001b")
}

func TestAutoCloseParentDetail(t *testing.T) {
	// The "auto-closed:" prefix belongs to the bd reason, not to the shared
	// detail — the event supplies its own framing and would otherwise stutter.
	assert.Equal(t, "all 1 child closed (Forge-a)",
		autoCloseParentDetail([]string{"Forge-a"}))
	assert.Equal(t, "all 2 children closed (Forge-a, Forge-b)",
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

	t.Run("a parent bd already closed never fails the merge close", func(t *testing.T) {
		d, closed := newDaemon(t, errors.New("bd close Forge-p1: already closed"))
		err := d.closeMergedBead(context.Background(), "Forge-c1", closeTestAnvil,
			anvilPathFor(t, d), "PR #7 merged", 7, nil)
		require.NoError(t, err)
		assert.Empty(t, *closed)
	})
}
