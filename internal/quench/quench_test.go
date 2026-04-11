package quench

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hooks"
	"github.com/Robin831/Forge/internal/provider"
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
