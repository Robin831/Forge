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
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
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
	// Providers is the ordered list of AI providers to try.
	// If empty, provider.Defaults() is used (Claude → Gemini).
	Providers []provider.Provider

	// BaseBranch overrides the base ref for worktree creation and PR
	// targeting. When set (e.g. for epic child beads), the worktree branches
	// from origin/<BaseBranch> and the PR targets this branch instead of the
	// repo default branch (origin/main or origin/master).
	BaseBranch string

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
	WardenReviewer func(ctx context.Context, wtPath, beadID, beadTitle, beadDescription, anvilPath string, db *state.DB, anvilName string, providers ...provider.Provider) (*warden.ReviewResult, error)
	// BeadReleaser overrides the default exec-based bd-update call for releasing
	// a bead back to open. Used in tests.
	BeadReleaser func(beadID, anvilPath string) error
	// SchematicRunner overrides schematic.Run. Used in tests.
	SchematicRunner func(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

// Run executes the full Smith → Temper → Warden pipeline for a bead.
func Run(ctx context.Context, p Params) *Outcome {
	start := time.Now()
	outcome := &Outcome{}
	workerID := p.WorkerID
	if workerID == "" {
		workerID = fmt.Sprintf("%s-%s-%d", p.AnvilName, p.Bead.ID, time.Now().Unix())
	}
	outcome.WorkerID = workerID

	// Resolve injectable dependencies, falling back to defaults.
	createWorktree := p.WorktreeCreator
	if createWorktree == nil {
		createWorktree = func(ctx context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
			return p.WorktreeManager.CreateWithOptions(ctx, anvilPath, beadID, worktree.CreateOptions{
				BaseBranch: p.BaseBranch,
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
	defer func() {
		log.Printf("[pipeline:%s] Cleaning up worktree", workerID)
		removeWorktree(ctx, p.AnvilConfig.Path, wt)
	}()

	// Record worker in state DB. Default phase to "smith"; if the Schematic
	// pre-worker is enabled it will be overwritten to "schematic" below.
	initialPhase := "smith"
	if p.SchematicConfig != nil {
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

	// Resolve provider list.
	providers := p.Providers
	if len(providers) == 0 {
		providers = provider.Defaults()
	}
	activeProviderIdx := 0

	// Build initial prompt context
	beadCtx := prompt.BeadContext{
		BeadID:       p.Bead.ID,
		Title:        p.Bead.Title,
		Description:  p.Bead.Description,
		IssueType:    p.Bead.IssueType,
		Priority:     p.Bead.Priority,
		Parent:       p.Bead.Parent,
		Branch:       wt.Branch,
		AnvilName:    p.AnvilName,
		AnvilPath:    p.AnvilConfig.Path,
		WorktreePath: wt.Path,
	}

	// Run Schematic pre-worker (optional)
	if p.SchematicConfig != nil {
		runSchematic := p.SchematicRunner
		if runSchematic == nil {
			runSchematic = schematic.Run
		}

		// Resolve per-anvil override
		schemCfg := *p.SchematicConfig
		if p.AnvilConfig.SchematicEnabled != nil {
			schemCfg.Enabled = *p.AnvilConfig.SchematicEnabled
		}

		if schematic.ShouldRun(schemCfg, p.Bead) {
			log.Printf("[pipeline:%s] Running Schematic pre-analysis", workerID)
			_ = p.DB.UpdateWorkerPhase(workerID, "schematic")
			_ = p.DB.LogEvent(state.EventSchematicStarted, "Analysing bead scope", p.Bead.ID, p.AnvilName)

			sResult := runSchematic(ctx, schemCfg, p.Bead, p.AnvilConfig.Path, providers[0])
			outcome.SchematicResult = sResult

			// Persist provider quota from the schematic's claude session.
			if sResult.Quota != nil {
				if err := p.DB.UpsertProviderQuota(string(providers[0].Kind), sResult.Quota); err != nil {
					log.Printf("[pipeline:%s] Failed to update provider %s quota from schematic: %v", workerID, providers[0].Label(), err)
				}
			}

			// Record Copilot premium request for schematic if applicable.
			if providers[0].Kind == provider.Copilot && sResult.Action != schematic.ActionSkip {
				if m := cost.CopilotPremiumMultiplier(providers[0].Model); m > 0 {
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

			default:
				// ActionSkip or unknown — continue without plan
				log.Printf("[pipeline:%s] Schematic skipped: %s", workerID, sResult.Reason)
				_ = p.DB.LogEvent(state.EventSchematicSkipped, sResult.Reason, p.Bead.ID, p.AnvilName)
			}
		} else {
			log.Printf("[pipeline:%s] Schematic not needed for this bead", workerID)
		}
	}

	// Build Smith prompt (with optional Schematic plan injected)
	customTmpl := prompt.LoadCustomTemplate(p.AnvilConfig.Path)
	if customTmpl != "" {
		p.PromptBuilder.CustomTemplate = customTmpl
	}

	beadCtx.Iteration = 1
	promptText, err := p.PromptBuilder.Build(beadCtx)
	if err != nil {
		outcome.Error = fmt.Errorf("building prompt: %w", err)
		outcome.Duration = time.Since(start)
		_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
		return outcome
	}

	// Resolve max iterations: prefer the param value (from config), fall back to the constant.
	maxIter := p.MaxIterations
	if maxIter <= 0 {
		maxIter = MaxIterations
	}

	// Feedback loop
	var currentPrompt = promptText

	for iteration := 1; iteration <= maxIter; iteration++ {
		outcome.Iterations = iteration
		log.Printf("[pipeline:%s] Iteration %d/%d", workerID, iteration, maxIter)

		// Run Smith (with provider fallback on rate limit)
		log.Printf("[pipeline:%s] Running Smith (provider: %s)", workerID, providers[activeProviderIdx].Label())
		_ = p.DB.UpdateWorkerPhase(workerID, "smith")
		_ = p.DB.LogEvent(state.EventSmithStarted, fmt.Sprintf("Iteration %d (provider: %s)", iteration, providers[activeProviderIdx].Label()), p.Bead.ID, p.AnvilName)

		logDir := wt.Path + "/.forge-logs"

		var smithResult *smith.Result
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
				outcome.Duration = time.Since(start)
				return outcome
			}
			_ = p.DB.UpdateWorkerPID(workerID, process.PID)
			_ = p.DB.UpdateWorkerLogPath(workerID, process.LogPath)
			smithResult = process.Wait()

			// A process that exits 0, or produces a genuine success result event
			// (subtype:"success" with is_error:false), completed successfully. The
			// Claude CLI handles internal retries for rate limits and resumes
			// automatically. Any rate_limit_event or non-zero exit code we saw was
			// either a warning or a transient block that resolved.
			// IMPORTANT: subtype:"success" + is_error:true is a hard rate-limit
			// rejection — Claude returns this when it couldn't start the session.
			// Do NOT clear RateLimited in that case; fall back to another provider.
			if smithResult.ExitCode == 0 || (smithResult.ResultSubtype == "success" && !smithResult.IsError) {
				smithResult.RateLimited = false
			}

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

			if !smithResult.RateLimited {
				activeProviderIdx = pi // remember for the next iteration
				break
			}
		}
		outcome.SmithResult = smithResult

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
				outcome.Duration = time.Since(start)
				return outcome
			}
			log.Printf("[pipeline:%s] Bead %s released back to open", workerID, p.Bead.ID)
			outcome.Error = fmt.Errorf("all providers (%d) are rate limited", len(providers))
			outcome.RateLimited = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
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
			outcome.Duration = time.Since(start)
			return outcome
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

		// Step 3: Run Temper (build/test)
		log.Printf("[pipeline:%s] Running Temper verification", workerID)
		_ = p.DB.UpdateWorkerPhase(workerID, "temper")
		temperCfg := p.TemperConfig
		if temperCfg == nil {
			detected := temper.DefaultConfigWithRace(wt.Path, temper.DetectOptionsFromAnvilFlag(p.AnvilConfig.GolangciLint), p.GoRaceDetection)
			temperCfg = &detected
		}

		temperResult := runTemper(ctx, wt.Path, *temperCfg, p.DB, p.Bead.ID, p.AnvilName)
		outcome.TemperResult = temperResult

		if !temperResult.Passed {
			log.Printf("[pipeline:%s] Temper failed at step: %s", workerID, temperResult.FailedStep)

			if iteration < maxIter {
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
			outcome.Duration = time.Since(start)
			return outcome
		}

		// Step 4: Run Warden review
		log.Printf("[pipeline:%s] Running Warden review", workerID)
		_ = p.DB.UpdateWorkerPhase(workerID, "warden")
		_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerReviewing)

		reviewResult, err := reviewWarden(ctx, wt.Path, p.Bead.ID, p.Bead.Title, p.Bead.Description, p.AnvilConfig.Path, p.DB, p.AnvilName, providers...)
		if err != nil {
			log.Printf("[pipeline:%s] Warden error: %v", workerID, err)
			// Warden failure is not fatal — default to approve and let human review
			outcome.ReviewResult = &warden.ReviewResult{
				Verdict: warden.VerdictApprove,
				Summary: "Warden failed, defaulting to approve for human review",
			}
			outcome.Verdict = warden.VerdictApprove
			outcome.Success = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerDone)
			_ = p.DB.LogEvent(state.EventWardenPass, "Warden failed, defaulting to approve", p.Bead.ID, p.AnvilName)
			outcome.Duration = time.Since(start)
			return outcome
		}

		outcome.ReviewResult = reviewResult

		// Record Copilot premium request for warden review if applicable.
		if reviewResult.UsedProvider != nil && reviewResult.UsedProvider.Kind == provider.Copilot {
			multiplier := cost.CopilotPremiumMultiplier(reviewResult.UsedProvider.Model)
			if multiplier > 0 {
				if err := p.DB.AddCopilotRequest(cost.Today(), multiplier); err != nil {
					log.Printf("[pipeline:%s] Failed to record copilot premium request for warden: %v", workerID, err)
				}
			}
		}

		switch reviewResult.Verdict {
		case warden.VerdictApprove:
			log.Printf("[pipeline:%s] Warden approved", workerID)
			outcome.Verdict = warden.VerdictApprove
			outcome.Success = true
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerMonitoring)
			_ = p.DB.UpdateWorkerPhase(workerID, "bellows")
			_ = p.DB.LogEvent(state.EventWardenPass, reviewResult.Summary, p.Bead.ID, p.AnvilName)

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

			if iteration < maxIter {
				// Rebuild prompt with warden feedback for next iteration
				beadCtx.Iteration = iteration + 1
				beadCtx.PriorFeedbackSource = "Warden code review"
				beadCtx.PriorFeedback = formatWardenFeedback(reviewResult.Summary, reviewResult.Issues)
				if rebuilt, err := p.PromptBuilder.Build(beadCtx); err == nil {
					currentPrompt = rebuilt
				} else {
					log.Printf("[pipeline:%s] Failed to rebuild prompt, using fallback: %v", workerID, err)
					currentPrompt = buildFixPrompt(beadCtx, "review", reviewResult.Summary, reviewResult.Issues)
				}
				_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerRunning)
				continue
			}

			// Max iterations reached with request_changes
			outcome.Verdict = warden.VerdictRequestChanges
			outcome.Error = fmt.Errorf("warden still requesting changes after %d iterations", iteration)
			_ = p.DB.UpdateWorkerStatus(workerID, state.WorkerFailed)
			outcome.Duration = time.Since(start)
			return outcome
		}
	}

	outcome.Duration = time.Since(start)
	return outcome
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
