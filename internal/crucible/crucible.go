package crucible

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// FeatureBranchPrefix is the branch prefix for Crucible feature branches.
const FeatureBranchPrefix = "feature/"

// Default poll interval when waiting for child PR merges.
const defaultMergePollInterval = 30 * time.Second

// Maximum time to wait for a single child PR merge before giving up.
const defaultMergeTimeout = 15 * time.Minute

// Params holds all dependencies needed to run a Crucible.
type Params struct {
	DB              *state.DB
	Logger          *slog.Logger
	WorktreeManager *worktree.Manager
	PromptBuilder   *prompt.Builder

	ParentBead  poller.Bead
	AnvilName   string
	AnvilConfig config.AnvilConfig

	ExtraFlags            []string
	Providers             []provider.Provider
	SchematicConfig       *schematic.Config
	GoRaceDetection       bool
	SmithTimeout          time.Duration
	MaxPipelineIterations int

	// WorkerID is the state DB worker record ID for this crucible run.
	// When set, the worker's PID and log_path are updated when the schematic
	// subprocess starts so that hearth can tail logs in real time.
	WorkerID string

	// StatusCallback is called when crucible state changes (for TUI tracking).
	StatusCallback func(Status)

	// AutoMergeCrucibleChildren controls whether child PRs are automatically
	// merged (squash) into the feature branch after the pipeline succeeds.
	// When false, child PRs are created but not merged (human review required).
	// Default: true (zero value auto-merges).
	AutoMergeCrucibleChildren bool

	// VCS is the VCS provider for PR operations. When nil, the default
	// GitHub provider is created lazily using DB.
	VCS vcs.Provider

	// Test injection points — when non-nil these replace the real implementations.
	PipelineRunner    func(ctx context.Context, p pipeline.Params) *pipeline.Outcome
	PRCreator         func(ctx context.Context, p vcs.CreateParams) (*vcs.PR, error)
	ChildFetcher      func(ctx context.Context, parentID, dir string) ([]poller.Bead, error)
	PRMerger          func(ctx context.Context, prNumber int, dir string) error
	BeadClaimer       func(ctx context.Context, beadID, dir string) error
	BeadCloser        func(ctx context.Context, beadID, dir string) error
	BeadResetter      func(ctx context.Context, beadID, dir string) error
	EpicBranchCreator func(ctx context.Context, dir, branch string) error
	SchematicRunner   func(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result
}

// Status tracks the current state of a Crucible for monitoring.
type Status struct {
	ParentID          string
	Anvil             string
	Branch            string
	Phase             string // "started", "parent", "dispatching", "waiting", "final_pr", "complete", "paused"
	TotalChildren     int
	CompletedChildren int
	CurrentChild      string
	StartedAt         time.Time
}

// Result is returned when the Crucible finishes or pauses.
type Result struct {
	Success       bool
	FinalPR       *vcs.PR // Non-nil when final PR was created successfully.
	ChildrenDone  int
	ChildrenTotal int
	Error         error
	PausedChildID string // Non-empty if paused due to child failure.
}

// Run orchestrates a parent bead's children on a feature branch.
//
// Flow: create feature branch → topo-sort children → dispatch each child
// (pipeline → PR → merge) → create final PR from feature branch to main.
func Run(ctx context.Context, p Params) *Result {
	log := p.Logger.With("crucible_parent", p.ParentBead.ID, "anvil", p.AnvilName)
	anvilPath := p.AnvilConfig.Path

	// Determine feature branch name.
	branch := FeatureBranchPrefix + sanitizeID(p.ParentBead.ID)
	log.Info("crucible started", "branch", branch)

	p.emitEvent(state.EventCrucibleStarted,
		fmt.Sprintf("Crucible started for %s — creating feature branch %s", p.ParentBead.ID, branch),
		p.ParentBead.ID)

	// 1. Create feature branch from main.
	if err := p.createEpicBranch(ctx, anvilPath, branch); err != nil {
		return &Result{Error: fmt.Errorf("creating feature branch: %w", err)}
	}

	// 1b. Run schematic on the parent bead to detect parent-has-work mode.
	parentHasWork := false
	if p.SchematicConfig != nil && len(p.Providers) > 0 {
		schemCfg := *p.SchematicConfig
		if p.DB != nil && p.WorkerID != "" {
			wID := p.WorkerID
			schemCfg.OnSpawn = func(pid int, logPath string) {
				if err := p.DB.UpdateWorkerPID(wID, pid); err != nil {
					slog.Warn("failed to record crucible schematic PID", "worker", wID, "err", err)
				}
				if err := p.DB.UpdateWorkerLogPath(wID, logPath); err != nil {
					slog.Warn("failed to record crucible schematic log path", "worker", wID, "err", err)
				}
			}
		}
		schemResult := p.runSchematic(ctx, schemCfg, p.ParentBead, anvilPath, p.Providers[0])
		if schemResult != nil && schemResult.Action == schematic.ActionPlan {
			parentHasWork = true
			log.Info("parent bead has own work, running parent-first mode", "plan_len", len(schemResult.Plan))
			p.emitEvent(state.EventCrucibleStarted,
				fmt.Sprintf("Crucible %s: parent has implementation work, running parent-first", p.ParentBead.ID),
				p.ParentBead.ID)

			p.updateStatus(Status{
				ParentID:  p.ParentBead.ID,
				Anvil:     p.AnvilName,
				Branch:    branch,
				Phase:     "parent",
				StartedAt: time.Now(),
			})

			// Run parent through the pipeline targeting the feature branch.
			parentOutcome := p.runChildPipeline(ctx, p.ParentBead, branch)

			if parentOutcome.NeedsHuman || parentOutcome.Error != nil || !parentOutcome.Success {
				reason := "parent pipeline failed"
				if parentOutcome.Error != nil {
					reason = parentOutcome.Error.Error()
				}
				// NoDiff from parent is acceptable — continue to children.
				if parentOutcome.ReviewResult != nil && parentOutcome.ReviewResult.NoDiff {
					log.Info("parent pipeline produced no diff, continuing to children")
					parentHasWork = false
				} else {
					log.Error("parent pipeline failed", "reason", reason)
					p.emitEvent(state.EventCruciblePaused,
						fmt.Sprintf("Crucible %s paused: parent pipeline failed — %s", p.ParentBead.ID, reason),
						p.ParentBead.ID)
					p.updateStatus(Status{
						ParentID: p.ParentBead.ID,
						Anvil:    p.AnvilName,
						Branch:   branch,
						Phase:    "paused",
					})
					return &Result{
						PausedChildID: p.ParentBead.ID,
						Error:         fmt.Errorf("parent pipeline failed: %s", reason),
					}
				}
			}

			// If parent produced changes, create a PR and merge into the feature branch.
			if parentHasWork && parentOutcome.Branch != "" {
				var changeSummary string
				if parentOutcome.ChangelogSummary != "" {
					changeSummary = parentOutcome.ChangelogSummary
				} else if parentOutcome.ReviewResult != nil && parentOutcome.ReviewResult.Summary != "" {
					changeSummary = parentOutcome.ReviewResult.Summary
				}

				pr, err := p.createPR(ctx, vcs.CreateParams{
					WorktreePath:    anvilPath,
					BeadID:          p.ParentBead.ID,
					Title:           fmt.Sprintf("%s (parent) (%s)", p.ParentBead.Title, p.ParentBead.ID),
					Branch:          parentOutcome.Branch,
					Base:            branch,
					AnvilName:       p.AnvilName,
					BeadTitle:       p.ParentBead.Title,
					BeadDescription: p.ParentBead.Description,
					BeadType:        p.ParentBead.IssueType,
					ChangeSummary:   changeSummary,
				})
				if err != nil {
					log.Error("failed to create parent PR", "error", err)
					// Continue — the branch changes are still pushed.
				} else if p.AutoMergeCrucibleChildren {
					if err := p.mergePR(ctx, pr.Number, anvilPath); err != nil {
						log.Error("failed to merge parent PR", "pr", pr.Number, "error", err)
						p.emitEvent(state.EventCruciblePaused,
							fmt.Sprintf("Crucible %s paused: parent PR #%d merge failed", p.ParentBead.ID, pr.Number),
							p.ParentBead.ID)
						p.updateStatus(Status{
							ParentID: p.ParentBead.ID,
							Anvil:    p.AnvilName,
							Branch:   branch,
							Phase:    "paused",
						})
						return &Result{
							PausedChildID: p.ParentBead.ID,
							Error:         fmt.Errorf("parent PR #%d merge failed: %w", pr.Number, err),
						}
					}
					log.Info("parent work merged into feature branch", "pr", pr.Number)
					p.emitEvent(state.EventCrucibleChildMerged,
						fmt.Sprintf("Crucible %s: parent work merged into %s via PR #%d", p.ParentBead.ID, branch, pr.Number),
						p.ParentBead.ID)
				}
			}
		} else if schemResult != nil && schemResult.Action == schematic.ActionClarify {
			log.Warn("parent bead needs clarification", "reason", schemResult.Reason)
			p.emitEvent(state.EventCruciblePaused,
				fmt.Sprintf("Crucible %s paused: parent needs clarification — %s", p.ParentBead.ID, schemResult.Reason),
				p.ParentBead.ID)
			return &Result{
				PausedChildID: p.ParentBead.ID,
				Error:         fmt.Errorf("parent needs clarification: %s", schemResult.Reason),
			}
		}
	}

	// 2. Fetch children.
	children, err := p.fetchChildren(ctx, p.ParentBead.ID, anvilPath)
	if err != nil {
		return &Result{Error: fmt.Errorf("fetching children: %w", err)}
	}
	if len(children) == 0 && !parentHasWork {
		log.Info("no children found and no parent work, nothing to do")
		return &Result{Success: true, ChildrenDone: 0, ChildrenTotal: 0}
	}
	if len(children) == 0 {
		log.Info("no children found but parent has work, skipping to final PR")
	}

	// 3. Topological sort.
	sorted, err := TopoSort(children)
	if err != nil {
		return &Result{Error: fmt.Errorf("sorting children: %w", err)}
	}

	log.Info("crucible children resolved", "count", len(sorted))
	p.emitEvent(state.EventCrucibleChildDispatched,
		fmt.Sprintf("Crucible %s: %d children queued on branch %s", p.ParentBead.ID, len(sorted), branch),
		p.ParentBead.ID)

	p.updateStatus(Status{
		ParentID:      p.ParentBead.ID,
		Anvil:         p.AnvilName,
		Branch:        branch,
		Phase:         "dispatching",
		TotalChildren: len(sorted),
		StartedAt:     time.Now(),
	})

	// 4. Dispatch each child in order.
	for i, child := range sorted {
		if ctx.Err() != nil {
			return &Result{
				Error:         ctx.Err(),
				ChildrenDone:  i,
				ChildrenTotal: len(sorted),
			}
		}

		// Skip children with unresolved external dependencies.
		if hasExternalBlockers(child, sorted, p.ParentBead.ID) {
			log.Warn("skipping child with external blockers", "child", child.ID)
			continue
		}

		// Auto-close orchestration meta-tasks that the Crucible already handles
		// (branch creation, committing/pushing, PR creation). These are workflow
		// steps intended for manual execution that the Crucible subsumes.
		if isOrchestrationTask(child) {
			log.Info("auto-closing orchestration meta-task", "child", child.ID, "title", child.Title)
			p.emitEvent(state.EventCrucibleChildMerged,
				fmt.Sprintf("Crucible %s: auto-closed orchestration task %s (%s)", p.ParentBead.ID, child.ID, child.Title),
				child.ID)
			if err := p.closeBead(ctx, child.ID, anvilPath); err != nil {
				log.Warn("failed to close orchestration task", "child", child.ID, "error", err)
			}
			continue
		}

		log.Info("dispatching crucible child", "child", child.ID, "index", i+1, "total", len(sorted))
		p.emitEvent(state.EventCrucibleChildDispatched,
			fmt.Sprintf("Crucible %s: dispatching child %s (%d/%d)", p.ParentBead.ID, child.ID, i+1, len(sorted)),
			child.ID)

		p.updateStatus(Status{
			ParentID:          p.ParentBead.ID,
			Anvil:             p.AnvilName,
			Branch:            branch,
			Phase:             "dispatching",
			TotalChildren:     len(sorted),
			CompletedChildren: i,
			CurrentChild:      child.ID,
			StartedAt:         time.Now(),
		})

		// Claim child bead.
		if err := p.claimBead(ctx, child.ID, anvilPath); err != nil {
			log.Error("failed to claim child", "child", child.ID, "error", err)
			continue // Skip this child rather than pausing the whole Crucible.
		}

		// Run pipeline for child, targeting the feature branch.
		childResult := p.runChildPipeline(ctx, child, branch)

		// NoDiff children (e.g. check-only tasks that investigate but produce
		// no code changes) are not failures — close them and continue.
		if childResult.NeedsHuman && childResult.ReviewResult != nil && childResult.ReviewResult.NoDiff {
			log.Info("crucible child produced no diff, closing and continuing", "child", child.ID)
			p.emitEvent(state.EventCrucibleChildMerged,
				fmt.Sprintf("Crucible %s: child %s completed with no changes (check-only)", p.ParentBead.ID, child.ID),
				child.ID)
			if err := p.closeBead(ctx, child.ID, anvilPath); err != nil {
				log.Warn("failed to close no-diff child bead", "child", child.ID, "error", err)
			}
			continue
		}

		if childResult.Error != nil || !childResult.Success {
			reason := "pipeline failed"
			if childResult.Error != nil {
				reason = childResult.Error.Error()
			}
			log.Error("crucible child failed", "child", child.ID, "reason", reason)

			// Reset the child bead to open so orphan recovery doesn't pick it up,
			// then mark it needs_human in state.db so the poller won't re-dispatch
			// it as a standalone bead outside crucible context.
			if err := p.resetBead(ctx, child.ID, anvilPath); err != nil {
				log.Warn("failed to reset failed child bead to open", "child", child.ID, "error", err)
			}
			p.markChildNeedsHuman(child.ID, reason)

			p.emitEvent(state.EventCrucibleChildFailed,
				fmt.Sprintf("Crucible %s: child %s failed — %s", p.ParentBead.ID, child.ID, reason),
				child.ID)
			p.emitEvent(state.EventCruciblePaused,
				fmt.Sprintf("Crucible %s paused: child %s failed", p.ParentBead.ID, child.ID),
				p.ParentBead.ID)

			p.updateStatus(Status{
				ParentID:          p.ParentBead.ID,
				Anvil:             p.AnvilName,
				Branch:            branch,
				Phase:             "paused",
				TotalChildren:     len(sorted),
				CompletedChildren: i,
				CurrentChild:      child.ID,
			})

			return &Result{
				ChildrenDone:  i,
				ChildrenTotal: len(sorted),
				PausedChildID: child.ID,
				Error:         fmt.Errorf("child %s failed: %s", child.ID, reason),
			}
		}

		// Build change summary preferring the changelog fragment, falling back to Warden review.
		var childChangeSummary string
		if childResult.ChangelogSummary != "" {
			childChangeSummary = childResult.ChangelogSummary
		} else if childResult.ReviewResult != nil && childResult.ReviewResult.Summary != "" {
			childChangeSummary = childResult.ReviewResult.Summary
		}

		// Create PR from child branch → feature branch.
		pr, err := p.createPR(ctx, vcs.CreateParams{
			WorktreePath:    anvilPath,
			BeadID:          child.ID,
			Title:           fmt.Sprintf("%s (%s)", child.Title, child.ID),
			Branch:          childResult.Branch,
			Base:            branch,
			AnvilName:       p.AnvilName,
			BeadTitle:       child.Title,
			BeadDescription: child.Description,
			BeadType:        child.IssueType,
			ChangeSummary:   childChangeSummary,
		})
		if err != nil {
			log.Error("failed to create child PR", "child", child.ID, "error", err)
			p.emitEvent(state.EventCrucibleChildFailed,
				fmt.Sprintf("Crucible %s: child %s PR creation failed — %v", p.ParentBead.ID, child.ID, err),
				child.ID)
			// Continue anyway — the branch changes are still pushed.
			continue
		}

		log.Info("crucible child PR created", "child", child.ID, "pr", pr.Number)
		p.emitEvent(state.EventCrucibleChildPRCreated,
			fmt.Sprintf("Crucible %s: child %s PR #%d created against %s", p.ParentBead.ID, child.ID, pr.Number, branch),
			child.ID)

		// Merge child PR into feature branch when auto-merge is enabled.
		// When AutoMergeCrucibleChildren is false, PRs are left open for human review.
		if !p.AutoMergeCrucibleChildren {
			log.Info("auto-merge disabled, skipping merge for child PR", "child", child.ID, "pr", pr.Number)
			continue
		}
		if err := p.mergePR(ctx, pr.Number, anvilPath); err != nil {
			log.Error("failed to merge child PR", "child", child.ID, "pr", pr.Number, "error", err)
			p.emitEvent(state.EventCrucibleChildFailed,
				fmt.Sprintf("Crucible %s: child %s PR #%d merge failed — %v", p.ParentBead.ID, child.ID, pr.Number, err),
				child.ID)
			p.emitEvent(state.EventCruciblePaused,
				fmt.Sprintf("Crucible %s paused: child %s PR merge failed", p.ParentBead.ID, child.ID),
				p.ParentBead.ID)

			return &Result{
				ChildrenDone:  i,
				ChildrenTotal: len(sorted),
				PausedChildID: child.ID,
				Error:         fmt.Errorf("child %s PR #%d merge failed: %w", child.ID, pr.Number, err),
			}
		}

		log.Info("crucible child merged", "child", child.ID, "pr", pr.Number)
		p.emitEvent(state.EventCrucibleChildMerged,
			fmt.Sprintf("Crucible %s: child %s merged into %s", p.ParentBead.ID, child.ID, branch),
			child.ID)

		// Close child bead.
		if err := p.closeBead(ctx, child.ID, anvilPath); err != nil {
			log.Warn("failed to close child bead", "child", child.ID, "error", err)
		}
	}

	// 5. Create final PR from feature branch → main.
	log.Info("all children complete, creating final PR", "branch", branch)
	p.updateStatus(Status{
		ParentID:          p.ParentBead.ID,
		Anvil:             p.AnvilName,
		Branch:            branch,
		Phase:             "final_pr",
		TotalChildren:     len(sorted),
		CompletedChildren: len(sorted),
	})

	finalPR, err := p.createPR(ctx, vcs.CreateParams{
		WorktreePath:    anvilPath,
		BeadID:          p.ParentBead.ID,
		Title:           fmt.Sprintf("%s (%s)", p.ParentBead.Title, p.ParentBead.ID),
		Branch:          branch,
		Base:            "", // empty = default branch (main); provider normalizes "" → "main"
		AnvilName:       p.AnvilName,
		BeadTitle:       p.ParentBead.Title,
		BeadDescription: p.ParentBead.Description,
		BeadType:        p.ParentBead.IssueType,
	})
	if err != nil {
		return &Result{
			ChildrenDone:  len(sorted),
			ChildrenTotal: len(sorted),
			Error:         fmt.Errorf("creating final PR: %w", err),
		}
	}

	log.Info("crucible final PR created", "pr", finalPR.Number, "url", finalPR.URL)
	p.emitEvent(state.EventCrucibleFinalPR,
		fmt.Sprintf("Crucible %s: final PR #%d created (%s → main)", p.ParentBead.ID, finalPR.Number, branch),
		p.ParentBead.ID)

	p.updateStatus(Status{
		ParentID:          p.ParentBead.ID,
		Anvil:             p.AnvilName,
		Branch:            branch,
		Phase:             "complete",
		TotalChildren:     len(sorted),
		CompletedChildren: len(sorted),
	})

	// Close the parent bead now that the final PR is created.
	if err := p.closeBead(ctx, p.ParentBead.ID, anvilPath); err != nil {
		log.Warn("failed to close parent bead", "parent", p.ParentBead.ID, "error", err)
	}

	return &Result{
		Success:       true,
		FinalPR:       finalPR,
		ChildrenDone:  len(sorted),
		ChildrenTotal: len(sorted),
	}
}

// runChildPipeline runs the pipeline for a single child bead targeting the feature branch.
func (p *Params) runChildPipeline(ctx context.Context, child poller.Bead, baseBranch string) *pipeline.Outcome {
	smithTimeout := p.SmithTimeout
	if smithTimeout <= 0 {
		smithTimeout = 30 * time.Minute
	}
	pipelineCtx, cancel := context.WithTimeout(ctx, smithTimeout)
	defer cancel()

	params := pipeline.Params{
		DB:              p.DB,
		WorktreeManager: p.WorktreeManager,
		PromptBuilder:   p.PromptBuilder,
		AnvilName:       p.AnvilName,
		AnvilConfig:     p.AnvilConfig,
		Bead:            child,
		ExtraFlags:      p.ExtraFlags,
		GoRaceDetection: p.GoRaceDetection,
		Providers:       p.Providers,
		BaseBranch:      baseBranch,
		SchematicConfig: p.SchematicConfig,
		MaxIterations:   p.MaxPipelineIterations,
	}

	if p.PipelineRunner != nil {
		return p.PipelineRunner(pipelineCtx, params)
	}
	return pipeline.Run(pipelineCtx, params)
}

// fetchChildren retrieves the child beads of a parent by calling bd show.
func (p *Params) fetchChildren(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
	if p.ChildFetcher != nil {
		return p.ChildFetcher(ctx, parentID, dir)
	}
	return FetchChildren(ctx, parentID, dir)
}

// FetchChildren calls `bd show <parentID> --json` to get the parent's blocks,
// then recursively fetches all descendants (children, grandchildren, etc.).
// This ensures the Crucible processes the entire dependency tree, not just
// direct children.
func FetchChildren(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
	parent, err := FetchBead(ctx, parentID, dir)
	if err != nil {
		return nil, fmt.Errorf("fetching parent %s: %w", parentID, err)
	}

	if len(parent.Blocks) == 0 {
		return nil, nil
	}

	// BFS to collect all descendants, avoiding cycles.
	seen := map[string]bool{parentID: true}
	queue := make([]string, len(parent.Blocks))
	copy(queue, parent.Blocks)

	var all []poller.Bead
	for len(queue) > 0 {
		childID := queue[0]
		queue = queue[1:]

		if seen[childID] {
			continue
		}
		seen[childID] = true

		child, err := FetchBead(ctx, childID, dir)
		if err != nil {
			slog.Warn("failed to fetch descendant bead", "parent_id", parentID, "child_id", childID, "dir", dir, "error", err)
			continue
		}

		// Only include open descendants.
		if strings.EqualFold(child.Status, "open") {
			all = append(all, child)
		}

		// Enqueue grandchildren.
		for _, gcID := range child.Blocks {
			if !seen[gcID] {
				queue = append(queue, gcID)
			}
		}
	}

	return all, nil
}

// bdShowBead extends the Bead with the raw dependents array that bd returns
// instead of a flat "blocks" field.
type bdShowBead struct {
	poller.Bead
	Dependents []struct {
		ID             string `json:"id"`
		DependencyType string `json:"dependency_type"`
	} `json:"dependents"`
}

// FetchBead calls `bd show <beadID> --json` and returns the parsed bead.
// bd show returns dependents as an array of objects with dependency_type,
// not a flat "blocks" array, so we extract blocks from the dependents.
func FetchBead(ctx context.Context, beadID, dir string) (poller.Bead, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "show", beadID, "--json"))
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return poller.Bead{}, fmt.Errorf("bd show %s: %w: %s", beadID, err, stderr.String())
	}

	// bd show --json may return an array with a single element: [{...}]
	output = unwrapJSONArray(output)

	var raw bdShowBead
	if err := json.Unmarshal(output, &raw); err != nil {
		return poller.Bead{}, fmt.Errorf("parsing bd show %s: %w", beadID, err)
	}

	bead := raw.Bead
	// Extract blocks from the dependents array.
	// Both "blocks" and "parent-child" dependency types indicate children.
	for _, dep := range raw.Dependents {
		if dep.DependencyType == "blocks" || dep.DependencyType == "parent-child" {
			bead.Blocks = append(bead.Blocks, dep.ID)
		}
	}
	return bead, nil
}

// unwrapJSONArray strips a wrapping JSON array if the output is `[{...}]`,
// returning just `{...}`. bd show --json returns an array with one element.
func unwrapJSONArray(data []byte) []byte {
	data = bytes.TrimSpace(data)
	if len(data) > 1 && data[0] == '[' {
		start := bytes.IndexByte(data, '{')
		end := bytes.LastIndexByte(data, '}')
		if start >= 0 && end > start {
			return data[start : end+1]
		}
	}
	return data
}

// runSchematic runs schematic analysis on a bead (or uses the injected SchematicRunner for testing).
func (p *Params) runSchematic(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result {
	if p.SchematicRunner != nil {
		return p.SchematicRunner(ctx, cfg, bead, anvilPath, pv)
	}
	return schematic.Run(ctx, cfg, bead, anvilPath, pv)
}

// createPR creates a pull request (or uses the injected PRCreator for testing).
func (p *Params) createPR(ctx context.Context, params vcs.CreateParams) (*vcs.PR, error) {
	if p.PRCreator != nil {
		return p.PRCreator(ctx, params)
	}
	if p.VCS != nil {
		return p.VCS.CreatePR(ctx, params)
	}
	return nil, fmt.Errorf("no VCS provider set on crucible Params")
}

// mergePR merges a PR via the VCS provider, polling until the merge succeeds
// or the context is cancelled.
func (p *Params) mergePR(ctx context.Context, prNumber int, dir string) error {
	if p.PRMerger != nil {
		return p.PRMerger(ctx, prNumber, dir)
	}

	prov := p.VCS
	if prov == nil {
		return fmt.Errorf("no VCS provider available for merge")
	}

	return MergePRWithProvider(ctx, prov, prNumber, dir)
}

// MergePRWithProvider merges a PR by number using the VCS provider. It retries
// with polling if the initial merge attempt fails (e.g. checks still running).
func MergePRWithProvider(ctx context.Context, prov vcs.Provider, prNumber int, dir string) error {
	// Try immediate merge first.
	if err := prov.MergePR(ctx, dir, prNumber, "squash"); err == nil {
		return nil
	}

	// Poll until merge succeeds or timeout.
	mergeCtx, cancel := context.WithTimeout(ctx, defaultMergeTimeout)
	defer cancel()

	ticker := time.NewTicker(defaultMergePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mergeCtx.Done():
			return fmt.Errorf("timed out waiting to merge PR #%d", prNumber)
		case <-ticker.C:
			if err := prov.MergePR(ctx, dir, prNumber, "squash"); err == nil {
				return nil
			}
			// Check if PR was already merged.
			if status, err := prov.CheckStatusLight(ctx, dir, prNumber); err == nil && status.IsMerged() {
				return nil
			}
		}
	}
}

// claimBead marks a bead as in_progress.
func (p *Params) claimBead(ctx context.Context, beadID, dir string) error {
	if p.BeadClaimer != nil {
		return p.BeadClaimer(ctx, beadID, dir)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "update", beadID, "--status=in_progress", "--json"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd update %s: %w: %s", beadID, err, out)
	}
	return nil
}

// closeBead marks a bead as closed.
func (p *Params) closeBead(ctx context.Context, beadID, dir string) error {
	if p.BeadCloser != nil {
		return p.BeadCloser(ctx, beadID, dir)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "close", beadID, "--force", "--reason=Crucible child completed", "--json"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd close %s: %w: %s", beadID, err, out)
	}
	return nil
}

// resetBead resets a bead back to open status.
func (p *Params) resetBead(ctx context.Context, beadID, dir string) error {
	if p.BeadResetter != nil {
		return p.BeadResetter(ctx, beadID, dir)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "update", beadID, "--status=open", "--json"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd update %s --status=open: %w: %s", beadID, err, out)
	}
	return nil
}

// markChildNeedsHuman marks a failed crucible child as needs_human in the
// state DB so the poller won't dispatch it as a standalone bead.
// It loads any existing retry record first to preserve retry counters.
func (p *Params) markChildNeedsHuman(beadID, reason string) {
	if p.DB == nil {
		return
	}
	log := p.Logger.With("child", beadID)

	// Load existing record to preserve retry counters (dispatch_failures, retry_count, etc.).
	rec, err := p.DB.GetRetry(beadID, p.AnvilName)
	if err != nil || rec == nil {
		if err != nil {
			log.Warn("failed to load existing retry record, creating new one", "error", err)
		}
		rec = &state.RetryRecord{
			BeadID: beadID,
			Anvil:  p.AnvilName,
		}
	}

	// Update only the fields we care about, preserving everything else.
	rec.NeedsHuman = true
	rec.LastError = fmt.Sprintf("crucible child failed: %s", reason)

	if err := p.DB.UpsertRetry(rec); err != nil {
		log.Error("failed to mark child as needs_human", "error", err)
	}
}

// createEpicBranch creates the feature branch, using the injected creator if set.
func (p *Params) createEpicBranch(ctx context.Context, dir, branch string) error {
	if p.EpicBranchCreator != nil {
		return p.EpicBranchCreator(ctx, dir, branch)
	}
	return p.WorktreeManager.CreateEpicBranch(ctx, dir, branch)
}

// emitEvent logs an event to the state DB.
func (p *Params) emitEvent(eventType state.EventType, msg, beadID string) {
	if p.DB != nil {
		_ = p.DB.LogEvent(eventType, msg, beadID, p.AnvilName)
	}
}

// updateStatus calls the StatusCallback if set.
func (p *Params) updateStatus(s Status) {
	if p.StatusCallback != nil {
		p.StatusCallback(s)
	}
}

// isOrchestrationTask returns true if a child bead describes a workflow step
// that the Crucible already handles automatically (branch creation, committing,
// pushing, PR creation). These meta-tasks are common in manually-planned epics
// but redundant when the Crucible orchestrates the work.
func isOrchestrationTask(b poller.Bead) bool {
	text := strings.ToLower(b.Title + " " + b.Description)

	// Branch creation — the Crucible creates the feature branch.
	if matchesOrchestrationPattern(text, []string{
		"create feature branch",
		"create branch",
		"checkout -b",
		"git checkout -b",
		"git fetch origin",
	}) {
		return true
	}

	// Committing and pushing — the pipeline handles git operations.
	if matchesOrchestrationPattern(text, []string{
		"commit and push",
		"git commit",
		"git push",
		"push to remote",
		"push changes",
	}) {
		// Only match if the task is purely about committing, not about
		// making changes AND committing.
		titleLower := strings.ToLower(b.Title)
		if strings.Contains(titleLower, "commit") ||
			strings.Contains(titleLower, "push") {
			return true
		}
	}

	// PR creation — the Crucible creates PRs for each child and the final PR.
	if matchesOrchestrationPattern(text, []string{
		"create pull request",
		"create pr",
		"gh pr create",
		"open pull request",
	}) {
		titleLower := strings.ToLower(b.Title)
		if strings.Contains(titleLower, "pull request") ||
			strings.Contains(titleLower, "create pr") {
			return true
		}
	}

	return false
}

// matchesOrchestrationPattern checks if the text contains any of the given patterns.
func matchesOrchestrationPattern(text string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// hasExternalBlockers returns true if the child has unresolved dependencies
// outside the sibling set and the parent.
func hasExternalBlockers(child poller.Bead, siblings []poller.Bead, parentID string) bool {
	siblingSet := make(map[string]struct{}, len(siblings))
	for _, s := range siblings {
		siblingSet[s.ID] = struct{}{}
	}
	for _, dep := range child.DependsOn {
		if dep == parentID {
			continue
		}
		if _, isSibling := siblingSet[dep]; isSibling {
			continue
		}
		// External dependency — we can't verify if it's resolved without fetching it.
		// For safety, treat any external dep as a potential blocker.
		return true
	}
	return false
}

// sanitizeID converts a bead ID to a safe branch name component.
func sanitizeID(id string) string {
	r := strings.NewReplacer(
		" ", "-",
		":", "-",
		"\\", "-",
		"/", "-",
	)
	return r.Replace(id)
}

// IsCrucibleCandidate returns true if a bead has children (blocks other beads)
// and is therefore a Crucible candidate.
func IsCrucibleCandidate(b poller.Bead) bool {
	return len(b.Blocks) > 0
}
