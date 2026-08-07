// Package pipeline orchestrates the full Smith → Temper → Warden → feedback loop.
//
// The pipeline runs a bead through:
//  0. Schematic analysis (optional pre-worker) — may produce a plan, decompose, clarify, or skip
//  1. Smith implementation
//  2. Temper build/test verification
//  3. Warden code review
//  4. If request_changes: re-run Smith with feedback, repeat (up to max iterations)
//  5. Final verdict → done or failed
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Robin831/Forge/internal/changelog"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/notify"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
)

// MaxIterations is the default maximum number of Smith-Warden cycles when no
// value is provided via Params.MaxIterations or the config.
const MaxIterations = 5

// defaultSteerGrace is how long an interrupted Smith spawn is given to exit
// cleanly after SIGINT before it is force-killed during steering (steer mode A).
// It mirrors the daemon's killWorkerProcess grace period.
const defaultSteerGrace = 5 * time.Second

// waitSmithWithSteer waits for a running Smith spawn to complete while watching
// the steer mailbox and the pause signal. If a steer message arrives before the
// spawn finishes, the spawn is gracefully interrupted (via interrupt) and its
// result is reaped with Wait() — preserving the session_id captured from the
// stream so far — and the steering message is returned (paused=false). If a pause
// is signalled instead, the spawn is gracefully interrupted the same way (the
// SAME steer interrupt path, so no failure is marked) and paused=true is returned
// with an empty message so the caller can park the pipeline. When the spawn
// completes on its own (or neither channel is wired) it returns the result with
// an empty message and paused=false.
//
// Race safety: the outer select may fire on steerCh/pauseCh at the very instant
// the spawn also completes (both cases ready → Go picks at random). When that
// happens the channel value has ALREADY been consumed, so it must never be
// silently discarded in favour of "prefer completion" — that is exactly how a
// pause or steer used to be lost while the operator was told it succeeded.
// Instead, once a value is received it is always propagated: the process is
// interrupted only when it is still running (a finished spawn has nothing to
// stop, and a finished session is precisely the mode-B resume case), and the
// received steer/pause is returned regardless.
//
// A pause and a steer are mutually exclusive per spawn: exactly one of them (or
// normal completion) drives the single select, so the pipeline never has to
// reconcile both for the same spawn.
func waitSmithWithSteer(proc *smith.Process, steerCh <-chan string, pauseCh <-chan struct{}, interrupt func(*smith.Process)) (result *smith.Result, steer string, paused bool) {
	if steerCh == nil && pauseCh == nil {
		return proc.Wait(), "", false
	}
	// A nil channel case in a select blocks forever (is never chosen), so an
	// unwired steerCh or pauseCh simply drops out of the select naturally.
	select {
	case <-proc.Done():
		// The spawn finished before any signal was received. Nothing was
		// consumed from steerCh/pauseCh, so a message that is still queued stays
		// in the mailbox for the between-spawns (mode B) drain — never lost.
		return proc.Wait(), "", false
	case msg, ok := <-steerCh:
		if !ok {
			// Channel was closed; treat as no steer.
			return proc.Wait(), "", false
		}
		// The steer has been consumed and MUST be acted on even if the spawn is
		// completing at the same instant. Interrupt only while the process is
		// still running; a finished session is resumed as the mode-B case.
		if proc.IsRunning() {
			interrupt(proc)
		}
		return proc.Wait(), msg, false
	case _, ok := <-pauseCh:
		if !ok {
			// Channel was closed; treat as no pause.
			return proc.Wait(), "", false
		}
		// The pause has been consumed and MUST park the worker even if the spawn
		// finished simultaneously — otherwise the bead would silently complete
		// despite an acknowledged pause. Interrupt only while still running.
		if proc.IsRunning() {
			interrupt(proc)
		}
		return proc.Wait(), "", true
	}
}

// drainSteer performs a single non-blocking receive from the steer mailbox. It
// returns the message and true when one was waiting, or ("", false) when the
// channel is nil, currently empty, or closed and empty. A closed channel with
// buffered messages still delivers them with ok==true — false is only returned
// once the channel is both closed and drained. Used for steer mode B, where a
// message enqueued while Temper/Warden ran is consumed BETWEEN spawns (as
// opposed to waitSmithWithSteer, which drains the same channel DURING a spawn).
func drainSteer(ch <-chan string) (string, bool) {
	if ch == nil {
		return "", false
	}
	select {
	case msg, ok := <-ch:
		if !ok {
			return "", false
		}
		msg = strings.TrimSpace(msg)
		if msg == "" {
			return "", false
		}
		return msg, true
	default:
		return "", false
	}
}

// drainPause performs a single non-blocking receive from the pause signal used
// by the pause/park/resume mechanic. It returns true when a pause was waiting
// (consuming it), or false when the channel is nil, currently empty, or closed.
// It is the between-spawns counterpart to waitSmithWithSteer's pause branch:
// waitSmithWithSteer only observes a pause DURING a spawn, so a pause enqueued
// while Temper/Warden ran (when no spawn is live to interrupt) would otherwise
// never be seen. It is drained at three points: at the loop top (park before the
// next spawn), immediately before a bead completes on Warden approval (park
// instead of completing), and — as a terminal safety net — in Run's deferred
// exit handler, which surfaces a still-pending pause in the event log when the
// pipeline finishes on a non-approval path (failure/escalation) with no further
// turn to honour it. Together these guarantee an acknowledged pause is either
// honoured (the pipeline parks) or explicitly surfaced, never silently dropped.
// The pipeline goroutine is the sole consumer of the pause signal, so this drain
// never races waitSmithWithSteer for the same token.
func drainPause(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case _, ok := <-ch:
		// A closed channel yields ok==false; that is not a pause request.
		return ok
	default:
		return false
	}
}

// mergeSteerWithFeedback combines an operator steer message (mode B) with the
// pending Warden/Temper feedback that would otherwise have driven the next
// Smith iteration. The steer message leads and is marked as taking precedence —
// the operator's intent is the authoritative instruction — while the automated
// feedback follows as additional context to address unless superseded. Either
// side being empty collapses to the other so no empty scaffolding is sent.
func mergeSteerWithFeedback(feedback, steer string) string {
	steer = strings.TrimSpace(steer)
	feedback = strings.TrimSpace(feedback)
	switch {
	case steer == "" && feedback == "":
		return ""
	case feedback == "":
		return steer
	case steer == "":
		return feedback
	}
	return fmt.Sprintf(
		"Operator steering message (address this first, it takes precedence):\n%s\n\n"+
			"The automated verification also requested the following changes; incorporate them as well unless the steering above supersedes them:\n%s",
		steer, feedback)
}

// appendSteerToPrompt folds an operator steer message (mode B) into a fresh
// Smith prompt when there is no session to resume. The steer text is appended
// under a clear header so it is honoured alongside the built prompt.
func appendSteerToPrompt(prompt, steer string) string {
	steer = strings.TrimSpace(steer)
	if steer == "" {
		return prompt
	}
	return prompt + "\n\n---\nOperator steering message (address this first, it takes precedence):\n" + steer
}

// Outcome represents the final result of the pipeline.
type Outcome struct {
	// Success is true if the bead was implemented and approved.
	Success bool
	// Verdict is the final Warden verdict.
	Verdict warden.Verdict
	// Iterations is how many Smith-Warden cycles were run.
	Iterations int
	// SmithResult is the last Smith result.
	SmithResult *smith.Result
	// TemperResult is the last Temper result.
	TemperResult *temper.Result
	// ReviewResult is the last Warden review result.
	ReviewResult *warden.ReviewResult
	// Duration is the total pipeline duration.
	Duration time.Duration
	// WorkerID is the worker ID used.
	WorkerID string
	// Branch is the git branch used.
	Branch string
	// Error is set if the pipeline failed before reaching a verdict.
	Error error
	// RateLimited is true when all providers were rate limited and the bead
	// has been released back to open so the poller can retry later.
	RateLimited bool
	// AuthFailed is true when a provider rejected the credentials. The bead is
	// NOT released for retry (a bad credential fails identically every time);
	// the daemon escalates it for human attention instead (Forge-d5ns).
	AuthFailed bool
	// AuthProvider is the label of the provider that failed authentication.
	// Populated only when AuthFailed is true.
	AuthProvider string
	// NeedsHuman is true when the pipeline has released the bead back to open
	// because it requires human attention (e.g., Smith produced no diff). The
	// current bd call only sets --status=open and does not add a separate
	// needs-human flag.
	NeedsHuman bool
	// SchematicResult is the result of the Schematic pre-worker, if it ran.
	SchematicResult *schematic.Result
	// Decomposed is true when the Schematic decomposed the bead into
	// sub-beads. The pipeline exits early without running Smith.
	Decomposed bool
	// NoChangesNeeded is true when Smith determined that no code changes are
	// required (e.g. the fix is already implemented or resolved upstream).
	NoChangesNeeded bool
	// NoChangesReason is the reason Smith gave for why no changes are needed.
	NoChangesReason string
	// ChangelogSummary is the extracted changelog fragment bullets (if any).
	ChangelogSummary string
	// EmptyDiff is true when the run reached Warden approval but the branch
	// carries no commits relative to its base — the work is already on the base
	// branch (e.g. a sibling PR shipped it first). PR creation is skipped: it
	// would fail with "No commits between <base> and <branch>" and every retry
	// would reproduce the identical empty branch. Success is false, but this is
	// a terminal outcome, not a failure: callers must not schedule a retry or
	// count it against the dispatch circuit breaker.
	EmptyDiff bool
	// EmptyDiffAction is the resolved settings.empty_diff_action for this run
	// (config.EmptyDiffActionClose or config.EmptyDiffActionAttention). Only
	// meaningful when EmptyDiff is true.
	EmptyDiffAction string
	// EmptyDiffBase is the git ref the branch was compared against (e.g.
	// "origin/main"). Only meaningful when EmptyDiff is true.
	EmptyDiffBase string
}

// DiffStat summarises the diff produced by Smith for skip-warden evaluation.
type DiffStat struct {
	LinesChanged         int
	FilesChanged         int
	TouchesSecurityFiles bool
	IsDocsOnly           bool
	IsTestsOnly          bool
	// Valid is true only when the diff stat was successfully computed.
	// A zero-value DiffStat (Valid==false) must never satisfy the skip criteria.
	Valid bool
}

// securityPathPatterns are substrings that flag a file path as security-sensitive.
var securityPathPatterns = []string{
	"auth", "crypto", "permission", "secret", "token",
	"credential", "password", "key", "cert", "acl", "rbac", "policy",
}

// shouldSkipWarden decides whether the Warden review can be auto-approved for
// a small, low-risk Copilot diff. All criteria must be true:
//  1. skipEnabled (copilot_skip_warden_small_diffs config toggle)
//  2. Primary provider is Copilot
//  3. Bead priority is P3 or P4 (>= 3)
//  4. Diff stat was successfully computed (Valid==true) and has at least one changed file
//  5. Diff is 100 lines or fewer changed (≤100)
//  6. No security-sensitive files touched
//  7. Changes are docs-only, tests-only, or at most 2 files
func shouldSkipWarden(diffStat DiffStat, bead poller.Bead, providers []provider.Provider, skipEnabled bool) bool {
	if !skipEnabled {
		return false
	}
	if len(providers) == 0 || providers[0].Kind != provider.Copilot {
		return false
	}
	if bead.Priority <= 2 {
		return false
	}
	// A failed or empty diff stat must never silently skip Warden.
	if !diffStat.Valid || diffStat.FilesChanged == 0 {
		return false
	}
	if diffStat.LinesChanged > 100 {
		return false
	}
	if diffStat.TouchesSecurityFiles {
		return false
	}
	return diffStat.IsDocsOnly || diffStat.IsTestsOnly || diffStat.FilesChanged <= 2
}

// computeDiffStat analyses the git diff in the worktree to produce a DiffStat.
// It shells out to "git diff --numstat" against the base branch and inspects file paths.
func computeDiffStat(worktreePath, baseSHA string) DiffStat {
	if baseSHA == "" {
		baseSHA = "HEAD~1"
	}
	var ds DiffStat

	// Get the list of changed files with line counts.
	numstatCmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "diff", "--numstat", baseSHA))
	numstatOut, err := numstatCmd.Output()
	if err != nil {
		return ds
	}

	allDocs := true
	allTests := true

	for _, line := range strings.Split(strings.TrimSpace(string(numstatOut)), "\n") {
		if line == "" {
			continue
		}
		// --numstat output is tab-delimited: "<added>\t<deleted>\t<path>"
		// For renames/copies the path field looks like "old => new" or "{old => new}/suffix".
		// We split on tabs to correctly handle file paths that contain spaces.
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		ds.FilesChanged++

		// Parse added/deleted lines (binary files show "-")
		var added, deleted int
		if fields[0] != "-" {
			fmt.Sscanf(fields[0], "%d", &added)
		}
		if fields[1] != "-" {
			fmt.Sscanf(fields[1], "%d", &deleted)
		}
		ds.LinesChanged += added + deleted

		// For rename/copy paths like "old => new" or "{a => b}/file", extract the
		// destination path for classification purposes.
		rawPath := fields[2]
		if idx := strings.Index(rawPath, " => "); idx >= 0 {
			rawPath = rawPath[idx+4:]
			// Handle brace notation: "prefix/{old => new}/suffix" — strip any trailing "}"
			rawPath = strings.TrimSuffix(rawPath, "}")
		}
		filePath := strings.ToLower(rawPath)

		// Check security-sensitive paths.
		for _, pat := range securityPathPatterns {
			if strings.Contains(filePath, pat) {
				ds.TouchesSecurityFiles = true
				break
			}
		}

		// Check docs-only: files under docs/ or ending in .md
		isDoc := strings.HasPrefix(filePath, "docs/") || strings.HasSuffix(filePath, ".md")
		if !isDoc {
			allDocs = false
		}

		// Check tests-only: files ending in _test.go, .test.ts, .test.js, .spec.ts, .spec.js,
		// or under a test/ or tests/ directory.
		isTest := strings.HasSuffix(filePath, "_test.go") ||
			strings.HasSuffix(filePath, ".test.ts") ||
			strings.HasSuffix(filePath, ".test.js") ||
			strings.HasSuffix(filePath, ".spec.ts") ||
			strings.HasSuffix(filePath, ".spec.js") ||
			strings.HasPrefix(filePath, "test/") ||
			strings.HasPrefix(filePath, "tests/") ||
			strings.Contains(filePath, "/test/") ||
			strings.Contains(filePath, "/tests/")
		if !isTest {
			allTests = false
		}
	}

	if ds.FilesChanged > 0 {
		ds.IsDocsOnly = allDocs
		ds.IsTestsOnly = allTests
	}
	ds.Valid = true
	return ds
}

// Params holds the dependencies for running a pipeline.
type Params struct {
	DB              *state.DB
	WorktreeManager *worktree.Manager
	PromptBuilder   *prompt.Builder
	AnvilName       string
	AnvilConfig     config.AnvilConfig
	Bead            poller.Bead
	ExtraFlags      []string
	TemperConfig    *temper.Config // nil = auto-detect
	// GoRaceDetection enables a separate 'go test -race' step in Temper.
	// Only used during auto-detection (when TemperConfig is nil).
	GoRaceDetection bool
	// TemperStepTimeout, TemperGitTimeout, and TemperOutputCap carry the
	// temper_step_timeout / temper_git_timeout / temper_output_cap settings so
	// they apply even on the auto-detect path (where TemperConfig is nil).
	// Each is applied only when the resolved config leaves the field unset, so
	// an explicit per-anvil value still wins. Zero values fall back to the
	// temper package defaults.
	TemperStepTimeout time.Duration
	TemperGitTimeout  time.Duration
	TemperOutputCap   int
	// Providers is the ordered list of AI providers to try for the Smith stage.
	// If empty, provider.Defaults() is used (Claude → Gemini).
	Providers []provider.Provider

	// WardenProviders is the provider chain for the Warden review stage.
	// When empty, Providers is used (with WardenModelOverride applied to
	// Copilot entries). When set, WardenModelOverride is ignored.
	WardenProviders []provider.Provider

	// SchematicProviders is the provider chain for the Schematic pre-analysis stage.
	// When empty, Providers is used (with SchematicModelOverride applied to
	// Copilot entries). When set, SchematicModelOverride is ignored.
	SchematicProviders []provider.Provider

	// BaseBranch overrides the base ref for worktree creation and PR
	// targeting. When set (e.g. for epic child beads), the worktree branches
	// from origin/<BaseBranch> and the PR targets this branch instead of the
	// repo default branch (origin/main or origin/master).
	BaseBranch string

	// ResetBranch, when true, instructs worktree creation to hard-reset an
	// existing branch back to the base ref. Set on retries to discard bad
	// commits from a previous failed pipeline run.
	ResetBranch bool

	// SchematicConfig controls the Schematic pre-worker. When nil, Schematic
	// is disabled (the default).
	SchematicConfig *schematic.Config
	// Notifier sends Teams webhook notifications. Nil-safe — calls are no-ops
	// when nil.
	Notifier *notify.Notifier

	// The following fields are optional injection points used in tests.
	// If nil, the default production implementations are used.

	// WorktreeCreator overrides WorktreeManager.Create. Used in tests.
	WorktreeCreator func(ctx context.Context, anvilPath, beadID string) (*worktree.Worktree, error)
	// WorktreeRemover overrides WorktreeManager.Remove. Used in tests.
	WorktreeRemover func(ctx context.Context, anvilPath string, wt *worktree.Worktree)
	// SmithRunner overrides smith.SpawnWithProvider. Used in tests.
	SmithRunner func(ctx context.Context, wtPath, promptText, logDir string, pv provider.Provider, extraFlags []string) (*smith.Process, error)
	// TemperRunner overrides temper.Run. Used in tests.
	TemperRunner func(ctx context.Context, wtPath string, cfg temper.Config, db *state.DB, beadID, anvilName string) *temper.Result
	// WardenReviewer overrides warden.Review. Used in tests.
	WardenReviewer func(ctx context.Context, wtPath, beadID, beadTitle, beadDescription, anvilPath string, db *state.DB, priorFeedback string, providers ...provider.Provider) (*warden.ReviewResult, error)
	// BeadReleaser overrides the default exec-based bd-update call for releasing
	// a bead back to open. Used in tests.
	BeadReleaser func(beadID, anvilPath string) error
	// SchematicRunner overrides schematic.Run. Used in tests.
	SchematicRunner func(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result
	// EmptyDiffChecker overrides hasEmptyDiff. Used in tests to simulate a
	// dirty worktree without depending on a real git repository.
	EmptyDiffChecker func(worktreePath, preSmithSHA string) bool
	// CommitCounter overrides countCommitsAhead, the git rev-list call that
	// decides whether the approved branch actually carries commits against its
	// base. Used in tests to simulate an empty branch without a real repo.
	CommitCounter func(ctx context.Context, worktreePath, base, branch string) (int, error)

	// WorkerID is the pre-generated worker ID to use for the state.db record.
	// When set (e.g. because the daemon inserted a pending worker row at claim
	// time to survive the claim→worktree crash window), the pipeline reuses
	// this ID so the pending row is overwritten by the running row on insert.
	// If empty, the pipeline generates a fresh ID as usual.
	WorkerID string

	// MaxIterations is the maximum number of Smith-Warden cycles before the
	// pipeline gives up. When zero or negative, MaxIterations (the package-level
	// constant, default 5) is used. This value should be populated from
	// config.Settings.MaxPipelineIterations.
	MaxIterations int

	// WardenModelOverride, when non-empty, overrides the Model field for any
	// Copilot provider entry when spawning the Warden review stage. Non-Copilot
	// providers are unaffected. Use to route review to a cheaper model (e.g.
	// claude-haiku-4-5) while keeping Smith on a stronger model.
	WardenModelOverride string

	// SchematicModelOverride, when non-empty, overrides the Model field for any
	// Copilot provider entry when spawning the Schematic pre-analysis stage.
	// Non-Copilot providers are unaffected.
	SchematicModelOverride string

	// EmptyDiffAction carries settings.empty_diff_action into the pipeline so
	// the resolved action travels with the outcome. Empty or unrecognised
	// values resolve to config.EmptyDiffActionAttention. See Outcome.EmptyDiff.
	EmptyDiffAction string

	// CopilotSkipWardenSmallDiffs, when true, allows the pipeline to auto-approve
	// small, low-risk diffs without running Warden when the primary provider is
	// Copilot. This saves one premium request for trivial changes.
	CopilotSkipWardenSmallDiffs bool

	// WardenFullRereview, when true, forces the Warden to do a full independent
	// review on every iteration instead of a focused re-review that only checks
	// whether prior feedback was addressed. Default: false (focused re-review).
	WardenFullRereview bool

	// CopilotCombinedSmithWarden, when true, embeds Warden review criteria
	// into the Smith prompt so Smith self-reviews its own diff. A real Warden
	// is still spawned for P0-P1 beads, when the self-review flags concerns,
	// or via random sampling. Only effective when the primary provider is
	// Copilot.
	CopilotCombinedSmithWarden bool

	// CopilotWardenSampleRate is the probability (0.0–1.0) that a real Warden
	// review is spawned even when the self-review approves. Default: 0.1.
	CopilotWardenSampleRate float64

	// SkipSmith, when true, skips the Schematic pre-worker and the initial
	// Smith run on the first iteration. The pipeline creates a worktree on
	// the existing branch (ResetBranch should be false) and proceeds directly
	// to Temper → Warden → PR. If Temper or Warden request changes on a later
	// iteration, Smith will still run normally on that iteration. Used by
	// force smith to continue the pipeline after smith has already completed
	// separately.
	SkipSmith bool

	// SteerCh, when non-nil, is drained while a Smith spawn is running (steer
	// mode A). A message on it gracefully interrupts the running spawn (SIGINT
	// to the process group + grace period, WITHOUT marking the worker failed),
	// preserves the captured session_id, and resumes the session in the same
	// worktree via the provider --resume flag with the steering message
	// delivered as the new stdin prompt. Each resume counts as one pipeline
	// iteration and is bounded by MaxIterations. Set by the daemon from the
	// bead's control-handle steer mailbox.
	SteerCh <-chan string

	// SteerGrace overrides the grace period given to an interrupted spawn to
	// exit cleanly before it is force-killed. When zero, defaultSteerGrace is
	// used.
	SteerGrace time.Duration

	// ParkHandle, when non-nil, wires the pause/park/resume mechanic into the
	// pipeline goroutine. A pause request (PauseRequested) gracefully interrupts
	// the running Smith spawn via the SAME steer interrupt path — no failure is
	// marked — records a ParkRecord (session_id + iteration state), transitions
	// the worker to paused, and blocks the goroutine on ResumeRequested without
	// exiting the pipeline loop. On resume the recorded session is respawned via
	// the steer resume path (`claude --resume <session>` with the resume
	// message, defaulting to DefaultResumeMessage) and the loop continues from
	// where it parked. Nil disables the mechanic. Set by the daemon from the
	// bead's control handle (see internal/daemon/control.go).
	ParkHandle ParkHandle

	// ShutdownCtx, when non-nil, is watched alongside the pipeline's cancellable
	// base context while a bead is parked so a parked goroutine unblocks promptly
	// on daemon shutdown. Because the park wait no longer rides the smith-timeout
	// context (that would let the timeout trip the pause), the base context alone
	// only carries IPC interrupt; this second edge ensures the shutdown drain does
	// not hang on a parked pipeline. When the shutdown fires while parked, the
	// worker is left paused (not failed) so it can resume after restart. Nil
	// disables the second edge (tests / callers without a daemon context).
	ShutdownCtx context.Context

	// SmithTimeout, when > 0, makes the pipeline own its smith-timeout deadline
	// internally rather than relying on a deadline baked into the passed-in ctx.
	// This is required for the pause/park/resume mechanic: while a bead is parked
	// the smith timeout must be suspended so wall-clock time spent paused does not
	// count against the budget. On each resume the pipeline advances the deadline
	// by the time it spent parked (see extendDeadlineForPause). When 0, the passed
	// ctx governs the deadline unchanged (used by callers that do not park, e.g.
	// crucible child pipelines). The daemon passes a cancellable (deadline-free)
	// ctx plus this field so the interrupt/shutdown cancel still propagates while
	// the timeout remains extendable.
	SmithTimeout time.Duration

	// ResumeSession, when non-nil, re-enters a previously paused bead after a
	// daemon restart: instead of building a fresh Smith prompt and spawning a new
	// session, the pipeline's FIRST iteration resumes the recorded Claude session
	// (SessionID) via the SAME steer respawn path used by the live pause/resume
	// mechanic (`claude --resume <SessionID>` with Message as the prompt). The
	// retained worktree is reused as-is (WorktreeManager reuse with
	// PreserveExisting). This is how daemon-restart recovery honours a Needs
	// Attention "resume" action when the parked pipeline goroutine did not survive
	// the restart. When SessionID is empty (e.g. a provider that never reported
	// one) the Message is folded into a fresh Smith prompt instead. Nil runs the
	// pipeline normally from the top.
	ResumeSession *ResumeSession

	// SmithInterrupter overrides how a running Smith spawn is gracefully
	// stopped during steering. Production calls smith.Process.Interrupt; tests
	// inject a stub to observe interruption without real signals.
	SmithInterrupter func(proc *smith.Process)

	// SpawnLive, when non-nil, is called with true just before the pipeline
	// begins waiting on a running Smith spawn (the window in which a steer
	// message interrupts that spawn — mode A) and false once that wait returns
	// (between spawns / Temper / Warden — mode B). The daemon wires it to the
	// bead's control handle so the IPC/API layer can label an incoming steer as
	// mode A vs mode B from live-spawn state rather than from any pipeline-wide
	// cancel. Nil disables the signal (tests / callers without a control handle).
	SpawnLive func(live bool)

	// SmithResumeRunner overrides the steer-mode resume spawn. Production routes
	// through smith.SpawnWithOptions with the provider --resume flag and a
	// steer-<ts>.log prefix; tests inject a stub. sessionID is the captured
	// session to resume and steerMsg is delivered as the new stdin prompt.
	SmithResumeRunner func(ctx context.Context, wtPath, steerMsg, logDir string, pv provider.Provider, sessionID string, extraFlags []string) (*smith.Process, error)

	// SteerNoteAppender overrides how the full steering message is persisted to
	// the bead's notes (the bead-notes mechanism). Production shells out to
	// `bd update <id> --append-notes`; tests inject a stub to avoid the bd CLI.
	// See recordSteer, which pairs the note with a bead_steered activity event.
	SteerNoteAppender func(beadID, anvilPath, note string) error
}

// wardenProviders returns the provider list for the Warden stage. When
// WardenProviders is explicitly set, it is returned directly. Otherwise the
// base providers are returned with WardenModelOverride applied to Copilot
// entries (legacy behavior).
func (p *Params) wardenProviders(providers []provider.Provider) []provider.Provider {
	if len(p.WardenProviders) > 0 {
		return p.WardenProviders
	}
	if p.WardenModelOverride == "" {
		return providers
	}
	cloned := make([]provider.Provider, len(providers))
	copy(cloned, providers)
	for i, pv := range cloned {
		if pv.Kind == provider.Copilot {
			cloned[i].Model = p.WardenModelOverride
		}
	}
	return cloned
}

// schematicProviders returns the provider list for the Schematic stage. When
// SchematicProviders is explicitly set, it is returned directly. Otherwise the
// base providers are returned with SchematicModelOverride applied to Copilot
// entries (legacy behavior).
func (p *Params) schematicProviders(providers []provider.Provider) []provider.Provider {
	if len(p.SchematicProviders) > 0 {
		return p.SchematicProviders
	}
	if p.SchematicModelOverride == "" {
		return providers
	}
	cloned := make([]provider.Provider, len(providers))
	copy(cloned, providers)
	for i, pv := range cloned {
		if pv.Kind == provider.Copilot {
			cloned[i].Model = p.SchematicModelOverride
		}
	}
	return cloned
}

// countBranchCommits counts the commits the worktree's branch carries against
// the base it will be merged into. It reports ok=false when the answer is
// unknown — the base ref could not be resolved, or git failed — so callers fall
// through to their normal path rather than mistaking an unknown for an empty
// branch. The resolved base ref is returned for diagnostics.
func (p *Params) countBranchCommits(ctx context.Context, workerID string, wt *worktree.Worktree) (base string, count int, ok bool) {
	if wt == nil || wt.Path == "" || wt.Branch == "" {
		return "", 0, false
	}
	base = resolveBaseRef(ctx, wt.Path, p.BaseBranch)
	if base == "" {
		log.Printf("[pipeline:%s] Cannot resolve base ref for branch %s — skipping empty-branch check", workerID, wt.Branch)
		return "", 0, false
	}
	counter := p.CommitCounter
	if counter == nil {
		counter = countCommitsAhead
	}
	count, err := counter(ctx, wt.Path, base, wt.Branch)
	if err != nil {
		log.Printf("[pipeline:%s] Commit count against %s failed — skipping empty-branch check: %v", workerID, base, err)
		return base, 0, false
	}
	return base, count, true
}

// releaseBead resets a bead status to open via the bd CLI. It always uses a
// fresh context derived from context.Background() so that a cancelled or
// timed-out pipeline context does not prevent the release from completing.
//
// NOTE: shutdown.Manager.resetBead contains equivalent logic. If the timeout,
// flags, or error formatting change here, keep that function in sync (and vice
// versa). A future cleanup could factor this into a shared executil helper used
// by both call sites.
func releaseBead(beadID, anvilPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
	defer cancel()
	// Clear both status and assignee so the poller can re-dispatch the bead.
	// The poller filters out any bead with a non-empty assignee (poller.go),
	// so failing to clear the assignee would leave the bead permanently invisible.
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "update", beadID, "--status=open", "--assignee=", "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd update %s --status=open --assignee= --json: %w: %s", beadID, err, out)
	}
	return nil
}

// appendSteerNote appends the full steering message to the bead's notes via the
// bd CLI (the bead-notes mechanism). Like releaseBead it uses a fresh context
// so a cancelled or timed-out pipeline context does not prevent the note from
// being recorded.
func appendSteerNote(beadID, anvilPath, note string) error {
	ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "update", beadID, "--append-notes", note))
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd update %s --append-notes: %w: %s", beadID, err, out)
	}
	return nil
}

// recordSteer persists an accepted operator steer exactly once. It writes a
// bead_steered event to the activity feed carrying a short excerpt of the
// steering message, and appends the full message to the bead's notes (the
// bead-notes mechanism) so the complete instruction survives beyond the
// truncated feed entry. Both writes are best-effort: neither a failed event log
// nor a failed note append derails the steer itself. It is called from both
// steer mode A (interrupt of a running spawn) and steer mode B (message
// consumed between spawns); mode identifies which path accepted the steer.
func (p *Params) recordSteer(workerID string, iteration int, mode, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}

	_ = p.DB.LogEvent(state.EventBeadSteered,
		fmt.Sprintf("Steer accepted (%s, iteration %d): %s", mode, iteration, truncateOutput(message, 200)),
		p.Bead.ID, p.AnvilName)

	appendNote := p.SteerNoteAppender
	if appendNote == nil {
		appendNote = appendSteerNote
	}
	note := fmt.Sprintf("Operator steer (%s, iteration %d):\n%s", mode, iteration, message)
	if err := appendNote(p.Bead.ID, p.AnvilConfig.Path, note); err != nil {
		log.Printf("[pipeline:%s] failed to append steer note to bead %s: %v", workerID, p.Bead.ID, err)
	}
}

func hasDepsUpdateLabel(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, depcheck.DepsUpdateLabel) {
			return true
		}
	}
	return false
}

// shouldRunSchematic determines whether the Schematic pre-worker should run
// for the given bead and provider chain.
//
// The Copilot provider charges per-request rather than per-token, so running
// Schematic would consume an additional premium request with limited benefit.
// When the first provider is Copilot, Schematic is skipped unless the bead is
// explicitly tagged "decompose".
//
// It returns (run, reason) so the caller can log a single accurate message
// with its workerID context prefix.
func shouldRunSchematic(cfg schematic.Config, bead poller.Bead, providers []provider.Provider) (bool, string) {
	if !cfg.Enabled {
		return false, "schematic disabled in config"
	}
	// Skip schematic for Copilot to save a premium request,
	// unless the bead explicitly needs decomposition.
	if len(providers) > 0 && providers[0].Kind == provider.Copilot {
		hasDecompose := false
		for _, tag := range bead.Labels {
			if strings.EqualFold(tag, "decompose") {
				hasDecompose = true
				break
			}
		}
		if !hasDecompose {
			return false, "skipping schematic for Copilot provider to save premium request"
		}
	}
	if !schematic.ShouldRun(cfg, bead) {
		return false, "schematic not needed for this bead"
	}
	return true, ""
}

// logIngotErr logs an ingot write error without propagating it. All ingot
// writes are best-effort — the pipeline must work identically if they fail.
func logIngotErr(workerID, op string, err error) {
	if err != nil {
		log.Printf("[pipeline:%s] ingot write failed op=%s: %v", workerID, op, err)
	}
}

// truncateOutput truncates s to at most maxRunes Unicode code points, cutting
// at the last newline before the limit to avoid partial lines.
func truncateOutput(s string, maxRunes int) string {
	// Scan up to maxRunes+1 runes only — avoids O(n) count over large output.
	byteOff := 0
	count := 0
	for byteOff < len(s) {
		_, size := utf8.DecodeRuneInString(s[byteOff:])
		if size == 0 {
			break
		}
		if count == maxRunes {
			// Truncation needed — cut at last newline before the limit.
			if idx := strings.LastIndex(s[:byteOff], "\n"); idx > 0 {
				return s[:idx]
			}
			return s[:byteOff]
		}
		byteOff += size
		count++
	}
	return s
}

// recordIngotTemperResults writes temper results and per-step test results to
// the ingot tables. All writes are best-effort.
func recordIngotTemperResults(db *state.DB, workerID, beadID, anvil string, temperResult *temper.Result, ingotRec *ingot.Ingot) {
	if db == nil {
		return
	}
	conn := db.Conn()
	if conn == nil {
		return
	}
	logIngotErr(workerID, "temper_results", ingot.UpdateIngotTemperResults(
		conn, beadID, anvil,
		temperResult.Passed,
		temperResult.FailedStep,
		int(temperResult.Duration.Milliseconds()),
	))
	if ingotRec == nil || ingotRec.ID <= 0 {
		// No valid ingot ID — skip per-step inserts to avoid FK violations.
		return
	}
	for i, step := range temperResult.Steps {
		tr := &ingot.TestResult{
			IngotID:       ingotRec.ID,
			StepIndex:     i,
			StepName:      step.Name,
			Command:       step.Command,
			ExitCode:      step.ExitCode,
			DurationMs:    int(step.Duration.Milliseconds()),
			Passed:        step.Passed,
			Optional:      step.Optional,
			Skipped:       step.Skipped,
			OutputSummary: truncateOutput(step.Output, 1000),
		}
		logIngotErr(workerID, "test_result", ingot.InsertTestResult(conn, tr))
	}
}

// Run executes the full Smith → Temper → Warden pipeline for a bead.
func Run(ctx context.Context, p Params) *Outcome {
	start := time.Now()
	outcome := &Outcome{}
	workerID := p.WorkerID
	if workerID == "" {
		workerID = fmt.Sprintf("%s-%s-%d", p.AnvilName, p.Bead.ID, time.Now().Unix())
	}
	outcome.WorkerID = workerID

	// Smith-timeout ownership. When SmithTimeout > 0 the pipeline manages its own
	// deadline (rather than inheriting one from ctx) so the pause/park/resume
	// mechanic can suspend it: while parked, wall-clock time must not count
	// against the smith budget. baseCtx keeps the caller's cancellation/interrupt
	// semantics (shutdown, IPC interrupt); ctx is re-derived with a deadline that
	// is pushed forward on each resume by the time spent parked. The park wait
	// blocks on baseCtx (never the timeout ctx) so a long pause cannot trip the
	// smith timeout. When SmithTimeout <= 0, ctx is used as-is (callers that do
	// not park, e.g. crucible children, keep their inherited deadline).
	baseCtx := ctx
	originalDeadline := time.Time{}
	var pauses pauseClock
	timeout := &timeoutGate{base: baseCtx}
	defer timeout.close()
	if p.SmithTimeout > 0 {
		originalDeadline = start.Add(p.SmithTimeout)
		ctx = timeout.set(originalDeadline)
	}

	// resumeDowngraded is armed when a needs-attention resume asked to recreate the
	// worktree from a surviving branch (RecreateFromBranch) but that branch turned
	// out to be gone (deleted, or merged and pruned). With no branch to recreate,
	// a session resume is impossible: createWorktree falls back to a fresh worktree
	// from base and this flag tells the resume-arming logic below to seed a fresh
	// Smith session with the operator message instead of attempting `claude --resume`.
	resumeDowngraded := false

	// Resolve injectable dependencies, falling back to defaults.
	createWorktree := p.WorktreeCreator
	if createWorktree == nil {
		createWorktree = func(ctx context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
			// Needs-attention resume: the worktree directory was torn down but the
			// forge/<bead> branch survives. Recreate the worktree at its exact
			// original path from that branch (sub-task 1's CreateFromBranch) so
			// `claude --resume` finds the transcript keyed on that cwd.
			if rs := p.ResumeSession; rs != nil && rs.RecreateFromBranch {
				wt, err := p.WorktreeManager.CreateFromBranch(ctx, anvilPath, beadID, rs.Branch, rs.WorktreePath)
				if err == nil {
					return wt, nil
				}
				if !errors.Is(err, worktree.ErrBranchDeleted) {
					return nil, err
				}
				// The surviving branch is gone, so the prior worktree — and with it
				// any resumable session — cannot be reconstructed. Recreate a fresh
				// worktree from base and let the resume-arming logic seed a fresh
				// Smith session with the operator message.
				log.Printf("[pipeline:%s] Resume branch %q for bead %s no longer exists — recreating a fresh worktree from base", workerID, rs.Branch, beadID)
				resumeDowngraded = true
			}
			return p.WorktreeManager.CreateWithOptions(ctx, anvilPath, beadID, worktree.CreateOptions{
				BaseBranch:              p.BaseBranch,
				ResetBranch:             p.ResetBranch,
				SkipNodeModulesJunction: hasDepsUpdateLabel(p.Bead.Labels),
				// Restart-resume: reuse the retained worktree exactly as it was
				// when the bead was paused, so `claude --resume` continues in place.
				// A needs-attention resume recreates from the branch instead (handled
				// above) and never wants to preserve a stale directory here.
				PreserveExisting: p.ResumeSession != nil && !p.ResumeSession.RecreateFromBranch,
			})
		}
	}
	removeWorktree := p.WorktreeRemover
	if removeWorktree == nil {
		removeWorktree = func(ctx context.Context, anvilPath string, wt *worktree.Worktree) {
			_ = p.WorktreeManager.Remove(ctx, anvilPath, wt)
		}
	}
	spawnSmith := p.SmithRunner
	if spawnSmith == nil {
		spawnSmith = smith.SpawnWithProvider
	}
	runTemper := p.TemperRunner
	if runTemper == nil {
		runTemper = temper.Run
	}
	reviewWarden := p.WardenReviewer
	if reviewWarden == nil {
		reviewWarden = warden.Review
	}
	doRelease := p.BeadReleaser
	if doRelease == nil {
		doRelease = releaseBead
	}
	checkEmptyDiff := p.EmptyDiffChecker
	if checkEmptyDiff == nil {
		checkEmptyDiff = hasEmptyDiff
	}
	// Steer mode A (interrupt & resume a running spawn). steerCh is drained
	// while a Smith spawn runs; interruptSpawn stops it gracefully; spawnResume
	// resumes the captured session with the steering message.
	steerCh := p.SteerCh
	steerGrace := p.SteerGrace
	if steerGrace <= 0 {
		steerGrace = defaultSteerGrace
	}
	// Pause/park/resume (see ParkHandle). pauseCh is watched alongside the steer
	// mailbox while a Smith spawn runs; a pause gracefully interrupts the spawn
	// (reusing interruptSpawn, so no failure is marked) and the pipeline then
	// parks on the handle's resume signal. A nil handle leaves both nil and the
	// mechanic disabled.
	parkHandle := p.ParkHandle
	var pauseCh <-chan struct{}
	if parkHandle != nil {
		pauseCh = parkHandle.PauseRequested()
	}

	// Terminal safety net: a pause or steer acknowledged to the operator with
	// success must NEVER be silently dropped. The in-spawn (waitSmithWithSteer),
	// loop-top, and Warden-approval drains honour a pending signal by parking or
	// re-iterating. But a signal can also be enqueued while Temper/Warden run on
	// an iteration that then reaches a TERMINAL non-approval path — a Temper
	// failure, exhausted iterations, a Smith/spawn error, or an escalation to a
	// human. On those paths there is no further spawn or loop turn to carry the
	// signal, and the worker exits failed/needs-human rather than paused, so the
	// pause/steer cannot take effect. Rather than discard it, drain and surface
	// it in the event log on the way out: the operator is told explicitly that
	// the signal could not be applied (and to re-dispatch), never left believing
	// a silent success. This runs on EVERY return; on the paths that already
	// consumed the signal (park, loop-top, Warden approval) the channels are
	// empty here, so it is a no-op.
	defer func() {
		if drainPause(pauseCh) {
			log.Printf("[pipeline:%s] Pause acknowledged but pipeline terminated before it could take effect — surfacing", workerID)
			_ = p.DB.LogEvent(state.EventBeadPaused,
				"Pause could not take effect: the pipeline had already terminated (no further Smith turn to interrupt). Re-dispatch the bead to apply it.",
				p.Bead.ID, p.AnvilName)
		}
		if steerMsg, ok := drainSteer(steerCh); ok {
			log.Printf("[pipeline:%s] Steer acknowledged but pipeline terminated before it could take effect — surfacing", workerID)
			_ = p.DB.LogEvent(state.EventBeadSteered,
				fmt.Sprintf("Steer could not take effect: the pipeline had already terminated (no further Smith turn). Re-dispatch the bead to apply it. Message: %q", steerMsg),
				p.Bead.ID, p.AnvilName)
		}
	}()

	interruptSpawn := p.SmithInterrupter
	if interruptSpawn == nil {
		interruptSpawn = func(proc *smith.Process) { proc.Interrupt(steerGrace) }
	}
	// waitSmith wraps waitSmithWithSteer with the SpawnLive signal so the control
	// handle knows a spawn is interruptible (mode A) only for the duration of the
	// wait. Outside this window (Temper/Warden/between spawns) a steer is mode B.
	waitSmith := func(proc *smith.Process) (*smith.Result, string, bool) {
		if p.SpawnLive != nil {
			p.SpawnLive(true)
			defer p.SpawnLive(false)
		}
		return waitSmithWithSteer(proc, steerCh, pauseCh, interruptSpawn)
	}
	spawnResume := p.SmithResumeRunner
	if spawnResume == nil {
		spawnResume = func(ctx context.Context, wtPath, steerMsg, logDir string, pv provider.Provider, sessionID string, extraFlags []string) (*smith.Process, error) {
			return smith.SpawnWithOptions(ctx, wtPath, steerMsg, logDir, pv, extraFlags, smith.SpawnOptions{
				LogPrefix:       "steer",
				ResumeSessionID: sessionID,
			})
		}
	}

	// recordSpawnAccounting persists provider quota plus token/cost usage for a
	// completed (non-rate-limited) Smith spawn. Shared by the normal
	// provider-fallback loop and the steer-mode resume path.
	recordSpawnAccounting := func(pv provider.Provider, smithResult *smith.Result) {
		if smithResult.Quota != nil {
			if err := p.DB.UpsertProviderQuota(string(pv.Kind), smithResult.Quota); err != nil {
				log.Printf("[pipeline:%s] Failed to update provider %s quota in DB: %v", workerID, pv.Label(), err)
			} else {
				resetStr := "n/a"
				if smithResult.Quota.RequestsReset != nil {
					resetStr = time.Until(*smithResult.Quota.RequestsReset).Round(time.Minute).String()
				}
				log.Printf("[pipeline:%s] Provider %s quota updated: %d/%d requests, %d/%d tokens remaining (reset in %s)",
					workerID, pv.Label(),
					smithResult.Quota.RequestsRemaining, smithResult.Quota.RequestsLimit,
					smithResult.Quota.TokensRemaining, smithResult.Quota.TokensLimit, resetStr)
			}
		}

		// Record Copilot premium request if this was a copilot invocation
		// that completed (not rate limited).
		if pv.Kind == provider.Copilot && !smithResult.RateLimited {
			multiplier := cost.CopilotPremiumMultiplier(pv.Model)
			if multiplier > 0 {
				if err := p.DB.AddCopilotRequest(cost.Today(), multiplier); err != nil {
					log.Printf("[pipeline:%s] Failed to record copilot premium request: %v", workerID, err)
				}
			}
		}

		// Record per-provider and aggregate daily costs for non-rate-limited completions.
		if !smithResult.RateLimited && (smithResult.TokensIn > 0 || smithResult.TokensOut > 0 || smithResult.CostUSD > 0) {
			today := cost.Today()
			pvName := string(pv.Kind)
			_ = p.DB.AddDailyCost(today, smithResult.TokensIn, smithResult.TokensOut, 0, 0, smithResult.CostUSD)
			_ = p.DB.AddProviderDailyCost(today, pvName, smithResult.TokensIn, smithResult.TokensOut, 0, 0, smithResult.CostUSD)
			_ = p.DB.AddBeadCost(p.Bead.ID, p.AnvilName, smithResult.TokensIn, smithResult.TokensOut, 0, 0, smithResult.CostUSD)
		}
	}

	// Step 1: Create worktree
	log.Printf("[pipeline:%s] Creating worktree for bead %s", workerID, p.Bead.ID)
	wt, err := createWorktree(ctx, p.AnvilConfig.Path, p.Bead.ID)
	if err != nil {
		// Mark the pending worker row (inserted at claim time) as failed so it
		// no longer counts against capacity checks.
		if p.DB != nil && workerID != "" {
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
		}
		outcome.Error = fmt.Errorf("creating worktree: %w", err)
		outcome.Duration = time.Since(start)
		return outcome
	}
	outcome.Branch = wt.Branch

	// retainWorktreeOnExit, when set, suppresses the deferred worktree teardown.
	// It is armed only when the pipeline exits with the worker left in the paused
	// status (a pause that could not be resumed before shutdown/interrupt): the
	// retained worktree must survive so a later resume — including a cold resume
	// after a daemon restart — can continue in place. Every normal exit leaves it
	// false and cleans up as before. If the operator instead discards the paused
	// bead, its worker row transitions out of 'paused' and ordinary orphan/worktree
	// cleanup reclaims the retained worktree.
	retainWorktreeOnExit := false
	defer func() {
		if retainWorktreeOnExit {
			log.Printf("[pipeline:%s] Retaining worktree for paused bead (awaiting resume)", workerID)
			return
		}
		log.Printf("[pipeline:%s] Cleaning up worktree", workerID)
		oldLogDir := filepath.Join(wt.Path, ".forge-logs")
		dstDir, err := PreserveWorktreeLogs(wt.Path, p.Bead.ID)
		if err != nil {
			log.Printf("[pipeline:%s] Warning: failed to preserve smith logs: %v", workerID, err)
		} else if dstDir != "" && p.DB != nil {
			// Repoint this bead's worker rows from the worktree .forge-logs
			// (about to be deleted) to the preserved copies so historical logs
			// remain servable by the web UI after cleanup.
			if n, rerr := p.DB.RepointWorkerLogPaths(p.Bead.ID, oldLogDir, dstDir); rerr != nil {
				log.Printf("[pipeline:%s] Warning: failed to repoint worker log paths: %v", workerID, rerr)
			} else if n > 0 {
				log.Printf("[pipeline:%s] Repointed %d worker log path(s) to %s", workerID, n, dstDir)
			}
		}
		removeWorktree(ctx, p.AnvilConfig.Path, wt)
	}()

	// Record worker in state DB. Default phase to "smith"; if the Schematic
	// pre-worker is enabled it will be overwritten to "schematic" below.
	// When SkipSmith is set, we jump straight to temper.
	initialPhase := "smith"
	if p.SkipSmith {
		initialPhase = "temper"
	} else if p.SchematicConfig != nil {
		initialPhase = "schematic"
	}
	dbWorker := &state.Worker{
		ID:        workerID,
		BeadID:    p.Bead.ID,
		Anvil:     p.AnvilName,
		Branch:    wt.Branch,
		Status:    state.WorkerRunning,
		Phase:     initialPhase,
		Title:     p.Bead.Title,
		StartedAt: time.Now(),
	}
	_ = p.DB.InsertWorker(dbWorker)
	_ = p.DB.LogEvent(state.EventBeadClaimed, fmt.Sprintf("Pipeline started for %s", p.Bead.ID), p.Bead.ID, p.AnvilName)

	// Create ingot record (best-effort lifecycle tracking).
	ingotRec := &ingot.Ingot{
		BeadID:   p.Bead.ID,
		Anvil:    p.AnvilName,
		Title:    p.Bead.Title,
		Branch:   wt.Branch,
		WorkerID: workerID,
		Status:   ingot.StatusInit,
	}
	if p.DB != nil {
		if err := ingot.InsertIngot(p.DB.Conn(), ingotRec); err != nil {
			logIngotErr(workerID, "insert", err)
			// On reruns the UNIQUE(bead_id, anvil) constraint fires — resolve the
			// existing ingot so downstream steps have a valid ID for FK inserts.
			if existing, getErr := ingot.GetIngot(p.DB.Conn(), p.Bead.ID, p.AnvilName); getErr == nil && existing != nil {
				ingotRec.ID = existing.ID
			}
		}
	}

	// markIngotFailed is a convenience to set the ingot to "failed" on any
	// abort path. It is best-effort and safe to call with a nil DB.
	markIngotFailed := func() {
		if p.DB != nil {
			logIngotErr(workerID, "failed", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusFailed))
		}
	}

	// Resolve provider list.
	providers := p.Providers
	if len(providers) == 0 {
		providers = provider.Defaults()
	}
	activeProviderIdx := 0

	// Determine whether combined Smith+Warden mode should be used. Only
	// enable when configured AND the primary provider is Copilot.
	combinedMode := p.CopilotCombinedSmithWarden && len(providers) > 0 && providers[0].Kind == provider.Copilot

	// Build initial prompt context
	beadCtx := prompt.BeadContext{
		BeadID:              p.Bead.ID,
		Title:               p.Bead.Title,
		Description:         p.Bead.SpecForPrompt(),
		Notes:               p.Bead.Notes,
		IssueType:           p.Bead.IssueType,
		Priority:            p.Bead.Priority,
		Parent:              p.Bead.Parent,
		ExternalRef:         p.Bead.ExternalRef,
		Branch:              wt.Branch,
		AnvilName:           p.AnvilName,
		AnvilPath:           p.AnvilConfig.Path,
		WorktreePath:        wt.Path,
		CopilotCombinedMode: combinedMode,
	}

	// When combined mode is active, load learned Warden rules for the anvil
	// and inject them into the prompt context.
	if combinedMode {
		if rf, err := warden.LoadRules(p.AnvilConfig.Path); err == nil {
			beadCtx.WardenRules = rf.FormatChecklist()
		}
	}

	// Build hook environment for pipeline stage hooks.
	hEnv := hookEnv{
		BeadID:       p.Bead.ID,
		WorktreePath: wt.Path,
		Branch:       wt.Branch,
		AnvilName:    p.AnvilName,
		AnvilPath:    p.AnvilConfig.Path,
		Iteration:    1,
	}

	// Run Schematic pre-worker (optional — skipped when SkipSmith is set)
	if !p.SkipSmith && p.SchematicConfig != nil {
		runSchematic := p.SchematicRunner
		if runSchematic == nil {
			runSchematic = schematic.Run
		}

		// Resolve per-anvil override
		schemCfg := *p.SchematicConfig
		if p.AnvilConfig.SchematicEnabled != nil {
			schemCfg.Enabled = *p.AnvilConfig.SchematicEnabled
		}
		if p.DB != nil {
			wID := workerID
			schemCfg.OnSpawn = func(pid int, logPath string) {
				if err := p.DB.UpdateWorkerPID(wID, pid); err != nil {
					log.Printf("[pipeline:%s] failed to record schematic PID: %v", wID, err)
				}
				if err := p.DB.UpdateWorkerLogPath(wID, logPath); err != nil {
					log.Printf("[pipeline:%s] failed to record schematic log path: %v", wID, err)
				}
			}
			beadID, anvilName := p.Bead.ID, p.AnvilName
			schemCfg.OnEvent = func(kind, message string) {
				// Kinds map 1:1 to state event types so partial-decomposition
				// failures and verdict-parse skips are distinctly visible in the
				// activity feed rather than silent.
				_ = p.DB.LogEvent(state.EventType(kind), message, beadID, anvilName)
			}
		}

		schemCfg.LogDir = filepath.Join(wt.Path, ".forge-logs")

		runSchemBool, skipReason := shouldRunSchematic(schemCfg, p.Bead, providers)
		if runSchemBool {
			// before_schematic hook
			hEnv.Stage = "schematic"
			if err := runHook(ctx, workerID, "before_schematic", hookCmd(p.AnvilConfig.Hooks, "before_schematic"), hEnv); err != nil {
				outcome.Error = fmt.Errorf("before_schematic hook: %w", err)
				outcome.Duration = time.Since(start)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
				markIngotFailed()
				return outcome
			}

			log.Printf("[pipeline:%s] Running Schematic pre-analysis", workerID)
			_ = p.DB.UpdateWorkerPhase(workerID, "schematic")
			_ = p.DB.LogEvent(state.EventSchematicStarted, "Analysing bead scope", p.Bead.ID, p.AnvilName)

			schemProviders := p.schematicProviders(providers)
			usedSchem := schemProviders[0]
			sResult := runSchematic(ctx, schemCfg, p.Bead, p.AnvilConfig.Path, usedSchem)
			outcome.SchematicResult = sResult

			// Persist provider quota from the schematic's claude session.
			// Use usedSchem (the possibly-overridden provider) so quota and cost
			// accounting reflect the actual model/kind used by Schematic.
			if sResult.Quota != nil {
				if err := p.DB.UpsertProviderQuota(string(usedSchem.Kind), sResult.Quota); err != nil {
					log.Printf("[pipeline:%s] Failed to update provider %s quota from schematic: %v", workerID, usedSchem.Label(), err)
				}
			}

			// Record Copilot premium request for schematic if applicable.
			if usedSchem.Kind == provider.Copilot && sResult.Action != schematic.ActionSkip {
				if m := cost.CopilotPremiumMultiplier(usedSchem.Model); m > 0 {
					_ = p.DB.AddCopilotRequest(cost.Today(), m)
				}
			}

			switch sResult.Action {
			case schematic.ActionDecompose:
				log.Printf("[pipeline:%s] Schematic decomposed bead into %d sub-beads",
					workerID, len(sResult.SubBeads))

				// Log a summary event with JSON payload containing all sub-bead details.
				// Fall back to a simple ID list if marshalling fails so the event is still useful.
				subBeadJSON, marshalErr := json.Marshal(sResult.SubBeads)
				var subBeadStr string
				if marshalErr != nil {
					log.Printf("[pipeline:%s] Failed to marshal sub-bead details: %v", workerID, marshalErr)
					ids := make([]string, len(sResult.SubBeads))
					for i, sb := range sResult.SubBeads {
						ids[i] = sb.ID
					}
					subBeadStr = strings.Join(ids, ", ")
				} else {
					subBeadStr = string(subBeadJSON)
				}
				_ = p.DB.LogEvent(state.EventSchematicDone,
					fmt.Sprintf("Decomposed into %d sub-beads: %s", len(sResult.SubBeads), subBeadStr),
					p.Bead.ID, p.AnvilName)

				// Log each sub-bead as an individual event for easy scanning.
				// Events include a (n/total) counter so insertion order is preserved even if
				// timestamps share the same second.
				for i, sb := range sResult.SubBeads {
					_ = p.DB.LogEvent(state.EventSchematicSubBead,
						fmt.Sprintf("Created sub-bead (%d/%d) %s: %s", i+1, len(sResult.SubBeads), sb.ID, sb.Title),
						p.Bead.ID, p.AnvilName)
				}

				// Send Teams notification for decomposition
				notifySubs := make([]notify.SubBead, len(sResult.SubBeads))
				for i, sb := range sResult.SubBeads {
					notifySubs[i] = notify.SubBead{ID: sb.ID, Title: sb.Title}
				}
				p.Notifier.BeadDecomposed(ctx, p.AnvilName, p.Bead.ID, p.Bead.Title, notifySubs)

				outcome.Decomposed = true
				outcome.Duration = time.Since(start)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				return outcome

			case schematic.ActionClarify:
				log.Printf("[pipeline:%s] Schematic says bead needs clarification: %s", workerID, sResult.Reason)
				_ = p.DB.LogEvent(state.EventSchematicDone,
					fmt.Sprintf("Needs clarification: %s", sResult.Reason),
					p.Bead.ID, p.AnvilName)

				// Mark clarification_needed in DB so the poller skips this bead
				// until it is manually cleared.
				_ = p.DB.SetClarificationNeeded(p.Bead.ID, p.AnvilName, true, sResult.Reason)
				_ = p.DB.LogEvent(state.EventClarificationNeeded,
					fmt.Sprintf("Bead %s needs clarification: %s", p.Bead.ID, sResult.Reason),
					p.Bead.ID, p.AnvilName)

				// Release bead back to open for human attention
				if err := doRelease(p.Bead.ID, p.AnvilConfig.Path); err != nil {
					log.Printf("[pipeline:%s] Failed to release bead after clarify: %v", workerID, err)
				} else {
					outcome.NeedsHuman = true
				}
				outcome.Duration = time.Since(start)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				return outcome

			case schematic.ActionPlan:
				log.Printf("[pipeline:%s] Schematic produced implementation plan", workerID)
				_ = p.DB.LogEvent(state.EventSchematicDone,
					fmt.Sprintf("Plan produced (%.1fs, $%.4f)", sResult.Duration.Seconds(), sResult.CostUSD),
					p.Bead.ID, p.AnvilName)
				beadCtx.SchematicPlan = sResult.Plan

			case schematic.ActionAlreadyDecomposed:
				log.Printf("[pipeline:%s] Bead already decomposed — no work remaining", workerID)
				_ = p.DB.LogEvent(state.EventSchematicSkipped, sResult.Reason, p.Bead.ID, p.AnvilName)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				outcome.NoChangesNeeded = true
				outcome.NoChangesReason = sResult.Reason
				outcome.Duration = time.Since(start)
				return outcome

			default:
				// ActionSkip or unknown — continue without plan
				log.Printf("[pipeline:%s] Schematic skipped: %s", workerID, sResult.Reason)
				_ = p.DB.LogEvent(state.EventSchematicSkipped, sResult.Reason, p.Bead.ID, p.AnvilName)
			}

			// after_schematic hook (best-effort — failures are logged but do not abort)
			if err := runHook(ctx, workerID, "after_schematic", hookCmd(p.AnvilConfig.Hooks, "after_schematic"), hEnv); err != nil {
				_ = p.DB.LogEvent(state.EventHookFailed, fmt.Sprintf("after_schematic hook failed: %v", err), p.Bead.ID, p.AnvilName)
			}
		} else {
			log.Printf("[pipeline:%s] %s", workerID, skipReason)
		}
	}

	// Load per-anvil custom prompt template (needed for prompt rebuilds on
	// temper/warden feedback even when SkipSmith is set).
	customTmpl := prompt.LoadCustomTemplate(p.AnvilConfig.Path)
	if customTmpl != "" {
		p.PromptBuilder.CustomTemplate = customTmpl
	}

	// Resolve max iterations: prefer the param value (from config), fall back to the constant.
	maxIter := p.MaxIterations
	if maxIter <= 0 {
		maxIter = MaxIterations
	}

	// Build Smith prompt (with optional Schematic plan injected).
	// When SkipSmith is set, smith already ran externally — skip prompt
	// building and jump straight to temper on the first iteration.
	var currentPrompt string
	if p.SkipSmith {
		log.Printf("[pipeline:%s] SkipSmith=true — proceeding directly to temper/warden", workerID)
		_ = p.DB.LogEvent(state.EventSmithDone, "Pipeline resumed (skip smith) for temper/warden/PR", p.Bead.ID, p.AnvilName)
	} else {
		beadCtx.Iteration = 1
		promptText, err := p.PromptBuilder.Build(beadCtx)
		if err != nil {
			outcome.Error = fmt.Errorf("building prompt: %w", err)
			outcome.Duration = time.Since(start)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			return outcome
		}
		currentPrompt = promptText
	}

	// Track prior warden feedback for focused re-review.
	var priorWardenFeedback string

	// Track RECHECK_PREVIOUS usage across iterations. Smith may emit it at
	// most once per bead — a second use means smith keeps insisting nothing
	// is wrong, which we treat as a pathological loop and escalate.
	recheckUseCount := 0

	// Steer resume state. When pendingResume is set, the next iteration resumes
	// a Claude session (resumeSessionID) with the steering message
	// (resumeMessage) as the new prompt, using resumeProvider, instead of
	// running the normal Smith prompt. It is armed by two paths:
	//   - Steer mode A: a steer message interrupts a RUNNING spawn (resumeMessage
	//     is the raw steer text, resumeSessionID is the interrupted session).
	//   - Steer mode B: a steer message enqueued while Temper/Warden ran is
	//     consumed BETWEEN spawns at the top of the next iteration (resumeMessage
	//     merges the steer text with the pending Warden/Temper feedback,
	//     resumeSessionID is the last completed session).
	var pendingResume bool
	var resumeSessionID string
	var resumeMessage string
	var resumeProvider provider.Provider
	var resumeParkStart time.Time

	// resumeFallbackToFresh is armed alongside an operator-initiated resume
	// (Params.ResumeSession with a session_id). If that resume spawn fails to
	// attach — the transcript is missing or `claude --resume` errors — the
	// pipeline does not fail the worker: it folds the operator message into the
	// fresh bd-context prompt and runs a normal Smith session on the same
	// iteration. It is consumed (disarmed) on the first resume attempt so a later
	// chained steer resume falls through the existing steer paths unchanged.
	var resumeFallbackToFresh bool

	// Resume seed (see Params.ResumeSession). Two operator-initiated flows arm
	// this: a daemon-restart cold resume of a paused bead (retained worktree), and
	// a needs-attention resume-with-message whose worktree was recreated from the
	// surviving branch above. Both arm the resume respawn for the FIRST iteration
	// so it flows through the exact same resume path as a live steer/park resume.
	// A fresh Smith session (operator message folded into the bd-context prompt)
	// is used instead of a resume when either:
	//   - the surviving branch was gone, so no session can be resumed (resumeDowngraded), or
	//   - no session_id was ever captured (e.g. a non-Claude provider).
	// When a resume IS armed, resumeFallbackToFresh lets it downgrade to a fresh
	// session at spawn time if the transcript is missing or the resume errors.
	if p.ResumeSession != nil {
		rmsg := strings.TrimSpace(p.ResumeSession.Message)
		if rmsg == "" {
			rmsg = DefaultResumeMessage
		}
		switch {
		case resumeDowngraded:
			if !p.SkipSmith {
				currentPrompt = appendSteerToPrompt(currentPrompt, rmsg)
			}
			log.Printf("[pipeline:%s] Resume: surviving branch gone — running a fresh session seeded with the operator message", workerID)
			_ = p.DB.LogEvent(state.EventBeadResumed,
				"Resume branch gone; running a fresh session seeded with the operator message",
				p.Bead.ID, p.AnvilName)
		case p.ResumeSession.SessionID != "":
			pendingResume = true
			resumeSessionID = p.ResumeSession.SessionID
			resumeProvider = p.ResumeSession.Provider
			resumeMessage = rmsg
			resumeFallbackToFresh = true
			log.Printf("[pipeline:%s] Resume: arming respawn of session %s (fresh-session fallback armed)", workerID, resumeSessionID)
			_ = p.DB.LogEvent(state.EventBeadResumed,
				fmt.Sprintf("Resuming recorded session %s", resumeSessionID),
				p.Bead.ID, p.AnvilName)
		case !p.SkipSmith:
			currentPrompt = appendSteerToPrompt(currentPrompt, rmsg)
			log.Printf("[pipeline:%s] Resume: no session_id captured — folded resume message into fresh prompt", workerID)
			_ = p.DB.LogEvent(state.EventBeadResumed,
				"Resumed (no session_id; folded resume message into fresh prompt)",
				p.Bead.ID, p.AnvilName)
		}
	}

	// lastSessionID is the session_id captured from the most recently completed
	// Smith spawn (fresh or resumed). Steer mode B uses it to resume that
	// session when a steer message is consumed between spawns.
	// lastSessionProvider is the provider that produced lastSessionID, so mode B
	// resumes with the correct provider even if activeProviderIdx has since
	// advanced to a different fallback.
	var lastSessionID string
	var lastSessionProvider provider.Provider

	// parkPipeline centralises the pause/park/resume bookkeeping so every path
	// that observes a pause — an in-spawn pause from waitSmithWithSteer AND a
	// between-spawns pause drained around Temper/Warden — parks identically and
	// can never silently drop the pause. It records a ParkRecord for the given
	// session/iteration, transitions the worker to paused, and blocks on the
	// resume signal. It returns cancelled=true when the park was cancelled by
	// context/shutdown (the caller must return outcome with the worker left
	// paused); cancelled=false means a resume arrived and the caller should
	// continue the loop — a captured session_id has been armed as pendingResume,
	// or, when none was captured, the resume message was folded into currentPrompt.
	parkPipeline := func(sessionID string, prov provider.Provider, iteration int) (cancelled bool) {
		resumeParkStart = time.Now()
		park := ParkRecord{
			SessionID: sessionID,
			Iteration: iteration,
			Provider:  prov,
		}
		log.Printf("[pipeline:%s] Pause requested during iteration %d (session=%q) — parking", workerID, iteration, park.SessionID)

		// Transition running -> paused.
		_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerPaused)
		_ = p.DB.LogEvent(state.EventBeadPaused,
			fmt.Sprintf("Paused at iteration %d (session %s)", iteration, park.SessionID),
			p.Bead.ID, p.AnvilName)

		// Park the goroutine on the resume signal without exiting the loop.
		// Block on baseCtx, NOT the smith-timeout ctx: a parked pipeline must
		// survive an arbitrarily long pause, so only cancellation/interrupt
		// (shutdown) may unblock the wait — never the smith timeout.
		pauseStart := time.Now()
		resumeMsg, ok := parkUntilResume(baseCtx, p.ShutdownCtx, parkHandle)
		if !ok {
			// The base context was cancelled (IPC interrupt), the daemon began
			// shutting down, or the handle went away while parked. Leave the
			// worker paused and exit WITHOUT marking it failed; the parked spawn
			// was already interrupted cleanly and the worker can resume after a
			// restart. Surface a non-nil error so the caller does not mistake a
			// still-parked outcome for success (baseCtx.Err() is nil when it was
			// the shutdown context, not baseCtx, that fired).
			parkErr := baseCtx.Err()
			if parkErr == nil {
				parkErr = context.Canceled
			}
			log.Printf("[pipeline:%s] Parked pipeline cancelled before resume: %v", workerID, parkErr)
			// Retain the worktree: the worker is left paused and its retained
			// worktree must survive so a resume (including a cold resume after a
			// daemon restart) can continue in place.
			retainWorktreeOnExit = true
			outcome.Error = parkErr
			outcome.Duration = time.Since(start)
			return true
		}

		// Set a temporary generous deadline so hooks and overhead code between
		// here and the next spawn do not see an expired context. The real
		// extension (accounting for ALL overhead between the park event and the
		// spawn) happens right before spawnResume/spawnSmith.
		if p.SmithTimeout > 0 {
			parkedFor := time.Since(pauseStart)
			log.Printf("[pipeline:%s] Resumed after %s parked; deadline will be extended before next spawn (total paused so far %s)",
				workerID, parkedFor.Round(time.Second), (pauses.Total() + parkedFor).Round(time.Second))
			ctx = timeout.set(time.Now().Add(p.SmithTimeout))
		}

		// Resume: transition paused -> running and continue.
		_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerRunning)
		_ = p.DB.LogEvent(state.EventBeadResumed,
			fmt.Sprintf("Resumed at iteration %d (session %s)", iteration, park.SessionID),
			p.Bead.ID, p.AnvilName)

		// Ensure the human-requested resume always gets an iteration to run, even
		// if the pause fired on the final allowed iteration. A pause is a
		// deliberate operator action, not an automated loop, so it must not be
		// silently dropped by the max-iteration bound.
		if iteration >= maxIter {
			maxIter = iteration + 1
		}

		if park.SessionID == "" {
			// No session to resume (e.g. a non-Claude provider that reported no
			// session_id, or a pause taken before any spawn produced one). Fold
			// the resume message into a fresh prompt so it is still honoured.
			currentPrompt = appendSteerToPrompt(currentPrompt, resumeMsg)
			log.Printf("[pipeline:%s] Resume with no session_id — folded resume message into fresh prompt", workerID)
			return false
		}

		// Arm the resume respawn of the parked session for the next iteration,
		// reusing the steer resume path (`claude --resume`).
		pendingResume = true
		resumeSessionID = park.SessionID
		resumeProvider = park.Provider
		resumeMessage = resumeMsg
		return false
	}

	// applyModeBSteer arms a steer message consumed BETWEEN spawns (mode B): at
	// the loop top, or immediately before a bead completes on Warden approval. It
	// records the steer, then either resumes the last completed session with the
	// steer text (merged with any pending Warden/Temper feedback) or folds it into
	// a fresh prompt when there is no session to resume. Shared by both call sites
	// so a between-spawns steer near completion is applied on the next iteration
	// rather than silently discarded.
	applyModeBSteer := func(steerMsg string, iteration int) {
		p.recordSteer(workerID, iteration, "mode B, between spawns", steerMsg)
		if lastSessionID != "" {
			pendingResume = true
			resumeSessionID = lastSessionID
			resumeProvider = lastSessionProvider
			resumeMessage = mergeSteerWithFeedback(beadCtx.PriorFeedback, steerMsg)
			log.Printf("[pipeline:%s] Steer mode B: resuming session %s with queued steer message (iteration %d)", workerID, resumeSessionID, iteration)
		} else {
			currentPrompt = appendSteerToPrompt(currentPrompt, steerMsg)
			log.Printf("[pipeline:%s] Steer mode B: no session to resume, folded queued steer message into fresh prompt (iteration %d)", workerID, iteration)
		}
	}

	// Feedback loop
	for iteration := 1; iteration <= maxIter; iteration++ {
		outcome.Iterations = iteration
		log.Printf("[pipeline:%s] Iteration %d/%d", workerID, iteration, maxIter)

		// Between-spawns pause (loop top): an operator may have paused while
		// Temper/Warden ran on the PRIOR iteration — a window with no live spawn
		// for waitSmithWithSteer to interrupt. Consume it HERE, before this
		// iteration's spawn, so an acknowledged pause parks the pipeline instead
		// of being carried silently past. parkPipeline blocks until a resume
		// arrives (or the park is cancelled by shutdown, in which case the worker
		// is left paused and we return). We pass the last completed session so the
		// resume continues it in place; on resume parkPipeline has armed
		// pendingResume (or folded the message into the prompt), so we fall
		// through into the spawn rather than continuing/skipping the iteration.
		//
		// The iteration > 1 && !pendingResume guard mirrors the mode-B steer
		// drain below: a between-spawns pause can only exist AFTER a prior
		// spawn+Temper+Warden cycle, so on iteration 1 a pending pause is instead
		// left for waitSmithWithSteer to observe as an in-spawn park of the very
		// first spawn (never consumed early here). Skipping when a resume is
		// already armed keeps this from clobbering a steer/park resume that is
		// about to run; such a pause is picked up by the next spawn's
		// waitSmithWithSteer instead, so it is never lost.
		if iteration > 1 && !pendingResume && drainPause(pauseCh) {
			log.Printf("[pipeline:%s] Pause pending at loop top (iteration %d) — parking before spawn", workerID, iteration)
			if parkPipeline(lastSessionID, lastSessionProvider, iteration) {
				return outcome
			}
		}

		// Capture HEAD before smith runs so we can detect new commits afterward
		// and compute the diff for the next iteration's prompt context.
		preSmithSHA := gitRevParseHEAD(wt.Path)

		// smithResult is declared at loop scope so the combined-mode
		// Warden logic can inspect Smith's output.
		var smithResult *smith.Result

		// Per-iteration recheck state. Set when smith emits RECHECK_PREVIOUS:
		// in iter > 1 to signal the previous iteration's code is correct and
		// the failure was environmental. When set, the empty-diff escalation
		// is skipped and a temper failure is treated as needs_human (not a
		// retry-with-feedback).
		var recheckThisIter bool
		var recheckRationale string

		// When SkipSmith is set on the first iteration, smith already
		// completed externally — skip directly to temper verification.
		if p.SkipSmith && iteration == 1 {
			log.Printf("[pipeline:%s] Skipping smith (already completed externally)", workerID)
			_ = p.DB.UpdateWorkerPhase(workerID, "temper")
		} else {

		// before_smith hook
		hEnv.Stage = "smith"
		hEnv.Iteration = iteration
		if err := runHook(ctx, workerID, "before_smith", hookCmd(p.AnvilConfig.Hooks, "before_smith"), hEnv); err != nil {
			outcome.Error = fmt.Errorf("before_smith hook: %w", err)
			outcome.Duration = time.Since(start)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			return outcome
		}

		_ = p.DB.UpdateWorkerPhase(workerID, "smith")
		if p.DB != nil {
			logIngotErr(workerID, "smith", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusSmith))
		}

		logDir := wt.Path + "/.forge-logs"

		// steerMsgThisIter is set when a steer message interrupted this
		// iteration's spawn (steer mode A). Handled after the spawn completes.
		var steerMsgThisIter string
		// pausedThisIter is set when a pause request interrupted this iteration's
		// spawn. Handled after the spawn completes: the pipeline records a park
		// record, marks the worker paused, and blocks until resume.
		var pausedThisIter bool
		// spawnProvider is the provider that ran this iteration's Smith spawn
		// (resume or fresh). Captured alongside lastSessionID so mode B resumes
		// with the correct provider.
		var spawnProvider provider.Provider

		// Steer mode B: a steer message may have been enqueued while Temper/
		// Warden ran on a prior iteration, when no spawn was active to
		// interrupt. Consume it HERE — before this iteration's spawn — so it is
		// delivered as part of the (resumed) session rather than immediately
		// interrupting a freshly launched spawn via waitSmithWithSteer.
		//
		// This is race-safe relative to mode A: the pipeline goroutine is the
		// sole consumer of steerCh, so a queued message is handled either here
		// (between spawns) or by waitSmithWithSteer (during a spawn), never both.
		// If a spawn starts between an operator's enqueue and this drain, the
		// message simply stays in the mailbox and is picked up by
		// waitSmithWithSteer as mode A instead — it is never lost or
		// double-consumed. We skip the drain when a resume is already armed
		// (mode A just fired) so the two paths cannot stack in one iteration.
		//
		// The iteration > 1 guard is what makes this window unambiguous: a
		// between-spawns message can only exist AFTER a prior spawn+Temper+Warden
		// cycle, so no legitimate mode B message can be present before the first
		// spawn. Skipping the drain on iteration 1 also preserves mode A for a
		// steer message enqueued to interrupt the very first spawn (there is no
		// prior session to resume it against yet).
		if iteration > 1 && !pendingResume {
			if steerMsg, ok := drainSteer(steerCh); ok {
				applyModeBSteer(steerMsg, iteration)
			}
		}

		// Finalize the smith-timeout extension right before the spawn so
		// that overhead between the park event and this point (DB ops,
		// gitRevParseHEAD, hooks) does not consume the remaining budget.
		if p.SmithTimeout > 0 && !resumeParkStart.IsZero() {
			parkedFor := time.Since(resumeParkStart)
			pauses.add(parkedFor)
			extendedDeadline := pauses.extend(originalDeadline)
			ctx = timeout.set(extendedDeadline)
			log.Printf("[pipeline:%s] Smith deadline extended to %s (total paused %s)",
				workerID, extendedDeadline.Format(time.RFC3339), pauses.Total().Round(time.Second))
			resumeParkStart = time.Time{}
		}

		// runFreshSmith drives the normal provider-fallback Smith spawn below. It
		// is skipped while a resume is armed, but a failed operator resume
		// (resumeFallbackToFresh) re-arms it so the fresh bd-context session runs
		// on the same iteration.
		runFreshSmith := !pendingResume
		if pendingResume {
			// Resume a Claude session with the steering message as the new
			// prompt. Armed by mode A (interrupt of a running spawn — resumeMessage
			// is the raw steer text), mode B (message consumed between spawns —
			// resumeMessage merges the steer text with pending feedback), or an
			// operator resume-with-message (Params.ResumeSession). A resume is
			// session-bound to a single provider, so there is no rate-limit
			// fallback here.
			pv := resumeProvider
			spawnProvider = pv
			log.Printf("[pipeline:%s] Resuming session %s with message (iteration %d, provider: %s)", workerID, resumeSessionID, iteration, pv.Label())
			_ = p.DB.LogEvent(state.EventSmithStarted, fmt.Sprintf("Resume of session %s (iteration %d)", resumeSessionID, iteration), p.Bead.ID, p.AnvilName)

			// resumeUnavailable is set when an operator resume could not attach to
			// its session (spawn error, or `claude --resume` reported the transcript
			// missing). It downgrades this iteration to a fresh bd-context session
			// rather than failing the worker.
			resumeUnavailable := false
			process, err := spawnResume(ctx, wt.Path, resumeMessage, logDir, pv, resumeSessionID, p.ExtraFlags)
			if err != nil {
				if !resumeFallbackToFresh {
					outcome.Error = fmt.Errorf("resuming smith session %s (%s): %w", resumeSessionID, pv.Label(), err)
					_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
					_ = p.DB.LogEvent(state.EventSmithFailed, err.Error(), p.Bead.ID, p.AnvilName)
					markIngotFailed()
					outcome.Duration = time.Since(start)
					return outcome
				}
				log.Printf("[pipeline:%s] Resume spawn for session %s failed to start (%v) — falling back to a fresh session", workerID, resumeSessionID, err)
				resumeUnavailable = true
			} else {
				_ = p.DB.UpdateWorkerPID(workerID, process.PID)
				_ = p.DB.UpdateWorkerLogPath(workerID, process.LogPath)
				smithResult, steerMsgThisIter, pausedThisIter = waitSmith(process)

				_ = p.DB.UpdateWorkerSession(workerID, smithResult.SessionID, smith.SessionModel(smithResult, pv))
				if smithResult.ResultSubtype == "success" && !smithResult.IsError && smithResult.ExitCode == 0 {
					smithResult.RateLimited = false
				}
				recordSpawnAccounting(pv, smithResult)

				// An operator resume whose transcript is missing or whose
				// `claude --resume` errored cannot continue the prior session.
				// A steer/pause interrupt of the resumed spawn is a deliberate
				// action (handled by its own path below), not a resume failure, so
				// exclude those before deciding to fall back.
				if resumeFallbackToFresh && steerMsgThisIter == "" && !pausedThisIter && smith.ResumeUnavailable(smithResult) {
					log.Printf("[pipeline:%s] Session %s could not be resumed (transcript missing / resume error) — falling back to a fresh session seeded with the operator message", workerID, resumeSessionID)
					_ = p.DB.LogEvent(state.EventBeadResumed,
						fmt.Sprintf("Resume of session %s failed (transcript missing / resume error); starting a fresh session seeded with the operator message", resumeSessionID),
						p.Bead.ID, p.AnvilName)
					resumeUnavailable = true
				}
			}

			// The resume for this iteration is consumed. Keep resumeProvider so
			// a chained steer (interrupt of the resumed spawn) resumes the same
			// session owner again. Disarm the one-shot fresh-session fallback so a
			// later chained steer resume uses the existing steer paths unchanged.
			pendingResume = false
			resumeFallbackToFresh = false

			if resumeUnavailable {
				// Seed the fresh bd-context prompt with the operator message and
				// run a normal Smith session on this same iteration.
				currentPrompt = appendSteerToPrompt(currentPrompt, resumeMessage)
				runFreshSmith = true
			}
		}
		if runFreshSmith {
			// Run Smith (with provider fallback on rate limit)
			log.Printf("[pipeline:%s] Running Smith (provider: %s)", workerID, providers[activeProviderIdx].Label())
			_ = p.DB.LogEvent(state.EventSmithStarted, fmt.Sprintf("Iteration %d (provider: %s)", iteration, providers[activeProviderIdx].Label()), p.Bead.ID, p.AnvilName)

			for pi := activeProviderIdx; pi < len(providers); pi++ {
				pv := providers[pi]
				if pi > activeProviderIdx {
					log.Printf("[pipeline:%s] Provider %s rate limited, retrying with %s", workerID, providers[pi-1].Label(), pv.Label())
					_ = p.DB.LogEvent(state.EventSmithStarted,
						fmt.Sprintf("Rate limit fallback to provider %s (iteration %d)", pv.Label(), iteration),
						p.Bead.ID, p.AnvilName)
				}
				process, err := spawnSmith(ctx, wt.Path, currentPrompt, logDir, pv, p.ExtraFlags)
				if err != nil {
					outcome.Error = fmt.Errorf("spawning smith (%s): %w", pv.Label(), err)
					_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
					_ = p.DB.LogEvent(state.EventSmithFailed, err.Error(), p.Bead.ID, p.AnvilName)
					markIngotFailed()
					outcome.Duration = time.Since(start)
					return outcome
				}
				_ = p.DB.UpdateWorkerPID(workerID, process.PID)
				_ = p.DB.UpdateWorkerLogPath(workerID, process.LogPath)
				smithResult, steerMsgThisIter, pausedThisIter = waitSmith(process)

				// Persist the captured session_id and model for this spawn. The
				// model comes from the stream (Claude reports it in-band) and falls
				// back to the provider selection. The loop only continues on rate
				// limit, so the final iteration recorded here is the kept spawn.
				_ = p.DB.UpdateWorkerSession(workerID, smithResult.SessionID, smith.SessionModel(smithResult, pv))

				// A process that produces a genuine success result event
				// (subtype:"success" with is_error:false) AND exits 0 completed
				// successfully. The Claude CLI handles internal retries for rate
				// limits and resumes automatically. Any rate_limit_event we saw was
				// either a warning or a transient block that resolved.
				// IMPORTANT: subtype:"success" + is_error:true is a hard rate-limit
				// rejection — Claude returns this when it couldn't start the session.
				// Do NOT clear RateLimited in that case; fall back to another provider.
				// IMPORTANT: ExitCode == 0 alone is NOT enough. The Claude CLI exits
				// 0 when killed mid-tool ("Request interrupted by user") with
				// subtype:"error_during_execution". Treating that as success would
				// let an empty diff slip through to Warden, which hard-rejects.
				if smithResult.ResultSubtype == "success" && !smithResult.IsError && smithResult.ExitCode == 0 {
					smithResult.RateLimited = false
				}

				recordSpawnAccounting(pv, smithResult)

				// A steer interrupt or a pause is not a rate limit — record the
				// session owner for the resume and stop trying other providers.
				if steerMsgThisIter != "" || pausedThisIter {
					activeProviderIdx = pi
					spawnProvider = pv
					resumeProvider = pv
					break
				}

				// An auth failure is a bad/missing credential — it will fail
				// identically no matter how many providers we rotate through, so
				// stop immediately and let the escalation path below surface it
				// (Forge-d5ns). Do NOT continue the rate-limit fallback loop.
				if smithResult.AuthFailed {
					activeProviderIdx = pi
					spawnProvider = pv
					break
				}

				if !smithResult.RateLimited {
					activeProviderIdx = pi // remember for the next iteration
					spawnProvider = pv
					break
				}
			}
		}
		outcome.SmithResult = smithResult

		// Remember the session_id of this spawn so a steer message consumed
		// between spawns (mode B) can resume it on a later iteration. Guard on
		// non-empty so a spawn that reported no session (e.g. a non-Claude
		// provider) does not clobber a session captured earlier.
		if smithResult != nil && smithResult.SessionID != "" {
			lastSessionID = smithResult.SessionID
			lastSessionProvider = spawnProvider
		}

		// Pause/park/resume: an operator paused this bead. The running spawn was
		// already gracefully interrupted by waitSmithWithSteer via the SAME steer
		// interrupt path, so it is NOT marked failed. Record a park record with
		// the interrupted session and the current loop iteration, transition the
		// worker to paused, and block until a resume arrives; then arm a resume
		// respawn of the parked session (reusing the steer resume machinery) and
		// continue the loop from where it parked. This is checked before the
		// rate-limit / exit-code verdicts because a paused spawn is expected to be
		// incomplete, and before steer mode A because the two are mutually
		// exclusive for a single spawn.
		if pausedThisIter {
			if parkPipeline(smithResult.SessionID, spawnProvider, iteration) {
				return outcome
			}
			continue
		}

		// Steer mode A: a steer message interrupted this spawn. Preserve the
		// captured session_id and, if iterations remain, resume the session
		// with the steering message as the new prompt (counts as one pipeline
		// iteration). This is checked before the rate-limit / exit-code
		// verdicts because an interrupted spawn is expected to be incomplete.
		if steerMsgThisIter != "" {
			log.Printf("[pipeline:%s] Steer interrupt during iteration %d (session=%q)", workerID, iteration, smithResult.SessionID)
			p.recordSteer(workerID, iteration, "mode A, interrupt", steerMsgThisIter)

			if smithResult.SessionID == "" {
				// Without a session_id we cannot resume (only Claude reports one).
				log.Printf("[pipeline:%s] Steer requested but no session_id captured — cannot resume", workerID)
				_ = p.DB.LogEvent(state.EventSmithFailed,
					"Steer requested but provider did not report a session_id; cannot resume",
					p.Bead.ID, p.AnvilName)
				outcome.NeedsHuman = true
				outcome.Error = fmt.Errorf("steer requested but no session_id available to resume")
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}

			if iteration >= maxIter {
				// No iterations left to respawn — stop and escalate per the
				// existing max-iteration behavior.
				log.Printf("[pipeline:%s] Steer interrupt at max iterations (%d) — not respawning", workerID, maxIter)
				_ = p.DB.LogEvent(state.EventSmithFailed,
					fmt.Sprintf("Steer interrupt reached max_pipeline_iterations (%d) — not respawning", maxIter),
					p.Bead.ID, p.AnvilName)
				outcome.NeedsHuman = true
				outcome.Error = fmt.Errorf("steer interrupt reached max_pipeline_iterations (%d)", maxIter)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}

			// Arm the resume for the next iteration.
			pendingResume = true
			resumeSessionID = smithResult.SessionID
			resumeMessage = steerMsgThisIter
			continue
		}

		if smithResult.AuthFailed {
			// A provider rejected the credentials. Retrying or rotating providers
			// is pointless — a bad/missing key fails identically every time and
			// would otherwise loop forever (Forge-d5ns). Escalate for human
			// attention instead of releasing the bead back to open.
			failedProvider := spawnProvider.Label()
			log.Printf("[pipeline:%s] Provider %s authentication failed — escalating bead %s for human attention", workerID, failedProvider, p.Bead.ID)
			msg := fmt.Sprintf("Provider %s: authentication failed — check API key/credentials", failedProvider)
			_ = p.DB.LogEvent(state.EventSmithFailed, msg, p.Bead.ID, p.AnvilName)
			outcome.AuthFailed = true
			outcome.AuthProvider = failedProvider
			outcome.NeedsHuman = true
			outcome.Error = fmt.Errorf("provider %s authentication failed: %s", failedProvider, smithResult.ErrorOutput)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}

		if smithResult.RateLimited {
			log.Printf("[pipeline:%s] All providers rate limited — releasing bead %s back to open", workerID, p.Bead.ID)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				"All providers rate limited — releasing bead back to open for retry",
				p.Bead.ID, p.AnvilName)
			// Reset the bead to open so the poller can retry after backoff.
			// Use a fresh context (not the pipeline ctx) so a timed-out pipeline
			// cannot prevent the release from completing.
			if err := doRelease(p.Bead.ID, p.AnvilConfig.Path); err != nil {
				log.Printf("[pipeline:%s] Failed to release bead %s back to open: %v", workerID, p.Bead.ID, err)
				_ = p.DB.LogEvent(state.EventSmithFailed,
					fmt.Sprintf("Failed to release bead back to open after rate limit: %v", err),
					p.Bead.ID, p.AnvilName)
				outcome.Error = fmt.Errorf("all providers (%d) are rate limited, and failed to release bead back to open: %w", len(providers), err)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}
			log.Printf("[pipeline:%s] Bead %s released back to open", workerID, p.Bead.ID)
			outcome.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
			outcome.RateLimited = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}

		if smithResult.ExitCode != 0 {
			log.Printf("[pipeline:%s] Smith failed exit=%d stderr=%s", workerID, smithResult.ExitCode, smithResult.ErrorOutput)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				fmt.Sprintf("Exit code %d after %.1fs: %s", smithResult.ExitCode, smithResult.Duration.Seconds(), smithResult.ErrorOutput),
				p.Bead.ID, p.AnvilName)
			outcome.Error = fmt.Errorf("smith exit code %d", smithResult.ExitCode)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}

		// A non-empty result subtype that is not "success" indicates an
		// incomplete session even when ExitCode is 0. The Claude CLI exits 0
		// when killed mid-tool and reports subtype:"error_during_execution";
		// without this guard the pipeline would proceed to temper/warden,
		// which would hard-reject on the empty diff and surface a misleading
		// "no diff" rejection instead of the real cause.
		if smithResult.ResultSubtype != "" && smithResult.ResultSubtype != "success" {
			log.Printf("[pipeline:%s] Smith reported non-success subtype=%q (exit=%d) — failing iteration",
				workerID, smithResult.ResultSubtype, smithResult.ExitCode)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				fmt.Sprintf("Smith subtype=%s after %.1fs (exit %d, is_error=%v): %s",
					smithResult.ResultSubtype, smithResult.Duration.Seconds(),
					smithResult.ExitCode, smithResult.IsError, smithResult.ErrorOutput),
				p.Bead.ID, p.AnvilName)
			outcome.Error = fmt.Errorf("smith subtype %q indicates incomplete session", smithResult.ResultSubtype)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}

		// In review iterations (iteration > 1), Smith may use RECHECK_PREVIOUS:
		// to signal that the previous iteration's code is correct and the
		// failure was environmental (which it has now fixed or which has
		// resolved itself). Re-run temper against the same SHA as iter 1; if
		// it passes proceed to warden, if it fails escalate to needs_human.
		// Capped at one use per bead — a second use is treated as a
		// pathological loop and escalated.
		if iteration > 1 {
			if rationale := ExtractRecheckPrevious(smithResult.FullOutput); rationale != "" {
				recheckUseCount++
				if recheckUseCount > 1 {
					log.Printf("[pipeline:%s] Smith emitted RECHECK_PREVIOUS twice on bead %s — escalating to needs_human", workerID, p.Bead.ID)
					_ = p.DB.LogEvent(state.EventSmithFailed,
						fmt.Sprintf("RECHECK_PREVIOUS used %d times on bead %s (max 1) — escalating: %s",
							recheckUseCount, p.Bead.ID, rationale),
						p.Bead.ID, p.AnvilName)
					outcome.NeedsHuman = true
					outcome.Error = fmt.Errorf("smith emitted RECHECK_PREVIOUS more than once on bead %s", p.Bead.ID)
					_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
					markIngotFailed()
					outcome.Duration = time.Since(start)
					return outcome
				}
				// Verify the worktree is actually unchanged before honouring
				// RECHECK_PREVIOUS. If Smith left commits or uncommitted changes
				// while emitting the marker, its claim that the previous
				// iteration's code is correct is contradicted by its own
				// actions — treat as smith_failed so the operator can inspect.
				// Only checked when preSmithSHA is known; if git failed to
				// capture the baseline SHA before Smith ran we cannot verify.
				if preSmithSHA != "" && !checkEmptyDiff(wt.Path, preSmithSHA) {
					log.Printf("[pipeline:%s] Smith emitted RECHECK_PREVIOUS but made changes — ignoring marker, escalating to needs_human", workerID)
					_ = p.DB.LogEvent(state.EventSmithFailed,
						fmt.Sprintf("RECHECK_PREVIOUS emitted in iteration %d but worktree has changes since %s — escalating: %s",
							iteration, preSmithSHA, rationale),
						p.Bead.ID, p.AnvilName)
					outcome.NeedsHuman = true
					outcome.Error = fmt.Errorf("smith emitted RECHECK_PREVIOUS but left changes in the worktree (iteration %d)", iteration)
					_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
					markIngotFailed()
					outcome.Duration = time.Since(start)
					return outcome
				}
				recheckThisIter = true
				recheckRationale = rationale
				log.Printf("[pipeline:%s] Smith emitted RECHECK_PREVIOUS in iteration %d: %s", workerID, iteration, rationale)
				_ = p.DB.LogEvent(state.EventSmithRecheck,
					fmt.Sprintf("Iteration %d: %s", iteration, rationale),
					p.Bead.ID, p.AnvilName)
			}
		}

		// In review iterations (iteration > 1), NO_CHANGES_NEEDED is not a valid
		// signal — the warden just raised specific issues that need to be fixed.
		// Treat it as needs_human so the user can decide how to proceed. The
		// supported alternative is RECHECK_PREVIOUS: (handled above), which
		// requires a clear environmental rationale.
		if iteration > 1 && !recheckThisIter {
			if reason := ExtractNoChangesNeeded(smithResult.FullOutput); reason != "" {
				log.Printf("[pipeline:%s] Smith emitted NO_CHANGES_NEEDED in review iteration %d — escalating to needs_human", workerID, iteration)
				_ = p.DB.LogEvent(state.EventSmithFailed,
					fmt.Sprintf("NO_CHANGES_NEEDED in review iteration %d (invalid): %s", iteration, reason),
					p.Bead.ID, p.AnvilName)
				outcome.NeedsHuman = true
				outcome.Error = fmt.Errorf("smith emitted NO_CHANGES_NEEDED in review iteration %d — warden feedback was not addressed", iteration)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}
		}

		// Check if Smith determined no changes are needed (iter 1 only — iter
		// > 1 NO_CHANGES_NEEDED has already been handled above as either an
		// escalation or, when paired with RECHECK_PREVIOUS, deliberately
		// ignored here in favor of the recheck flow).
		if iteration == 1 {
			if reason := ExtractNoChangesNeeded(smithResult.FullOutput); reason != "" {
				log.Printf("[pipeline:%s] Smith says no changes needed: %s", workerID, reason)
				outcome.NoChangesNeeded = true
				outcome.NoChangesReason = reason
				_ = p.DB.LogEvent(state.EventSmithDone,
					fmt.Sprintf("Completed in %.1fs ($%.4f) \u2014 NO_CHANGES_NEEDED: %s", smithResult.Duration.Seconds(), smithResult.CostUSD, reason),
					p.Bead.ID, p.AnvilName)
				if s := smithResult.GeminiStats; s != nil {
					_ = p.DB.LogEvent(state.EventSmithStats,
						fmt.Sprintf("tokens_in=%d tokens_out=%d total=%d cached=%d input=%d tool_calls=%d duration_ms=%d",
							s.InputTokens, s.OutputTokens, s.TotalTokens, s.Cached, s.Input, s.ToolCalls, s.DurationMs),
						p.Bead.ID, p.AnvilName)
				}
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				outcome.Duration = time.Since(start)
				return outcome
			}
		}

		// Check if Smith explicitly escalated for human help.
		if reason := ExtractNeedsHuman(smithResult.FullOutput); reason != "" {
			log.Printf("[pipeline:%s] Smith escalated: NEEDS_HUMAN: %s", workerID, reason)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				fmt.Sprintf("Smith escalated — needs human: %s", reason),
				p.Bead.ID, p.AnvilName)
			if err := doRelease(p.Bead.ID, p.AnvilConfig.Path); err != nil {
				log.Printf("[pipeline:%s] Failed to release bead %s after NEEDS_HUMAN: %v", workerID, p.Bead.ID, err)
			} else {
				outcome.NeedsHuman = true
			}
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
			outcome.Duration = time.Since(start)
			return outcome
		}

		_ = p.DB.LogEvent(state.EventSmithDone,
			fmt.Sprintf("Completed in %.1fs ($%.4f)", smithResult.Duration.Seconds(), smithResult.CostUSD),
			p.Bead.ID, p.AnvilName)

		if s := smithResult.GeminiStats; s != nil {
			_ = p.DB.LogEvent(state.EventSmithStats,
				fmt.Sprintf("tokens_in=%d tokens_out=%d total=%d cached=%d input=%d tool_calls=%d duration_ms=%d",
					s.InputTokens, s.OutputTokens, s.TotalTokens, s.Cached, s.Input, s.ToolCalls, s.DurationMs),
				p.Bead.ID, p.AnvilName)
		}

		// In review iterations, check whether smith actually made any changes.
		// If the diff is empty, warden would just reject again with the same
		// feedback — escalate to needs_human instead of looping pointlessly.
		// Skip this check when smith emitted RECHECK_PREVIOUS: an empty diff
		// is the expected outcome there.
		if iteration > 1 && !recheckThisIter && checkEmptyDiff(wt.Path, preSmithSHA) {
			log.Printf("[pipeline:%s] Smith made no changes in review iteration %d — escalating to needs_human", workerID, iteration)
			_ = p.DB.LogEvent(state.EventSmithFailed,
				fmt.Sprintf("Smith made no changes in review iteration %d — warden feedback was not addressed", iteration),
				p.Bead.ID, p.AnvilName)
			outcome.NeedsHuman = true
			outcome.Error = fmt.Errorf("smith made no changes in response to warden feedback (iteration %d)", iteration)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}
		} // end else (smith phase)

		// after_smith hook (best-effort — failures are logged but do not abort)
		// Ensure Smith-stage hook metadata is populated even when Smith was
		// intentionally skipped on the first iteration.
		hEnv.Stage = "smith"
		hEnv.Iteration = iteration
		if err := runHook(ctx, workerID, "after_smith", hookCmd(p.AnvilConfig.Hooks, "after_smith"), hEnv); err != nil {
			_ = p.DB.LogEvent(state.EventHookFailed, fmt.Sprintf("after_smith hook failed: %v", err), p.Bead.ID, p.AnvilName)
		}

		// Step 2.5: Check deny patterns (post-Smith diff validation)
		if p.AnvilConfig.Smith != nil && p.AnvilConfig.Smith.DenyPatterns != nil {
			smithRawOutput := ""
			if smithResult != nil {
				smithRawOutput = smithResult.Output
			}
			violations, denyErr := checkDenyPatterns(wt.Path, preSmithSHA, smithRawOutput, p.AnvilConfig.Smith.DenyPatterns)
			if denyErr != nil {
				summary := fmt.Sprintf("deny pattern validation failed: %v", denyErr)
				log.Printf("[pipeline:%s] %s", workerID, summary)
				_ = p.DB.LogEvent(state.EventSmithFailed, summary, p.Bead.ID, p.AnvilName)

				if iteration < maxIter {
					// Unlink junctions/symlinks before reset+clean to prevent
					// git from following them into the main checkout.
					worktree.UnlinkReparsePoints(wt.Path)
					// Reset to pre-Smith state and retry with feedback.
					resetCmd := executil.HideWindow(exec.Command("git", "-C", wt.Path, "reset", "--hard", preSmithSHA))
					if resetErr := resetCmd.Run(); resetErr != nil {
						log.Printf("[pipeline:%s] Failed to reset after deny validation error: %v", workerID, resetErr)
					}
					// Clean any untracked files Smith may have added.
					cleanCmd := executil.HideWindow(exec.Command("git", "-C", wt.Path, "clean", "-fd", "-e", "node_modules"))
					_ = cleanCmd.Run()
					// Re-link node_modules after clean, but only when not in
					// SkipNodeModulesJunction mode (deps-update beads must not
					// have junctions re-created into the main checkout).
					if !hasDepsUpdateLabel(p.Bead.Labels) {
						if linkErr := worktree.LinkNodeModules(p.AnvilConfig.Path, wt.Path); linkErr != nil {
							log.Printf("[pipeline:%s] Warning: failed to re-link node_modules after deny reset: %v", workerID, linkErr)
						}
					}
					// Rewind the remote branch so the denied commits are not
					// left on origin (where a non-fast-forward on retry would
					// otherwise fail or leave stale state).
					forcePushCmd := executil.HideWindow(exec.Command("git", "-C", wt.Path, "push", "--force-with-lease", "origin", wt.Branch))
					if fpErr := forcePushCmd.Run(); fpErr != nil {
						log.Printf("[pipeline:%s] Warning: force-push after deny validation error reset failed (branch may not exist on remote yet): %v", workerID, fpErr)
					}

					beadCtx.Iteration = iteration + 1
					beadCtx.PriorFeedbackSource = "deny pattern validation"
					beadCtx.PriorFeedback = summary + "\nDeny-pattern enforcement is a hard requirement. Reimplement and ensure the changes can be validated without modifying denied files or using denied commands."
					if rebuilt, err := p.PromptBuilder.Build(beadCtx); err == nil {
						currentPrompt = rebuilt
					} else {
						log.Printf("[pipeline:%s] Failed to rebuild prompt, using fallback: %v", workerID, err)
						currentPrompt = buildFixPrompt(beadCtx, "deny pattern validation", summary, nil)
					}
					continue
				}

				outcome.Error = fmt.Errorf("deny pattern validation failed after %d iterations: %w", iteration, denyErr)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}
			if len(violations) > 0 {
				summary := formatDenyViolations(violations)
				log.Printf("[pipeline:%s] Deny pattern violations: %s", workerID, summary)
				_ = p.DB.LogEvent(state.EventSmithFailed, summary, p.Bead.ID, p.AnvilName)

				if iteration < maxIter {
					// Unlink junctions/symlinks before reset+clean to prevent
					// git from following them into the main checkout.
					worktree.UnlinkReparsePoints(wt.Path)
					// Reset to pre-Smith state and retry with feedback.
					resetCmd := executil.HideWindow(exec.Command("git", "-C", wt.Path, "reset", "--hard", preSmithSHA))
					if resetErr := resetCmd.Run(); resetErr != nil {
						log.Printf("[pipeline:%s] Failed to reset after deny violation: %v", workerID, resetErr)
					}
					// Clean any untracked files Smith may have added.
					cleanCmd := executil.HideWindow(exec.Command("git", "-C", wt.Path, "clean", "-fd", "-e", "node_modules"))
					_ = cleanCmd.Run()
					// Re-link node_modules after clean, but only when not in
					// SkipNodeModulesJunction mode (deps-update beads must not
					// have junctions re-created into the main checkout).
					if !hasDepsUpdateLabel(p.Bead.Labels) {
						if linkErr := worktree.LinkNodeModules(p.AnvilConfig.Path, wt.Path); linkErr != nil {
							log.Printf("[pipeline:%s] Warning: failed to re-link node_modules after deny reset: %v", workerID, linkErr)
						}
					}
					// Rewind the remote branch so the denied commits are not
					// left on origin (where a non-fast-forward on retry would
					// otherwise fail or leave stale state).
					forcePushCmd := executil.HideWindow(exec.Command("git", "-C", wt.Path, "push", "--force-with-lease", "origin", wt.Branch))
					if fpErr := forcePushCmd.Run(); fpErr != nil {
						log.Printf("[pipeline:%s] Warning: force-push after deny violation reset failed (branch may not exist on remote yet): %v", workerID, fpErr)
					}

					beadCtx.Iteration = iteration + 1
					beadCtx.PriorFeedbackSource = "deny pattern validation"
					beadCtx.PriorFeedback = summary + "\nYou MUST NOT modify denied files or run denied commands. Reimplement without violating these constraints."
					if rebuilt, err := p.PromptBuilder.Build(beadCtx); err == nil {
						currentPrompt = rebuilt
					} else {
						log.Printf("[pipeline:%s] Failed to rebuild prompt, using fallback: %v", workerID, err)
						currentPrompt = buildFixPrompt(beadCtx, "deny pattern validation", summary, nil)
					}
					continue
				}

				outcome.Error = fmt.Errorf("deny pattern violations after %d iterations: %s", iteration, summary)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}
		}

		// before_temper hook
		hEnv.Stage = "temper"
		hEnv.Iteration = iteration
		if err := runHook(ctx, workerID, "before_temper", hookCmd(p.AnvilConfig.Hooks, "before_temper"), hEnv); err != nil {
			outcome.Error = fmt.Errorf("before_temper hook: %w", err)
			outcome.Duration = time.Since(start)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			return outcome
		}

		// Step 3: Run Temper (build/test)
		log.Printf("[pipeline:%s] Running Temper verification", workerID)
		_ = p.DB.UpdateWorkerPhase(workerID, "temper")
		if p.DB != nil {
			logIngotErr(workerID, "temper", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusTemper))
		}
		// Build a per-iteration copy of the temper config so we never mutate
		// the shared p.TemperConfig and always have a fresh changed-file list.
		var iterTemperCfg temper.Config
		if p.TemperConfig != nil {
			iterTemperCfg = *p.TemperConfig
		} else {
			iterTemperCfg = temper.DefaultConfigWithRace(wt.Path, temper.DetectOptionsFromAnvilFlag(p.AnvilConfig.GolangciLint), p.GoRaceDetection)
		}
		// Apply the configurable timeouts/output cap. Only fill fields the
		// resolved config left unset so a per-anvil temper config still wins.
		if iterTemperCfg.StepTimeout <= 0 {
			iterTemperCfg.StepTimeout = p.TemperStepTimeout
		}
		if iterTemperCfg.GitTimeout <= 0 {
			iterTemperCfg.GitTimeout = p.TemperGitTimeout
		}
		if iterTemperCfg.OutputCap <= 0 {
			iterTemperCfg.OutputCap = p.TemperOutputCap
		}

		// Recompute the changed-file list every iteration so that files added
		// by Smith in later iterations are not silently missed by path filters.
		// Resolve the base ref the same way as the worktree manager: try
		// origin/main first, then fall back to origin/master.
		baseRef := resolveTemperBaseRef(ctx, wt.Path, wt.BaseBranch)
		if baseRef != "" {
			changed, err := temper.ChangedFilesFromGit(ctx, wt.Path, baseRef)
			if err != nil {
				log.Printf("[pipeline:%s] Warning: could not compute changed files for path filtering: %v", workerID, err)
			} else {
				iterTemperCfg.ChangedFiles = changed
			}
		}

		temperResult := runTemper(ctx, wt.Path, iterTemperCfg, p.DB, p.Bead.ID, p.AnvilName)
		outcome.TemperResult = temperResult

		// Infra/timeout retry-without-smith: a step killed by its deadline or by
		// an infrastructure failure (signal death, host crash) is not evidence
		// that Smith's code is wrong. Re-run Temper ONCE without invoking Smith;
		// only if it fails the same way do we escalate to needs-attention with
		// the classification — never loop Smith on a phantom failure.
		if !temperResult.Passed && temperResult.Classification.IsRetryableWithoutSmith() {
			// A cancelled pipeline context (daemon shutdown or IPC interrupt)
			// kills the step via SIGKILL, which classifyFailure reports as an
			// infra failure. That is NOT an infrastructure problem with the
			// code — retrying would just be killed again and escalating would
			// wrongly flag the bead as needs_human. Abort cleanly like the
			// parked-pipeline path so the bead stays open for a retry after
			// restart.
			if ctxErr := ctx.Err(); ctxErr != nil {
				log.Printf("[pipeline:%s] Temper step %q killed by context cancellation (%v) — aborting without escalation",
					workerID, temperResult.FailedStep, ctxErr)
				outcome.Error = ctxErr
				outcome.Duration = time.Since(start)
				return outcome
			}

			log.Printf("[pipeline:%s] Temper failed at step %q with classification %q — retrying once without Smith",
				workerID, temperResult.FailedStep, temperResult.Classification)
			_ = p.DB.LogEvent(state.EventTemperFailed,
				fmt.Sprintf("Temper step %q classified as %s (iteration %d) — retrying once without Smith",
					temperResult.FailedStep, temperResult.Classification, iteration),
				p.Bead.ID, p.AnvilName)

			retryResult := runTemper(ctx, wt.Path, iterTemperCfg, p.DB, p.Bead.ID, p.AnvilName)
			temperResult = retryResult
			outcome.TemperResult = temperResult

			// The retry itself can be interrupted mid-flight by a shutdown; the
			// same cancellation-induced SIGKILL would look like a persistent
			// infra failure. Abort cleanly rather than escalating to needs_human.
			if ctxErr := ctx.Err(); ctxErr != nil {
				log.Printf("[pipeline:%s] Temper retry for step %q killed by context cancellation (%v) — aborting without escalation",
					workerID, retryResult.FailedStep, ctxErr)
				outcome.Error = ctxErr
				outcome.Duration = time.Since(start)
				return outcome
			}

			if !retryResult.Passed && retryResult.Classification.IsRetryableWithoutSmith() {
				// Still infra/timeout after the retry: escalate to needs-attention
				// with the classification instead of looping Smith on it.
				log.Printf("[pipeline:%s] Temper %s failure persisted after retry at step %q — escalating to needs_human",
					workerID, retryResult.Classification, retryResult.FailedStep)
				recordIngotTemperResults(p.DB, workerID, p.Bead.ID, p.AnvilName, temperResult, ingotRec)
				_ = p.DB.LogEvent(state.EventTemperFailed,
					fmt.Sprintf("Temper %s failure persisted after one retry at step %q (iteration %d) — needs human attention. %s",
						retryResult.Classification, retryResult.FailedStep, iteration, retryResult.Summary),
					p.Bead.ID, p.AnvilName)
				outcome.NeedsHuman = true
				outcome.Error = fmt.Errorf("temper %s failure persisted after retry at step %s", retryResult.Classification, retryResult.FailedStep)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}
			// Retry passed, or surfaced a real test/build failure: fall through
			// to the normal handling below (Warden on pass, Smith loop on a real
			// code failure).
		}

		// Record the (possibly retried) Temper result once so per-step ingot
		// rows are not duplicated by the retry above.
		recordIngotTemperResults(p.DB, workerID, p.Bead.ID, p.AnvilName, temperResult, ingotRec)

		// after_temper hook (best-effort)
		if err := runHook(ctx, workerID, "after_temper", hookCmd(p.AnvilConfig.Hooks, "after_temper"), hEnv); err != nil {
			_ = p.DB.LogEvent(state.EventHookFailed, fmt.Sprintf("after_temper hook failed: %v", err), p.Bead.ID, p.AnvilName)
		}

		if !temperResult.Passed {
			log.Printf("[pipeline:%s] Temper failed at step: %s", workerID, temperResult.FailedStep)

			// When smith asserted RECHECK_PREVIOUS this iteration, a temper
			// failure means the rationale was wrong — the previous iteration's
			// code is still broken. Don't loop back to smith with feedback;
			// escalate to needs_human and preserve both the rationale and the
			// temper failure in the failure event so the operator sees both.
			if recheckThisIter {
				log.Printf("[pipeline:%s] Temper failed after RECHECK_PREVIOUS in iteration %d — escalating to needs_human", workerID, iteration)
				_ = p.DB.LogEvent(state.EventSmithFailed,
					fmt.Sprintf("Temper failed after RECHECK_PREVIOUS (iteration %d). Smith rationale: %s. Temper failure: %s",
						iteration, recheckRationale, temperResult.Summary),
					p.Bead.ID, p.AnvilName)
				outcome.NeedsHuman = true
				outcome.Error = fmt.Errorf("temper failed after RECHECK_PREVIOUS at step %s: %s", temperResult.FailedStep, recheckRationale)
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				markIngotFailed()
				outcome.Duration = time.Since(start)
				return outcome
			}

			if iteration < maxIter {
				// Capture the diff from this iteration so the next smith
				// session can see what was already implemented.
				beadCtx.PriorDiff = truncateDiff(gitDiffSince(wt.Path, preSmithSHA), maxDiffLen)

				// Rebuild prompt with temper feedback for next iteration
				beadCtx.Iteration = iteration + 1
				beadCtx.PriorFeedbackSource = "build/test verification"
				beadCtx.PriorFeedback = temperResult.Summary
				if rebuilt, err := p.PromptBuilder.Build(beadCtx); err == nil {
					currentPrompt = rebuilt
				} else {
					log.Printf("[pipeline:%s] Failed to rebuild prompt, using fallback: %v", workerID, err)
					currentPrompt = buildFixPrompt(beadCtx, "build/test", temperResult.Summary, nil)
				}
				continue
			}

			// Final iteration and still failing
			outcome.Error = fmt.Errorf("temper verification failed after %d iterations: %s", iteration, temperResult.FailedStep)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}

		// before_warden hook
		hEnv.Stage = "warden"
		hEnv.Iteration = iteration
		if err := runHook(ctx, workerID, "before_warden", hookCmd(p.AnvilConfig.Hooks, "before_warden"), hEnv); err != nil {
			outcome.Error = fmt.Errorf("before_warden hook: %w", err)
			outcome.Duration = time.Since(start)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			return outcome
		}

		// Step 4: Run Warden review (or skip for small Copilot diffs / combined mode)
		diffStat := computeDiffStat(wt.Path, preSmithSHA)
		var reviewResult *warden.ReviewResult

		// Combined Smith+Warden mode: parse Smith's self-review and decide
		// whether a real Warden is needed.
		// forceRealWarden bypasses shouldSkipWarden so a real review always runs
		// when we cannot derive a verdict from Smith's output.
		var forceRealWarden bool
		if combinedMode && smithResult == nil {
			forceRealWarden = true
			log.Printf("[pipeline:%s] Combined mode: smithResult is nil (SkipSmith=%v, iteration=%d), falling back to real Warden", workerID, p.SkipSmith, iteration)
		} else if combinedMode && smithResult != nil {
			selfReview := parseSelfReview(smithResult.FullOutput)
			// Only skip real Warden when Smith actually ran under Copilot; if Smith
			// fell back to a different provider the combined-mode guarantee no longer
			// holds, so force a real review regardless of the self-review verdict.
			if smithResult.ProviderUsed != provider.Copilot {
				log.Printf("[pipeline:%s] Combined mode: Smith ran under non-Copilot provider (%v), forcing real Warden", workerID, smithResult.ProviderUsed)
				forceRealWarden = true
			} else if !shouldRunRealWarden(selfReview, p.Bead, p.CopilotWardenSampleRate) {
				log.Printf("[pipeline:%s] Combined mode: self-review approved, skipping real Warden", workerID)
				_ = p.DB.UpdateWorkerPhase(workerID, "warden")
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerReviewing)
				if p.DB != nil {
					logIngotErr(workerID, "warden", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusWarden))
				}
				summary := "Auto-approved: Smith self-review passed (combined mode)"
				reviewResult = &warden.ReviewResult{
					Verdict: warden.VerdictApprove,
					Summary: summary,
				}
			} else {
				if selfReview == nil {
					log.Printf("[pipeline:%s] Combined mode: self-review parse failed, falling back to real Warden", workerID)
				} else if selfReview.Verdict == "request_changes" {
					log.Printf("[pipeline:%s] Combined mode: self-review flagged concerns, running real Warden", workerID)
				} else if p.Bead.Priority <= 1 {
					log.Printf("[pipeline:%s] Combined mode: P%d bead requires real Warden", workerID, p.Bead.Priority)
				} else {
					log.Printf("[pipeline:%s] Combined mode: sampled for real Warden review", workerID)
				}
			}
		}

		if reviewResult != nil {
			// Already resolved (skip-warden or combined mode auto-approve).
		} else if !forceRealWarden && shouldSkipWarden(diffStat, p.Bead, providers, p.CopilotSkipWardenSmallDiffs) {
			log.Printf("[pipeline:%s] Skipping Warden review for small Copilot diff (lines_changed=%d, files_changed=%d, reason=low-risk diff under threshold)",
				workerID, diffStat.LinesChanged, diffStat.FilesChanged)
			_ = p.DB.UpdateWorkerPhase(workerID, "warden")
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerReviewing)
			if p.DB != nil {
				logIngotErr(workerID, "warden", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusWarden))
			}
			_ = p.DB.LogEvent(state.EventWardenPass,
				fmt.Sprintf("Warden skipped: small low-risk Copilot diff (%d lines, %d files)", diffStat.LinesChanged, diffStat.FilesChanged),
				p.Bead.ID, p.AnvilName)
			reviewResult = &warden.ReviewResult{
				Verdict: warden.VerdictApprove,
				Summary: fmt.Sprintf("Auto-approved: small low-risk diff (%d lines, %d files)", diffStat.LinesChanged, diffStat.FilesChanged),
			}
		} else {
			log.Printf("[pipeline:%s] Running Warden review", workerID)
			_ = p.DB.UpdateWorkerPhase(workerID, "warden")
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerReviewing)
			if p.DB != nil {
				logIngotErr(workerID, "warden", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusWarden))
			}

			// On re-review iterations, pass prior feedback unless full re-review is configured.
			wardenFeedbackArg := priorWardenFeedback
			if p.WardenFullRereview {
				wardenFeedbackArg = ""
			}

			var err error
			reviewResult, err = reviewWarden(ctx, wt.Path, p.Bead.ID, p.Bead.Title, p.Bead.SpecForPrompt(), p.AnvilConfig.Path, p.DB, wardenFeedbackArg, p.wardenProviders(providers)...)
			if err != nil {
				log.Printf("[pipeline:%s] Warden error: %v", workerID, err)
				// Warden failure is not fatal — default to approve and let human review
				reviewResult = &warden.ReviewResult{
					Verdict: warden.VerdictApprove,
					Summary: "Warden failed, defaulting to approve for human review",
				}
				_ = p.DB.LogEvent(state.EventWardenPass, "Warden failed, defaulting to approve", p.Bead.ID, p.AnvilName)
			} else {
				// Record Copilot premium request for warden review if applicable.
				if reviewResult.UsedProvider != nil && reviewResult.UsedProvider.Kind == provider.Copilot {
					multiplier := cost.CopilotPremiumMultiplier(reviewResult.UsedProvider.Model)
					if multiplier > 0 {
						if err := p.DB.AddCopilotRequest(cost.Today(), multiplier); err != nil {
							log.Printf("[pipeline:%s] Failed to record copilot premium request for warden: %v", workerID, err)
						}
					}
				}
			}
		}
		outcome.ReviewResult = reviewResult

		// after_warden hook (best-effort)
		if err := runHook(ctx, workerID, "after_warden", hookCmd(p.AnvilConfig.Hooks, "after_warden"), hEnv); err != nil {
			_ = p.DB.LogEvent(state.EventHookFailed, fmt.Sprintf("after_warden hook failed: %v", err), p.Bead.ID, p.AnvilName)
		}

		switch reviewResult.Verdict {
		case warden.VerdictApprove:
			// A pause or steer may have been enqueued while Temper/Warden ran on
			// this iteration — a window with no live spawn for waitSmithWithSteer
			// to interrupt. If one is pending it must be honoured here rather than
			// silently dropped by a bead that is about to complete: the operator
			// was told the pause/steer succeeded. A pending pause parks the
			// pipeline (it resumes for another turn); a pending steer re-enters the
			// loop with the correction applied. Both are drained before any
			// completion state is written.
			if drainPause(pauseCh) {
				log.Printf("[pipeline:%s] Pause pending at Warden approval (iteration %d) — parking instead of completing", workerID, iteration)
				if parkPipeline(lastSessionID, lastSessionProvider, iteration) {
					return outcome
				}
				continue
			}
			if steerMsg, ok := drainSteer(steerCh); ok {
				log.Printf("[pipeline:%s] Steer pending at Warden approval (iteration %d) — applying instead of completing", workerID, iteration)
				// A steer is a deliberate operator action, so give it a turn even
				// if approval landed on the final allowed iteration.
				if iteration >= maxIter {
					maxIter = iteration + 1
				}
				applyModeBSteer(steerMsg, iteration)
				continue
			}

			log.Printf("[pipeline:%s] Warden approved", workerID)
			outcome.Verdict = warden.VerdictApprove

			// A clean run can still leave an empty branch: the change may have
			// landed on the base branch while this bead was in flight (a
			// sibling PR shipping the same work), so Smith rebuilds the same
			// artifacts, commits nothing, and Temper/Warden happily pass. PR
			// creation would then fail with "No commits between <base> and
			// <branch>" and every retry would reproduce it, so short-circuit
			// here with a distinct, non-retryable outcome instead.
			if base, count, ok := p.countBranchCommits(ctx, workerID, wt); ok && count == 0 {
				action, recognised := config.ResolveEmptyDiffAction(p.EmptyDiffAction)
				if !recognised {
					log.Printf("[pipeline:%s] Unrecognised empty_diff_action %q — falling back to %q", workerID, p.EmptyDiffAction, action)
				}
				log.Printf("[pipeline:%s] Branch %s has no commits vs %s — skipping PR creation (action=%s)", workerID, wt.Branch, base, action)
				outcome.EmptyDiff = true
				outcome.EmptyDiffAction = action
				outcome.EmptyDiffBase = base
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
				// The review did approve — record it so the ingot does not sit
				// in "warden" forever. The missing PR is explained by the
				// smith_empty_result event below.
				if p.DB != nil {
					logIngotErr(workerID, "approved", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusApproved))
				}
				_ = p.DB.LogEvent(state.EventWardenPass, reviewResult.Summary, p.Bead.ID, p.AnvilName)
				_ = p.DB.LogEvent(state.EventSmithEmptyResult,
					fmt.Sprintf("Branch %s has no commits vs %s — the work is already on the base branch; skipping PR creation (action=%s)",
						wt.Branch, base, action),
					p.Bead.ID, p.AnvilName)
				outcome.Duration = time.Since(start)
				return outcome
			}

			outcome.Success = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerMonitoring)
			_ = p.DB.UpdateWorkerPhase(workerID, "bellows")
			_ = p.DB.LogEvent(state.EventWardenPass, reviewResult.Summary, p.Bead.ID, p.AnvilName)
			if p.DB != nil {
				logIngotErr(workerID, "approved", ingot.UpdateIngotStatus(p.DB.Conn(), p.Bead.ID, p.AnvilName, ingot.StatusApproved))
			}

			outcome.ChangelogSummary = ExtractChangelogSummary(wt.Path, p.Bead.ID)

			// Ensure the branch is pushed to the remote before the worktree
			// is cleaned up. Smith is instructed to push, but as a safety net
			// we push here too — this is critical for the Crucible flow where
			// the PR is created after the pipeline returns.
			pushCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "push", "-u", "origin", wt.Branch))
			pushCmd.Dir = wt.Path
			if pushErr := pushCmd.Run(); pushErr != nil {
				log.Printf("[pipeline:%s] Warning: explicit push failed (Smith may have already pushed): %v", workerID, pushErr)
			}

			outcome.Duration = time.Since(start)
			return outcome

		case warden.VerdictReject:
			log.Printf("[pipeline:%s] Warden hard-rejected", workerID)
			outcome.Verdict = warden.VerdictReject
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			_ = p.DB.LogEvent(state.EventWardenHardReject,
				fmt.Sprintf("Verdict: reject — %s", wardenEventSummary(reviewResult)),
				p.Bead.ID, p.AnvilName)
			if reviewResult.NoDiff {
				// Smith produced no diff — release the bead back to open so
				// a human can investigate and retry rather than leaving it
				// stuck in_progress with no active worker.
				// Use a fresh context (not the pipeline ctx) so a timed-out
				// pipeline cannot prevent the release from completing.
				log.Printf("[pipeline:%s] No-diff rejection — releasing bead %s back to open for human review", workerID, p.Bead.ID)
				if releaseErr := doRelease(p.Bead.ID, p.AnvilConfig.Path); releaseErr != nil {
					log.Printf("[pipeline:%s] Failed to release bead %s back to open: %v", workerID, p.Bead.ID, releaseErr)
					_ = p.DB.LogEvent(state.EventWardenHardReject,
						fmt.Sprintf("Failed to release bead back to open after no-diff: %v", releaseErr),
						p.Bead.ID, p.AnvilName)
				} else {
					log.Printf("[pipeline:%s] Bead %s released back to open (no-diff)", workerID, p.Bead.ID)
					_ = p.DB.LogEvent(state.EventWardenHardReject,
						"Bead released back to open — Smith produced no diff, needs human attention",
						p.Bead.ID, p.AnvilName)
					outcome.NeedsHuman = true
				}
			}
			outcome.Duration = time.Since(start)
			return outcome

		case warden.VerdictRequestChanges:
			log.Printf("[pipeline:%s] Warden requests changes (iteration %d)", workerID, iteration)
			_ = p.DB.LogEvent(state.EventWardenReject,
				fmt.Sprintf("Request changes (iteration %d/%d): %s", iteration, maxIter, wardenEventSummary(reviewResult)),
				p.Bead.ID, p.AnvilName)

			// Capture warden feedback for focused re-review on next iteration.
			priorWardenFeedback = formatWardenFeedback(reviewResult.Summary, reviewResult.Issues)

			if iteration < maxIter {
				// Capture the diff from this iteration so the next smith
				// session can see what was already implemented.
				beadCtx.PriorDiff = truncateDiff(gitDiffSince(wt.Path, preSmithSHA), maxDiffLen)

				// Rebuild prompt with warden feedback for next iteration
				beadCtx.Iteration = iteration + 1
				beadCtx.PriorFeedbackSource = "Warden code review"
				beadCtx.PriorFeedback = priorWardenFeedback
				if rebuilt, err := p.PromptBuilder.Build(beadCtx); err == nil {
					currentPrompt = rebuilt
				} else {
					log.Printf("[pipeline:%s] Failed to rebuild prompt, using fallback: %v", workerID, err)
					currentPrompt = buildFixPrompt(beadCtx, "review", reviewResult.Summary, reviewResult.Issues)
				}
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerRunning)
				continue
			}

			// Max iterations reached with request_changes — mark NeedsHuman
			// so the daemon skips the circuit breaker and surfaces this
			// immediately in Hearth's Needs Attention panel.
			outcome.Verdict = warden.VerdictRequestChanges
			outcome.NeedsHuman = true
			outcome.Error = fmt.Errorf("warden still requesting changes after %d iterations", iteration)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			markIngotFailed()
			outcome.Duration = time.Since(start)
			return outcome
		}
	}

	outcome.Duration = time.Since(start)
	return outcome
}

// gitRevParseHEAD returns the current HEAD commit SHA for the given worktree.
// Returns an empty string on error.
func gitRevParseHEAD(worktreePath string) string {
	cmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "rev-parse", "HEAD"))
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveTemperBaseRef returns the git ref to use as the base for computing
// changed files for Temper path filtering. When baseBranch is set explicitly
// it is used directly (as "origin/<baseBranch>"). Otherwise it mirrors the
// worktree manager's auto-detection: try origin/main, then origin/master.
// Returns an empty string if no valid ref is found.
//
// The candidate probes run with the git repo-location env vars stripped (see
// executil.CleanGitEnv) so a daemon that itself lives in a git worktree cannot
// have its own repository answer for the anvil's — an inherited GIT_DIR makes
// `git -C <worktree> rev-parse` resolve origin/main from the ambient repo, and
// Temper would then filter paths against a base ref belonging to a different
// repository.
func resolveTemperBaseRef(ctx context.Context, worktreePath, baseBranch string) string {
	if baseBranch != "" {
		return "origin/" + baseBranch
	}
	for _, candidate := range []string{"origin/main", "origin/master"} {
		cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--verify", candidate))
		cmd.Env = executil.CleanGitEnv()
		if err := cmd.Run(); err == nil {
			return candidate
		}
	}
	return ""
}

// resolveBaseRef returns the remote-tracking ref the worktree's branch will be
// merged into: "origin/<baseBranch>" when a base branch is known (e.g. a
// Crucible child targeting its epic branch), otherwise origin/main or
// origin/master, whichever exists. Returns an empty string when none resolves.
//
// Like resolveTemperBaseRef this runs with the git repo-location env vars
// stripped, so a daemon that itself lives in a git worktree cannot have its own
// repository answer for the anvil's. Unlike resolveTemperBaseRef it also verifies an
// explicitly configured base branch instead of trusting it.
func resolveBaseRef(ctx context.Context, worktreePath, baseBranch string) string {
	verify := func(ref string) bool {
		cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--verify", ref))
		cmd.Env = executil.CleanGitEnv()
		return cmd.Run() == nil
	}
	if baseBranch != "" {
		ref := "origin/" + baseBranch
		if verify(ref) {
			return ref
		}
		return ""
	}
	for _, candidate := range []string{"origin/main", "origin/master"} {
		if verify(candidate) {
			return candidate
		}
	}
	return ""
}

// countCommitsAhead returns how many commits branch carries that base does not,
// i.e. `git rev-list --count <base>..<branch>`. This is the same comparison the
// forge (GitHub/GitLab/Gitea) performs when opening a PR, so a zero here means
// PR creation would fail with "No commits between <base> and <branch>".
func countCommitsAhead(ctx context.Context, worktreePath, base, branch string) (int, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath,
		"rev-list", "--count", base+".."+branch))
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s..%s: %w", base, branch, err)
	}
	trimmed := strings.TrimSpace(string(out))
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s..%s: unparseable output %q: %w", base, branch, trimmed, err)
	}
	return n, nil
}

// hasEmptyDiff reports whether the worktree has no uncommitted changes and no
// new commits since preSmithSHA. The preSmithSHA is captured before smith runs
// so that post-push state (where @{upstream} == HEAD) doesn't falsely indicate
// an empty diff.
func hasEmptyDiff(worktreePath, preSmithSHA string) bool {
	env := executil.CleanGitEnv()
	// Check for uncommitted changes (staged or unstaged).
	statusCmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "status", "--porcelain"))
	statusCmd.Env = env
	statusOut, err := statusCmd.Output()
	if err != nil {
		return false // assume changes on error
	}
	if len(strings.TrimSpace(string(statusOut))) > 0 {
		return false // uncommitted changes present
	}
	// If we have a pre-smith SHA, compare directly against it.
	if preSmithSHA != "" {
		currentSHA := gitRevParseHEAD(worktreePath)
		if currentSHA == "" {
			return false // unknown — assume changes on git error
		}
		if currentSHA != preSmithSHA {
			return false // new commits exist — smith did real work
		}
		// Same SHA means no new commits were added.
		return true
	}
	// Fallback: diff against parent commit.
	diffCmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "diff", "HEAD~1"))
	diffCmd.Env = env
	diffOut, err := diffCmd.Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(diffOut))) == 0
}

// maxDiffLen is the maximum number of bytes to include from a prior
// iteration's diff. Longer diffs are truncated at the last newline before
// this limit so the prompt stays within a reasonable size.
const maxDiffLen = 8000

// gitDiffSince returns the diff between fromSHA and HEAD in the given worktree.
// Returns an empty string on error or if fromSHA is empty.
func gitDiffSince(worktreePath, fromSHA string) string {
	if fromSHA == "" {
		return ""
	}
	// Use "git diff <fromSHA>" (without ..HEAD) so that staged/unstaged
	// changes in the worktree are included alongside committed changes.
	cmd := executil.HideWindow(exec.Command("git", "-C", worktreePath, "diff", fromSHA))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// truncateDiff truncates a diff string to at most maxLen bytes, cutting at
// the last newline before the limit so partial lines are not included. It
// avoids splitting multi-byte UTF-8 sequences by searching for the newline
// within the valid string prefix rather than slicing raw bytes.
func truncateDiff(diff string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(diff) <= maxLen {
		return diff
	}
	// Find the last newline that starts at or before maxLen bytes. Because
	// '\n' is a single-byte character, LastIndex on the full string up to a
	// byte boundary is safe — but the resulting prefix is only valid UTF-8 if
	// we cut at a newline (which never appears inside a multi-byte sequence).
	candidate := diff[:maxLen]
	if idx := strings.LastIndex(candidate, "\n"); idx > 0 {
		return diff[:idx] + "\n... (diff truncated)"
	}
	// No newline found — the entire first chunk is one long line. Trim bytes
	// from the end until we land on a valid UTF-8 boundary to avoid returning
	// a string with a split multi-byte rune.
	for len(candidate) > 0 && !utf8.ValidString(candidate) {
		candidate = candidate[:len(candidate)-1]
	}
	return candidate + "\n... (diff truncated)"
}

// ExtractNoChangesNeeded scans Smith output for the NO_CHANGES_NEEDED: marker
// and returns the reason string. Returns empty string if not found.
func ExtractNoChangesNeeded(output string) string {
	const marker = "NO_CHANGES_NEEDED:"
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, marker) {
			reason := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
			if reason != "" {
				return reason
			}
		}
	}
	return ""
}

// ExtractNeedsHuman scans Smith output for the NEEDS_HUMAN: marker and returns
// the reason string. Returns empty string if not found.
func ExtractNeedsHuman(output string) string {
	const marker = "NEEDS_HUMAN:"
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, marker) {
			reason := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
			if reason != "" {
				return reason
			}
		}
	}
	return ""
}

// ExtractRecheckPrevious scans Smith output for the RECHECK_PREVIOUS: marker
// and returns the rationale string. Smith uses this on review iterations to
// signal that the previous iteration's code is correct and the failure was
// environmental (already fixed or self-resolved). Returns empty string if not
// found.
func ExtractRecheckPrevious(output string) string {
	const marker = "RECHECK_PREVIOUS:"
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, marker) {
			reason := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
			if reason != "" {
				return reason
			}
		}
	}
	return ""
}

// formatWardenFeedback combines the review summary and structured issues into a
// single pre-formatted feedback string for inclusion in BeadContext.PriorFeedback.
func formatWardenFeedback(summary string, issues []warden.ReviewIssue) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString(summary)
	}

	if len(issues) > 0 {
		b.WriteString("\n\n### Specific Issues\n\n")
		for i, issue := range issues {
			fmt.Fprintf(&b, "%d. **[%s]** %s", i+1, issue.Severity, issue.Message)
			if issue.File != "" {
				fmt.Fprintf(&b, " (in `%s`", issue.File)
				if issue.Line > 0 {
					fmt.Fprintf(&b, " line %d", issue.Line)
				}
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
	}

	result := b.String()
	if result == "" {
		return "Warden requested changes but did not provide details."
	}
	return result
}

// wardenEventSummary produces a concise human-readable summary for the event log.
// It shows the actual review feedback (summary + issues) instead of internal
// parsing metadata like "Inferred request_changes from output (claude fallback)".
func wardenEventSummary(r *warden.ReviewResult) string {
	// Prefer the real summary if it doesn't look like an internal fallback message.
	summary := r.Summary
	if strings.HasPrefix(summary, "Inferred ") || strings.HasPrefix(summary, "Could not parse") {
		summary = ""
	}

	// Build a compact issues list.
	var parts []string
	if summary != "" {
		parts = append(parts, summary)
	}
	for _, issue := range r.Issues {
		msg := issue.Message
		if issue.File != "" {
			msg = fmt.Sprintf("%s (%s)", msg, issue.File)
		}
		parts = append(parts, msg)
	}

	if len(parts) == 0 {
		return "No details provided"
	}
	return strings.Join(parts, "; ")
}

// buildFixPrompt creates a prompt for Smith to fix issues found by Temper or Warden.
// Retained as a fallback if the prompt builder fails to rebuild on retry.
func buildFixPrompt(beadCtx prompt.BeadContext, source, summary string, issues []warden.ReviewIssue) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are continuing work on bead %s in the %s repository.

## Previous Attempt

Your previous implementation had issues identified by the %s:

%s

`, beadCtx.BeadID, beadCtx.AnvilName, source, summary)

	if beadCtx.PriorDiff != "" {
		fmt.Fprintf(&b, "## What Was Already Implemented\n\n```diff\n%s\n```\n\n", beadCtx.PriorDiff)
		b.WriteString("**IMPORTANT: Do NOT re-explore the codebase.** The diff above shows exactly what you\nchanged in your previous iteration. Go directly to the files listed in the feedback.\nDo NOT re-read unrelated files or re-explore the codebase.\n\n")
	}

	if len(issues) > 0 {
		b.WriteString("## Specific Issues to Fix\n\n")
		for i, issue := range issues {
			fmt.Fprintf(&b, "%d. **[%s]** %s", i+1, issue.Severity, issue.Message)
			if issue.File != "" {
				fmt.Fprintf(&b, " (in `%s`", issue.File)
				if issue.Line > 0 {
					fmt.Fprintf(&b, " line %d", issue.Line)
				}
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, `## Instructions

1. Fix ALL the issues listed above
2. Do NOT run builds, tests, or linters — Temper will verify automatically after you finish
3. Commit your fixes with a clear message
4. Push to the branch: %s

## Original Task

**Bead**: %s
**Title**: %s

%s

## Working Directory

You are working in: %s
Branch: %s
`,
		beadCtx.Branch,
		beadCtx.BeadID, beadCtx.Title, beadCtx.Description,
		beadCtx.WorktreePath, beadCtx.Branch)

	return b.String()
}

// PreserveWorktreeLogs copies smith log files from the worktree's .forge-logs
// directory to a persistent location at ~/.forge/logs/<beadID>/ before the
// worktree is removed. Returns the destination directory path, or an empty
// string if no logs were found. The DB log_path is updated by the caller to
// point to this persistent directory so post-mortem debugging remains possible
// after worktree cleanup.
func PreserveWorktreeLogs(worktreePath, beadID string) (string, error) {
	srcDir := filepath.Join(worktreePath, ".forge-logs")
	entries, err := os.ReadDir(srcDir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading log dir: %w", err)
	}

	// Filter to regular files only before creating any directories.
	// Use Type().IsRegular() rather than !IsDir() to also exclude symlinks,
	// which copyFile would follow into arbitrary locations.
	var fileEntries []os.DirEntry
	for _, e := range entries {
		if e.Type().IsRegular() {
			fileEntries = append(fileEntries, e)
		}
	}
	if len(fileEntries) == 0 {
		return "", nil
	}

	safeID := forge.SanitizeBeadID(beadID)

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	dstDir := filepath.Join(home, ".forge", "logs", safeID)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("creating persistent log dir: %w", err)
	}

	for _, entry := range fileEntries {
		src := filepath.Join(srcDir, entry.Name())
		dst := filepath.Join(dstDir, entry.Name())
		if err := copyFile(src, dst); err != nil {
			log.Printf("[pipeline:%s] Warning: failed to copy log %s: %v", beadID, entry.Name(), err)
		}
	}
	return dstDir, nil
}

// copyFile copies a single file from src to dst.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	return nil
}

// ExtractChangelogSummary attempts to parse a Smith changelog fragment from the
// worktree and returns the bullet points as a single string. It searches a
// prioritized set of filename patterns so repositories that use suffixed
// fragment names (e.g. <beadID>-technical.en.md) are covered in addition to the
// canonical exact-name variants. The canonical <beadID>.md is preferred over
// the legacy <beadID>.en.md fallback, plain <beadID>.nb.md remains lower
// priority, and suffixed variants are checked last.
func ExtractChangelogSummary(wtPath, beadID string) string {
	dir := filepath.Join(wtPath, "changelog.d")
	patterns := []string{
		beadID + ".md",
		beadID + ".en.md",
		beadID + ".nb.md",
		beadID + "-*.md",
		beadID + "-*.en.md",
		beadID + "-*.nb.md",
	}
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(dir, p))
		sort.Strings(matches) // deterministic pick when multiple files match
		for _, path := range matches {
			if frag, err := changelog.ParseFragment(path); err == nil && len(frag.Bullets) > 0 {
				return strings.Join(frag.Bullets, "\n")
			}
		}
	}
	return ""
}
