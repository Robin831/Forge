package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/go-chi/chi/v5"
)

// Beads-Forge per-turn streaming and polling endpoints.
//
// These siblings of POST /api/forge/sessions/{id}/turn surface the in-flight
// TurnState created by that handler:
//
//   - GET /api/forge/sessions/{id}/turn/{turn_id}/stream
//     Server-Sent Events feed. Subscribes to the TurnState's Events channel
//     and forwards each event (text_delta, tool_use, tool_result, message,
//     complete, error) as a typed SSE event. Closes when the goroutine
//     terminates or the client disconnects.
//
//   - GET /api/forge/sessions/{id}/turn/{turn_id}
//     JSON snapshot of the current TurnState. Intended for clients that
//     cannot consume SSE (curl, integration tests, slow polling fallbacks).

// handleForgeSessionTurnStream serves SSE for one async turn. It writes the
// usual text/event-stream headers, flushes immediately so reverse proxies
// don't buffer the response, then subscribes to the TurnState broadcaster
// to receive a dedicated per-client event channel. Multiple concurrent SSE
// consumers each get their own channel so no events are stolen by a sibling.
//
// Clients that connect after the turn has already terminated see an
// immediately-closed subscriber channel and we detect this via Done, then
// synthesise a terminal frame from the snapshot so they observe a
// deterministic complete/error event before the connection ends.
func (s *Server) handleForgeSessionTurnStream(w http.ResponseWriter, r *http.Request) {
	// Validate the session up front (auth + ownership) so genuine 401/404
	// failures are still surfaced as JSON before we commit to the SSE stream.
	sessionID, ok := s.lookupSession(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	turnID := chi.URLParam(r, "turn_id")
	if turnID == "" {
		writeError(w, http.StatusBadRequest, "turn_id is required")
		return
	}

	writeSSEHeaders(w, flusher)

	// Resolve the turn only after the SSE headers are committed. A missing or
	// foreign turn on the reconnect path (expired by GC, evicted past the
	// retention cap, or lost on a daemon restart) is reported as a graceful
	// turn_expired event on the 200 stream rather than a 404 — the SPA reuses
	// its complete-path refetch, so the spinner clears and the canonical
	// messages reload instead of the client hanging on a dead stream. Before
	// that fallback, a persisted mid-turn snapshot (if any) is replayed so the
	// client recovers the partial output instead of an empty bubble.
	st, ok := s.turnStore.Get(turnID)
	if !ok || st.SessionID != sessionID {
		s.emitReconnectFallback(w, flusher, sessionID, turnID)
		return
	}

	// Subscribe before checking Done to avoid the race where the turn
	// completes between the Done check and the subscribe call. If the turn
	// is already done, Subscribe returns an immediately-closed channel and
	// the broadcaster marks itself closed atomically under its mutex.
	subCh := st.Subscribe(r.Context())

	// Late-connect: Done is closed (and therefore the broadcaster is also
	// closed). Synthesise a terminal frame from the snapshot so the client
	// observes a complete/error event rather than an immediately-closed
	// stream with no payload.
	select {
	case <-st.Done:
		emitTerminalTurnSSE(w, flusher, st.Snapshot())
		return
	default:
	}

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	// sawTerminal tracks whether a TurnEventComplete or TurnEventError was
	// delivered through subCh. If the channel closes without one (a rare
	// race where the turn ends between Subscribe and the Done check above),
	// we synthesise the terminal frame from the snapshot so the client is
	// never left without a closing event.
	var sawTerminal bool
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-subCh:
			if !ok {
				if !sawTerminal {
					emitTerminalTurnSSE(w, flusher, st.Snapshot())
				}
				return
			}
			if ev.Type == TurnEventComplete || ev.Type == TurnEventError {
				sawTerminal = true
			}
			writeTurnSSEEvent(w, flusher, ev)
		}
	}
}

// emitReconnectFallback handles the reconnect/startup path where the live
// TurnState for turnID is no longer in the store (expired by GC, evicted past
// the retention cap, or lost on a daemon restart). Before falling back to a
// bare turn_expired event — which would leave the reconnecting client with an
// empty assistant bubble — it queries the persisted turn snapshot written by
// the streaming loop. When a matching, still-partial snapshot carries
// accumulated text, that text is replayed as a text_delta so the client
// recovers the mid-turn output. The graceful turn_expired event always follows
// so the SPA refetches its canonical messages and clears the spinner (no
// completion is streamed live from a turn that is already gone).
func (s *Server) emitReconnectFallback(w http.ResponseWriter, flusher http.Flusher, sessionID int64, turnID string) {
	if text := s.partialTurnText(sessionID, turnID); text != "" {
		writeTurnSSEEvent(w, flusher, TurnEvent{Type: TurnEventTextDelta, Data: text})
	}
	writeTurnSSEEvent(w, flusher, TurnEvent{
		Type: TurnEventTurnExpired,
		Data: turnExpiredData{Message: turnExpiredMessage},
	})
}

// partialTurnText returns the accumulated text of the session's latest
// persisted turn snapshot when it belongs to turnID and has not completed, or
// "" when there is nothing worth replaying: no snapshot, a best-effort lookup
// error, a snapshot for a different turn, an empty snapshot, or a completed
// turn whose canonical assistant message the client will refetch on
// turn_expired anyway. Snapshot persistence is best-effort, so a query error
// (e.g. the forge_turn_snapshots table predates the migration) degrades
// silently to the plain turn_expired behaviour.
func (s *Server) partialTurnText(sessionID int64, turnID string) string {
	snap, err := s.db.GetLatestTurnSnapshot(sessionID)
	if err != nil || snap == nil {
		return ""
	}
	if snap.TurnID != turnID || snap.Status == state.ForgeTurnStatusComplete {
		return ""
	}
	return snap.AccumulatedText
}

// handleForgeSessionTurnGet serves a JSON snapshot of the TurnState. The
// snapshot is a point-in-time copy — clients that want incremental updates
// should use the /stream sibling instead of polling this endpoint in a tight
// loop.
func (s *Server) handleForgeSessionTurnGet(w http.ResponseWriter, r *http.Request) {
	st, ok := s.lookupTurn(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, st.Snapshot())
}

// lookupSession resolves the {id} URL param and validates that the signed-in
// user owns the session. Writes the appropriate 4xx/5xx response and returns
// ok=false on any failure so callers can return immediately.
func (s *Server) lookupSession(w http.ResponseWriter, r *http.Request) (int64, bool) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return 0, false
	}
	id, ok := parseForgeSessionID(w, r)
	if !ok {
		return 0, false
	}
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session: "+err.Error())
		return 0, false
	}
	if row == nil || !forgeSessionVisibleTo(row, sess.Username) {
		writeError(w, http.StatusNotFound, "session not found")
		return 0, false
	}
	return id, true
}

// lookupTurn resolves the {id, turn_id} URL params, validates that the
// signed-in user owns the session, and returns the registered TurnState.
// Writes the appropriate 4xx/5xx response and returns ok=false on any
// failure so callers can return immediately. Used by the JSON snapshot
// endpoint, where a missing turn is a plain 404; the SSE stream handler
// instead reports a missing turn as a graceful turn_expired event.
func (s *Server) lookupTurn(w http.ResponseWriter, r *http.Request) (*TurnState, bool) {
	id, ok := s.lookupSession(w, r)
	if !ok {
		return nil, false
	}
	turnID := chi.URLParam(r, "turn_id")
	if turnID == "" {
		writeError(w, http.StatusBadRequest, "turn_id is required")
		return nil, false
	}
	st, ok := s.turnStore.Get(turnID)
	if !ok || st.SessionID != id {
		writeError(w, http.StatusNotFound, "turn not found")
		return nil, false
	}
	return st, true
}

// writeSSEHeaders writes the standard text/event-stream headers plus an
// initial retry hint and flushes so reverse proxies don't buffer the
// response before the first event arrives.
func writeSSEHeaders(w http.ResponseWriter, flusher http.Flusher) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()
}

// writeTurnSSEEvent encodes one TurnEvent as `event: <type>\ndata: <json>\n\n`
// and flushes. JSON marshalling failure is unexpected (Data is set by the
// runner from concrete types) but falls back to a null payload rather than
// dropping the frame entirely.
func writeTurnSSEEvent(w http.ResponseWriter, flusher http.Flusher, ev TurnEvent) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		data = []byte("null")
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
	flusher.Flush()
}

// emitTerminalTurnSSE writes a synthesised complete/error frame from the
// snapshot. Used when the SSE consumer connects after the runner goroutine
// has already closed the Events channel.
func emitTerminalTurnSSE(w http.ResponseWriter, flusher http.Flusher, snap TurnSnapshot) {
	switch snap.Status {
	case TurnStatusComplete:
		writeTurnSSEEvent(w, flusher, TurnEvent{Type: TurnEventComplete, Data: snap.FinalMessageID})
	case TurnStatusError:
		writeTurnSSEEvent(w, flusher, TurnEvent{Type: TurnEventError, Data: snap.Error})
	}
}
