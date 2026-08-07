package state

import (
	"database/sql"
	"fmt"
	"time"
)

// ReviewFixDispatch counts the burnish (review-fix) workers dispatched against
// one PR head.
//
// The lifecycle manager's ReviewFixCnt bounds review fixes per PR for its whole
// life, which conflates two very different situations: a PR that is genuinely
// progressing (each fix moves the head, each round addresses new comments) and
// a PR that rebuilds the identical work every cycle because the previous fix
// never landed. The second is what burns 25 minutes of Opus per round for
// nothing (Forge-xl50), and it is recognisable by exactly one thing: the head
// did not move.
//
// So this row is keyed to the head SHA and resets the moment the head changes.
// A count that keeps climbing against an unchanged head means the loop is not
// converging, and that is what the circuit breaker trips on.
type ReviewFixDispatch struct {
	Anvil    string
	PRNumber int
	// HeadSHA is the PR head the attempts were made against. A dispatch for a
	// different head resets Attempts to 1.
	HeadSHA string
	// Attempts is how many review-fix workers have been dispatched against
	// HeadSHA.
	Attempts int
	// LastResult is a short outcome tag for the most recent dispatch
	// ("pushed", "unverified_push", "preserved", "failed", ...), recorded so an
	// operator reading a tripped breaker can see how the attempts ended.
	LastResult string
	UpdatedAt  time.Time
}

// Result tags recorded in ReviewFixDispatch.LastResult. They are short, stable
// strings so a tripped breaker's Needs Attention entry reads the same way in
// every daemon lifetime.
const (
	// ReviewFixResultPushed — the fix was verified and pushed.
	ReviewFixResultPushed = "pushed"
	// ReviewFixResultUnverifiedPush — verification never completed, the fix was
	// pushed anyway and marked unverified.
	ReviewFixResultUnverifiedPush = "unverified_push"
	// ReviewFixResultPreserved — the fix could not be pushed and its worktree
	// was kept so the commit is recoverable.
	ReviewFixResultPreserved = "preserved"
	// ReviewFixResultFailed — the attempt produced nothing to push.
	ReviewFixResultFailed = "failed"
)

// RecordReviewFixDispatch registers a review-fix dispatch against headSHA and
// returns the attempt count for that head, this dispatch included. A headSHA
// different from the stored one resets the count to 1: a moved head is
// genuinely new work and must not inherit the previous head's budget.
func (db *DB) RecordReviewFixDispatch(anvil string, prNumber int, headSHA string) (int, error) {
	if anvil == "" || prNumber <= 0 {
		return 0, fmt.Errorf("review fix dispatch requires anvil and pr_number")
	}
	if headSHA == "" {
		return 0, fmt.Errorf("review fix dispatch requires a head SHA")
	}
	now := time.Now().Format(dbTimeLayout)
	// The head_sha comparison lives in the upsert so the read and the increment
	// are one statement — two concurrent dispatches for the same PR cannot both
	// read attempts=N and both write N+1.
	_, err := db.conn.Exec(
		`INSERT INTO review_fix_dispatches (anvil, pr_number, head_sha, attempts, last_result, updated_at)
		 VALUES (?, ?, ?, 1, '', ?)
		 ON CONFLICT(anvil, pr_number) DO UPDATE SET
			attempts    = CASE WHEN review_fix_dispatches.head_sha = excluded.head_sha
			                   THEN review_fix_dispatches.attempts + 1 ELSE 1 END,
			last_result = CASE WHEN review_fix_dispatches.head_sha = excluded.head_sha
			                   THEN review_fix_dispatches.last_result ELSE '' END,
			head_sha    = excluded.head_sha,
			updated_at  = excluded.updated_at`,
		anvil, prNumber, headSHA, now,
	)
	if err != nil {
		return 0, fmt.Errorf("recording review fix dispatch for %s/%d: %w", anvil, prNumber, err)
	}
	var attempts int
	err = db.conn.QueryRow(
		`SELECT attempts FROM review_fix_dispatches WHERE anvil = ? AND pr_number = ?`,
		anvil, prNumber).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("reading review fix dispatch count for %s/%d: %w", anvil, prNumber, err)
	}
	return attempts, nil
}

// GetReviewFixDispatch returns the recorded dispatch bookkeeping for a PR, or
// nil when none exists. A missing row is not an error.
func (db *DB) GetReviewFixDispatch(anvil string, prNumber int) (*ReviewFixDispatch, error) {
	var r ReviewFixDispatch
	var updatedAt string
	err := db.conn.QueryRow(
		`SELECT anvil, pr_number, head_sha, attempts, last_result, updated_at
		 FROM review_fix_dispatches WHERE anvil = ? AND pr_number = ?`,
		anvil, prNumber).Scan(&r.Anvil, &r.PRNumber, &r.HeadSHA, &r.Attempts, &r.LastResult, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading review fix dispatch for %s/%d: %w", anvil, prNumber, err)
	}
	r.UpdatedAt = parseTime(updatedAt)
	return &r, nil
}

// SetReviewFixDispatchResult records how the most recent dispatch ended. It is
// a no-op when no row exists — the outcome of a dispatch nobody counted is not
// worth inventing a row for.
func (db *DB) SetReviewFixDispatchResult(anvil string, prNumber int, result string) error {
	_, err := db.conn.Exec(
		`UPDATE review_fix_dispatches SET last_result = ?, updated_at = ?
		 WHERE anvil = ? AND pr_number = ?`,
		result, time.Now().Format(dbTimeLayout), anvil, prNumber)
	if err != nil {
		return fmt.Errorf("recording review fix dispatch result for %s/%d: %w", anvil, prNumber, err)
	}
	return nil
}

// DeleteReviewFixDispatch drops the bookkeeping for a PR. Called when the PR
// leaves the fix loop for good (merged or closed) and when an operator resets
// the PR's fix counters, so a retry starts from a clean budget.
func (db *DB) DeleteReviewFixDispatch(anvil string, prNumber int) error {
	_, err := db.conn.Exec(
		`DELETE FROM review_fix_dispatches WHERE anvil = ? AND pr_number = ?`, anvil, prNumber)
	return err
}
