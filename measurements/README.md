# measurements/

Versioned before/after cost measurements, so a comparison can be re-read and
re-derived later rather than surviving only as a number in a bead comment.

| What | Where |
|---|---|
| The comparison, windows, invocations and caveats | [comparison.md](comparison.md) |
| Methodology behind the figures | [../docs/assay-cost-attribution.md](../docs/assay-cost-attribution.md) |
| The tool | `forge cost assay` (`internal/cost/attribution.go`, `cmd/forge/cost.go`) |

## Per-pass turn and spend sample (Forge-sra6, 2026-08-28)

The sample behind the `max_cost_per_pass_usd` $1.50 → $3.00 decision. Kept here
for the same reason as everything else in this directory, plus one specific to
it: its sources expire. `daemon.log` rotates at 50 MB keeping 3 compressed
backups, roughly a fortnight at current volume, and preserved pass logs are
swept at `settings.log_retention_days`. These two files are the sample; they
cannot be re-derived once the logs behind them roll off.

| File | What |
|---|---|
| `assay-passes-open-20260828T133700Z.json` | one row per pass observation off `daemon.log`'s `passes=` field — timestamp, PR, pass, `turns`, `term`, `tools`, `files` |
| `assay-session-costs-open-20260828T133700Z.json` | one row per pass **session** off the preserved `assay-*.log` files — `est` is `costTracker`'s in-flight figure reproduced from the per-message usage blocks, `billed` the provider's `total_cost_usd` |

Neither is the output of a shipped subcommand, so the exception to the
verbatim rule below is stated rather than assumed: they were produced by the
extractors printed in
[../docs/assay-turn-budget.md](../docs/assay-turn-budget.md), which is also
where the regime boundaries that partition them live. The analysis reads
those, not the logs.

## Conventions

- **Filenames encode the window**: `<phase>-<since>-<until>.<ext>`, with `open`
  for an unbounded side — `before-open-20260825T114554Z.json` is an open lower
  bound up to `2026-08-25T11:45:54Z`.
- **`.txt`/`.json`/`.csv` are raw tool output, committed verbatim.** Never
  hand-edit them: an artifact that has been touched is no longer evidence of
  what the tool reported. Re-run the invocation instead.
- **`comparison.md` and this file are the only hand-written ones.**
- **Empty is a result.** A report showing zero runs is committed as-is rather
  than omitted — an absent file reads as "not done yet", a zero-run report says
  "measured, and there was nothing there".
- **Both sides of a comparison use the same invocation** apart from the window
  flags. Changing `--anvil`, `--model-tier` or `--include-skipped` between the
  two produces two reports that cannot be compared.
