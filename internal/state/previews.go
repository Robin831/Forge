package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Preview lifecycle statuses. A preview starts as PreviewStarting and settles
// into one of the terminal-ish states below once every service has been health
// checked. The row survives daemon restarts so the Kiln manager can reconcile
// orphaned processes and worktrees on startup.
const (
	// PreviewStarting means services are being spawned or health checked.
	PreviewStarting = "starting"
	// PreviewRunning means every service reached PreviewServiceHealthy.
	PreviewRunning = "running"
	// PreviewDegraded means at least one service is healthy and at least one
	// failed. The healthy ones keep running — a failing service never takes
	// its siblings down with it.
	PreviewDegraded = "degraded"
	// PreviewFailed means no service became healthy.
	PreviewFailed = "failed"
	// PreviewStopped means the preview has been torn down.
	PreviewStopped = "stopped"
)

// Per-service health states, mirroring the manifest's starting → healthy |
// failed machine, plus the one transition that happens after it: a service that
// passed its readiness check and later died.
const (
	PreviewServiceStarting = "starting"
	PreviewServiceHealthy  = "healthy"
	PreviewServiceFailed   = "failed"
	// PreviewServiceExited means the service became healthy and then its process
	// exited. It is deliberately distinct from PreviewServiceFailed: "never came
	// up" and "came up and then died" are different problems, and a service that
	// served for seven minutes before dying is the case where the log holds the
	// answer. Both count as not-serving everywhere the distinction does not
	// matter (the preview status fold, the entry link).
	PreviewServiceExited = "exited"
)

// PreviewServiceServing reports whether a service in this state is expected to
// answer requests. Starting counts: ports are allocated up front and a preview
// link is offered while it comes up, which is the behaviour a readiness check
// exists to resolve. Failed and exited do not.
func PreviewServiceServing(health string) bool {
	switch health {
	case PreviewServiceFailed, PreviewServiceExited:
		return false
	default:
		return true
	}
}

// PreviewService is one supervised process inside a preview, as persisted in
// the previews.services JSON column.
type PreviewService struct {
	// Name is the manifest's service name.
	Name string `json:"name"`
	// Port is the port allocated to this service from the preview port range.
	Port int `json:"port"`
	// Health is one of PreviewServiceStarting/Healthy/Failed/Exited.
	Health string `json:"health"`
	// PID is the process group leader's PID; 0 when the service never started
	// or has already exited.
	PID int `json:"pid"`
	// LogPath is the file this service's stdout/stderr is appended to, under
	// ~/.forge/logs/<beadID>/.
	LogPath string `json:"log_path,omitempty"`
	// Entry marks the service whose URL is *the* preview link.
	Entry bool `json:"entry,omitempty"`
	// Error explains a failed service (health timeout, spawn error, early exit).
	Error string `json:"error,omitempty"`
	// StartedAt is when this service's process was spawned. Zero when it never
	// started.
	StartedAt time.Time `json:"started_at,omitzero"`
	// ExitedAt is when its process was reaped. Zero while it is still running,
	// which is what makes it the end of the uptime interval: a service that died
	// reports the lifetime it had, not a clock that keeps counting.
	ExitedAt time.Time `json:"exited_at,omitzero"`
	// ExitCode is the process's exit status once it has exited. nil while it
	// runs, and also for a process killed by a signal — which has no exit code,
	// only a cause, and that is what Error carries.
	ExitCode *int `json:"exit_code,omitempty"`
}

// Lifetime is how long this service's process ran: from its spawn to its exit,
// or to now while it is still running. Zero when the service never started.
//
// It is the one place uptime is derived, so a dead service's number freezes at
// its death on every surface rather than in whichever renderer remembered to
// stop counting.
func (s PreviewService) Lifetime(now time.Time) time.Duration {
	if s.StartedAt.IsZero() {
		return 0
	}
	end := now
	if !s.ExitedAt.IsZero() {
		end = s.ExitedAt
	}
	d := end.Sub(s.StartedAt)
	if d < 0 {
		return 0
	}
	return d
}

// Preview is the persisted record of one preview environment.
type Preview struct {
	BeadID       string
	Anvil        string
	Branch       string
	Status       string
	WorktreePath string
	Services     []PreviewService
	CreatedAt    time.Time
	LastActiveAt time.Time
}

// Service returns the named service of this preview.
func (p Preview) Service(name string) (PreviewService, bool) {
	for _, svc := range p.Services {
		if svc.Name == name {
			return svc, true
		}
	}
	return PreviewService{}, false
}

// UpsertPreview inserts or refreshes the record for a preview.
//
// created_at is only written on insert: an update carries the running state
// (status, services, last_active_at) and must not rewrite when the preview was
// born. Zero timestamps default to now so callers can persist a fresh record
// without filling them in.
func (db *DB) UpsertPreview(p Preview) error {
	services, err := json.Marshal(nonNilServices(p.Services))
	if err != nil {
		return fmt.Errorf("encoding preview services for %s: %w", p.BeadID, err)
	}
	now := time.Now()
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	lastActive := p.LastActiveAt
	if lastActive.IsZero() {
		lastActive = now
	}
	status := p.Status
	if status == "" {
		status = PreviewStarting
	}

	_, err = db.conn.Exec(
		`INSERT INTO previews
		    (bead_id, anvil, branch, status, worktree_path, services, created_at, last_active_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(bead_id) DO UPDATE SET
		    anvil = excluded.anvil,
		    branch = excluded.branch,
		    status = excluded.status,
		    worktree_path = excluded.worktree_path,
		    services = excluded.services,
		    last_active_at = excluded.last_active_at`,
		p.BeadID, p.Anvil, p.Branch, status, p.WorktreePath, string(services),
		createdAt.Format(dbTimeLayout), lastActive.Format(dbTimeLayout),
	)
	if err != nil {
		return fmt.Errorf("writing preview %s: %w", p.BeadID, err)
	}
	return nil
}

// GetPreview returns the preview record for a bead. A missing preview is not an
// error: it returns (nil, nil), so callers can treat "no preview" as normal.
func (db *DB) GetPreview(beadID string) (*Preview, error) {
	row := db.conn.QueryRow(
		`SELECT bead_id, anvil, branch, status, worktree_path, services, created_at, last_active_at
		   FROM previews WHERE bead_id = ?`, beadID)
	p, err := scanPreview(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading preview %s: %w", beadID, err)
	}
	return &p, nil
}

// ListPreviews returns every preview record, most recently created first.
func (db *DB) ListPreviews() ([]Preview, error) {
	rows, err := db.conn.Query(
		`SELECT bead_id, anvil, branch, status, worktree_path, services, created_at, last_active_at
		   FROM previews ORDER BY created_at DESC, bead_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("listing previews: %w", err)
	}
	defer rows.Close()

	var out []Preview
	for rows.Next() {
		p, err := scanPreview(rows)
		if err != nil {
			return nil, fmt.Errorf("listing previews: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPreviewStatus updates only the overall status of a preview (and marks it
// active). The returned bool reports whether a row existed.
func (db *DB) SetPreviewStatus(beadID, status string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE previews SET status = ?, last_active_at = ? WHERE bead_id = ?`,
		status, time.Now().Format(dbTimeLayout), beadID)
	if err != nil {
		return false, fmt.Errorf("updating preview %s status: %w", beadID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// TouchPreview refreshes a preview's last_active_at, which is what the idle
// reaper measures against. The returned bool reports whether a row existed.
func (db *DB) TouchPreview(beadID string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE previews SET last_active_at = ? WHERE bead_id = ?`,
		time.Now().Format(dbTimeLayout), beadID)
	if err != nil {
		return false, fmt.Errorf("touching preview %s: %w", beadID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DeletePreview removes a preview record. Deleting an unknown preview is a
// no-op, so teardown paths can call it unconditionally.
func (db *DB) DeletePreview(beadID string) error {
	if _, err := db.conn.Exec(`DELETE FROM previews WHERE bead_id = ?`, beadID); err != nil {
		return fmt.Errorf("deleting preview %s: %w", beadID, err)
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanPreview(row rowScanner) (Preview, error) {
	var (
		p                     Preview
		services              string
		createdAt, lastActive string
	)
	if err := row.Scan(&p.BeadID, &p.Anvil, &p.Branch, &p.Status, &p.WorktreePath,
		&services, &createdAt, &lastActive); err != nil {
		return Preview{}, err
	}
	if services != "" {
		if err := json.Unmarshal([]byte(services), &p.Services); err != nil {
			return Preview{}, fmt.Errorf("decoding services of preview %s: %w", p.BeadID, err)
		}
	}
	p.CreatedAt = parseTime(createdAt)
	p.LastActiveAt = parseTime(lastActive)
	return p, nil
}

// nonNilServices keeps the stored JSON as `[]` rather than `null` for a preview
// with no services, so anything reading the column (including a future SQL
// query or the web API) sees a list either way.
func nonNilServices(services []PreviewService) []PreviewService {
	if services == nil {
		return []PreviewService{}
	}
	return services
}
