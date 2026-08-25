// Package assay implements Forge's AI pull-request review engine ("Assay").
//
// Review runs a multi-pass review over a PR diff: a cheap Triage pass scopes
// which files warrant deeper inspection, then five deep passes (Logic,
// Security, Conventions, Tests-missing, Repo-specific) run in parallel and emit
// structured findings. On a repeat review of the same PR, every pass prompt
// carries the already-reported findings from earlier runs (so the model does
// not restate them) and, when the caller supplies an incremental diff, an
// explicit delta-review framing. Findings are then aggregated in order:
// deduplicated by a stable content hash, collapsed across passes by body
// similarity, suppressed against prior findings (resolved ones included), then
// bounded by two cumulative per-PR budgets — the Nit cap and the total
// findings cap.
//
// Every finding is idempotent per PR head SHA: re-running Review against the
// same head produces the same hashes, and persistence is OR-IGNORE keyed on the
// hash, so a finding is recorded exactly once.
//
// The engine never hard-codes a model identifier — all model selection flows
// through Config (ModelTier / TriageModel / ReviewModel). See config.go.
package assay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/diff"
	"github.com/Robin831/Forge/internal/state"
)

// Severity classifies how seriously a finding should be treated.
type Severity string

const (
	// SeverityImportant marks a finding that should block or strongly influence
	// the merge decision. Important findings are never capped or suppressed.
	SeverityImportant Severity = "Important"
	// SeverityNit marks a low-priority style/polish finding. Nits are subject to
	// the Nit cap and to already-posted suppression on re-runs.
	SeverityNit Severity = "Nit"
	// SeverityPreExisting marks an issue that predates the diff under review.
	SeverityPreExisting Severity = "PreExisting"
)

// ReviewRequest carries everything the engine needs to review one PR head.
type ReviewRequest struct {
	// Anvil is the repository (anvil) name; used to key persisted rows.
	Anvil string
	// AnvilPath is the local checkout path, used to read repo-level context
	// (REVIEW.md, AGENTS.md, etc.). Optional — may be empty. When set and
	// RepoGuidance is empty, Review() reads <AnvilPath>/REVIEW.md into
	// RepoGuidance once per run so every pass sees the same trusted guidance.
	AnvilPath string
	// RepoGuidance carries the repository's REVIEW.md content (or any
	// equivalent trusted instructions) injected into every pass prompt at
	// highest priority. Optional — empty means the engine uses default
	// calibration only. Pre-populated by Review() from AnvilPath when unset.
	RepoGuidance string
	// PRNumber is the pull request number under review.
	PRNumber int
	// HeadSHA is the PR head commit OID. It is the idempotency key: findings are
	// recorded against this SHA and a repeat review of the same SHA is a no-op.
	HeadSHA string
	// Diff is the unified git diff to review.
	Diff string
	// BeadID is the originating bead, used for context/logging. Optional.
	BeadID string
	// Title is the PR/bead title, supplied to passes as scope context. Optional.
	Title string
	// Description is the PR/bead body, supplied as scope context. Optional.
	Description string
	// WorkDir is the directory the default Smith-based runner executes in
	// (typically the PR worktree). Ignored when a custom runner is installed.
	WorkDir string
	// Incremental marks a delta review: Diff carries only the changes pushed
	// since the last reviewed commit (BaselineSHA), not the whole base..head
	// diff. Every pass prompt then says so explicitly — the model on a repeat
	// pass otherwise believes it is seeing the PR for the first time and
	// re-derives findings for code it already commented on.
	Incremental bool
	// BaselineSHA is the previously reviewed commit an incremental Diff is
	// relative to. Informational (named in the prompt); empty is fine.
	BaselineSHA string
	// ElidedFiles names the files whose hunks were dropped from Diff before
	// the review because they matched an auto-generated or skip-path glob.
	// Review() populates it from the filter it runs, replacing anything a
	// caller set — the list has to describe the diff the passes were actually
	// handed, so it is derived where the filtering happens and nowhere else.
	// Every pass prompt names it: without the note a lockfile-only PR reads to
	// a pass as an empty one, and an operator has no way to see the filter
	// working at all.
	ElidedFiles []string
	// PriorFindings lists findings recorded on earlier reviews of this PR,
	// injected into every pass prompt as an already-reported list the model
	// must not restate. Review() populates it from the DB when empty and a DB
	// is present; a caller (or test) that sets it keeps full control.
	PriorFindings []PriorFinding
	// OnPassLog, when set, is called with the log path of each pass as it is
	// spawned — triage first, then the deep passes concurrently. The daemon
	// uses the first call to point the Assay worker row's log_path at a file
	// the Hearth live panel can stream; without it the panel renders an
	// endlessly empty transcript for the whole run. Ignored when a custom
	// runner is installed. Implementations must be safe for concurrent use.
	OnPassLog func(logPath string)
}

// PriorFinding is one finding from an earlier review of the same PR, as it is
// surfaced to the passes: enough to recognize and avoid restating it, nothing
// more.
type PriorFinding struct {
	// Anchor locates the finding (e.g. "path/to/file.go:42").
	Anchor string
	// Severity is the recorded severity label.
	Severity string
	// Title is the one-line summary as originally reported.
	Title string
	// Resolved reports whether the finding's thread has since been resolved
	// (fixed, or stale for consecutive reviews). Resolved findings must not be
	// re-reported at all.
	Resolved bool
}

// Finding is a single code-review observation emitted by a pass.
type Finding struct {
	// File is the affected file path (b-side path from the diff).
	File string `json:"file"`
	// Anchor locates the finding within the file (e.g. "path/to/file.go:42").
	Anchor string `json:"anchor"`
	// Category is a short machine label for the kind of issue (defaults to the
	// emitting pass name when the model omits it).
	Category string `json:"category"`
	// Severity is the triaged seriousness of the finding.
	Severity Severity `json:"severity"`
	// Title is a one-line summary.
	Title string `json:"title"`
	// Body is the full explanation.
	Body string `json:"body"`
	// Evidence is an optional supporting snippet (diff lines, etc.).
	Evidence string `json:"evidence"`
	// SourcePass records which pass produced the finding.
	SourcePass string `json:"-"`
	// Hash is the dedup key: sha256(anchor + category + canonical(body)).
	// Computed by the engine, not the model.
	Hash string `json:"-"`
}

// PassReport captures per-pass execution metadata for observability.
//
// The telemetry half (Turns/TerminationReason/Attempts/Retried) exists so the
// turn budget can be tuned against real runs: a pass that ends on
// error_max_turns having burned the whole budget on a nine-line diff is
// exploring, not reading a large change, and only the per-pass numbers say
// which of the two happened.
type PassReport struct {
	// Name is the pass identifier (e.g. "logic").
	Name string
	// Findings is the number of findings the pass contributed before
	// aggregation.
	Findings int
	// CostUSD is the model cost attributed to the pass, summed over every
	// provider session it took — a strict-JSON re-prompt and a turn-budget
	// retry alike, and failed sessions as well as successful ones. Note that
	// this counts sessions, not Attempts: the two fields deliberately measure
	// different things.
	CostUSD float64
	// Turns is the turn count of the session the pass recorded — the final
	// one, not the sum, so the number stays comparable to the --max-turns
	// budget a single session is given.
	Turns int
	// TerminationReason is how the pass ended: "" when it answered, else the
	// same label FailedPasses carries (a provider result subtype where there
	// is one, e.g. "error_max_turns").
	TerminationReason string
	// Attempts is how many *turn-budget* attempts the pass took — 2 when it was
	// re-run after exhausting its budget. It is not a session count: an attempt
	// whose first reply will not parse makes a second provider session with a
	// stricter reminder, and that session is one attempt's business, not a
	// second attempt at the budget. CostUSD does count it, which is why the two
	// fields can disagree; the retry telemetry is what Attempts is for, and
	// only a turn-budget re-run belongs in it.
	Attempts int
	// Retried reports whether the pass was re-run in a fresh session after
	// hitting error_max_turns.
	Retried bool
	// CacheCreationTokens is how many input tokens the pass paid to write into
	// the provider's prompt cache, and CacheReadTokens how many it served from
	// a prefix already there. Both are summed over every provider session the
	// pass made, like CostUSD.
	//
	// This pair is the only in-band evidence that the shared-prefix prompt
	// ordering and the staggered fan-out are still working. When they are, one
	// deep pass reports a large creation and the other four report a large read
	// with a small creation of their own instruction block. When they are not —
	// somebody moves a pass-specific string above the shared head, or the
	// stagger is removed — every pass reports a large creation again, and the
	// per-run redundancy (the sum of CacheCreationTokens minus the largest one)
	// goes straight back to what buildPassPrompt measured before the ordering
	// change. Nothing branches on these; they exist so the regression is
	// visible in the daemon's log line rather than only in a monthly bill.
	CacheCreationTokens int
	CacheReadTokens     int
	// Primer reports whether this was the pass run alone ahead of the fan-out
	// to write the shared prefix. Exactly one deep pass carries it, and it is
	// the pass whose large CacheCreationTokens is expected rather than a
	// regression.
	Primer bool
}

// ReviewResult is the outcome of a Review.
type ReviewResult struct {
	// Findings is the final, aggregated set (deduped, suppressed, capped).
	Findings []Finding
	// HeadSHA echoes the reviewed head SHA.
	HeadSHA string
	// ShadowMode reports whether the review ran in shadow mode (no posting).
	ShadowMode bool
	// CostUSD is the total model cost across all passes.
	CostUSD float64
	// Duration is the wall-clock time spent in Review.
	Duration time.Duration
	// Passes holds per-pass metadata.
	Passes []PassReport
	// NitsCapped is the number of Nit findings dropped by the Nit cap.
	NitsCapped int
	// NitsSuppressed is the number of Nit findings dropped because they were
	// already posted on a prior review of this PR.
	NitsSuppressed int
	// TotalCapped is the number of findings (any severity) dropped by the
	// cumulative per-PR findings budget (Config.MaxFindingsPerPR).
	TotalCapped int
	// PassErrors lists per-pass error strings (formatted "<pass>: <reason>")
	// for any deep pass that failed. A non-empty PassErrors with a non-nil
	// result means at least one pass produced findings; aggregation only
	// fails hard when every deep pass errored.
	PassErrors []string
	// FailedPasses is the structured form of PassErrors: which deep passes did
	// not review this head, and why. It is what the run record persists and
	// what the worker status text and the PR summary comment both render, so
	// the three never disagree about coverage.
	FailedPasses []PassFailure
	// TotalPasses is the number of deep passes attempted.
	TotalPasses int
	// CompletedPasses is how many of them actually reviewed the head.
	CompletedPasses int
	// Status is the run's three-way outcome: complete (every pass reviewed the
	// head), partial (some did), failed (none did — in which case Review
	// returns an error rather than a result).
	Status RunStatus
	// ElidedFiles names the files the generated-file filter dropped before the
	// diff reached any pass, and ElidedBytes is what they weighed in the
	// unfiltered diff. Reported so an operator can see the filter working —
	// a filter nobody can observe is one nobody notices has stopped matching.
	ElidedFiles []string
	ElidedBytes int
}

// StatusText renders the run's one-line status, e.g.
// "partial: 3 of 5 passes completed (failed: logic — error_max_turns)".
func (r *ReviewResult) StatusText() string {
	return RenderStatusText(r.Status, r.CompletedPasses, r.TotalPasses, r.FailedPasses)
}

// PassTelemetryText renders the per-pass turn telemetry as one line, e.g.
// "pass=triage turns=3 term=success, pass=logic turns=12 term=error_max_turns
// retry=1". It is what the daemon adds to the Assay log line so a turn budget
// can be tuned from logged runs rather than guessed at.
func (r *ReviewResult) PassTelemetryText() string {
	return RenderPassTelemetry(r.Passes)
}

// RunError is the error Review returns once a run has spent money: it carries
// the cost billed up to the point the run failed, so a run that produces no
// result still reaches cost tracking.
//
// This is the run-level counterpart of PassError.CostUSD, and it exists for the
// same reason: a failure is not a refund. The provider bills a triage session
// that ended on error_max_turns — the subtype that by definition burned the
// whole turn budget — exactly like one that answered, and a run whose every
// deep pass failed has paid for six full sessions. Without this the daemon's
// error path recorded zero cost for both, silently shrinking the numerator of
// the daily spend that `daily_cost_limit` enforces.
//
// Error() reproduces the wrapped message verbatim and Unwrap exposes the cause,
// so callers that only log or errors.As on the underlying error are unaffected.
type RunError struct {
	// CostUSD is what the run had been billed when it failed.
	CostUSD float64
	// Err is the underlying failure.
	Err error
}

func (e *RunError) Error() string { return e.Err.Error() }

func (e *RunError) Unwrap() error { return e.Err }

// RunCost reports the cost carried by an error from Review. An error raised
// before any provider session ran (a malformed request, an unbuildable prompt)
// carries none and reports 0, as does any error this package did not build —
// an undercount is the safe direction for a number feeding a spend limit, a
// fabricated one is not.
func RunCost(err error) float64 {
	var re *RunError
	if errors.As(err, &re) {
		return re.CostUSD
	}
	return 0
}

// Review runs the multi-pass Assay review for req and returns the aggregated
// result. db may be nil to skip all persistence and posted-Nit suppression
// (useful for dry runs); when non-nil, findings are written to pr_findings
// (OR IGNORE, so the call is idempotent per HeadSHA).
//
// A pass that cannot produce parseable JSON after one retry is surfaced as a
// run error and aborts the whole review.
func Review(ctx context.Context, req ReviewRequest, db *state.DB, cfg Config) (*ReviewResult, error) {
	start := time.Now()

	runner := cfg.runner
	if runner == nil {
		if req.WorkDir == "" {
			return nil, fmt.Errorf("assay: ReviewRequest.WorkDir is required when using the default Smith-based runner")
		}
		runner = newSmithRunner(cfg, req)
	}

	// Pick up the anvil's REVIEW.md once and forward it to every pass.
	// A caller that already set RepoGuidance (e.g. tests, or a future
	// alternate source) keeps full control; otherwise we read from disk.
	if req.RepoGuidance == "" {
		req.RepoGuidance = loadRepoGuidance(req.AnvilPath)
	}

	// Pre-shape the diff: drop auto-generated and skip-path hunks, then cap
	// size. These filters are shared with the Warden so behaviour is identical.
	patterns := append(append([]string{}, cfg.autoGenPatterns()...), cfg.SkipPaths...)
	filtered, elided := diff.FilterAutoGenerated(req.Diff, patterns)
	// What the filter removed is measured here, before the truncation cap, so
	// the number describes the elision and not the cap that may follow it.
	elidedBytes := len(req.Diff) - len(filtered)
	req.ElidedFiles = elided
	filtered = diff.Truncate(filtered, cfg.maxDiffBytes())

	// Prior findings from earlier reviews of this PR — resolved ones included —
	// feed three things: the already-reported list every pass prompt carries,
	// cross-run similarity suppression, and the cumulative per-PR budgets. They
	// are loaded before any session runs so the prompts can name them; an error
	// here has billed nothing, so it returns plain rather than as a RunError.
	var prior []state.Finding
	if db != nil {
		var ferr error
		prior, ferr = db.AllFindings(req.Anvil, req.PRNumber)
		if ferr != nil {
			return nil, fmt.Errorf("assay: querying prior findings: %w", ferr)
		}
	}
	if len(req.PriorFindings) == 0 {
		req.PriorFindings = priorFindingsFromState(prior)
	}
	priorActive, priorActiveNits := 0, 0
	for _, p := range prior {
		if p.ResolvedAt == nil {
			priorActive++
			if p.Severity == string(SeverityNit) {
				priorActiveNits++
			}
		}
	}

	var (
		passes    []PassReport
		totalCost float64
	)

	// fail wraps an error raised after sessions have run so the run's spend to
	// that point survives the nil result — see RunError.
	fail := func(err error) (*ReviewResult, error) {
		return nil, &RunError{CostUSD: totalCost, Err: err}
	}

	// 1. Triage — scope which files warrant deeper review. Its cost is banked
	// before the error check: runTriage reports what its sessions cost whether
	// or not they answered, and a triage failure aborts the run, so this is the
	// only place that spend can be attributed.
	triageRes, err := runTriage(ctx, runner, cfg, req, filtered)
	totalCost += triageRes.cost
	if err != nil {
		return fail(err)
	}
	triage := triageRes.result
	// Always one attempt: triage gets no turn-budget retry (see runTriage). Its
	// strict-JSON re-prompt, when it needs one, is a second session inside that
	// single attempt — counted in CostUSD, not here.
	passes = append(passes, PassReport{
		Name:                passTriage.Name,
		Findings:            0,
		CostUSD:             triageRes.cost,
		Turns:               triageRes.turns,
		Attempts:            1,
		CacheCreationTokens: triageRes.cacheCreation,
		CacheReadTokens:     triageRes.cacheRead,
	})

	scoped := scopeDiffToFiles(filtered, triage.ReviewFiles)

	// 2. Deep passes — a staggered fan-out. A pass that exhausts its turn
	// budget is re-run once inside runDeepPass; only the final attempt's
	// outcome reaches this loop, so a pass that recovers on its retry is an
	// ordinary success here and never reaches PassErrors.
	//
	// The stagger is the scheduling half of the shared-prefix work
	// buildPassPrompt does. All five prompts now open with the same bytes —
	// guidance, context, prior findings and the whole scoped diff — but a
	// simultaneous launch turns that into five simultaneous cache MISSES: on
	// one observed PR all five passes started in the same millisecond and every
	// one of them paid to write the identical prefix. So one primer pass runs
	// alone until the provider starts answering it, by which point the prefix
	// is in the cache, and only then are the other four released together.
	//
	// The primer is a deep pass and not triage on purpose: triage is fed the
	// FILTERED diff while these get the SCOPED one, so priming from triage
	// would silently stop working the moment triage narrowed anything — the
	// runs where it was working hardest.
	//
	// The wait costs the run the primer's time to first token and no more. The
	// four released passes still run concurrently with each other AND with the
	// rest of the primer's session, so wall-clock stays bounded by the slowest
	// single pass rather than by any serialisation.
	outcomes := make([]passResult, len(deepPasses))
	var wg sync.WaitGroup
	primed := newGate()
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Open the gate unconditionally when the primer returns, not only on
		// its first token: a primer that fails to spawn, is refused as rate
		// limited, or runs behind a provider that streams no structured events
		// has no token to signal with, and must not hold the other four for the
		// whole fallback wait.
		defer primed.open()
		p := deepPasses[primerPass]
		outcomes[primerPass] = runDeepPass(withFirstOutput(ctx, primed.open), runner, cfg, req, scoped, triage.Notes, p)
	}()
	primed.wait(ctx, cfg.primerWait())
	for i, p := range deepPasses {
		if i == primerPass {
			continue
		}
		wg.Add(1)
		go func(i int, p passDef) {
			defer wg.Done()
			outcomes[i] = runDeepPass(ctx, runner, cfg, req, scoped, triage.Notes, p)
		}(i, p)
	}
	wg.Wait()

	var all []Finding
	var passErrors []string
	var failedPasses []PassFailure
	for i, o := range outcomes {
		// Count cost regardless of success — the model ran either way, and a
		// retried pass ran twice.
		totalCost += o.cost
		passes = append(passes, PassReport{
			Name:                deepPasses[i].Name,
			Findings:            len(o.findings),
			CostUSD:             o.cost,
			Turns:               o.turns,
			TerminationReason:   o.failure.Reason,
			Attempts:            o.attempts,
			Retried:             o.retried,
			CacheCreationTokens: o.cacheCreation,
			CacheReadTokens:     o.cacheRead,
			Primer:              i == primerPass,
		})
		if o.err != nil {
			passErrors = append(passErrors, o.err.Error())
			// runDeepPass already classified the final attempt's error; reusing
			// its PassFailure keeps one derivation rather than two that could
			// drift if classification ever becomes attempt-sensitive.
			failedPasses = append(failedPasses, o.failure)
			continue
		}
		all = append(all, o.findings...)
	}
	// The pass tally is computed once, here, and carried on the result. Nothing
	// downstream re-derives coverage from the pass-error count.
	status := DeriveStatus(len(deepPasses), failedPasses)
	completedPasses := len(deepPasses) - len(failedPasses)

	// Only hard-fail when every deep pass errored. A single pass hitting
	// error_max_turns or a transient rate-limit must not throw away findings
	// from the other four passes. Partial runs come back as a result whose
	// Status is RunStatusPartial, with the missing passes named in
	// FailedPasses and recorded on the run record by the caller.
	if status == RunStatusFailed {
		return fail(fmt.Errorf("all assay deep passes failed: %s", strings.Join(passErrors, "; ")))
	}

	// 3. Aggregate: hash-dedupe → collapse near-duplicates across passes →
	// suppress near-duplicates of prior findings → suppress already-posted
	// Nits → apply the cumulative Nit budget → apply the cumulative total
	// budget.
	deduped := dedupeByHash(all)
	// Same-anchor near-duplicates from different passes (e.g. tests-missing
	// and logic each flagging the same untested code path with different
	// category labels) bypass hash dedup because Hash includes category.
	// Collapse them by body similarity so the operator sees one finding per
	// concern, not two.
	deduped = dedupeBySimilarity(deduped)
	// Cross-run dedup: on a repeat review of the same PR the model often
	// regenerates the same concern with slightly different wording, which
	// canonicalizes to a different Hash and would otherwise INSERT as a
	// fresh row. Drop new findings whose body overlaps highly with a prior
	// finding at the same or a nearby anchor — resolved findings included: a
	// resolved thread's concern regenerating as a reworded "new" finding is
	// exactly the resurrection this pass exists to stop.
	if len(prior) > 0 {
		existing := make([]ExistingFinding, 0, len(prior))
		for _, r := range prior {
			existing = append(existing, ExistingFinding{Anchor: r.Anchor, Body: r.Body, Category: r.Category})
		}
		deduped = suppressSimilarToExisting(deduped, existing)
	}

	var posted map[string]bool
	if db != nil {
		posted, err = db.PostedFindingHashes(req.Anvil, req.PRNumber)
		if err != nil {
			return fail(fmt.Errorf("assay: querying posted findings: %w", err))
		}
	}
	suppressed, nSuppressed := suppressPostedNits(deduped, posted)
	// Both caps are cumulative across the PR's whole review history, not
	// per-run: what a prior run already contributed (and has not been
	// resolved) is subtracted from this run's budget. Per-run caps were how N
	// passes came to mean N×nit_cap posted Nits.
	nitBudget := -1
	if cfg.NitCap > 0 {
		nitBudget = max(0, cfg.NitCap-priorActiveNits)
	}
	capped, nCapped := capNitsBudget(suppressed, nitBudget)
	totalBudget := -1
	if cfg.MaxFindingsPerPR > 0 {
		totalBudget = max(0, cfg.MaxFindingsPerPR-priorActive)
	}
	capped, nTotalCapped := capTotalFindings(capped, totalBudget)

	// 4. Persist findings (idempotent per HeadSHA via OR IGNORE on the hash).
	if db != nil {
		if err := persistFindings(db, req, capped); err != nil {
			return fail(err)
		}
	}

	return &ReviewResult{
		Findings:        capped,
		HeadSHA:         req.HeadSHA,
		ShadowMode:      cfg.ShadowMode,
		CostUSD:         totalCost,
		Duration:        time.Since(start),
		Passes:          passes,
		NitsCapped:      nCapped,
		NitsSuppressed:  nSuppressed,
		TotalCapped:     nTotalCapped,
		PassErrors:      passErrors,
		FailedPasses:    failedPasses,
		TotalPasses:     len(deepPasses),
		CompletedPasses: completedPasses,
		Status:          status,
		ElidedFiles:     elided,
		ElidedBytes:     elidedBytes,
	}, nil
}

// priorFindingsFromState projects persisted findings onto the narrow
// PriorFinding shape the pass prompts consume.
func priorFindingsFromState(prior []state.Finding) []PriorFinding {
	if len(prior) == 0 {
		return nil
	}
	out := make([]PriorFinding, 0, len(prior))
	for _, p := range prior {
		out = append(out, PriorFinding{
			Anchor:   p.Anchor,
			Severity: p.Severity,
			Title:    p.Title,
			Resolved: p.ResolvedAt != nil,
		})
	}
	return out
}

// persistFindings writes each finding to pr_findings. Insertion is OR IGNORE on
// the finding hash, so repeat reviews of the same head are no-ops.
func persistFindings(db *state.DB, req ReviewRequest, findings []Finding) error {
	for _, f := range findings {
		row := state.Finding{
			Anvil:       req.Anvil,
			PRNumber:    req.PRNumber,
			HeadSHA:     req.HeadSHA,
			FindingHash: f.Hash,
			File:        f.File,
			Anchor:      f.Anchor,
			Severity:    string(f.Severity),
			Category:    f.Category,
			Title:       f.Title,
			Body:        f.Body,
			Evidence:    f.Evidence,
			SourcePass:  f.SourcePass,
			Posted:      false,
		}
		if err := db.InsertFinding(row); err != nil {
			return fmt.Errorf("assay: inserting finding %q: %w", f.Hash, err)
		}
	}
	return nil
}
