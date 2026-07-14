package web

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/forgechat"
)

// streamStubRunner is a forgechat.StreamingRunner used to drive runTurnAsync's
// incremental path deterministically. It replays a fixed chunk sequence through
// onChunk (in order, synchronously) and then returns a canned TurnResponse, so
// tests can assert the broadcaster fans out one event per chunk in arrival
// order rather than a single batched delta at the end.
type streamStubRunner struct {
	mu       sync.Mutex
	calls    []forgechat.TurnRequest
	chunks   []forgechat.StreamChunk
	response *forgechat.TurnResponse
	err      error
}

func (s *streamStubRunner) Turn(ctx context.Context, req forgechat.TurnRequest) (*forgechat.TurnResponse, error) {
	return s.TurnStream(ctx, req, nil)
}

func (s *streamStubRunner) TurnStream(ctx context.Context, req forgechat.TurnRequest, onChunk forgechat.StreamFunc) (*forgechat.TurnResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	chunks := s.chunks
	resp := s.response
	err := s.err
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if onChunk != nil {
		for _, c := range chunks {
			onChunk(c)
		}
	}
	if resp != nil {
		return resp, nil
	}
	return &forgechat.TurnResponse{
		Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "final"}},
	}, nil
}

// collectTurnEvents subscribes to st and drains every event until the
// broadcaster closes (the runner goroutine finished). It returns the events in
// arrival order.
func collectTurnEvents(t *testing.T, st *TurnState, run func()) []TurnEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := st.Subscribe(ctx)

	go run()

	var events []TurnEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatalf("timed out collecting turn events (got %d so far)", len(events))
		}
	}
}

// TestRunTurnAsync_StreamsIncrementalDeltasInOrder is the core producer test:
// a streaming runner replays interleaved text + tool chunks, and runTurnAsync
// must broadcast one text_delta per text chunk (never a single batched delta),
// interleave the tool events in arrival order, and finish with message +
// complete. The accumulated snapshot text must equal the concatenated deltas.
func TestRunTurnAsync_StreamsIncrementalDeltasInOrder(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"hi"}`)

	runner := &streamStubRunner{
		chunks: []forgechat.StreamChunk{
			{Kind: forgechat.StreamChunkText, Text: "Hel"},
			{Kind: forgechat.StreamChunkText, Text: "lo "},
			{Kind: forgechat.StreamChunkToolUse, ToolName: "Read", ToolID: "t1"},
			{Kind: forgechat.StreamChunkToolResult, ToolID: "t1"},
			{Kind: forgechat.StreamChunkText, Text: "world"},
		},
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "Hello world"}},
		},
	}
	srv.SetChatRunner(runner)

	st := srv.TurnStore().New("turn-stream-order", id)
	req := forgechat.TurnRequest{Stage: forgechat.StageDrafting, Mode: forgechat.ModeChat, SessionID: id}
	events := collectTurnEvents(t, st, func() { srv.runTurnAsync(st, req) })

	// Expected precise ordering of event types.
	wantTypes := []TurnEventType{
		TurnEventTextDelta,  // "Hel"
		TurnEventTextDelta,  // "lo "
		TurnEventToolUse,    // Read/t1
		TurnEventToolResult, // t1
		TurnEventTextDelta,  // "world"
		TurnEventMessage,    // persisted assistant row
		TurnEventComplete,   // final
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("expected %d events, got %d: %+v", len(wantTypes), len(events), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d: want type %q, got %q (all=%+v)", i, want, events[i].Type, events)
		}
	}

	// The text deltas must be the incremental slices, in order — not one batched
	// "Hello world" delta.
	var deltas []string
	for _, ev := range events {
		if ev.Type == TurnEventTextDelta {
			s, ok := ev.Data.(string)
			if !ok {
				t.Fatalf("text_delta data should be a string, got %T", ev.Data)
			}
			deltas = append(deltas, s)
		}
		if ev.Type == TurnEventTextDelta && ev.Data == "Hello world" {
			t.Fatalf("text_delta was batched into the full reply; expected incremental slices")
		}
	}
	wantDeltas := []string{"Hel", "lo ", "world"}
	if len(deltas) != len(wantDeltas) {
		t.Fatalf("expected %d incremental deltas, got %v", len(wantDeltas), deltas)
	}
	for i, want := range wantDeltas {
		if deltas[i] != want {
			t.Fatalf("delta %d: want %q, got %q", i, want, deltas[i])
		}
	}

	// The tool events must carry the correlating tool id.
	var sawToolUse, sawToolResult bool
	for _, ev := range events {
		switch ev.Type {
		case TurnEventToolUse:
			te, ok := ev.Data.(TurnToolEvent)
			if !ok || te.Name != "Read" || te.ID != "t1" {
				t.Fatalf("tool_use payload wrong: %+v", ev.Data)
			}
			sawToolUse = true
		case TurnEventToolResult:
			te, ok := ev.Data.(TurnToolEvent)
			if !ok || te.ID != "t1" {
				t.Fatalf("tool_result payload wrong: %+v", ev.Data)
			}
			sawToolResult = true
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Fatalf("expected both tool_use and tool_result events, saw use=%v result=%v", sawToolUse, sawToolResult)
	}

	// Snapshot text must equal the concatenation of the streamed deltas, and the
	// terminal status must be complete.
	if got := st.Text(); got != "Hello world" {
		t.Fatalf("snapshot text should equal concatenated deltas, got %q", got)
	}
	snap := st.Snapshot()
	if snap.Status != TurnStatusComplete {
		t.Fatalf("expected complete status, got %s", snap.Status)
	}
	if len(snap.ToolEvents) != 2 {
		t.Fatalf("expected 2 recorded tool events in snapshot, got %d", len(snap.ToolEvents))
	}
}

// TestRunTurnAsync_NonStreamingRunnerBatchesSingleDelta guards the fallback:
// a plain Runner (no TurnStream) still yields exactly one batched text_delta
// carrying the full reply, followed by message + complete — the pre-streaming
// contract, unchanged.
func TestRunTurnAsync_NonStreamingRunnerBatchesSingleDelta(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"hi"}`)

	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "batched reply"}},
		},
	}
	srv.SetChatRunner(runner)

	st := srv.TurnStore().New("turn-batched", id)
	req := forgechat.TurnRequest{Stage: forgechat.StageDrafting, Mode: forgechat.ModeChat, SessionID: id}
	events := collectTurnEvents(t, st, func() { srv.runTurnAsync(st, req) })

	var deltas []string
	var complete bool
	for _, ev := range events {
		if ev.Type == TurnEventTextDelta {
			deltas = append(deltas, ev.Data.(string))
		}
		if ev.Type == TurnEventComplete {
			complete = true
		}
	}
	if len(deltas) != 1 || deltas[0] != "batched reply" {
		t.Fatalf("non-streaming runner should emit one batched delta, got %v", deltas)
	}
	if !complete {
		t.Fatalf("expected a complete event")
	}
}

// TestRunTurnAsync_GrillingDoesNotStreamRawJSONText verifies the mode gate:
// a grilling turn's assistant text (a JSON envelope) must not be forwarded as
// incremental text deltas. Only the batched question message drives text_delta,
// keeping the persisted rows / snapshot text identical to the pre-streaming
// behaviour. Tool events still stream.
func TestRunTurnAsync_GrillingDoesNotStreamRawJSONText(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"hi"}`)

	runner := &streamStubRunner{
		chunks: []forgechat.StreamChunk{
			{Kind: forgechat.StreamChunkText, Text: `{"questions":[`},
			{Kind: forgechat.StreamChunkToolUse, ToolName: "Grep", ToolID: "g1"},
			{Kind: forgechat.StreamChunkText, Text: `]}`},
		},
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{
				Kind:     "question",
				Content:  "Sync or async?",
				Metadata: `{"options":[{"id":"sync","label":"Sync"}]}`,
			}},
		},
	}
	srv.SetChatRunner(runner)

	st := srv.TurnStore().New("turn-grill", id)
	req := forgechat.TurnRequest{Stage: forgechat.StageGrilling, Mode: forgechat.ModeGrill, SessionID: id}
	events := collectTurnEvents(t, st, func() { srv.runTurnAsync(st, req) })

	var deltas []string
	var sawToolUse bool
	for _, ev := range events {
		if ev.Type == TurnEventTextDelta {
			deltas = append(deltas, ev.Data.(string))
		}
		if ev.Type == TurnEventToolUse {
			sawToolUse = true
		}
	}
	// The only text_delta is the batched question prompt; the raw JSON chunks
	// were suppressed by the prose-only gate.
	if len(deltas) != 1 || deltas[0] != "Sync or async?" {
		t.Fatalf("grilling should emit only the batched question delta, got %v", deltas)
	}
	if !sawToolUse {
		t.Fatalf("tool events should stream even during grilling")
	}
	if got := st.Text(); got != "Sync or async?" {
		t.Fatalf("grilling snapshot text should be the question prompt, got %q", got)
	}
}
