package state

import (
	"fmt"
	"time"
)

// PendingBeadClose records a bead whose PR has already merged but whose
// `bd close` has not yet succeeded. The row exists purely so the close is
// retried on later Bellows cycles instead of being lost with a single WARN
// line — a merged PR whose bead stays open blocks every dependent bead behind
// it (observed 2026-08-06: Forge-ir70 stayed IN_PROGRESS after PR #773 merged
// and had to be closed by hand, stalling Forge-66sn).
type PendingBeadClose struct {
	BeadID   string
	Anvil    string
	PRNumber int
	// Reason is the `bd close --reason` text to reuse on every re-attempt so
	// the eventually-closed bead records why it closed.
	Reason string
	// Attempts is the cumulative number of `bd close` attempts across all
	// cycles, not just the most recent burst.
	Attempts  int
	LastError string
	// MergedAt is when the close was first found to be stuck. It is written on
	// insert only and preserved across re-attempts, so an operator can see how
	// long the bead has been blocking its dependents. Leave it zero on write
	// to have the store stamp it.
	MergedAt  time.Time
	UpdatedAt time.Time
}

// UpsertPendingBeadClose records (or refreshes) a pending close. Attempts is
// accumulated rather than overwritten so the count reflects total work done
// across cycles; MergedAt is only written on insert so the original merge time
// survives re-attempts.
func (db *DB) UpsertPendingBeadClose(p PendingBeadClose) error {
	if p.BeadID == "" || p.Anvil == "" {
		return fmt.Errorf("pending bead close requires bead_id and anvil")
	}
	now := time.Now().Format(dbTimeLayout)
	mergedAt := now
	if !p.MergedAt.IsZero() {
		mergedAt = p.MergedAt.Format(dbTimeLayout)
	}
	_, err := db.conn.Exec(
		`INSERT INTO pending_bead_closes (bead_id, anvil, pr_number, reason, attempts, last_error, merged_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(bead_id, anvil) DO UPDATE SET
			pr_number  = excluded.pr_number,
			reason     = excluded.reason,
			attempts   = pending_bead_closes.attempts + excluded.attempts,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		p.BeadID, p.Anvil, p.PRNumber, p.Reason, p.Attempts, p.LastError, mergedAt, now,
	)
	return err
}

// PendingBeadCloses returns every outstanding close, oldest merge first so the
// longest-stuck bead is retried before newer ones.
func (db *DB) PendingBeadCloses() ([]PendingBeadClose, error) {
	rows, err := db.conn.Query(
		`SELECT bead_id, anvil, pr_number, reason, attempts, last_error, merged_at, updated_at
		 FROM pending_bead_closes ORDER BY merged_at, bead_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingBeadClose
	for rows.Next() {
		var p PendingBeadClose
		var mergedAt, updatedAt string
		if err := rows.Scan(&p.BeadID, &p.Anvil, &p.PRNumber, &p.Reason, &p.Attempts, &p.LastError, &mergedAt, &updatedAt); err != nil {
			return nil, err
		}
		p.MergedAt = parseTime(mergedAt)
		p.UpdatedAt = parseTime(updatedAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingBeadClose drops the row once the bead is confirmed closed.
func (db *DB) DeletePendingBeadClose(beadID, anvil string) error {
	_, err := db.conn.Exec(
		`DELETE FROM pending_bead_closes WHERE bead_id = ? AND anvil = ?`, beadID, anvil)
	return err
}
