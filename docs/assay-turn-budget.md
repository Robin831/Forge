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
