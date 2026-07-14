package state

import (
	"database/sql"
	"errors"
	"time"
)

// ForgeTurnStatus is the lifecycle status of a persisted Beads-Forge turn
// snapshot. It is stored as open-ended TEXT in the forge_turn_snapshots table,
// so callers may extend the set without a schema migration — this typed
// constant list is advisory, matching the pattern used for session status /
// stage / message kind in forge_sessions.go.
type ForgeTurnStatus string

const (
	// ForgeTurnStatusInProgress marks a turn whose AI runner is still
	// streaming; the accumulated_text is a partial, mid-turn snapshot.
	ForgeTurnStatusInProgress ForgeTurnStatus = "in_progress"
	// ForgeTurnStatusComplete marks a turn that finished successfully; the
	// accumulated_text is the final assistant text.
	ForgeTurnStatusComplete ForgeTurnStatus = "complete"
	// ForgeTurnStatusExpired marks a turn that was abandoned or garbage
	// collected before completing; the accumulated_text is whatever partial
	// text had been streamed when it expired.
	ForgeTurnStatusExpired ForgeTurnStatus = "expired"
)

// ForgeTurnSnapshot is the persisted mid-turn state of a single Beads-Forge AI
// turn. It mirrors the in-memory web.TurnState so an in-flight turn survives a
// reconnecting client or a daemon restart: the accumulated assistant text and
// the turn status are checkpointed here as they stream.
//
// Rows are keyed by (SessionID, TurnID). TurnID is the client-facing UUID
// handle returned from POST /turn, so a snapshot is addressed by turn rather
// than by message row.
type ForgeTurnSnapshot struct {
	// SessionID is the owning forge_sessions row.
	SessionID int64
	// TurnID is the UUID handle for the turn (the /turn public identifier).
	TurnID string
	// Status is the turn lifecycle status (in_progress | complete | expired).
	Status ForgeTurnStatus
	// AccumulatedText is the assistant text streamed so far (partial while
	// in_progress, final once complete).
	AccumulatedText string
	// UpdatedAt is the last time this snapshot was written.
	UpdatedAt time.Time
}

// UpsertTurnSnapshot inserts or updates the snapshot for (sessionID, turnID),
// setting the status, accumulated text, and advancing updated_at to now. It is
// safe to call repeatedly as the turn streams: the first call inserts and
// subsequent calls overwrite the same row (no duplicate accumulates). Returns
// the persisted snapshot including the stamped updated_at.
//
// sessionID and turnID are required; an empty turnID is rejected so a snapshot
// can always be addressed by its (session, turn) key.
func (db *DB) UpsertTurnSnapshot(sessionID int64, turnID string, status ForgeTurnStatus, text string) (ForgeTurnSnapshot, error) {
	if sessionID == 0 {
		return ForgeTurnSnapshot{}, errors.New("forge turn snapshot: session_id is required")
	}
	if turnID == "" {
		return ForgeTurnSnapshot{}, errors.New("forge turn snapshot: turn_id is required")
	}
	if status == "" {
		status = ForgeTurnStatusInProgress
	}
	now := time.Now().UTC()
	_, err := db.conn.Exec(
		`INSERT INTO forge_turn_snapshots (session_id, turn_id, status, accumulated_text, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(session_id, turn_id) DO UPDATE SET
		     status = excluded.status,
		     accumulated_text = excluded.accumulated_text,
		     updated_at = excluded.updated_at`,
		sessionID, turnID, string(status), text, now.Format(dbTimeLayout),
	)
	if err != nil {
		return ForgeTurnSnapshot{}, err
	}
	return ForgeTurnSnapshot{
		SessionID:       sessionID,
		TurnID:          turnID,
		Status:          status,
		AccumulatedText: text,
		UpdatedAt:       now,
	}, nil
}

// GetLatestTurnSnapshot returns the most recently updated snapshot for a
// session, or nil when the session has no snapshots. Callers use this to
// resume the newest in-flight turn after a reconnect or restart. Ties on
// updated_at are broken by turn_id descending for a deterministic result.
func (db *DB) GetLatestTurnSnapshot(sessionID int64) (*ForgeTurnSnapshot, error) {
	row := db.conn.QueryRow(
		`SELECT session_id, turn_id, status, accumulated_text, updated_at
		 FROM forge_turn_snapshots
		 WHERE session_id = ?
		 ORDER BY updated_at DESC, turn_id DESC
		 LIMIT 1`,
		sessionID,
	)
	return scanForgeTurnSnapshotRow(row)
}

// GetTurnSnapshot returns the snapshot for a specific (sessionID, turnID), or
// nil when no such row exists. Useful when a client reconnects with a known
// turn UUID rather than asking for the session's latest.
func (db *DB) GetTurnSnapshot(sessionID int64, turnID string) (*ForgeTurnSnapshot, error) {
	row := db.conn.QueryRow(
		`SELECT session_id, turn_id, status, accumulated_text, updated_at
		 FROM forge_turn_snapshots
		 WHERE session_id = ? AND turn_id = ?`,
		sessionID, turnID,
	)
	return scanForgeTurnSnapshotRow(row)
}

// scanForgeTurnSnapshotRow scans a single sql.Row into a ForgeTurnSnapshot,
// returning (nil, nil) when the row does not exist.
func scanForgeTurnSnapshotRow(row *sql.Row) (*ForgeTurnSnapshot, error) {
	var s ForgeTurnSnapshot
	var status, updatedAt string
	err := row.Scan(&s.SessionID, &s.TurnID, &status, &s.AccumulatedText, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Status = ForgeTurnStatus(status)
	s.UpdatedAt = parseTime(updatedAt)
	return &s, nil
}
