package state

import (
	"fmt"
	"sort"
	"time"
)

// Checker names recorded in anvil_check_success. They are stored values, so
// they are constants rather than strings written at each call site: a typo at
// one end produces a row nothing ever reads, which looks exactly like a checker
// that is running fine.
const (
	CheckerDepcheck    = "depcheck"
	CheckerVulncheck   = "vulncheck"
	CheckerQuestgiver  = "questgiver"
	CheckerPRReconcile = "pr_reconcile"
)

// CheckerLabel renders a checker name for an operator. An unknown name is
// returned as-is rather than hidden, since a row nothing recognises is itself
// worth seeing.
func CheckerLabel(checker string) string {
	switch checker {
	case CheckerDepcheck:
		return "dependency scan"
	case CheckerVulncheck:
		return "vulnerability scan"
	case CheckerQuestgiver:
		return "E2E quest run"
	case CheckerPRReconcile:
		return "PR reconcile"
	default:
		return checker
	}
}

// StaleCheck is one (anvil, checker) pair that has not completed a cycle
// recently enough. It is DERIVED at read time rather than stored, which is the
// shape of this whole feature: there is no "stale" row to raise, clear or
// re-arm, and a checker that completes once stops being stale the moment it
// writes its timestamp.
type StaleCheck struct {
	Anvil   string
	Checker string
	// Since is the last success, or — when the checker has never succeeded —
	// the first time it was seen to start a cycle.
	Since time.Time
	// EverSucceeded separates "stopped working" from "never worked". They need
	// different sentences: the second is usually a misconfiguration, and
	// reporting it as a regression sends the operator looking for a change that
	// never happened.
	EverSucceeded bool
	// Threshold is the age at which this pair was judged stale, carried so the
	// message can quote the number it was measured against rather than a
	// multiplier the reader would have to apply themselves.
	Threshold time.Duration
	// Age is how long it has actually been.
	Age time.Duration
}

// Title renders the needs-attention headline. As with DepcheckFailure the
// sentence lives on the record, so it cannot come to exist in two packages.
func (s StaleCheck) Title() string {
	if !s.EverSucceeded {
		return fmt.Sprintf("Anvil %s: %s has never completed", s.Anvil, CheckerLabel(s.Checker))
	}
	return fmt.Sprintf("Anvil %s: %s has not completed in %s",
		s.Anvil, CheckerLabel(s.Checker), roundDuration(s.Age))
}

// Detail renders the operator-facing body.
//
// It deliberately claims nothing about WHY. This check reads no error
// classification — that is the point of it, since it exists for the case where
// the classification was wrong — so it can say a checker has stopped completing
// and nothing more. A guess at the cause here would be the one kind of wrong
// that sends an operator somewhere else entirely.
func (s StaleCheck) Detail() string {
	when := fmt.Sprintf("last completed %s ago", roundDuration(s.Age))
	if !s.EverSucceeded {
		when = fmt.Sprintf("has not completed once in the %s since Forge first ran it", roundDuration(s.Age))
	}
	return fmt.Sprintf(
		"The %s for anvil %s %s, against a threshold of %s. Forge is not saying why: this reads only whether a "+
			"cycle finished, never how a failure was classified, so it fires just as well when the failure was "+
			"reported as transient and retried quietly. Look at the anvil's recent events and at daemon.log for "+
			"this checker. The entry withdraws itself on the next completed cycle.",
		CheckerLabel(s.Checker), s.Anvil, when, roundDuration(s.Threshold))
}

// roundDuration renders a duration at the coarsest unit that still carries
// information, so a headline reads "3 days" rather than "72h13m41.2s".
func roundDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	}
}

// BeginCheck records that a checker has STARTED a cycle for an anvil, if it has
// no row yet. This is what makes "never succeeded" observable: without it, a
// checker that has failed every time since Forge started has no row at all, and
// an absent row is indistinguishable from a checker nobody has configured.
//
// Registering on start, rather than from a list of configured checkers, is also
// what keeps the set honest — a disabled checker never registers and so can
// never go stale, and no second place has to be kept in step with which
// checkers exist.
func (db *DB) BeginCheck(anvil, checker string) error {
	if anvil == "" || checker == "" {
		return fmt.Errorf("check needs an anvil and a checker (got %q/%q)", anvil, checker)
	}
	_, err := db.conn.Exec(
		`INSERT INTO anvil_check_success (anvil, checker, first_seen_at, last_success_at)
		 VALUES (?, ?, ?, '')
		 ON CONFLICT(anvil, checker) DO NOTHING`,
		anvil, checker, time.Now().Format(dbTimeLayout))
	return err
}

// RecordCheckSuccess stamps a completed cycle.
//
// Written on SUCCESS and never on failure, which is what lets one timestamp
// answer both "is it failing" and "did it silently stop running": a checker
// whose goroutine is gone writes nothing, exactly like one that fails every
// time.
func (db *DB) RecordCheckSuccess(anvil, checker string) error {
	if anvil == "" || checker == "" {
		return fmt.Errorf("check success needs an anvil and a checker (got %q/%q)", anvil, checker)
	}
	now := time.Now().Format(dbTimeLayout)
	_, err := db.conn.Exec(
		`INSERT INTO anvil_check_success (anvil, checker, first_seen_at, last_success_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(anvil, checker) DO UPDATE SET last_success_at = excluded.last_success_at`,
		anvil, checker, now, now)
	return err
}

// CheckRecord is one stored row.
type CheckRecord struct {
	Anvil       string
	Checker     string
	FirstSeen   time.Time
	LastSuccess time.Time
}

// CheckRecords returns every recorded (anvil, checker) pair.
func (db *DB) CheckRecords() ([]CheckRecord, error) {
	rows, err := db.conn.Query(
		`SELECT anvil, checker, first_seen_at, last_success_at FROM anvil_check_success`)
	if err != nil {
		return nil, fmt.Errorf("reading anvil check records: %w", err)
	}
	defer rows.Close()

	var out []CheckRecord
	for rows.Next() {
		var r CheckRecord
		var first, last string
		if err := rows.Scan(&r.Anvil, &r.Checker, &first, &last); err != nil {
			return nil, fmt.Errorf("scanning anvil check record: %w", err)
		}
		// An unparseable timestamp is left as the zero time, which reads as
		// very old — the safe direction here, since a row Forge cannot date is
		// one to surface rather than one to call healthy.
		r.FirstSeen, _ = time.Parse(dbTimeLayout, first)
		r.LastSuccess, _ = time.Parse(dbTimeLayout, last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StalenessParams is what a caller supplies to judge staleness. The thresholds
// are passed in rather than read here because they come from configuration, and
// the state package does not read configuration.
type StalenessParams struct {
	// Thresholds is the maximum age per checker. A checker absent from the map
	// is never judged, so a caller that cannot resolve an interval reports
	// nothing rather than inventing one.
	Thresholds map[string]time.Duration
	// KnownAnvils bounds the answer to anvils that still exist. A deregistered
	// anvil's rows would otherwise be stale forever with no action that could
	// clear them.
	//
	// NIL means do not filter; a non-nil map filters even when it is EMPTY.
	// The distinction is load-bearing rather than pedantic: a caller with no
	// anvils configured supplies an empty map, and reading that as "do not
	// filter" would report every leftover row in the table — the worst kind of
	// false positive, since not one of them names an anvil the reader still has.
	KnownAnvils map[string]bool
	// Now is injected so the judgement is testable without sleeping.
	Now time.Time
}

// StaleChecks returns the (anvil, checker) pairs whose last completed cycle is
// older than their threshold, most overdue first.
//
// It reads no error classification of any kind, deliberately. This is the
// backstop for a failure that was classified transient and retried quietly
// forever, so one that consulted the same classification would be blind to
// exactly the fault it exists to catch.
func (db *DB) StaleChecks(p StalenessParams) ([]StaleCheck, error) {
	if len(p.Thresholds) == 0 {
		return nil, nil
	}
	records, err := db.CheckRecords()
	if err != nil {
		return nil, err
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	var stale []StaleCheck
	for _, r := range records {
		threshold, ok := p.Thresholds[r.Checker]
		if !ok || threshold <= 0 {
			continue
		}
		if p.KnownAnvils != nil && !p.KnownAnvils[r.Anvil] {
			continue
		}
		since := r.LastSuccess
		ever := !r.LastSuccess.IsZero()
		if !ever {
			since = r.FirstSeen
		}
		age := now.Sub(since)
		if age <= threshold {
			continue
		}
		stale = append(stale, StaleCheck{
			Anvil:         r.Anvil,
			Checker:       r.Checker,
			Since:         since,
			EverSucceeded: ever,
			Threshold:     threshold,
			Age:           age,
		})
	}

	sortStaleChecks(stale)
	return stale, nil
}

// sortStaleChecks orders the panel: most overdue first, then anvil and checker
// so equal ages render in a stable order rather than in SQLite's.
//
// Split out because it is the one part of the judgement that can be tested
// against exact ages — the timestamps themselves are written with time.Now(),
// so a test driving the table can only ever produce rows of the same age.
func sortStaleChecks(stale []StaleCheck) {
	sort.SliceStable(stale, func(i, j int) bool {
		if stale[i].Age != stale[j].Age {
			return stale[i].Age > stale[j].Age
		}
		if stale[i].Anvil != stale[j].Anvil {
			return stale[i].Anvil < stale[j].Anvil
		}
		return stale[i].Checker < stale[j].Checker
	})
}
