package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ConsolidatedBead returns the bead depcheck last recorded as the anvil's
// consolidated dependency-update bead, or "" when the anvil has none.
//
// The mapping is kept here, rather than re-derived from the beads pool on every
// scan, because a pool is not necessarily one per anvil: Munin and Explorer
// point at the same database on purpose. Anything written into the bead — its
// title first of all — is therefore a key both anvils match, and a key a human
// can edit. The bead id in this table is neither.
func (db *DB) ConsolidatedBead(anvil string) (string, error) {
	var beadID string
	err := db.conn.QueryRow(
		`SELECT bead_id FROM depcheck_consolidated_beads WHERE anvil = ?`, anvil).Scan(&beadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading consolidated bead for anvil %q: %w", anvil, err)
	}
	return beadID, nil
}

// SetConsolidatedBead records which bead the anvil's consolidated dependency
// updates are collected in. One row per anvil: a second row would mean the
// anvil had forked, which is the condition this table exists to prevent.
func (db *DB) SetConsolidatedBead(anvil, beadID string) error {
	if anvil == "" {
		return errors.New("consolidated bead without an anvil")
	}
	if beadID == "" {
		return fmt.Errorf("consolidated bead for anvil %q without a bead id", anvil)
	}
	_, err := db.conn.Exec(
		`INSERT INTO depcheck_consolidated_beads (anvil, bead_id, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(anvil) DO UPDATE SET
		    bead_id    = excluded.bead_id,
		    updated_at = excluded.updated_at`,
		anvil, beadID, time.Now().Format(dbTimeLayout))
	return err
}

// ClearConsolidatedBead drops the anvil's mapping so the next scan adopts or
// creates a bead instead of following a pin that no longer resolves. Called
// when the recorded bead has been closed or deleted; clearing an anvil that has
// no mapping is a no-op.
func (db *DB) ClearConsolidatedBead(anvil string) error {
	_, err := db.conn.Exec(`DELETE FROM depcheck_consolidated_beads WHERE anvil = ?`, anvil)
	return err
}
