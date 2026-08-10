package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hooks"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfNoShell(t *testing.T) {
	t.Helper()
	shell, _ := hooks.ShellArgs()
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("hooks require %s which is not available", shell)
	}
}

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell syntax not supported by cmd /c on Windows")
	}
}

// TestBeforeSmithHook_Abort verifies that a failing before_smith hook aborts
// the pipeline with an error.
func TestBeforeSmithHook_Abort(t *testing.T) {
	skipIfNoShell(t)
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.AnvilConfig.Hooks = &config.HooksConfig{
		BeforeSmith: "exit 1",
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove}, nil
	}

	outcome := Run(context.Background(), params)
	assert.False(t, outcome.Success)
	assert.Error(t, outcome.Error)
	assert.Contains(t, outcome.Error.Error(), "before_smith hook")
}

// TestAfterSmithHook_NoAbort verifies that a failing after_smith hook does not
// abort the pipeline (after hooks are best-effort).
func TestAfterSmithHook_NoAbort(t *testing.T) {
	skipIfNoShell(t)
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.AnvilConfig.Hooks = &config.HooksConfig{
		AfterSmith: "exit 1",
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "lgtm"}, nil
	}

	outcome := Run(context.Background(), params)
	assert.True(t, outcome.Success)
}

// TestBeforeTemperHook_ReceivesEnv verifies that a before_temper hook receives
// the correct environment variables and can create files in the worktree.
func TestBeforeTemperHook_ReceivesEnv(t *testing.T) {
	skipIfNoShell(t)
	skipIfWindows(t) // uses POSIX $VAR expansion and > redirection
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	// Use a hook that writes env vars to a file so we can verify them.
	params.AnvilConfig.Hooks = &config.HooksConfig{
		BeforeTemper: `echo "$FORGE_BEAD_ID|$FORGE_STAGE|$FORGE_ANVIL_NAME" > hook-env.txt`,
	}

	// Capture the worktree path from the temper runner so we can read the file.
	var wtPath string
	params.TemperRunner = func(_ context.Context, wt string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		wtPath = wt
		return &temper.Result{Passed: true}
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "ok"}, nil
	}

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Success)

	data, err := os.ReadFile(filepath.Join(wtPath, "hook-env.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "test-bead|temper|test-anvil")
}

// TestHooks_SuccessfulPipeline verifies that all hooks run during a successful
// pipeline pass without interfering.
func TestHooks_SuccessfulPipeline(t *testing.T) {
	skipIfNoShell(t)
	skipIfWindows(t) // uses POSIX >> append redirection
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)

	params.AnvilConfig.Hooks = &config.HooksConfig{
		BeforeSmith:  "echo before_smith >> hook-log.txt",
		AfterSmith:   "echo after_smith >> hook-log.txt",
		BeforeTemper: "echo before_temper >> hook-log.txt",
		AfterTemper:  "echo after_temper >> hook-log.txt",
		BeforeWarden: "echo before_warden >> hook-log.txt",
		AfterWarden:  "echo after_warden >> hook-log.txt",
	}

	var wtPath string
	origSmith := params.SmithRunner
	params.SmithRunner = func(ctx context.Context, wt, promptText, logDir string, pv provider.Provider, flags []string) (*smith.Process, error) {
		wtPath = wt
		return origSmith(ctx, wt, promptText, logDir, pv, flags)
	}
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "lgtm"}, nil
	}

	outcome := Run(context.Background(), params)
	require.True(t, outcome.Success)

	data, err := os.ReadFile(filepath.Join(wtPath, "hook-log.txt"))
	require.NoError(t, err)
	lines := string(data)
	assert.Contains(t, lines, "before_smith")
	assert.Contains(t, lines, "after_smith")
	assert.Contains(t, lines, "before_temper")
	assert.Contains(t, lines, "after_temper")
	assert.Contains(t, lines, "before_warden")
	assert.Contains(t, lines, "after_warden")
}
