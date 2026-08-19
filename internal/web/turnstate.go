package web

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
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
	TurnEventMessage  TurnEventType = "message"
	TurnEventComplete TurnEventType = "complete"
	TurnEventError    TurnEventType = "error"
	// TurnEventTurnExpired is emitted on the SSE stream when a reconnecting
	// client asks for a turn that is no longer in the store — expired by GC,
	// evicted past the retention cap, or lost on a daemon restart. It carries
	// a turnExpiredData payload and lets the SPA refetch the canonical
	// messages (the same path it takes on `complete`) instead of receiving a
	// 404 that would leave a dangling spinner.
	TurnEventTurnExpired TurnEventType = "turn_expired"
)

// turnExpiredMessage is the human-readable payload string for a turn_expired
// SSE event.
const turnExpiredMessage = "turn expired — refresh session"

// turnExpiredData is the Data payload for a TurnEventTurnExpired event.
type turnExpiredData struct {
	Message string `json:"message"`
}

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
	// completedAt is stamped the first time the turn reaches a terminal state
	// (complete or error). Zero while the turn is still pending/running, which
	// keeps in-flight turns immune to expiry and eviction.
	completedAt time.Time
	// now supplies the clock used to stamp completedAt. Defaults to time.Now;
	// the owning TurnStore injects its own clock so tests can drive expiry
	// deterministically without sleeping.
	now func() time.Time

	bcast      turnBroadcaster
	subBufSize int
	// Done is closed by the goroutine when the turn reaches a terminal state.
	// Callers that need to wait for completion (tests, polling endpoints) can
	// select on this without consuming events.
	Done chan struct{}
}

// isTerminalStatus reports whether s is a terminal turn state.
func isTerminalStatus(s TurnStatus) bool {
	return s == TurnStatusComplete || s == TurnStatusError
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
		now:        time.Now,
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

// setStatus updates the status under the mutex. Reaching a terminal state
// (complete or error) stamps completedAt so the TurnStore can expire and
// evict the turn once it is no longer in flight.
func (t *TurnState) setStatus(s TurnStatus) {
	t.mu.Lock()
	t.status = s
	if isTerminalStatus(s) && t.completedAt.IsZero() {
		t.completedAt = t.clock()
	}
	t.mu.Unlock()
}

// clock returns the injected clock, defaulting to time.Now when unset (e.g.
// a TurnState constructed directly in a test without newTurnState).
func (t *TurnState) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// completionTime returns the terminal-transition timestamp and whether the
// turn has reached a terminal state. Safe for concurrent use.
func (t *TurnState) completionTime() (time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.completedAt, !t.completedAt.IsZero()
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
	if t.completedAt.IsZero() {
		t.completedAt = t.clock()
	}
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

// defaultTurnExpiry / defaultTurnRetentionCap mirror the config-package
// defaults (config.DefaultForgeChatTurnExpiry / ...RetentionCap). They are
// duplicated here rather than imported so the store stays usable — with sane
// GC bounds — when constructed directly (tests, foundation callers) without
// the daemon threading config through Configure.
const (
	defaultTurnExpiry       = 30 * time.Minute
	defaultTurnRetentionCap = 1000
	// defaultTurnSweepInterval is how often the background sweeper reclaims
	// expired turns when Start is called without an explicit interval.
	defaultTurnSweepInterval = time.Minute
)

// TurnStore is the process-local registry of in-flight and recently-finished
// turns. Keyed by turn UUID and guarded by an RWMutex so the SSE and polling
// endpoints can read concurrently with the handler creating new turns.
//
// Completed turns are garbage-collected two ways: an expiry sweep drops turns
// whose terminal transition is older than expiry, and a retention cap evicts
// the oldest completed turns once the total count exceeds cap. In-flight
// (pending/running) turns are never expired or evicted.
type TurnStore struct {
	mu    sync.RWMutex
	turns map[string]*TurnState

	// expiry is how long a completed turn is retained before the sweep (and
	// lazy Get check) treat it as gone. <=0 disables expiry.
	expiry time.Duration
	// retentionCap bounds the number of retained turns; the oldest completed
	// turns are evicted first when exceeded. <=0 disables the cap.
	retentionCap int
	// now is the injectable clock used for expiry comparisons. Defaults to
	// time.Now; tests replace it to drive expiry timing deterministically.
	now func() time.Time
}

// NewTurnStore constructs an empty store with the default expiry (30m) and
// retention cap (1000). Call Configure to override before serving.
func NewTurnStore() *TurnStore {
	return &TurnStore{
		turns:        make(map[string]*TurnState),
		expiry:       defaultTurnExpiry,
		retentionCap: defaultTurnRetentionCap,
		now:          time.Now,
	}
}

// Configure sets the GC parameters. A non-positive expiry disables the expiry
// sweep; a non-positive cap disables retention-cap eviction. Intended to be
// called once at startup (before Start) from the daemon's resolved config.
func (s *TurnStore) Configure(expiry time.Duration, retentionCap int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiry = expiry
	s.retentionCap = retentionCap
}

// New creates a fresh TurnState and registers it in the store, enforcing the
// retention cap once the new turn is added.
func (s *TurnStore) New(id string, sessionID int64) *TurnState {
	t := newTurnState(id, sessionID, 64)
	s.mu.Lock()
	if s.now != nil {
		t.now = s.now
	}
	s.turns[id] = t
	s.enforceCapLocked()
	s.mu.Unlock()
	return t
}

// Get looks up a turn by id. ok is false when the id is unknown or when the
// turn has expired (completed longer ago than expiry) — an expired turn is
// removed lazily so a reconnecting SSE client observes it as gone even
// between background sweeps.
func (s *TurnStore) Get(id string) (*TurnState, bool) {
	s.mu.RLock()
	t, ok := s.turns[id]
	expired := ok && s.isExpiredLocked(t)
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if expired {
		s.Delete(id)
		return nil, false
	}
	return t, true
}

// Delete removes a turn from the store.
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

// clockNow returns the store's injected clock, defaulting to time.Now.
func (s *TurnStore) clockNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// isExpiredLocked reports whether t has been completed for longer than expiry.
// In-flight turns (no completion timestamp) are never expired. Callers must
// hold at least a read lock (it only reads store fields; the turn's own state
// is read under the turn's mutex).
func (s *TurnStore) isExpiredLocked(t *TurnState) bool {
	if s.expiry <= 0 {
		return false
	}
	completedAt, done := t.completionTime()
	if !done {
		return false
	}
	return s.clockNow().Sub(completedAt) >= s.expiry
}

// sweep removes every expired completed turn and then enforces the retention
// cap. Returns the number of turns removed. Safe for concurrent use; called
// by the background sweeper and directly from tests.
func (s *TurnStore) sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, t := range s.turns {
		if s.isExpiredLocked(t) {
			delete(s.turns, id)
			removed++
		}
	}
	removed += s.enforceCapLocked()
	return removed
}

// enforceCapLocked evicts the oldest completed turns until the total count is
// within retentionCap. In-flight turns are never evicted, so the count may
// remain above the cap when too many turns are simultaneously in flight.
// Returns the number of turns evicted. Callers must hold the write lock.
func (s *TurnStore) enforceCapLocked() int {
	if s.retentionCap <= 0 || len(s.turns) <= s.retentionCap {
		return 0
	}
	type entry struct {
		id string
		at time.Time
	}
	completed := make([]entry, 0, len(s.turns))
	for id, t := range s.turns {
		if at, done := t.completionTime(); done {
			completed = append(completed, entry{id: id, at: at})
		}
	}
	// Oldest completion first so we evict the least-recently-finished turns.
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].at.Before(completed[j].at)
	})
	excess := len(s.turns) - s.retentionCap
	evicted := 0
	for i := 0; i < len(completed) && evicted < excess; i++ {
		delete(s.turns, completed[i].id)
		evicted++
	}
	return evicted
}

// StartSweeper runs the background expiry/eviction sweep on a ticker until ctx
// is cancelled. interval <=0 uses defaultTurnSweepInterval. The daemon starts
// this from Server.Start so shutdown (ctx cancellation) stops it cleanly.
func (s *TurnStore) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultTurnSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}
