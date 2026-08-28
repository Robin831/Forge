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
have to be reassembled per pass. What the daemon log does not carry is cost,
which is the next script (and Forge-6ltv):

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

And the tracker-unit cost distribution, which the daemon log cannot give —
per-pass cost is not a rendered telemetry field, so it has to be reproduced
from the preserved session logs by doing what `AddTurnCost` does: dedupe by
message id, price at the row `cost.EstimatePricing` resolves for the configured
model. Without this the only per-session figure available is `total_cost_usd`,
which is the wrong quantity by the 1.61x above.

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
construction: they emit no `result` event. The crossings have to be read off
the daemon log's `error_max_cost` lines, which carry the estimate at death.
