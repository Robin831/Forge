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
	"github.com/Robin831/Forge/internal/ghpr"
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

	ExtraFlags      []string
	Providers       []provider.Provider
	SchematicConfig *schematic.Config
	GoRaceDetection bool
	SmithTimeout    time.Duration

	// StatusCallback is called when crucible state changes (for TUI tracking).
	StatusCallback func(Status)

	// AutoMergeCrucibleChildren controls whether child PRs are automatically
	// merged (squash) into the feature branch after the pipeline succeeds.
	// When false, child PRs are created but not merged (human review required).
	// Default: true (zero value auto-merges).
	AutoMergeCrucibleChildren bool

	// Test injection points — when non-nil these replace the real implementations.
	PipelineRunner     func(ctx context.Context, p pipeline.Params) *pipeline.Outcome
	PRCreator          func(ctx context.Context, p ghpr.CreateParams) (*ghpr.PR, error)
	ChildFetcher       func(ctx context.Context, parentID, dir string) ([]poller.Bead, error)
	PRMerger           func(ctx context.Context, prNumber int, dir string) error
	BeadClaimer        func(ctx context.Context, beadID, dir string) error
	BeadCloser         func(ctx context.Context, beadID, dir string) error
	EpicBranchCreator  func(ctx context.Context, dir, branch string) error
}

// Status tracks the current state of a Crucible for monitoring.
type Status struct {
	ParentID          string
	Anvil             string
	Branch            string
	Phase             string // "started", "dispatching", "waiting", "final_pr", "complete", "paused"
	TotalChildren     int
	CompletedChildren int
	CurrentChild      string
	StartedAt         time.Time
}

// Result is returned when the Crucible finishes or pauses.
type Result struct {
	Success       bool
	FinalPR       *ghpr.PR // Non-nil when final PR was created successfully.
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

	// 2. Fetch children.
	children, err := p.fetchChildren(ctx, p.ParentBead.ID, anvilPath)
	if err != nil {
		return &Result{Error: fmt.Errorf("fetching children: %w", err)}
	}
	if len(children) == 0 {
		log.Info("no children found, creating final PR directly")
		// No children — create final PR from feature branch (which is identical to main).
		// This is a no-op edge case; just return success.
		return &Result{Success: true, ChildrenDone: 0, ChildrenTotal: 0}
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
		if childResult.Error != nil || !childResult.Success {
			reason := "pipeline failed"
			if childResult.Error != nil {
				reason = childResult.Error.Error()
			}
			log.Error("crucible child failed", "child", child.ID, "reason", reason)
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

		// Create PR from child branch → feature branch.
		pr, err := p.createPR(ctx, ghpr.CreateParams{
			WorktreePath: anvilPath,
			BeadID:       child.ID,
			Title:        fmt.Sprintf("%s (%s)", child.Title, child.ID),
			Branch:       childResult.Branch,
			Base:         branch,
			AnvilName:    p.AnvilName,
			DB:           p.DB,
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

	finalPR, err := p.createPR(ctx, ghpr.CreateParams{
		WorktreePath: anvilPath,
		BeadID:       p.ParentBead.ID,
		Title:        fmt.Sprintf("%s (%s)", p.ParentBead.Title, p.ParentBead.ID),
		Branch:       branch,
		Base:         "", // main (repo default)
		AnvilName:    p.AnvilName,
		DB:           p.DB,
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
// then fetches each child's full details.
func FetchChildren(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
	parent, err := FetchBead(ctx, parentID, dir)
	if err != nil {
		return nil, fmt.Errorf("fetching parent %s: %w", parentID, err)
	}

	if len(parent.Blocks) == 0 {
		return nil, nil
	}

	var children []poller.Bead
	for _, childID := range parent.Blocks {
		child, err := FetchBead(ctx, childID, dir)
		if err != nil {
			// Log and skip — a missing child shouldn't block the whole Crucible.
			slog.Warn("failed to fetch child bead", "parent_id", parentID, "child_id", childID, "dir", dir, "error", err)
			continue
		}
		// Only include open children (not already closed or in_progress by someone else).
		if strings.EqualFold(child.Status, "open") {
			children = append(children, child)
		}
	}

	return children, nil
}

// FetchBead calls `bd show <beadID> --json` and returns the parsed bead.
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

	var bead poller.Bead
	if err := json.Unmarshal(output, &bead); err != nil {
		return poller.Bead{}, fmt.Errorf("parsing bd show %s: %w", beadID, err)
	}
	return bead, nil
}

// createPR creates a pull request (or uses the injected PRCreator for testing).
func (p *Params) createPR(ctx context.Context, params ghpr.CreateParams) (*ghpr.PR, error) {
	if p.PRCreator != nil {
		return p.PRCreator(ctx, params)
	}
	return ghpr.Create(ctx, params)
}

// mergePR merges a PR using gh pr merge --squash, polling until the merge succeeds
// or the context is cancelled.
func (p *Params) mergePR(ctx context.Context, prNumber int, dir string) error {
	if p.PRMerger != nil {
		return p.PRMerger(ctx, prNumber, dir)
	}
	return MergePR(ctx, prNumber, dir)
}

// MergePR merges a PR by number using gh pr merge --squash. It retries with
// polling if the initial merge attempt fails (e.g. checks still running).
func MergePR(ctx context.Context, prNumber int, dir string) error {
	// Try immediate merge first.
	if err := attemptMerge(ctx, prNumber, dir); err == nil {
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
			if err := attemptMerge(ctx, prNumber, dir); err == nil {
				return nil
			}
			// Check if PR was already merged.
			if merged, _ := isPRMerged(ctx, prNumber, dir); merged {
				return nil
			}
		}
	}
}

// attemptMerge tries to merge a PR once.
func attemptMerge(ctx context.Context, prNumber int, dir string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "pr", "merge",
		fmt.Sprintf("%d", prNumber), "--squash", "--delete-branch"))
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge %d: %w: %s", prNumber, err, out)
	}
	return nil
}

// isPRMerged checks if a PR has been merged.
func isPRMerged(ctx context.Context, prNumber int, dir string) (bool, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "gh", "pr", "view",
		fmt.Sprintf("%d", prNumber), "--json", "state", "--jq", ".state"))
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "MERGED", nil
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

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "bd", "close", beadID, "--reason=Crucible child completed", "--json"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd close %s: %w: %s", beadID, err, out)
	}
	return nil
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
