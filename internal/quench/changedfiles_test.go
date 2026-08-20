package quench

import (
	"context"
	"errors"
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

	var gotWorktree, gotBase string
	changedFilesFn = func(_ context.Context, worktreePath, baseBranch string) ([]string, error) {
		gotWorktree, gotBase = worktreePath, baseBranch
		return []string{"client/src/App.tsx"}, nil
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
	for i, cfg := range seen {
		if len(cfg.ChangedFiles) != 1 || cfg.ChangedFiles[0] != "client/src/App.tsx" {
			t.Errorf("temper run %d carried ChangedFiles=%v, want the branch's changed files", i+1, cfg.ChangedFiles)
		}
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
	changedFilesFn = func(_ context.Context, _, _ string) ([]string, error) {
		return nil, errors.New("git diff: fatal: bad revision")
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
