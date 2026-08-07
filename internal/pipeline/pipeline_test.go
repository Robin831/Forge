package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
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
		// Stub the steer note append so tests never shell out to the bd CLI.
		SteerNoteAppender: func(_, _, _ string) error { return nil },
		Providers:         []provider.Provider{{Kind: provider.Claude}},
	}
	return p, &releasedBeadID, &mu
}

// TestNoDiff_ReleasesBeadToOpen verifies that when Warden returns NoDiff=true,
// the bead is released back to open via BeadReleaser and the outcome has
// NeedsHuman=true.
func TestNoDiff_ReleasesBeadToOpen(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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

	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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
		WardenReviewer: func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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

// TestAuthFailed_EscalatesWithoutRelease verifies that an authentication
// failure is escalated for human attention (AuthFailed + NeedsHuman) rather than
// released back to open for retry — a bad credential fails identically on every
// attempt, so releasing it would loop forever (Forge-d5ns).
func TestAuthFailed_EscalatesWithoutRelease(t *testing.T) {
	db := newTestDB(t)
	params, releasedID, mu := baseParams(t, db)

	// Multiple providers configured — an auth failure must NOT rotate through
	// them (unlike a rate limit). It should stop on the first provider.
	params.Providers = []provider.Provider{{Kind: provider.Claude}, {Kind: provider.Gemini}}

	var smithCalls int
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCalls++
		return smith.NewProcessForTest(&smith.Result{
			ExitCode:    2,
			AuthFailed:  true,
			ErrorOutput: "Error: Invalid API key provided",
		}), nil
	}

	outcome := Run(context.Background(), params)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, outcome.AuthFailed, "outcome.AuthFailed should be true")
	assert.True(t, outcome.NeedsHuman, "auth failure must escalate for human attention")
	assert.NotEmpty(t, outcome.AuthProvider, "the failing provider must be named")
	assert.NotNil(t, outcome.Error)
	assert.False(t, outcome.Success)
	assert.False(t, outcome.RateLimited, "auth failure is not a rate limit")
	assert.Empty(t, *releasedID, "auth failure must NOT release the bead back to open")
	assert.Equal(t, 1, smithCalls, "auth failure must not rotate through providers")
}

// TestWardenApprove_Success verifies the happy path where Warden approves.
// It also verifies that the changelog fragment summary is extracted.
func TestWardenApprove_Success(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.WorktreeCreator = func(_ context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
		return &worktree.Worktree{
			BeadID:    beadID,
			AnvilPath: anvilPath,
			Path:      t.TempDir(),
			Branch:    "forge/" + beadID,
		}, nil
	}

	params.WardenReviewer = func(_ context.Context, wtPath, beadID, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		// Create a fake changelog fragment in the worktree.
		changelogDir := filepath.Join(wtPath, "changelog.d")
		require.NoError(t, os.MkdirAll(changelogDir, 0o755))
		content := "category: Added\n- **Feature X** - Detailed description of feature X.\n- **Feature Y** - Detailed description of feature Y.\n"
		require.NoError(t, os.WriteFile(filepath.Join(changelogDir, beadID+".md"), []byte(content), 0o644))

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

	// Verify changelog summary extraction.
	assert.Contains(t, outcome.ChangelogSummary, "**Feature X**")
	assert.Contains(t, outcome.ChangelogSummary, "**Feature Y**")
	assert.Contains(t, outcome.ChangelogSummary, "\n")
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
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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
			SubBeads: []schematic.SubBead{{ID: "sub-1", Title: "Task A"}, {ID: "sub-2", Title: "Task B"}},
			Reason:   "Too large, splitting",
		}
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Decomposed)
	assert.False(t, smithCalled, "Smith should not run when bead is decomposed")
	assert.Equal(t, schematic.ActionDecompose, outcome.SchematicResult.Action)
	assert.Nil(t, outcome.Error)
}

// TestSchematic_Decompose_LogsEvents verifies that when Schematic decomposes a bead,
// a summary EventSchematicDone event containing sub-bead details is logged, and one
// EventSchematicSubBead event is written per sub-bead.
func TestSchematic_Decompose_LogsEvents(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Multiple independent features to implement"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action:   schematic.ActionDecompose,
			SubBeads: []schematic.SubBead{{ID: "sub-1", Title: "Task A"}, {ID: "sub-2", Title: "Task B"}},
			Reason:   "Too large, splitting",
		}
	}

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Decomposed)

	events, err := db.RecentEvents(20)
	require.NoError(t, err)

	// Collect events by type
	var summaryEvents, subBeadEvents []state.Event
	for _, e := range events {
		switch e.Type {
		case state.EventSchematicDone:
			summaryEvents = append(summaryEvents, e)
		case state.EventSchematicSubBead:
			subBeadEvents = append(subBeadEvents, e)
		}
	}

	require.Len(t, summaryEvents, 1, "exactly one EventSchematicDone summary event expected")
	// Events are ordered DESC by timestamp, then ID. The summary event is logged *before* the sub-bead events.
	assert.Contains(t, summaryEvents[0].Message, `[{"id":"sub-1","title":"Task A"},{"id":"sub-2","title":"Task B"}]`)

	require.Len(t, subBeadEvents, 2, "one EventSchematicSubBead event per sub-bead expected")
	// Events are ordered DESC, so subBeadEvents[0] is the *last* one logged ((2/2)).
	assert.Contains(t, subBeadEvents[0].Message, "(2/2)")
	assert.Contains(t, subBeadEvents[0].Message, "sub-2")
	assert.Contains(t, subBeadEvents[0].Message, "Task B")
	assert.Contains(t, subBeadEvents[1].Message, "(1/2)")
	assert.Contains(t, subBeadEvents[1].Message, "sub-1")
	assert.Contains(t, subBeadEvents[1].Message, "Task A")
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

	// Verify that clarification_needed flag was set in DB with the correct reason
	r, err := db.GetRetry("test-bead", "test-anvil")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.ClarificationNeeded)
	assert.Equal(t, "Missing acceptance criteria", r.LastError)
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
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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
			got := ExtractNeedsHuman(tt.input)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// TestExtractNoChangesNeeded verifies the NO_CHANGES_NEEDED marker extraction logic.
func TestExtractNoChangesNeeded(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"marker with reason", "NO_CHANGES_NEEDED: Already fixed in commit abc123", "Already fixed in commit abc123"},
		{"marker mid-output", "Some text\nNO_CHANGES_NEEDED: Issue resolved upstream\nMore text", "Issue resolved upstream"},
		{"marker with leading spaces", "  NO_CHANGES_NEEDED: Indented reason", "Indented reason"},
		{"no marker", "Normal output without the marker", ""},
		{"marker without reason", "NO_CHANGES_NEEDED:", ""},
		{"partial marker", "NO_CHANGES_NEEDED_MAYBE: not a real marker", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractNoChangesNeeded(tt.input)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// TestExtractRecheckPrevious verifies the RECHECK_PREVIOUS marker extraction logic.
func TestExtractRecheckPrevious(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"marker with rationale", "RECHECK_PREVIOUS: Stale core.worktree caused build failure; unset and verified", "Stale core.worktree caused build failure; unset and verified"},
		{"marker mid-output", "Investigated.\nRECHECK_PREVIOUS: Transient toolchain failure resolved on rerun.\nDone.", "Transient toolchain failure resolved on rerun."},
		{"marker with leading spaces", "  RECHECK_PREVIOUS: Indented rationale", "Indented rationale"},
		{"no marker", "Normal output", ""},
		{"marker without rationale", "RECHECK_PREVIOUS:", ""},
		{"partial marker", "RECHECK_PREVIOUSLY: not a real marker", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractRecheckPrevious(tt.input)
			assert.Equal(t, tt.expect, got)
		})
	}
}

// TestSmith_ErrorDuringExecution_TreatedAsFailure verifies that when the Claude
// CLI exits 0 but reports subtype="error_during_execution" (kill mid-tool), the
// pipeline marks the iteration as failed and does NOT proceed to Temper or
// Warden — preventing a misleading empty-diff hard-reject downstream.
func TestSmith_ErrorDuringExecution_TreatedAsFailure(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:      0,
		ResultSubtype: "error_during_execution",
		IsError:       false,
		FullOutput:    "Request interrupted by user",
	})
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		t.Fatal("Temper should not run when smith subtype indicates incomplete session")
		return nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		t.Fatal("Warden should not run when smith subtype indicates incomplete session")
		return nil, nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.Success)
	assert.False(t, outcome.NeedsHuman)
	require.NotNil(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "error_during_execution")

	// Verify a smith_failed event was logged with the actual subtype string.
	events, err := db.RecentEvents(20)
	require.NoError(t, err)
	var failedEvents []state.Event
	for _, e := range events {
		if e.Type == state.EventSmithFailed {
			failedEvents = append(failedEvents, e)
		}
	}
	require.NotEmpty(t, failedEvents, "expected at least one smith_failed event")
	var sawSubtype bool
	for _, e := range failedEvents {
		if e.BeadID == "test-bead" && strings.Contains(e.Message, "error_during_execution") {
			sawSubtype = true
			break
		}
	}
	assert.True(t, sawSubtype, "expected smith_failed event to mention subtype=error_during_execution")
}

// TestSmith_RecheckPrevious_HappyPath verifies the RECHECK_PREVIOUS escape
// hatch: on iteration 2, smith emits the marker; the empty-diff escalation is
// skipped, temper is re-run against the same SHA, and the pipeline proceeds to
// warden as normal. A smith_recheck event is logged.
func TestSmith_RecheckPrevious_HappyPath(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	smithCall := 0
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCall++
		switch smithCall {
		case 1:
			return smith.NewProcessForTest(&smith.Result{ExitCode: 0, FullOutput: "Implemented."}), nil
		case 2:
			return smith.NewProcessForTest(&smith.Result{
				ExitCode:   0,
				FullOutput: "Verified previous iteration is correct.\nRECHECK_PREVIOUS: Stale core.worktree config caused the build to fail; unset it and reverified `go build ./...` passes against the iter-1 commit.",
			}), nil
		default:
			t.Fatalf("unexpected smith call %d", smithCall)
			return nil, nil
		}
	}

	temperCall := 0
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperCall++
		if temperCall == 1 {
			return &temper.Result{Passed: false, FailedStep: "build", Summary: "go build failed: stale core.worktree"}
		}
		return &temper.Result{Passed: true}
	}

	wardenCall := 0
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCall++
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.False(t, outcome.NeedsHuman)
	assert.Equal(t, 2, smithCall, "Smith should be called twice")
	assert.Equal(t, 2, temperCall, "Temper should be called twice (initial fail + recheck pass)")
	assert.Equal(t, 1, wardenCall, "Warden should run once after the recheck pass")
	assert.Nil(t, outcome.Error)

	// Verify a smith_recheck event was recorded.
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	var recheckEvents []state.Event
	for _, e := range events {
		if e.Type == state.EventSmithRecheck {
			recheckEvents = append(recheckEvents, e)
		}
	}
	require.Len(t, recheckEvents, 1, "exactly one smith_recheck event expected")
	assert.Contains(t, recheckEvents[0].Message, "Stale core.worktree config")
}

// TestSmith_RecheckPrevious_TemperFailsAfterMarker_EscalatesToNeedsHuman
// verifies that when smith asserts RECHECK_PREVIOUS but temper fails again,
// the bead is escalated with both the rationale and the temper failure
// preserved on the failure event.
func TestSmith_RecheckPrevious_TemperFailsAfterMarker_EscalatesToNeedsHuman(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	smithCall := 0
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCall++
		switch smithCall {
		case 1:
			return smith.NewProcessForTest(&smith.Result{ExitCode: 0, FullOutput: "Implemented."}), nil
		case 2:
			return smith.NewProcessForTest(&smith.Result{
				ExitCode:   0,
				FullOutput: "RECHECK_PREVIOUS: I believe the failure was a flake.",
			}), nil
		default:
			t.Fatalf("unexpected smith call %d", smithCall)
			return nil, nil
		}
	}

	temperCall := 0
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperCall++
		// Temper keeps failing — flake hypothesis was wrong.
		return &temper.Result{Passed: false, FailedStep: "test", Summary: "TestFoo failed: real bug, not a flake"}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		t.Fatal("Warden should not run when temper fails after RECHECK_PREVIOUS")
		return nil, nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.Success)
	assert.True(t, outcome.NeedsHuman, "NeedsHuman must be set when RECHECK_PREVIOUS is followed by a temper fail")
	require.NotNil(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "RECHECK_PREVIOUS")
	assert.Equal(t, 2, smithCall, "Smith should be called twice")
	assert.Equal(t, 2, temperCall, "Temper should be called twice (iter1 + recheck rerun)")

	// Verify the failure event preserves both rationale and temper info.
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	var sawCombined bool
	for _, e := range events {
		if e.Type == state.EventSmithFailed &&
			strings.Contains(e.Message, "RECHECK_PREVIOUS") &&
			strings.Contains(e.Message, "flake") &&
			strings.Contains(e.Message, "real bug") {
			sawCombined = true
			break
		}
	}
	assert.True(t, sawCombined, "expected smith_failed event combining rationale and temper failure")

	// Also verify a smith_recheck event was recorded before the escalation.
	var recheckCount int
	for _, e := range events {
		if e.Type == state.EventSmithRecheck {
			recheckCount++
		}
	}
	assert.Equal(t, 1, recheckCount, "exactly one smith_recheck event expected")
}

// TestSmith_RecheckPrevious_RepeatedUse_StrictCap verifies the strict cap when
// the recheck path is chained: a second RECHECK_PREVIOUS use on the same bead
// is rejected as a pathological loop. We force this by having the first
// recheck pass temper (so the pipeline survives) but warden requesting changes,
// driving a second iteration where smith again emits RECHECK_PREVIOUS.
func TestSmith_RecheckPrevious_RepeatedUse_StrictCap(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	smithCall := 0
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCall++
		switch smithCall {
		case 1:
			return smith.NewProcessForTest(&smith.Result{ExitCode: 0, FullOutput: "Implemented."}), nil
		case 2:
			return smith.NewProcessForTest(&smith.Result{
				ExitCode:   0,
				FullOutput: "RECHECK_PREVIOUS: First env fix; verified.",
			}), nil
		case 3:
			return smith.NewProcessForTest(&smith.Result{
				ExitCode:   0,
				FullOutput: "RECHECK_PREVIOUS: Second env claim — should be rejected.",
			}), nil
		default:
			t.Fatalf("unexpected smith call %d", smithCall)
			return nil, nil
		}
	}
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		// Always pass temper so the pipeline reaches Warden.
		return &temper.Result{Passed: true}
	}
	wardenCall := 0
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCall++
		// Iteration 1: request changes to drive iteration 2.
		// Iteration 2: also request changes (after first recheck) to drive iteration 3.
		// On iteration 3 the second RECHECK should be rejected before warden runs.
		if wardenCall <= 2 {
			return &warden.ReviewResult{
				Verdict: warden.VerdictRequestChanges,
				Summary: "Please address X",
			}, nil
		}
		t.Fatalf("Warden should not be called more than twice; got call %d", wardenCall)
		return nil, nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.Success)
	assert.True(t, outcome.NeedsHuman)
	require.NotNil(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "RECHECK_PREVIOUS more than once")
	assert.Equal(t, 3, smithCall, "Smith should be called three times before the cap kicks in")

	// Exactly one smith_recheck event should have been recorded (the second
	// RECHECK is rejected as smith_failed, not smith_recheck).
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	var recheckEvents int
	for _, e := range events {
		if e.Type == state.EventSmithRecheck {
			recheckEvents++
		}
	}
	assert.Equal(t, 1, recheckEvents, "only the first RECHECK should produce a smith_recheck event")
}

// TestSmith_RecheckPrevious_DirtyWorktree_Escalates verifies that RECHECK_PREVIOUS
// is ignored and escalated when Smith actually left commits in the worktree while
// emitting the marker, contradicting its own claim that nothing changed.
func TestSmith_RecheckPrevious_DirtyWorktree_Escalates(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	// Use a real git repo so gitRevParseHEAD returns a non-empty SHA and
	// hasEmptyDiff can detect real commits (both use gitCmdCleanEnv internally).
	wtPath, _ := initGitRepo(t)
	params.WorktreeCreator = func(_ context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
		return &worktree.Worktree{BeadID: beadID, AnvilPath: anvilPath, Path: wtPath, Branch: "forge/" + beadID}, nil
	}

	env := cleanGitEnv()
	smithCall := 0
	params.SmithRunner = func(_ context.Context, worktreePath, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCall++
		switch smithCall {
		case 1:
			return smith.NewProcessForTest(&smith.Result{ExitCode: 0, FullOutput: "Implemented."}), nil
		case 2:
			// Smith makes a real commit while emitting RECHECK_PREVIOUS,
			// contradicting its claim that the worktree is unchanged.
			require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "sneaky.go"), []byte("package main"), 0o644))
			addCmd := exec.Command("git", "-C", worktreePath, "add", ".")
			addCmd.Env = env
			require.NoError(t, addCmd.Run())
			commitCmd := exec.Command("git", "-C", worktreePath, "commit", "-m", "sneaky change")
			commitCmd.Env = env
			require.NoError(t, commitCmd.Run())
			return smith.NewProcessForTest(&smith.Result{
				ExitCode:   0,
				FullOutput: "RECHECK_PREVIOUS: I believe the failure was environmental.",
			}), nil
		default:
			t.Fatalf("unexpected smith call %d", smithCall)
			return nil, nil
		}
	}

	temperCall := 0
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperCall++
		// Iter 1 fails to drive a second Smith iteration.
		if temperCall == 1 {
			return &temper.Result{Passed: false, FailedStep: "build", Summary: "build failed: stale config"}
		}
		t.Fatal("temper should not run after dirty-worktree escalation")
		return nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		t.Fatal("warden should not run when RECHECK_PREVIOUS has dirty worktree")
		return nil, nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, outcome.Success)
	assert.True(t, outcome.NeedsHuman, "NeedsHuman must be set when RECHECK_PREVIOUS contradicts dirty worktree")
	require.NotNil(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "RECHECK_PREVIOUS")
	assert.Equal(t, 2, smithCall, "Smith should be called twice before escalation")
	assert.Equal(t, 1, temperCall, "Temper should only be called once (iter 1)")

	// Verify a smith_failed event was logged for the dirty worktree, with no
	// smith_recheck event (marker was rejected, not honoured).
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	var sawSmithFailed bool
	var sawSmithRecheck bool
	for _, e := range events {
		switch e.Type {
		case state.EventSmithFailed:
			if strings.Contains(e.Message, "RECHECK_PREVIOUS") {
				sawSmithFailed = true
			}
		case state.EventSmithRecheck:
			sawSmithRecheck = true
		}
	}
	assert.True(t, sawSmithFailed, "expected smith_failed event for dirty-worktree RECHECK_PREVIOUS")
	assert.False(t, sawSmithRecheck, "expected no smith_recheck event when worktree is dirty")
}

// TestSmith_NoChangesNeeded_SkipsWardenAndTemper verifies that when Smith outputs
// the NO_CHANGES_NEEDED: marker, Warden and Temper are skipped and the outcome
// has NoChangesNeeded=true.
func TestSmith_NoChangesNeeded_SkipsWardenAndTemper(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "I investigated and found the bug was already fixed.\nNO_CHANGES_NEEDED: The fix was already applied in the previous release.\nDone.",
	})
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		t.Fatal("Temper should not be called when Smith signals no changes needed")
		return nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		t.Fatal("Warden should not be called when Smith signals no changes needed")
		return nil, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.NoChangesNeeded)
	assert.Equal(t, "The fix was already applied in the previous release.", outcome.NoChangesReason)
	assert.False(t, outcome.Success)
	assert.False(t, outcome.NeedsHuman)
	assert.Nil(t, outcome.Error)
}

// TestWardenFeedback_PassedToSmithOnRetry verifies that when Warden requests
// changes, the feedback is included in the Smith prompt for the next iteration.
func TestWardenFeedback_PassedToSmithOnRetry(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	var capturedPrompts []string
	iteration := 0
	params.SmithRunner = func(_ context.Context, _, promptText, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		capturedPrompts = append(capturedPrompts, promptText)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		iteration++
		if iteration == 1 {
			return &warden.ReviewResult{
				Verdict: warden.VerdictRequestChanges,
				Summary: "Missing error handling in foo.go",
				Issues: []warden.ReviewIssue{
					{Severity: "error", Message: "Unchecked error return from bar()", File: "foo.go", Line: 42},
					{Severity: "warning", Message: "Missing nil check", File: "baz.go"},
				},
			}, nil
		}
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	outcome := Run(context.Background(), params)

	require.True(t, outcome.Success)
	require.Len(t, capturedPrompts, 2, "Smith should be called twice (initial + retry)")

	// First prompt should NOT contain feedback
	assert.NotContains(t, capturedPrompts[0], "ITERATION 2 FIX")

	// Second prompt should contain warden feedback
	assert.Contains(t, capturedPrompts[1], "ITERATION 2 FIX")
	assert.Contains(t, capturedPrompts[1], "Warden code review")
	assert.Contains(t, capturedPrompts[1], "Missing error handling in foo.go")
	assert.Contains(t, capturedPrompts[1], "Unchecked error return from bar()")
	assert.Contains(t, capturedPrompts[1], "foo.go")
	assert.Contains(t, capturedPrompts[1], "line 42")
	assert.Contains(t, capturedPrompts[1], "Missing nil check")
	// Verify repo context is still included (AGENTS.md etc. come from the builder)
	assert.Contains(t, capturedPrompts[1], "autonomous AI developer")
	// Without real git changes, PriorDiff is empty so the skip-re-exploration
	// directive should NOT appear in the prompt.
	assert.NotContains(t, capturedPrompts[1], "Do NOT re-explore the codebase")
}

// TestTemperFeedback_PassedToSmithOnRetry verifies that when Temper fails,
// the failure details are included in the Smith prompt for the next iteration.
func TestTemperFeedback_PassedToSmithOnRetry(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	var capturedPrompts []string
	params.SmithRunner = func(_ context.Context, _, promptText, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		capturedPrompts = append(capturedPrompts, promptText)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	temperIteration := 0
	params.TemperRunner = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperIteration++
		if temperIteration == 1 {
			return &temper.Result{Passed: false, FailedStep: "test", Summary: "TestFoo failed: expected 42 got 0"}
		}
		return &temper.Result{Passed: true}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	outcome := Run(context.Background(), params)

	require.True(t, outcome.Success)
	require.Len(t, capturedPrompts, 2)

	assert.NotContains(t, capturedPrompts[0], "ITERATION 2 FIX")
	assert.Contains(t, capturedPrompts[1], "ITERATION 2 FIX")
	assert.Contains(t, capturedPrompts[1], "build/test verification")
	assert.Contains(t, capturedPrompts[1], "TestFoo failed")
}

// TestFormatWardenFeedback verifies the warden feedback formatting helper.
func TestFormatWardenFeedback(t *testing.T) {
	got := formatWardenFeedback("Two issues found.", []warden.ReviewIssue{
		{Severity: "error", Message: "Missing tests", File: "foo.go", Line: 42},
		{Severity: "warning", Message: "Unused import", File: "bar.go"},
	})

	assert.Contains(t, got, "Two issues found.")
	assert.Contains(t, got, "[error]")
	assert.Contains(t, got, "Missing tests")
	assert.Contains(t, got, "foo.go")
	assert.Contains(t, got, "line 42")
	assert.Contains(t, got, "[warning]")
	assert.Contains(t, got, "bar.go")

	// No issues case
	noIssues := formatWardenFeedback("Looks bad.", nil)
	assert.Equal(t, "Looks bad.", noIssues)
	assert.NotContains(t, noIssues, "Specific Issues")

	// Empty summary and no issues should still yield some non-empty feedback,
	// so that retry prompts are never completely blank.
	emptySummaryNoIssues := formatWardenFeedback("", nil)
	assert.NotEmpty(t, emptySummaryNoIssues)
}

// TestGoRaceDetection_AutoConfig verifies that Params.GoRaceDetection is
// plumbed into the auto-detected Temper config when TemperConfig is nil,
// resulting in a "race" step being included for Go projects.
func TestGoRaceDetection_AutoConfig(t *testing.T) {
	db := newTestDB(t)

	tests := []struct {
		name         string
		raceEnabled  bool
		wantRaceStep bool
	}{
		{"race enabled", true, true},
		{"race disabled", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, _, _ := baseParams(t, db)

			// Create a worktree with go.mod so Go auto-detection triggers the race step.
			goWorktreeDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(goWorktreeDir, "go.mod"), []byte("module test\n\ngo 1.21\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			params.WorktreeCreator = func(_ context.Context, _, beadID string) (*worktree.Worktree, error) {
				return &worktree.Worktree{
					BeadID:    beadID,
					AnvilPath: params.AnvilConfig.Path,
					Path:      goWorktreeDir,
					Branch:    "forge/" + beadID,
				}, nil
			}

			params.GoRaceDetection = tt.raceEnabled
			params.TemperConfig = nil // force auto-detection

			var capturedConfig temper.Config
			params.TemperRunner = func(_ context.Context, _ string, cfg temper.Config, _ *state.DB, _, _ string) *temper.Result {
				capturedConfig = cfg
				return &temper.Result{Passed: true}
			}
			params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
				return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
			}

			Run(context.Background(), params)

			hasRaceStep := false
			for _, step := range capturedConfig.Steps {
				if step.Name == "race" {
					hasRaceStep = true
					break
				}
			}
			assert.Equal(t, tt.wantRaceStep, hasRaceStep, "race step presence should match GoRaceDetection flag")
			assert.Equal(t, tt.raceEnabled, capturedConfig.GoRaceDetection, "GoRaceDetection should be plumbed to temper config")
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
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
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

// TestSchematic_Quota_PersistedToStateDB verifies that when Schematic returns a
// non-nil Quota, the pipeline persists it to provider_quotas in state.db.
func TestSchematic_Quota_PersistedToStateDB(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "Implement the feature with enough words to trigger schematic"
	params.SchematicRunner = func(_ context.Context, _ schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		return &schematic.Result{
			Action: schematic.ActionPlan,
			Plan:   "1. Write code",
			Reason: "Simple plan",
			Quota: &provider.Quota{
				RequestsLimit:     100,
				RequestsRemaining: 42,
			},
		}
	}

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Success)

	// The schematic quota must have been written to provider_quotas.
	got, err := db.GetProviderQuota(string(params.Providers[0].Kind))
	require.NoError(t, err)
	require.NotNil(t, got, "expected a quota row for provider %s", params.Providers[0].Kind)
	assert.Equal(t, 100, got.RequestsLimit)
	assert.Equal(t, 42, got.RequestsRemaining)
}

// TestPreserveWorktreeLogs_NoLogDir returns empty string and no error when
// the .forge-logs directory does not exist in the worktree.
func TestPreserveWorktreeLogs_NoLogDir(t *testing.T) {
	wtDir := t.TempDir()
	dst, err := PreserveWorktreeLogs(wtDir, "bead-xyz")
	require.NoError(t, err)
	assert.Empty(t, dst, "no destination dir expected when source does not exist")
}

// TestPreserveWorktreeLogs_EmptyLogDir returns empty string and no error when
// the .forge-logs directory exists but contains no files.
func TestPreserveWorktreeLogs_EmptyLogDir(t *testing.T) {
	wtDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wtDir, ".forge-logs"), 0o755))

	dst, err := PreserveWorktreeLogs(wtDir, "bead-empty")
	require.NoError(t, err)
	assert.Empty(t, dst, "no destination dir expected when log dir is empty")
}

// TestPreserveWorktreeLogs_CopiesFiles verifies that log files are copied to
// ~/.forge/logs/<beadID>/ and the returned path points to that directory.
func TestPreserveWorktreeLogs_CopiesFiles(t *testing.T) {
	// Redirect the home directory so we don't pollute the real ~/.forge/logs.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome) // Windows

	wtDir := t.TempDir()
	logSrc := filepath.Join(wtDir, ".forge-logs")
	require.NoError(t, os.MkdirAll(logSrc, 0o755))

	// Create two log files.
	require.NoError(t, os.WriteFile(filepath.Join(logSrc, "smith.log"), []byte("smith output"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(logSrc, "session.log"), []byte("session data"), 0o644))

	dst, err := PreserveWorktreeLogs(wtDir, "bead-abc")
	require.NoError(t, err)
	require.NotEmpty(t, dst)

	// The destination directory should be exactly fakeHome/.forge/logs/bead-abc.
	expectedDir := filepath.Join(fakeHome, ".forge", "logs", "bead-abc")
	assert.Equal(t, expectedDir, dst)

	// Both files should exist at the destination.
	smithContent, readErr := os.ReadFile(filepath.Join(dst, "smith.log"))
	require.NoError(t, readErr)
	assert.Equal(t, "smith output", string(smithContent))

	sessionContent, readErr := os.ReadFile(filepath.Join(dst, "session.log"))
	require.NoError(t, readErr)
	assert.Equal(t, "session data", string(sessionContent))
}

// TestPreserveWorktreeLogs_SkipsSubdirs verifies that subdirectories inside
// .forge-logs are not copied (only plain files are preserved).
func TestPreserveWorktreeLogs_SkipsSubdirs(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	wtDir := t.TempDir()
	logSrc := filepath.Join(wtDir, ".forge-logs")
	require.NoError(t, os.MkdirAll(logSrc, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logSrc, "output.log"), []byte("data"), 0o644))
	// A nested sub-directory that should be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(logSrc, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logSrc, "subdir", "nested.log"), []byte("nested"), 0o644))

	dst, err := PreserveWorktreeLogs(wtDir, "bead-sub")
	require.NoError(t, err)
	require.NotEmpty(t, dst)

	// The plain log should be copied.
	_, statErr := os.Stat(filepath.Join(dst, "output.log"))
	require.NoError(t, statErr)

	// The subdir itself should NOT be copied (PreserveWorktreeLogs skips dirs).
	_, statErr = os.Stat(filepath.Join(dst, "subdir"))
	require.True(t, os.IsNotExist(statErr), "subdirectory should not be copied to persistent log dir")
}

// TestPreserveWorktreeLogs_SkipsSymlinks verifies that symlinks inside
// .forge-logs are not copied, preventing symlink-following into arbitrary locations.
func TestPreserveWorktreeLogs_SkipsSymlinks(t *testing.T) {
	if err := os.Symlink(".", filepath.Join(t.TempDir(), "probe")); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	wtDir := t.TempDir()
	logSrc := filepath.Join(wtDir, ".forge-logs")
	require.NoError(t, os.MkdirAll(logSrc, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(logSrc, "real.log"), []byte("data"), 0o644))

	// Create a symlink pointing to an external file.
	externalFile := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(externalFile, []byte("sensitive"), 0o644))
	require.NoError(t, os.Symlink(externalFile, filepath.Join(logSrc, "link.log")))

	dst, err := PreserveWorktreeLogs(wtDir, "bead-sym")
	require.NoError(t, err)
	require.NotEmpty(t, dst)

	// The regular file should be copied.
	_, statErr := os.Stat(filepath.Join(dst, "real.log"))
	require.NoError(t, statErr)

	// The symlink should NOT be copied.
	_, statErr = os.Lstat(filepath.Join(dst, "link.log"))
	require.True(t, os.IsNotExist(statErr), "symlink should not be copied to persistent log dir")
}

// TestMaxIterations_StopsAfterConfiguredCap verifies that when Params.MaxIterations
// is set to a small value, the pipeline stops after that many Smith-Warden cycles
// even if Warden keeps requesting changes.
func TestMaxIterations_StopsAfterConfiguredCap(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	smithCallCount := 0
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCallCount++
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{
			Verdict: warden.VerdictRequestChanges,
			Summary: "Still has issues",
		}, nil
	}

	params.MaxIterations = 1

	outcome := Run(context.Background(), params)

	assert.Equal(t, 1, smithCallCount, "Smith should only run once when MaxIterations=1")
	assert.False(t, outcome.Success)
	assert.Equal(t, warden.VerdictRequestChanges, outcome.Verdict)
	assert.NotNil(t, outcome.Error)
}

// TestSkipSmith_SkipsSmithOnFirstIteration verifies the SkipSmith=true control-
// flow path introduced for force-smith resumption:
//   - SmithRunner is NOT invoked on iteration 1 (smith already ran externally)
//   - Phases and statuses are updated as expected (initial phase = "temper")
//   - A request_changes verdict from Warden on iteration 1 triggers a Smith
//     rerun on iteration 2 (subsequent iterations are NOT skipped)
func TestSkipSmith_SkipsSmithOnFirstIteration(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	var smithCallCount int
	params.SmithRunner = func(_ context.Context, _, _ string, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		smithCallCount++
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0}), nil
	}

	wardenCallCount := 0
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCallCount++
		if wardenCallCount == 1 {
			// First Warden call: request changes to trigger Smith on iteration 2.
			return &warden.ReviewResult{
				Verdict: warden.VerdictRequestChanges,
				Summary: "Add missing error handling",
				Issues: []warden.ReviewIssue{
					{Severity: "error", Message: "unchecked error", File: "main.go", Line: 10},
				},
			}, nil
		}
		// Second Warden call: approve.
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	params.SkipSmith = true
	params.MaxIterations = 3

	outcome := Run(context.Background(), params)

	// Pipeline should succeed after iteration 2 (Warden approves on second call).
	require.True(t, outcome.Success, "pipeline should succeed")
	assert.Equal(t, warden.VerdictApprove, outcome.Verdict)

	// Smith must NOT have been invoked on iteration 1 (SkipSmith=true),
	// but MUST have been invoked exactly once on iteration 2.
	assert.Equal(t, 1, smithCallCount, "Smith should be called exactly once (on iteration 2, not iteration 1)")

	// Warden should have been called twice: once returning request_changes,
	// once returning approve.
	assert.Equal(t, 2, wardenCallCount, "Warden should be called twice")

	// Verify a worker row exists in state DB for the bead. The pipeline sets
	// the initial phase to "temper" (not "smith") when SkipSmith=true. The
	// final status is either WorkerDone or WorkerMonitoring depending on
	// whether a VCS provider created a PR.
	workers, err := db.AllWorkers(0)
	require.NoError(t, err)
	require.NotEmpty(t, workers)
	var found bool
	for _, w := range workers {
		if w.BeadID == "test-bead" {
			found = true
			assert.True(t,
				w.Status == state.WorkerDone || w.Status == state.WorkerMonitoring,
				"worker status should be done or monitoring, got: %s", w.Status)
			break
		}
	}
	assert.True(t, found, "worker row should exist in state DB")
}

// TestSchematic_OnSpawn_UpdatesWorkerPIDAndLogPath verifies that when the
// schematic runner invokes cfg.OnSpawn, the pipeline persists the PID and
// log_path into the worker row in state.db. This is the key contract that
// allows Hearth to tail logs during the schematic phase.
//
// Note: the Smith runner (which runs after schematic) also calls
// UpdateWorkerPID/LogPath with its own values. To isolate the schematic
// OnSpawn behaviour, we snapshot the DB immediately after the callback fires
// — before the Smith phase overwrites them.
func TestSchematic_OnSpawn_UpdatesWorkerPIDAndLogPath(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	const workerID = "spawn-test-worker"
	params.WorkerID = workerID

	var snapshotPID int
	var snapshotLogPath string

	schemCfg := schematic.Config{Enabled: true, WordThreshold: 1}
	params.SchematicConfig = &schemCfg
	params.Bead.Description = "A task with enough words to trigger schematic analysis"
	params.SchematicRunner = func(_ context.Context, cfg schematic.Config, _ poller.Bead, _ string, _ provider.Provider) *schematic.Result {
		require.NotNil(t, cfg.OnSpawn, "pipeline must wire OnSpawn before calling SchematicRunner")
		cfg.OnSpawn(12345, "/fake/smith.log")
		// Snapshot immediately — Smith will overwrite these with its own PID/path.
		workers, err := db.AllWorkers(0)
		require.NoError(t, err)
		for _, w := range workers {
			if w.ID == workerID {
				snapshotPID = w.PID
				snapshotLogPath = w.LogPath
				break
			}
		}
		return &schematic.Result{Action: schematic.ActionSkip, Reason: "simple enough"}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	Run(context.Background(), params)

	assert.Equal(t, 12345, snapshotPID, "worker PID should be updated via OnSpawn callback")
	assert.Equal(t, "/fake/smith.log", snapshotLogPath, "worker log_path should be updated via OnSpawn callback")
}

// cleanGitEnv returns os.Environ() with GIT_DIR and GIT_WORK_TREE removed so
// that git commands in tests are not redirected to the worktree that hosts the
// test process.
func cleanGitEnv() []string {
	return executil.CleanGitEnv()
}

// initGitRepo creates a temporary git repo with an initial commit and returns
// the repo path and the HEAD SHA after that commit.
func initGitRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	env := cleanGitEnv()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	// Create an initial commit so HEAD exists.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644))
	addCmd := exec.Command("git", "-C", dir, "add", ".")
	addCmd.Env = env
	require.NoError(t, addCmd.Run())
	commitCmd := exec.Command("git", "-C", dir, "commit", "-m", "initial")
	commitCmd.Env = env
	require.NoError(t, commitCmd.Run())

	// Use a clean-env rev-parse since gitRevParseHEAD inherits GIT_DIR.
	shaCmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	shaCmd.Env = env
	shaOut, err := shaCmd.Output()
	require.NoError(t, err)
	sha := strings.TrimSpace(string(shaOut))
	require.NotEmpty(t, sha)
	return dir, sha
}

// TestHasEmptyDiff_NewCommits verifies that hasEmptyDiff returns false when
// smith adds commits after the saved pre-smith SHA (the core fix for Forge-z9h6).
func TestHasEmptyDiff_NewCommits(t *testing.T) {
	dir, preSHA := initGitRepo(t)
	env := cleanGitEnv()

	// Simulate smith adding a commit.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package main"), 0o644))
	addCmd := exec.Command("git", "-C", dir, "add", ".")
	addCmd.Env = env
	require.NoError(t, addCmd.Run())
	commitCmd := exec.Command("git", "-C", dir, "commit", "-m", "fix: smith did work")
	commitCmd.Env = env
	require.NoError(t, commitCmd.Run())

	assert.False(t, hasEmptyDiff(dir, preSHA), "hasEmptyDiff should be false when smith added commits")
}

// TestHasEmptyDiff_NoCommits verifies that hasEmptyDiff returns true when
// HEAD hasn't moved from the pre-smith SHA (smith made no changes).
func TestHasEmptyDiff_NoCommits(t *testing.T) {
	dir, preSHA := initGitRepo(t)

	// Smith ran but made no commits — HEAD is still at preSHA.
	assert.True(t, hasEmptyDiff(dir, preSHA), "hasEmptyDiff should be true when no commits were added")
}

// TestHasEmptyDiff_UncommittedChanges verifies that hasEmptyDiff returns false
// when there are uncommitted changes in the worktree, even with the same HEAD SHA.
func TestHasEmptyDiff_UncommittedChanges(t *testing.T) {
	dir, preSHA := initGitRepo(t)

	// Smith left unstaged changes without committing.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wip.go"), []byte("package main"), 0o644))

	assert.False(t, hasEmptyDiff(dir, preSHA), "hasEmptyDiff should be false when uncommitted changes exist")
}

// TestHasEmptyDiff_EmptyPreSmithSHA verifies the fallback path when no
// pre-smith SHA was captured (e.g. gitRevParseHEAD failed).
func TestHasEmptyDiff_EmptyPreSmithSHA(t *testing.T) {
	dir, _ := initGitRepo(t)

	// With empty preSHA, falls back to diffing HEAD~1. Since the repo has
	// only one commit, HEAD~1 will fail and hasEmptyDiff returns false.
	assert.False(t, hasEmptyDiff(dir, ""), "hasEmptyDiff with empty preSHA and single commit should return false")
}

func TestTruncateDiff(t *testing.T) {
	tests := []struct {
		name   string
		diff   string
		maxLen int
		want   string
	}{
		{
			name:   "short diff unchanged",
			diff:   "line1\nline2\n",
			maxLen: 100,
			want:   "line1\nline2\n",
		},
		{
			name:   "exact length unchanged",
			diff:   "abc\ndef\n",
			maxLen: 8,
			want:   "abc\ndef\n",
		},
		{
			name:   "truncates at last newline",
			diff:   "line1\nline2\nline3\n",
			maxLen: 12,
			want:   "line1\nline2\n... (diff truncated)",
		},
		{
			name:   "no newline in range",
			diff:   "abcdefghijklmnop",
			maxLen: 10,
			want:   "abcdefghij\n... (diff truncated)",
		},
		{
			name:   "zero maxLen returns empty",
			diff:   "some diff",
			maxLen: 0,
			want:   "",
		},
		{
			name:   "negative maxLen returns empty",
			diff:   "some diff",
			maxLen: -1,
			want:   "",
		},
		{
			name:   "empty diff unchanged",
			diff:   "",
			maxLen: 100,
			want:   "",
		},
		{
			// "é" is 2 bytes (0xC3 0xA9); maxLen=4 cuts into its second byte.
			// The fix should back up to a valid UTF-8 boundary ("abc").
			name:   "non-ASCII no newline returns valid UTF-8",
			diff:   "abc\xc3\xa9xyz", // "abcéxyz"
			maxLen: 4,
			want:   "abc\n... (diff truncated)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateDiff(tt.diff, tt.maxLen)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShouldRunSchematic(t *testing.T) {
	enabled := schematic.Config{Enabled: true, WordThreshold: 1}
	disabled := schematic.Config{Enabled: false, WordThreshold: 1}

	beadSimple := poller.Bead{Description: "do something"}
	beadDecompose := poller.Bead{Description: "do something", Labels: []string{"decompose"}}

	tests := []struct {
		name      string
		cfg       schematic.Config
		bead      poller.Bead
		providers []provider.Provider
		want      bool
	}{
		{
			name:      "Copilot provider without decompose tag skips schematic",
			cfg:       enabled,
			bead:      beadSimple,
			providers: []provider.Provider{{Kind: provider.Copilot}},
			want:      false,
		},
		{
			name:      "Copilot provider with decompose tag runs schematic",
			cfg:       enabled,
			bead:      beadDecompose,
			providers: []provider.Provider{{Kind: provider.Copilot}},
			want:      true,
		},
		{
			name:      "non-Copilot provider runs schematic when enabled",
			cfg:       enabled,
			bead:      beadSimple,
			providers: []provider.Provider{{Kind: provider.Claude}},
			want:      true,
		},
		{
			name:      "schematic disabled skips regardless of provider",
			cfg:       disabled,
			bead:      beadDecompose,
			providers: []provider.Provider{{Kind: provider.Copilot}},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := shouldRunSchematic(tt.cfg, tt.bead, tt.providers)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestWardenProviders_OverrideCopilotModel verifies that wardenProviders
// replaces the model on Copilot entries when WardenModelOverride is set.
func TestWardenProviders_OverrideCopilotModel(t *testing.T) {
	p := &Params{WardenModelOverride: "claude-haiku-4-5"}
	providers := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-sonnet-4-6"},
		{Kind: provider.Claude, Model: "claude-opus-4-6"},
	}
	got := p.wardenProviders(providers)
	assert.Equal(t, "claude-haiku-4-5", got[0].Model, "Copilot model should be overridden")
	assert.Equal(t, "claude-opus-4-6", got[1].Model, "non-Copilot model should be unchanged")
}

// TestWardenProviders_EmptyOverride verifies that wardenProviders returns the
// original slice (no clone) when WardenModelOverride is empty.
func TestWardenProviders_EmptyOverride(t *testing.T) {
	p := &Params{WardenModelOverride: ""}
	providers := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-sonnet-4-6"},
	}
	got := p.wardenProviders(providers)
	// Assert aliasing: mutating got must be visible through providers.
	got[0].Model = "mutated"
	assert.Equal(t, "mutated", providers[0].Model, "wardenProviders must return the original slice when override is empty")
}

// TestWardenProviders_NonCopilotUnmodified verifies that non-Copilot providers
// are not modified even when WardenModelOverride is set.
func TestWardenProviders_NonCopilotUnmodified(t *testing.T) {
	p := &Params{WardenModelOverride: "claude-haiku-4-5"}
	providers := []provider.Provider{
		{Kind: provider.Claude, Model: "claude-opus-4-6"},
		{Kind: provider.Gemini, Model: "gemini-2.5-pro"},
	}
	got := p.wardenProviders(providers)
	assert.Equal(t, "claude-opus-4-6", got[0].Model)
	assert.Equal(t, "gemini-2.5-pro", got[1].Model)
}

// TestSchematicProviders_OverrideCopilotModel verifies that schematicProviders
// replaces the model on Copilot entries when SchematicModelOverride is set.
func TestSchematicProviders_OverrideCopilotModel(t *testing.T) {
	p := &Params{SchematicModelOverride: "claude-haiku-4-5"}
	providers := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-sonnet-4-6"},
		{Kind: provider.Claude, Model: "claude-opus-4-6"},
	}
	got := p.schematicProviders(providers)
	assert.Equal(t, "claude-haiku-4-5", got[0].Model, "Copilot model should be overridden")
	assert.Equal(t, "claude-opus-4-6", got[1].Model, "non-Copilot model should be unchanged")
}

// TestSchematicProviders_EmptyOverride verifies that schematicProviders returns
// the original slice (no clone) when SchematicModelOverride is empty.
func TestSchematicProviders_EmptyOverride(t *testing.T) {
	p := &Params{SchematicModelOverride: ""}
	providers := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-sonnet-4-6"},
	}
	got := p.schematicProviders(providers)
	// Assert aliasing: mutating got must be visible through providers.
	got[0].Model = "mutated"
	assert.Equal(t, "mutated", providers[0].Model, "schematicProviders must return the original slice when override is empty")
}

// TestSchematicProviders_NonCopilotUnmodified verifies that non-Copilot
// providers are not modified even when SchematicModelOverride is set.
func TestSchematicProviders_NonCopilotUnmodified(t *testing.T) {
	p := &Params{SchematicModelOverride: "claude-haiku-4-5"}
	providers := []provider.Provider{
		{Kind: provider.Claude, Model: "claude-opus-4-6"},
		{Kind: provider.Gemini, Model: "gemini-2.5-pro"},
	}
	got := p.schematicProviders(providers)
	assert.Equal(t, "claude-opus-4-6", got[0].Model)
	assert.Equal(t, "gemini-2.5-pro", got[1].Model)
}

// TestWardenProviders_DoesNotMutateOriginal verifies that wardenProviders
// clones the slice and does not modify the original providers.
func TestWardenProviders_DoesNotMutateOriginal(t *testing.T) {
	p := &Params{WardenModelOverride: "claude-haiku-4-5"}
	original := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-sonnet-4-6"},
	}
	_ = p.wardenProviders(original)
	assert.Equal(t, "claude-sonnet-4-6", original[0].Model, "original slice must not be mutated")
}

// TestWardenProviders_ExplicitWardenProviders verifies that when WardenProviders
// is set (via stage_providers), it is returned directly regardless of base providers
// or WardenModelOverride.
func TestWardenProviders_ExplicitWardenProviders(t *testing.T) {
	explicit := []provider.Provider{
		{Kind: provider.Claude, Model: "claude-sonnet-4-6"},
	}
	p := &Params{
		WardenProviders:     explicit,
		WardenModelOverride: "claude-haiku-4-5", // should be ignored
	}
	base := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-opus-4-6"},
	}
	got := p.wardenProviders(base)
	assert.Equal(t, explicit, got, "explicit WardenProviders must take precedence")
}

// TestSchematicProviders_ExplicitSchematicProviders verifies that when
// SchematicProviders is set (via stage_providers), it is returned directly.
func TestSchematicProviders_ExplicitSchematicProviders(t *testing.T) {
	explicit := []provider.Provider{
		{Kind: provider.Gemini, Model: "gemini-2.5-flash"},
	}
	p := &Params{
		SchematicProviders:     explicit,
		SchematicModelOverride: "claude-haiku-4-5", // should be ignored
	}
	base := []provider.Provider{
		{Kind: provider.Copilot, Model: "claude-opus-4-6"},
	}
	got := p.schematicProviders(base)
	assert.Equal(t, explicit, got, "explicit SchematicProviders must take precedence")
}

// --- shouldSkipWarden tests ---

func TestShouldSkipWarden_AllCriteriaMet(t *testing.T) {
	ds := DiffStat{LinesChanged: 50, FilesChanged: 1, IsDocsOnly: true, Valid: true}
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.True(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_DisabledInConfig(t *testing.T) {
	ds := DiffStat{LinesChanged: 50, FilesChanged: 1, IsDocsOnly: true, Valid: true}
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.False(t, shouldSkipWarden(ds, bead, providers, false))
}

func TestShouldSkipWarden_NotCopilotProvider(t *testing.T) {
	ds := DiffStat{LinesChanged: 50, FilesChanged: 1, IsDocsOnly: true, Valid: true}
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Claude}}
	assert.False(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_HighPriority(t *testing.T) {
	for _, p := range []int{0, 1, 2} {
		ds := DiffStat{LinesChanged: 50, FilesChanged: 1, IsDocsOnly: true, Valid: true}
		bead := poller.Bead{Priority: p}
		providers := []provider.Provider{{Kind: provider.Copilot}}
		assert.False(t, shouldSkipWarden(ds, bead, providers, true),
			"should not skip for priority %d", p)
	}
}

func TestShouldSkipWarden_InvalidDiffStat(t *testing.T) {
	// A zero-value / invalid DiffStat must never satisfy skip criteria,
	// even if all other conditions are met (guards against git failures).
	ds := DiffStat{} // Valid==false, FilesChanged==0
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.False(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_TooManyLines(t *testing.T) {
	ds := DiffStat{LinesChanged: 101, FilesChanged: 1, IsDocsOnly: true, Valid: true}
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.False(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_SecurityFiles(t *testing.T) {
	ds := DiffStat{LinesChanged: 20, FilesChanged: 1, TouchesSecurityFiles: true, Valid: true}
	bead := poller.Bead{Priority: 4}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.False(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_TooManyFilesNotDocsOrTests(t *testing.T) {
	ds := DiffStat{LinesChanged: 30, FilesChanged: 3, Valid: true}
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.False(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_DocsOnlyManyFiles(t *testing.T) {
	ds := DiffStat{LinesChanged: 80, FilesChanged: 5, IsDocsOnly: true, Valid: true}
	bead := poller.Bead{Priority: 3}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.True(t, shouldSkipWarden(ds, bead, providers, true))
}

func TestShouldSkipWarden_TestsOnlyManyFiles(t *testing.T) {
	ds := DiffStat{LinesChanged: 90, FilesChanged: 4, IsTestsOnly: true, Valid: true}
	bead := poller.Bead{Priority: 4}
	providers := []provider.Provider{{Kind: provider.Copilot}}
	assert.True(t, shouldSkipWarden(ds, bead, providers, true))
}

// --- Combined Smith+Warden mode pipeline tests ---

// TestCombinedMode_SelfReviewApprove_SkipsRealWarden verifies that when
// combined mode is active (Copilot provider + config enabled) and Smith's
// self-review approves with no concerns, the real Warden is NOT invoked.
func TestCombinedMode_SelfReviewApprove_SkipsRealWarden(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	// Enable combined mode with Copilot provider.
	params.Providers = []provider.Provider{{Kind: provider.Copilot}}
	params.CopilotCombinedSmithWarden = true
	params.CopilotWardenSampleRate = 0.0 // disable sampling so we deterministically skip
	params.Bead.Priority = 3             // not high-priority

	// Smith outputs a passing self-review.
	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:     0,
		ProviderUsed: provider.Copilot,
		FullOutput: "Implemented the change.\n\n```json\n{\"self_review\": {\"verdict\": \"approve\", \"concerns\": []}}\n```\n",
	})

	wardenCalled := false
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCalled = true
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.False(t, wardenCalled, "real Warden should NOT be called when self-review approves in combined mode")
}

// TestCombinedMode_SelfReviewRequestChanges_RunsRealWarden verifies that
// when Smith's self-review has verdict "request_changes", the real Warden
// is invoked as a fallback.
func TestCombinedMode_SelfReviewRequestChanges_RunsRealWarden(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.Providers = []provider.Provider{{Kind: provider.Copilot}}
	params.CopilotCombinedSmithWarden = true
	params.CopilotWardenSampleRate = 0.0
	params.Bead.Priority = 3

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:     0,
		ProviderUsed: provider.Copilot,
		FullOutput: "Implemented but found issues.\n\n```json\n{\"self_review\": {\"verdict\": \"request_changes\", \"concerns\": [\"missing error handling\"]}}\n```\n",
	})

	wardenCalled := false
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCalled = true
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM after review"}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.True(t, wardenCalled, "real Warden should run when self-review flags concerns")
}

// TestCombinedMode_HighPriority_AlwaysRunsRealWarden verifies that P0 and
// P1 beads always get a real Warden review, even when the self-review
// approves with no concerns.
func TestCombinedMode_HighPriority_AlwaysRunsRealWarden(t *testing.T) {
	for _, prio := range []int{0, 1} {
		t.Run(fmt.Sprintf("P%d", prio), func(t *testing.T) {
			db := newTestDB(t)
			params, _, _ := baseParams(t, db)

			params.Providers = []provider.Provider{{Kind: provider.Copilot}}
			params.CopilotCombinedSmithWarden = true
			params.CopilotWardenSampleRate = 0.0 // sampling off
			params.Bead.Priority = prio

			params.SmithRunner = immediateSmith(&smith.Result{
				ExitCode:     0,
				ProviderUsed: provider.Copilot,
				FullOutput: "Done.\n\n```json\n{\"self_review\": {\"verdict\": \"approve\", \"concerns\": []}}\n```\n",
			})

			wardenCalled := false
			params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
				wardenCalled = true
				return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
			}

			outcome := Run(context.Background(), params)

			assert.True(t, outcome.Success)
			assert.True(t, wardenCalled, "real Warden must always run for P%d beads", prio)
		})
	}
}

// TestCombinedMode_ParseFailure_FallsBackToRealWarden verifies that when
// Smith's output lacks a valid self-review JSON block, the pipeline falls
// back to running a real Warden review.
func TestCombinedMode_ParseFailure_FallsBackToRealWarden(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.Providers = []provider.Provider{{Kind: provider.Copilot}}
	params.CopilotCombinedSmithWarden = true
	params.CopilotWardenSampleRate = 0.0
	params.Bead.Priority = 3

	// Smith output with no self-review JSON.
	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "Implemented the feature. All done!",
	})

	wardenCalled := false
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCalled = true
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.True(t, wardenCalled, "real Warden must run when self-review JSON is missing")
}

// TestCombinedMode_NonCopilot_RunsNormalWarden verifies that combined mode
// has no effect when the primary provider is not Copilot.
func TestCombinedMode_NonCopilot_RunsNormalWarden(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.Providers = []provider.Provider{{Kind: provider.Claude}}
	params.CopilotCombinedSmithWarden = true // enabled, but provider is Claude
	params.CopilotWardenSampleRate = 0.0
	params.Bead.Priority = 3

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "Done.\n\n```json\n{\"self_review\": {\"verdict\": \"approve\", \"concerns\": []}}\n```\n",
	})

	wardenCalled := false
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCalled = true
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.True(t, wardenCalled, "real Warden must run when provider is not Copilot, even with combined mode enabled")
}

// TestCombinedMode_Disabled_RunsNormalWarden verifies that when combined
// mode is disabled (default), the normal Warden path is always used.
func TestCombinedMode_Disabled_RunsNormalWarden(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.Providers = []provider.Provider{{Kind: provider.Copilot}}
	params.CopilotCombinedSmithWarden = false // disabled
	params.Bead.Priority = 3

	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:   0,
		FullOutput: "Done.\n\n```json\n{\"self_review\": {\"verdict\": \"approve\", \"concerns\": []}}\n```\n",
	})

	wardenCalled := false
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCalled = true
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.True(t, wardenCalled, "real Warden must run when combined mode is disabled")
}



// TestCombinedMode_FallbackProvider_RunsNormalWarden verifies that when Smith
// starts with Copilot (combinedMode=true) but actually ran under a different
// provider due to rate-limiting fallback, a real Warden is forced.
func TestCombinedMode_FallbackProvider_RunsNormalWarden(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.Providers = []provider.Provider{{Kind: provider.Copilot}}
	params.CopilotCombinedSmithWarden = true
	params.CopilotWardenSampleRate = 0.0 // would skip Warden if Copilot ran
	params.Bead.Priority = 3

	// Smith fell back to Claude (e.g. due to Copilot rate limit).
	params.SmithRunner = immediateSmith(&smith.Result{
		ExitCode:     0,
			FullOutput:   "Done.\n\n```json\n{\"self_review\": {\"verdict\": \"approve\", \"concerns\": []}}\n```\n",
		ProviderUsed: provider.Claude,
	})

	wardenCalled := false
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		wardenCalled = true
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success)
	assert.True(t, wardenCalled, "real Warden must run when Smith fell back to a non-Copilot provider")
}

// writeFragment writes a changelog fragment with the given filename in the
// worktree's changelog.d directory.
func writeFragment(t *testing.T, wtPath, filename, content string) {
	t.Helper()
	dir := filepath.Join(wtPath, "changelog.d")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644))
}

// TestExtractChangelogSummary covers the full matrix of fragment filename
// variants that real anvils produce, including the Munin-style suffixed names
// that used to fall through to the warden verdict fallback.
func TestExtractChangelogSummary(t *testing.T) {
	const bead = "Forge-abcd"
	const enBullet = "- **Plain EN bullet** - detail. (Forge-abcd)"
	const plainBullet = "- **Plain bullet** - detail. (Forge-abcd)"
	const techEnBullet = "- **Technical EN bullet** - detail. (Forge-abcd)"
	const techNbBullet = "- **Technical NB bullet** - detalj. (Forge-abcd)"
	const nbBullet = "- **Plain NB bullet** - detalj. (Forge-abcd)"
	const header = "category: Added\n"

	t.Run("plain .md fragment", func(t *testing.T) {
		dir := t.TempDir()
		writeFragment(t, dir, bead+".md", header+plainBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Plain bullet")
	})

	t.Run("plain .en.md fragment", func(t *testing.T) {
		dir := t.TempDir()
		writeFragment(t, dir, bead+".en.md", header+enBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Plain EN bullet")
	})

	t.Run("suffixed -technical.en.md fragment (Munin convention)", func(t *testing.T) {
		dir := t.TempDir()
		writeFragment(t, dir, bead+"-technical.en.md", header+techEnBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Technical EN bullet",
			"Munin-style -technical.en.md fragment must be picked up (was previously leaking warden verdict)")
	})

	t.Run("English preferred over Norwegian for plain fragments", func(t *testing.T) {
		dir := t.TempDir()
		writeFragment(t, dir, bead+".en.md", header+enBullet+"\n")
		writeFragment(t, dir, bead+".nb.md", header+nbBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Plain EN bullet")
		assert.NotContains(t, got, "Plain NB bullet")
	})

	t.Run("English preferred over Norwegian for suffixed fragments", func(t *testing.T) {
		dir := t.TempDir()
		writeFragment(t, dir, bead+"-technical.en.md", header+techEnBullet+"\n")
		writeFragment(t, dir, bead+"-technical.nb.md", header+techNbBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Technical EN bullet")
		assert.NotContains(t, got, "Technical NB bullet")
	})

	t.Run("no fragment returns empty", func(t *testing.T) {
		dir := t.TempDir()
		got := ExtractChangelogSummary(dir, bead)
		assert.Equal(t, "", got)
	})

	t.Run("fragment with zero bullets falls through", func(t *testing.T) {
		dir := t.TempDir()
		// A category-only fragment with no bullets must not masquerade as a
		// successful match; the next pattern should win instead.
		writeFragment(t, dir, bead+".en.md", "category: Added\n")
		writeFragment(t, dir, bead+"-technical.en.md", header+techEnBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Technical EN bullet",
			"zero-bullet fragment must not short-circuit the pattern search")
	})

	t.Run("plain .md preferred over suffixed variant", func(t *testing.T) {
		dir := t.TempDir()
		writeFragment(t, dir, bead+".md", header+plainBullet+"\n")
		writeFragment(t, dir, bead+"-technical.en.md", header+techEnBullet+"\n")
		got := ExtractChangelogSummary(dir, bead)
		assert.Contains(t, got, "Plain bullet")
		assert.NotContains(t, got, "Technical EN bullet")
	})
}
