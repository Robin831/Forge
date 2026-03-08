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
	"github.com/Robin831/Forge/internal/state"
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

	// Verify children were closed.
	if len(closedBeads) != 2 {
		t.Errorf("expected 2 closed beads, got %d", len(closedBeads))
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
