package state

import (
	"database/sql"
	"errors"
	"time"
)

// ForgeSession is one conversation row that backs the Beads-Forge page in
// Hearth 2.0. Each session pairs a list of messages (forge_session_messages)
// with metadata about who started it and what stage it is in.
//
// Status values are an open-ended TEXT to avoid an enum migration when later
// beads add stages (plan_ready, beads_created, etc.). The foundation bead
// uses only "draft" and "archived".
type ForgeSession struct {
	ID        int64
	Title     string
	Status    string
	Anvil     string
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ForgeSessionMessage is one entry in a forge session conversation. Roles
// follow the chat-style convention: "user", "assistant", "system". The
// foundation bead only writes "user" entries through its API; the AI bead
// will add "assistant" (and any "system" framing) once claude is wired in.
type ForgeSessionMessage struct {
	ID        int64
	SessionID int64
	Role      string
	Content   string
	CreatedAt time.Time
}

// Permitted forge session status values. Other beads may add more — the DB
// stores TEXT so this is an advisory list rather than a strict enum.
const (
	ForgeSessionStatusDraft    = "draft"
	ForgeSessionStatusArchived = "archived"
)

// Permitted forge session message roles.
const (
	ForgeMessageRoleUser      = "user"
	ForgeMessageRoleAssistant = "assistant"
	ForgeMessageRoleSystem    = "system"
)

// CreateForgeSession inserts a new session row, returning the assigned ID
// and the canonical timestamps. CreatedBy may be empty when the caller is
// unable to attribute the session to a user.
func (db *DB) CreateForgeSession(s ForgeSession) (ForgeSession, error) {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	if s.Status == "" {
		s.Status = ForgeSessionStatusDraft
	}
	res, err := db.conn.Exec(
		`INSERT INTO forge_sessions (title, status, anvil, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		s.Title, s.Status, s.Anvil, s.CreatedBy,
		s.CreatedAt.UTC().Format(dbTimeLayout),
		s.UpdatedAt.UTC().Format(dbTimeLayout),
	)
	if err != nil {
		return ForgeSession{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ForgeSession{}, err
	}
	s.ID = id
	return s, nil
}

// GetForgeSession returns the session with the given ID, or nil if no row
// exists. Errors other than ErrNoRows are returned to the caller.
func (db *DB) GetForgeSession(id int64) (*ForgeSession, error) {
	row := db.conn.QueryRow(
		`SELECT id, title, status, anvil, created_by, created_at, updated_at
		 FROM forge_sessions WHERE id = ?`, id,
	)
	return scanForgeSessionRow(row)
}

// ListForgeSessions returns sessions ordered by most-recent activity first.
// When createdBy is non-empty, only sessions started by that user are
// returned; this keeps the sidebar scoped to the signed-in user. Limit is
// clamped to [1, 200].
func (db *DB) ListForgeSessions(createdBy string, limit int) ([]ForgeSession, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT id, title, status, anvil, created_by, created_at, updated_at
	      FROM forge_sessions`
	args := []any{}
	if createdBy != "" {
		q += ` WHERE created_by = ?`
		args = append(args, createdBy)
	}
	q += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForgeSession{}
	for rows.Next() {
		s, err := scanForgeSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// UpdateForgeSession applies a partial update to the session row. Only the
// fields whose pointer is non-nil are written, so callers can rename a
// session without touching its status (or vice versa). The updated_at
// column is always advanced.
func (db *DB) UpdateForgeSession(id int64, title, status *string) error {
	if title == nil && status == nil {
		// Still bump updated_at so the sidebar reorders even on a no-op
		// touch (e.g. when a message is appended via TouchForgeSession).
		_, err := db.conn.Exec(
			`UPDATE forge_sessions SET updated_at = ? WHERE id = ?`,
			time.Now().UTC().Format(dbTimeLayout), id,
		)
		return err
	}
	q := `UPDATE forge_sessions SET updated_at = ?`
	args := []any{time.Now().UTC().Format(dbTimeLayout)}
	if title != nil {
		q += `, title = ?`
		args = append(args, *title)
	}
	if status != nil {
		q += `, status = ?`
		args = append(args, *status)
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	_, err := db.conn.Exec(q, args...)
	return err
}

// TouchForgeSession bumps updated_at to now without changing other fields.
// Called after appending a message so the sidebar reorders correctly.
func (db *DB) TouchForgeSession(id int64) error {
	return db.UpdateForgeSession(id, nil, nil)
}

// DeleteForgeSession removes a session and its messages. Foreign keys are
// declared with ON DELETE CASCADE in the schema, but SQLite only honours
// that when foreign_keys = ON, so we delete messages explicitly to be
// independent of the connection-level pragma.
func (db *DB) DeleteForgeSession(id int64) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM forge_session_messages WHERE session_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM forge_sessions WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendForgeSessionMessage inserts a new message and bumps the parent
// session's updated_at in a single transaction. Returns the persisted
// message with its assigned ID and timestamp.
func (db *DB) AppendForgeSessionMessage(m ForgeSessionMessage) (ForgeSessionMessage, error) {
	if m.SessionID == 0 {
		return ForgeSessionMessage{}, errors.New("forge session message: session_id is required")
	}
	if m.Role == "" {
		return ForgeSessionMessage{}, errors.New("forge session message: role is required")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return ForgeSessionMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`INSERT INTO forge_session_messages (session_id, role, content, created_at)
		 VALUES (?, ?, ?, ?)`,
		m.SessionID, m.Role, m.Content,
		m.CreatedAt.UTC().Format(dbTimeLayout),
	)
	if err != nil {
		return ForgeSessionMessage{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ForgeSessionMessage{}, err
	}
	if _, err := tx.Exec(
		`UPDATE forge_sessions SET updated_at = ? WHERE id = ?`,
		m.CreatedAt.UTC().Format(dbTimeLayout), m.SessionID,
	); err != nil {
		return ForgeSessionMessage{}, err
	}
	if err := tx.Commit(); err != nil {
		return ForgeSessionMessage{}, err
	}
	m.ID = id
	return m, nil
}

// ListForgeSessionMessages returns the messages for a session in insertion
// order (oldest first), suitable for direct rendering in the chat view.
func (db *DB) ListForgeSessionMessages(sessionID int64) ([]ForgeSessionMessage, error) {
	rows, err := db.conn.Query(
		`SELECT id, session_id, role, content, created_at
		 FROM forge_session_messages
		 WHERE session_id = ?
		 ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForgeSessionMessage{}
	for rows.Next() {
		var m ForgeSessionMessage
		var createdAt string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ForgeSessionWithCount pairs a session with its pre-computed message count.
// Returned by ListForgeSessionsWithCounts to avoid a separate COUNT per row.
type ForgeSessionWithCount struct {
	ForgeSession
	MessageCount int
}

// ListForgeSessionsWithCounts returns sessions with their message counts using
// a single LEFT JOIN query, eliminating the N+1 pattern of a per-row COUNT.
// When createdBy is non-empty, rows owned by that user plus unattributed rows
// (created_by="") are returned, consistent with forgeSessionVisibleTo.
func (db *DB) ListForgeSessionsWithCounts(createdBy string, limit int) ([]ForgeSessionWithCount, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT s.id, s.title, s.status, s.anvil, s.created_by, s.created_at, s.updated_at,
	             COUNT(m.id) AS message_count
	      FROM forge_sessions s
	      LEFT JOIN forge_session_messages m ON m.session_id = s.id`
	args := []any{}
	if createdBy != "" {
		q += ` WHERE (s.created_by = ? OR s.created_by = '')`
		args = append(args, createdBy)
	}
	q += ` GROUP BY s.id ORDER BY s.updated_at DESC, s.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ForgeSessionWithCount{}
	for rows.Next() {
		var sc ForgeSessionWithCount
		var createdAt, updatedAt string
		if err := rows.Scan(
			&sc.ID, &sc.Title, &sc.Status, &sc.Anvil, &sc.CreatedBy,
			&createdAt, &updatedAt, &sc.MessageCount,
		); err != nil {
			return nil, err
		}
		sc.CreatedAt = parseTime(createdAt)
		sc.UpdatedAt = parseTime(updatedAt)
		out = append(out, sc)
	}
	return out, rows.Err()
}

// CountForgeSessionMessages returns the number of persisted messages for a
// session. Used by the API to populate the sidebar preview without sending
// the full message list.
func (db *DB) CountForgeSessionMessages(sessionID int64) (int, error) {
	row := db.conn.QueryRow(
		`SELECT COUNT(*) FROM forge_session_messages WHERE session_id = ?`,
		sessionID,
	)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// scanForgeSessionRow scans a single sql.Row into a ForgeSession. Returns
// (nil, nil) when the row does not exist.
func scanForgeSessionRow(row *sql.Row) (*ForgeSession, error) {
	var s ForgeSession
	var createdAt, updatedAt string
	err := row.Scan(&s.ID, &s.Title, &s.Status, &s.Anvil, &s.CreatedBy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = parseTime(createdAt)
	s.UpdatedAt = parseTime(updatedAt)
	return &s, nil
}

// scanForgeSessionRows scans the next row of a sql.Rows cursor into a
// ForgeSession. The caller is responsible for advancing the cursor.
func scanForgeSessionRows(rows *sql.Rows) (*ForgeSession, error) {
	var s ForgeSession
	var createdAt, updatedAt string
	if err := rows.Scan(&s.ID, &s.Title, &s.Status, &s.Anvil, &s.CreatedBy, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	s.CreatedAt = parseTime(createdAt)
	s.UpdatedAt = parseTime(updatedAt)
	return &s, nil
}
