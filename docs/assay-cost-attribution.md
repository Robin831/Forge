# Assay repeat-review cost attribution

`forge cost assay` answers one question repeatably: of what Assay spent over a
window, how much went on reviewing a PR for the **first** time and how much on
reviewing it **again** — and how much of that spend is prompt-cache traffic,
split into the two classes that are priced differently.

It exists so that a before/after comparison across a cost-reduction change is
apples-to-apples: the same query, the same definitions, run twice.

| Piece | Where |
|---|---|
| Analysis module | `internal/cost/attribution.go` (beside the pricing table it prices with) |
| Data source | `state.DB.AssayRunHistoryForWindow` → the `assay_runs` table |
| CLI | `cmd/forge/cost.go` — `forge cost assay` |

```bash
forge cost assay                                          # everything, human-readable
forge cost assay --since 2026-06-01 --until 2026-07-01    # one window
forge cost assay --format json --out before.json          # machine-readable snapshot
forge cost assay --format csv  --out before.csv
forge cost assay --anvil forge --model-tier opus
forge cost assay --expect-repeat-cost 2326.54 --expect-repeat-runs 780
```

`--since` is inclusive, `--until` exclusive, both accept `YYYY-MM-DD` (UTC
midnight) or RFC3339, and both are optional — an omitted bound is open on that
side, so consecutive windows tile without a run being counted in two of them.

## Methodology

These are the choices the numbers depend on. A second report that changes any of
them is not comparable with the first, which is why each is echoed in the output.

**Recorded spend and priced attribution are never summed.** A report carries two
different kinds of dollar figure:

- **Recorded** (`cost_usd`, the `RECORDED $` column) is what the provider
  reported for the run. It is the authoritative total and the figure the
  first-vs-repeat split is computed from.
- **Priced** (the `cache_creation` / `cache_read` token classes) is tokens × that
  class's own rate from `internal/cost`'s pricing table. It explains the cache
  *component* of recorded spend and is a strict subset of it — `assay_runs`
  persists cache tokens but not plain input/output tokens, so the two cache
  classes can never add up to the recorded total.

A cache write and a cache read differ by more than a factor of ten ($3.75/M vs
$0.30/M at Sonnet rates), so they are summed and priced separately; collapsing
them into one input rate would misattribute the bulk of the traffic.

**Run ordinals are derived over each PR's full history, then restricted to the
window.** Ordinal 1 is a PR's first review, n>1 a re-review. The query returns
every run of every PR that has a run in the window — including runs from before
it — precisely so an ordinal is never window-local. Deriving over the window
alone is wrong in the one direction that flatters the answer: a PR first reviewed
the day before the window opens would have its second review counted as a first,
moving repeat spend into the first-run column. The table reports how many
earlier runs it read for this purpose.

A PR is keyed by **anvil + PR number**, because PR numbers are per-repository:
without the anvil, PR #12 in two repos would be one PR with twice the reviews.

Timestamp ties break on run id, so two runs stamped in the same millisecond
always order the same way. An ordinal that depends on the storage layer's row
order is not repeatable, and repeatability is the point.

**Skipped runs are excluded by default.** A run carrying a `skipped_reason`
dispatched no passes and reviewed no code. This matches the definition of "a run"
the per-PR run cap already uses (`state.CountAssayRuns` excludes such a row), and
it matters for more than the count: counting a skip would push a PR's genuine
second review to ordinal 3. `--include-skipped` keeps them; the default report
says how many it dropped.

**Absent cache accounting is reported as `unknown`, not as zero.**
`assay_runs.cache_creation_tokens` / `cache_read_tokens` read zero on three kinds
of row that cannot be told apart: one written before the columns existed, one
behind a backend that reports no cache accounting, and a run that genuinely
shared nothing. A row with both at zero is therefore classified `unknown`. Its
recorded cost still lands in every total — the money is never lost — but it is
reported as unattributable rather than as a confident zero, and the report says
what share of the window that is. This is the graceful degradation older rows
need: the report never fails on them.

**The zero-finding tail is counted alongside.** A repeat review that reports
nothing is the spend most likely to have bought nothing, so `ZERO-FIND RUNS` /
`ZERO-FIND $` are broken out per group rather than left to a follow-up query.

## Baseline reconciliation

The tool was specified against a published baseline of **$2,326.54 across 780
repeat runs**. That figure **does not reproduce against this repository's
`assay_runs` data**, and the difference is a difference of dataset, not of
method. Measured on the Forge host on 2026-08-25 over an unbounded window:

| | Published baseline | `forge cost assay` |
|---|---|---|
| Repeat runs | 780 | 37 |
| Repeat-run spend | $2,326.54 | $261.36 |
| First-run spend | — | $352.32 (50 runs) |
| Total recorded spend | — | $613.68 (87 runs, 50 PRs) |

The evidence that this is a coverage gap rather than a maths error:

- `assay_runs` on this host holds **87 rows in total** — first row
  `2026-07-13`, last `2026-08-25`. 780 repeat runs cannot be drawn from 87 rows
  however they are grouped, so no ordinal, filter or pricing choice closes the
  gap.
- The `events` log reaches back to `2026-03-30`, but its earliest `assay_*` event
  is `2026-08-10`. Assay run recording is simply younger than the spend the
  baseline covers.
- The whole-instance spend across every stage over the same period is $3,609.82
  (`daily_costs`), of which Assay's recorded $613.68 is a part. The published
  figure sits between the two, consistent with its having been derived from a
  different instance, a wider set of stages, or provider-side billing data rather
  than from `assay_runs`.

**No maths was adjusted to close this delta.** Bending the report to hit a number
whose source it cannot see would produce a tool that reproduces one figure and is
wrong about every subsequent one. What the tool guarantees instead is that the
*method* is fixed and stated, so before/after comparisons taken with it are
comparable with each other.

Practical consequence for a before/after measurement: take the "before" snapshot
**with this tool**, over the window being changed, and compare it against an
"after" snapshot taken the same way. Do not compare an "after" number against the
$2,326.54 baseline — they are not measurements of the same population.

```bash
# before the change lands
forge cost assay --since 2026-08-01 --until 2026-09-01 --format json --out before.json
# after it has been live for a comparable period
forge cost assay --since 2026-09-01 --until 2026-10-01 --format json --out after.json
```

`--expect-repeat-cost` / `--expect-repeat-runs` print the reconciliation shown
above and never fail the command: a mismatch can mean the methodology drifted or
that the expected figure came from a different dataset, and only the operator
knows which. `cost.ValidateBaseline` is the same check for a caller in Go.

## A caveat on the cache-token split, today

Every one of the 87 rows currently in `assay_runs` on this host reports zero
cache tokens, so today's report attributes **100% of recorded spend to the
`unknown` token class**. `assay_runs.cache_creation_tokens` /
`cache_read_tokens` were added with Assay's prompt-cache telemetry; rows written
before that carry the column default. The split becomes informative as runs
recorded after the instrumentation accumulate — the report is built to say so
plainly in the meantime rather than to present a zero that reads as "no cache
traffic".

## Reuse

`cost.DeriveRunOrdinals` is exported as the single definition of "the nth review
of this PR". Anything else asking that question — a zero-finding-tail
investigation, a re-review trigger gate — should read it from there rather than
deriving a second answer that can disagree.
