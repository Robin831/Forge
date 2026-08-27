package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DepcheckFailure is one anvil whose dependency scan is BLOCKED: a git failure
// that will produce the identical failure on every run until an operator
// changes something about the checkout (a detached HEAD with no upstream, a
// remote that does not resolve, a ref the fetch cannot lock).
//
// It is persisted for a different reason than deploy_failures is. A blocked
// scan is not silent — it writes a depcheck_failed event every run — and that
// is exactly the problem: an identical line every night is indistinguishable
// from noise, so the condition that caused it goes unread for weeks. The row is
// what lets depcheck escalate the condition ONCE, on the run its signature
// first appears, and stay quiet on every run after it while the state is
// unchanged.
//
// One row per anvil, keyed on Signature. A blocked anvil has exactly one
// blocking condition at a time (the scan stops at the first one), so a second
// row could only ever describe a superseded state; when the condition CHANGES,
// the new signature is a fresh escalation rather than a second row, because the
// operator's next action changed with it.
//
// The row is cleared by depcheck itself once the anvil's manifests can be read
// again, so a recurrence after a fix escalates as a new condition — and so a
// flag no path clears can never linger in Needs Attention.
type DepcheckFailure struct {
	// Anvil is the anvil whose scan is blocked.
	Anvil string
	// Kind names the failure class ("blocked"); it is stored so the row is
	// self-describing and a future class can share the table.
	Kind string
	// Signature is the stable digest of the normalised failure. Two runs
	// hitting the same blocking condition produce the same value; that equality
	// is the whole suppression rule.
	Signature string
	// Title is the needs-attention headline.
	Title string
	// Detail is the operator-facing description: what is blocking, and what
	// resolves it.
	Detail string
	// Occurrences counts the runs that hit this signature, including the one
	// that escalated it. It is the number that says how long a condition has
	// been ignored without re-emitting an event to say so.
	Occurrences int
	// FirstSeen is when this signature was first recorded; LastSeen is the most
	// recent run that hit it.
	FirstSeen time.Time
	LastSeen  time.Time
}

// DepcheckKindBlocked is the DepcheckFailure.Kind for a git failure that
// repeats identically until an operator intervenes.
const DepcheckKindBlocked = "blocked"

// depcheckAttentionTitle is the needs-attention title for a blocked scan, kept
// as a function so an empty stored title still reads as something.
func depcheckAttentionTitle(f DepcheckFailure) string {
	if f.Title != "" {
		return f.Title
	}
	return fmt.Sprintf("Anvil %s: dependency scan blocked", f.Anvil)
}

// RecordDepcheckFailure records the anvil's outstanding blocked scan and
// reports whether it is NEW — either the anvil had no outstanding failure, or
// it had one with a different signature.
//
// That boolean is the escalation gate: true means the operator has not been
// told about THIS condition yet, false means the run hit a condition already on
// the attention panel and must stay silent. An unchanged signature still
// refreshes the row (last_seen, occurrences, and the rendered detail, which may
// name paths that have moved), because the entry an operator reads should
// describe the latest observation, not the first one.
func (db *DB) RecordDepcheckFailure(f DepcheckFailure) (bool, error) {
	if f.Anvil == "" {
		return false, errors.New("depcheck failure without an anvil")
	}
	if f.Signature == "" {
		return false, fmt.Errorf("depcheck failure for anvil %q without a signature", f.Anvil)
	}
	now := time.Now()
	lastSeen := f.LastSeen
	if lastSeen.IsZero() {
		lastSeen = now
	}

	var prevSignature, prevFirstSeen string
	var prevOccurrences int
	err := db.conn.QueryRow(
		`SELECT signature, first_seen, occurrences FROM depcheck_failures WHERE anvil = ?`, f.Anvil,
	).Scan(&prevSignature, &prevFirstSeen, &prevOccurrences)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No outstanding failure: this is a fresh escalation.
	case err != nil:
		return false, err
	}

	fresh := prevSignature != f.Signature
	firstSeen := lastSeen
	occurrences := 1
	if !fresh {
		if parsed := parseTime(prevFirstSeen); !parsed.IsZero() {
			firstSeen = parsed
		}
		occurrences = prevOccurrences + 1
	}

	kind := f.Kind
	if kind == "" {
		kind = DepcheckKindBlocked
	}

	_, err = db.conn.Exec(
		`INSERT INTO depcheck_failures
		    (anvil, kind, signature, title, detail, occurrences, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(anvil) DO UPDATE SET
		    kind = excluded.kind,
		    signature = excluded.signature,
		    title = excluded.title,
		    detail = excluded.detail,
		    occurrences = excluded.occurrences,
		    first_seen = excluded.first_seen,
		    last_seen = excluded.last_seen`,
		f.Anvil, kind, f.Signature, f.Title, f.Detail, occurrences,
		firstSeen.Format(dbTimeLayout), lastSeen.Format(dbTimeLayout),
	)
	if err != nil {
		return false, err
	}
	return fresh, nil
}

// ClearDepcheckFailure removes an anvil's outstanding blocked scan, reporting
// whether there was one. Clearing an anvil that is not blocked is a no-op, so
// the successful-scan path can call it unconditionally.
func (db *DB) ClearDepcheckFailure(anvil string) (bool, error) {
	res, err := db.conn.Exec(`DELETE FROM depcheck_failures WHERE anvil = ?`, anvil)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DepcheckFailures returns every anvil whose dependency scan is blocked, oldest
// condition first — the one that has been blocking longest is the one that has
// been ignored longest.
func (db *DB) DepcheckFailures() ([]DepcheckFailure, error) {
	rows, err := db.conn.Query(
		`SELECT anvil, kind, signature, title, detail, occurrences, first_seen, last_seen
		   FROM depcheck_failures
		  ORDER BY first_seen ASC, anvil ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DepcheckFailure
	for rows.Next() {
		var (
			f                   DepcheckFailure
			firstSeen, lastSeen string
		)
		if err := rows.Scan(&f.Anvil, &f.Kind, &f.Signature, &f.Title, &f.Detail,
			&f.Occurrences, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		f.FirstSeen = parseTime(firstSeen)
		f.LastSeen = parseTime(lastSeen)
		out = append(out, f)
	}
	return out, rows.Err()
}

// PruneDepcheckFailures deletes rows for anvils that are no longer registered.
// Nothing else clears them — the clear happens on a successful scan of that
// anvil, which a deregistered anvil never gets — so without this an anvil
// removed from the config keeps a needs-attention entry that no action can
// resolve. Passing an empty list is a no-op: a config with no anvils must not
// wipe the table.
func (db *DB) PruneDepcheckFailures(keep []string) error {
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
		`DELETE FROM depcheck_failures WHERE anvil NOT IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}
