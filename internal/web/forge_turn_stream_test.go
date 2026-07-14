package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/forgechat"
)

// sseFrame is one decoded `event:`/`data:` block emitted by the turn stream.
type sseFrame struct {
	Event string
	Data  string
}

// readSSEFrames scans the body for up to maxFrames complete frames (separated
// by blank lines). Returns whatever it accumulated within the deadline; the
// caller is responsible for any further assertions on length.
func readSSEFrames(t *testing.T, r *http.Response, maxFrames int, deadline time.Duration) []sseFrame {
	t.Helper()
	out := make(chan []sseFrame, 1)
	go func() {
		scanner := bufio.NewScanner(r.Body)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		var (
			frames []sseFrame
			cur    sseFrame
		)
		flush := func() {
			if cur.Event != "" || cur.Data != "" {
				frames = append(frames, cur)
			}
			cur = sseFrame{}
		}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				flush()
				if len(frames) >= maxFrames {
					out <- frames
					return
				}
			case strings.HasPrefix(line, "event: "):
				cur.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if cur.Data != "" {
					cur.Data += "\n"
				}
				cur.Data += strings.TrimPrefix(line, "data: ")
			}
		}
		flush()
		out <- frames
	}()
	select {
	case f := <-out:
		return f
	case <-time.After(deadline):
		return nil
	}
}

// turnStreamRequest opens an authenticated SSE connection for the given
// session/turn. The caller is responsible for closing the response body.
func turnStreamRequest(t *testing.T, srv *Server, sessionID int64, turnID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	cookie := loginAndGetCookie(t, srv)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	url := ts.URL + "/api/forge/sessions/" + itoa(sessionID) + "/turn/" + turnID + "/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("do request: %v", err)
	}
	return resp, cancel
}

func TestForgeTurnGet_ReturnsSnapshot(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "claude says hi"}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"hello"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)

	get := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions/"+itoa(id)+"/turn/"+turnID, "", cookie)
	if get.Code != http.StatusOK {
		t.Fatalf("get: %d body=%s", get.Code, get.Body.String())
	}
	var snap TurnSnapshot
	if err := json.Unmarshal(get.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v body=%s", err, get.Body.String())
	}
	if snap.ID != turnID {
		t.Fatalf("snapshot id mismatch: got %q want %q", snap.ID, turnID)
	}
	if snap.SessionID != id {
		t.Fatalf("snapshot session_id mismatch: got %d want %d", snap.SessionID, id)
	}
	if snap.Status != TurnStatusComplete {
		t.Fatalf("expected complete, got %s", snap.Status)
	}
	if snap.Text != "claude says hi" {
		t.Fatalf("expected accumulated text, got %q", snap.Text)
	}
	if snap.FinalMessageID == 0 {
		t.Fatalf("expected non-zero final_message_id, got %d", snap.FinalMessageID)
	}
}

func TestForgeTurnGet_UnknownTurnReturns404(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions/"+itoa(id)+"/turn/does-not-exist", "", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestForgeTurnGet_WrongSessionReturns404(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv)
	idA := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"a"}`)
	idB := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"b"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(idA)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)

	// The turn belongs to idA — requesting it under idB must 404 so a
	// client cannot enumerate / cross-read turns from other sessions.
	get := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions/"+itoa(idB)+"/turn/"+turnID, "", cookie)
	if get.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-session turn, got %d body=%s", get.Code, get.Body.String())
	}
}

// After a daemon restart (or GC expiry / retention-cap eviction) the turn is
// gone from the store. A reconnecting SSE client must receive a graceful
// turn_expired event on a 200 stream — not a 404 — so the SPA refetches
// canonical messages and clears its spinner instead of hanging on a dead
// stream. The stream must also close cleanly right after (no dangling frames).
func TestForgeTurnStream_MissingTurnEmitsTurnExpired(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	// No turn was ever registered under this id — simulates a turn lost on a
	// daemon restart / already garbage-collected.
	resp, cancel := turnStreamRequest(t, srv, id, "lost-on-restart")
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 SSE stream, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type: got %q want text/event-stream", got)
	}

	frames := readSSEFrames(t, resp, 1, 2*time.Second)
	if len(frames) == 0 {
		t.Fatal("expected a turn_expired frame, got none")
	}
	if frames[0].Event != string(TurnEventTurnExpired) {
		t.Fatalf("expected turn_expired event, got %q (frames=%+v)", frames[0].Event, frames)
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(frames[0].Data), &payload); err != nil {
		t.Fatalf("decode turn_expired payload %q: %v", frames[0].Data, err)
	}
	if payload.Message == "" {
		t.Fatalf("turn_expired payload should carry a message, got %q", frames[0].Data)
	}
}

// A turn evicted from the store after it completed (e.g. GC/expiry between the
// initial load and a reconnect) must also degrade to turn_expired rather than
// 404 on the stream path.
func TestForgeTurnStream_ExpiredAfterCompletionEmitsTurnExpired(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "done"}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)

	// Simulate the completed turn being reclaimed by GC before the reconnect.
	srv.TurnStore().Delete(turnID)

	resp, cancel := turnStreamRequest(t, srv, id, turnID)
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 SSE stream, got %d", resp.StatusCode)
	}
	frames := readSSEFrames(t, resp, 1, 2*time.Second)
	if len(frames) == 0 || frames[0].Event != string(TurnEventTurnExpired) {
		t.Fatalf("expected turn_expired event, got frames=%+v", frames)
	}
}

func TestForgeTurnGet_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions/1/turn/abc", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// When the turn has already terminated by the time the SSE consumer opens
// the connection, the handler must synthesise a terminal frame from the
// snapshot so the client observes a deterministic complete/error event
// before the stream closes.
func TestForgeTurnStream_LateConnectEmitsTerminalEvent(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "all done"}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	st := waitForTurn(t, srv, turnID)
	if st.Status() != TurnStatusComplete {
		t.Fatalf("expected complete, got %s", st.Status())
	}

	resp, cancel := turnStreamRequest(t, srv, id, turnID)
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type: got %q want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: got %q want no-cache", got)
	}
	if got := resp.Header.Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection: got %q want keep-alive", got)
	}

	frames := readSSEFrames(t, resp, 1, 2*time.Second)
	if len(frames) == 0 {
		t.Fatalf("expected at least one terminal frame, got none")
	}
	found := false
	for _, f := range frames {
		if f.Event == string(TurnEventComplete) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a complete event, got frames=%+v", frames)
	}
}

// SSE consumers that connect while the runner is still working must receive
// the events live: text_delta for the chunk, message for the persisted row,
// complete for the terminal frame. The gated stub blocks the runner so the
// SSE connection is established before any event is emitted.
func TestForgeTurnStream_ForwardsLiveEvents(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	gate := make(chan struct{})
	runner := &stubRunner{
		gate: gate,
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "streamed reply"}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())

	resp, cancel := turnStreamRequest(t, srv, id, turnID)
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Now unblock the runner — events should flow through to the SSE stream.
	close(gate)

	frames := readSSEFrames(t, resp, 3, 5*time.Second)
	if len(frames) < 1 {
		t.Fatalf("expected at least one streamed frame, got %+v", frames)
	}

	eventTypes := make(map[string]int)
	var textDeltaData string
	for _, f := range frames {
		eventTypes[f.Event]++
		if f.Event == string(TurnEventTextDelta) {
			textDeltaData = f.Data
		}
	}
	if eventTypes[string(TurnEventTextDelta)] == 0 {
		t.Fatalf("expected text_delta event, got %+v", frames)
	}
	if eventTypes[string(TurnEventComplete)] == 0 {
		t.Fatalf("expected complete event, got %+v", frames)
	}
	// The text_delta payload is JSON-encoded — the runner emits the raw
	// chunk string so the JSON value is a quoted string.
	if textDeltaData != `"streamed reply"` {
		t.Fatalf("text_delta data wrong: got %q want \"\"streamed reply\"\"", textDeltaData)
	}
}

// Runner errors land on the TurnState as an error event. The SSE consumer
// must receive an `error` frame before the channel closes.
func TestForgeTurnStream_RunnerErrorEmitsErrorFrame(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{err: errors.New("upstream down")}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)

	resp, cancel := turnStreamRequest(t, srv, id, turnID)
	defer cancel()
	defer resp.Body.Close()

	frames := readSSEFrames(t, resp, 1, 2*time.Second)
	if len(frames) == 0 {
		t.Fatalf("expected an error frame, got none")
	}
	found := false
	for _, f := range frames {
		if f.Event == string(TurnEventError) && strings.Contains(f.Data, "upstream down") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected error frame containing 'upstream down', got frames=%+v", frames)
	}
}

// Unknown turn ids on the stream endpoint degrade to a graceful turn_expired
// event on a 200 stream (rather than a 404) so a reconnecting client refetches
// its canonical messages instead of hanging on a dead stream.
func TestForgeTurnStream_UnknownTurnEmitsTurnExpired(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions/"+itoa(id)+"/turn/does-not-exist/stream", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 SSE stream, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: "+string(TurnEventTurnExpired)) {
		t.Fatalf("expected a turn_expired event, got body=%s", rec.Body.String())
	}
}

// Cross-session reads must not leak another session's turn. Since the SSE
// headers are already committed, the handler degrades to the same graceful
// turn_expired event rather than exposing the foreign turn — the client only
// learns to refetch its own session's messages.
func TestForgeTurnStream_WrongSessionEmitsTurnExpired(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv)
	idA := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"a"}`)
	idB := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"b"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(idA)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)

	get := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions/"+itoa(idB)+"/turn/"+turnID+"/stream", "", cookie)
	if get.Code != http.StatusOK {
		t.Fatalf("expected 200 SSE stream for cross-session stream, got %d body=%s", get.Code, get.Body.String())
	}
	if !strings.Contains(get.Body.String(), "event: "+string(TurnEventTurnExpired)) {
		t.Fatalf("cross-session stream must degrade to turn_expired, got body=%s", get.Body.String())
	}
}

// Closing the client's request context must release the SSE handler so the
// goroutine doesn't leak. We verify by closing the body and waiting for the
// runner's gate to be safe to close.
func TestForgeTurnStream_ClientDisconnectReleasesHandler(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	runner := &stubRunner{gate: gate}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())

	resp, cancel := turnStreamRequest(t, srv, id, turnID)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Tear down the client side — the handler's r.Context().Done() select
	// case must fire and return. We confirm the body actually closes.
	cancel()
	done := make(chan struct{})
	go func() {
		_ = resp.Body.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not release on client disconnect within 2s")
	}
}
