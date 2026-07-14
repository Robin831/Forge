package web

import (
	"log/slog"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// turnSnapshotSink is the subset of *state.DB the snapshot writer needs. An
// interface keeps the writer unit-testable without a real database and lets a
// nil sink degrade to a no-op (persistence is best-effort).
type turnSnapshotSink interface {
	UpsertTurnSnapshot(sessionID int64, turnID string, status state.ForgeTurnStatus, text string) (state.ForgeTurnSnapshot, error)
}

// Throttle defaults for mid-turn snapshot persistence. A snapshot is written
// no more often than every defaultSnapshotMinInterval, unless the accumulated
// text has grown by at least defaultSnapshotMinByteDelta since the last write —
// in which case it flushes promptly so a fast burst is not held back by the
// interval. Together they bound DB churn while keeping the persisted snapshot
// reasonably fresh for a reconnecting client.
const (
	defaultSnapshotMinInterval  = 750 * time.Millisecond
	defaultSnapshotMinByteDelta = 256
)

// turnSnapshotWriter persists throttled mid-turn snapshots for a single turn
// via the store layer's UpsertTurnSnapshot. It is driven from the streaming
// loop: Update is called as text accumulates (writes are rate-limited by a min
// interval and a byte delta), and Finalize forces a last write with the
// terminal status when the turn ends.
//
// Persistence is best-effort. If a write fails — most notably because the
// forge_turn_snapshots table is absent (the migration has not been applied yet
// on an older database) — the writer logs once and disables itself so a missing
// table never breaks the streaming turn. All methods are safe for concurrent
// use; the streaming callback and the finalization path may run on different
// goroutines.
type turnSnapshotWriter struct {
	db        turnSnapshotSink
	sessionID int64
	turnID    string
	logger    *slog.Logger

	minInterval  time.Duration
	minByteDelta int
	now          func() time.Time

	mu          sync.Mutex
	disabled    bool
	lastWriteAt time.Time
	lastLen     int
}

// newTurnSnapshotWriter builds a writer with the default throttle parameters.
// A nil sink (or one that fails on first use) yields a no-op writer so callers
// never have to nil-check before invoking Update / Finalize.
func newTurnSnapshotWriter(db turnSnapshotSink, sessionID int64, turnID string, logger *slog.Logger) *turnSnapshotWriter {
	w := &turnSnapshotWriter{
		db:           db,
		sessionID:    sessionID,
		turnID:       turnID,
		logger:       logger,
		minInterval:  defaultSnapshotMinInterval,
		minByteDelta: defaultSnapshotMinByteDelta,
		now:          time.Now,
	}
	// A nil sink, an empty turn id, or a zero session can never be persisted;
	// disable up front so Update / Finalize are cheap no-ops.
	if db == nil || turnID == "" || sessionID == 0 {
		w.disabled = true
	}
	return w
}

// Update persists an in-progress snapshot of the accumulated text if the
// throttle allows it. It writes when either the min interval has elapsed since
// the last write or the text has grown by at least the byte delta; calls that
// add no new bytes are ignored. Cheap to call on every streamed chunk.
func (w *turnSnapshotWriter) Update(text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disabled {
		return
	}
	now := w.clock()
	byteDelta := len(text) - w.lastLen
	if byteDelta <= 0 {
		// No new content (or a shorter string, which we never regress the
		// snapshot to) — nothing worth a write.
		return
	}
	// Hold the write back only while both limits say "too soon": not enough
	// time has passed AND not enough new bytes have accumulated. A large burst
	// flushes immediately even within the interval; a slow trickle waits for
	// the interval. The first write (zero lastWriteAt) always passes.
	if now.Sub(w.lastWriteAt) < w.minInterval && byteDelta < w.minByteDelta {
		return
	}
	w.writeLocked(text, state.ForgeTurnStatusInProgress, now)
}

// Finalize forces a last write with the given terminal status, bypassing the
// throttle so the completed (or expired) text is always persisted. Safe to call
// even when no in-progress snapshot was ever written.
func (w *turnSnapshotWriter) Finalize(text string, status state.ForgeTurnStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disabled {
		return
	}
	w.writeLocked(text, status, w.clock())
}

// writeLocked performs the actual upsert and records the write watermark. On
// error it disables the writer so the turn is never blocked by snapshot
// persistence problems (e.g. a missing table before the migration runs).
// Callers must hold w.mu.
func (w *turnSnapshotWriter) writeLocked(text string, status state.ForgeTurnStatus, now time.Time) {
	if _, err := w.db.UpsertTurnSnapshot(w.sessionID, w.turnID, status, text); err != nil {
		if w.logger != nil {
			w.logger.Warn("turn snapshot persistence disabled",
				"turn_id", w.turnID,
				"session_id", w.sessionID,
				"error", err)
		}
		w.disabled = true
		return
	}
	w.lastWriteAt = now
	w.lastLen = len(text)
}

// clock returns the injected clock, defaulting to time.Now.
func (w *turnSnapshotWriter) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}
