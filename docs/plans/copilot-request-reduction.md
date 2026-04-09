# Reducing Copilot Premium Requests

> GitHub Discussion: #224

## Problem

Copilot charges **per request** (not per token). Each pipeline stage spawns an independent Copilot CLI invocation, and the feedback loop between Smith and Warden can multiply requests significantly.

**Current request count per bead:**

| Scenario | Requests | Premium Cost (Sonnet 1x) |
|----------|----------|--------------------------|
| Happy path (Smith + Warden) | 2 | 2x |
| With Schematic | 3 | 3x |
| Warden requests changes (5 iterations) | 10 | 10x |
| + CI fix (2 attempts) | 12 | 12x |
| + Review fix (5 attempts) | 17 | 17x |

With Opus (3x multiplier) or Opus-fast (30x), costs escalate dramatically.

## Current Request Flow

```
                          Copilot Requests
                          ────────────────
Schematic (optional)      1 request
Smith (implementation)    1 request
  ↕ Warden (review)      1 request    ← repeats up to 5x
CI Fix                    1 request    ← up to 2 attempts
Review Fix                1 request    ← up to 5 attempts
```

Each of these is a separate `copilot` CLI invocation → separate premium request charge.

## Optimization Strategies

### Strategy 1: Conditional Warden Skip for Small Changes

**Impact: ~30% of beads save 1 request each**

When Smith produces a small, low-risk diff, skip the Warden review entirely.

**Skip criteria** (all must be true):
- Diff is under 100 lines changed
- Changes are limited to: tests, docs, config, or single-file patches
- No security-sensitive files touched (auth, crypto, permissions)
- Bead priority is P3 or P4

**Implementation** in `internal/pipeline/pipeline.go`:

```go
func shouldSkipWarden(diffStat DiffStat, bead BeadInfo) bool {
    if bead.Priority <= 2 {
        return false // always review important beads
    }
    if diffStat.LinesChanged > 100 {
        return false
    }
    if diffStat.TouchesSecurityFiles {
        return false
    }
    return diffStat.IsDocsOnly || diffStat.IsTestsOnly || diffStat.FilesChanged <= 2
}
```

Add a config toggle:

```yaml
settings:
  copilot_skip_warden_small_diffs: true
```

### Strategy 2: Skip Schematic for Copilot Provider

**Impact: Save 1 request per large bead**

Schematic pre-analysis adds value for Claude (plan before code), but is an extra request on Copilot. Since Copilot bills per-request, the analysis cost may outweigh the benefit.

**Implementation** in `internal/pipeline/pipeline.go`:

When the active provider is Copilot, skip the Schematic phase unless the bead is explicitly tagged for decomposition.

```go
func (p *Pipeline) shouldRunSchematic(providers []provider.Config) bool {
    if !p.SchematicConfig.Enabled {
        return false
    }
    // Skip schematic for Copilot to save a premium request,
    // unless the bead explicitly needs decomposition
    if providers[0].Kind == provider.Copilot && !p.Bead.HasTag("decompose") {
        return false
    }
    return p.beadExceedsWordThreshold() || p.Bead.HasTag("decompose")
}
```

### Strategy 3: Use Cheaper Models for Review Stages

**Impact: 67% cost reduction on Warden/Schematic requests**

Copilot premium multipliers vary by model:
- Sonnet 4.6: **1x**
- Haiku 4.5: **0.33x**
- Opus 4.6: **3x**

Warden and Schematic don't need the strongest model — they produce short structured verdicts, not complex code. Route these stages to Haiku when using Copilot.

**Implementation** — add per-stage model override in config:

```yaml
settings:
  smith_providers: ["copilot/claude-sonnet-4-6"]  # 1x for code generation
  warden_model_override: claude-haiku-4-5          # 0.33x for review
  schematic_model_override: claude-haiku-4-5       # 0.33x for analysis
```

> **Note:** `smith_providers` uses the existing `[]string` format (`"kind/model"`). The `warden_model_override` and `schematic_model_override` are new string fields on `SettingsConfig` that only override the model within the provider, not the provider itself.

In `internal/pipeline/pipeline.go`, when spawning Warden or Schematic with a Copilot provider, substitute the model:

```go
func (p *Pipeline) wardenProviders() []provider.Config {
    providers := p.Providers
    if override := p.Config.Settings.WardenModelOverride; override != "" {
        for i, pv := range providers {
            if pv.Kind == provider.Copilot {
                providers[i].Model = override
            }
        }
    }
    return providers
}
```

### Strategy 4: Combined Smith+Warden Prompt (Single Request)

**Impact: Eliminate Warden as separate request — saves 1-5 requests per bead**

Instead of running Smith and Warden as separate sessions, embed review criteria directly into the Smith prompt and have Smith self-review before finalizing.

**How it works:**

1. Append Warden's review checklist + learned rules to the Smith prompt
2. Smith implements the change AND reviews its own diff
3. Smith outputs a structured `self_review` section with verdict
4. Pipeline only spawns a real Warden if Smith's self-review flags concerns

**Implementation** in `internal/prompt/prompt.go`:

Add a `CopilotMode` flag to the prompt builder that appends review instructions:

```go
if builder.CopilotMode {
    prompt += "\n\n## Self-Review Checklist\n"
    prompt += "After implementing the change, review your own diff against these criteria:\n"
    prompt += wardenRules  // from warden learned rules
    prompt += "\nOutput a JSON block: {\"self_review\": \"approve\" | \"needs_revision\", \"concerns\": [...]}\n"
    prompt += "If needs_revision, revise your code before finalizing.\n"
}
```

**Trade-off:** Loses independent review. Mitigate by:
- Only enabling for Copilot provider (Claude keeps separate Warden)
- Running full Warden on P0-P1 beads regardless
- Sampling: run real Warden on 10-20% of beads to validate self-review quality

```yaml
settings:
  copilot_combined_smith_warden: true
  copilot_warden_sample_rate: 0.1  # 10% get real Warden review
```

### Strategy 5: Batch Lifecycle Fixes

**Impact: Save 1-3 requests per failed PR**

When a PR has multiple CI failures or review comments, batch them into a single Smith invocation instead of spawning separate cifix/reviewfix workers per issue.

**Implementation** in `internal/quench/quench.go` and `internal/burnish/burnish.go`:

```go
// Instead of one Fix() call per failed check:
func BatchFix(ctx context.Context, failures []CIFailure) *FixResult {
    combinedPrompt := buildBatchPrompt(failures) // all failures in one prompt
    result := smith.SpawnWithProvider(ctx, combinedPrompt, ...)
    return result
}
```

## Recommended Rollout

### Phase 1 — Quick wins, low risk (Week 1)

| Strategy | Expected Savings | Risk |
|----------|-----------------|------|
| Skip Schematic for Copilot (#2) | -1 req/large bead | Low |
| Cheaper models for review (#3) | -67% cost on review reqs | Low-Medium |

**Savings estimate:** ~40% cost reduction

### Phase 2 — Medium risk (Week 2-3)

| Strategy | Expected Savings | Risk |
|----------|-----------------|------|
| Skip Warden for small diffs (#1) | -1 req/30% of beads | Medium |
| Batch lifecycle fixes (#5) | -1-3 req/failed PR | Medium |

**Savings estimate:** additional ~20% cost reduction

### Phase 3 — Architectural change (Week 4+)

| Strategy | Expected Savings | Risk |
|----------|-----------------|------|
| Combined Smith+Warden (#4) | -1-5 req/bead | High |

**Savings estimate:** additional ~30% cost reduction, but requires quality validation

## Measurement

Track these metrics before and after each phase:

1. **Copilot requests/day** — from `copilot_premium_requests` table
2. **Requests/bead** — count events per bead (schematic + smith + warden + cifix + reviewfix)
3. **First-pass approval rate** — % of beads where Warden approves on first try
4. **Quality regression** — track PR rejection rate, CI failure rate after merge

Add a dashboard query:

The existing `copilot_premium_requests` table is keyed by `date` with `requests_used` (REAL, weighted count) and `request_limit` (INTEGER). Query using the actual schema:

```sql
SELECT
    date,
    requests_used as premium_requests,
    request_limit
FROM copilot_premium_requests
WHERE date >= date('now', '-30 days')
ORDER BY date;
```

> **Note:** Per-invocation tracking (which stage triggered each request) would require a schema extension — e.g., adding an `invocations` table with `date`, `bead_id`, `stage`, `multiplier` columns. This is recommended for measuring Strategy 1-4 effectiveness but is not strictly required for Phase 1.

## Config Summary

All new settings with defaults:

```yaml
settings:
  # Strategy 1: Skip warden for small Copilot diffs
  copilot_skip_warden_small_diffs: false  # opt-in

  # Strategy 2: Skip schematic for Copilot (automatic when provider is Copilot)
  # No config needed — keyed off provider kind

  # Strategy 3: Cheaper models for non-Smith stages
  warden_model_override: ""               # e.g. "claude-haiku-4.5"
  schematic_model_override: ""            # e.g. "claude-haiku-4.5"

  # Strategy 4: Combined Smith+Warden
  copilot_combined_smith_warden: false    # opt-in, high risk
  copilot_warden_sample_rate: 0.1        # 10% get real Warden

  # Strategy 5: Batch lifecycle fixes
  copilot_batch_ci_fixes: false           # opt-in
  copilot_batch_review_fixes: false       # opt-in
```

## Files to Modify

| File | Changes |
|------|---------|
| `internal/pipeline/pipeline.go` | Schematic skip logic, Warden skip logic, combined mode |
| `internal/config/config.go` | New settings fields |
| `internal/prompt/prompt.go` | Self-review checklist injection for combined mode |
| `internal/warden/warden.go` | Accept model override parameter |
| `internal/schematic/schematic.go` | Accept model override parameter |
| `internal/quench/quench.go` | Batch fix mode |
| `internal/burnish/burnish.go` | Batch fix mode |
| `internal/cost/premium.go` | No changes needed (multipliers already correct) |
| `docs/configuration.md` | Document new settings |
