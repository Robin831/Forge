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

## Per-pass turn and spend sample (Forge-cikv, 2026-08-29)

The R2/R3 sample — the same two extractors, run again once Assay runs had
accumulated under the reading prompts. Note that the window holds **two**
regimes, because Forge-sra6's own $1.50 → $3.00 raise deployed inside it
(2026-08-28 16:32:22 +02:00); the boundaries are in
[../docs/assay-turn-budget.md](../docs/assay-turn-budget.md) and the analysis
partitions on them.

| File | What |
|---|---|
| `assay-passes-20260827T131525Z-20260829T062100Z.json` | 78 pass observations, cut at the reading-prompt boundary by the printed extractor's own filter line |
| `assay-session-costs-20260827T131525Z-20260829T062100Z.json` | 95 pass sessions; additionally carries each session's `tool_use` block names and turn count, which the printed extractor discards |

Both filenames open at the R2 boundary (`20260827T131525Z`), which is the
window the extractors were *asked* for and not the one they found — the first
run inside it is a day later, 2026-08-28 16:02:06 +02:00.

That boundary cut is not a modification: both printed extractors carry the
filter themselves (`rows = [... if r['ts'] >= '2026-08-27T15:15:25']`, and its
epoch-ms twin in the session one), so applying it is running them verbatim.
The added fields are the second file's only departure from the verbatim
extractor, and they add rather than alter — the `est`/`billed` columns are
computed by the printed code unchanged. They exist because `files=` is
structurally 0 on this deployment (every tool call is `Bash`, which names no
`file_path`), so the tool-name breakdown is the only way to tell a pass that
read the code from one that did not.

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

## Warden rule paths: repo-wide glob share (Forge-jehv, 2026-09-02)

The before/after behind the Pass 3 narrowing, plus why the bead's own 51%
baseline (munin) is not among the numbers.

| File | What |
|---|---|
| [warden-paths-narrowing-20260902.md](warden-paths-narrowing-20260902.md) | the counts, the invocation that reproduces them, and the caveats |

The tool is a skipped-by-default test rather than a script:
`FORGE_MEASURE_ANVIL=<anvil> go test ./internal/smelter -run TestMeasureRepoWidePaths -v -timeout 60m`.
It counts with the shipped `smelter.repoWideGlob`, so the number quoted for a
run is counted by the same definition the rewrite acts on.
