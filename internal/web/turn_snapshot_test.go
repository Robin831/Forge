package web

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// fakeSnapshotSink records every UpsertTurnSnapshot call so tests can assert
// the throttle behaviour without a real database. A non-nil failWith makes
// every call fail, exercising the graceful-degradation path.
type fakeSnapshotSink struct {
	mu       sync.Mutex
	calls    []state.ForgeTurnSnapshot
	failWith error
}

func (f *fakeSnapshotSink) UpsertTurnSnapshot(sessionID int64, turnID string, status state.ForgeTurnStatus, text string) (state.ForgeTurnSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return state.ForgeTurnSnapshot{}, f.failWith
	}
	snap := state.ForgeTurnSnapshot{SessionID: sessionID, TurnID: turnID, Status: status, AccumulatedText: text}
	f.calls = append(f.calls, snap)
	return snap, nil
}

func (f *fakeSnapshotSink) snapshot() []state.ForgeTurnSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]state.ForgeTurnSnapshot, len(f.calls))
	copy(out, f.calls)
	return out
}

// newTestWriter builds a writer backed by fake time so the throttle can be
// exercised deterministically. The returned advance func moves the clock.
func newTestWriter(sink turnSnapshotSink) (*turnSnapshotWriter, func(time.Duration)) {
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0).UTC()
	w := newTurnSnapshotWriter(sink, 42, "turn-abc", nil)
	w.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	return w, advance
}

// TestTurnSnapshotWriter_ThrottlesByInterval verifies a slow trickle of small
// deltas is held back until the min interval elapses, and that the first write
// always goes through.
func TestTurnSnapshotWriter_ThrottlesByInterval(t *testing.T) {
	sink := &fakeSnapshotSink{}
	w, advance := newTestWriter(sink)

	// The first write always passes; a small delta within the interval is
	// held back; once the interval elapses the next delta flushes.
	w.Update("a")
	w.Update("ab")
	advance(w.minInterval)
	w.Update("abc")

	calls := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 writes, got %d: %+v", len(calls), calls)
	}
	if calls[0].AccumulatedText != "a" || calls[1].AccumulatedText != "abc" {
		t.Fatalf("unexpected write texts: %+v", calls)
	}
	for _, c := range calls {
		if c.Status != state.ForgeTurnStatusInProgress {
			t.Fatalf("Update writes must be in_progress, got %q", c.Status)
		}
	}
}

// TestTurnSnapshotWriter_FlushesLargeByteDelta verifies a burst larger than the
// byte-delta threshold flushes immediately, even within the min interval.
func TestTurnSnapshotWriter_FlushesLargeByteDelta(t *testing.T) {
	sink := &fakeSnapshotSink{}
	w, _ := newTestWriter(sink)

	w.Update("start")
	// A burst larger than the byte-delta threshold flushes even though no time
	// has advanced (the interval alone would have held it back).
	big := "start" + string(make([]byte, w.minByteDelta))
	w.Update(big)

	if got := len(sink.snapshot()); got != 2 {
		t.Fatalf("expected 2 writes (interval bypassed by byte delta), got %d", got)
	}
}

// TestTurnSnapshotWriter_IgnoresNonGrowingText verifies that calls which add no
// new bytes never trigger a write.
func TestTurnSnapshotWriter_IgnoresNonGrowingText(t *testing.T) {
	sink := &fakeSnapshotSink{}
	w, advance := newTestWriter(sink)

	w.Update("hello")
	advance(w.minInterval)
	w.Update("hello") // same length -> no new content
	advance(w.minInterval)
	w.Update("hi") // shorter -> never regress

	if got := len(sink.snapshot()); got != 1 {
		t.Fatalf("expected 1 write, got %d", got)
	}
}

// TestTurnSnapshotWriter_FinalizeBypassesThrottle verifies Finalize always
// writes with the terminal status regardless of interval or byte delta.
func TestTurnSnapshotWriter_FinalizeBypassesThrottle(t *testing.T) {
	sink := &fakeSnapshotSink{}
	w, _ := newTestWriter(sink)

	w.Update("partial")
	// Second update is throttled away; Finalize must still write the final text.
	w.Update("partial+")
	w.Finalize("partial+done", state.ForgeTurnStatusComplete)

	calls := sink.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 writes (1 update + 1 finalize), got %d: %+v", len(calls), calls)
	}
	last := calls[len(calls)-1]
	if last.Status != state.ForgeTurnStatusComplete || last.AccumulatedText != "partial+done" {
		t.Fatalf("finalize wrote wrong snapshot: %+v", last)
	}
}

// TestTurnSnapshotWriter_DisablesOnError verifies that a failing sink (e.g. the
// snapshots table is missing because the migration hasn't run) disables the
// writer after one attempt rather than retrying on every chunk.
func TestTurnSnapshotWriter_DisablesOnError(t *testing.T) {
	sink := &fakeSnapshotSink{failWith: errors.New("no such table: forge_turn_snapshots")}
	w, advance := newTestWriter(sink)

	// First attempt fails, disabling the writer; later calls make no attempts.
	w.Update("a")
	advance(w.minInterval)
	w.Update("abcdef")
	w.Finalize("final", state.ForgeTurnStatusComplete)

	if !w.disabled {
		t.Fatal("writer should be disabled after a failing write")
	}
}

// TestNewTurnSnapshotWriter_NoOpWhenUnpersistable verifies that a nil sink or a
// missing key produces a disabled (no-op) writer so callers need not nil-check.
func TestNewTurnSnapshotWriter_NoOpWhenUnpersistable(t *testing.T) {
	if !newTurnSnapshotWriter(nil, 1, "t", nil).disabled {
		t.Fatal("nil sink should yield a disabled writer")
	}
	if !newTurnSnapshotWriter(&fakeSnapshotSink{}, 0, "t", nil).disabled {
		t.Fatal("zero session should yield a disabled writer")
	}
	if !newTurnSnapshotWriter(&fakeSnapshotSink{}, 1, "", nil).disabled {
		t.Fatal("empty turn id should yield a disabled writer")
	}
}
