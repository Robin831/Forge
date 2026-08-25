# Assay zero-finding run analysis

Bead: **Forge-39kt** under **Forge-1xuo** (re-measure repeat-review cost and
investigate the zero-finding tail).

This is analysis, not a feature. It adds `forge cost zero-findings` and changes
no Assay behaviour: nothing here skips a review, short-circuits a pass, caches a
verdict or gates a dispatch. Its whole purpose is to size one cell before anybody
builds a heuristic to exploit it.

**Bottom line: the cell is empty. The proposed skip/short-circuit behaviour
should be dropped.** The evidence is below.

## The question

Assay runs that report **zero findings** are the spend most likely to have bought
nothing. But "found nothing" is not the same as "wasted": the only way to learn a
diff is clean is to read it. What would be waste is the *nth* clean look at a PR
whose reviewable content has not moved since the last look — and that is the only
population a skip heuristic could ever recover.

So every zero-finding run is classified on two axes and lands in exactly one cell:

| Cell | Meaning | Recoverable by a skip? |
|---|---|---|
| `first_or_no_prior` | ordinal 1 — the PR's first review | **No.** Reading the diff is how you learn it is clean. |
| `repeat_clean_unchanged` | ordinal > 1, same head commit as the previous run | **Yes** — this is the target. |
| `repeat_clean_changed` | ordinal > 1, head moved since the previous run | **No.** New content, reviewed once. |
| `repeat_clean_unknown` | ordinal > 1, a head SHA is missing | **Unknowable** — reported as a gap, never inferred. |

## Result

Measured on the Forge host on 2026-08-25 over an unbounded window
(`forge cost zero-findings --details`; raw output committed as
[`measurements/zero-findings-open-20260825T173427Z.txt`](../measurements/zero-findings-open-20260825T173427Z.txt),
with `.json` and `.csv` beside it):

```
runs in window    87 ($613.68 recorded)
zero-finding      20 run(s) over 19 PR(s) ($84.03, 13.7% of window spend)
```

| Cell | Runs | PRs | Cost | % runs | % zero-finding $ | % all Assay $ | of which no coverage |
|---|---|---|---|---|---|---|---|
| `first_or_no_prior` | 9 | 9 | $29.40 | 45.0% | 35.0% | 4.8% | 2 ($3.13) |
| **`repeat_clean_unchanged`** | **1** | **1** | **$0.00** | **5.0%** | **0.0%** | **0.0%** | **1 ($0.00)** |
| `repeat_clean_changed` | 10 | 10 | $54.63 | 50.0% | 65.0% | 8.9% | 1 ($0.00) |
| `repeat_clean_unknown` | 0 | 0 | $0.00 | 0.0% | 0.0% | 0.0% | 0 ($0.00) |
| **TOTAL** | **20** | **19** | **$84.03** | 100% | 100% | 13.7% | 4 ($3.13) |

**Headline: repeat clean reviews of unchanged substance are 1 run and $0.00 —
0.0% of zero-finding spend and 0.0% of all Assay spend.**

And that single run does not survive inspection. It is `forge#837` run 62, and
both it and its predecessor (run 61, same head `2703506d`) are `status=failed`,
rate-limited at triage, zero passes completed, $0.00 each. It reports zero
findings because **no pass ever ran**, not because a review read the diff and
found nothing. There was no review to skip.

**Excluding runs that never reviewed anything, the recoverable cell is 0 runs and
$0.00.**

## Where the zero-finding spend actually goes

The $84.03 is not idle re-reviewing. It splits into two things a skip cannot touch:

- **$29.40 (35%) first reviews.** Nine PRs whose first Assay review found
  nothing. This is Assay working: the diff was clean and it took a review to
  establish that.
- **$54.63 (65%) repeats over a moved head.** Ten runs, and the `PREV HAD
  FINDINGS` column says **all ten** followed a run that *did* report findings
  (7, 6, 6, 4, 4, 3, 2, 1, 1 findings). That is the fix→re-review loop closing
  correctly: Assay flagged something, the author pushed a fix, the re-review
  read the new head and confirmed it clean. Skipping those is not saving money,
  it is removing the confirmation the loop exists to produce.

Not one zero-finding repeat in the dataset followed a *clean* review of the same
content. The `PREV CLEAN` sub-column — which is what a skip heuristic would
actually key on — is 1 run, $0.00, and that one is the failed pair above.

## Baseline reconciliation: 208 runs / $429.95 does not reproduce

The bead specifies "the 208 zero-finding runs ($429.95)". Measured here:

| | Published figure | `forge cost zero-findings` |
|---|---|---|
| Zero-finding runs | 208 | 20 |
| Zero-finding spend | $429.95 | $84.03 |
| Delta | | −188 runs, −$345.92 |

**No filter was adjusted to close this gap**, per the instruction the analysis
was given. The gap is a difference of dataset, not of method, and it is the same
one [docs/assay-cost-attribution.md](assay-cost-attribution.md) already documents
for the $2,326.54 / 780-repeat-run baseline: `assay_runs` on this host holds **87
rows in total**, first `2026-07-13`, last `2026-08-25`. 208 zero-finding runs
cannot be drawn from 87 rows however they are grouped. Assay run recording is
simply younger than the spend those figures cover.

`--expect-runs` / `--expect-cost` print this reconciliation and never fail the
command, for the same reason `forge cost assay`'s does: a mismatch can mean the
methodology drifted or that the expected figure came from another dataset, and
only the operator knows which.

**This weakens the sample, not the conclusion.** 20 runs is a small population,
but the finding is not "the cell is small" — it is "the cell is *empty*, and the
one row in it is a rate-limited failure". Even taking the published 208/$429.95
proportions at face value, nothing in this data suggests the unchanged-repeat
cell is anything other than negligible, because Assay's own re-dispatch gate
(`LastReviewedSHA`) *already* prevents the case: it does not re-review a head it
has reviewed. A second run over one head SHA is close to impossible by
construction, which is exactly what the numbers show.

## Recommendation

**Drop the skip/short-circuit behavioural change.** The parent bead flagged it as
the weakest item and made it conditional on this evidence. The evidence is in:

1. The cell it would target is **0 runs / $0.00** after excluding failed runs.
2. The mechanism that would make it non-empty — a re-review of an unchanged head
   — is already prevented upstream by `LastReviewedSHA`. Building a second gate
   for a case a first gate already closes adds a way to skip a review that should
   have happened, in exchange for nothing.
3. Two thirds of zero-finding spend is the fix→re-review confirmation loop
   (`repeat_clean_changed`, every one following a run with findings). Skipping
   those would remove the "your fix is clean" signal, which is not waste.

If zero-finding spend is worth attacking, the larger and more tractable target is
the **first-review** cell ($29.40, 35%) — cheaper triage or earlier `shouldSkip`
scoping, not a repeat-detection cache.

## Methodology

The analysis lives in [`internal/cost/zero_finding_analysis.go`](../internal/cost/zero_finding_analysis.go),
beside the attribution report it shares its inputs with.

- **Ordinals** come from `cost.DeriveRunOrdinals` — the single exported
  definition of "the nth review of this PR", derived over each PR's **full**
  history before the window filter is applied. Re-deriving it here would produce
  a second answer that can disagree with `forge cost assay`.
- **The predecessor** of a run is the immediately preceding run of the same PR in
  that same full-history ordering, so a run whose predecessor sits just outside
  the window is classified against its real predecessor rather than promoted to
  "first". The detail rows record whether the predecessor was in-window.
- **Eligibility** (skipped-run exclusion, anvil filter) is `cost.eligibleRuns`,
  shared with `BuildReport`, so the two reports cannot assign one run two
  different ordinals.
- **PR identity** is anvil + PR number: PR numbers are per-repository.

### The substance axis is `head_sha`, and that is all there is

An `assay_runs` row records the head commit it reviewed and **nothing else about
what it read**: no diff hash, no changed-file set, no diff byte count, no base
SHA. So the substance question is answered by comparing head SHAs, as a tri-state
rather than a bool, and nothing is inferred from timestamps or cost:

- `unchanged` — same head commit. The confident direction, and the same key
  Assay's own re-dispatch gate uses.
- `changed` — different head commit. A **proxy, not a proof**: a push touching
  only a lockfile moves the head while leaving the reviewable diff identical
  after the generated-file filter. It errs towards *not* claiming recoverable
  spend, which is the safe direction for a build/don't-build decision.
- `unknown` — a head SHA is absent on one side. Reported as a gap; its spend
  still counts in every total, so the money is never lost to the ambiguity.

**The one caveat the data cannot close:** a run's diff is taken against the PR's
base, and the base is not recorded. Two runs over one head SHA whose base moved
between them read different diffs while looking identical here. That direction
**over-states** the recoverable cell — which only strengthens a conclusion that
the cell is empty.

### Zero findings is not the same as zero coverage

A run that dies before any pass runs reports `findings_count = 0` exactly as a
clean review does. `RunRecord.NoCoverage` separates the two — a persisted
`failed` status, or (for rows written before that column existed) an error with
no recorded spend. Such a run is **still classified and still counted**, so the
population reconciles, but it is annotated in its own column and netted out of
the headline: a heuristic that skips a re-review cannot recover spend on a run
that never reviewed. Four of the 20 zero-finding runs ($3.13) are in this
category, including the only member of the recoverable cell.

## Reproducing

```bash
forge cost zero-findings                       # whole recorded history
forge cost zero-findings --details             # plus the rows behind each cell
forge cost zero-findings --since 2026-08-01 --format json --out zf.json
forge cost zero-findings --expect-runs 208 --expect-cost 429.95
```
