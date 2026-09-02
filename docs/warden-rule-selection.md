# Warden rule selection: what the review-time cap was really doing

`settings.warden.max_rules_per_review` (default 30) decides which learned rules
reach the Warden's checklist. Until Forge-n9aq it did so with `rules[:30]` —
plain truncation in file order, with no ranking, recency preference or sampling.

Rules are **appended** to `.forge/warden-rules.yaml` as they are learned, so file
order is age order, and the cap therefore returned the file's oldest surviving
rules and only ever those. The failure is invisible in any single review: a
checklist of 30 rules reads exactly the same whether 30 candidates survived the
filters or 431 did. It is only visible in aggregate, across many real PRs.

## Method

For each merged PR of an anvil, take the file list and the diff the merge
actually produced, run the review-time selection over the anvil's own rules
file, and union the emitted rule IDs across every PR. The result is the set of
rules that **can** fire at all.

The PR corpus is derived from the repository itself — squash merges on `main`
carry the PR number in the subject line, so each first-parent commit matching
`(#N)$` is one PR:

```bash
git log --first-parent --format='%H %s' origin/main | grep -E '\(#[0-9]+\)$'
git show --name-only --format= <sha>     # the PR's changed files
git show --format= --unified=3 <sha>     # the diff the review would have read
```

A synthetic single-file diff proves nothing here: it emits a plausible-looking
30 rules whether or not the bug is present. Only the union across a real PR
history separates "30 rules were chosen for this diff" from "the same 30 rules
are chosen for every diff".

## Measured

Two anvils, each against every squash-merged PR in its own history, with the
shipped filter configuration (all three filters on, `max_rules_per_review: 30`):

| Anvil | Rules on file | PRs | Reachable BEFORE | Reachable AFTER |
|---|---|---|---|---|
| Forge | 727 | 841 | **125** (17.2%) | **369** (50.8%) |
| Hytte | 2295 | 841 | **119** (5.2%) | **556** (24.2%) |

By the month a rule was learned:

| Anvil | | 2026-03 | 2026-04 | 2026-05 | 2026-06 | 2026-07 | 2026-08 |
|---|---|---|---|---|---|---|---|
| Forge | before | 115 | 3 | 3 | 0 | 0 | 4 |
| Forge | after | 93 | 64 | 129 | 21 | 57 | 5 |
| Hytte | before | 105 | 9 | 3 | 0 | 0 | 1 |
| Hytte | after | 44 | 135 | 301 | 40 | 32 | 3 |

Rules learned after 2026-05 that could reach any review at all: Forge 4 → 83,
Hytte 1 → 75. On both anvils the learner had been writing into a file where only
the head was read.

The originating report measured the same thing on a third anvil (munin, 1793
rules, 615 PRs): 61 rules reachable, 1732 that could never fire, and nothing
learned since May 2026 reachable at all. That checkout is not present on this
host, so the numbers above are the reproduction — same failure, same shape, two
independent rules files, one of them larger than munin's.

Path narrowing was tested first and is **not** the fix: rewriting 1785 of
munin's 1793 rules to the areas their own source PRs touched — cutting rules
with three or more repo-wide `**/*` globs from 917 to 8 — changed zero of the 30
emitted rules for every PR shape tested. The oldest rules are broad enough to
survive any narrowing that still matches the PR that taught them, so they keep
the head of the file.

## What replaced it

See "How the review-time set is chosen" in [configuration.md](configuration.md)
for the ranking (specificity, pattern relevance, recency), the two-word pattern
threshold, the per-review funnel log line, and the `max_rules_in_file` ceiling.

The one property worth restating here is the guard: the ordering is total and
content-derived down to its last tiebreak, so the emitted checklist is identical
however the rules file is ordered. `TestSelectionIsIndependentOfFileOrder`
shuffles the file and compares the emitted set **and its order**; under the old
truncation that comparison changed completely.
