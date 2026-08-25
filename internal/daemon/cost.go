package daemon

import (
	"github.com/Robin831/Forge/internal/cost"
)

// recordStageCost persists one completed provider session's token usage into
// the three cost tables, for a stage the daemon runs itself rather than through
// the pipeline: the Crucible's schematic check and an Assay review. It is the
// daemon-side twin of the pipeline's own recordStageCost, and both go through
// cost.Record, so every stage that spends money lands in the same three tables
// with the same prompt-cache columns.
//
// stage names the caller in the failure log. A zero usage writes nothing.
func (d *Daemon) recordStageCost(stage, provName, beadID, anvil string, u cost.Usage) {
	if d.db == nil {
		return
	}
	if err := cost.Record(d.db, provName, beadID, anvil, u); err != nil {
		d.logger.Warn("cost write failed", "stage", stage, "bead", beadID, "anvil", anvil, "error", err)
	}
}

// assayCostStage is the stage tag an Assay run's spend is recorded under. It is
// a constant because it appears in the failure log and, more to the point,
// because it is what a grep for the single recording site lands on.
const assayCostStage = "assay"

// recordAssayCost folds one finished Assay run's spend into the three cost
// tables. Assay bills into assay_runs.cost_usd as its own per-run ledger, and
// that row alone is what the assay.daily_cost_limit_usd sub-budget is measured
// against (state.AssayCostUSDSince sums assay_runs, never daily_costs) — so
// without this the money a review spent never reached the main ledger, and the
// global settings.daily_cost_limit was enforced against a day's spend that
// excluded every PR review.
//
// This is where an Assay run is recorded, and the ONLY place it may be. The two
// other paths that could plausibly claim the job must not:
//
//   - the pipeline's own (*Params).recordStageCost — Assay never runs inside
//     pipeline.Run (it is dispatched by Bellows against an open PR, long after
//     the pipeline that produced the branch finished), so a stage entry added
//     there for it would be recording a second time, not a first;
//   - internal/bellows, which decides whether a review runs but never sees what
//     one cost: it hands the dispatch to the daemon and the engine's usage
//     comes back here.
//
// Duplicating the call in either would double-count into daily_costs and
// provider_daily_costs while assay_runs stayed right, which reads as a pricing
// bug rather than a bookkeeping one and is exactly the shape that survives
// review.
//
// Exactly once per run, both terminal paths included: runAssayReview calls this
// from the success/partial branch or from the failure branch, never both, and a
// failed run is recorded on the same terms as a completed one because a failure
// is not a refund — the sessions it made before it died were billed. A run that
// spent nothing (an unfetchable diff, a head with no reviewable changes) passes
// a zero usage, which cost.Record drops.
//
// Backfill: historical assay_runs rows predating this recording are NOT
// reflected in daily_costs — roughly $2,650 of spend since 2026-06-01 — and no
// backfill is performed. Doing one is an explicit, separate operation, and it
// would have to guard against re-folding rows already counted here: there is no
// marker on either side saying which assay_runs rows have reached daily_costs,
// so a backfill needs a cutoff timestamp (the first run recorded through this
// helper) rather than a re-scan of the table.
func (d *Daemon) recordAssayCost(provName, beadID, anvil string, u cost.Usage) {
	d.recordStageCost(assayCostStage, provName, beadID, anvil, u)
}
