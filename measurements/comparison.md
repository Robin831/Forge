# Assay repeat-review cost: before/after measurement

Bead: **Forge-9tlm** (measurement) under **Forge-1xuo** (re-measure repeat-review cost).
Tooling: `forge cost assay` — see [docs/assay-cost-attribution.md](../docs/assay-cost-attribution.md)
for the methodology these numbers depend on.

**Status: the BEFORE half is captured and committed. The AFTER half cannot be run
yet — the post-change window currently holds zero Assay runs.** The section
[After window: why it is empty](#after-window-why-it-is-empty) says exactly what
has to be true before the comparison can be completed, and the invocation to
complete it is already written down below so the second run is a copy-paste.

Nothing in this file is projected, extrapolated or filled in from expectation.
Empty cells are empty.

## Measurement windows

The change set this measures is the Assay cost-reduction work that landed on
`main` on 2026-08-25. Both boundaries are the commit timestamps of that set, so
the windows are defined by the change and not fitted to the data:

| Window | Bound | Absolute (UTC) | Anchor |
|---|---|---|---|
| **before** | since | (open) | all recorded history; first `assay_runs` row is `2026-07-13T12:58:07Z` |
| | until | `2026-08-25T11:45:54Z` | `d654992` — first cost-reduction commit (prompt ordering + staggered fan-out, Forge-1v68) |
| **transition** | since | `2026-08-25T11:45:54Z` | as above |
| | until | `2026-08-26T00:00:00Z` | first midnight after the last commit in the set |
| **after** | since | `2026-08-26T00:00:00Z` | as above — safely past `e59e323` (`2026-08-25T16:28:06Z`), the last commit in the set |
| | until | (open) | |

`--since` is inclusive and `--until` exclusive, so the three windows tile exactly:
no run is counted twice and none is dropped.

### Why there is a transition window

The change set did not land at one instant — it landed as ten commits spread
across `2026-08-25T11:45:54Z`–`16:28:06Z`, and the daemon self-deploys, so runs
during that span executed against a partly-updated Forge. Those 11 runs are
neither cleanly before nor cleanly after. They are reported as their own window
rather than being folded into either side or quietly discarded: 11 runs and
$80.24 that do belong to the ledger, and assigning them to a side would flatter
or damage whichever side they were put on.

## Script invocation

Identical apart from the window arguments. Run from a checkout with `~/bin/forge`
on the same host as `~/.forge/state.db`:

```bash
# BEFORE — captured 2026-08-25
forge cost assay --until 2026-08-25T11:45:54Z --format table > measurements/before-open-20260825T114554Z.txt
forge cost assay --until 2026-08-25T11:45:54Z --format json --out measurements/before-open-20260825T114554Z.json
forge cost assay --until 2026-08-25T11:45:54Z --format csv  --out measurements/before-open-20260825T114554Z.csv

# TRANSITION — captured 2026-08-25
forge cost assay --since 2026-08-25T11:45:54Z --until 2026-08-26T00:00:00Z --format table > measurements/transition-20260825T114554Z-20260826.txt
forge cost assay --since 2026-08-25T11:45:54Z --until 2026-08-26T00:00:00Z --format json --out measurements/transition-20260825T114554Z-20260826.json

# AFTER — captured 2026-08-25, EMPTY. Re-run this verbatim once runs have accumulated.
forge cost assay --since 2026-08-26 --format table > measurements/after-20260826-open.txt
forge cost assay --since 2026-08-26 --format json --out measurements/after-20260826-open.json
```

Every flag not passed is left at its default deliberately, and the defaults are
part of the methodology: skipped runs excluded (matching the per-PR run cap),
sonnet pricing rows, all anvils. The second run must not add `--anvil`,
`--include-skipped` or `--model-tier` — any of them makes the two reports
incomparable.

## Artifacts

| File | Window | Runs |
|---|---|---|
| `before-open-20260825T114554Z.{txt,json,csv}` | before | 76 |
| `transition-20260825T114554Z-20260826.{txt,json}` | transition | 11 |
| `after-20260826-open.{txt,json}` | after | 0 |

Raw tool output, committed verbatim and not hand-edited. Only this file and
`README.md` are written by hand.

## Comparison

Normalised metrics are the headline; raw totals are supporting context only,
because the windows do not hold comparable run volumes and a totals-only
comparison would read a traffic change as a cost change.

| Metric | before | transition | after | delta | % delta |
|---|---|---|---|---|---|
| **Cost per run** | **$7.0190** | $7.2947 | — | — | — |
| **Cost per repeat run** | **$7.0071** | $7.4270 | — | — | — |
| Cost per first run | $7.0277 | $7.1844 | — | — | — |
| Repeat share of recorded spend | 42.0% | 46.3% | — | — | — |
| `cache_read` share of cache tokens | unknown | unknown | — | — | — |
| Reviews per PR | 1.727 | 1.833 | — | — | — |
| *Total recorded spend* | *$533.44* | *$80.24* | *$0.00* | — | — |
| *Runs / PRs* | *76 / 44* | *11 / 6* | *0 / 0* | — | — |

Supporting detail for the before window:

| | runs | PRs | recorded $ | share | $/run | zero-finding runs | zero-finding $ |
|---|---|---|---|---|---|---|---|
| first_run | 44 | 44 | 309.22 | 58.0% | 7.0277 | 8 | 26.27 |
| repeat_run | 32 | 31 | 224.23 | 42.0% | 7.0071 | 10 | 48.67 |
| **total** | **76** | **44** | **533.44** | 100% | **7.0190** | **18** | **74.93** |

Run-ordinal distribution (before): ordinal 1 — 44 runs / $309.22; ordinal 2 —
31 runs / $218.02; ordinal 3 — 1 run / $6.20; nothing at ordinal 4 or beyond.
Re-review depth on this instance is shallow: a PR reviewed a second time is the
common repeat case and a third review is a single run in the whole history.

## After window: why it is empty

The `after` report is committed showing 0 runs. That is the measured state, not
a missing file. Two independent things are true, and both have to change before
the after half means anything:

1. **No Assay run exists after the change set landed.** The newest row in
   `assay_runs` started `2026-08-25T14:16:20Z`; the last commit in the set is
   `2026-08-25T16:28:06Z`. The whole table is 87 rows spanning `2026-07-13` to
   `2026-08-25`, so at the observed rate (~76 runs over six weeks) a run volume
   comparable to the before window's 76 is a matter of weeks, not hours.

2. **No row carries cache accounting.** All 87 rows report
   `cache_creation_tokens = 0` and `cache_read_tokens = 0`, so every one is
   classified `unknown` — 100% of before-window spend is unattributable by token
   class. This is correct behaviour, not a bug: the instrumentation that
   populates those columns (`41a198e`, Forge-wvb6/`#858`) landed at
   `2026-08-25T15:05:46Z`, after the last run started. The
   `cache_creation` vs `cache_read` split the bead asks for is therefore
   **structurally unavailable for the before window and can never be
   backfilled** — the tokens were never recorded.

The consequence for the comparison is worth stating plainly rather than
discovering later: **the cache-token-class axis will be a one-sided measurement.**
The after window will have it; the before window cannot. Any before/after
statement about cache-write vs cache-read must be reported as
"post-change absolute figures, no pre-change comparator", never as a delta
against an assumed pre-change zero. Zero on those columns means *not knowable*,
and treating it as a real zero would manufacture an infinite improvement out of
missing instrumentation.

The recorded-spend axis (cost per run, cost per repeat run, repeat share) is
unaffected — `cost_usd` has been recorded for every row since `2026-07-13` — so
that half of the comparison will be genuinely two-sided.

## Completing the measurement

1. Wait until `forge cost assay --since 2026-08-26` reports a run count
   approaching the before window's 76. Fewer than ~30 runs is not worth reading:
   at n runs a single outlier moves the mean by roughly its own size over n, and
   the before window's own per-run spread is wide (individual runs $3.13–$12.26).
2. Re-run the three AFTER invocations above **verbatim**, overwriting
   `after-20260826-open.{txt,json}`.
3. Fill the `after`, `delta` and `% delta` columns in the table above from the
   JSON, and confirm `cache_creation`/`cache_read` are non-zero in the after
   output before quoting any token-class figure. If they are still zero, say so
   and do not report a token-class split.
4. Re-check the caveats below and record the result on Forge-1xuo.

## Caveats

Stated up front rather than left for a reader to find:

- **The after window is empty.** No conclusion about whether the change set
  reduced spend can be drawn from this file today. It is a baseline, not a
  result.
- **The token-class split is one-sided by construction** — see above. It cannot
  be backfilled.
- **Small sample.** 76 before-window runs over 44 PRs, and re-review depth
  reaching ordinal 3 exactly once. Per-run means over samples this size move
  under single large runs.
- **Workload mix is not controlled.** Both windows are whatever PRs Forge
  happened to review. A window heavy with large-diff PRs costs more per run for
  reasons that have nothing to do with the change set. There is no normalisation
  for diff size here because `assay_runs` records none.
- **The transition window is real spend that belongs to neither side** and is
  excluded from both. It is reported separately so the ledger still adds up:
  76 + 11 + 0 = 87 rows = the whole table.
- **The published $2,326.54 / 780-repeat-run baseline does not reproduce here**
  and no attempt was made to make it. That is a different dataset —
  `docs/assay-cost-attribution.md` documents the evidence. The before figures in
  this file supersede it as the comparator for this instance.
- **Skipped runs are excluded** from every window (the tool's default). The
  skip-empty-review change (`e7cf9dc`, Forge-oei0) creates rows carrying
  `skipped_reason`, so post-change it will convert some runs that would have
  cost money into rows costing nothing and counted nowhere. This is the right
  default — it keeps ordinals honest — but it means the after window's *run
  count* will understate the traffic Assay saw. Read the after figures as cost
  per *dispatched* review, and use `--include-skipped` as a secondary cut if the
  number of skips matters.
