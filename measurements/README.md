# measurements/

Versioned before/after cost measurements, so a comparison can be re-read and
re-derived later rather than surviving only as a number in a bead comment.

| What | Where |
|---|---|
| The comparison, windows, invocations and caveats | [comparison.md](comparison.md) |
| Methodology behind the figures | [../docs/assay-cost-attribution.md](../docs/assay-cost-attribution.md) |
| The tool | `forge cost assay` (`internal/cost/attribution.go`, `cmd/forge/cost.go`) |

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
