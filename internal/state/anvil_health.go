package state

import (
	"database/sql"
	"strings"
	"time"
)

// AnvilHealth records the last known health of one anvil's beads database.
//
// Today the only tracked condition is "wedged": the beads (Dolt) working set is
// mid-merge with unresolved conflicts, so every bd write against the anvil is
// rolled back. The row is written by the daemon's full poll and cleared
// automatically once the conflicts are resolved, so no operator dismissal is
// needed (and a stale flag can never linger).
type AnvilHealth struct {
	Anvil string
	// Wedged is true while dolt_conflicts reports unresolved conflicts.
	Wedged bool
	// ConflictTables is the human-readable table list with per-table counts,
	// e.g. "issues (3), labels (1)".
	ConflictTables string
	ConflictCount  int
	// Branch is the anvil's active beads branch (e.g. "beads-sync").
	Branch string
	// Ahead/Behind are the local-vs-upstream commit counts. Only meaningful
	// when DivergenceKnown is true.
	Ahead           int
	Behind          int
	DivergenceKnown bool
	// Detail is the operator-facing description surfaced in needs-attention.
	Detail string
	// DetectedAt is when the wedge was first observed; it survives subsequent
	// polls so the daemon can report how long the anvil has been unusable.
	DetectedAt time.Time
	UpdatedAt  time.Time
}

// WedgedFor returns how long the anvil has been wedged, or 0 when unknown.
func (a AnvilHealth) WedgedFor() time.Duration {
	if a.DetectedAt.IsZero() {
		return 0
	}
	return time.Since(a.DetectedAt)
}

// MarkAnvilWedged records (or refreshes) the wedged state for an anvil.
//
// The returned bool is true when this is a fresh detection — the anvil was not
// already flagged — so callers can log/notify once per wedge rather than on
// every poll. The returned time is when the wedge was first observed; it is
// preserved across refreshes so callers can report how long the anvil has been
// unusable.
func (db *DB) MarkAnvilWedged(h AnvilHealth) (bool, time.Time, error) {
	now := time.Now()
	detectedAt := now
	first := true

	var prevDetected string
	err := db.conn.QueryRow(
		`SELECT detected_at FROM anvil_health WHERE anvil = ? AND wedged = 1`, h.Anvil,
	).Scan(&prevDetected)
	switch {
	case err == nil:
		first = false
		if t := parseTime(prevDetected); !t.IsZero() {
			detectedAt = t
		}
	case err == sql.ErrNoRows:
		// Fresh detection (or a previously healthy row) — keep first=true.
	default:
		return false, time.Time{}, err
	}

	_, err = db.conn.Exec(
		`INSERT INTO anvil_health
		    (anvil, wedged, conflict_tables, conflict_count, branch, ahead, behind,
		     divergence_known, detail, detected_at, updated_at)
		 VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(anvil) DO UPDATE SET
		    wedged = 1,
		    conflict_tables = excluded.conflict_tables,
		    conflict_count = excluded.conflict_count,
		    branch = excluded.branch,
		    ahead = excluded.ahead,
		    behind = excluded.behind,
		    divergence_known = excluded.divergence_known,
		    detail = excluded.detail,
		    detected_at = excluded.detected_at,
		    updated_at = excluded.updated_at`,
		h.Anvil, h.ConflictTables, h.ConflictCount, h.Branch, h.Ahead, h.Behind,
		boolToInt(h.DivergenceKnown), h.Detail,
		detectedAt.Format(dbTimeLayout), now.Format(dbTimeLayout),
	)
	if err != nil {
		return false, time.Time{}, err
	}
	return first, detectedAt, nil
}

// ClearAnvilWedged clears the wedged flag for an anvil. The returned bool is
// true when the anvil was actually flagged, so callers can log the recovery
// exactly once. Clearing an unknown or already-healthy anvil is a no-op.
func (db *DB) ClearAnvilWedged(anvil string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE anvil_health
		    SET wedged = 0, conflict_tables = '', conflict_count = 0, detail = '',
		        detected_at = '', updated_at = ?
		  WHERE anvil = ? AND wedged = 1`,
		time.Now().Format(dbTimeLayout), anvil,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// WedgedAnvils returns every anvil currently flagged as wedged, oldest wedge
// first (the longest-broken anvil is the most urgent).
func (db *DB) WedgedAnvils() ([]AnvilHealth, error) {
	rows, err := db.conn.Query(
		`SELECT anvil, conflict_tables, conflict_count, branch, ahead, behind,
		        divergence_known, detail, detected_at, updated_at
		   FROM anvil_health
		  WHERE wedged = 1
		  ORDER BY detected_at ASC, anvil ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AnvilHealth
	for rows.Next() {
		var h AnvilHealth
		var divergenceKnown int
		var detectedAt, updatedAt string
		if err := rows.Scan(&h.Anvil, &h.ConflictTables, &h.ConflictCount, &h.Branch,
			&h.Ahead, &h.Behind, &divergenceKnown, &h.Detail, &detectedAt, &updatedAt); err != nil {
			return nil, err
		}
		h.Wedged = true
		h.DivergenceKnown = divergenceKnown != 0
		h.DetectedAt = parseTime(detectedAt)
		h.UpdatedAt = parseTime(updatedAt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// IsAnvilWedged reports whether the given anvil is currently flagged as wedged.
func (db *DB) IsAnvilWedged(anvil string) (bool, error) {
	var wedged int
	err := db.conn.QueryRow(`SELECT wedged FROM anvil_health WHERE anvil = ?`, anvil).Scan(&wedged)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return wedged != 0, nil
}

// PruneAnvilHealth deletes rows for anvils that are no longer registered, so a
// removed anvil cannot keep a stale needs-attention entry alive. Passing an
// empty list is a no-op (a config with no anvils must not wipe the table).
func (db *DB) PruneAnvilHealth(keep []string) error {
	if len(keep) == 0 {
		return nil
	}
	placeholders := make([]string, len(keep))
	args := make([]any, len(keep))
	for i, name := range keep {
		placeholders[i] = "?"
		args[i] = name
	}
	_, err := db.conn.Exec(
		`DELETE FROM anvil_health WHERE anvil NOT IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}
