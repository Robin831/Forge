package web

import (
	"strings"
	"sync"
)

// TurnStatus is the lifecycle phase of an in-flight Beads-Forge turn.
type TurnStatus string

const (
	// TurnStatusPending is the initial state, set when the handler creates the
	// store entry but the goroutine has not yet begun.
	TurnStatusPending TurnStatus = "pending"
	// TurnStatusRunning is set when the goroutine has started invoking the AI
	// runner.
	TurnStatusRunning TurnStatus = "running"
	// TurnStatusComplete is the terminal success state.
	TurnStatusComplete TurnStatus = "complete"
	// TurnStatusError is the terminal failure state. State.Err holds the
	// underlying error.
	TurnStatusError TurnStatus = "error"
)

// TurnEventType is the typed kind of an event emitted on TurnState.Events.
// Mirrors the SSE event names the sibling streaming endpoint will surface
// to clients.
type TurnEventType string

const (
	TurnEventTextDelta  TurnEventType = "text_delta"
	TurnEventToolUse    TurnEventType = "tool_use"
	TurnEventToolResult TurnEventType = "tool_result"
	// TurnEventMessage carries a persisted assistant message. Data is the
	// state.ForgeSessionMessage row so SSE consumers can replay the chat
	// view in real time.
	TurnEventMessage TurnEventType = "message"
	TurnEventComplete TurnEventType = "complete"
	TurnEventError    TurnEventType = "error"
)

// TurnEvent is one item delivered on TurnState.Events. Data's concrete type
// depends on Type — consumers should type-switch on it.
type TurnEvent struct {
	Type TurnEventType
	Data any
}

// TurnState holds the in-flight state of a single Beads-Forge AI turn.
//
// It is created by the /turn handler before launching the background
// goroutine, and read by the SSE / polling endpoints (sibling sub-tasks).
// The Events channel fans out incremental updates; the Done channel signals
// terminal status so callers can wait without polling.
//
// All mutating helpers take the mutex internally; readers should call
// Snapshot rather than touching the unexported fields directly.
type TurnState struct {
	// ID is the UUID assigned to this turn. It is the public handle returned
	// from POST /turn and used in the SSE / polling URLs.
	ID string
	// SessionID is the owning forge_session row.
	SessionID int64

	mu             sync.RWMutex
	status         TurnStatus
	text           strings.Builder
	toolEvents     []TurnEvent
	finalMessageID int64
	err            error

	// Events fans out incremental updates to SSE subscribers. Closed when the
	// goroutine exits (after Done). Buffered so a slow or absent consumer
	// cannot block the producer indefinitely; events overflow is dropped (the
	// SSE endpoint replays missed state via Snapshot).
	Events chan TurnEvent
	// Done is closed by the goroutine when the turn reaches a terminal state.
	// Callers that need to wait for completion (tests, polling endpoints) can
	// select on this without consuming events.
	Done chan struct{}
}

// newTurnState constructs a TurnState in TurnStatusPending. eventsBuffer sizes
// the events channel; a buffer of 0 makes the producer block on the
// consumer, which is fine for tests but bad for real SSE traffic.
func newTurnState(id string, sessionID int64, eventsBuffer int) *TurnState {
	if eventsBuffer <= 0 {
		eventsBuffer = 64
	}
	return &TurnState{
		ID:        id,
		SessionID: sessionID,
		status:    TurnStatusPending,
		Events:    make(chan TurnEvent, eventsBuffer),
		Done:      make(chan struct{}),
	}
}

// Status returns the current status.
func (t *TurnState) Status() TurnStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.status
}

// setStatus updates the status under the mutex.
func (t *TurnState) setStatus(s TurnStatus) {
	t.mu.Lock()
	t.status = s
	t.mu.Unlock()
}

// AppendText accumulates streamed assistant text. The full accumulated text
// is available via Snapshot.Text.
func (t *TurnState) AppendText(chunk string) {
	t.mu.Lock()
	t.text.WriteString(chunk)
	t.mu.Unlock()
}

// RecordToolEvent appends a tool_use / tool_result event to the persisted
// log alongside emitting it on the channel. Callers should use this rather
// than mutating Events directly so the polling endpoint can replay tool
// activity from the snapshot.
func (t *TurnState) RecordToolEvent(ev TurnEvent) {
	t.mu.Lock()
	t.toolEvents = append(t.toolEvents, ev)
	t.mu.Unlock()
}

// SetFinalMessageID records the ID of the last assistant message persisted
// during this turn. SSE clients use this on the `complete` event to fetch
// the canonical row from the messages list.
func (t *TurnState) SetFinalMessageID(id int64) {
	t.mu.Lock()
	t.finalMessageID = id
	t.mu.Unlock()
}

// SetError records the terminal error and transitions status to error.
func (t *TurnState) SetError(err error) {
	t.mu.Lock()
	t.err = err
	t.status = TurnStatusError
	t.mu.Unlock()
}

// Err returns the recorded error, if any.
func (t *TurnState) Err() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.err
}

// FinalMessageID returns the id of the last persisted assistant message.
// Zero before the turn completes.
func (t *TurnState) FinalMessageID() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.finalMessageID
}

// Text returns the accumulated assistant text so far.
func (t *TurnState) Text() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.text.String()
}

// TurnSnapshot is the immutable view of a TurnState returned by Snapshot.
// The polling endpoint serialises this directly to JSON.
type TurnSnapshot struct {
	ID             string      `json:"id"`
	SessionID      int64       `json:"session_id"`
	Status         TurnStatus  `json:"status"`
	Text           string      `json:"text,omitempty"`
	ToolEvents     []TurnEvent `json:"tool_events,omitempty"`
	FinalMessageID int64       `json:"final_message_id,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// Snapshot returns a point-in-time copy of the state. Safe to call from any
// goroutine.
func (t *TurnState) Snapshot() TurnSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snap := TurnSnapshot{
		ID:             t.ID,
		SessionID:      t.SessionID,
		Status:         t.status,
		Text:           t.text.String(),
		FinalMessageID: t.finalMessageID,
	}
	if len(t.toolEvents) > 0 {
		snap.ToolEvents = make([]TurnEvent, len(t.toolEvents))
		copy(snap.ToolEvents, t.toolEvents)
	}
	if t.err != nil {
		snap.Error = t.err.Error()
	}
	return snap
}

// Emit pushes an event on the Events channel without blocking. If the
// channel is full (slow consumer or no subscriber), the event is dropped —
// the SSE endpoint replays missed state via Snapshot on initial connect.
func (t *TurnState) Emit(ev TurnEvent) {
	select {
	case t.Events <- ev:
	default:
	}
}

// TurnStore is the process-local registry of in-flight and recently-finished
// turns. Keyed by turn UUID and guarded by an RWMutex so the SSE and polling
// endpoints can read concurrently with the handler creating new turns.
type TurnStore struct {
	mu    sync.RWMutex
	turns map[string]*TurnState
}

// NewTurnStore constructs an empty store.
func NewTurnStore() *TurnStore {
	return &TurnStore{turns: make(map[string]*TurnState)}
}

// New creates a fresh TurnState and registers it in the store.
func (s *TurnStore) New(id string, sessionID int64) *TurnState {
	t := newTurnState(id, sessionID, 64)
	s.mu.Lock()
	s.turns[id] = t
	s.mu.Unlock()
	return t
}

// Get looks up a turn by id. ok is false when the id is unknown.
func (s *TurnStore) Get(id string) (*TurnState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.turns[id]
	return t, ok
}

// Delete removes a turn from the store. Used by future garbage-collection
// once SSE consumers have caught up; the foundation bead retains everything
// for the process lifetime.
func (s *TurnStore) Delete(id string) {
	s.mu.Lock()
	delete(s.turns, id)
	s.mu.Unlock()
}

// Len returns the number of registered turns. Test helper.
func (s *TurnStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.turns)
}
