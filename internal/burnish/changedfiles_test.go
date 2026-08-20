package burnish

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vcs"
)

// recordTemperConfigs replaces temperRunFn with a stub that records the config
// each verification run was handed and returns the given result.
func recordTemperConfigs(passed bool) *[]temper.Config {
	var mu sync.Mutex
	seen := make([]temper.Config, 0, 2)
	temperRunFn = func(_ context.Context, _ string, cfg temper.Config, _ *state.DB, _, _ string) *temper.Result {
		mu.Lock()
		seen = append(seen, cfg)
		mu.Unlock()
		return &temper.Result{Passed: passed}
	}
	return &seen
}

func changesRequested() *fakeVCS {
	return &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}
}

// TestFix_VerificationGatesOnChangedFiles is the core of Forge-bxdg: burnish
// verification must hand Temper the files this PR changed. A nil list means
// "unknown" to Temper and disables path filtering outright, so without this
// even steps that declare `paths` ran on every burnish attempt — the whole
// backend suite re-run for a one-line frontend fix.
func TestFix_VerificationGatesOnChangedFiles(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	seen := recordTemperConfigs(true)
	gitPushFn = func(_ context.Context, _, _ string) error { return nil }
	gitRevParseFn = noRevParse

	var gotWorktree, gotBase string
	changedFilesFn = func(_ context.Context, worktreePath, baseBranch, _ string) []string {
		gotWorktree, gotBase = worktreePath, baseBranch
		return []string{"web/src/App.tsx"}
	}

	params := defaultFixParams(db, changesRequested())
	params.BaseBranch = "develop"

	result := Fix(context.Background(), params)

	if !result.Addressed {
		t.Fatalf("expected Addressed=true, got error: %v", result.Error)
	}
	if gotWorktree != params.WorktreePath {
		t.Errorf("changed files computed for %q, want the PR worktree %q", gotWorktree, params.WorktreePath)
	}
	if gotBase != "develop" {
		t.Errorf("changed files computed against base %q, want the PR's base branch %q", gotBase, "develop")
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 verification run, got %d", len(*seen))
	}
	if got := (*seen)[0].ChangedFiles; len(got) != 1 || got[0] != "web/src/App.tsx" {
		t.Errorf("verification config carried ChangedFiles=%v, want the branch's changed files", got)
	}
}

// TestFix_ChangedFilesError_FailsOpen pins the fail-open contract: a git
// failure says nothing about the diff, so verification runs with a nil list
// (which runs every step) rather than gating on a list it could not read or
// failing the fix outright.
func TestFix_ChangedFilesError_FailsOpen(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	seen := recordTemperConfigs(true)
	gitPushFn = func(_ context.Context, _, _ string) error { return nil }
	gitRevParseFn = noRevParse

	// The fail-open decision lives in temper.ChangedFilesOrNil, which answers
	// nil on a git error; the stub stands in for that answer.
	changedFilesFn = func(_ context.Context, _, _, _ string) []string {
		return nil
	}

	result := Fix(context.Background(), defaultFixParams(db, changesRequested()))

	if !result.Addressed {
		t.Fatalf("a changed-file failure must not fail the fix; got error: %v", result.Error)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 verification run, got %d", len(*seen))
	}
	if (*seen)[0].ChangedFiles != nil {
		t.Errorf("expected nil ChangedFiles (run everything), got %v", (*seen)[0].ChangedFiles)
	}
}

// TestFix_TimeoutRetryKeepsChangedFiles covers the retry path: a verification
// re-run after a timeout re-verifies the same tree, so it must be gated the
// same way rather than silently reverting to running everything.
func TestFix_TimeoutRetryKeepsChangedFiles(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)

	var mu sync.Mutex
	var seen []temper.Config
	calls := 0
	temperRunFn = func(ctx context.Context, _ string, cfg temper.Config, _ *state.DB, _, _ string) *temper.Result {
		mu.Lock()
		seen = append(seen, cfg)
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-ctx.Done()
			return &temper.Result{Passed: false, FailedStep: "blocked"}
		}
		return &temper.Result{Passed: true}
	}
	gitPushFn = func(_ context.Context, _, _ string) error { return nil }
	gitRevParseFn = fixedRevParse("aaaaaaaaaaaa", "bbbbbbbbbbbb")
	changedFilesFn = func(_ context.Context, _, _, _ string) []string {
		return []string{"internal/api/handler.go"}
	}

	params := defaultFixParams(db, changesRequested())
	params.VerifyTimeout = 50 * time.Millisecond
	params.VerifyRetries = 1

	result := Fix(context.Background(), params)

	if !result.Addressed {
		t.Fatalf("expected Addressed=true after the retry verified, got error: %v", result.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 verification runs (1 timeout + 1 retry), got %d", len(seen))
	}
	for i, cfg := range seen {
		if len(cfg.ChangedFiles) != 1 || cfg.ChangedFiles[0] != "internal/api/handler.go" {
			t.Errorf("run %d carried ChangedFiles=%v, want the branch's changed files", i+1, cfg.ChangedFiles)
		}
	}
}

// TestBatchFix_VerificationGatesOnChangedFiles covers the batched path, which
// reaches verification through its own params struct.
func TestBatchFix_VerificationGatesOnChangedFiles(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	seen := recordTemperConfigs(true)
	gitPushFn = func(_ context.Context, _, _ string) error { return nil }
	gitRevParseFn = noRevParse

	var gotBase string
	changedFilesFn = func(_ context.Context, _, baseBranch, _ string) []string {
		gotBase = baseBranch
		return []string{"src/Api/Program.cs"}
	}

	v := changesRequested()
	result := BatchFix(context.Background(), BatchFixParams{
		WorktreePath: t.TempDir(),
		BeadID:       "test-1",
		AnvilName:    "test-anvil",
		PRNumber:     42,
		Branch:       "forge/test",
		BaseBranch:   "feature/Forge-epic",
		DB:           db,
		Comments:     v.comments,
		VCS:          v,
		Providers:    []provider.Provider{{Kind: provider.Claude, Model: "test"}},
	})

	if !result.Addressed {
		t.Fatalf("expected Addressed=true, got error: %v", result.Error)
	}
	if gotBase != "feature/Forge-epic" {
		t.Errorf("changed files computed against base %q, want the PR's base branch", gotBase)
	}
	if len(*seen) != 1 || len((*seen)[0].ChangedFiles) != 1 {
		t.Fatalf("expected one gated verification run, got %+v", *seen)
	}
}

// TestLogSkippedSteps_ReportsOnlyGatedSkips keeps the burnish log honest: a
// step Temper blocked is not a step the diff made unnecessary.
func TestLogSkippedSteps_ReportsOnlyGatedSkips(t *testing.T) {
	r := &temper.Result{Steps: []temper.StepResult{
		{Name: "dotnet-build", Skipped: true, SkipReason: temper.SkipReasonPathFilter},
		{Name: "install", Skipped: true, SkipReason: temper.SkipReasonBlockedInstall},
		{Name: "web:test", Passed: true},
	}}

	got := temper.PathSkippedStepNames(r)

	if len(got) != 1 || got[0] != "dotnet-build" {
		t.Errorf("PathSkippedStepNames = %v, want only the path-gated step", got)
	}
	// The logger itself must tolerate a nil result (a timed-out run has none).
	temper.LogPathSkips(verifyParams{prNumber: 1, beadID: "b"}.logPrefix(), nil)
}

// TestFix_ChangedFilesRederivedPerAttempt covers the loop-back path: a
// verification failure sends burnish back to Smith, which rewrites files, so
// the next attempt's verification must be gated on a freshly derived list
// rather than the one the failed attempt computed. A stub answering the same
// list every time cannot tell the two apart, so this one answers differently
// per call and counts them.
func TestFix_ChangedFilesRederivedPerAttempt(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	gitPushFn = func(_ context.Context, _, _ string) error { return nil }
	gitRevParseFn = noRevParse

	var mu sync.Mutex
	var seen []temper.Config
	temperCalls := 0
	temperRunFn = func(_ context.Context, _ string, cfg temper.Config, _ *state.DB, _, _ string) *temper.Result {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, cfg)
		temperCalls++
		if temperCalls == 1 {
			return &temper.Result{Passed: false, FailedStep: "test"}
		}
		return &temper.Result{Passed: true}
	}

	changedCalls := 0
	changedFilesFn = func(_ context.Context, _, _, _ string) []string {
		mu.Lock()
		defer mu.Unlock()
		changedCalls++
		if changedCalls == 1 {
			return []string{"internal/api/handler.go"}
		}
		return []string{"internal/api/handler.go", "internal/api/handler_test.go"}
	}

	params := defaultFixParams(db, changesRequested())
	params.MaxAttempts = 2

	result := Fix(context.Background(), params)

	if !result.Addressed {
		t.Fatalf("expected Addressed=true after the second attempt verified, got error: %v", result.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 verification runs (one per Smith attempt), got %d", len(seen))
	}
	if changedCalls != 2 {
		t.Errorf("changed files computed %d time(s), want once per attempt (2)", changedCalls)
	}
	if slices.Equal(seen[0].ChangedFiles, seen[1].ChangedFiles) {
		t.Errorf("both attempts carried the same ChangedFiles (%v) — the list was not re-derived after Smith reran", seen[0].ChangedFiles)
	}
	if len(seen[1].ChangedFiles) != 2 {
		t.Errorf("second attempt carried ChangedFiles=%v, want the list re-derived after Smith rewrote files", seen[1].ChangedFiles)
	}
}
