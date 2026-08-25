package daemon

// Tests for the engine-level Assay skip: a run whose filtered diff carried no
// reviewable hunks dispatches no passes, and every surface that reports it says
// it was skipped rather than reporting an empty finding set as a clean review.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// skippedAssayResult is what the engine returns when shouldSkip fires: complete
// coverage of nothing, with the reason attached.
func skippedAssayResult() *assay.ReviewResult {
	return &assay.ReviewResult{
		Status:        assay.RunStatusComplete,
		SkippedReason: assay.SkipReasonNoReviewableChanges,
		ElidedFiles:   []string{"package-lock.json"},
		ElidedBytes:   4096,
	}
}

func TestRunAssayReviewRecordsEngineSkip(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return skippedAssayResult(), nil
	}

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.Equal(t, assay.SkipReasonNoReviewableChanges, run.SkippedReason)
	require.Equal(t, state.AssayStatusComplete, run.Status)
	require.Zero(t, run.FindingsCount)
	require.Zero(t, run.CostUSD)

	// The head counts as reviewed, so Bellows does not re-dispatch the same
	// skip every poll...
	sha, err := db.LastReviewedSHA("forge", 347)
	require.NoError(t, err)
	require.Equal(t, "deadbeef", sha)
	// ...while the run never consumes the per-PR executed-run cap: no pass ran.
	n, err := db.CountAssayRuns("forge", 347)
	require.NoError(t, err)
	require.Zero(t, n)
}

// A skipped run must not close itself out in the activity feed as "0 findings":
// that is the phrasing of a clean review, and this run read no code at all.
func TestRunAssayReviewSkipEventNamesTheSkip(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return skippedAssayResult(), nil
	}

	_, err := runTestAssayReview(t, d)
	require.NoError(t, err)

	events, err := db.RecentEvents(10)
	require.NoError(t, err)
	var msg string
	for _, e := range events {
		if e.Type == state.EventAssayCompleted {
			msg = e.Message
		}
	}
	require.NotEmpty(t, msg, "expected one assay_completed event for the skipped run")
	require.Contains(t, msg, "skipped: "+assay.SkipReasonNoReviewableChanges)
	require.NotContains(t, msg, "0 findings")
}

// The PR-facing summary line is the other half of the same honesty rule.
func TestAssaySummaryLineReportsSkipRatherThanCleanReview(t *testing.T) {
	skipped := assaySummaryLine(skippedAssayResult())
	require.Contains(t, skipped, "skipped")
	require.NotContains(t, skipped, "no issues found")

	// A run that really did review the diff and found nothing keeps the clean
	// line — the two cases are only distinguishable by the skip reason.
	clean := assaySummaryLine(&assay.ReviewResult{Status: assay.RunStatusComplete})
	require.Equal(t, "Assay (AI review): no issues found.", clean)

	withFindings := assaySummaryLine(&assay.ReviewResult{
		Status: assay.RunStatusComplete,
		Findings: []assay.Finding{
			{Severity: assay.SeverityImportant},
			{Severity: assay.SeverityNit},
		},
	})
	require.Equal(t, "Assay (AI review): 1 important, 1 nit", withFindings)
}
