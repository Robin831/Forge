package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"golang.org/x/crypto/bcrypt"
)

// This file exercises the full restart-mid-turn partial-output path end to end,
// tying together the three sibling sub-tasks:
//
//   - the store layer (state.UpsertTurnSnapshot / GetLatestTurnSnapshot),
//   - the throttled writer driven from the streaming loop (turnSnapshotWriter),
//   - the SSE reconnect fallback that replays the persisted partial before the
//     graceful turn_expired event (emitReconnectFallback / partialTurnText).
//
// The sibling tests verify each layer in isolation (the writer against an
// in-memory fake sink, the store against SQLite, the reconnect against a
// hand-seeded snapshot row). These tests instead drive the layers together
// against a real SQLite database and across a simulated daemon restart, so a
// regression in the seam between any two layers is caught.

// gatedStreamRunner is a forgechat.StreamingRunner that streams a first chunk,
// blocks on a gate (modelling a turn still in flight when the daemon dies),
// then — once released — streams a second chunk and returns the final response.
// It lets a test observe the persisted mid-turn snapshot while the turn is
// deliberately wedged, then let the turn complete to observe the terminal write.
type gatedStreamRunner struct {
	firstChunk  string
	secondChunk string
	gate        chan struct{}

	started  chan struct{} // closed once the first chunk has been streamed
	startOne sync.Once
}

func (g *gatedStreamRunner) Turn(ctx context.Context, req forgechat.TurnRequest) (*forgechat.TurnResponse, error) {
	return g.TurnStream(ctx, req, nil)
}

func (g *gatedStreamRunner) TurnStream(ctx context.Context, req forgechat.TurnRequest, onChunk forgechat.StreamFunc) (*forgechat.TurnResponse, error) {
	if onChunk != nil {
		onChunk(forgechat.StreamChunk{Kind: forgechat.StreamChunkText, Text: g.firstChunk})
	}
	// Signal that the partial has been streamed (and therefore persisted by the
	// snapshot writer) so the test can proceed deterministically.
	g.startOne.Do(func() {
		if g.started != nil {
			close(g.started)
		}
	})

	if g.gate != nil {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if onChunk != nil && g.secondChunk != "" {
		onChunk(forgechat.StreamChunk{Kind: forgechat.StreamChunkText, Text: g.secondChunk})
	}
	return &forgechat.TurnResponse{
		Messages: []forgechat.EmittedMessage{{Kind: "text", Content: g.firstChunk + g.secondChunk}},
	}, nil
}

// newServerOnDBPath builds a Server backed by the database file at dbPath. Two
// servers built on the same path share the persisted state but each get their
// own fresh in-memory TurnStore — exactly the shape of a daemon restart, where
// the SQLite file survives but the live turn registry does not. The DB handle is
// closed on test cleanup.
func newServerOnDBPath(t *testing.T, dbPath string) *Server {
	t.Helper()
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("open db %q: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })

	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	handler := func(cmd ipc.Command) ipc.Response {
		return ipc.Response{Type: "ok", Payload: []byte(`{}`)}
	}
	srv, err := New(Config{
		Addr:  ":0",
		Users: map[string]string{"alice": string(hash)},
	}, db, handler, slog.Default())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

// waitForTurnSnapshot polls the store until a snapshot with the wanted status
// (and non-empty text) exists for (sessionID, turnID), or fails after the
// deadline. Used to observe the writer persisting the mid-turn partial before
// the test simulates a restart.
func waitForTurnSnapshot(t *testing.T, db *state.DB, sessionID int64, turnID string, want state.ForgeTurnStatus, deadline time.Duration) *state.ForgeTurnSnapshot {
	t.Helper()
	stop := time.Now().Add(deadline)
	for {
		snap, err := db.GetTurnSnapshot(sessionID, turnID)
		if err != nil {
			t.Fatalf("GetTurnSnapshot: %v", err)
		}
		if snap != nil && snap.Status == want && snap.AccumulatedText != "" {
			return snap
		}
		if time.Now().After(stop) {
			t.Fatalf("snapshot for turn %q did not reach status %q within %s (last=%+v)", turnID, want, deadline, snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTurnSnapshotE2E_PartialSurvivesDaemonRestartAndReplays is the headline
// end-to-end scenario for this bead: start a turn, accumulate partial text via
// the streaming loop, simulate a daemon restart mid-turn, reconnect, and assert
// the persisted partial snapshot is surfaced (as a text_delta) before the
// turn_expired fallback. Finally, let the wedged turn complete and assert the
// terminal write flips the persisted snapshot to status=complete with the full
// text — the final-write-on-completion contract, verified against the real
// store rather than a fake sink.
func TestTurnSnapshotE2E_PartialSurvivesDaemonRestartAndReplays(t *testing.T) {
	dbPath := t.TempDir() + "/state.db"

	// --- Original daemon: start a streaming turn that wedges mid-flight. ---
	srv1 := newServerOnDBPath(t, dbPath)
	const partial = "Partial answer streamed before the restart."
	const rest = " And the rest, produced only after completion."
	runner := &gatedStreamRunner{
		firstChunk:  partial,
		secondChunk: rest,
		gate:        make(chan struct{}),
		started:     make(chan struct{}),
	}
	srv1.SetChatRunner(runner)

	cookie := loginAndGetCookie(t, srv1)
	sessionID := createForgeSessionHelper(t, srv1, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv1, http.MethodPost, "/api/forge/sessions/"+itoa(sessionID)+"/turn", `{"content":"go"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())

	// Wait for the streaming loop to persist the mid-turn partial. The turn is
	// now wedged on the gate — exactly the state a daemon would be in when it is
	// killed mid-turn.
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never streamed its first chunk")
	}
	partialSnap := waitForTurnSnapshot(t, srv1.db, sessionID, turnID, state.ForgeTurnStatusInProgress, 2*time.Second)
	if partialSnap.AccumulatedText != partial {
		t.Fatalf("persisted partial mismatch: got %q want %q", partialSnap.AccumulatedText, partial)
	}

	// --- Restart: a fresh daemon opens the same DB file with an empty store. ---
	srv2 := newServerOnDBPath(t, dbPath)
	// The restarted daemon has no live TurnState for this turn: it was lost with
	// the previous process.
	if _, ok := srv2.TurnStore().Get(turnID); ok {
		t.Fatal("restarted server unexpectedly has the turn in its store")
	}

	// Reconnect the SSE client against the restarted daemon. The persisted
	// partial must be replayed as a text_delta before the graceful turn_expired.
	resp, cancel := turnStreamRequest(t, srv2, sessionID, turnID)
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 SSE stream after restart, got %d", resp.StatusCode)
	}

	frames := readSSEFrames(t, resp, 2, 2*time.Second)
	if len(frames) < 2 {
		t.Fatalf("expected text_delta + turn_expired after restart, got %+v", frames)
	}
	if frames[0].Event != string(TurnEventTextDelta) {
		t.Fatalf("first frame after restart should be the replayed partial text_delta, got %q (frames=%+v)", frames[0].Event, frames)
	}
	var gotText string
	if err := json.Unmarshal([]byte(frames[0].Data), &gotText); err != nil {
		t.Fatalf("decode replayed text_delta %q: %v", frames[0].Data, err)
	}
	if gotText != partial {
		t.Fatalf("replayed partial mismatch: got %q want %q", gotText, partial)
	}
	if frames[1].Event != string(TurnEventTurnExpired) {
		t.Fatalf("second frame should be turn_expired, got %q (frames=%+v)", frames[1].Event, frames)
	}

	// --- Let the wedged turn finish on the original daemon and assert the
	// terminal write. ---
	close(runner.gate)
	st := waitForTurn(t, srv1, turnID)
	if st.Status() != TurnStatusComplete {
		t.Fatalf("expected the released turn to complete, got %s", st.Status())
	}

	finalSnap, err := srv1.db.GetTurnSnapshot(sessionID, turnID)
	if err != nil {
		t.Fatalf("GetTurnSnapshot after completion: %v", err)
	}
	if finalSnap == nil {
		t.Fatal("expected a persisted snapshot after completion")
	}
	if finalSnap.Status != state.ForgeTurnStatusComplete {
		t.Fatalf("final write must set status=complete, got %q", finalSnap.Status)
	}
	if finalSnap.AccumulatedText != partial+rest {
		t.Fatalf("final snapshot text mismatch: got %q want %q", finalSnap.AccumulatedText, partial+rest)
	}
}

// TestTurnSnapshotE2E_NoSnapshotReplaysNothing covers the read-back-returns-nil
// arm end to end: when a daemon restart leaves no persisted snapshot for the
// reconnecting turn (the turn was lost before any partial was ever written),
// GetLatestTurnSnapshot returns nil and the reconnect surfaces only the
// turn_expired fallback — no phantom text_delta from an unrelated turn.
func TestTurnSnapshotE2E_NoSnapshotReplaysNothing(t *testing.T) {
	dbPath := t.TempDir() + "/state.db"

	srv1 := newServerOnDBPath(t, dbPath)
	srv1.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv1)
	sessionID := createForgeSessionHelper(t, srv1, cookie, `{"initial_message":"start"}`)

	// Read-back on a session that never persisted a snapshot must be nil.
	if snap, err := srv1.db.GetLatestTurnSnapshot(sessionID); err != nil {
		t.Fatalf("GetLatestTurnSnapshot: %v", err)
	} else if snap != nil {
		t.Fatalf("expected nil snapshot for a session with none, got %+v", snap)
	}

	// Restart and reconnect to a turn id that never produced a snapshot.
	srv2 := newServerOnDBPath(t, dbPath)
	resp, cancel := turnStreamRequest(t, srv2, sessionID, "never-streamed")
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 SSE stream, got %d", resp.StatusCode)
	}
	frames := readSSEFrames(t, resp, 2, time.Second)
	if len(frames) == 0 {
		t.Fatal("expected a turn_expired frame, got none")
	}
	// With no snapshot to replay, the very first frame must be turn_expired —
	// there must be no leading text_delta.
	if frames[0].Event != string(TurnEventTurnExpired) {
		t.Fatalf("with no snapshot the first frame must be turn_expired, got %q (frames=%+v)", frames[0].Event, frames)
	}
	for _, f := range frames {
		if f.Event == string(TurnEventTextDelta) {
			t.Fatalf("no snapshot exists; nothing should be replayed, got frames=%+v", frames)
		}
	}
}

// newRealStoreWriter builds a turnSnapshotWriter backed by a real SQLite store
// and a manually-advanced clock, returning the writer, its session id, and the
// clock-advance func. Unlike the sibling writer tests (which use an in-memory
// fake sink), this exercises the writer's throttle decisions against the actual
// UpsertTurnSnapshot / GetTurnSnapshot round-trip.
func newRealStoreWriter(t *testing.T, db *state.DB, turnID string) (*turnSnapshotWriter, int64, func(time.Duration)) {
	t.Helper()
	sess, err := db.CreateForgeSession(state.ForgeSession{Title: "e2e writer", Anvil: "anvil-1"})
	if err != nil {
		t.Fatalf("CreateForgeSession: %v", err)
	}
	w := newTurnSnapshotWriter(db, sess.ID, turnID, nil)
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0).UTC()
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
	return w, sess.ID, advance
}

// TestTurnSnapshotE2E_ThrottledWritesPersistToStore verifies the throttling
// cadence against the real store: small deltas inside the min interval are held
// back (the persisted row does not advance), a delta after the interval flushes,
// and Finalize forces the terminal complete write regardless of the throttle.
// It also confirms read-back returns nil for a session that has no snapshot.
func TestTurnSnapshotE2E_ThrottledWritesPersistToStore(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	const turnID = "throttle-store"
	w, sessionID, advance := newRealStoreWriter(t, srv.db, turnID)

	// First write always passes: the partial is persisted immediately.
	w.Update("aaaa")
	if got := readSnapshotText(t, srv.db, sessionID, turnID); got != "aaaa" {
		t.Fatalf("first write should persist %q, got %q", "aaaa", got)
	}

	// A tiny delta within the interval is throttled: the persisted row is
	// unchanged (no new content reaches the store).
	w.Update("aaaab")
	if got := readSnapshotText(t, srv.db, sessionID, turnID); got != "aaaa" {
		t.Fatalf("throttled write should NOT advance the store, got %q", got)
	}

	// Once the interval elapses, the next delta flushes to the store.
	advance(w.minInterval)
	w.Update("aaaabc")
	snap := readSnapshot(t, srv.db, sessionID, turnID)
	if snap.AccumulatedText != "aaaabc" {
		t.Fatalf("post-interval write should persist %q, got %q", "aaaabc", snap.AccumulatedText)
	}
	if snap.Status != state.ForgeTurnStatusInProgress {
		t.Fatalf("mid-turn writes must be in_progress, got %q", snap.Status)
	}

	// Finalize forces the terminal write with status=complete and the full text,
	// bypassing the throttle entirely.
	w.Finalize("aaaabc-final", state.ForgeTurnStatusComplete)
	final := readSnapshot(t, srv.db, sessionID, turnID)
	if final.Status != state.ForgeTurnStatusComplete {
		t.Fatalf("Finalize must set status=complete, got %q", final.Status)
	}
	if final.AccumulatedText != "aaaabc-final" {
		t.Fatalf("Finalize must persist the full text, got %q", final.AccumulatedText)
	}

	// The completed snapshot is the session's latest; read-back returns it.
	latest, err := srv.db.GetLatestTurnSnapshot(sessionID)
	if err != nil {
		t.Fatalf("GetLatestTurnSnapshot: %v", err)
	}
	if latest == nil || latest.TurnID != turnID || latest.Status != state.ForgeTurnStatusComplete {
		t.Fatalf("latest snapshot read-back wrong: %+v", latest)
	}

	// Read-back on a fresh session with no snapshot returns nil.
	empty, err := srv.db.CreateForgeSession(state.ForgeSession{Title: "empty", Anvil: "anvil-1"})
	if err != nil {
		t.Fatalf("CreateForgeSession: %v", err)
	}
	if snap, err := srv.db.GetLatestTurnSnapshot(empty.ID); err != nil {
		t.Fatalf("GetLatestTurnSnapshot(empty): %v", err)
	} else if snap != nil {
		t.Fatalf("expected nil snapshot for empty session, got %+v", snap)
	}
}

func readSnapshot(t *testing.T, db *state.DB, sessionID int64, turnID string) *state.ForgeTurnSnapshot {
	t.Helper()
	snap, err := db.GetTurnSnapshot(sessionID, turnID)
	if err != nil {
		t.Fatalf("GetTurnSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatalf("expected a snapshot for turn %q, got nil", turnID)
	}
	return snap
}

func readSnapshotText(t *testing.T, db *state.DB, sessionID int64, turnID string) string {
	t.Helper()
	return readSnapshot(t, db, sessionID, turnID).AccumulatedText
}
