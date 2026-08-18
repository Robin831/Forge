package crucible

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/warden"
)

// testDB creates a temporary state DB for testing.
func testDB(t *testing.T) *state.DB {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := state.Open(tmpDir + "/test.db")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRun_NoChildren(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:   "test-anvil",
		AnvilConfig: config.AnvilConfig{Path: t.TempDir()},
		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil // succeed without git
		},
		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return nil, nil // No children
		},
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected success when no children")
	}
	if result.ChildrenDone != 0 {
		t.Errorf("expected 0 children done, got %d", result.ChildrenDone)
	}
}

func TestRun_WithChildren_MockPipeline(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var dispatchedChildren []string
	var createdPRs []vcs.CreateParams
	var mergedPRs []int
	var closedBeads []string
	prCounter := 0

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil // succeed without git
		},

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-2", Title: "Second child", DependsOn: []string{"child-1", "parent-1"}},
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			dispatchedChildren = append(dispatchedChildren, pp.Bead.ID)
			return &pipeline.Outcome{
				Success: true,
				Branch:  fmt.Sprintf("forge/%s", pp.Bead.ID),
			}
		},

		PRCreator: func(ctx context.Context, pp vcs.CreateParams) (*vcs.PR, error) {
			prCounter++
			createdPRs = append(createdPRs, pp)
			return &vcs.PR{Number: prCounter, URL: fmt.Sprintf("https://github.com/test/pr/%d", prCounter)}, nil
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error {
			mergedPRs = append(mergedPRs, prNumber)
			return nil
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error {
			return nil
		},

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			closedBeads = append(closedBeads, beadID)
			return nil
		},
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	// Verify child dispatch order: child-1 before child-2 (topo sorted).
	if len(dispatchedChildren) != 2 {
		t.Fatalf("expected 2 dispatches, got %d", len(dispatchedChildren))
	}
	if dispatchedChildren[0] != "child-1" {
		t.Errorf("expected child-1 first, got %s", dispatchedChildren[0])
	}
	if dispatchedChildren[1] != "child-2" {
		t.Errorf("expected child-2 second, got %s", dispatchedChildren[1])
	}

	// Verify child PRs target the feature branch.
	for _, pr := range createdPRs[:2] { // First 2 are child PRs
		if pr.Base != "feature/parent-1" {
			t.Errorf("child PR base should be feature/parent-1, got %s", pr.Base)
		}
	}

	// Verify final PR targets main (empty base).
	if len(createdPRs) >= 3 && createdPRs[2].Base != "" {
		t.Errorf("final PR base should be empty (main), got %s", createdPRs[2].Base)
	}

	// Verify children were merged.
	if len(mergedPRs) != 2 {
		t.Errorf("expected 2 merged PRs, got %d", len(mergedPRs))
	}

	// Verify children + parent were closed.
	if len(closedBeads) != 3 {
		t.Errorf("expected 3 closed beads (2 children + parent), got %d", len(closedBeads))
	}

	// Verify result.
	if !result.Success {
		t.Error("expected success")
	}
	if result.ChildrenTotal != 2 {
		t.Errorf("expected 2 children total, got %d", result.ChildrenTotal)
	}
}

func TestRun_ChildPipelineFailure_Pauses(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var resetBeads []string

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:   "test-anvil",
		AnvilConfig: config.AnvilConfig{Path: t.TempDir()},

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil // succeed without git
		},

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			return &pipeline.Outcome{
				Success: false,
				Error:   fmt.Errorf("smith failed"),
			}
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error {
			return nil
		},

		BeadResetter: func(ctx context.Context, beadID, dir string) error {
			resetBeads = append(resetBeads, beadID)
			return nil
		},
	}

	result := Run(context.Background(), p)

	if result.Error == nil {
		t.Fatal("expected error when child pipeline fails")
	}
	if result.PausedChildID != "child-1" {
		t.Errorf("expected paused child to be child-1, got %q", result.PausedChildID)
	}

	// Verify child bead was reset to open.
	if len(resetBeads) != 1 || resetBeads[0] != "child-1" {
		t.Errorf("expected child-1 to be reset, got %v", resetBeads)
	}

	// Verify child bead is marked needs_human in state DB.
	retry, err := db.GetRetry("child-1", "test-anvil")
	if err != nil {
		t.Fatalf("failed to get retry record: %v", err)
	}
	if retry == nil {
		t.Fatal("expected retry record for child-1, got nil")
	}
	if !retry.NeedsHuman {
		t.Error("expected child-1 to be marked needs_human")
	}
	if retry.LastError == "" {
		t.Error("expected last_error to contain failure reason")
	}
}

func TestRun_NoDiffChild_ClosesAndContinues(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var closedBeads []string
	var createdPRs []vcs.CreateParams

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil
		},

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "check-child", Title: "Check something", DependsOn: []string{"parent-1"}},
				{ID: "code-child", Title: "Do code work", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			if pp.Bead.ID == "check-child" {
				// NoDiff child — check-only task with no code changes
				return &pipeline.Outcome{
					Success:    false,
					NeedsHuman: true,
					ReviewResult: &warden.ReviewResult{
						Verdict: warden.VerdictReject,
						NoDiff:  true,
						Summary: "No changes detected",
					},
				}
			}
			return &pipeline.Outcome{
				Success: true,
				Branch:  "worktree-branch",
			}
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error {
			return nil
		},

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			closedBeads = append(closedBeads, beadID)
			return nil
		},

		PRCreator: func(ctx context.Context, params vcs.CreateParams) (*vcs.PR, error) {
			createdPRs = append(createdPRs, params)
			return &vcs.PR{Number: len(createdPRs)}, nil
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error {
			return nil
		},
	}

	result := Run(context.Background(), p)

	if !result.Success {
		t.Fatalf("expected success, got error: %v", result.Error)
	}
	// check-child should be closed (no-diff), code-child should be closed (merged),
	// plus parent closed = 3 total
	if len(closedBeads) != 3 {
		t.Errorf("expected 3 closed beads, got %d: %v", len(closedBeads), closedBeads)
	}
	// Only code-child + final PR should be created (not check-child since it had no changes)
	if len(createdPRs) != 2 {
		t.Errorf("expected 2 PRs (code-child + final), got %d", len(createdPRs))
	}
}

// TestRun_ChildPRCreationFailure_Pauses verifies that a child PR-creation
// failure pauses the Crucible (setting PausedChildID) instead of continuing on
// to ship a final PR without that child's work. Retrying once must not swallow
// a persistent failure, and the parent must stay open.
func TestRun_ChildPRCreationFailure_Pauses(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var closedBeads []string
	prAttempts := 0

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error { return nil },

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			return &pipeline.Outcome{Success: true, Branch: "forge/child-1"}
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error { return nil },

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			closedBeads = append(closedBeads, beadID)
			return nil
		},

		PRCreator: func(ctx context.Context, params vcs.CreateParams) (*vcs.PR, error) {
			prAttempts++
			return nil, fmt.Errorf("gh timed out")
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error { return nil },
	}

	result := Run(context.Background(), p)

	if result.Error == nil {
		t.Fatal("expected error when child PR creation fails")
	}
	if result.Success {
		t.Error("expected Success=false when a child PR could not be created")
	}
	if result.PausedChildID != "child-1" {
		t.Errorf("expected paused child to be child-1, got %q", result.PausedChildID)
	}
	// One automatic retry means exactly two attempts for the failing child, and
	// no final-PR attempt (which would be a third).
	if prAttempts != 2 {
		t.Errorf("expected 2 PR creation attempts (initial + retry), got %d", prAttempts)
	}
	if result.FinalPR != nil {
		t.Error("expected no final PR when a child PR failed")
	}
	// The parent bead must NOT be closed while the epic is incomplete.
	for _, b := range closedBeads {
		if b == "parent-1" {
			t.Error("parent bead must not be closed when a child PR failed")
		}
	}
}

// TestRun_ChildPRCreationFailure_RetrySucceeds verifies the single automatic
// retry absorbs a transient PR-creation failure: the second attempt succeeds
// and the Crucible completes normally without pausing.
func TestRun_ChildPRCreationFailure_RetrySucceeds(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var closedBeads []string
	prAttempts := 0

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error { return nil },

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			return &pipeline.Outcome{Success: true, Branch: "forge/child-1"}
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error { return nil },

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			closedBeads = append(closedBeads, beadID)
			return nil
		},

		PRCreator: func(ctx context.Context, params vcs.CreateParams) (*vcs.PR, error) {
			prAttempts++
			// Fail only the very first attempt (transient), succeed thereafter.
			if prAttempts == 1 {
				return nil, fmt.Errorf("transient gh failure")
			}
			return &vcs.PR{Number: prAttempts, URL: fmt.Sprintf("https://github.com/test/pr/%d", prAttempts)}, nil
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error { return nil },
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("expected success after retry, got error: %v", result.Error)
	}
	if !result.Success {
		t.Error("expected Success=true after the retry succeeds")
	}
	if result.PausedChildID != "" {
		t.Errorf("expected no paused child, got %q", result.PausedChildID)
	}
	// child-1: attempt 1 fails, attempt 2 succeeds; final PR: attempt 3.
	if prAttempts != 3 {
		t.Errorf("expected 3 PR creation attempts (child retry + final), got %d", prAttempts)
	}
	if result.ChildrenDone != 1 {
		t.Errorf("expected 1 child done, got %d", result.ChildrenDone)
	}
	if result.ChildrenSkipped != 0 {
		t.Errorf("expected 0 children skipped, got %d", result.ChildrenSkipped)
	}
}

// TestRun_SkippedChild_DoesNotShipIncompleteEpic verifies that a child skipped
// for external blockers leaves the epic incomplete: the Crucible pauses instead
// of shipping a final PR, the parent is not closed, and the accounting
// distinguishes completed from skipped children.
func TestRun_SkippedChild_DoesNotShipIncompleteEpic(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var closedBeads []string
	var createdPRs []vcs.CreateParams

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error { return nil },

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				// child-1 completes normally.
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
				// child-2 has an unresolved external dependency and is skipped.
				{ID: "child-2", Title: "Second child", DependsOn: []string{"parent-1", "external-dep"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			return &pipeline.Outcome{Success: true, Branch: fmt.Sprintf("forge/%s", pp.Bead.ID)}
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error { return nil },

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			closedBeads = append(closedBeads, beadID)
			return nil
		},

		PRCreator: func(ctx context.Context, params vcs.CreateParams) (*vcs.PR, error) {
			createdPRs = append(createdPRs, params)
			return &vcs.PR{Number: len(createdPRs)}, nil
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error { return nil },
	}

	result := Run(context.Background(), p)

	if result.Error == nil {
		t.Fatal("expected error when a child is skipped")
	}
	if result.Success {
		t.Error("expected Success=false when a child was skipped")
	}
	if result.ChildrenDone != 1 {
		t.Errorf("expected 1 child completed, got %d", result.ChildrenDone)
	}
	if result.ChildrenSkipped != 1 {
		t.Errorf("expected 1 child skipped, got %d", result.ChildrenSkipped)
	}
	if result.PausedChildID != "child-2" {
		t.Errorf("expected paused child to be child-2, got %q", result.PausedChildID)
	}
	// The parent bead must NOT be closed while a child was skipped.
	for _, b := range closedBeads {
		if b == "parent-1" {
			t.Error("parent bead must not be closed when a child was skipped")
		}
	}
	// Only child-1's PR should have been created — no final PR for the incomplete epic.
	for _, pr := range createdPRs {
		if pr.BeadID == "parent-1" {
			t.Error("no final PR should be created for an incomplete epic")
		}
	}
}

// TestIsCrucibleCandidate covers the inverted default (Forge-fblf): children
// alone are not enough — the parent must opt in with the "crucible" label or an
// explicit "epic-branch:<name>".
func TestIsCrucibleCandidate(t *testing.T) {
	tests := []struct {
		name   string
		bead   poller.Bead
		expect bool
	}{
		{"no blocks", poller.Bead{ID: "a"}, false},
		{"empty blocks", poller.Bead{ID: "a", Blocks: []string{}}, false},
		{"has blocks, no opt-in label", poller.Bead{ID: "a", Blocks: []string{"b"}}, false},
		{"epic type, no opt-in label", poller.Bead{ID: "a", IssueType: "epic", Blocks: []string{"b"}}, false},
		{"opt-in label, no blocks", poller.Bead{ID: "a", Labels: []string{"crucible"}}, false},
		{"has blocks + crucible label", poller.Bead{ID: "a", Blocks: []string{"b"}, Labels: []string{"crucible"}}, true},
		{"has blocks + epic-branch label", poller.Bead{ID: "a", Blocks: []string{"b"}, Labels: []string{"epic-branch:foo"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCrucibleCandidate(tt.bead)
			if got != tt.expect {
				t.Errorf("IsCrucibleCandidate() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestIsOrchestrationTask(t *testing.T) {
	tests := []struct {
		name string
		bead poller.Bead
		want bool
	}{
		{"branch creation", poller.Bead{Title: "Create feature branch", Description: "git checkout -b feature/foo"}, true},
		{"commit and push", poller.Bead{Title: "Commit and push changes", Description: "git add && git commit"}, true},
		{"create PR", poller.Bead{Title: "Create pull request", Description: "gh pr create --title ..."}, true},
		{"actual work", poller.Bead{Title: "Update API packages", Description: "dotnet add package Foo"}, false},
		{"check task", poller.Bead{Title: "Check outdated API packages", Description: "dotnet list package --outdated"}, false},
		{"run tests", poller.Bead{Title: "Run API tests and format", Description: "dotnet test"}, false},
		{"update changelogs", poller.Bead{Title: "Update changelogs", Description: "Update CHANGELOG.md"}, false},
		{"push in description only", poller.Bead{Title: "Update client packages", Description: "ncu -u && npm install && git push"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOrchestrationTask(tt.bead)
			if got != tt.want {
				t.Errorf("isOrchestrationTask(%q) = %v, want %v", tt.bead.Title, got, tt.want)
			}
		})
	}
}

func TestHasExternalBlockers(t *testing.T) {
	siblings := []poller.Bead{
		{ID: "child-1"},
		{ID: "child-2"},
	}

	tests := []struct {
		name     string
		child    poller.Bead
		parentID string
		expect   bool
	}{
		{
			"no deps",
			poller.Bead{ID: "child-1"},
			"parent-1",
			false,
		},
		{
			"only parent dep",
			poller.Bead{ID: "child-1", DependsOn: []string{"parent-1"}},
			"parent-1",
			false,
		},
		{
			"sibling dep",
			poller.Bead{ID: "child-2", DependsOn: []string{"parent-1", "child-1"}},
			"parent-1",
			false,
		},
		{
			"external dep",
			poller.Bead{ID: "child-1", DependsOn: []string{"parent-1", "external-bead"}},
			"parent-1",
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasExternalBlockers(tt.child, siblings, tt.parentID)
			if got != tt.expect {
				t.Errorf("hasExternalBlockers() = %v, want %v", got, tt.expect)
			}
		})
	}
}

// TestRun_SchematicOnSpawn_UpdatesWorkerPIDAndLogPath verifies that when
// Crucible's SchematicRunner invokes cfg.OnSpawn, the worker record in
// state.db is updated with the subprocess PID and log_path. This is the
// contract that lets Hearth tail logs during the crucible schematic phase.
func TestRun_SchematicOnSpawn_UpdatesWorkerPIDAndLogPath(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	const workerID = "crucible-spawn-worker"

	// Seed a worker row so UpdateWorkerPID/LogPath have a target row to update.
	if err := db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    "parent-1",
		Anvil:     "test-anvil",
		Status:    state.WorkerRunning,
		Phase:     "crucible",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seeding worker row: %v", err)
	}

	p := Params{
		DB:       db,
		Logger:   logger,
		WorkerID: workerID,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:       "test-anvil",
		AnvilConfig:     config.AnvilConfig{Path: t.TempDir()},
		Providers:       []provider.Provider{{Kind: provider.Claude}},
		SchematicConfig: &schematic.Config{Enabled: true},
		SchematicRunner: func(_ context.Context, cfg schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
			if cfg.OnSpawn != nil {
				cfg.OnSpawn(54321, "/fake/crucible.log")
			}
			return &schematic.Result{Action: schematic.ActionSkip, Reason: "skip"}
		},
		EpicBranchCreator: func(_ context.Context, _, _ string) error {
			return nil
		},
		ChildFetcher: func(_ context.Context, _, _ string) ([]poller.Bead, error) {
			return nil, nil // no children — crucible exits cleanly after schematic
		},
	}

	result := Run(context.Background(), p)
	if result.Error != nil {
		t.Fatalf("unexpected crucible error: %v", result.Error)
	}

	workers, err := db.AllWorkers(0)
	if err != nil {
		t.Fatalf("querying workers: %v", err)
	}

	var found *state.Worker
	for i := range workers {
		if workers[i].ID == workerID {
			found = &workers[i]
			break
		}
	}
	if found == nil {
		t.Fatal("worker row not found in state.db after crucible run")
	}
	if found.PID != 54321 {
		t.Errorf("expected worker PID 54321, got %d", found.PID)
	}
	if found.LogPath != "/fake/crucible.log" {
		t.Errorf("expected worker LogPath /fake/crucible.log, got %q", found.LogPath)
	}
}

// TestRun_ChildEmptyBranch_SkipsPRAndContinues verifies that a child whose work
// already landed on the feature branch (empty branch, no commits vs base) is
// closed and counted as done instead of pausing the epic — and, critically,
// that no PR is opened for it.
func TestRun_ChildEmptyBranch_SkipsPRAndContinues(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var createdPRs []vcs.CreateParams
	var closedBeads []string

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                 "test-anvil",
		AnvilConfig:               config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,
		EmptyDiffAction:           config.EmptyDiffActionClose,

		EpicBranchCreator: func(_ context.Context, _, _ string) error { return nil },

		ChildFetcher: func(_ context.Context, _, _ string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "First child", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(_ context.Context, pp pipeline.Params) *pipeline.Outcome {
			if pp.EmptyDiffAction != config.EmptyDiffActionClose {
				t.Errorf("child pipeline should inherit empty_diff_action, got %q", pp.EmptyDiffAction)
			}
			return &pipeline.Outcome{
				EmptyDiff:       true,
				EmptyDiffAction: config.EmptyDiffActionClose,
				EmptyDiffBase:   "origin/feature/parent-1",
				Branch:          "forge/child-1",
			}
		},

		PRCreator: func(_ context.Context, params vcs.CreateParams) (*vcs.PR, error) {
			createdPRs = append(createdPRs, params)
			return &vcs.PR{Number: len(createdPRs), URL: "https://example.test/pr"}, nil
		},

		PRMerger:    func(_ context.Context, _ int, _ string) error { return nil },
		BeadClaimer: func(_ context.Context, _, _ string) error { return nil },
		BeadCloser: func(_ context.Context, beadID, _ string) error {
			closedBeads = append(closedBeads, beadID)
			return nil
		},
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Error("an empty-branch child must not fail the epic")
	}
	if result.ChildrenDone != 1 {
		t.Errorf("expected the empty-branch child to count as done, got %d", result.ChildrenDone)
	}
	// Only the final epic PR may exist — never one for the empty child branch.
	for _, pr := range createdPRs {
		if pr.Branch == "forge/child-1" {
			t.Error("no PR may be created for a branch with no commits")
		}
	}
	found := false
	for _, id := range closedBeads {
		if id == "child-1" {
			found = true
		}
	}
	if !found {
		t.Error("the empty-branch child should be closed")
	}
}

// TestRun_BranchNameMatchesPoller is the regression test for the epic/ vs
// feature/ mismatch (Forge-fblf): the Crucible built "feature/<id>" while the
// poller stamped children with "epic/<id>", so a child dispatched
// independently failed with "base branch not found on origin" and burned the
// dispatch circuit breaker. Both now derive the name from epic.BranchName.
func TestRun_BranchNameMatchesPoller(t *testing.T) {
	tests := []struct {
		name   string
		bead   poller.Bead
		expect string
	}{
		{
			name:   "derived name",
			bead:   poller.Bead{ID: "parent-1", IssueType: "epic", Labels: []string{"crucible"}},
			expect: "feature/parent-1",
		},
		{
			name:   "explicit epic-branch label wins",
			bead:   poller.Bead{ID: "parent-1", Labels: []string{"crucible", "epic-branch:foo"}},
			expect: "foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

			var createdBranch string
			p := Params{
				DB:          db,
				Logger:      logger,
				ParentBead:  tt.bead,
				AnvilName:   "test-anvil",
				AnvilConfig: config.AnvilConfig{Path: t.TempDir()},
				EpicBranchCreator: func(_ context.Context, _, branch string) error {
					createdBranch = branch
					return nil
				},
				ChildFetcher: func(_ context.Context, _, _ string) ([]poller.Bead, error) {
					return nil, nil
				},
			}

			if result := Run(context.Background(), p); result.Error != nil {
				t.Fatalf("unexpected error: %v", result.Error)
			}
			if createdBranch != tt.expect {
				t.Errorf("crucible branch = %q, want %q", createdBranch, tt.expect)
			}
			if got := poller.ExtractParentBranch(tt.bead); got != createdBranch {
				t.Errorf("poller derives %q but the Crucible created %q — children would base on a branch that never exists", got, createdBranch)
			}
		})
	}
}
