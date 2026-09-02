# Warden rule paths: repo-wide glob share before and after Pass 3 narrowing

Forge-jehv. Measured 2026-09-02.

## What is counted

The share of a rules file's rules carrying **three or more repo-wide globs** — a
rule that names a language three times over and a location not at all, so the
path filter it carries excludes almost no diff.

"Repo-wide" is `smelter.repoWideGlob`: `**/*`, `**` and the bare `**/*.ext`. It
is counted by the shipped predicate rather than by a grep, because a script with
its own idea of the term would report a before/after about something else. The
counting and the run are one test:

```
FORGE_MEASURE_ANVIL=/path/to/anvil \
  go test ./internal/smelter -run TestMeasureRepoWidePaths -v -timeout 60m
```

It loads `<anvil>/.forge/warden-rules.yaml`, counts, runs Pass 3 in memory
against the real pull requests behind the rules (one `gh api` call per distinct
source PR), and counts again. It writes nothing.

## Results

`forge` — `/home/robin/source/Forge`, 727 rules, 258 distinct source PRs:

| rules carrying | before | after |
|---|---|---|
| >= 1 repo-wide glob | 726 (99.9%) | 726 (99.9%) |
| >= 2 repo-wide globs | 649 (89.3%) | 595 (81.8%) |
| **>= 3 repo-wide globs** | **88 (12.1%)** | **69 (9.5%)** |

Pass 3 narrowed 60 rules and filled none (the file has no rule with empty
paths). Run took 138s.

`hytte` — `/home/robin/Hytte`, 2295 rules, 529 distinct source PRs:

| rules carrying | before | after |
|---|---|---|
| >= 1 repo-wide glob | 2295 (100.0%) | 2295 (100.0%) |
| >= 2 repo-wide globs | 2252 (98.1%) | 2074 (90.4%) |
| **>= 3 repo-wide globs** | **1551 (67.6%)** | **869 (37.9%)** |

Pass 3 narrowed 750 rules and filled none. Run took 297s.

## Caveats

**The bead's baseline is munin's, and munin is not an anvil on this host.** The
51% quoted for it does not reproduce here and was not made to: `forge`'s own
file is at 12.1% and `hytte`'s at 67.6%, so the figure is a property of a
particular file's history, not a constant — `hytte`, a multi-language repository
whose rules routinely carry `**/*.md`, `**/*.go`, `**/*.tsx` and `**/*.json`
together, is the closest of the two to the shape munin's 51% describes.
The numbers above are what this host can count, and the invocation is recorded so the same measurement can be
run against munin's checkout by whoever has one — the command is the same, only
`FORGE_MEASURE_ANVIL` changes.

**The ceiling on what narrowing can do is the derivation, not the predicate.**
`globsForRule` only produces a narrower set when the rule's own text names a
language (`languageSignals`) whose glob the source PR corroborates; for a rule
whose text names none, the candidate is the PR's own extension set, which is
generally what the rule already carries. That is why 60 of `forge`'s 649
eligible rules moved rather than most of them: the other 589 re-derived the set
they already had, which `isStrictlyNarrower` declines as equal. `hytte` moves
far more (750 rules, and the headline from 67.6% to 37.9%) because its rules are
mostly about Go or React and its PRs mostly touch four extensions at once, so
the language inference has something to subtract on nearly every one. Widening the
signal table is the lever on that number, and it is a separate change.

**Only the >= 3 row is the bead's metric.** The >= 1 and >= 2 rows are reported
beside it because a headline that moved while the neighbouring rows did not is
worth being able to see: >= 1 is unmoved by construction here, since narrowing a
Go rule to `**/*.go` leaves it carrying one repo-wide glob.
