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

	"github.com/Robin831/Forge/internal/cost"
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
	// LogKey ties every session log this run writes back to the run record:
	// each pass writes assay-<LogKey>-<pass>-<ts>-<seq>.log, and the caller
	// persists the same key on the assay_runs row. It is what lets the bead
	// Logs panel render one run as one row rather than six loose sessions.
	// Optional and all-digits by convention (see PassLogPrefix); empty means
	// the sessions are still named by pass but are not grouped into a run.
	// Ignored when a custom runner is installed.
	LogKey string
	// Incremental marks a delta review: Diff carries only the changes pushed
	// since the last reviewed commit (BaselineSHA), not the whole base..head
	// diff. Every pass prompt then says so explicitly — the model on a repeat
	// pass otherwise believes it is seeing the PR for the first time and
	// re-derives findings for code it already commented on.
	Incremental bool
	// BaselineSHA is the previously reviewed commit an incremental Diff is
	// relative to. Informational (named in the prompt); empty is fine.
	BaselineSHA string
	// elided names the files whose hunks were dropped from Diff before the
	// review, split by which filter dropped them. Every pass prompt names it:
	// without the note a lockfile-only PR reads to a pass as an empty one, and
	// an operator has no way to see the filter working at all.
	//
	// It is unexported, unlike RepoGuidance and PriorFindings beside it, and
	// that is the difference in kind rather than an oversight: those two are
	// "supply it or Review() will", while this one has to describe the diff the
	// passes were actually handed, so it is derived where the filtering happens
	// and nowhere else. An exported field Review() always overwrote would
	// advertise an input that is never read as one.
	elided elidedFiles
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
// The telemetry half (Turns/TerminationReason/Attempts/Retried, plus
// ToolCalls/FilesRead) exists so the turn budget can be tuned against real
// runs: a pass that ends on error_max_turns having burned the whole budget on a
// nine-line diff is exploring, not reading a large change, and only the
// per-pass numbers say which of the two happened. The two accumulation
// conventions in that half are deliberate and differ per field — Turns is the
// final session's, CostUSD and ToolCalls are summed over sessions, FilesRead is
// the size of their deduplicated union — so each field says which it uses.
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
	// EstCostUSD is costTracker's running estimate of that same spend, summed
	// over the same sessions so the two are the same fold of the same work.
	//
	// It is not a redundant second copy of CostUSD, and the difference is the
	// whole reason it is here: assay.max_cost_per_pass_usd is compared against
	// THIS quantity and never against the provider's total_cost_usd, and the
	// two differ by a structural factor, because a message's usage block is
	// stamped when the message starts, so its output_tokens reads 2 or 3
	// however much the model then writes. A ceiling sized by reading CostUSD
	// off a log is therefore that much more permissive than it looks; the
	// measured factor is in docs/assay-turn-budget.md, which holds it once so a
	// re-measurement does not have to be chased through four comments. Before
	// this field existed the ceiling could not
	// be sized from the daemon log at all: the tracker had to be reproduced by
	// hand over the preserved session logs, and the sessions the ceiling
	// actually killed were absent from that reproduction by construction, since
	// they emit no result event (see docs/assay-turn-budget.md).
	//
	// Zero when no ceiling is configured — a disabled tracker accumulates
	// nothing — and zero for a backend that streams no per-turn usage, which
	// are exactly the two cases in which the ceiling could never fire.
	// RenderPassTelemetry omits the field there rather than printing a zero
	// that would read as a free pass.
	//
	// For the near-universal single-session pass this IS the figure the ceiling
	// compared against. A pass that took a strict-JSON re-prompt or a
	// turn-budget retry reports the sum, which is an upper bound on any one of
	// its sessions; Attempts and Retried are what say a pass is in that case.
	EstCostUSD float64
	// Turns is the turn count of the session the pass recorded — the final
	// one, not the sum, so the number stays comparable to the --max-turns
	// budget a single session is given. It is counted in model messages, which
	// is the unit that budget is written in; see turnCounter for why the
	// provider's own num_turns is not.
	Turns int
	// ToolCalls is how many tool calls the pass made, SUMMED over every
	// provider session it took — a strict-JSON re-prompt and a turn-budget
	// retry alike, and failed sessions as well as successful ones. That is the
	// CostUSD convention rather than the Turns one, because what it measures is
	// how much this pass explored, and exploration a re-prompt or a retry paid
	// for was still exploration.
	//
	// Both fields exist because Turns is a weak proxy for the question that
	// matters, which is whether a pass read any code at all. A pass that
	// answers in one or two turns emitted its JSON without calling a tool: it
	// treated the diff as the whole world, which for the security and
	// repo-specific passes was the majority outcome and cost real findings — an
	// endpoint missing the permission filter its siblings apply, or an
	// unsynchronized cache reached from a parallel loop in another file, is
	// invisible in a diff and obvious two files away. Turns cannot separate
	// that from a pass that read three files and answered; ToolCalls=0 says it
	// outright.
	//
	// Zero where the count could be established and was genuinely nil, and zero
	// again for a backend that reports no tool telemetry at all — the two are
	// one value on a single pass, which is why RenderPassTelemetry decides
	// between them at the level of the RUN (see there).
	ToolCalls int
	// FilesRead is how many DISTINCT files those sessions opened between them:
	// the size of the deduplicated union, not a sum, so two sessions that each
	// read the same three files report 3 rather than 6 (runDeepPass merges the
	// lists with mergeOpenedFiles and reports len of the union; runPassAttempt
	// and runTriage merge their re-prompt's list the same way). It is the
	// cumulative field ToolCalls is — every session of the pass contributes —
	// and only the fold differs, because "how many files did this pass read"
	// is a question about a set.
	//
	// Zero for a backend that names no file in its tool events, which includes
	// one that reports a tool-call COUNT but no per-call file path: ToolCalls
	// can be non-zero here while this stays 0.
	FilesRead int
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
	// RetrySkipped reports that the pass hit error_max_turns, was eligible for
	// that re-run, and did not get one because no modified inputs could be
	// constructed — so the run reports partial coverage instead of paying a
	// second full price for a request already asked (see runDeepPass). It is
	// never true alongside Retried.
	RetrySkipped bool
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
	// Provider is the kind of backend that ran the pass ("claude", "copilot",
	// "gemini"), resolved from the Config's per-tier hints exactly as the
	// session itself was.
	//
	// It is here for one reason: what a zero in ToolCalls/FilesRead MEANS is a
	// property of the backend, not of the pass or of the run. A run is not one
	// provider — triage resolves its own from assay.triage_provider and falls
	// back to the review provider only when that is empty — so with
	// triage_provider: claude and review_provider: copilot, triage streams
	// tool_use blocks Forge can count while the five deep passes stream plain
	// text carrying no tool telemetry at all. Reading triage's non-zero count
	// as proof that the whole run's zeros are measurements would render
	// tools=0 for all five, which says they answered from diff text alone: the
	// exact false signal the counter exists to report truthfully.
	// RenderPassTelemetry therefore groups by this field.
	//
	// Empty for a PassReport assembled without one, which groups all such
	// passes together — the run-level reading, which is right whenever a run
	// does turn out to be one provider.
	Provider string
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
	// CacheCreationTokens and CacheReadTokens are the run's prompt-cache
	// accounting: what every pass together paid to write prefixes into the
	// provider's cache, and what they served from prefixes already there.
	// They are accumulated exactly like CostUSD — over every provider session
	// the run made, failed sessions included, since a session that died after
	// writing its prefix was still billed for it.
	//
	// The run-level pair is what makes the saving measurable ACROSS runs,
	// which the per-pass pair in Passes cannot show: the shared-prefix
	// ordering means one pass writes the prefix and the other four read it
	// within a run, and the stable-prefix ordering (see stablePrefix) means a
	// SECOND run of the same PR finds that opening already cached and reads it
	// rather than writing it again. Persisted on assay_runs by the daemon, so
	// the trend is a query rather than a re-reading of old log lines.
	CacheCreationTokens int
	CacheReadTokens     int
	// Usage is the run's whole token accounting — input, output and the
	// provider's prompt-cache read/write counts, summed over every session of
	// every pass, failed sessions included. It is what the daily and per-bead
	// cost tables record. Its cost half is the same number as CostUSD and its
	// cache halves the same as the pair above, which stay their own fields
	// because callers render and persist them directly.
	Usage cost.Usage
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
	// SkippedReason is non-empty on a run that dispatched no passes at all
	// because the filtered diff had nothing to review (see shouldSkip). Such a
	// run is Complete rather than Failed on purpose: nothing went wrong, the
	// head has been considered and does not need reviewing again, and the run
	// must not be retried — which is what Failed would mean to the daemon's
	// LastReviewedSHA gate. This field is the whole difference between such a
	// run and one that reviewed the diff and found nothing, so every surface
	// that reports a run reads it: the assay_runs row, the PR summary line and
	// the activity feed's terminal event.
	SkippedReason string
	// ElidedFiles names the files the built-in generated-file filter dropped
	// before the diff reached any pass, and ElidedBytes is what they weighed in
	// the unfiltered diff. Reported so an operator can see that filter working
	// — a filter nobody can observe is one nobody notices has stopped matching.
	//
	// SkippedFiles/SkippedBytes are the same two numbers for this anvil's own
	// assay.skip_paths. They are kept apart from the pair above because they
	// answer a different question: "is the built-in lockfile list still
	// matching anything" is the silent failure this reporting exists for, and
	// a repo with a broad skip_paths would drown it in a combined count.
	ElidedFiles  []string
	ElidedBytes  int
	SkippedFiles []string
	SkippedBytes int
}

// StatusText renders the run's one-line status, e.g.
// "partial: 3 of 5 passes completed (failed: logic — error_max_turns)".
//
// A skipped run reports the skip instead of its pass tally: rendering it
// through the tally would read "complete: 0 of 0 passes completed", which is
// the one description of a skip that sounds like a review.
func (r *ReviewResult) StatusText() string {
	if r.SkippedReason != "" {
		return "skipped: " + r.SkippedReason
	}
	return RenderStatusText(r.Status, r.CompletedPasses, r.TotalPasses, r.FailedPasses)
}

// PassTelemetryText renders the per-pass turn telemetry as one line, e.g.
// "pass=triage turns=3 term=success, pass=logic turns=12 term=error_max_turns
// retry=1". It is what the daemon adds to the Assay log line so a turn budget
// can be tuned from logged runs rather than guessed at.
func (r *ReviewResult) PassTelemetryText() string {
	return RenderPassTelemetry(r.Passes)
}

// RedundantCacheWriteTokens is the headline number the shared-prefix ordering
// and the staggered fan-out are measured by: the cache-write tokens this run
// paid for MORE THAN ONCE.
//
// One write of the shared prefix is the intended cost — somebody has to put it
// in the cache — so the redundancy is the sum of every pass's write minus the
// largest single one. When the ordering and the stagger are both working, that
// is the sum of four small per-pass instruction blocks. When either breaks,
// every pass writes the whole diff again and this jumps by roughly (n-1) diffs,
// which is the 75.6%-of-all-write-tokens state buildPassPrompt measured.
//
// Returns 0 for a backend that reports no cache accounting at all, which is
// indistinguishable here from a perfectly shared run — the honest reading is
// "nothing to report", and RenderPassTelemetry omits the per-pass fields in
// exactly the same case.
func (r *ReviewResult) RedundantCacheWriteTokens() int {
	total, largest := 0, 0
	for _, p := range r.Passes {
		total += p.CacheCreationTokens
		largest = max(largest, p.CacheCreationTokens)
	}
	return total - largest
}

// RunError is the error Review returns once a run has spent money: it carries
// the cost billed up to the point the run failed, so a run that produces no
// result still reaches cost tracking.
//
// This is the run-level counterpart of PassError's own telemetry, and it exists
// for the same reason: a failure is not a refund. The provider bills a triage session
// that ended on error_max_turns — the subtype that by definition burned the
// whole turn budget — exactly like one that answered, and a run whose every
// deep pass failed has paid for six full sessions. Without this the daemon's
// error path recorded zero cost for both, silently shrinking the numerator of
// the daily spend that `daily_cost_limit` enforces.
//
// Error() reproduces the wrapped message verbatim and Unwrap exposes the cause,
// so callers that only log or errors.As on the underlying error are unaffected.
type RunError struct {
	// Usage is everything the run had been billed when it failed — tokens,
	// prompt-cache counts and the dollars behind them — so a run that dies
	// still records its cache columns and not just its cost. It is the one
	// field: a separate CostUSD would be a second copy of
	// Usage.EstimatedCostUSD that has to be kept in step with it, and RunCost
	// and RunCacheTokens read it back through RunUsage rather than from fields
	// of their own.
	Usage cost.Usage
	// Err is the underlying failure.
	Err error
}

func (e *RunError) Error() string { return e.Err.Error() }

func (e *RunError) Unwrap() error { return e.Err }

// RunUsage reports the accounting carried by an error from Review: the tokens,
// prompt-cache counts and dollars a failed run had been billed, for the cost
// tables that record all of them together. An error raised before any provider
// session ran (a malformed request, an unbuildable prompt) carries none and
// reports the zero usage, as does any error this package did not build — an
// undercount is the safe direction for numbers feeding a spend limit, a
// fabricated one is not.
func RunUsage(err error) cost.Usage {
	var re *RunError
	if errors.As(err, &re) {
		return re.Usage
	}
	return cost.Usage{}
}

// RunCost is the dollars half of RunUsage, for the callers that record only the
// figure. It delegates rather than reading a field of its own, so the two can
// never report different things for the same error.
func RunCost(err error) float64 { return RunUsage(err).EstimatedCostUSD }

// RunCacheTokens is the prompt-cache half of RunUsage, for the callers that
// persist the two columns. It delegates for the same reason RunCost does: one
// reading of the error, so the accessors cannot disagree about it.
func RunCacheTokens(err error) (creation, read int) {
	u := RunUsage(err)
	return u.CacheWriteTokens, u.CacheReadTokens
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
	// size. The generated-file list is shared with the Warden so behaviour is
	// identical there.
	//
	// The two filters run separately rather than over one concatenated pattern
	// list. They mean different things and the passes are told so: the first
	// list is Forge's own definition of a machine-written file, the second is
	// whatever this anvil chose not to have reviewed — which may be perfectly
	// hand-written prose. Merging them would tell every pass that an anvil's
	// "docs/**" is generated, and would merge the observability too, hiding
	// the one number that says whether the lockfile globs still match.
	filtered, elided := diff.FilterAutoGenerated(req.Diff, cfg.autoGenPatterns())
	// What each filter removed is measured here, before the truncation cap, so
	// the numbers describe the elision and not the cap that may follow it.
	elidedBytes := len(req.Diff) - len(filtered)
	beforeSkip := len(filtered)
	filtered, skipped := diff.FilterAutoGenerated(filtered, cfg.SkipPaths)
	skippedBytes := beforeSkip - len(filtered)
	req.elided = elidedFiles{generated: elided, skipped: skipped}
	filtered = diff.Truncate(filtered, cfg.maxDiffBytes())

	// Nothing left to review — return before a single session is spawned.
	//
	// This is the cheapest possible outcome and the one the old code paid the
	// most for: a push of nothing but lockfiles arrived here as a diff whose
	// every block the filter had just removed, and the run went on to spend a
	// triage session and five deep sessions asking five models to review it.
	// The result still carries the elision counts, so the log line says what
	// the diff consisted of rather than merely that it was empty.
	if reason, skip := shouldSkip(filtered); skip {
		return &ReviewResult{
			HeadSHA:       req.HeadSHA,
			ShadowMode:    cfg.ShadowMode,
			Status:        RunStatusComplete,
			SkippedReason: reason,
			Duration:      time.Since(start),
			ElidedFiles:   elided,
			ElidedBytes:   elidedBytes,
			SkippedFiles:  skipped,
			SkippedBytes:  skippedBytes,
		}, nil
	}

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
		passes []PassReport
		// totalUsage is the run's whole token accounting — every session of
		// every pass, failed ones included, banked as each pass reports. It is
		// what reaches the daily and per-bead cost tables, and it is the one
		// accumulator the run-level cost and cache-token fields are both read
		// off, so they cannot drift apart. A pass that wrote the prefix and
		// then died was billed for the write, so its usage is banked too.
		totalUsage cost.Usage
	)

	// fail wraps an error raised after sessions have run so the run's spend to
	// that point survives the nil result — see RunError.
	fail := func(err error) (*ReviewResult, error) {
		return nil, &RunError{Usage: totalUsage, Err: err}
	}

	// 1. Triage — scope which files warrant deeper review. Its cost is banked
	// before the error check: runTriage reports what its sessions cost whether
	// or not they answered, and a triage failure aborts the run, so this is the
	// only place that spend can be attributed.
	triageRes, err := runTriage(ctx, runner, cfg, req, filtered)
	totalUsage.Add(triageRes.usage)
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
		CostUSD:             triageRes.usage.EstimatedCostUSD,
		EstCostUSD:          triageRes.estCostUSD,
		Turns:               triageRes.turns,
		ToolCalls:           triageRes.toolCalls,
		FilesRead:           triageRes.filesRead,
		Attempts:            1,
		CacheCreationTokens: triageRes.usage.CacheWriteTokens,
		CacheReadTokens:     triageRes.usage.CacheReadTokens,
		Provider:            string(cfg.providerFor(passTriage.Tier).Kind),
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
		// retried pass ran twice. Its cache accounting is banked on the same
		// terms.
		totalUsage.Add(o.usage)
		passes = append(passes, PassReport{
			Name:                deepPasses[i].Name,
			Findings:            len(o.findings),
			CostUSD:             o.usage.EstimatedCostUSD,
			EstCostUSD:          o.estCostUSD,
			Turns:               o.turns,
			ToolCalls:           o.toolCalls,
			FilesRead:           o.filesRead,
			TerminationReason:   o.failure.Reason,
			Attempts:            o.attempts,
			Retried:             o.retried,
			RetrySkipped:        o.retrySkipped,
			CacheCreationTokens: o.usage.CacheWriteTokens,
			CacheReadTokens:     o.usage.CacheReadTokens,
			Primer:              i == primerPass,
			Provider:            string(cfg.providerFor(deepPasses[i].Tier).Kind),
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
		Findings:            capped,
		HeadSHA:             req.HeadSHA,
		ShadowMode:          cfg.ShadowMode,
		CostUSD:             totalUsage.EstimatedCostUSD,
		CacheCreationTokens: totalUsage.CacheWriteTokens,
		CacheReadTokens:     totalUsage.CacheReadTokens,
		Usage:               totalUsage,
		Duration:            time.Since(start),
		Passes:              passes,
		NitsCapped:          nCapped,
		NitsSuppressed:      nSuppressed,
		TotalCapped:         nTotalCapped,
		PassErrors:          passErrors,
		FailedPasses:        failedPasses,
		TotalPasses:         len(deepPasses),
		CompletedPasses:     completedPasses,
		Status:              status,
		ElidedFiles:         elided,
		ElidedBytes:         elidedBytes,
		SkippedFiles:        skipped,
		SkippedBytes:        skippedBytes,
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
