package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AnvilBead returns the bead an anvil's scanner recorded for one recurring
// condition — kind names the scanner, key names the condition within it (a
// quest name, a vulnerability id) — or "" when the anvil has none.
//
// The mapping lives here rather than being re-derived from the beads pool on
// every scan because a pool is not necessarily one per anvil: Munin and Explorer
// point at the same database on purpose. Every field of the bead itself — its
// title, the anvil name in its description — is therefore a key both anvils can
// match, and a key a human can edit. The bead id in this table is neither.
//
// See DB.ConsolidatedBead, which is the same argument for depcheck's one bead
// per anvil; it predates this table and keeps its own.
func (db *DB) AnvilBead(anvil, kind, key string) (string, error) {
	var beadID string
	err := db.conn.QueryRow(
		`SELECT bead_id FROM anvil_beads WHERE anvil = ? AND kind = ? AND lookup_key = ?`,
		anvil, kind, key).Scan(&beadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s bead %q for anvil %q: %w", kind, key, anvil, err)
	}
	return beadID, nil
}

// SetAnvilBead records which bead holds one anvil's report of a recurring
// condition. One row per (anvil, kind, key): a second row would mean the
// condition had forked into two beads, which is what the table prevents.
func (db *DB) SetAnvilBead(anvil, kind, key, beadID string) error {
	if anvil == "" || kind == "" || key == "" {
		return fmt.Errorf("anvil bead needs anvil, kind and key (got %q/%q/%q)", anvil, kind, key)
	}
	if beadID == "" {
		return fmt.Errorf("anvil bead %s/%s/%s without a bead id", anvil, kind, key)
	}
	_, err := db.conn.Exec(
		`INSERT INTO anvil_beads (anvil, kind, lookup_key, bead_id, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(anvil, kind, lookup_key) DO UPDATE SET
		    bead_id    = excluded.bead_id,
		    updated_at = excluded.updated_at`,
		anvil, kind, key, beadID, time.Now().Format(dbTimeLayout))
	return err
}

// ClearAnvilBead drops the mapping so the next scan adopts or creates a bead
// instead of following a pin that no longer resolves. Called when the recorded
// bead has been closed or deleted; clearing a mapping that does not exist is a
// no-op.
func (db *DB) ClearAnvilBead(anvil, kind, key string) error {
	_, err := db.conn.Exec(
		`DELETE FROM anvil_beads WHERE anvil = ? AND kind = ? AND lookup_key = ?`,
		anvil, kind, key)
	return err
}
