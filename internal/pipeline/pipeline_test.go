package pipeline

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDB opens a temporary SQLite state DB for testing.
func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeWorktree returns a WorktreeCreator that creates an in-memory worktree
// backed by a temp dir, without running any git commands.
func fakeWorktreeCreator(t *testing.T) func(ctx context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
	t.Helper()
	return func(_ context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
		return &worktree.Worktree{
			BeadID:    beadID,
			AnvilPath: anvilPath,
			Path:      t.TempDir(),
			Branch:    "forge/" + beadID,
		}, nil
	}
}

func noopRemover(_ context.Context, _ string, _ *worktree.Worktree) {}

// immediateSmith returns a SmithRunner that resolves immediately with the
// given result, without spawning any external process.
func immediateSmith(result *smith.Result) func(context.Context, string, string, string, provider.Provider, []string) (*smith.Process, error) {
	return func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(result), nil
	}
}

// passingTemper returns a TemperRunner that always reports success.
func passingTemper() func(context.Context, string, temper.Config, *state.DB, string, string) *temper.Result {
	return func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		return &temper.Result{Passed: true}
	}
}

// baseParams builds a Params with all external calls mocked and a recording
// BeadReleaser. It is the baseline for all NoDiff/rate-limit tests.
func baseParams(t *testing.T, db *state.DB) (Params, *string, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var releasedBeadID string

	p := Params{
		DB:        db,
		AnvilName: "test-anvil",
		AnvilConfig: config.AnvilConfig{
			Path: t.TempDir(),
		},
		Bead: poller.Bead{
			ID:    "test-bead",
			Title: "Test bead",
		},
		PromptBuilder:   prompt.NewBuilder(),
		WorktreeCreator: fakeWorktreeCreator(t),
		WorktreeRemover: noopRemover,
		SmithRunner:     immediateSmith(&smith.Result{ExitCode: 0}),
		TemperRunner:    passingTemper(),
		BeadReleaser: func(beadID, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			releasedBeadID = beadID
			return nil
		},
		Providers: []provider.Provider{{Kind: provider.Claude}},
	}
	return p, &releasedBeadID, &mu
}

// TestNoDiff_ReleasesBeadToOpen verifies that when Warden returns NoDiff=true,
// the bead is released back to open via BeadReleaser and the outcome has
// NeedsHuman=true.
func TestNoDiff_ReleasesBeadToOpen(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{
			Verdict: warden.VerdictReject,
			NoDiff:  true,
			Summary: "No changes detected — Smith produced no diff",
		}, nil
	}

	outcome := Run(context.Background(), params)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "test-bead", *releasedID, "BeadReleaser must be called with the correct bead ID")
	assert.True(t, outcome.NeedsHuman, "NeedsHuman should be true after NoDiff rejection")
	assert.Equal(t, warden.VerdictReject, outcome.Verdict)
	assert.False(t, outcome.Success)
}

// TestNoDiff_NeedsHumanFalse_WhenReleaseFails verifies that when
// BeadReleaser fails, NeedsHuman remains false (it is only set when the
// release succeeds).
func TestNoDiff_NeedsHumanFalse_WhenReleaseFails(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{
			Verdict: warden.VerdictReject,
			NoDiff:  true,
			Summary: "No changes detected",
		}, nil
	}
	params.BeadReleaser = func(beadID, _ string) error {
		return assert.AnError
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.NeedsHuman, "NeedsHuman should be false when BeadReleaser fails")
	assert.Equal(t, warden.VerdictReject, outcome.Verdict)
}

// TestNoDiff_BeadReleaser_IgnoresCancelledPipelineCtx verifies that the
// BeadReleaser is still called even when the pipeline context is already
// cancelled. This guards against the regression where release was derived
// from the pipeline ctx (which might have timed out).
func TestNoDiff_BeadReleaser_IgnoresCancelledPipelineCtx(t *testing.T) {
	db := newTestDB(t)

	var released bool
	var mu sync.Mutex

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run() is called

	params := Params{
		DB:        db,
		AnvilName: "test-anvil",
		AnvilConfig: config.AnvilConfig{
			Path: t.TempDir(),
		},
		Bead: poller.Bead{
			ID:    "ctx-bead",
			Title: "Context test bead",
		},
		PromptBuilder: prompt.NewBuilder(),
		// WorktreeCreator ignores ctx so we get past the worktree step
		// despite the cancelled context.
		WorktreeCreator: fakeWorktreeCreator(t),
		WorktreeRemover: noopRemover,
		// SmithRunner ignores ctx
		SmithRunner: immediateSmith(&smith.Result{ExitCode: 0}),
		// TemperRunner ignores ctx
		TemperRunner: passingTemper(),
		// WardenReviewer ignores ctx and returns NoDiff
		WardenReviewer: func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
			return &warden.ReviewResult{
				Verdict: warden.VerdictReject,
				NoDiff:  true,
				Summary: "No changes detected",
			}, nil
		},
		BeadReleaser: func(beadID, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			released = true
			return nil
		},
		Providers: []provider.Provider{{Kind: provider.Claude}},
	}

	outcome := Run(cancelledCtx, params)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, released, "BeadReleaser must be called even with a cancelled pipeline context")
	assert.True(t, outcome.NeedsHuman)
}

// TestRateLimited_ReleasesBeadToOpen verifies that when all providers are rate
// limited, the bead is released back to open and the outcome has RateLimited=true.
func TestRateLimited_ReleasesBeadToOpen(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	// Make the smith runner return a rate-limited result.
	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:    1,
		RateLimited: true,
	})

	// Warden should not be called for rate-limited path, but set it anyway.
	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	outcome := Run(context.Background(), params)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "test-bead", *releasedID, "BeadReleaser must be called for rate-limited bead")
	assert.True(t, outcome.RateLimited, "outcome.RateLimited should be true")
	assert.NotNil(t, outcome.Error)
	assert.False(t, outcome.Success)
}

// TestWardenApprove_Success verifies the happy path where Warden approves.
func TestWardenApprove_Success(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{
			Verdict: warden.VerdictApprove,
			Summary: "Looks good!",
		}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.Equal(t, warden.VerdictApprove, outcome.Verdict)
	assert.Nil(t, outcome.Error)
	assert.False(t, outcome.NeedsHuman)
	assert.False(t, outcome.RateLimited)
}

// TestReleaseBead_UsesBackgroundContext is a regression test for the context-
// cancellation bug. It verifies that releaseBead uses context.Background()
// internally, so a cancelled/expired caller context does not prevent the
// release from happening.
func TestReleaseBead_UsesBackgroundContext(t *testing.T) {
	t.Skip("Skipped: this test relies on external `bd` command and is non-hermetic; behavior should be verified via injected BeadReleaser/command wrapper tests.")
}

// TestBuildFixPrompt_WithIssues verifies that buildFixPrompt includes all
// issue details when issues are provided.
func TestBuildFixPrompt_WithIssues(t *testing.T) {
	bc := prompt.BeadContext{
		BeadID:       "test-123",
		Title:        "Add feature X",
		Description:  "Implement feature X as described.",
		AnvilName:    "my-anvil",
		Branch:       "forge/test-123",
		WorktreePath: "/tmp/worktrees/test-123",
	}

	issues := []warden.ReviewIssue{
		{Severity: "medium", Message: "Missing tests", File: "foo.go", Line: 42},
		{Severity: "low", Message: "Unused import", File: "bar.go"},
	}

	got := buildFixPrompt(bc, "review", "Two issues found.", issues)

	assert.Contains(t, got, "test-123")
	assert.Contains(t, got, "my-anvil")
	assert.Contains(t, got, "review")
	assert.Contains(t, got, "Two issues found.")
	assert.Contains(t, got, "[medium]")
	assert.Contains(t, got, "Missing tests")
	assert.Contains(t, got, "foo.go")
	assert.Contains(t, got, "line 42")
	assert.Contains(t, got, "[low]")
	assert.Contains(t, got, "bar.go")
	assert.NotContains(t, got, "line 0", "zero line number should not be printed")
	assert.Contains(t, got, "forge/test-123")
}

// TestBuildFixPrompt_NoIssues verifies that buildFixPrompt works without issues.
func TestBuildFixPrompt_NoIssues(t *testing.T) {
	bc := prompt.BeadContext{
		BeadID:    "bead-abc",
		Title:     "Fix bug",
		AnvilName: "repo",
		Branch:    "forge/bead-abc",
	}

	got := buildFixPrompt(bc, "build/test", "Build failed.", nil)

	assert.Contains(t, got, "bead-abc")
	assert.Contains(t, got, "build/test")
	assert.Contains(t, got, "Build failed.")
	assert.NotContains(t, got, "## Specific Issues to Fix")
}

// TestSchematic_Plan_InjectsIntoSmithPrompt verifies that when Schematic
// produces a plan, it is included in the Smith prompt.
func TestSchematic_Plan_InjectsIntoSmithPrompt(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	var capturedPrompt string
	params.SmithRunner = func(_ context.Context, _, promptText, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		capturedPrompt = promptText
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Implement the foo feature with bar integration"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action: schematic.ActionPlan,
			Plan:   "1. Create foo.go\n2. Add bar client\n3. Write tests",
			Reason: "Focused single task",
		}
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.NotNil(t, outcome.SchematicResult)
	assert.Equal(t, schematic.ActionPlan, outcome.SchematicResult.Action)
	assert.Contains(t, capturedPrompt, "Create foo.go")
	assert.Contains(t, capturedPrompt, "Implementation Plan")
}

// TestSchematic_Decompose_ExitsEarly verifies that when Schematic decomposes
// a bead, the pipeline exits early without running Smith.
func TestSchematic_Decompose_ExitsEarly(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	smithCalled := false
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCalled = true
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Multiple independent features to implement"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action:   schematic.ActionDecompose,
			SubBeads: []string{"sub-1", "sub-2"},
			Reason:   "Too large, splitting",
		}
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Decomposed)
	assert.False(t, smithCalled, "Smith should not run when bead is decomposed")
	assert.Equal(t, schematic.ActionDecompose, outcome.SchematicResult.Action)
	assert.Nil(t, outcome.Error)
}

// TestSchematic_Clarify_ReleasesBeadAndSetsNeedsHuman verifies that when
// Schematic says clarification is needed, the bead is released.
func TestSchematic_Clarify_ReleasesBeadAndSetsNeedsHuman(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Ambiguous requirements that need more context"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action: schematic.ActionClarify,
			Reason: "Missing acceptance criteria",
		}
	}

	outcome := Run(context.Background(), params)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, outcome.NeedsHuman)
	assert.Equal(t, "test-bead", *releasedID)
	assert.Equal(t, schematic.ActionClarify, outcome.SchematicResult.Action)
}

// TestSchematic_Clarify_NeedsHumanFalse_WhenReleaseFails verifies that when
// Schematic says clarification is needed but releasing the bead fails,
// NeedsHuman remains false (mirroring the NoDiff path behavior).
func TestSchematic_Clarify_NeedsHumanFalse_WhenReleaseFails(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	// Simulate a failure in BeadReleaser.
	params.BeadReleaser = func(beadID, _ string) error {
		return assert.AnError
	}

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Ambiguous requirements that need more context"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action: schematic.ActionClarify,
			Reason: "Missing acceptance criteria",
		}
	}

	outcome := Run(context.Background(), params)

	mu.Lock()
	defer mu.Unlock()
	assert.False(t, outcome.NeedsHuman, "NeedsHuman should be false when BeadReleaser fails")
	assert.Empty(t, *releasedID, "bead should not be marked as released when BeadReleaser fails")
	assert.Equal(t, schematic.ActionClarify, outcome.SchematicResult.Action)
}

// TestSchematic_Skip_ContinuesToSmith verifies that when Schematic skips,
// the pipeline continues normally to Smith.
func TestSchematic_Skip_ContinuesToSmith(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	smithCalled := false
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCalled = true
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Simple task"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action: schematic.ActionSkip,
			Reason: "Simple enough",
		}
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.True(t, smithCalled, "Smith should run when Schematic skips")
}

// TestSmith_NeedsHuman_ReleasesBeadAndSetsFlag verifies that when Smith outputs
// the NEEDS_HUMAN: marker, the bead is released and NeedsHuman is set.
func TestSmith_NeedsHuman_ReleasesBeadAndSetsFlag(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "I investigated the task but cannot proceed.\nNEEDS_HUMAN: Missing API credentials for the payment service\nStopping here.",
	})
	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		t.Fatal("Warden should not be called when Smith escalates")
		return nil, nil
	}

	outcome := Run(context.Background(), params)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "test-bead", *releasedID, "BeadReleaser must be called with the correct bead ID")
	assert.True(t, outcome.NeedsHuman, "NeedsHuman should be true when Smith escalates")
	assert.False(t, outcome.Success)
	assert.Nil(t, outcome.Error)
}

// TestSmith_NeedsHuman_NotTriggeredWithoutMarker verifies that normal Smith
// output without the NEEDS_HUMAN marker proceeds to Temper/Warden as usual.
func TestSmith_NeedsHuman_NotTriggeredWithoutMarker(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "Implemented the feature successfully.\nAll changes committed and pushed.",
	})
	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.False(t, outcome.NeedsHuman)
}

// TestSmith_NeedsHuman_ReleaseFails verifies that NeedsHuman stays false when
// the bead release fails after a NEEDS_HUMAN escalation.
func TestSmith_NeedsHuman_ReleaseFails(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "NEEDS_HUMAN: Cannot resolve ambiguous requirements",
	})
	params.BeadReleaser = func(_, _ string) error {
		return assert.AnError
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.NeedsHuman, "NeedsHuman should be false when release fails")
}

// TestExtractNeedsHuman verifies the NEEDS_HUMAN marker extraction logic.
func TestExtractNeedsHuman(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"marker with reason", "NEEDS_HUMAN: Missing API key", "Missing API key"},
		{"marker mid-output", "Some text\nNEEDS_HUMAN: Ambiguous spec\nMore text", "Ambiguous spec"},
		{"marker with leading spaces", "  NEEDS_HUMAN: Indented reason", "Indented reason"},
		{"no marker", "Normal output without escalation", ""},
		{"marker without reason", "NEEDS_HUMAN:", ""},
		{"partial marker", "NEEDS_HUMANOID: not a real marker", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractNeedsHuman(tt.input)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// TestSchematic_PerAnvilDisable verifies that per-anvil SchematicEnabled=false
// overrides the global setting.
func TestSchematic_PerAnvilDisable(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	schematicCalled := false
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		schematicCalled = true
		return &schematic.Result{Action: schematic.ActionSkip}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "A task with enough words to trigger the threshold"

	// Per-anvil disable
	disabled := false
	params.AnvilConfig.SchematicEnabled = &disabled

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.False(t, schematicCalled, "Schematic should not run when per-anvil disabled")
}
