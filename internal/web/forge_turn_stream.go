package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
// don't buffer the response, then forwards every TurnEvent it receives on
// the TurnState.Events channel. When the channel closes (the runner
// goroutine has reached a terminal state) the handler returns.
//
// Clients that connect after the turn has already terminated would otherwise
// see an empty stream — the Events channel is already closed and the
// terminal event has been drained. We catch that case up front and emit a
// synthesised complete/error event from the snapshot so late SSE consumers
// still observe a deterministic terminal frame before the connection ends.
func (s *Server) handleForgeSessionTurnStream(w http.ResponseWriter, r *http.Request) {
	st, ok := s.lookupTurn(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// Late-connect: the runner goroutine has already exited and closed both
	// channels. Synthesise a terminal frame from the snapshot so the client
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

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-st.Events:
			if !ok {
				return
			}
			writeTurnSSEEvent(w, flusher, ev)
		}
	}
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

// lookupTurn resolves the {id, turn_id} URL params, validates that the
// signed-in user owns the session, and returns the registered TurnState.
// Writes the appropriate 4xx/5xx response and returns ok=false on any
// failure so callers can return immediately.
func (s *Server) lookupTurn(w http.ResponseWriter, r *http.Request) (*TurnState, bool) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	id, ok := parseForgeSessionID(w, r)
	if !ok {
		return nil, false
	}
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session: "+err.Error())
		return nil, false
	}
	if row == nil || !forgeSessionVisibleTo(row, sess.Username) {
		writeError(w, http.StatusNotFound, "session not found")
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
