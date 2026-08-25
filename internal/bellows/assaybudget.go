package bellows

import (
	"fmt"
	"log"
	"time"
)

// In-flight reservations for the Assay daily cost cap.
//
// DailyCostLimitUSD is measured against assay_runs (state.AssayCostUSDSince),
// and a row lands in that table only when a run ENDS. The gate was therefore
// comparing a limit against completed spend alone, blind to every review
// already running: with N concurrent reviews the day overshoots by roughly N x
// what one review costs, which on 2026-08-19 was $185.45 against a $150 limit.
//
// The fix is the same one the main daily_cost_limit gate already uses for Smith
// workers (Forge-s3w7): a review is admitted only if recorded spend, the
// estimates held for the reviews already in flight, and one more estimate for
// the review being admitted all fit under the limit — and the admitted review
// holds its estimate until its cost is recorded.
//
// The ledger lives here, next to the gate that reads it, rather than in the
// daemon that runs the reviews: the gate is the only thing that can refuse a
// review, and a reservation the refusal cannot see is not a reservation. The
// daemon pins and releases through the two exported methods below.

const (
	// assayRunCostSamples is how many recent recorded runs seed the rolling
	// mean. Twenty is enough to be a mean rather than the last run, and short
	// enough to follow a real change in what a review costs (a model change, a
	// pass added) within a day rather than a month.
	assayRunCostSamples = 20

	// assayAdmissionLease bounds a reservation taken by the gate but never
	// picked up by a run. Admission and dispatch are separate steps — the emit
	// travels through the lifecycle manager, which can drop it (no worker slot,
	// a failed worker-row insert, a PR detached in between) — so without a
	// lease one dropped dispatch would hold budget for the rest of the day and
	// a handful of them would wedge reviews entirely. Thirty minutes is far
	// longer than any emit→dispatch window and far shorter than a day.
	assayAdmissionLease = 30 * time.Minute

	// assayRunLease is the backstop on a reservation pinned by a running
	// review. Release is deferred at the top of the run, so this only fires
	// for a hold whose release never ran at all; it is deliberately longer
	// than any plausible review (five passes, each turn- and cost-bounded)
	// so it can never expire under a review that is still spending.
	assayRunLease = 2 * time.Hour
)

// assayReservation is one estimate held against the daily Assay cap.
type assayReservation struct {
	estimateUSD float64
	// expires bounds the hold. Every reservation has one: a hold with no
	// expiry that is somehow never released would silently disable Assay for
	// the remainder of the daemon's life, which is a worse failure than the
	// overrun this whole mechanism exists to prevent.
	expires time.Time
}

// assayRunKey identifies a review by what both ends of its life know: the
// anvil, the PR and the head being reviewed. It is deliberately not an opaque
// counter — the gate admits a review and the daemon releases it several
// goroutines and one event later, with no channel between them to carry a
// handle, so the two must be able to derive the same key independently.
//
// Because the head is part of the key, a PR whose head advances while a review
// of the previous head is still running holds TWO estimates until the older
// one is released or its assayRunLease expires. That is deliberate rather than
// overlooked: nothing cancels the superseded review, so it is still spending,
// and dropping its hold the moment a new head appeared would free budget
// against a session that is going to bill for it — the exact blindness this
// ledger exists to remove. The cost of keeping it is one PR reserving twice
// its share for at most the width of a review; the cost of dropping it is the
// overrun back.
func assayRunKey(anvil string, prNumber int, headSHA string) string {
	return fmt.Sprintf("%s#%d@%s", anvil, prNumber, headSHA)
}

// assayBudgetAdmits reports whether one more review of `estimate` fits under
// `limit` given the spend already recorded and the estimates already held.
// A limit <= 0 means no cap.
//
// This is the one place the projection is computed. The pure trigger gate
// (shouldEmitReviewNeeded) and the atomic admission below both call it, so the
// answer a test pins and the answer the ledger enforces cannot drift apart.
func assayBudgetAdmits(recorded, reserved, estimate, limit float64) bool {
	if limit <= 0 {
		return true
	}
	return recorded+reserved+estimate <= limit
}

// clampAssayEstimate bounds a per-run estimate by the daily cap it is measured
// against, so an estimate larger than the whole budget cannot turn Assay off.
//
// Without it the projection is unsatisfiable by construction: an estimate above
// the limit fails `recorded + reserved + estimate <= limit` even on a day with
// nothing recorded and nothing in flight, so every review is refused under
// daily_cost_limit — and assayUpToDate, which tests the identical condition,
// then releases every head to merge readiness as "reviewed". A deployment whose
// reviews genuinely cost more than its cap would silently auto-merge every PR
// unreviewed rather than reviewing one and stopping, which is strictly worse
// than the overrun: the cap is a spend limit, never a switch that disables
// review while leaving merges running.
//
// Clamping keeps the guarantee that matters — a day that has spent nothing
// always admits exactly one review — and the first run to finish records real
// spend, at which point the ordinary exhaustion branch takes over and refuses
// the rest of the day. A limit of 0 (no cap) reserves nothing and clamps
// nothing.
func clampAssayEstimate(estimate, limit float64) float64 {
	if limit > 0 && estimate > limit {
		return limit
	}
	return estimate
}

// reservedAssayCostUSD is the total estimate held for admitted-but-unrecorded
// reviews. Expired reservations are pruned as they are found: the read is the
// only moment at which a stale hold matters, so it is also the only moment
// worth walking the map for one.
func (m *Monitor) reservedAssayCostUSD() float64 {
	m.assayBudgetMu.Lock()
	defer m.assayBudgetMu.Unlock()
	return m.reservedAssayCostLocked()
}

func (m *Monitor) reservedAssayCostLocked() float64 {
	now := m.nowFn()
	var total float64
	for key, r := range m.assayReservations {
		if !r.expires.IsZero() && now.After(r.expires) {
			delete(m.assayReservations, key)
			log.Printf("[bellows] Assay reservation %s expired without a recorded run; releasing $%.2f", key, r.estimateUSD)
			continue
		}
		total += r.estimateUSD
	}
	return total
}

// admitAssayRun is the atomic check-and-reserve behind the daily cost gate: it
// re-evaluates the projection and, if it fits, records the hold — both under a
// single acquisition of the lock.
//
// It has to be one operation. Checking and then reserving is a race the gate
// loses in exactly the case it exists for: bellows walks its PRs faster than a
// dispatched review reaches the daemon, so two PRs examined a millisecond apart
// would both read the same reserved total and both be admitted against the last
// slot of the budget.
//
// It returns the reserved total the decision was actually made against, so a
// refusal can be reported with the arithmetic that produced it rather than with
// the snapshot the caller read a moment earlier — the whole reason the re-check
// exists is that the two can differ.
func (m *Monitor) admitAssayRun(key string, estimate, recorded, limit float64) (bool, float64) {
	m.assayBudgetMu.Lock()
	defer m.assayBudgetMu.Unlock()
	// A re-admission of a key already held (the same head admitted twice
	// before its run started) must not count itself twice, so its current hold
	// is excluded from the projection it is measured against.
	reserved := m.reservedAssayCostLocked() - m.assayReservations[key].estimateUSD
	if !assayBudgetAdmits(recorded, reserved, estimate, limit) {
		return false, reserved
	}
	if m.assayReservations == nil {
		m.assayReservations = make(map[string]assayReservation)
	}
	m.assayReservations[key] = assayReservation{
		estimateUSD: estimate,
		expires:     m.nowFn().Add(assayAdmissionLease),
	}
	return true, reserved
}

// HoldAssayReservation pins the in-flight estimate for a review that is
// starting, and returns the amount held. The daemon calls it at the top of
// every run, whatever admitted it.
//
// It is not a second admission: a review the operator asked for by hand
// (forge assay rerun) or one the Burnish coordination path starts is not
// refused on budget, but its spend is just as invisible to the gate until it
// finishes, so it must be counted while it runs. For a review the gate did
// admit, this replaces the admission hold with a longer-lived one keyed the
// same way — one reservation, refreshed, never two.
func (m *Monitor) HoldAssayReservation(anvil string, prNumber int, headSHA string) float64 {
	estimate := m.assayRunCostEstimate(m.assayRunCostBounds(anvil))
	m.assayBudgetMu.Lock()
	defer m.assayBudgetMu.Unlock()
	if m.assayReservations == nil {
		m.assayReservations = make(map[string]assayReservation)
	}
	key := assayRunKey(anvil, prNumber, headSHA)
	// The admission's estimate is kept when it is the larger of the two: the
	// gate admitted the review against that number, so lowering it here would
	// free budget the admission decision was made on.
	if held, ok := m.assayReservations[key]; ok && held.estimateUSD > estimate {
		estimate = held.estimateUSD
	}
	m.assayReservations[key] = assayReservation{
		estimateUSD: estimate,
		expires:     m.nowFn().Add(assayRunLease),
	}
	return estimate
}

// ReleaseAssayReservation drops a review's hold and folds what it actually cost
// into the rolling mean that estimates the next one.
//
// The daemon calls it from the same deferred close as the run's own recording,
// AFTER the assay_runs row is written. That ordering is the whole point of
// putting the release there: released first, the spend would be neither
// reserved nor recorded for the width of that window, which is precisely the
// blindness the reservation exists to remove. Releasing a key that is not held
// is a no-op, so a manual re-run, a double release or a run whose hold has
// already expired all pass through harmlessly.
func (m *Monitor) ReleaseAssayReservation(anvil string, prNumber int, headSHA string, actualCostUSD float64) {
	m.assayBudgetMu.Lock()
	defer m.assayBudgetMu.Unlock()
	delete(m.assayReservations, assayRunKey(anvil, prNumber, headSHA))
	// A run that recorded nothing (a failed diff fetch, a skipped review) is
	// not a $0 review; folding it in would drag the estimate toward zero and
	// re-open the overrun.
	if actualCostUSD <= 0 {
		return
	}
	m.avgAssayRunCostN++
	m.avgAssayRunCost += (actualCostUSD - m.avgAssayRunCost) / float64(m.avgAssayRunCostN)
}

// assayRunCostBounds is the anvil's configured floor for the estimate and the
// daily cap that bounds it from above. They are read together because a
// caller that took only one of them could size a hold the gate would then
// refuse forever (see clampAssayEstimate).
func (m *Monitor) assayRunCostBounds(anvil string) (floor, limit float64) {
	if m.assayConfig == nil {
		return 0, 0
	}
	cfg := m.assayConfig(anvil)
	return cfg.RunCostEstimateUSD, cfg.DailyCostLimitUSD
}

// assayRunCostEstimate returns what one not-yet-finished review is assumed to
// cost: the larger of the rolling mean of recorded runs and the configured
// floor.
//
// The larger of the two, rather than the mean alone, for the same reason the
// per-worker estimate behind the main daily_cost_limit takes a floor: an
// estimate that reads low re-admits the overrun, while one that reads high only
// stops reviews slightly early against a cap that is itself a safety limit. The
// floor is what covers the window in which there is no mean to take — a daemon
// that has just started and whose first review has not yet finished.
//
// The mean is seeded once per daemon lifetime from the last
// assayRunCostSamples recorded runs, so a restart does not throw away what the
// deployment already knows about its own review costs, and is folded forward
// from there by ReleaseAssayReservation. A seed query that fails is not
// retried and not fatal: the floor governs until the first run of the new
// lifetime records its cost.
func (m *Monitor) assayRunCostEstimate(floor, limit float64) float64 {
	m.seedAssayRunCostMean()
	m.assayBudgetMu.Lock()
	avg := m.avgAssayRunCost
	m.assayBudgetMu.Unlock()
	estimate := floor
	if avg > estimate {
		estimate = avg
	}
	// Bounded by the cap it will be measured against. The mean is seeded from
	// recorded history, so a deployment whose reviews have cost more than its
	// own daily cap seeds an estimate no day could ever satisfy — which would
	// refuse every review from an unspent day and, through assayUpToDate,
	// release every head to merge readiness unreviewed. The estimate stops at
	// the limit instead: one review is always admissible on a day that has
	// spent nothing, and what it actually costs is what closes the day.
	return clampAssayEstimate(estimate, limit)
}

// seedAssayRunCostMean primes the rolling mean from recorded history, once.
func (m *Monitor) seedAssayRunCostMean() {
	m.assayBudgetMu.Lock()
	if m.assayCostSeeded || m.db == nil {
		m.assayBudgetMu.Unlock()
		return
	}
	m.assayCostSeeded = true
	m.assayBudgetMu.Unlock()

	costs, err := m.db.RecentAssayRunCostsUSD(assayRunCostSamples)
	if err != nil {
		log.Printf("[bellows] Failed to seed Assay run cost estimate from recorded runs: %v", err)
		return
	}
	m.assayBudgetMu.Lock()
	defer m.assayBudgetMu.Unlock()
	for _, c := range costs {
		if c <= 0 {
			continue
		}
		m.avgAssayRunCostN++
		m.avgAssayRunCost += (c - m.avgAssayRunCost) / float64(m.avgAssayRunCostN)
	}
}
