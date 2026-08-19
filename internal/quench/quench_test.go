package quench

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hooks"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vcs"
)

func TestBuildBatchCIPrompt(t *testing.T) {
	p := BatchFixParams{
		PRNumber:     42,
		Branch:       "forge/Forge-xyz",
		BeadID:       "Forge-xyz",
		WorktreePath: "/tmp/worktree",
		FailingChecks: []vcs.CICheck{
			{Name: "build", Status: "fail"},
			{Name: "lint", Status: "fail"},
			{Name: "test", Status: "fail"},
		},
		CILogs: map[string]string{
			"build": "error: missing import\n  main.go:10",
			"lint":  "golangci-lint: unused variable x",
		},
	}

	prompt := buildBatchCIPrompt(p)

	// Verify PR metadata is included.
	if !strings.Contains(prompt, "PR #42") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "forge/Forge-xyz") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "Forge-xyz") {
		t.Error("prompt should contain bead ID")
	}

	// Verify all check names are present.
	for _, name := range []string{"build", "lint", "test"} {
		if !strings.Contains(prompt, name) {
			t.Errorf("prompt should contain check name %q", name)
		}
	}

	// Verify CI logs are included for checks that have them.
	if !strings.Contains(prompt, "missing import") {
		t.Error("prompt should contain build log content")
	}
	if !strings.Contains(prompt, "unused variable") {
		t.Error("prompt should contain lint log content")
	}

	// Verify numbered list format.
	if !strings.Contains(prompt, "1. build") {
		t.Error("prompt should number checks starting at 1")
	}
	if !strings.Contains(prompt, "2. lint") {
		t.Error("prompt should number checks sequentially")
	}

	// Verify instructions mention all failures.
	if !strings.Contains(prompt, "3 failing checks") {
		t.Error("prompt should mention total number of failing checks in instructions")
	}
}

func TestBuildBatchCIPrompt_NoLogs(t *testing.T) {
	p := BatchFixParams{
		PRNumber:     7,
		Branch:       "fix/ci",
		BeadID:       "test-1",
		WorktreePath: "/tmp/wt",
		FailingChecks: []vcs.CICheck{
			{Name: "changelog-check", Status: "fail"},
		},
		CILogs: nil,
	}

	prompt := buildBatchCIPrompt(p)

	if !strings.Contains(prompt, "changelog-check") {
		t.Error("prompt should contain check name even without logs")
	}
	// Should not contain log block markers when no logs available.
	if strings.Contains(prompt, "**CI Log:**") {
		t.Error("prompt should not contain CI Log header when no log for the check exists")
	}
}

func TestBatchFix_NoFailingChecks(t *testing.T) {
	result := BatchFix(context.Background(), BatchFixParams{
		PRNumber:      42,
		Branch:        "forge/test",
		BeadID:        "test-1",
		WorktreePath:  "/tmp/wt",
		FailingChecks: nil,
	})

	if !result.Fixed {
		t.Error("BatchFix with no failing checks should return Fixed=true")
	}
	if result.Error != nil {
		t.Errorf("BatchFix with no failing checks should not error, got: %v", result.Error)
	}
}

func TestBatchFix_NoProviders(t *testing.T) {
	// With an empty provider list, the smith loop never executes and
	// smithResult stays nil. Verify BatchFix surfaces the error.
	result := BatchFix(context.Background(), BatchFixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BeadID:       "test-1",
		WorktreePath: t.TempDir(),
		FailingChecks: []vcs.CICheck{
			{Name: "build", Status: "fail"},
		},
		Providers: []provider.Provider{},
	})

	if result.Fixed {
		t.Error("BatchFix should not return Fixed=true when smith cannot spawn")
	}
	if result.Error == nil {
		t.Error("BatchFix should return an error when smith fails to spawn")
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "hello"
	if got := truncateOutput(short, 10); got != short {
		t.Errorf("truncateOutput(%q, 10) = %q, want %q", short, got, short)
	}

	long := strings.Repeat("x", 100)
	got := truncateOutput(long, 50)
	if !strings.HasPrefix(got, "... (truncated)") {
		t.Error("truncated output should start with truncation marker")
	}
	if len(got) > 50+len("... (truncated)\n") {
		t.Errorf("truncated output too long: %d chars", len(got))
	}
}

// --- Hook tests ---

func TestHookRunFn_BeforeTemperHook_Invoked(t *testing.T) {
	origHookRun := hookRunFn
	defer func() { hookRunFn = origHookRun }()

	var hooksCalled []string
	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if cmd != "" {
			hooksCalled = append(hooksCalled, hookName)
		}
		return nil
	}

	// HookCmd should resolve the configured hook.
	hc := &config.HooksConfig{BeforeTemper: "echo setup"}
	cmd := hooks.HookCmd(hc, "before_temper")
	if cmd != "echo setup" {
		t.Errorf("expected 'echo setup', got %q", cmd)
	}

	// Simulate what quench does: call hookRunFn.
	env := hooks.HookEnv{
		BeadID:    "test-bead",
		Stage:     "temper",
		Iteration: 1,
	}
	err := hookRunFn(context.Background(), "w1", "before_temper", cmd, env)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(hooksCalled) != 1 || hooksCalled[0] != "before_temper" {
		t.Errorf("expected [before_temper], got %v", hooksCalled)
	}
}

func TestHookRunFn_BeforeTemperHook_Fails_AbortsQuench(t *testing.T) {
	origHookRun := hookRunFn
	defer func() { hookRunFn = origHookRun }()

	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "before_temper" && cmd != "" {
			return fmt.Errorf("hook before_temper failed: exit status 1")
		}
		return nil
	}

	hc := &config.HooksConfig{BeforeTemper: "exit 1"}
	cmd := hooks.HookCmd(hc, "before_temper")

	env := hooks.HookEnv{BeadID: "test-bead", Stage: "temper", Iteration: 1}
	err := hookRunFn(context.Background(), "w1", "before_temper", cmd, env)
	if err == nil {
		t.Fatal("expected error from before_temper hook")
	}
	if !strings.Contains(err.Error(), "before_temper") {
		t.Errorf("error should mention before_temper, got: %v", err)
	}
}

func TestHookRunFn_AfterTemperHook_Fails_NonFatal(t *testing.T) {
	origHookRun := hookRunFn
	defer func() { hookRunFn = origHookRun }()

	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "after_temper" && cmd != "" {
			return fmt.Errorf("after_temper hook failed")
		}
		return nil
	}

	hc := &config.HooksConfig{AfterTemper: "exit 1"}
	cmd := hooks.HookCmd(hc, "after_temper")

	env := hooks.HookEnv{BeadID: "test-bead", Stage: "temper", Iteration: 1}
	err := hookRunFn(context.Background(), "w1", "after_temper", cmd, env)
	// The error is returned by hookRunFn, but quench only logs it.
	// Verify the hook does return an error (quench's code path logs it and continues).
	if err == nil {
		t.Fatal("expected error from after_temper hook")
	}
}

func TestHookRunFn_NilHooks_NoOp(t *testing.T) {
	origHookRun := hookRunFn
	defer func() { hookRunFn = origHookRun }()

	called := false
	hookRunFn = func(_ context.Context, _, _, cmd string, _ hooks.HookEnv) error {
		if cmd != "" {
			called = true
		}
		return nil
	}

	// HookCmd with nil hooks returns "".
	cmd := hooks.HookCmd(nil, "before_temper")
	if cmd != "" {
		t.Errorf("expected empty cmd for nil hooks, got %q", cmd)
	}

	// hookRunFn with empty cmd is a no-op (the real RunHook returns nil immediately).
	env := hooks.HookEnv{BeadID: "test-bead", Stage: "temper"}
	_ = hookRunFn(context.Background(), "w1", "before_temper", cmd, env)
	if called {
		t.Error("hook should not have been called with empty cmd")
	}
}

// --- Fix-level hook ordering / abort tests ---

// fakeVCS is a minimal vcs.Provider stub for quench Fix tests.
type fakeVCS struct {
	failingChecks []vcs.CICheck
}

func (f *fakeVCS) CreatePR(_ context.Context, _ vcs.CreateParams) (*vcs.PR, error) {
	return nil, nil
}
func (f *fakeVCS) MergePR(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeVCS) CheckStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (f *fakeVCS) CheckStatusLight(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (f *fakeVCS) ListOpenPRs(_ context.Context, _ string) ([]vcs.OpenPR, error) {
	return nil, nil
}
func (f *fakeVCS) GetPRByHeadBranch(_ context.Context, _ string, _ string) (*vcs.OpenPR, error) {
	return nil, nil
}
func (f *fakeVCS) GetRepoOwnerAndName(_ context.Context, _ string) (string, string, error) {
	return "owner", "repo", nil
}
func (f *fakeVCS) FetchUnresolvedThreadCount(_ context.Context, _ string, _ int) (int, error) {
	return 0, nil
}
func (f *fakeVCS) FetchPendingReviewRequests(_ context.Context, _ string, _ int) ([]vcs.ReviewRequest, error) {
	return nil, nil
}
func (f *fakeVCS) FetchPRChecks(_ context.Context, _ string, _ int) (string, []vcs.CICheck, error) {
	return "", f.failingChecks, nil
}
func (f *fakeVCS) FetchCILogs(_ context.Context, _ string, _ []vcs.CICheck) (map[string]string, error) {
	return nil, nil
}
func (f *fakeVCS) FetchReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (f *fakeVCS) ResolveThread(_ context.Context, _ string, _ string) error { return nil }
func (f *fakeVCS) Platform() vcs.Platform                                    { return vcs.GitHub }

// openTestDB opens a temporary SQLite state database for use in tests.
func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// quenchTestHarness saves and restores the package-level function stubs.
type quenchTestHarness struct {
	origHookRun    func(ctx context.Context, workerID, hookName, cmd string, env hooks.HookEnv) error
	origTemperRun  func(ctx context.Context, worktreePath string, cfg temper.Config, db *state.DB, beadID, anvil string) *temper.Result
	origSmithSpawn func(ctx context.Context, worktreePath, prompt, logDir string, pv provider.Provider, extraFlags []string) (*smith.Process, error)
}

func newQuenchTestHarness() *quenchTestHarness {
	return &quenchTestHarness{
		origHookRun:    hookRunFn,
		origTemperRun:  temperRunFn,
		origSmithSpawn: smithSpawnFn,
	}
}

func (h *quenchTestHarness) restore() {
	hookRunFn = h.origHookRun
	temperRunFn = h.origTemperRun
	smithSpawnFn = h.origSmithSpawn
}

// TestFix_BeforeTemper_AbortsOnError verifies that a before_temper hook failure
// prevents temper from running and causes Fix to return an error.
func TestFix_BeforeTemper_AbortsOnError(t *testing.T) {
	h := newQuenchTestHarness()
	defer h.restore()

	temperCalled := false
	temperRunFn = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperCalled = true
		return &temper.Result{Passed: true}
	}

	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "before_temper" && cmd != "" {
			return fmt.Errorf("before_temper hook failed")
		}
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
		Hooks:        &config.HooksConfig{BeforeTemper: "exit 1"},
	})

	if result.Fixed {
		t.Error("Fix should not be Fixed=true when before_temper hook aborts")
	}
	if result.Error == nil {
		t.Fatal("Fix should return an error when before_temper hook fails")
	}
	if !strings.Contains(result.Error.Error(), "before_temper") {
		t.Errorf("error should mention before_temper, got: %v", result.Error)
	}
	if temperCalled {
		t.Error("temperRunFn should not be called when before_temper hook aborts")
	}
}

// TestFix_AfterTemper_NonFatal_ReproRun verifies that an after_temper hook
// failure during the repro temper run is non-fatal: Fix continues and returns
// Fixed=true when temper passes and no CI checks are failing.
func TestFix_AfterTemper_NonFatal_ReproRun(t *testing.T) {
	h := newQuenchTestHarness()
	defer h.restore()

	temperRunFn = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		return &temper.Result{Passed: true}
	}

	afterTemperCalled := false
	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "after_temper" && cmd != "" {
			afterTemperCalled = true
			return fmt.Errorf("after_temper hook failed")
		}
		return nil
	}

	cfg := temper.Config{}
	// VCS returns no failing checks — so temper passing means we're fixed.
	result := Fix(context.Background(), FixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BeadID:       "test-bead",
		WorktreePath: t.TempDir(),
		VCS:          &fakeVCS{},
		Providers:    []provider.Provider{{Kind: provider.Claude}},
		TemperConfig: &cfg,
		Hooks:        &config.HooksConfig{AfterTemper: "exit 1"},
	})

	if !afterTemperCalled {
		t.Error("after_temper hook should have been called")
	}
	if !result.Fixed {
		t.Errorf("Fix should return Fixed=true even when after_temper hook fails (non-fatal); error: %v", result.Error)
	}
	if result.Error != nil {
		t.Errorf("Fix should not return an error when after_temper hook fails (non-fatal), got: %v", result.Error)
	}
}

// TestFix_AfterTemper_NonFatal_VerifyRun verifies that an after_temper hook
// failure during the verify temper run (after Smith) is also non-fatal: Fix
// continues and returns Fixed=true when the verify temper passes.
func TestFix_AfterTemper_NonFatal_VerifyRun(t *testing.T) {
	h := newQuenchTestHarness()
	defer h.restore()

	// First temper call (repro) fails so we proceed to smith.
	// Second temper call (verify) passes.
	callCount := 0
	temperRunFn = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		callCount++
		if callCount == 1 {
			return &temper.Result{Passed: false, FailedStep: "build"}
		}
		return &temper.Result{Passed: true}
	}

	// Smith succeeds.
	smithSpawnFn = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{
			ExitCode:      0,
			ResultSubtype: "success",
			IsError:       false,
		}), nil
	}

	afterTemperCallCount := 0
	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "after_temper" && cmd != "" {
			afterTemperCallCount++
			return fmt.Errorf("after_temper hook failed")
		}
		return nil
	}

	cfg := temper.Config{}
	db := openTestDB(t)
	// VCS returns one failing check so Fix doesn't short-circuit on the repro pass.
	result := Fix(context.Background(), FixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BeadID:       "test-bead",
		WorktreePath: t.TempDir(),
		VCS: &fakeVCS{
			failingChecks: []vcs.CICheck{{Name: "build", Status: "failure"}},
		},
		Providers:    []provider.Provider{{Kind: provider.Claude}},
		TemperConfig: &cfg,
		Hooks:        &config.HooksConfig{AfterTemper: "exit 1"},
		DB:           db,
	})

	if callCount != 2 {
		t.Errorf("expected 2 temperRunFn calls (repro + verify), got %d", callCount)
	}
	if afterTemperCallCount < 1 {
		t.Error("after_temper hook should have been called at least once (on the verify run)")
	}
	if !result.Fixed {
		t.Errorf("Fix should return Fixed=true even when after_temper hook fails (non-fatal); error: %v", result.Error)
	}
	if result.Error != nil {
		t.Errorf("Fix should not return an error when after_temper hook fails (non-fatal), got: %v", result.Error)
	}
}
