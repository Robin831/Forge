package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfNoShell(t *testing.T) {
	t.Helper()
	shell, _ := ShellArgs()
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("hooks require %s which is not available", shell)
	}
}

// skipIfWindows skips tests that use POSIX shell syntax ($VAR, >>, redirection)
// which is not compatible with cmd /c on Windows.
func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell syntax not supported by cmd /c on Windows")
	}
}

func TestRunHook_Empty_NoOp(t *testing.T) {
	err := RunHook(context.Background(), "w1", "before_smith", "", HookEnv{})
	assert.NoError(t, err)
}

func TestRunHook_Success(t *testing.T) {
	skipIfNoShell(t)
	skipIfWindows(t) // uses POSIX $VAR expansion and > redirection
	dir := t.TempDir()
	env := HookEnv{
		BeadID:       "bead-1",
		WorktreePath: dir,
		Branch:       "forge/bead-1",
		AnvilName:    "myrepo",
		AnvilPath:    "/tmp/myrepo",
		Stage:        "smith",
		Iteration:    2,
	}
	marker := filepath.Join(dir, "hook-ran.txt")
	cmd := "echo $FORGE_BEAD_ID $FORGE_STAGE $FORGE_ITERATION > " + marker
	err := RunHook(context.Background(), "w1", "before_smith", cmd, env)
	require.NoError(t, err)

	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Contains(t, string(data), "bead-1 smith 2")
}

func TestRunHook_Failure(t *testing.T) {
	skipIfNoShell(t)
	env := HookEnv{WorktreePath: t.TempDir()}
	err := RunHook(context.Background(), "w1", "before_temper", "exit 1", env)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "hook before_temper failed")
}

func TestHookCmd_NilHooks(t *testing.T) {
	assert.Equal(t, "", HookCmd(nil, "before_smith"))
}

func TestHookCmd_AllStages(t *testing.T) {
	h := &config.HooksConfig{
		BeforeSchematic: "a",
		AfterSchematic:  "b",
		BeforeSmith:     "c",
		AfterSmith:      "d",
		BeforeTemper:    "e",
		AfterTemper:     "f",
		BeforeWarden:    "g",
		AfterWarden:     "h",
	}
	assert.Equal(t, "a", HookCmd(h, "before_schematic"))
	assert.Equal(t, "b", HookCmd(h, "after_schematic"))
	assert.Equal(t, "c", HookCmd(h, "before_smith"))
	assert.Equal(t, "d", HookCmd(h, "after_smith"))
	assert.Equal(t, "e", HookCmd(h, "before_temper"))
	assert.Equal(t, "f", HookCmd(h, "after_temper"))
	assert.Equal(t, "g", HookCmd(h, "before_warden"))
	assert.Equal(t, "h", HookCmd(h, "after_warden"))
	assert.Equal(t, "", HookCmd(h, "unknown"))
}

func TestHookEnv_Environ(t *testing.T) {
	env := HookEnv{
		BeadID:       "test-bead",
		WorktreePath: "/tmp/wt",
		Branch:       "forge/test-bead",
		AnvilName:    "repo",
		AnvilPath:    "/src/repo",
		Stage:        "temper",
		Iteration:    3,
	}
	vars := env.Environ()
	assert.Contains(t, vars, "FORGE_BEAD_ID=test-bead")
	assert.Contains(t, vars, "FORGE_WORKTREE_PATH=/tmp/wt")
	assert.Contains(t, vars, "FORGE_BRANCH=forge/test-bead")
	assert.Contains(t, vars, "FORGE_ANVIL_NAME=repo")
	assert.Contains(t, vars, "FORGE_ANVIL_PATH=/src/repo")
	assert.Contains(t, vars, "FORGE_STAGE=temper")
	assert.Contains(t, vars, "FORGE_ITERATION=3")
}

func TestFilterForgeEnv(t *testing.T) {
	input := []string{
		"HOME=/home/user",
		"FORGE_BEAD_ID=old-bead",
		"PATH=/usr/bin",
		"FORGE_STAGE=smith",
		"GOPATH=/go",
	}
	got := FilterForgeEnv(input)
	assert.Equal(t, []string{"HOME=/home/user", "PATH=/usr/bin", "GOPATH=/go"}, got)
}

func TestShellArgs(t *testing.T) {
	shell, flag := ShellArgs()
	assert.NotEmpty(t, shell)
	assert.NotEmpty(t, flag)
}
