// Package state manages the SQLite state database for The Forge.
//
// The database lives at ~/.forge/state.db and tracks:
//   - workers: active and historical Smith worker processes
//   - prs: pull requests created by Forge across anvils
//   - events: timestamped log of all significant actions
package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	_ "modernc.org/sqlite" // Pure-Go SQLite driver
)

// DB wraps a SQLite connection for Forge state.
type DB struct {
	conn *sql.DB
	path string
}

// DefaultPath returns ~/.forge/state.db.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(home, ".forge", "state.db"), nil
}

// Open opens or creates the state database at the given path.
// If path is empty, DefaultPath() is used.
func Open(path string) (*DB, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}

	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening state database: %w", err)
	}

	// Enable WAL mode and set pragmas for concurrency
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}

	db := &DB{conn: conn, path: path}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating state database: %w", err)
	}

	return db, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// Path returns the database file path.
func (db *DB) Path() string {
	return db.path
}

// Conn returns the underlying sql.DB for direct queries.
func (db *DB) Conn() *sql.DB {
	return db.conn
}

// migrate creates or updates the database schema.
func (db *DB) migrate() error {
	if _, err := db.conn.Exec(schema); err != nil {
		return err
	}
	// Additive migrations for existing databases
	if _, err := db.conn.Exec(`ALTER TABLE workers ADD COLUMN phase TEXT NOT NULL DEFAULT ''`); err != nil {
		// Ignore error if column already exists (SQLite doesn't have IF NOT EXISTS for columns)
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	if _, err := db.conn.Exec(`ALTER TABLE prs ADD COLUMN is_conflicting INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	if _, err := db.conn.Exec(`ALTER TABLE prs ADD COLUMN ci_passing INTEGER NOT NULL DEFAULT 1`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	if _, err := db.conn.Exec(`ALTER TABLE prs ADD COLUMN ci_fix_count INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	if _, err := db.conn.Exec(`ALTER TABLE prs ADD COLUMN review_fix_count INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS workers (
    id          TEXT PRIMARY KEY,
    bead_id     TEXT NOT NULL,
    anvil       TEXT NOT NULL,
    branch      TEXT NOT NULL DEFAULT '',
    pid         INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'pending',
    phase       TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    completed_at TEXT,
    log_path    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS prs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    number          INTEGER NOT NULL,
    anvil           TEXT NOT NULL,
    bead_id         TEXT NOT NULL DEFAULT '',
    branch          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'open',
    created_at      TEXT NOT NULL,
    last_checked    TEXT,
    is_conflicting  INTEGER NOT NULL DEFAULT 0,
    ci_passing      INTEGER NOT NULL DEFAULT 1,
    ci_fix_count    INTEGER NOT NULL DEFAULT 0,
    review_fix_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TEXT NOT NULL,
    type      TEXT NOT NULL,
    message   TEXT NOT NULL DEFAULT '',
    bead_id   TEXT NOT NULL DEFAULT '',
    anvil     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_workers_status ON workers(status);
CREATE INDEX IF NOT EXISTS idx_workers_anvil ON workers(anvil);
CREATE INDEX IF NOT EXISTS idx_prs_status ON prs(status);
CREATE INDEX IF NOT EXISTS idx_prs_anvil ON prs(anvil);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);
CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);

CREATE TABLE IF NOT EXISTS retries (
    bead_id      TEXT NOT NULL,
    anvil        TEXT NOT NULL,
    retry_count  INTEGER NOT NULL DEFAULT 0,
    next_retry   TEXT,
    needs_human  INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (bead_id, anvil)
);

CREATE INDEX IF NOT EXISTS idx_retries_needs_human ON retries(needs_human);

CREATE TABLE IF NOT EXISTS bead_costs (
    bead_id          TEXT NOT NULL,
    anvil            TEXT NOT NULL,
    input_tokens     INTEGER NOT NULL DEFAULT 0,
    output_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_read       INTEGER NOT NULL DEFAULT 0,
    cache_write      INTEGER NOT NULL DEFAULT 0,
    estimated_cost   REAL NOT NULL DEFAULT 0,
    updated_at       TEXT NOT NULL,
    PRIMARY KEY (bead_id, anvil)
);

CREATE TABLE IF NOT EXISTS daily_costs (
    date             TEXT PRIMARY KEY,
    input_tokens     INTEGER NOT NULL DEFAULT 0,
    output_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_read       INTEGER NOT NULL DEFAULT 0,
    cache_write      INTEGER NOT NULL DEFAULT 0,
    estimated_cost   REAL NOT NULL DEFAULT 0,
    cost_limit       REAL NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS provider_quotas (
    provider           TEXT PRIMARY KEY,
    requests_limit     INTEGER NOT NULL DEFAULT 0,
    requests_remaining INTEGER NOT NULL DEFAULT 0,
    requests_reset     TEXT,
    tokens_limit       INTEGER NOT NULL DEFAULT 0,
    tokens_remaining   INTEGER NOT NULL DEFAULT 0,
    tokens_reset       TEXT,
    updated_at         TEXT NOT NULL
);
`

// WorkerStatus represents the lifecycle state of a Smith worker.
type WorkerStatus string

const (
	WorkerPending    WorkerStatus = "pending"
	WorkerRunning    WorkerStatus = "running"
	WorkerReviewing  WorkerStatus = "reviewing"
	WorkerMonitoring WorkerStatus = "monitoring"
	WorkerDone       WorkerStatus = "done"
	WorkerFailed     WorkerStatus = "failed"
	WorkerTimeout    WorkerStatus = "timeout"
)

// Worker represents a Smith worker entry.
type Worker struct {
	ID          string
	BeadID      string
	Anvil       string
	Branch      string
	PID         int
	Status      WorkerStatus
	Phase       string // active component: smith|temper|warden|bellows|idle
	StartedAt   time.Time
	CompletedAt *time.Time
	LogPath     string
}

// InsertWorker adds a new worker record.
func (db *DB) InsertWorker(w *Worker) error {
	_, err := db.conn.Exec(
		`INSERT INTO workers (id, bead_id, anvil, branch, pid, status, phase, started_at, log_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.BeadID, w.Anvil, w.Branch, w.PID, string(w.Status), w.Phase,
		w.StartedAt.Format(time.RFC3339), w.LogPath,
	)
	return err
}

// UpdateWorkerPhase updates the active pipeline phase for a worker.
func (db *DB) UpdateWorkerPhase(id string, phase string) error {
	_, err := db.conn.Exec(`UPDATE workers SET phase = ? WHERE id = ?`, phase, id)
	return err
}

// UpdateWorkerStatus updates a worker's status and optionally sets completed_at.
func (db *DB) UpdateWorkerStatus(id string, status WorkerStatus) error {
	if status == WorkerDone || status == WorkerFailed || status == WorkerTimeout {
		_, err := db.conn.Exec(
			`UPDATE workers SET status = ?, completed_at = ? WHERE id = ?`,
			string(status), time.Now().Format(time.RFC3339), id,
		)
		return err
	}
	_, err := db.conn.Exec(`UPDATE workers SET status = ? WHERE id = ?`, string(status), id)
	return err
}

// UpdateWorkerPID updates the PID of a running worker.
func (db *DB) UpdateWorkerPID(id string, pid int) error {
	_, err := db.conn.Exec(`UPDATE workers SET pid = ? WHERE id = ?`, pid, id)
	return err
}

// UpdateWorkerLogPath updates the log path of a worker.
func (db *DB) UpdateWorkerLogPath(id string, logPath string) error {
	_, err := db.conn.Exec(`UPDATE workers SET log_path = ? WHERE id = ?`, logPath, id)
	return err
}

// ActiveWorkers returns all workers with non-terminal status.
func (db *DB) ActiveWorkers() ([]Worker, error) {
	return db.queryWorkers(`SELECT id, bead_id, anvil, branch, pid, status, phase, started_at, completed_at, log_path
		FROM workers WHERE status IN ('pending', 'running', 'reviewing', 'monitoring')
		ORDER BY started_at`)
}

// ActiveWorkerByBead returns the non-terminal worker for a given bead ID.
func (db *DB) ActiveWorkerByBead(beadID string) (*Worker, error) {
	workers, err := db.queryWorkers(`SELECT id, bead_id, anvil, branch, pid, status, phase, started_at, completed_at, log_path
		FROM workers WHERE bead_id = ? AND status IN ('pending', 'running', 'reviewing', 'monitoring')
		LIMIT 1`, beadID)
	if err != nil {
		return nil, err
	}
	if len(workers) == 0 {
		return nil, nil
	}
	return &workers[0], nil
}

// CompleteWorkersByBead marks all non-terminal workers for a bead as Done.
func (db *DB) CompleteWorkersByBead(beadID string) error {
	_, err := db.conn.Exec(
		`UPDATE workers SET status = ?, completed_at = ?
		 WHERE bead_id = ? AND status IN ('pending', 'running', 'reviewing', 'monitoring')`,
		string(WorkerDone), time.Now().Format(time.RFC3339), beadID,
	)
	return err
}

// WorkersByAnvil returns all workers for a given anvil.
func (db *DB) WorkersByAnvil(anvil string) ([]Worker, error) {
	return db.queryWorkers(`SELECT id, bead_id, anvil, branch, pid, status, phase, started_at, completed_at, log_path
		FROM workers WHERE anvil = ?
		ORDER BY started_at DESC`, anvil)
}

// CompletedWorkers returns workers in terminal states (done, failed, timeout),
// ordered by most recently completed first. Limit 0 means no limit.
func (db *DB) CompletedWorkers(limit int) ([]Worker, error) {
	query := `SELECT id, bead_id, anvil, branch, pid, status, phase, started_at, completed_at, log_path
		FROM workers WHERE status IN ('done', 'failed', 'timeout')
		ORDER BY completed_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	return db.queryWorkers(query)
}

// AllWorkers returns all workers ordered by most recent first.
func (db *DB) AllWorkers(limit int) ([]Worker, error) {
	query := `SELECT id, bead_id, anvil, branch, pid, status, phase, started_at, completed_at, log_path
		FROM workers ORDER BY started_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	return db.queryWorkers(query)
}

func (db *DB) queryWorkers(query string, args ...any) ([]Worker, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		var w Worker
		var status string
		var startedAt string
		var completedAt sql.NullString
		if err := rows.Scan(&w.ID, &w.BeadID, &w.Anvil, &w.Branch, &w.PID,
			&status, &w.Phase, &startedAt, &completedAt, &w.LogPath); err != nil {
			return nil, err
		}
		w.Status = WorkerStatus(status)
		w.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
		if completedAt.Valid {
			t, _ := time.Parse(time.RFC3339, completedAt.String)
			w.CompletedAt = &t
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

// PRStatus represents the lifecycle of a pull request.
type PRStatus string

const (
	PROpen      PRStatus = "open"
	PRApproved  PRStatus = "approved"
	PRMerged    PRStatus = "merged"
	PRClosed    PRStatus = "closed"
	PRNeedsFix  PRStatus = "needs_fix"
)

// PR represents a pull request entry.
type PR struct {
	ID               int
	Number           int
	Anvil            string
	BeadID           string
	Branch           string
	Status           PRStatus
	CreatedAt        time.Time
	LastChecked      *time.Time
	IsConflicting    bool
	CIPassing        bool
	CIFixCount       int
	ReviewFixCount   int
}

// InsertPR adds a new PR record.
func (db *DB) InsertPR(pr *PR) error {
	ciPassing := 1
	if !pr.CIPassing {
		ciPassing = 0
	}
	res, err := db.conn.Exec(
		`INSERT INTO prs (number, anvil, bead_id, branch, status, created_at, ci_passing, ci_fix_count, review_fix_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pr.Number, pr.Anvil, pr.BeadID, pr.Branch, string(pr.Status),
		pr.CreatedAt.Format(time.RFC3339), ciPassing, pr.CIFixCount, pr.ReviewFixCount,
	)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	pr.ID = int(id)
	return nil
}

// UpdatePRStatus updates a PR's status and last_checked time.
func (db *DB) UpdatePRStatus(id int, status PRStatus) error {
	_, err := db.conn.Exec(
		`UPDATE prs SET status = ?, last_checked = ? WHERE id = ?`,
		string(status), time.Now().Format(time.RFC3339), id,
	)
	return err
}

// UpdatePRLifecycle updates the lifecycle-specific tracking fields for a PR.
func (db *DB) UpdatePRLifecycle(id int, ciPassing bool, ciFixCount, reviewFixCount int) error {
	cp := 0
	if ciPassing {
		cp = 1
	}
	_, err := db.conn.Exec(
		`UPDATE prs SET ci_passing = ?, ci_fix_count = ?, review_fix_count = ?, last_checked = ? WHERE id = ?`,
		cp, ciFixCount, reviewFixCount, time.Now().Format(time.RFC3339), id,
	)
	return err
}

// OpenPRs returns all PRs with non-terminal status.
func (db *DB) OpenPRs() ([]PR, error) {
	return db.queryPRs(`SELECT id, number, anvil, bead_id, branch, status, created_at, last_checked, is_conflicting, ci_passing, ci_fix_count, review_fix_count
		FROM prs WHERE status IN ('open', 'approved', 'needs_fix')
		ORDER BY created_at`)
}

// UpdatePRConflicting sets or clears the persisted conflict flag for a PR.
func (db *DB) UpdatePRConflicting(id int, conflicting bool) error {
	val := 0
	if conflicting {
		val = 1
	}
	_, err := db.conn.Exec(
		`UPDATE prs SET is_conflicting = ?, last_checked = ? WHERE id = ?`,
		val, time.Now().Format(time.RFC3339), id,
	)
	return err
}

func (db *DB) queryPRs(query string, args ...any) ([]PR, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prs []PR
	for rows.Next() {
		var p PR
		var status string
		var createdAt string
		var lastChecked sql.NullString
		var isConflicting, ciPassing int
		if err := rows.Scan(&p.ID, &p.Number, &p.Anvil, &p.BeadID, &p.Branch,
			&status, &createdAt, &lastChecked, &isConflicting, &ciPassing, &p.CIFixCount, &p.ReviewFixCount); err != nil {
			return nil, err
		}
		p.Status = PRStatus(status)
		p.IsConflicting = isConflicting != 0
		p.CIPassing = ciPassing != 0
		p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if lastChecked.Valid {
			t, _ := time.Parse(time.RFC3339, lastChecked.String)
			p.LastChecked = &t
		}
		prs = append(prs, p)
	}
	return prs, rows.Err()
}

// EventType categorizes events in the log.
type EventType string

const (
	EventDaemonStarted  EventType = "daemon_started"
	EventDaemonStopped  EventType = "daemon_stopped"
	EventConfigReload   EventType = "config_reload"
	EventOrphanCleanup  EventType = "orphan_cleanup"
	EventPoll           EventType = "poll"
	EventPollError      EventType = "poll_error"
	EventBeadClaimed    EventType = "bead_claimed"
	EventSmithStarted   EventType = "smith_started"
	EventSmithDone      EventType = "smith_done"
	EventSmithStats     EventType = "smith_stats"
	EventSmithFailed    EventType = "smith_failed"
	EventWardenStarted  EventType = "warden_started"
	EventWardenPass     EventType = "warden_pass"
	EventWardenReject   EventType = "warden_reject"
	EventTemperStarted  EventType = "temper_started"
	EventTemperPassed   EventType = "temper_passed"
	EventTemperFailed   EventType = "temper_failed"
	EventBellowsStarted EventType = "bellows_started"
	EventCIFailed       EventType = "ci_failed"
	EventCIFixStarted   EventType = "ci_fix_started"
	EventCIFixSuccess   EventType = "ci_fix_success"
	EventCIFixFailed    EventType = "ci_fix_failed"
	EventReviewChanges          EventType = "review_changes"
	EventReviewFixStarted        EventType = "review_fix_started"
	EventReviewFixSuccess        EventType = "review_fix_success"
	EventReviewFixFailed         EventType = "review_fix_failed"
	EventReviewThreadResolved    EventType = "review_thread_resolved"
	EventReviewFixSmithError     EventType = "review_fix_smith_error"
	EventPRCreated      EventType = "pr_created"
	EventPRMerged       EventType = "pr_merged"
	EventPRClosed       EventType = "pr_closed"
	EventPRConflicting  EventType = "pr_conflicting"
	EventPRNeedsFix     EventType = "pr_needs_fix"
	EventRebaseStarted  EventType = "rebase_started"
	EventRebaseSuccess  EventType = "rebase_success"
	EventRebaseFailed   EventType = "rebase_failed"
	EventLifecycleExhausted EventType = "lifecycle_exhausted"
	EventError          EventType = "error"
)

// Event represents a logged event.
type Event struct {
	ID        int
	Timestamp time.Time
	Type      EventType
	Message   string
	BeadID    string
	Anvil     string
}

// LogEvent records an event in the database.
func (db *DB) LogEvent(typ EventType, message, beadID, anvil string) error {
	_, err := db.conn.Exec(
		`INSERT INTO events (timestamp, type, message, bead_id, anvil)
		 VALUES (?, ?, ?, ?, ?)`,
		time.Now().Format(time.RFC3339), string(typ), message, beadID, anvil,
	)
	return err
}

// RecentEvents returns the most recent n events.
func (db *DB) RecentEvents(n int) ([]Event, error) {
	rows, err := db.conn.Query(
		`SELECT id, timestamp, type, message, bead_id, anvil
		 FROM events ORDER BY timestamp DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var typ, ts string
		if err := rows.Scan(&e.ID, &ts, &typ, &e.Message, &e.BeadID, &e.Anvil); err != nil {
			return nil, err
		}
		e.Type = EventType(typ)
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
		events = append(events, e)
	}
	return events, rows.Err()
}

// --- Retry tracking ---

// RetryRecord tracks retry state for a bead.
type RetryRecord struct {
	BeadID     string
	Anvil      string
	RetryCount int
	NextRetry  *time.Time
	NeedsHuman bool
	LastError  string
	UpdatedAt  time.Time
}

// GetRetry returns the retry record for a bead, or nil if none exists.
func (db *DB) GetRetry(beadID, anvil string) (*RetryRecord, error) {
	row := db.conn.QueryRow(
		`SELECT bead_id, anvil, retry_count, next_retry, needs_human, last_error, updated_at
		 FROM retries WHERE bead_id = ? AND anvil = ?`, beadID, anvil)

	var r RetryRecord
	var nextRetry sql.NullString
	var updatedAt string
	var needsHuman int
	err := row.Scan(&r.BeadID, &r.Anvil, &r.RetryCount, &nextRetry, &needsHuman, &r.LastError, &updatedAt)
	if err != nil {
		return nil, err
	}
	r.NeedsHuman = needsHuman != 0
	r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if nextRetry.Valid {
		t, _ := time.Parse(time.RFC3339, nextRetry.String)
		r.NextRetry = &t
	}
	return &r, nil
}

// UpsertRetry creates or updates a retry record.
func (db *DB) UpsertRetry(r *RetryRecord) error {
	var nextRetry *string
	if r.NextRetry != nil {
		s := r.NextRetry.Format(time.RFC3339)
		nextRetry = &s
	}
	needsHuman := 0
	if r.NeedsHuman {
		needsHuman = 1
	}
	_, err := db.conn.Exec(
		`INSERT INTO retries (bead_id, anvil, retry_count, next_retry, needs_human, last_error, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(bead_id, anvil) DO UPDATE SET
			retry_count = excluded.retry_count,
			next_retry = excluded.next_retry,
			needs_human = excluded.needs_human,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		r.BeadID, r.Anvil, r.RetryCount, nextRetry, needsHuman,
		r.LastError, time.Now().Format(time.RFC3339),
	)
	return err
}

// PendingRetries returns retries that are ready to be attempted (next_retry <= now).
func (db *DB) PendingRetries() ([]RetryRecord, error) {
	now := time.Now().Format(time.RFC3339)
	rows, err := db.conn.Query(
		`SELECT bead_id, anvil, retry_count, next_retry, needs_human, last_error, updated_at
		 FROM retries WHERE needs_human = 0 AND (next_retry IS NULL OR next_retry <= ?)
		 ORDER BY next_retry`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []RetryRecord
	for rows.Next() {
		var r RetryRecord
		var nextRetry sql.NullString
		var updatedAt string
		var needsHuman int
		if err := rows.Scan(&r.BeadID, &r.Anvil, &r.RetryCount, &nextRetry, &needsHuman, &r.LastError, &updatedAt); err != nil {
			return nil, err
		}
		r.NeedsHuman = needsHuman != 0
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		if nextRetry.Valid {
			t, _ := time.Parse(time.RFC3339, nextRetry.String)
			r.NextRetry = &t
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// NeedsHumanBeads returns all beads that have exhausted retries.
func (db *DB) NeedsHumanBeads() ([]RetryRecord, error) {
	rows, err := db.conn.Query(
		`SELECT bead_id, anvil, retry_count, next_retry, needs_human, last_error, updated_at
		 FROM retries WHERE needs_human = 1 ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []RetryRecord
	for rows.Next() {
		var r RetryRecord
		var nextRetry sql.NullString
		var updatedAt string
		var needsHuman int
		if err := rows.Scan(&r.BeadID, &r.Anvil, &r.RetryCount, &nextRetry, &needsHuman, &r.LastError, &updatedAt); err != nil {
			return nil, err
		}
		r.NeedsHuman = needsHuman != 0
		r.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		if nextRetry.Valid {
			t, _ := time.Parse(time.RFC3339, nextRetry.String)
			r.NextRetry = &t
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// ClearRetry removes the retry record for a bead (typically after success).
func (db *DB) ClearRetry(beadID, anvil string) error {
	_, err := db.conn.Exec(`DELETE FROM retries WHERE bead_id = ? AND anvil = ?`, beadID, anvil)
	return err
}

// --- Cost tracking ---

// AddBeadCost adds token usage to a bead's cumulative cost.
func (db *DB) AddBeadCost(beadID, anvil string, input, output, cacheRead, cacheWrite int, cost float64) error {
	_, err := db.conn.Exec(
		`INSERT INTO bead_costs (bead_id, anvil, input_tokens, output_tokens, cache_read, cache_write, estimated_cost, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(bead_id, anvil) DO UPDATE SET
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			cache_read = cache_read + excluded.cache_read,
			cache_write = cache_write + excluded.cache_write,
			estimated_cost = estimated_cost + excluded.estimated_cost,
			updated_at = excluded.updated_at`,
		beadID, anvil, input, output, cacheRead, cacheWrite, cost,
		time.Now().Format(time.RFC3339),
	)
	return err
}

// AddDailyCost adds token usage to today's aggregate.
func (db *DB) AddDailyCost(date string, input, output, cacheRead, cacheWrite int, cost float64) error {
	_, err := db.conn.Exec(
		`INSERT INTO daily_costs (date, input_tokens, output_tokens, cache_read, cache_write, estimated_cost)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(date) DO UPDATE SET
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			cache_read = cache_read + excluded.cache_read,
			cache_write = cache_write + excluded.cache_write,
			estimated_cost = estimated_cost + excluded.estimated_cost`,
		date, input, output, cacheRead, cacheWrite, cost,
	)
	return err
}

// GetDailyCost returns cost data for a specific date.
func (db *DB) GetDailyCost(date string) (inputTokens, outputTokens, cacheRead, cacheWrite int, cost, limit float64, err error) {
	err = db.conn.QueryRow(
		`SELECT input_tokens, output_tokens, cache_read, cache_write, estimated_cost, cost_limit
		 FROM daily_costs WHERE date = ?`, date).
		Scan(&inputTokens, &outputTokens, &cacheRead, &cacheWrite, &cost, &limit)
	return
}

// SetDailyCostLimit sets the cost limit for a specific date.
func (db *DB) SetDailyCostLimit(date string, limit float64) error {
	_, err := db.conn.Exec(
		`INSERT INTO daily_costs (date, cost_limit) VALUES (?, ?)
		 ON CONFLICT(date) DO UPDATE SET cost_limit = excluded.cost_limit`,
		date, limit,
	)
	return err
}

// TotalCostSince returns aggregate cost since a given date.
func (db *DB) TotalCostSince(sinceDate string) (float64, error) {
	var total sql.NullFloat64
	err := db.conn.QueryRow(
		`SELECT SUM(estimated_cost) FROM daily_costs WHERE date >= ?`, sinceDate).
		Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Float64, nil
}

// RecentDailyCosts returns daily cost records, most recent first.
func (db *DB) RecentDailyCosts(n int) ([]struct {
	Date          string
	InputTokens   int
	OutputTokens  int
	EstimatedCost float64
}, error) {
	rows, err := db.conn.Query(
		`SELECT date, input_tokens, output_tokens, estimated_cost
		 FROM daily_costs ORDER BY date DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var costs []struct {
		Date          string
		InputTokens   int
		OutputTokens  int
		EstimatedCost float64
	}
	for rows.Next() {
		var c struct {
			Date          string
			InputTokens   int
			OutputTokens  int
			EstimatedCost float64
		}
		if err := rows.Scan(&c.Date, &c.InputTokens, &c.OutputTokens, &c.EstimatedCost); err != nil {
			return nil, err
		}
		costs = append(costs, c)
	}
	return costs, rows.Err()
}

// --- Provider Quota tracking ---

// UpsertProviderQuota creates or updates a provider's quota record.
func (db *DB) UpsertProviderQuota(pv string, q *provider.Quota) error {
	var reqReset, tokReset *string
	if q.RequestsReset != nil {
		s := q.RequestsReset.Format(time.RFC3339)
		reqReset = &s
	}
	if q.TokensReset != nil {
		s := q.TokensReset.Format(time.RFC3339)
		tokReset = &s
	}

	_, err := db.conn.Exec(
		`INSERT INTO provider_quotas (
			provider, requests_limit, requests_remaining, requests_reset,
			tokens_limit, tokens_remaining, tokens_reset, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider) DO UPDATE SET
			requests_limit = excluded.requests_limit,
			requests_remaining = excluded.requests_remaining,
			requests_reset = excluded.requests_reset,
			tokens_limit = excluded.tokens_limit,
			tokens_remaining = excluded.tokens_remaining,
			tokens_reset = excluded.tokens_reset,
			updated_at = excluded.updated_at`,
		pv, q.RequestsLimit, q.RequestsRemaining, reqReset,
		q.TokensLimit, q.TokensRemaining, tokReset, time.Now().Format(time.RFC3339),
	)
	return err
}

// GetAllProviderQuotas returns all known provider quotas.
func (db *DB) GetAllProviderQuotas() (map[string]provider.Quota, error) {
	rows, err := db.conn.Query(
		`SELECT provider, requests_limit, requests_remaining, requests_reset,
		        tokens_limit, tokens_remaining, tokens_reset
		 FROM provider_quotas`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	quotas := make(map[string]provider.Quota)
	for rows.Next() {
		var pv string
		var q provider.Quota
		var reqReset, tokReset sql.NullString
		if err := rows.Scan(&pv, &q.RequestsLimit, &q.RequestsRemaining, &reqReset,
			&q.TokensLimit, &q.TokensRemaining, &tokReset); err != nil {
			return nil, err
		}
		if reqReset.Valid {
			t, _ := time.Parse(time.RFC3339, reqReset.String)
			q.RequestsReset = &t
		}
		if tokReset.Valid {
			t, _ := time.Parse(time.RFC3339, tokReset.String)
			q.TokensReset = &t
		}
		quotas[pv] = q
	}
	return quotas, rows.Err()
}

// GetProviderQuota returns the quota for a specific provider.
func (db *DB) GetProviderQuota(pv string) (*provider.Quota, error) {
	row := db.conn.QueryRow(
		`SELECT requests_limit, requests_remaining, requests_reset,
		        tokens_limit, tokens_remaining, tokens_reset
		 FROM provider_quotas WHERE provider = ?`, pv)

	var q provider.Quota
	var reqReset, tokReset sql.NullString
	err := row.Scan(&q.RequestsLimit, &q.RequestsRemaining, &reqReset,
		&q.TokensLimit, &q.TokensRemaining, &tokReset)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if reqReset.Valid {
		t, _ := time.Parse(time.RFC3339, reqReset.String)
		q.RequestsReset = &t
	}
	if tokReset.Valid {
		t, _ := time.Parse(time.RFC3339, tokReset.String)
		q.TokensReset = &t
	}
	return &q, nil
}
