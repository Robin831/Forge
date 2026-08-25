package bellows

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

// admits drops admitAssayRun's reserved-total return so a test that only cares
// whether the review was let through reads as one expression.
func admits(admitted bool, _ float64) bool { return admitted }

// ledgerMonitor is a Monitor with nothing wired but the Assay budget: the
// reservation ledger touches no VCS provider and, unless a test says
// otherwise, no database.
func ledgerMonitor(db *state.DB, floorUSD float64) *Monitor {
	m := New(db, nil, time.Minute, nil, nil, nil, nil, nil)
	m.SetAssayConfig(func(string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, DailyCostLimitUSD: 10, RunCostEstimateUSD: floorUSD}
	})
	return m
}

func TestAssayBudgetAdmits(t *testing.T) {
	tests := []struct {
		name                                string
		recorded, reserved, estimate, limit float64
		want                                bool
	}{
		{"no limit admits anything", 500, 500, 500, 0, true},
		{"comfortably under", 1, 1, 1, 10, true},
		{"exactly at the limit is admitted", 4, 4, 2, 10, true},
		{"one cent over is refused", 4, 4, 2.01, 10, false},
		{"recorded alone leaves no room", 10, 0, 1, 10, false},
		// The case the whole mechanism exists for: recorded spend is well
		// under the cap and the gate would have admitted on it alone, but the
		// reviews already running have the budget spoken for.
		{"in-flight reservations close the gap", 6, 6, 3, 10, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, assayBudgetAdmits(tt.recorded, tt.reserved, tt.estimate, tt.limit))
		})
	}
}

// TestAdmitAssayRunBlocksTheRunThatWouldBreakTheCap is the bead's case: with a
// limit that fits three reviews, the fourth admission is refused rather than
// admitted to run the day over budget.
func TestAdmitAssayRunBlocksTheRunThatWouldBreakTheCap(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	const (
		limit    = 10.0
		estimate = 3.0
	)

	for i := 0; i < 3; i++ {
		assert.True(t, admits(m.admitAssayRun(assayRunKey("anvil", i, "head"), estimate, 0, limit)),
			"review %d fits under the cap (%0.2f reserved so far)", i, float64(i)*estimate)
	}
	assert.False(t, admits(m.admitAssayRun(assayRunKey("anvil", 3, "head"), estimate, 0, limit)),
		"the fourth review would project $12.00 against a $10.00 limit and must be refused")
	assert.InDelta(t, 9.0, m.reservedAssayCostUSD(), 1e-9,
		"a refused admission must not leave a reservation behind")
}

// TestAdmitAssayRunIsAtomicUnderConcurrency dispatches the admissions from
// separate goroutines, which is the arrangement a check-then-reserve pair
// loses: every one of them would read the same reserved total and every one
// would be admitted.
func TestAdmitAssayRunIsAtomicUnderConcurrency(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	const (
		limit     = 10.0
		estimate  = 3.0
		attempts  = 24
		wantAdmit = 3 // floor(10 / 3)
	)

	var (
		mu       sync.Mutex
		admitted int
		wg       sync.WaitGroup
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ok, _ := m.admitAssayRun(assayRunKey("anvil", i, "head"), estimate, 0, limit); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, wantAdmit, admitted, "exactly floor(limit/estimate) concurrent reviews may be admitted")
	assert.InDelta(t, float64(wantAdmit)*estimate, m.reservedAssayCostUSD(), 1e-9)
}

// TestAdmitAssayRunReAdmittingOneKeyDoesNotDoubleCount covers the same head
// passing the gate twice before its run starts: the second admission replaces
// the first hold rather than stacking a second one on top of it.
func TestAdmitAssayRunReAdmittingOneKeyDoesNotDoubleCount(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	key := assayRunKey("anvil", 7, "headsha")

	require.True(t, admits(m.admitAssayRun(key, 3, 0, 10)))
	require.True(t, admits(m.admitAssayRun(key, 3, 0, 10)))
	assert.InDelta(t, 3.0, m.reservedAssayCostUSD(), 1e-9)
}

// TestReleaseAssayReservationFreesTheBudgetAndFeedsTheEstimate walks the whole
// loop: a full ledger refuses, the finished run releases, and the refused
// review is admitted on the next attempt — with the cost it actually incurred
// now sizing the estimate.
func TestReleaseAssayReservationFreesTheBudgetAndFeedsTheEstimate(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	const limit = 10.0

	require.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 1, "h1"), 4, 0, limit)))
	require.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 2, "h2"), 4, 0, limit)))
	require.False(t, admits(m.admitAssayRun(assayRunKey("anvil", 3, "h3"), 4, 0, limit)))

	m.ReleaseAssayReservation("anvil", 1, "h1", 6.50)

	assert.InDelta(t, 4.0, m.reservedAssayCostUSD(), 1e-9, "the finished review's hold is gone")
	assert.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 3, "h3"), 4, 0, limit)),
		"the deferred review is admitted once the budget frees up")
	assert.InDelta(t, 6.50, m.assayRunCostEstimate(0, 0), 1e-9,
		"the recorded cost is what the next estimate is taken from")
}

// TestReleaseAssayReservationIgnoresACostlessRun keeps a run that spent nothing
// — a failed diff fetch, a skipped review — out of the rolling mean. Folding it
// in would drag the estimate toward zero, which is the one direction an
// estimate guarding a spend cap must not err in.
func TestReleaseAssayReservationIgnoresACostlessRun(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	m.ReleaseAssayReservation("anvil", 1, "h1", 8.0)
	require.InDelta(t, 8.0, m.assayRunCostEstimate(0, 0), 1e-9)

	m.ReleaseAssayReservation("anvil", 2, "h2", 0)
	assert.InDelta(t, 8.0, m.assayRunCostEstimate(0, 0), 1e-9)
}

// TestReleaseAssayReservationIsIdempotent — the run path releases in a defer
// and the reservation may already have expired underneath it.
func TestReleaseAssayReservationIsIdempotent(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	require.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 1, "h1"), 4, 0, 10)))
	m.ReleaseAssayReservation("anvil", 1, "h1", 4)
	m.ReleaseAssayReservation("anvil", 1, "h1", 4)
	m.ReleaseAssayReservation("anvil", 9, "never-held", 4)
	assert.Zero(t, m.reservedAssayCostUSD())
}

// TestAssayAdmissionExpiresWhenNoRunPicksItUp covers a dispatch dropped between
// the gate and the daemon (no worker slot, a failed worker-row insert). Without
// the lease that hold would sit on the budget for the rest of the day.
func TestAssayAdmissionExpiresWhenNoRunPicksItUp(t *testing.T) {
	m := ledgerMonitor(nil, 0)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	require.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 1, "h1"), 4, 0, 10)))
	require.InDelta(t, 4.0, m.reservedAssayCostUSD(), 1e-9)

	now = now.Add(assayAdmissionLease - time.Minute)
	assert.InDelta(t, 4.0, m.reservedAssayCostUSD(), 1e-9, "still inside the lease")

	now = now.Add(2 * time.Minute)
	assert.Zero(t, m.reservedAssayCostUSD(), "the abandoned admission must not hold budget forever")
}

// TestHoldAssayReservationOutlivesTheAdmissionLease: a review that runs longer
// than the emit→dispatch lease keeps its hold, because the run pins it on a
// lease of its own.
func TestHoldAssayReservationOutlivesTheAdmissionLease(t *testing.T) {
	m := ledgerMonitor(nil, 4)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	require.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 1, "h1"), 4, 0, 10)))
	held := m.HoldAssayReservation("anvil", 1, "h1")
	assert.InDelta(t, 4.0, held, 1e-9)
	assert.InDelta(t, 4.0, m.reservedAssayCostUSD(), 1e-9,
		"pinning an admitted review refreshes its hold rather than adding a second one")

	now = now.Add(assayAdmissionLease + time.Minute)
	assert.InDelta(t, 4.0, m.reservedAssayCostUSD(), 1e-9, "a running review keeps its reservation")

	now = now.Add(assayRunLease)
	assert.Zero(t, m.reservedAssayCostUSD(), "the run lease is the backstop on a hold never released")
}

// TestHoldAssayReservationKeepsTheLargerAdmissionEstimate: the gate admitted the
// review against its estimate, so a smaller one at run start must not hand back
// budget the admission decision was made on.
func TestHoldAssayReservationKeepsTheLargerAdmissionEstimate(t *testing.T) {
	m := ledgerMonitor(nil, 1)
	require.True(t, admits(m.admitAssayRun(assayRunKey("anvil", 1, "h1"), 6, 0, 10)))
	assert.InDelta(t, 6.0, m.HoldAssayReservation("anvil", 1, "h1"), 1e-9)
}

// TestHoldAssayReservationCountsARunTheGateNeverSaw — `forge assay rerun` and
// the Burnish coordination path start reviews without passing the gate. They
// are not refused on budget, but their spend must still be visible to the gate
// while it is in flight.
func TestHoldAssayReservationCountsARunTheGateNeverSaw(t *testing.T) {
	m := ledgerMonitor(nil, 5)
	assert.InDelta(t, 5.0, m.HoldAssayReservation("anvil", 42, "manual"), 1e-9)
	assert.InDelta(t, 5.0, m.reservedAssayCostUSD(), 1e-9)
	assert.False(t, admits(m.admitAssayRun(assayRunKey("anvil", 1, "h1"), 5, 2, 10)),
		"a manual re-run's in-flight spend counts against an automatic review's admission")
}

func TestAssayRunCostEstimateSeedsFromRecordedRuns(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	for i, cost := range []float64{6, 8, 10} {
		require.NoError(t, db.RecordAssayRun(&state.AssayRun{
			Anvil: "anvil", PRNumber: i + 1, HeadSHA: "h", StartedAt: time.Now(), CostUSD: cost,
		}))
	}
	// A skipped run reviewed nothing and spent nothing: it must not dilute the
	// mean the estimate is taken from.
	require.NoError(t, db.RecordAssayRun(&state.AssayRun{
		Anvil: "anvil", PRNumber: 4, HeadSHA: "h", StartedAt: time.Now(), SkippedReason: "no reviewable changes",
	}))

	m := ledgerMonitor(db, 0)
	assert.InDelta(t, 8.0, m.assayRunCostEstimate(0, 0), 1e-9, "mean of the recorded runs")
	assert.InDelta(t, 12.0, m.assayRunCostEstimate(12, 0), 1e-9, "the floor wins when it is the larger")
}

// TestAssayRunCostEstimateFallsBackToTheFloorOnAColdLedger — a daemon that has
// just started, with nothing recorded, must still reserve something.
func TestAssayRunCostEstimateFallsBackToTheFloorOnAColdLedger(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()
	m := ledgerMonitor(db, 5)
	assert.InDelta(t, 5.0, m.assayRunCostEstimate(5, 0), 1e-9)
}

// TestMaybeEmitReviewNeeded_DefersTheReviewThatWouldBreakTheDailyCap is the
// end-to-end shape of the bead: three PRs due for review, a cap that fits two
// of them, and the third deferred rather than dispatched — with the deferral
// clearing once one of the running reviews records its cost.
func TestMaybeEmitReviewNeeded_DefersTheReviewThatWouldBreakTheDailyCap(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	prs := make([]*state.PR, 3)
	for i := range prs {
		prs[i] = &state.PR{
			Number:    301 + i,
			Anvil:     "my-anvil",
			BeadID:    "forge-cap",
			Branch:    "forge/forge-cap",
			Status:    state.PROpen,
			CreatedAt: time.Now(),
		}
		require.NoError(t, db.InsertPR(prs[i]))
	}

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
	m.SetAssayConfig(func(string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, SkipDrafts: true, DebounceSeconds: 300,
			DailyCostLimitUSD: 10, RunCostEstimateUSD: 4}
	})

	var mu sync.Mutex
	var got []PREvent
	m.OnEvent(func(_ context.Context, ev PREvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	emitFor := func(pr *state.PR, head string) {
		status := &vcs.PRStatus{State: "OPEN", HeadRefName: "forge/forge-cap", HeadSHA: head}
		m.maybeEmitReviewNeeded(context.Background(), pr, status, &prSnapshot{CIPassing: true}, float64Ptr(0))
	}

	emitFor(prs[0], "head1") // 0 + 0 + 4 <= 10
	emitFor(prs[1], "head2") // 0 + 4 + 4 <= 10
	emitFor(prs[2], "head3") // 0 + 8 + 4  > 10

	mu.Lock()
	require.Len(t, got, 2, "the third review must be deferred, not dispatched over the cap")
	mu.Unlock()
	assert.InDelta(t, 8.0, m.reservedAssayCostUSD(), 1e-9)

	// A deferred head is NOT released to merge readiness: unlike an exhausted
	// day, this clears in minutes, and calling the head reviewed would let the
	// PR merge on the strength of a queue.
	assert.False(t, m.assayUpToDate(prs[2], "head3", float64Ptr(0)),
		"a reservation-deferred head must stay unreviewed for merge readiness")

	// The first review finishes and records what it cost; the deferred one is
	// dispatched on the next poll.
	m.ReleaseAssayReservation("my-anvil", prs[0].Number, "head1", 3.0)
	emitFor(prs[2], "head3")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 3)
	assert.Equal(t, "head3", got[2].HeadSHA)
}

// TestShouldEmitReviewNeeded_BudgetReasons pins which of the two budget
// refusals fires, because they are not interchangeable: one releases the head
// to merge readiness for the rest of the day and the other must not.
func TestShouldEmitReviewNeeded_BudgetReasons(t *testing.T) {
	base := func() reviewGateInputs {
		return reviewGateInputs{
			enabled: true, managed: true, open: true,
			headSHA: "head", now: time.Now(),
			debounceSeconds: 300,
			dailyCostLimit:  10,
			estimateUSD:     4,
		}
	}
	tests := []struct {
		name       string
		mutate     func(*reviewGateInputs)
		wantEmit   bool
		wantReason string
	}{
		{"room for the review", func(in *reviewGateInputs) {}, true, ""},
		{"recorded spend at the cap", func(in *reviewGateInputs) { in.dailyCostUSD = 10 }, false, assaySuppressedDailyCost},
		// Durable for the day: no reservation is involved, the day simply has
		// less left than a review costs.
		{"headroom smaller than one review", func(in *reviewGateInputs) { in.dailyCostUSD = 7 }, false, assaySuppressedDailyCost},
		// Transient: the budget fits this review, the ones already running are
		// what leaves no room.
		{"in-flight reviews leave no room", func(in *reviewGateInputs) { in.reservedUSD = 7 }, false, assaySuppressedInFlightBudget},
		{"in-flight reviews still leave room", func(in *reviewGateInputs) { in.reservedUSD = 6 }, true, ""},
		{"no limit ignores both", func(in *reviewGateInputs) {
			in.dailyCostLimit = 0
			in.dailyCostUSD, in.reservedUSD = 99, 99
		}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.mutate(&in)
			emit, reason := shouldEmitReviewNeeded(in)
			assert.Equal(t, tt.wantEmit, emit)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

// TestAssayUpToDateReleasesAHeadTheBudgetCanNoLongerCover is the other half of
// that distinction. The gate refuses a review the remaining budget cannot pay
// for; if readiness were not released on the same condition, every green PR
// would sit out of Ready-to-Merge until UTC midnight with nothing to show why.
func TestAssayUpToDateReleasesAHeadTheBudgetCanNoLongerCover(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{Number: 401, Anvil: "my-anvil", BeadID: "forge-hr", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(pr))

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
	m.SetAssayConfig(func(string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, DailyCostLimitUSD: 10, RunCostEstimateUSD: 4}
	})

	assert.False(t, m.assayUpToDate(pr, "head", float64Ptr(2)), "$8 left covers a $4 review")
	assert.True(t, m.assayUpToDate(pr, "head", float64Ptr(7)), "$3 left will never cover a $4 review today")
	assert.True(t, m.assayUpToDate(pr, "head", float64Ptr(10)), "the budget is spent")

	// A reservation is not a reason to release: it clears in minutes.
	require.True(t, admits(m.admitAssayRun(assayRunKey("my-anvil", 401, "head"), 4, 2, 10)))
	assert.False(t, m.assayUpToDate(pr, "head", float64Ptr(2)),
		"an in-flight reservation must not release the head to merge readiness")
}

// shippedDefaultsMonitor builds a Monitor whose Assay gate config is the one an
// unconfigured deployment actually runs: the shipped daily cap and the shipped
// estimate floor, read from config.Defaults() rather than restated as literals,
// so this test follows either number if it moves.
func shippedDefaultsMonitor(t *testing.T, db *state.DB) (m *Monitor, limit, floor float64) {
	t.Helper()
	def := config.Defaults().Assay
	limit, floor = def.GetDailyCostLimitUSD(), def.GetRunCostEstimateUSD()
	m = New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
	m.SetAssayConfig(func(string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, SkipDrafts: true, DebounceSeconds: 300,
			DailyCostLimitUSD: limit, RunCostEstimateUSD: floor}
	})
	return m, limit, floor
}

// insertReviewablePRs creates n open PRs on one anvil, each of which the Assay
// trigger gate would dispatch a review for.
func insertReviewablePRs(t *testing.T, db *state.DB, first, n int) []*state.PR {
	t.Helper()
	prs := make([]*state.PR, n)
	for i := range prs {
		prs[i] = &state.PR{
			Number:    first + i,
			Anvil:     "my-anvil",
			BeadID:    "forge-def",
			Branch:    "forge/forge-def",
			Status:    state.PROpen,
			CreatedAt: time.Now(),
		}
		require.NoError(t, db.InsertPR(prs[i]))
	}
	return prs
}

// TestAssayBudgetUnderShippedDefaults pins the headroom branch against the
// SHIPPED defaults rather than against hand-picked numbers.
//
// It exists because every other budget test here chooses a limit and a floor
// that are comfortably apart (limit 10, floor 4), and in that configuration the
// branch behaves benignly whatever the relationship between the two defaults
// happens to be. The refusal it guards is not benign: a review refused under
// daily_cost_limit has its head released to merge readiness by assayUpToDate,
// so a default pair that refuses early does not review PRs more cheaply — it
// auto-merges them unreviewed, which is what happened on 2026-08-07 (twenty
// PRs) and 2026-08-09.
//
// The specific arrangement that produces it is floor >= cap: `recorded + 0 +
// estimate <= limit` then fails at the first cent of recorded spend, and every
// PR for the rest of the day is released as "reviewed".
func TestAssayBudgetUnderShippedDefaults(t *testing.T) {
	def := config.Defaults().Assay
	limit, floor := def.GetDailyCostLimitUSD(), def.GetRunCostEstimateUSD()

	require.Greater(t, limit, 0.0, "the shipped default must keep a cap; 0 would mean unlimited Assay spend")
	require.Less(t, floor, limit,
		"the shipped estimate floor ($%.2f) must sit materially below the shipped daily cap ($%.2f): "+
			"at floor >= cap an unconfigured deployment admits at most one review a day and releases every "+
			"later head to merge readiness unreviewed", floor, limit)
}

// TestShippedDefaultsKeepReviewingAfterTheDayHasSpentSomething is the probe
// from the review, as a test: one cent of recorded spend must not close the day.
func TestShippedDefaultsKeepReviewingAfterTheDayHasSpentSomething(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	m, limit, floor := shippedDefaultsMonitor(t, db)
	const recorded = 0.01
	wantAdmit := int(math.Floor((limit - recorded) / floor))
	require.GreaterOrEqual(t, wantAdmit, 2,
		"with the shipped defaults a day that has spent a cent must still fit more than one review")

	prs := insertReviewablePRs(t, db, 501, wantAdmit+1)

	var mu sync.Mutex
	var got []PREvent
	m.OnEvent(func(_ context.Context, ev PREvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	for i, pr := range prs {
		status := &vcs.PRStatus{State: "OPEN", HeadRefName: "forge/forge-def", HeadSHA: fmt.Sprintf("head%d", i)}
		m.maybeEmitReviewNeeded(context.Background(), pr, status, &prSnapshot{CIPassing: true}, float64Ptr(recorded))
	}

	mu.Lock()
	assert.Len(t, got, wantAdmit, "the day's budget must admit every review that fits under it, not just the first")
	mu.Unlock()

	// The first PR — the one a floor == cap default would already have refused
	// — must not have been released to merge readiness as "reviewed".
	assert.False(t, m.assayUpToDate(prs[0], "head0", float64Ptr(recorded)),
		"a head the budget can still pay to review must not count as reviewed")
}

// TestShippedDefaultsStillAdmitAReviewWhenTheMeanExceedsTheCap is the other
// half: the rolling mean seeds from recorded history, so a deployment whose
// reviews have cost more than its own cap (this one: 87 runs, mean $7.05,
// against the shipped $5.00) seeds an estimate no day could ever satisfy.
// Unclamped, that refuses every review from a completely unspent day and turns
// Assay off while leaving auto-merge running.
func TestShippedDefaultsStillAdmitAReviewWhenTheMeanExceedsTheCap(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	m, limit, floor := shippedDefaultsMonitor(t, db)
	prs := insertReviewablePRs(t, db, 601, 1)

	// Fold a run more expensive than the whole daily cap into the rolling mean,
	// the way a finished review does.
	m.ReleaseAssayReservation("my-anvil", 999, "gone", 7.05)
	require.Greater(t, 7.05, limit, "the fixture only means something if the mean exceeds the cap")

	assert.InDelta(t, limit, m.assayRunCostEstimate(floor, limit), 1e-9,
		"an estimate larger than the whole cap is clamped to it, so the projection stays satisfiable")

	var mu sync.Mutex
	var got []PREvent
	m.OnEvent(func(_ context.Context, ev PREvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	status := &vcs.PRStatus{State: "OPEN", HeadRefName: "forge/forge-def", HeadSHA: "head0"}
	m.maybeEmitReviewNeeded(context.Background(), prs[0], status, &prSnapshot{CIPassing: true}, float64Ptr(0))

	mu.Lock()
	assert.Len(t, got, 1, "a day that has spent nothing must always admit one review")
	mu.Unlock()
	assert.False(t, m.assayUpToDate(prs[0], "head0", float64Ptr(0)),
		"an unspent day must not release a head to merge readiness as reviewed")
}
