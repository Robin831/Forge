// Package assay implements Forge's AI pull-request review engine ("Assay").
//
// Review runs a multi-pass review over a PR diff: a cheap Triage pass scopes
// which files warrant deeper inspection, then five deep passes (Logic,
// Security, Conventions, Tests-missing, Repo-specific) run in parallel and emit
// structured findings. Findings are then aggregated in order: deduplicated by a
// stable content hash, then (on a repeat review of the same PR) already-posted
// Nits are suppressed, then the remaining Nits are capped at the configured Nit
// budget.
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
	// OnPassLog, when set, is called with the log path of each pass as it is
	// spawned — triage first, then the deep passes concurrently. The daemon
	// uses the first call to point the Assay worker row's log_path at a file
	// the Hearth live panel can stream; without it the panel renders an
	// endlessly empty transcript for the whole run. Ignored when a custom
	// runner is installed. Implementations must be safe for concurrent use.
	OnPassLog func(logPath string)
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
	// session it took.
	CostUSD float64
	// Turns is the turn count of the session the pass recorded — the final
	// one, not the sum, so the number stays comparable to the --max-turns
	// budget a single session is given.
	Turns int
	// TerminationReason is how the pass ended: "" when it answered, else the
	// same label FailedPasses carries (a provider result subtype where there
	// is one, e.g. "error_max_turns").
	TerminationReason string
	// Attempts is how many sessions the pass took — 2 when it was retried
	// after exhausting its turn budget.
	Attempts int
	// Retried reports whether the pass was re-run in a fresh session after
	// hitting error_max_turns.
	Retried bool
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
	filtered, _ := diff.FilterAutoGenerated(req.Diff, patterns)
	filtered = diff.Truncate(filtered, cfg.maxDiffBytes())

	var (
		passes    []PassReport
		totalCost float64
	)

	// 1. Triage — scope which files warrant deeper review.
	triage, triageCost, triageTurns, err := runTriage(ctx, runner, cfg, req, filtered)
	if err != nil {
		return nil, err
	}
	totalCost += triageCost
	passes = append(passes, PassReport{
		Name:     passTriage.Name,
		Findings: 0,
		CostUSD:  triageCost,
		Turns:    triageTurns,
		Attempts: 1,
	})

	scoped := scopeDiffToFiles(filtered, triage.ReviewFiles)

	// 2. Deep passes — run concurrently (Smith-style fan-out). A pass that
	// exhausts its turn budget is re-run once inside runDeepPass; only the
	// final attempt's outcome reaches this loop, so a pass that recovers on
	// its retry is an ordinary success here and never reaches PassErrors.
	outcomes := make([]passResult, len(deepPasses))
	var wg sync.WaitGroup
	for i, p := range deepPasses {
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
			Name:              deepPasses[i].Name,
			Findings:          len(o.findings),
			CostUSD:           o.cost,
			Turns:             o.turns,
			TerminationReason: o.reason,
			Attempts:          o.attempts,
			Retried:           o.retried,
		})
		if o.err != nil {
			passErrors = append(passErrors, o.err.Error())
			failedPasses = append(failedPasses, classifyPassError(deepPasses[i].Name, o.err))
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
		return nil, fmt.Errorf("all assay deep passes failed: %s", strings.Join(passErrors, "; "))
	}

	// 3. Aggregate: hash-dedupe → collapse near-duplicates across passes →
	// suppress near-duplicates of already-stored findings → suppress
	// already-posted Nits → cap Nits.
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
	// fresh row. Drop new findings whose body overlaps highly with an
	// already-active (non-resolved) finding at the same anchor.
	if db != nil {
		raw, ferr := db.ActiveFindings(req.Anvil, req.PRNumber)
		if ferr != nil {
			return nil, fmt.Errorf("assay: querying active findings: %w", ferr)
		}
		if len(raw) > 0 {
			existing := make([]ExistingFinding, 0, len(raw))
			for _, r := range raw {
				existing = append(existing, ExistingFinding{Anchor: r.Anchor, Body: r.Body})
			}
			deduped = suppressSimilarToExisting(deduped, existing)
		}
	}

	var posted map[string]bool
	if db != nil {
		posted, err = db.PostedFindingHashes(req.Anvil, req.PRNumber)
		if err != nil {
			return nil, fmt.Errorf("assay: querying posted findings: %w", err)
		}
	}
	suppressed, nSuppressed := suppressPostedNits(deduped, posted)
	capped, nCapped := capNits(suppressed, cfg.NitCap)

	// 4. Persist findings (idempotent per HeadSHA via OR IGNORE on the hash).
	if db != nil {
		if err := persistFindings(db, req, capped); err != nil {
			return nil, err
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
		PassErrors:      passErrors,
		FailedPasses:    failedPasses,
		TotalPasses:     len(deepPasses),
		CompletedPasses: completedPasses,
		Status:          status,
	}, nil
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
