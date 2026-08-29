# Assay: where `max_turns_per_pass` comes from

The audit trail behind `assay.max_turns_per_pass` and the engine default
(`assayMaxTurns`, `internal/assay/passes.go`). Written 2026-08-25 under bead
Forge-y0u2, which asked for the budget to be re-tuned from the per-pass turn
counts Assay had started logging.

The short version: the logged figure was not in the budget's unit, and once it
is, the distribution turns out to be censored by the very cap it was meant to
justify. The default moved **12 → 16**, and the per-anvil override — which
already existed — is the right tool for a repo that needs more.

## The sample

Every Assay pass session preserved under `~/.forge/logs/<beadID>/assay-*.log`
on the robinedvardsmith.com deployment, 2026-08-03 to 2026-08-25: **451
sessions** (triage plus the five deep passes) across 3 anvils, all run under a
12-turn cap (nothing in that deployment's config sets `max_turns_per_pass`, so
every session got the engine default).

A session is one model invocation. Sessions are counted by splitting each log
at its `result` events, since a strict-JSON re-prompt or a turn-budget retry can
append a second session to the same file.

## Finding 1 — the logged `turns=` was in the wrong unit

`PassReport.Turns` carried the provider's `num_turns` and the daemon's `Assay
review completed` line rendered it as `turns=N` beside a budget written in a
different unit:

- `--max-turns N` caps the number of **model messages** the session may
  produce. This is exact, not inferred: all 32 capped sessions in the sample
  have precisely 12 distinct assistant message ids, and no session of the 451
  has more.
- Claude's `num_turns` on a session that answered is the number of
  **tool-result rounds** plus one (`num_turns == tool_uses + 1` in 161 of the
  162 successful sessions where the two figures disagreed at all). A message
  issuing three parallel tool calls is one turn against the budget and three
  rounds in `num_turns`.
- Claude's `num_turns` on a session the budget killed is the constant **cap+1**
   — 13 for every one of the 32 — whatever that session actually did.

So the reported figure overstated 38% of successful sessions, by a median of 2
and by up to 7, and said nothing at all about the capped ones. Read literally it
claimed sessions using 17 and 19 turns of a 12-turn budget.

`turnCounter` (`internal/assay/turns.go`) now counts the distinct model messages
off the stream and that is what `Turns` carries, on both the success and the
failure path. A backend whose messages carry no ids (Gemini's deltas) still
reports the provider's own figure — a field that cannot be derived must degrade
to the old number, never to zero.

**All numbers below are in the corrected unit** (model messages), recomputed
from the raw session logs. The `num_turns` columns are shown only to document
the gap.

## Finding 2 — the distribution is censored at the cap

Model messages per session, all sessions in the sample:

| turns | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| sessions | 173 | 33 | 35 | 38 | 33 | 25 | 23 | 19 | 11 | 14 | 8 | **39** |
| of which converged | 173 | 33 | 35 | 38 | 33 | 25 | 23 | 19 | 11 | 14 | 8 | **7** |

The count decays smoothly from 4 to 11 and then spikes to nearly 5× its
neighbour at exactly 12 — the cap — where 32 of the 39 sessions died. That spike
is not demand for 12 turns; it is every session that wanted more, piled against
the ceiling.

Which is why the percentiles the bead asked for cannot answer the question they
were asked to answer:

| anvil | n | p50 | p90 | p95 | p99 | max | hit the cap | (`num_turns` p95 / p99 / max) |
|---|---|---|---|---|---|---|---|---|
| forge | 343 | 3 | 11 | 12 | 12 | 12 | 28 (8.2%) | 13 / 16.6 / 19 |
| hytte | 84 | 4 | 8.7 | 11.7 | 12 | 12 | 4 (4.8%) | 13 / 14.2 / 15 |
| ext-* | 24 | 2 | 5 | 5.9 | 7.5 | 8 | 0 | 7.9 / 10.3 / 11 |
| **all** | **451** | **3** | **11** | **12** | **12** | **12** | **32 (7.1%)** | 13 / 16 / 19 |
| tool-using only | 278 | 5.5 | 12 | 12 | 12 | 12 | 32 (11.5%) | 14.1 / 17 / 19 |

p95 and p99 are both 12 on two of the three anvils, and 12 is the cap. They are
artefacts of the clip, not evidence about the budget — and the true tail beyond
12 is simply unobserved. Note also how the tempting reading of the *logged*
figure ("p99 is 16, so raise to 16-17") gets close to the right answer for
entirely the wrong reason: those are tool rounds, not turns.

## The decision

**Engine default 12 → 16.** The argument is the shape of the distribution, not
a percentile of it: 16 clears the censoring point by four turns, which covers
the sessions that were a couple of reads short, without pretending to know how
far the true tail runs.

Raising it is close to free where it does not bind. A turn budget is a clip
point, not an allowance — a pass that answers in 5 turns costs exactly what it
did before, and 69% of sessions in the sample answered in 5 or fewer. Where it
*does* bind, what it replaces is more expensive than what it buys: a clipped
pass pays for its full 12 turns **and** a retry session (`buildRetryMods`), and
still reports partial coverage when the retry misses too. 7.1% of all sessions
and 11.5% of tool-using ones were paying that, across 11 distinct beads.

The runaway that a tight turn cap used to stand in for is now bounded in the
unit that actually matters, by `assay.max_cost_per_pass_usd` — a session looping
on tool calls is stopped on spend rather than on a turn count that also clips
honest work.

**No change to the override mechanism.** The bead asked for per-anvil
`max_turns_per_pass` resolved in the loader with a global fallback; that landed
in 938ef8c and is live. `AssayConfig.MaxTurnsPerPass` is a `*int` (so unset is
distinguishable from 0), `Config.ResolvedAssay(anvil)` overlays it, and
`FromAssayConfig` → `passTurnBudget` is the single site that reads it. This bead
added the table-driven coverage for that resolution and a validation error for a
negative value, which the engine had been silently ignoring.

The bead behind this note named `munin` as the repo whose starved passes forced
the budget to be made configurable in the first place (Munin PR #4763, three
deep passes dying at the cap on a +190/-9 diff). Nothing is stamped for it here:
`munin` is not an anvil on this deployment and Forge ships no `config.yaml` —
the file is operator-written. The override for it belongs in that operator's
config, as:

```yaml
anvils:
  munin:
    assay:
      max_turns_per_pass: 30
```

## Re-deriving this

The default should be revisited once there is data from *above* the censoring
point — with the counter fix, sessions converging at 13-16 are now visible as
such rather than clipped. Roughly:

```bash
# per-session model-message counts, from the preserved pass logs
python3 - <<'PY'
import json, glob, os, collections
rows = []
for f in glob.glob(os.path.expanduser('~/.forge/logs/*/assay-*.log')):
    msgs, tools = set(), 0
    for ln in open(f, errors='replace'):
        if not ln.startswith('{'):
            continue
        try: o = json.loads(ln)
        except Exception: continue
        if o.get('type') == 'assistant':
            m = o.get('message', {})
            if m.get('id'): msgs.add(m['id'])
            tools += sum(1 for c in m.get('content') or [] if c.get('type') == 'tool_use')
        elif o.get('type') == 'result':          # one session ends here
            rows.append((os.path.basename(os.path.dirname(f)), o.get('subtype'), len(msgs), tools))
            msgs, tools = set(), 0
print(collections.Counter(r[2] for r in rows))
print(collections.Counter(r[1] for r in rows))
PY
```

The daemon log's own `pass=… turns=…` fields are the cheaper source and are now
in the right unit, but they only go back as far as the current retention window.

## Addendum — `tools=` / `files=` (Forge-q2qz)

Turns turned out to be a weak proxy for the question the budget work kept
circling: not *how much* a pass explored, but whether it explored **at all**. A
pass that terminates in one or two turns produced its JSON without calling a
tool — it never opened a file, and by the turn count alone that is
indistinguishable from a cheap pass that read what it needed and answered.

Across the 20 most recent runs on the skybert forge (2026-08-27) the split was:

| pass | mean turns | runs at ≤2 turns |
|---|---|---|
| triage | 1.0 | 20 (by design) |
| security | 2.5 | 13 |
| repo-specific | 9.8 | 7 |
| tests-missing | 11.7 | 1 |
| logic | 12.9 | 1 |
| conventions | 13.7 | 1 |

Two passes were reviewing diff text and nothing else, which is how an endpoint
missing the per-resource permission filter its siblings apply, and an
unsynchronized cache reached from a `Parallel.ForEach` in another file, are both
reviewed and both missed — neither is visible in a hunk, and both are obvious
one file away. The cause was the prompt, not the cap: `security.md` closed with
*"Do not speculate about code you cannot see"*, written against hallucination
and read as an instruction to stay inside the diff. `logic.md` carried no such
clause and averaged 12.9 turns against the same budget.

`RenderPassTelemetry` now carries `tools=<calls> files=<distinct paths>` per
pass. `tools=` is summed over the pass's sessions (the `CostUSD` convention, not
the final-session `turns=` one — a re-prompt's or a retry's exploration was
exploration too); `files=` is the size of the deduplicated union of what those
sessions opened, since "how many files did this pass read" is a question about a
set.

`tools=0` **is** rendered — it is the whole point of the field — whenever it can
be told apart from a missing measurement. On one pass alone it cannot: a pass
that made no tool call and a backend that reports no tool telemetry are the same
zero. The run resolves it, since one run is one provider: if any pass of the run
reported a non-zero count, a sibling's zero is a genuine "never opened
anything". Where no pass in the run reported one, the fields are omitted
together rather than printed as zeros that claim to know which case it was. (A
backend that reports its tool calls somewhere other than per-message — Gemini,
in its result event — is folded back in by `observedToolCalls` before any of
this, so it counts as measured.)

That makes the script above unnecessary for this question: `tools=0` on a
completed pass says outright what a low `turns=` only hinted at.

## Addendum — do 16 turns and $1.50 still hold? (Forge-q2qz)

Teaching two passes to explore raises the question this file exists for, since
both bounds were measured against the behaviour that change replaces.

**The turn cap (`assayMaxTurns` = 16).** Logic is the worked precedent: it
carries no anti-reading clause, averages 12.9 turns against this budget, and its
`error_max_turns` rate is part of what set the number. Moving security (2.5) and
repo-specific (9.8) into that regime is the intent of the change, so two more
passes will sit where logic sits — a mean four turns under the cap, on a
distribution whose right tail cannot be read off the log (see the censoring
argument above). The cap is deliberately not raised here on a prediction: a turn
exhaustion is **recoverable**, since `error_max_turns` earns one modified retry
(halved budget, an "answer now" instruction, and the diff scoped to the files
the dead session opened), so an under-sized cap costs money and a slower pass
rather than coverage.

**The spend ceiling (`assay.max_cost_per_pass_usd` = $1.50/session).** This is
the bound that bites, because `ReasonMaxCost` is terminal by design — never
retried, since a re-run buys the identical runaway at full price — so a pass
killed on spend counts as failed and its coverage drops out of the run. A
security pass that used to answer in ~2 turns and now reads files like logic
does is a materially larger session, and on the largest PRs the plausible
failure is that the pass Assay just taught to read is the one that gets stopped.
It is left at $1.50 for the same reason the cap is left at 16 — there is no
post-change data yet, and a bound moved on a guess is exactly what this file was
written to prevent — but it is the number to watch first.

**Re-measure rather than re-guess** (Forge-sra6). The telemetry added in the
same commit is what answers it: per-pass `turns=`/`tools=`/`files=`, the
`error_max_turns` and `error_max_cost` rates for security and repo-specific
specifically, and `forge assay stats` for the per-run cost. Act if either the
two passes start exhausting their turn budget materially more often than logic
does, or any pass reaches `error_max_cost` at all — the first says the cap is
too low for the work now asked of them, the second says the ceiling is removing
coverage silently. Record whatever the new distribution turns out to be here.

*Answered below, 2026-08-28 — but not on the data this paragraph asked for. The
reading prompts had been live for 24 hours and no Assay run had happened in
them, so the post-change distribution is empty. What the pre-change window does
show is that the second trigger had **already** fired.*

## Re-measurement — 2026-08-28 (Forge-sra6)

The paragraph above asked for the two bounds to be re-measured under the reading
prompts. **They could not be**: the prompts went live 24 hours before this was
run and no Assay review has happened since. What follows is therefore two
things — the null result on the question that was asked, and the answer the
window *before* it turns out to contain, which is that the spend ceiling had
already started removing coverage.

### The regimes, and where the boundaries are

Three behaviours have to be kept apart, and each boundary is the **deploy** that
carried the change, not the merge that landed it — Forge-q2qz merged at
2026-08-27 15:07 and was live at 15:15. Both boundaries are recoverable from the
daemon log's `self-deploy: restarting unit` lines, which carry the `build_sha`:

| regime | from | what is true in it |
|---|---|---|
| R0 | — | 12-turn cap; `turns=` is the provider's `num_turns`, i.e. the wrong unit (Finding 1) |
| R1 | 2026-08-25 17:39:44 +02:00 (`4795e636`) | 16-turn cap; corrected `turnCounter`; **old** prompts |
| R2 | 2026-08-27 15:15:25 +02:00 (`d9f3ec5a`) | reading prompts (Forge-q2qz) |

R0 numbers are not comparable with R1's and are not mixed into anything below.

### The null result: R2 is empty

**0 runs, 0 pass sessions**, from the deploy at 2026-08-27 15:15 to
2026-08-28 15:37. `assay_runs` holds 109 rows, the newest started
2026-08-27T13:04:53Z — ten minutes before the deploy — and no preserved
`assay-*.log` carries a run key past the boundary either. (Six of them carry a
later *mtime*, which is the 15:04 run's handles being flushed as the daemon went
down; the run key in the filename is the figure to filter on, not the mtime.)

The proximate reason is in the log one minute after the restart:

```
[bellows] PR #872: Assay review skipped — daily Assay budget exhausted
($104.04 spent >= $100.00 limit, resets at UTC midnight)
```

and nothing has opened a PR since. So the two questions the bead asked —
whether security and repo-specific now exhaust their turn budget more often
than logic does, and what they cost once they read files — have **no evidence
either way**, and no number here should be read as answering them.

`tools=`/`files=` likewise have **zero observations**: that telemetry shipped in
the same commit as the reading prompts, so no logged line carries it. The
Forge-q2qz addendum's per-pass turn table above (measured by hand off the
session logs) is still the only pre-change reading of that behaviour.

### R1 — what the corrected counter measured under the old prompts

22 runs / 132 pass observations from the daemon log's `passes=` field, and 141
pass sessions from the preserved logs (more, because a retried or re-prompted
pass runs more than one session and the log line reports only the final one).
All on the `forge` anvil, `claude-opus-5` for both triage and review.

Turns, per pass, final attempt (`RenderPassTelemetry`'s unit — model messages):

| pass | n | mean | p50 | p90 | max | `error_max_turns` | `error_max_cost` |
|---|---|---|---|---|---|---|---|
| triage | 22 | 1.0 | 1.0 | 1.0 | 1 | 0 | 0 |
| logic | 22 | 7.6 | 8.5 | 14.7 | 16 | 0 | 1 |
| security | 22 | 3.7 | 3.0 | 7.0 | 10 | 0 | 0 |
| conventions | 22 | 8.9 | 10.0 | 14.0 | 16 | 0 | 1 |
| tests-missing | 22 | 8.5 | 9.0 | 13.9 | 16 | 0 | 1 |
| repo-specific | 22 | 9.3 | 9.5 | 15.0 | 16 | 0 | 1 |

Per-session, before the retry that hides it: **12 of 141 sessions (8.5%) hit the
16-turn cap** — logic 5/26 (19%), repo-specific 3/24 (12%), conventions 2/23,
tests-missing 2/23, security 0/22, triage 0/23 — and **every one of them
recovered on its modified retry**, which is why no *pass* reports
`error_max_turns` at all. That is `buildRetryMods` working exactly as specified.

It is also the first datum from above the old censoring point, and it does not
say what a naive reading would expect: raising the cap 12 → 16 did **not** lower
the rate at which sessions reach it (7.1% at 12, 8.5% at 16). Passes expand into
the budget they are given. That is an argument against reaching for the cap
again — the binding constraint is not the number.

### The finding: the spend ceiling had already become a clip point

`assay.max_cost_per_pass_usd` is compared against `costTracker`'s **running
estimate**, not against the provider's final `total_cost_usd`. The two are not
the same quantity and must not be mixed: the estimate prices input and both
cache halves off each message's usage block, whose `output_tokens` is stamped
when the message *starts* and so reads 2 or 3 however much the model then
writes. Reproducing the tracker exactly over the 141 R1 sessions (dedupe by
message id, price at the Opus row) gives:

| pass | n | est. mean | est. p90 | est. p95 | est. max | billed/est. |
|---|---|---|---|---|---|---|
| triage | 23 | $0.42 | $0.62 | $0.62 | $0.92 | 1.63x |
| logic | 26 | $0.78 | $1.32 | $1.34 | $1.45 | 1.64x |
| security | 22 | $0.48 | $0.82 | $0.94 | $0.98 | 1.63x |
| conventions | 23 | $0.77 | $1.19 | $1.24 | $1.27 | 1.60x |
| tests-missing | 23 | $0.74 | $1.21 | $1.23 | $1.38 | 1.57x |
| repo-specific | 24 | $0.84 | $1.32 | $1.38 | $1.47 | 1.62x |
| **all** | **141** | **$0.68** | **$1.23** | **$1.32** | **$1.47** | **1.61x** |

Every surviving session is under $1.50 by construction — crossing it is what
ends a session — so this is a censored distribution in exactly the way the turn
one was, and its shape is the point: **the maximum is $1.467, or 98% of the
ceiling**, and p95 is 88% of it. The tail is piled against the wall.

What went over the wall, in one run:

```
2026-08-27T14:25:09  pr=872  logic          stopped after 12 turns: est. $1.58 >= $1.50
2026-08-27T14:25:09  pr=872  conventions    stopped after 13 turns: est. $1.57 >= $1.50
2026-08-27T14:25:09  pr=872  tests-missing  stopped after 13 turns: est. $1.51 >= $1.50
2026-08-27T14:25:09  pr=872  repo-specific  stopped after 13 turns: est. $1.55 >= $1.50
2026-08-27T14:25:09  pr=872  status="partial: 1 of 5 passes completed (… — error_max_cost)"
```

Four of five deep passes, in one run, killed between one and eight cents past
the ceiling. `ReasonMaxCost` is terminal by design and never retried, so that is
80% of a review's coverage removed for an overshoot inside the estimate's own
error bar — while the run still paid for all four sessions, because a stop is
not a refund. This is not a runaway being braked; it is honest work being
clipped, which is precisely what the turn cap was moved off in the section
above.

Two further observations pin it down:

- **The two bounds have become adjacent.** The 12 sessions that hit the 16-turn
  cap died holding estimates of $0.85–$1.45; the largest was five cents short of
  the ceiling, and had it been given more turns it would have crossed rather
  than converged. So raising the turn cap while leaving the ceiling would
  mostly convert `error_max_turns` — recoverable, one retry — into
  `error_max_cost` — terminal, coverage gone. If either bound moves first it has
  to be this one.
- **Not every crossing is an overshoot.** On 2026-08-25 16:16 (R0), PR #854's
  *triage* pass was stopped after **one turn** at an estimated $3.13, failing
  the whole run. A single message can exceed any moderate ceiling on a large
  enough prompt, and $3.00 would not have saved that one either. Nothing here
  claims to fix that case; `max_diff_bytes` and `skip_paths` are its levers.

### Per-run cost (`forge assay stats`)

The daemon's daily line, 2026-08-28 — no drift flag:

| ISO week | runs | mean $/run | mean s | complete | partial |
|---|---|---|---|---|---|
| 2026-W31 | 4 | $3.02 | 276 | — | — |
| 2026-W32 | 15 | $4.80 | 161 | — | — |
| 2026-W33 | 10 | $11.53 | 246 | 8 @ $11.20 | 0 |
| 2026-W34 | 38 | $7.66 | 174 | 31 @ $7.72 | 5 @ $10.40 |
| 2026-W35 | 35 | $7.52 | 222 | 31 @ $7.40 | 3 @ $10.32 |

The runs missing from the split are `unknown` — all 4 of W31, all 15 of W32 and
2 of W33 predate `assay_runs.status`, and W34/W35 each carry a `failed` run.
`WeeklyStats.All` is folded from the buckets, so the mean column is the
count-weighted blend of all of them, not of the two shown.

The R1 subset alone is 22 runs at a $7.29 mean, $10.80 max — indistinguishable
from the surrounding weeks. Note again what `WeeklyStats` was built to show:
partial runs cost **more** than complete ones in both weeks that have the split,
which is the same fact the ceiling section arrives at from the other side.

### The decision

**`max_cost_per_pass_usd` default $1.50 → $3.00.**
(`defaultAssayMaxCostPerPassUSD`, `internal/config/config.go`.)

The bead's rule was to act if any pass reached `error_max_cost` at all, because
that means the ceiling is removing coverage silently. Four did, in one run,
none of them more than 6% past the limit. The rule fired on pre-change data, which makes the case
stronger rather than weaker: the reading prompts move security and
repo-specific toward logic's session size, and logic already sat at 89% of the
ceiling at p95.

The number is not a percentile — it cannot be, since the ceiling truncates the
distribution it would be read from. It is the 12 → 16 argument in dollars:
clear the observed censoring point ($1.47 surviving max, $1.58 largest
crossing) by a factor, and check that what is left still catches the thing the
bound exists for. It does — a runaway is an order-of-magnitude event, not a 5%
overshoot, and $3.00 estimated is about **$4.80 billed** at the measured 1.61x
— roughly 4.4x the $1.09 mean billed session in this sample, and two thirds of
an entire typical run ($7.29 mean over R1) spent on one of its six sessions. A
pass that reaches that is not reading a large diff.

Set it in the **estimate's** unit, and note which direction the gap runs: the
ceiling is compared against a figure ~1.6x *below* the eventual invoice, so a
value chosen by reading billed session costs is that much more permissive than
it looks — a "$3.00" ceiling picked off an invoice does not bite until about
$4.80 of real spend. The gap is structural, not drift: the output column is
understated by construction (see `AddTurnCost`), so it will not close.

**No change to `assayMaxTurns` (16).** Its trigger did not fire: no pass in R1
ended `error_max_turns`, every session that reached the cap recovered on retry,
and security — the pass the change targets — never reached it once in 22 runs.
The trigger as written compares security and repo-specific *against logic under
the reading prompts*, which is R2, which is empty. Raising the cap now would
also be the wrong order of operations, per the adjacency finding above.

### What is still owed, and how to run it

The R2 measurement, tracked as **Forge-cikv**. It needs Assay runs under the
reading prompts, and this deployment produced none in 24 hours — largely
because `assay.daily_cost_limit_usd` had been exhausted. Re-run this once there
are ~20 runs past 2026-08-27 15:15 and record the result as another dated
section rather than editing these, since the drift across regimes is what this
file is for. Two retention limits bound how long that is possible:

- `daemon.log` rotates at 50 MB keeping 3 compressed backups, which at current
  volume is roughly a fortnight of `passes=` lines in total. Extract
  incrementally rather than assuming a month is still on disk.
- preserved per-bead logs are swept at `settings.log_retention_days` (30).

The daemon-log extractor. This is the cheap source and the one to start from:
its fields are already at *pass* level — `term=` is the outcome after the retry,
`tools=` is summed over the pass's sessions and `files=` is their deduplicated
union — whereas the session logs hold the raw events those were folded from and
have to be reassembled per pass.

Since Forge-6ltv the daemon log carries **cost too**, and carries it in the
ceiling's own unit, so the by-hand reproduction below is no longer the way to
size `max_cost_per_pass_usd`. Each `passes=` segment now ends with two fields
before `primer=`:

- **`cost_est=`** — `costTracker`'s running estimate, summed over the pass's
  sessions. **This is the quantity `assay.max_cost_per_pass_usd` is compared
  against**, and the only one a ceiling can be sized from. It is present for a
  pass the ceiling *stopped*, which is what the reproduction below can never
  recover.
- **`cost_usd=`** — the provider's billed `total_cost_usd`, over the same
  sessions. **Do not size the ceiling from this field.** It is the larger
  quantity by the structural ~1.61x measured above (a message's usage block is
  stamped when the message starts, so `output_tokens` is understated by
  construction and the gap will not close). A "$3.00" ceiling picked by reading
  `cost_usd` does not bite until about $4.80 of estimate.

Both are omitted rather than printed as zero where nothing measured them — a
run with no ceiling configured, or a backend that streams no per-turn usage —
on the same discipline `tools=`/`files=` follow, since `cost_est=0` would read
as a pass that cost nothing. On a pass the ceiling stopped the two read
**equal**: such a session emits no result event, so it has no provider figure
and both report the tracker's snapshot; `term=error_max_cost` on the same
segment says so outright.

So the R2 cost distribution is the first extractor plus two fields — add
`est=f.get('cost_est')` beside `tools=` and fold on it. What follows is the
pre-Forge-6ltv method, kept for reading runs logged before the fields existed
(and for auditing the tracker itself against raw events):

```bash
python3 - <<'PY'
import re, gzip, glob, os, json, collections, statistics
LINE  = re.compile(r'time=(\S+).*?msg="Assay review completed".*?passes="([^"]*)"')
FIELD = re.compile(r'\b(\w+)=([^\s,]+)')
op = lambda p: gzip.open(p, 'rt', errors='replace') if p.endswith('.gz') else open(p, errors='replace')
rows = []
for p in sorted(glob.glob(os.path.expanduser('~/.forge/logs/daemon*.log*'))):
    for ln in op(p):
        m = LINE.search(ln)
        if not m:
            continue
        for seg in m.group(2).split(', pass='):
            f = dict(FIELD.findall(seg if seg.startswith('pass=') else 'pass=' + seg))
            rows.append(dict(ts=m.group(1), name=f.get('pass'), turns=int(f.get('turns', 0)),
                             term=f.get('term'), tools=f.get('tools'), files=f.get('files')))
rows = [r for r in rows if r['ts'] >= '2026-08-27T15:15:25']     # the R2 boundary
for n in ('triage', 'logic', 'security', 'conventions', 'tests-missing', 'repo-specific'):
    rs = [r for r in rows if r['name'] == n]
    if rs:
        print(n, len(rs), round(statistics.mean(r['turns'] for r in rs), 1),
              collections.Counter(r['term'] for r in rs))
PY
```

The tracker-unit cost distribution as it had to be built *before* `cost_est=`
existed: reproduced from the preserved session logs by doing what `AddTurnCost`
does — dedupe by message id, price at the row `cost.EstimatePricing` resolves
for the configured model. This is what the 1.61x table above was measured with.
Reach for it only for runs predating the field, or to check the rendered figure
against the raw events; note the blind spot recorded under it, which is the
reason `cost_est=` was added at all.

```bash
python3 - <<'PY'
import json, glob, os, re, statistics
IN, OUT, CR, CW = 5.00, 25.00, 0.50, 6.25          # the Opus row of internal/cost
rows = []
for f in glob.glob(os.path.expanduser('~/.forge/logs/*/assay-*.log')):
    m = re.match(r'assay-(\d+)-([a-z-]+)-(\d+)-(\d+)\.log$', os.path.basename(f))
    if not m:                                       # pre-naming assay-<ts>-<seq>.log
        continue
    seen, est = set(), 0.0
    for ln in open(f, errors='replace'):
        if not ln.startswith('{'):
            continue
        try: o = json.loads(ln)
        except Exception: continue
        if o.get('type') == 'assistant':
            mm = o.get('message', {}); mid = mm.get('id') or ''
            if mid and mid in seen:                 # one API message, N block events
                continue
            if mid: seen.add(mid)
            u = mm.get('usage') or {}
            est += (u.get('input_tokens', 0) * IN + u.get('output_tokens', 0) * OUT
                    + u.get('cache_read_input_tokens', 0) * CR
                    + u.get('cache_creation_input_tokens', 0) * CW) / 1_000_000
        elif o.get('type') == 'result':             # one session ends here
            rows.append(dict(run=int(m.group(1)), name=m.group(2), est=round(est, 4),
                             billed=o.get('total_cost_usd'), subtype=o.get('subtype')))
            seen, est = set(), 0.0
rows = [r for r in rows if r['run'] >= 1787836525000]        # the R2 boundary, in epoch ms
for n in ('triage', 'logic', 'security', 'conventions', 'tests-missing', 'repo-specific'):
    rs = [r for r in rows if r['name'] == n]
    if rs:
        e = sorted(r['est'] for r in rs)
        print(n, len(rs), 'mean', round(statistics.mean(e), 3), 'max', e[-1])
PY
```

The run key in the filename is the epoch-ms `LogKey` the daemon minted, which
is what makes the boundary filter above a plain integer comparison — and note
that the sessions killed on cost are **absent** from this sample by
construction: they emit no `result` event. That is the blind spot: the script
reproduces the ceiling's unit for every session *except* the ones the ceiling
fired on, which are the only sessions that prove where it is set wrong. Those
now come off the daemon log's own `cost_est=` beside `term=error_max_cost`,
which is a `passes=` field like any other rather than a message to be parsed.

## Re-measurement — 2026-08-29 (Forge-cikv)

The R2 sample the section above could not take. It exists now, and it answers
both questions the parent asked — but the window turned out to hold a boundary
neither the parent nor Forge-sra6 anticipated, so the first job is to say where
it is.

### There are two post-change regimes, not one

Forge-sra6's own change — `max_cost_per_pass_usd` $1.50 → $3.00 — merged as
PR #874 and deployed at **2026-08-28 16:32:22 +02:00** (`71d3dfcd`), which is
*inside* the window this bead was told to measure. So "under the reading
prompts" is two regimes, and mixing them would compare a ceiling against
itself:

| regime | from | reading prompts | ceiling |
|---|---|---|---|
| R1 | 2026-08-25 17:39:44 +02:00 (`4795e636`) | no | $1.50 |
| R2 | 2026-08-27 15:15:25 +02:00 (`d9f3ec5a`) | **yes** | $1.50 |
| R3 | 2026-08-28 16:32:22 +02:00 (`71d3dfcd`) | **yes** | **$3.00** |

R2 is one run wide. Everything below that speaks about the ceiling is R3;
everything that speaks about the prompts is R2+R3, since the prompts are
identical across both.

### The sample, and what it is short of

**13 runs, 78 pass observations, 95 pass sessions**, from
2026-08-28 16:02:06 +02:00 to 2026-08-29 08:20:48 +02:00. All on the `forge`
anvil, `claude-opus-5` for triage and review both. The split across the two
regimes, so the headline reconciles on the page:

| regime | runs | pass observations | pass sessions |
|---|---|---|---|
| R2 | 1 (PR #874) | 6 | 7 |
| R3 | 12 | 72 | 88 |
| **total** | **13** | **78** | **95** |

R2's single run is the one place the observation and session columns do not
differ by retries alone, so it is worth stating why: its six passes ran eight sessions — triage,
five deep passes, plus a retry each in `logic` and `repo-specific` — and only
seven have a row. The missing one is the `conventions` session the $1.50
ceiling killed, which emits no `result` event and so cannot appear in a
session extract at all (the same construction the section above notes). R3's
72 pass observations are 12 runs x 6 passes exactly.

This is **13 runs where the bead asked for ~20**, and that shortfall is stated
rather than smoothed over: the per-*run* figures below (mean cost, mean
duration) are thin at n=12 and are reported as orientation, not as a
distribution. The questions the bead actually turns on are per *session* — is
any pass cost-terminated, which bound ends a session, does a pass read files —
and those have 88 observations, which is a real sample. Nothing here is tuned
off the run-level numbers.

Both extractors from the section above ran **verbatim and clean**, which is
itself a result: the `passes=` field and the session-log event shapes have not
drifted, so the R1 figures and these are the same measurement. Retention did
not bite either — the oldest recoverable `passes=` line on disk is
2026-08-11T07:08:22, well before the R2 boundary, so there is **no gap** in
this window. (`daemon.log` stood at 48.4 MiB against `MaxSizeMB: 50`, i.e.
**97% of its rotation threshold**, when this was taken — that margin was luck
rather than headroom.)

The artifacts, extracted 2026-08-29T06:21Z:

| File | What |
|---|---|
| `measurements/assay-passes-20260827T131525Z-20260829T062100Z.json` | 78 rows, one per pass observation off `daemon.log` |
| `measurements/assay-session-costs-20260827T131525Z-20260829T062100Z.json` | 95 rows, one per pass session off the preserved `assay-*.log` files |

Both filenames open at `20260827T131525Z`, which is the **R2 boundary** — the
window the extractors were asked for, per this directory's naming convention —
and not the window they found: the first run inside it is 2026-08-28 16:02:06
+02:00, a day later, because `assay.daily_cost_limit_usd` was exhausted for
most of the intervening day. The prose above names the observed window; the
filenames name the requested one.

Both extractors were run with their boundary filter as printed — the
`rows = [... if r['ts'] >= '2026-08-27T15:15:25']` line in the first and the
epoch-ms twin of it in the second — so the filtering is part of the verbatim
extractor rather than a modification of it. The session extractor was
additionally asked for each session's `tool_use` block names and turn count,
which the printed one discards; that is the only departure from verbatim and
it adds fields rather than changing any.

### The ceiling is now non-binding, and it was still binding at $1.50

**Zero `error_max_cost` in 12 runs / 88 sessions under $3.00.** The whole
recoverable log holds three ceiling-stop events, six killed passes between
them, and every one is behind the raise and against the **$1.50** ceiling:
PR #854's triage at $3.13 (R0, 2026-08-25 16:16 — the one the section above
notes $3.00 would not have saved either), PR #872's four-pass wipeout (R1,
2026-08-27 14:25), and one in R2.

Estimated session cost — `costTracker`'s unit, reproduced as before — over R3:

| pass | n | mean | p50 | p90 | p90 as % of $3.00 | max | max as % of $3.00 | over $1.50 |
|---|---|---|---|---|---|---|---|---|
| triage | 12 | $0.39 | $0.41 | $0.67 | 22% | $0.81 | 27% | 0 |
| logic | 15 | $0.77 | $0.66 | $1.40 | 47% | $1.84 | 61% | 2 |
| security | 15 | $0.76 | $0.74 | $1.55 | 52% | $1.64 | 55% | 2 |
| conventions | 16 | $0.71 | $0.69 | $1.30 | 43% | $1.56 | 52% | 1 |
| tests-missing | 13 | $0.68 | $0.65 | $1.26 | 42% | $1.58 | 53% | 1 |
| repo-specific | 17 | $0.77 | $0.83 | $1.38 | 46% | $1.61 | 54% | 2 |
| **all** | **88** | **$0.69** | **$0.63** | **$1.40** | **47%** | **$1.84** | **61%** | **8** |

The shape is the point, and it is the opposite of R1's. There the maximum was
98% of the ceiling and p95 was 88% — a distribution piled against the wall,
censored by it. Here p90 across all 88 sessions ($1.40) is **47%** of the
ceiling and the single largest session is **61%** — both read off the **all**
row above. Per pass, p90 runs 42%–52% across the five deep passes. The tail
has room.

The last column is the counterfactual, and it is why the raise was not
cosmetic: **8 of 88 sessions (9%) came in above $1.50**, spread across all five
deep passes rather than concentrated in one. Under the old ceiling every one of
them would have been killed on `ReasonMaxCost` — terminal, never retried — and
their runs would have reported partial coverage while still paying for the
sessions.

R2's single run is the same fact from the other side. PR #874's `conventions`
pass — on the very bead that raised the ceiling — died at
`estimated session cost $1.53 reached the $1.50 per-pass ceiling`, after 16
turns. Three cents over, at the turn cap, with the fix for it in the diff it
was reviewing. So the reading prompts *did* push sessions through the old
ceiling, exactly as Forge-sra6 predicted from pre-change data; the prediction
had one run to be confirmed on before the change landed, and it was confirmed
on it.

### The turn cap is now the binding bound

The two are no longer adjacent, and they have swapped places. Every deep
session in R3 that did not answer was ended by the **turn** cap:

| bound | deep sessions ended (R3, n=76) |
|---|---|
| 16-turn cap (`error_max_turns`) | 16 |
| $3.00 ceiling (`error_max_cost`) | **0** |
| answered | 60 |

At session level, before the retry hides it:

| pass | sessions | hit the 16-turn cap | rate | mean turns | mean tool calls |
|---|---|---|---|---|---|
| logic | 15 | 3 | 20% | 8.3 | 9.3 |
| security | 15 | 3 | 20% | 7.9 | 9.3 |
| conventions | 16 | 4 | 25% | 7.8 | 8.5 |
| tests-missing | 13 | 1 | 8% | 7.1 | 7.2 |
| repo-specific | 17 | 5 | 29% | 8.8 | 10.4 |
| **all deep** | **76** | **16** | **21%** | — | — |

**21.1% of deep sessions (16 of 76), against R1's 10.2% (12 of 118).** Both
sides of that ratio are deep sessions, which is the only way to compare them:
R1's headline 8.5% is quoted above over **all 141** of its sessions, 23 of them
triage, and no triage session in either regime has ever reached the cap — so
the all-session figure is diluted by a pass that cannot contribute to it. Like
for like the jump is **2.07x** on the deep-session basis (21.1% / 10.2%) and
2.14x on the all-session one (16 of 88 = 18.2%, against 12 of 141 = 8.5%).
Call it **twice the rate**, on the same cap, from the prompt change alone —
which is the intended effect showing up as pressure on the bound it was always
going to press on.

And the adjacency that made Forge-sra6 refuse to touch the turn cap is gone.
The R1 argument was that sessions dying on turns were holding $0.85–$1.45
against a $1.50 ceiling, so giving them more turns would only convert a
recoverable `error_max_turns` into a terminal `error_max_cost`. The 16 sessions
the cap killed here hold a mean of **$1.15** and a maximum of **$1.84** —
38% and 61% of the ceiling. There is room above them now. That conversion
argument no longer applies.

This is a **recommendation, not a change** (see below).

### The prompts worked, and `files=` cannot see it

The first `tools=` observation there has ever been, since the counter shipped
in the same commit as the prompts.

| pass | pre-change (Forge-q2qz, by hand) | now (R3, all sessions) |
|---|---|---|
| security | mean 2.5 turns; 13 of 20 runs answered from diff text alone | mean 7.9 turns, 9.3 tool calls; **12 of 12 first attempts called a tool** |
| repo-specific | ~7 of 20 runs answered from diff text alone | mean 8.8 turns, 10.4 tool calls |

Security's lowest first attempt in R3 still made one tool call. The behaviour
the parent bead was written against — a pass reviewing the diff without opening
the code — does not appear in this sample at all: **58 of 60 first attempts
called at least one tool**, and both exceptions are single-turn.

The 18 sessions that made no tool call are **all 16 retries plus those two**.
That is `answerNowInstruction` working: the retry is told to stop exploring and
answer from what it has, and every one of them did, in exactly 1 turn.

Cost of reading, which is the fourth thing the bead asked for:

| deep sessions (R3) | n | mean est. | p90 | max | mean turns | mean tools |
|---|---|---|---|---|---|---|
| called a tool | 58 | $0.87 | $1.58 | $1.84 | 10.2 | 11.8 |
| answered without tools | 18 | $0.31 | $0.53 | $0.61 | 1.0 | 0 |

A reading session costs about **2.8x** a non-reading one. That ratio is the
price of the change, and at the run level it is close to invisible: 12 R3 runs
average **$7.83** against R1's 22-run **$7.29** (+7%), same mean duration
(239s), max $14.39 against $10.80. The reading prompts moved the *maximum* run
cost, not the typical one.

**`files=` is 0 on every one of the 78 pass observations, and it is wrong.**
Not "no files were read" — 742 tool calls were made across these 95 sessions
and **every single one of them is `Bash`**. `fileTracker.add` records a path
only from a `tool_use` block whose input carries `file_path`, which is the
`Read` tool's shape; a session that reads with `cat`, `sed -n` or `grep` names
no `file_path` and is counted by `tools=` while contributing nothing to
`files=`. On this deployment that is 100% of sessions, so `files=` is
structurally zero and says nothing. `tools=` is unaffected and is the field
that carries the signal — which is the counter's own design working
(`addCall` counts a block naming no file precisely so the two cannot be
conflated), but it leaves half the telemetry inert. Filed as **Forge-72oy**.

This also means `openedDiffFiles` — the third of `buildRetryMods`' three
modifications, the diff scoped to the files the failed session opened — is
never constructed here. The 16 retries in this sample were modified by reduced
budget and appended instruction alone, and all 16 still recovered.

### The decision

**No setting is changed by this bead.** Forge-sra6 already spent one adjustment
on a pre-change window, and a second unreviewed tune inside the same fortnight
would confound the next regime comparison — R3 would then be two regimes as
well, for the same reason R2 turned out to be.

**On `max_cost_per_pass_usd` ($3.00): keep.** Its trigger is "any pass reaches
`error_max_cost`", and none did in 12 runs. p90 at 47% of the ceiling and a
maximum at 61% is the non-binding bound the raise was meant to produce, and the
9% of sessions that cleared the old $1.50 mark say the raise was load-bearing
rather than precautionary. Nothing here is a regression against Forge-sra6's
intent.

**On `assayMaxTurns` (16): recommend raising to 24, filed as Forge-55eq.** The
trigger this time did fire, twice over:

- 21.1% of deep sessions reach the cap (16 of 76), against 10.2% under the old
  prompts (12 of 118) — twice the rate, both figures over deep sessions only.
  That is not a cap that occasionally bites.
- The reason Forge-sra6 declined to move it — the two bounds being adjacent, so
  more turns would convert recoverable turn deaths into terminal cost deaths —
  is measured gone: turn-killed sessions hold $1.15 mean / $1.84 max against
  $3.00.

24 is the same argument the 12 → 16 move used, and it carries the same caveat
that move earned: raising 12 → 16 did **not** lower the rate at which sessions
reached the cap (7.1% → 8.5%), because passes expand into the budget they are
given. So the expected outcome of 16 → 24 is a lower *retry* rate, not a lower
cap-hit rate, and the reason to want it is that a retry is a second session at
full price whose scoping modification (`openedDiffFiles`) is inert here.

That last point is why the ordering matters, and why this is a recommendation
rather than an edit: **fix `files=` first.** The retry currently runs on two of
its three modifications, and the third is the one that narrows the question
rather than merely shortening it. A cap raise evaluated before that fix would
be measuring the wrong retry.

### What is still owed

- **~20 runs.** This is 13. The session-level answers are firm; the run-level
  cost figures are not, and a later section should re-state them at n≥20 rather
  than treat $7.83 as settled.
- **The `files=` fix** (Forge-72oy), and a re-measurement of the retry once
  `openedDiffFiles` is actually being constructed.
- **The turn-cap raise** (Forge-55eq, blocked on Forge-72oy), after both of the
  above.

Retention is the clock on all three: `daemon.log` was at 97% of its rotation
threshold when this was taken. Extract before analysing, as this section did.

## `files=` fixed — 2026-08-29 (Forge-72oy)

The measurement above ends with three owed items, and the first of them is now
closed: `fileTracker` reads a path from a shell command line as well as from a
`file_path`-shaped tool input (`internal/assay/bashpaths.go`,
`toolUseInput.paths`). Nothing else moved — no setting, no prompt, no cap.

What changes as a result:

- **`files=` becomes a measurement.** It was structurally zero on all 78 pass
  observations because 100% of the 742 tool calls were `Bash`; a session that
  reads with `cat`, `sed -n` or `grep` now contributes the paths it named. It is
  a slight over-count by construction — a path named on a command line that did
  not open it still lands in the set — and is read as the rough figure it is:
  the question it answers is whether the pass went and looked, which `tools=`
  alone cannot separate from a pass that ran `go build` twice.
- **The retry gets its third modification.** `openedDiffFiles` can now select,
  so a turn-budget retry is scoped to the changed files the failed session
  actually opened rather than running on reduced budget and appended instruction
  alone. The safety property is unchanged and is what makes the loose parse
  acceptable: only files already in the diff are ever selected, so a misparsed
  token narrows the retry by less than it should have and can never reach
  outside the change under review.

**Forge-55eq (`assayMaxTurns` 16 → 24) is unblocked, and is still not an edit
to make blind.** The reason for the ordering was that a cap raise measured
against a retry running on two of its three modifications measures the wrong
retry. That objection is gone, but the evidence behind the raise was gathered
under the inert retry, so the re-measurement it wants is a fresh window under
this fix: the deep-session cap-hit rate (21.1% here), the retry rate, and
whether a scoped retry recovers on fewer turns and less money than the
unscoped one did. The 16 retries in the sample above all recovered without
scoping, so the raise's expected win — fewer retries, not fewer cap hits — is
what the new window has to show.
