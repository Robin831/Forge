package bellows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

// readyWatcher is an assay-disabled monitor over a scripted status sequence
// that counts ready-to-merge events and auto-merge handler calls.
type readyWatcher struct {
	m          *Monitor
	ready      int
	autoMerges int
}

func newReadyWatcher(t *testing.T, db *state.DB, statuses ...*vcs.PRStatus) *readyWatcher {
	t.Helper()
	fake := &fakeVCSProvider{checkStatusFunc: func(call int) (*vcs.PRStatus, error) {
		if call > len(statuses) {
			call = len(statuses)
		}
		s := *statuses[call-1]
		return &s, nil
	}}
	w := &readyWatcher{}
	w.m = New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, func() int { return 5 }, nil)
	w.m.OnEvent(func(_ context.Context, e PREvent) {
		if e.EventType == EventPRReadyToMerge {
			w.ready++
		}
	})
	w.m.SetAutoMergeHandler(func(context.Context, string, state.PR) { w.autoMerges++ })
	return w
}

func insertOpenPR(t *testing.T, db *state.DB, n int) *state.PR {
	t.Helper()
	pr := &state.PR{
		Number:    n,
		Anvil:     "test-anvil",
		BeadID:    "forge-threads",
		Branch:    "forge/forge-threads",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	return pr
}

var (
	withThreads = &vcs.PRStatus{State: "OPEN", HeadSHA: "abc", UnresolvedThreads: 2}
	uncounted   = &vcs.PRStatus{State: "OPEN", HeadSHA: "abc", UnresolvedThreadsUnknown: true}
	clean       = &vcs.PRStatus{State: "OPEN", HeadSHA: "abc"}
)

// Explorer #401: open review threads, then a poll whose thread query timed out.
// That poll's zero count measured nothing, so it must not be read as "threads
// resolved" — no ready edge, no auto-merge. The next poll that can count them
// and finds them resolved fires the edge exactly once.
func TestCheckPR_UncountedThreadsNeverReady(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	insertOpenPR(t, db, 401)
	w := newReadyWatcher(t, db, withThreads, uncounted, clean, clean)

	w.m.checkAll(context.Background()) // threads open
	w.m.checkAll(context.Background()) // thread count unknown
	assert.Zero(t, w.ready, "an uncounted thread state must not announce the PR ready")
	assert.Zero(t, w.autoMerges, "an uncounted thread state must not auto-merge")

	w.m.checkAll(context.Background()) // threads counted: none left
	w.m.checkAll(context.Background()) // unchanged
	assert.Equal(t, 1, w.ready, "the edge fires once, on the first poll that can count the threads")
	assert.Equal(t, 1, w.autoMerges)
}

// A daemon that first sees a PR during a GraphQL outage has no previous
// snapshot to carry from; that first poll must still not be ready.
func TestCheckPR_UncountedOnFirstSighting(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	pr := insertOpenPR(t, db, 402)
	w := newReadyWatcher(t, db, uncounted, clean)

	w.m.checkAll(context.Background())
	assert.Zero(t, w.ready)

	// The persisted mergeability must fail closed too: the Ready to Merge panel
	// and the manual merge_pr gate read it.
	ready, err := db.IsPRReadyToMerge(pr.ID)
	require.NoError(t, err)
	assert.False(t, ready, "an uncounted thread state must persist as not ready")

	w.m.checkAll(context.Background())
	assert.Equal(t, 1, w.ready)
}

// RearmAutoMerge re-fires the ready edge on the next poll that finds the PR
// still ready, once — it is how a transiently failed auto-merge gets retried.
func TestRearmAutoMerge_RefiresOnce(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	pr := insertOpenPR(t, db, 403)
	w := newReadyWatcher(t, db, clean)

	w.m.checkAll(context.Background())
	require.Equal(t, 1, w.autoMerges, "assay disabled: ready on first sighting")

	w.m.RearmAutoMerge(pr.Anvil, pr.Number)
	w.m.checkAll(context.Background())
	assert.Equal(t, 2, w.autoMerges, "a re-armed PR that is still ready re-fires auto-merge")

	w.m.checkAll(context.Background())
	assert.Equal(t, 2, w.autoMerges, "the re-arm is one-shot")
}

// Re-arming a PR bellows has no snapshot for is a no-op, not a panic.
func TestRearmAutoMerge_NoSnapshot(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	w := newReadyWatcher(t, db, clean)
	assert.NotPanics(t, func() { w.m.RearmAutoMerge("test-anvil", 999) })
}
