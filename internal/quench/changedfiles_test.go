package quench

import (
	"context"
	"slices"
	"testing"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vcs"
)

// TestFix_TemperGatesOnChangedFiles is the quench half of Forge-bxdg: both the
// reproduce run and the post-Smith verify run must hand Temper the files this
// PR changed. A nil list means "unknown" to Temper and disables path filtering
// outright, so a frontend-only CI failure re-ran the whole backend suite.
func TestFix_TemperGatesOnChangedFiles(t *testing.T) {
	h := newQuenchTestHarness()
	defer h.restore()

	var seen []temper.Config
	callCount := 0
	temperRunFn = func(_ context.Context, _ string, cfg temper.Config, _ *state.DB, _, _ string) *temper.Result {
		seen = append(seen, cfg)
		callCount++
		if callCount == 1 {
			return &temper.Result{Passed: false, FailedStep: "build"}
		}
		return &temper.Result{Passed: true}
	}
	smithSpawnFn = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, ResultSubtype: "success"}), nil
	}

	// A different answer per call, so the two runs cannot both be satisfied by
	// one computation: Smith commits a fix between them, and the verify run
	// has to see the file it ADDED. A stub returning the same list every time
	// would pass even if the list were hoisted to once per attempt.
	var gotWorktree, gotBase string
	changedCalls := 0
	changedFilesFn = func(_ context.Context, worktreePath, baseBranch, _ string) []string {
		gotWorktree, gotBase = worktreePath, baseBranch
		changedCalls++
		if changedCalls == 1 {
			return []string{"client/src/App.tsx"}
		}
		return []string{"client/src/App.tsx", "client/src/Fixed.tsx"}
	}

	// The anvil's shared temper config: quench must not stamp this PR's
	// changed-file list onto it.
	anvilCfg := temper.Config{Steps: []temper.Step{{Name: "build", Command: "echo"}}}
	worktree := t.TempDir()
	db := openTestDB(t)

	result := Fix(context.Background(), FixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BaseBranch:   "develop",
		BeadID:       "test-bead",
		WorktreePath: worktree,
		VCS:          &fakeVCS{failingChecks: []vcs.CICheck{{Name: "build", Status: "failure"}}},
		Providers:    []provider.Provider{{Kind: provider.Claude}},
		TemperConfig: &anvilCfg,
		DB:           db,
	})

	if !result.Fixed {
		t.Fatalf("expected Fixed=true after the verify run passed; error: %v", result.Error)
	}
	if gotWorktree != worktree || gotBase != "develop" {
		t.Errorf("changed files computed for (%q, %q), want (%q, %q)", gotWorktree, gotBase, worktree, "develop")
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 temper runs (repro + verify), got %d", len(seen))
	}
	if changedCalls != 2 {
		t.Errorf("changed files computed %d time(s), want once per temper run (2)", changedCalls)
	}
	if len(seen[0].ChangedFiles) != 1 || seen[0].ChangedFiles[0] != "client/src/App.tsx" {
		t.Errorf("reproduce run carried ChangedFiles=%v, want the branch's changed files", seen[0].ChangedFiles)
	}
	// The verify run must carry the RE-derived list, not the reproduce run's:
	// a file Smith created between them is gated on by the second list alone.
	if len(seen[1].ChangedFiles) != 2 || seen[1].ChangedFiles[1] != "client/src/Fixed.tsx" {
		t.Errorf("verify run carried ChangedFiles=%v, want the list re-derived after Smith committed", seen[1].ChangedFiles)
	}
	if slices.Equal(seen[0].ChangedFiles, seen[1].ChangedFiles) {
		t.Errorf("both temper runs carried the same ChangedFiles (%v) — the post-Smith list was not re-derived", seen[0].ChangedFiles)
	}
	if anvilCfg.ChangedFiles != nil {
		t.Errorf("the anvil's shared temper config was mutated: ChangedFiles=%v", anvilCfg.ChangedFiles)
	}
}

// TestFix_ChangedFilesError_FailsOpen pins the fail-open contract: a git
// failure leaves the list nil (Temper then runs every step) instead of gating
// on something unreadable or failing the CI fix.
func TestFix_ChangedFilesError_FailsOpen(t *testing.T) {
	h := newQuenchTestHarness()
	defer h.restore()

	var seen []temper.Config
	temperRunFn = func(_ context.Context, _ string, cfg temper.Config, _ *state.DB, _, _ string) *temper.Result {
		seen = append(seen, cfg)
		return &temper.Result{Passed: true}
	}
	// The fail-open decision lives in temper.ChangedFilesOrNil, which answers
	// nil on a git error; the stub stands in for that answer.
	changedFilesFn = func(_ context.Context, _, _, _ string) []string {
		return nil
	}

	cfg := temper.Config{}
	result := Fix(context.Background(), FixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BeadID:       "test-bead",
		WorktreePath: t.TempDir(),
		VCS:          &fakeVCS{},
		Providers:    []provider.Provider{{Kind: provider.Claude}},
		TemperConfig: &cfg,
	})

	if !result.Fixed {
		t.Fatalf("a changed-file failure must not fail the CI fix; error: %v", result.Error)
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 temper run, got %d", len(seen))
	}
	if seen[0].ChangedFiles != nil {
		t.Errorf("expected nil ChangedFiles (run everything), got %v", seen[0].ChangedFiles)
	}
}
