package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/state"
)

// TestUncountFix_RateLimited verifies that a rate-limited dispatch can be
// given back: the counter returns to its pre-dispatch value, the change is
// persisted, and repeated uncounts floor at zero instead of going negative.
// This is the counter half of the 2026-08-19 incident fix, where a session
// limit burned review_fix_count to max in minutes of 2-minute Bellows polls
// (PRs #834–836) and flagged every affected bead needs_human.
func TestUncountFix_RateLimited(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	pr := &state.PR{
		Number:    900,
		Anvil:     "test-anvil",
		BeadID:    "bd-rl",
		Branch:    "forge/bd-rl",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
		CIPassing: true,
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}

	m := New(db, testLogger(), func(ctx context.Context, req ActionRequest) {})

	// Review fix: count one dispatch, then give it back.
	m.HandleEvent(ctx, makeEvent(900, bellows.EventReviewChanges))
	if st := m.GetState("test-anvil", 900); st.ReviewFixCnt != 1 {
		t.Fatalf("expected ReviewFixCnt 1 after dispatch, got %d", st.ReviewFixCnt)
	}
	m.UncountReviewFix("test-anvil", 900)
	if st := m.GetState("test-anvil", 900); st.ReviewFixCnt != 0 {
		t.Fatalf("expected ReviewFixCnt 0 after uncount, got %d", st.ReviewFixCnt)
	}
	// Floor: uncounting with nothing counted must not go negative.
	m.UncountReviewFix("test-anvil", 900)
	if st := m.GetState("test-anvil", 900); st.ReviewFixCnt != 0 {
		t.Fatalf("expected ReviewFixCnt to floor at 0, got %d", st.ReviewFixCnt)
	}

	// CI fix: same contract.
	m.HandleEvent(ctx, makeEvent(900, bellows.EventCIFailed))
	if st := m.GetState("test-anvil", 900); st.CIFixCount != 1 {
		t.Fatalf("expected CIFixCount 1 after dispatch, got %d", st.CIFixCount)
	}
	m.UncountCIFix("test-anvil", 900)
	m.UncountCIFix("test-anvil", 900)
	if st := m.GetState("test-anvil", 900); st.CIFixCount != 0 {
		t.Fatalf("expected CIFixCount to floor at 0, got %d", st.CIFixCount)
	}

	// Unknown PR: must be a silent no-op.
	m.UncountReviewFix("test-anvil", 999)

	// Persistence: a fresh manager loading from the DB must see the uncounted
	// values, not the pre-uncount ones.
	m2 := New(db, testLogger(), func(ctx context.Context, req ActionRequest) {})
	if err := m2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	st := m2.GetState("test-anvil", 900)
	if st == nil {
		t.Fatal("state not reloaded")
	}
	if st.ReviewFixCnt != 0 || st.CIFixCount != 0 {
		t.Fatalf("expected persisted counts 0/0, got review=%d ci=%d", st.ReviewFixCnt, st.CIFixCount)
	}
}
