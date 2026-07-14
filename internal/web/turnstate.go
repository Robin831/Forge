package web

import (
	"context"
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
	Type TurnEventType `json:"type"`
	Data any           `json:"data,omitempty"`
}

// TurnToolEvent is the Data payload for TurnEventToolUse / TurnEventToolResult.
// Name identifies the invoked tool (empty for a bare tool_result) and ID is the
// provider-assigned tool_use id so an SSE consumer can correlate a result back
// to the invocation that produced it.
type TurnToolEvent struct {
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

// turnBroadcaster fans out events to per-client subscriber channels so
// multiple concurrent SSE consumers each receive a complete, independent
// copy of the event stream rather than competing for a single shared channel.
type turnBroadcaster struct {
	mu     sync.Mutex
	subs   map[int]chan TurnEvent
	next   int
	closed bool
}

func newTurnBroadcaster() turnBroadcaster {
	return turnBroadcaster{subs: make(map[int]chan TurnEvent)}
}

// subscribe registers a new per-client channel with the given buffer size and
// returns its id + channel. If the broadcaster is already closed (the turn is
// done) the returned channel is immediately closed so callers can detect the
// terminal state without missing a closing notification.
func (b *turnBroadcaster) subscribe(bufSize int) (int, chan TurnEvent) {
	ch := make(chan TurnEvent, bufSize)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return -1, ch
	}
	id := b.next
	b.next++
	b.subs[id] = ch
	return id, ch
}

// unsubscribe removes a previously registered subscriber. id=-1 (returned when
// subscribing to a closed broadcaster) is a no-op.
func (b *turnBroadcaster) unsubscribe(id int) {
	if id < 0 {
		return
	}
	b.mu.Lock()
	delete(b.subs, id)
	b.mu.Unlock()
}

// emit delivers ev to every registered subscriber without blocking; events are
// dropped for any subscriber whose buffer is full.
func (b *turnBroadcaster) emit(ev TurnEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// closeAll marks the broadcaster closed, closes every registered subscriber
// channel, and clears the subscriber map. Any subsequent subscribe call will
// receive an immediately-closed channel.
func (b *turnBroadcaster) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = make(map[int]chan TurnEvent)
}

// TurnState holds the in-flight state of a single Beads-Forge AI turn.
//
// It is created by the /turn handler before launching the background
// goroutine, and read by the SSE / polling endpoints (sibling sub-tasks).
// Call Subscribe to obtain a per-client channel that receives every event
// emitted by the runner; Done signals the terminal state.
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

	bcast      turnBroadcaster
	subBufSize int
	// Done is closed by the goroutine when the turn reaches a terminal state.
	// Callers that need to wait for completion (tests, polling endpoints) can
	// select on this without consuming events.
	Done chan struct{}
}

// newTurnState constructs a TurnState in TurnStatusPending. eventsBuffer
// controls the per-subscriber channel buffer size returned by Subscribe; <0
// uses a default of 64. Pass 0 to create unbuffered subscriber channels
// (fine for tests with a synchronous consumer, not for production SSE).
func newTurnState(id string, sessionID int64, eventsBuffer int) *TurnState {
	if eventsBuffer < 0 {
		eventsBuffer = 64
	}
	return &TurnState{
		ID:         id,
		SessionID:  sessionID,
		status:     TurnStatusPending,
		bcast:      newTurnBroadcaster(),
		subBufSize: eventsBuffer,
		Done:       make(chan struct{}),
	}
}

// Subscribe returns a dedicated per-client channel that receives every event
// the runner emits. The channel is closed when the turn ends. When the turn
// has already ended the returned channel is immediately closed so consumers
// can detect that without missing a terminal notification.
//
// The goroutine launched here unsubscribes when ctx is cancelled (typically
// when the HTTP request ends), keeping the broadcaster map tidy.
func (t *TurnState) Subscribe(ctx context.Context) <-chan TurnEvent {
	id, ch := t.bcast.subscribe(t.subBufSize)
	go func() {
		<-ctx.Done()
		t.bcast.unsubscribe(id)
	}()
	return ch
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
// log so the polling endpoint can replay tool activity from the snapshot.
// Callers that also need fanout should call Emit separately.
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

// Emit broadcasts ev to all per-client subscriber channels without blocking.
// Events are dropped for any subscriber whose buffer is full; the SSE
// endpoint replays missed state via Snapshot on initial connect.
func (t *TurnState) Emit(ev TurnEvent) {
	t.bcast.emit(ev)
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
