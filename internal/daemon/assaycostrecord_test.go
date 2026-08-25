package daemon

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/state"
)

// assayLedger reads back the three cost tables plus the per-run assay ledger,
// which is the whole point of these tests: the four numbers have to agree.
type assayLedger struct {
	assayRuns   float64
	daily       float64
	byProvider  map[string]float64
	bead        float64
	dailyTokens [4]int // input, output, cache read, cache write
}

func readAssayLedger(t *testing.T, db *state.DB, beadID, anvil string) assayLedger {
	t.Helper()

	var l assayLedger
	require.NoError(t, db.Conn().QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0) FROM assay_runs`).Scan(&l.assayRuns))

	// No row at all is the "nothing was recorded" answer, not an error: a run
	// that spent nothing must leave daily_costs untouched rather than stamping
	// a zero row on the day.
	in, out, cr, cw, dailyCost, _, err := db.GetDailyCost(cost.Today())
	if !errors.Is(err, sql.ErrNoRows) {
		require.NoError(t, err)
		l.daily = dailyCost
		l.dailyTokens = [4]int{in, out, cr, cw}
	}

	provs, err := db.GetProviderDailyCosts(cost.Today())
	require.NoError(t, err)
	l.byProvider = map[string]float64{}
	for _, p := range provs {
		l.byProvider[p.Provider] = p.EstimatedCost
	}

	l.bead, err = db.GetBeadCost(beadID, anvil)
	require.NoError(t, err)
	return l
}

// completedAssayResult is one finished review whose per-run cost and whose
// ledger usage are the same money reported twice — which is what the engine
// does: ReviewResult.CostUSD is Usage.EstimatedCostUSD.
func completedAssayResult(costUSD float64) *assay.ReviewResult {
	return &assay.ReviewResult{
		Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
		Findings: make([]assay.Finding, 3),
		CostUSD:  costUSD,
		Usage: cost.Usage{
			InputTokens:      12000,
			OutputTokens:     4000,
			CacheReadTokens:  166000,
			CacheWriteTokens: 44200,
			EstimatedCostUSD: costUSD,
		},
		CacheCreationTokens: 44200,
		CacheReadTokens:     166000,
	}
}

// TestRunAssayReviewFoldsSpendIntoDailyCosts is the invariant the whole
// recording exists for: a finished review lands in assay_runs AND in the main
// ledger, for the same amount.
//
// Assay used to bill only into assay_runs.cost_usd, so every PR review was
// invisible to daily_costs, to provider_daily_costs and to the bead's own row —
// the global settings.daily_cost_limit was enforced against a day's spend with
// the reviews cut out of it. Checking the tables agree rather than merely that
// each is non-zero is the point: a wrong projection (dropping cache tokens,
// recording a pass's cost instead of the run's) leaves both populated.
func TestRunAssayReviewFoldsSpendIntoDailyCosts(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return completedAssayResult(2.80), nil
	}

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.Equal(t, 2.80, run.CostUSD)

	l := readAssayLedger(t, db, "Forge-abc1", "forge")
	require.InDelta(t, 2.80, l.assayRuns, 1e-9, "assay_runs keeps its own per-run ledger")
	require.InDelta(t, 2.80, l.daily, 1e-9, "the same run reaches daily_costs")
	require.InDelta(t, 2.80, l.bead, 1e-9, "and the bead's cumulative row")
	// Attributed to the provider the deep passes ran on — the default here,
	// since the test config names none.
	require.InDelta(t, 2.80, l.byProvider["claude"], 1e-9)
	require.Len(t, l.byProvider, 1, "one run must not be split across provider rows")

	// The tokens travel with the dollars: a review recorded with its cost but
	// without its prompt-cache accounting is exactly the row the cache columns
	// exist to make visible.
	require.Equal(t, [4]int{12000, 4000, 166000, 44200}, l.dailyTokens)
}

// TestRunAssayReviewRecordsSpendExactlyOnce is the anti-double-count
// regression. One review is one fold into daily_costs: the daemon's
// runAssayReview is the single recording site, and the pipeline stage wrapper
// (which never runs Assay) and bellows (which dispatches it but never sees its
// cost) must stay out of it. A second call anywhere would leave assay_runs
// correct and the main ledger at twice the money, which reads as a pricing bug.
func TestRunAssayReviewRecordsSpendExactlyOnce(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return completedAssayResult(1.25), nil
	}

	// Three dispatched reviews: the ledger must track the runs one for one, so
	// a duplicate write shows up as 2x rather than being absorbed.
	for i := 0; i < 3; i++ {
		_, err := runTestAssayReview(t, d)
		require.NoError(t, err)
	}

	l := readAssayLedger(t, db, "Forge-abc1", "forge")
	require.InDelta(t, 3.75, l.assayRuns, 1e-9)
	require.InDelta(t, 3.75, l.daily, 1e-9, "one fold per run, not two")
	require.InDelta(t, 3.75, l.byProvider["claude"], 1e-9)
	require.InDelta(t, 3.75, l.bead, 1e-9)
}

// TestRunAssayReviewFoldsFailedRunSpend: a failure is not a refund. A run that
// died still paid for the sessions it made, so its spend reaches the main
// ledger on the same terms as a completed run's — and by the same single call,
// since the two terminal branches are mutually exclusive.
func TestRunAssayReviewFoldsFailedRunSpend(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return nil, &assay.RunError{
			Usage: cost.Usage{
				InputTokens:      8000,
				OutputTokens:     500,
				CacheReadTokens:  900,
				CacheWriteTokens: 41500,
				EstimatedCostUSD: 1.75,
			},
			Err: errors.New("all assay deep passes failed"),
		}
	}

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.Equal(t, state.AssayStatusFailed, run.Status)

	l := readAssayLedger(t, db, "Forge-abc1", "forge")
	require.InDelta(t, 1.75, l.assayRuns, 1e-9)
	require.InDelta(t, 1.75, l.daily, 1e-9)
	require.InDelta(t, 1.75, l.byProvider["claude"], 1e-9)
	require.Equal(t, [4]int{8000, 500, 900, 41500}, l.dailyTokens)
}

// TestRunAssayReviewRecordsNothingForUnspentRuns: a run that never reached a
// provider session writes no ledger row at all. A zero-dollar row in
// daily_costs is not free — it is a day that looks accounted for.
func TestRunAssayReviewRecordsNothingForUnspentRuns(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, d *Daemon)
	}{
		{
			name: "the diff could not be fetched",
			setup: func(_ *testing.T, d *Daemon) {
				d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) {
					return nil, errors.New("exit status 1")
				}
			},
		},
		{
			name: "the engine skipped the head",
			setup: func(_ *testing.T, d *Daemon) {
				d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
					return &assay.ReviewResult{
						Status:        assay.RunStatusComplete,
						SkippedReason: assay.SkipReasonNoReviewableChanges,
					}, nil
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, db := newAssayRunDaemon(t)
			tt.setup(t, d)

			_, err := runTestAssayReview(t, d)
			require.NoError(t, err)

			l := readAssayLedger(t, db, "Forge-abc1", "forge")
			require.Zero(t, l.daily)
			require.Empty(t, l.byProvider)
			require.Zero(t, l.bead)
		})
	}
}
