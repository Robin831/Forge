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
	"time"

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
	_, err := db.conn.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS workers (
    id          TEXT PRIMARY KEY,
    bead_id     TEXT NOT NULL,
    anvil       TEXT NOT NULL,
    branch      TEXT NOT NULL DEFAULT '',
    pid         INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'pending',
    started_at  TEXT NOT NULL,
    completed_at TEXT,
    log_path    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS prs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    number       INTEGER NOT NULL,
    anvil        TEXT NOT NULL,
    bead_id      TEXT NOT NULL DEFAULT '',
    branch       TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'open',
    created_at   TEXT NOT NULL,
    last_checked TEXT
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
`

// WorkerStatus represents the lifecycle state of a Smith worker.
type WorkerStatus string

const (
	WorkerPending   WorkerStatus = "pending"
	WorkerRunning   WorkerStatus = "running"
	WorkerReviewing WorkerStatus = "reviewing"
	WorkerDone      WorkerStatus = "done"
	WorkerFailed    WorkerStatus = "failed"
	WorkerTimeout   WorkerStatus = "timeout"
)

// Worker represents a Smith worker entry.
type Worker struct {
	ID          string
	BeadID      string
	Anvil       string
	Branch      string
	PID         int
	Status      WorkerStatus
	StartedAt   time.Time
	CompletedAt *time.Time
	LogPath     string
}

// InsertWorker adds a new worker record.
func (db *DB) InsertWorker(w *Worker) error {
	_, err := db.conn.Exec(
		`INSERT INTO workers (id, bead_id, anvil, branch, pid, status, started_at, log_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.BeadID, w.Anvil, w.Branch, w.PID, string(w.Status),
		w.StartedAt.Format(time.RFC3339), w.LogPath,
	)
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
	return db.queryWorkers(`SELECT id, bead_id, anvil, branch, pid, status, started_at, completed_at, log_path
		FROM workers WHERE status IN ('pending', 'running', 'reviewing')
		ORDER BY started_at`)
}

// WorkersByAnvil returns all workers for a given anvil.
func (db *DB) WorkersByAnvil(anvil string) ([]Worker, error) {
	return db.queryWorkers(`SELECT id, bead_id, anvil, branch, pid, status, started_at, completed_at, log_path
		FROM workers WHERE anvil = ?
		ORDER BY started_at DESC`, anvil)
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
			&status, &startedAt, &completedAt, &w.LogPath); err != nil {
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
	ID          int
	Number      int
	Anvil       string
	BeadID      string
	Branch      string
	Status      PRStatus
	CreatedAt   time.Time
	LastChecked *time.Time
}

// InsertPR adds a new PR record.
func (db *DB) InsertPR(pr *PR) error {
	res, err := db.conn.Exec(
		`INSERT INTO prs (number, anvil, bead_id, branch, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		pr.Number, pr.Anvil, pr.BeadID, pr.Branch, string(pr.Status),
		pr.CreatedAt.Format(time.RFC3339),
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

// OpenPRs returns all PRs with non-terminal status.
func (db *DB) OpenPRs() ([]PR, error) {
	return db.queryPRs(`SELECT id, number, anvil, bead_id, branch, status, created_at, last_checked
		FROM prs WHERE status IN ('open', 'approved', 'needs_fix')
		ORDER BY created_at`)
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
		if err := rows.Scan(&p.ID, &p.Number, &p.Anvil, &p.BeadID, &p.Branch,
			&status, &createdAt, &lastChecked); err != nil {
			return nil, err
		}
		p.Status = PRStatus(status)
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
	EventBeadClaimed  EventType = "bead_claimed"
	EventSmithStarted EventType = "smith_started"
	EventSmithDone    EventType = "smith_done"
	EventSmithFailed  EventType = "smith_failed"
	EventWardenPass   EventType = "warden_pass"
	EventWardenReject EventType = "warden_reject"
	EventPRCreated    EventType = "pr_created"
	EventPRMerged     EventType = "pr_merged"
	EventPRNeedsFix   EventType = "pr_needs_fix"
	EventError        EventType = "error"
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
