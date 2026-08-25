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
