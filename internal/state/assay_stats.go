package state

import (
	"database/sql"
	"time"
)

// AssayRunSample is the minimal projection of an assay_runs row needed to
// aggregate spend and duration over time: when the run finished, what it cost,
// how long it took and which coverage outcome it reached.
//
// It is deliberately narrower than AssayRun. The question it answers ("what
// does a run cost, and is that number moving?") spans every run of every PR, so
// the row set is large and none of the per-run detail — findings, cache
// accounting, the failed-pass list — is part of the answer.
type AssayRunSample struct {
	// CompletedAt is finished_at, falling back to started_at for a row whose
	// finish was never recorded. It is what a run is bucketed by: a run is
	// spend at the moment it stops spending, not when it started.
	CompletedAt time.Time
	DurationMs  int64
	CostUSD     float64
	// Status is the coverage outcome as persisted: AssayStatusComplete,
	// AssayStatusPartial, AssayStatusFailed, or empty on rows written before
	// coverage was recorded.
	Status string
}

// AssayRunSamplesSince returns one sample per executed Assay run started at or
// after the cutoff, oldest first.
//
// Skipped runs are excluded on the same terms as CountAssayRuns: a run that
// never reviewed a diff (a failed diff fetch, a trigger-gate refusal) reviewed
// nothing and spent nothing, so counting it would dilute every mean it lands
// in. A run that failed after spending is NOT skipped and is included — a
// failure is not a refund, and a week in which the spend moved into failed runs
// is exactly the drift this data exists to expose.
//
// The cutoff is formatted with dbTimeLayout, whose lexicographic ordering
// matches chronological ordering, so the TEXT comparison is valid.
func (db *DB) AssayRunSamplesSince(since time.Time) ([]AssayRunSample, error) {
	rows, err := db.conn.Query(
		`SELECT started_at, finished_at, duration_ms, cost_usd, status
		 FROM assay_runs
		 WHERE started_at >= ? AND COALESCE(skipped_reason, '') = ''
		 ORDER BY started_at ASC`,
		since.UTC().Format(dbTimeLayout),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AssayRunSample
	for rows.Next() {
		var (
			s          AssayRunSample
			startedAt  string
			finishedAt sql.NullString
		)
		if err := rows.Scan(&startedAt, &finishedAt, &s.DurationMs, &s.CostUSD, &s.Status); err != nil {
			return nil, err
		}
		if finishedAt.Valid && finishedAt.String != "" {
			s.CompletedAt = parseTime(finishedAt.String)
		}
		if s.CompletedAt.IsZero() {
			s.CompletedAt = parseTime(startedAt)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
