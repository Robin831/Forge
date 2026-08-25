package state

import (
	"database/sql"
	"fmt"
	"time"
)

// AssayRunHistoryForWindow returns every assay_runs row belonging to a PR that
// has at least one run started in [since, until) — the PR's FULL history, not
// just the rows inside the window.
//
// The wider fetch is the whole point. A repeat-cost report has to know a run's
// ordinal (1 = the PR's first review, n>1 = a re-review), and an ordinal
// derived over the window alone is wrong in exactly the direction that flatters
// the answer: a PR first reviewed the day before the window opens has its
// second review counted as a first, moving repeat spend into the first-run
// column. So the caller derives ordinals over everything returned here and only
// then filters to the window it asked about.
//
// Rows are ordered by (anvil, pr_number, started_at, id) so the ordinal
// derivation sees each PR's runs contiguously and in a deterministic order;
// started_at is written with dbTimeLayout, whose lexicographic order matches
// chronological order, so the TEXT comparisons and ORDER BY are valid.
//
// A zero `until` means "no upper bound". A zero `since` means "from the first
// row on"; both bounds are half-open at the top (started_at < until) so that
// consecutive windows tile without double-counting a run on the boundary.
func (db *DB) AssayRunHistoryForWindow(since, until time.Time) ([]AssayRun, error) {
	// The window predicate is built rather than fixed because a zero bound is
	// "unbounded", not "1 January year 1": passing the zero time as a formatted
	// string would still work for `since` but reads as a real cutoff, and for
	// `until` it would exclude everything.
	where := "1=1"
	var args []any
	if !since.IsZero() {
		where += " AND w.started_at >= ?"
		args = append(args, since.UTC().Format(dbTimeLayout))
	}
	if !until.IsZero() {
		where += " AND w.started_at < ?"
		args = append(args, until.UTC().Format(dbTimeLayout))
	}

	rows, err := db.conn.Query(
		`SELECT r.id, r.anvil, r.pr_number, r.head_sha, r.started_at, r.finished_at,
		        r.duration_ms, r.cost_usd, r.findings_count, r.skipped_reason,
		        r.shadow_mode, r.posted_count, r.error, r.status,
		        r.completed_passes, r.total_passes, r.failed_passes,
		        r.cache_creation_tokens, r.cache_read_tokens, r.log_key, r.pass_findings
		 FROM assay_runs r
		 WHERE EXISTS (
		     SELECT 1 FROM assay_runs w
		     WHERE w.anvil = r.anvil AND w.pr_number = r.pr_number AND `+where+`
		 )
		 ORDER BY r.anvil ASC, r.pr_number ASC, r.started_at ASC, r.id ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying assay run history: %w", err)
	}
	defer rows.Close()

	var out []AssayRun
	for rows.Next() {
		var (
			r            AssayRun
			startedAt    string
			finishedAt   sql.NullString
			shadow       int
			failedPasses string
			passFindings string
		)
		if err := rows.Scan(
			&r.ID, &r.Anvil, &r.PRNumber, &r.HeadSHA, &startedAt, &finishedAt,
			&r.DurationMs, &r.CostUSD, &r.FindingsCount, &r.SkippedReason,
			&shadow, &r.PostedCount, &r.Error, &r.Status,
			&r.CompletedPasses, &r.TotalPasses, &failedPasses,
			&r.CacheCreationTokens, &r.CacheReadTokens, &r.LogKey, &passFindings,
		); err != nil {
			return nil, fmt.Errorf("scanning assay run history: %w", err)
		}
		r.StartedAt = parseTime(startedAt)
		if finishedAt.Valid && finishedAt.String != "" {
			t := parseTime(finishedAt.String)
			r.FinishedAt = &t
		}
		r.ShadowMode = shadow != 0
		r.FailedPasses = DecodeAssayPassFailures(failedPasses)
		r.PassFindings = DecodeAssayPassFindings(passFindings)
		out = append(out, r)
	}
	return out, rows.Err()
}
