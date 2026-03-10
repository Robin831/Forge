package crucible

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/state"
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
	var createdPRs []ghpr.CreateParams
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

		PRCreator: func(ctx context.Context, pp ghpr.CreateParams) (*ghpr.PR, error) {
			prCounter++
			createdPRs = append(createdPRs, pp)
			return &ghpr.PR{Number: prCounter, URL: fmt.Sprintf("https://github.com/test/pr/%d", prCounter)}, nil
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
	}

	result := Run(context.Background(), p)

	if result.Error == nil {
		t.Fatal("expected error when child pipeline fails")
	}
	if result.PausedChildID != "child-1" {
		t.Errorf("expected paused child to be child-1, got %q", result.PausedChildID)
	}
}

func TestRun_NoDiffChild_ClosesAndContinues(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var closedBeads []string
	var createdPRs []ghpr.CreateParams

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:    "parent-1",
			Title: "Parent bead",
		},
		AnvilName:                "test-anvil",
		AnvilConfig:              config.AnvilConfig{Path: t.TempDir()},
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

		PRCreator: func(ctx context.Context, params ghpr.CreateParams) (*ghpr.PR, error) {
			createdPRs = append(createdPRs, params)
			return &ghpr.PR{Number: len(createdPRs)}, nil
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

func TestIsCrucibleCandidate(t *testing.T) {
	tests := []struct {
		name   string
		bead   poller.Bead
		expect bool
	}{
		{"no blocks", poller.Bead{ID: "a"}, false},
		{"empty blocks", poller.Bead{ID: "a", Blocks: []string{}}, false},
		{"has blocks", poller.Bead{ID: "a", Blocks: []string{"b"}}, true},
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
		name  string
		bead  poller.Bead
		want  bool
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

func TestRun_ParentHasWork_RunsParentFirst(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var pipelineOrder []string
	var createdPRs []ghpr.CreateParams
	var mergedPRs []int
	var closedBeads []string
	prCounter := 0

	schemCfg := &schematic.Config{Enabled: true, WordThreshold: 1}

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:          "parent-1",
			Title:       "Parent with own work",
			Description: "This parent has implementation work",
		},
		AnvilName:                "test-anvil",
		AnvilConfig:              config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,
		SchematicConfig:          schemCfg,
		Providers:                []provider.Provider{{Kind: "claude"}},

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil
		},

		SchematicRunner: func(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result {
			return &schematic.Result{
				Action: schematic.ActionPlan,
				Plan:   "Step 1: do parent work",
			}
		},

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "Child task", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			pipelineOrder = append(pipelineOrder, pp.Bead.ID)
			return &pipeline.Outcome{
				Success: true,
				Branch:  fmt.Sprintf("forge/%s", pp.Bead.ID),
			}
		},

		PRCreator: func(ctx context.Context, pp ghpr.CreateParams) (*ghpr.PR, error) {
			prCounter++
			createdPRs = append(createdPRs, pp)
			return &ghpr.PR{Number: prCounter, URL: fmt.Sprintf("https://github.com/test/pr/%d", prCounter)}, nil
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
	if !result.Success {
		t.Fatal("expected success")
	}

	// Parent should run BEFORE child.
	if len(pipelineOrder) != 2 {
		t.Fatalf("expected 2 pipeline runs, got %d: %v", len(pipelineOrder), pipelineOrder)
	}
	if pipelineOrder[0] != "parent-1" {
		t.Errorf("expected parent-1 first, got %s", pipelineOrder[0])
	}
	if pipelineOrder[1] != "child-1" {
		t.Errorf("expected child-1 second, got %s", pipelineOrder[1])
	}

	// PRs: parent PR (against feature branch) + child PR (against feature branch) + final PR
	if len(createdPRs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(createdPRs))
	}
	// Parent PR targets feature branch.
	if createdPRs[0].Base != "feature/parent-1" {
		t.Errorf("parent PR base should be feature/parent-1, got %s", createdPRs[0].Base)
	}
	// Child PR targets feature branch.
	if createdPRs[1].Base != "feature/parent-1" {
		t.Errorf("child PR base should be feature/parent-1, got %s", createdPRs[1].Base)
	}
	// Final PR targets main.
	if createdPRs[2].Base != "" {
		t.Errorf("final PR base should be empty (main), got %s", createdPRs[2].Base)
	}

	// Both parent and child PRs should be merged, plus final is not merged by crucible.
	if len(mergedPRs) != 2 {
		t.Errorf("expected 2 merged PRs (parent + child), got %d", len(mergedPRs))
	}
}

func TestRun_SchematicSkip_OrchestratorOnly(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var pipelineOrder []string
	prCounter := 0
	schemCfg := &schematic.Config{Enabled: true, WordThreshold: 1}

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:          "parent-1",
			Title:       "Pure orchestrator",
			Description: "This parent just coordinates children",
		},
		AnvilName:                "test-anvil",
		AnvilConfig:              config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,
		SchematicConfig:          schemCfg,
		Providers:                []provider.Provider{{Kind: "claude"}},

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil
		},

		SchematicRunner: func(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result {
			return &schematic.Result{
				Action: schematic.ActionSkip,
				Reason: "Simple orchestration bead",
			}
		},

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return []poller.Bead{
				{ID: "child-1", Title: "Child task", DependsOn: []string{"parent-1"}},
			}, nil
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			pipelineOrder = append(pipelineOrder, pp.Bead.ID)
			return &pipeline.Outcome{
				Success: true,
				Branch:  fmt.Sprintf("forge/%s", pp.Bead.ID),
			}
		},

		PRCreator: func(ctx context.Context, pp ghpr.CreateParams) (*ghpr.PR, error) {
			prCounter++
			return &ghpr.PR{Number: prCounter}, nil
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error {
			return nil
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error {
			return nil
		},

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			return nil
		},
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected success")
	}

	// Only child should run through pipeline (no parent pipeline run).
	if len(pipelineOrder) != 1 {
		t.Fatalf("expected 1 pipeline run, got %d: %v", len(pipelineOrder), pipelineOrder)
	}
	if pipelineOrder[0] != "child-1" {
		t.Errorf("expected child-1, got %s", pipelineOrder[0])
	}
}

func TestRun_ParentHasWork_NoChildren_CreatesFinalPR(t *testing.T) {
	db := testDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	var createdPRs []ghpr.CreateParams
	prCounter := 0
	schemCfg := &schematic.Config{Enabled: true, WordThreshold: 1}

	p := Params{
		DB:     db,
		Logger: logger,
		ParentBead: poller.Bead{
			ID:          "parent-1",
			Title:       "Parent with work, no children",
			Description: "This parent has work but no children to orchestrate",
		},
		AnvilName:                "test-anvil",
		AnvilConfig:              config.AnvilConfig{Path: t.TempDir()},
		AutoMergeCrucibleChildren: true,
		SchematicConfig:          schemCfg,
		Providers:                []provider.Provider{{Kind: "claude"}},

		EpicBranchCreator: func(ctx context.Context, dir, branch string) error {
			return nil
		},

		SchematicRunner: func(ctx context.Context, cfg schematic.Config, bead poller.Bead, anvilPath string, pv provider.Provider) *schematic.Result {
			return &schematic.Result{
				Action: schematic.ActionPlan,
				Plan:   "Step 1: do parent work",
			}
		},

		ChildFetcher: func(ctx context.Context, parentID, dir string) ([]poller.Bead, error) {
			return nil, nil // No children
		},

		PipelineRunner: func(ctx context.Context, pp pipeline.Params) *pipeline.Outcome {
			return &pipeline.Outcome{
				Success: true,
				Branch:  "forge/parent-1",
			}
		},

		PRCreator: func(ctx context.Context, pp ghpr.CreateParams) (*ghpr.PR, error) {
			prCounter++
			createdPRs = append(createdPRs, pp)
			return &ghpr.PR{Number: prCounter}, nil
		},

		PRMerger: func(ctx context.Context, prNumber int, dir string) error {
			return nil
		},

		BeadClaimer: func(ctx context.Context, beadID, dir string) error {
			return nil
		},

		BeadCloser: func(ctx context.Context, beadID, dir string) error {
			return nil
		},
	}

	result := Run(context.Background(), p)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Success {
		t.Fatal("expected success")
	}

	// Should have parent PR (merged to feature branch) + final PR (feature → main).
	if len(createdPRs) != 2 {
		t.Fatalf("expected 2 PRs (parent + final), got %d", len(createdPRs))
	}
	// Final PR targets main.
	if createdPRs[1].Base != "" {
		t.Errorf("final PR base should be empty (main), got %s", createdPRs[1].Base)
	}
}

func TestSanitizeID(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"simple", "simple"},
		{"with spaces", "with-spaces"},
		{"with:colons", "with-colons"},
		{"with\\backslashes", "with-backslashes"},
		{"Forge-abc", "Forge-abc"},
		{"project/123", "project-123"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeID(tt.input)
			if got != tt.expect {
				t.Errorf("sanitizeID(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}
