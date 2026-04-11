// Package hooks provides shared pipeline stage hook execution for pipeline,
// burnish, and quench workers. Hooks are shell commands configured per-anvil
// that run before or after specific pipeline stages (smith, temper, warden, etc.).
package hooks

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
)

// HookTimeout is the maximum time a single hook command is allowed to run.
const HookTimeout = 60 * time.Second

// HookEnv holds the environment variables passed to pipeline hook commands.
type HookEnv struct {
	BeadID       string
	WorktreePath string
	Branch       string
	AnvilName    string
	AnvilPath    string
	Stage        string
	Iteration    int
}

// Environ returns the hook context as a slice of KEY=VALUE strings suitable
// for appending to exec.Cmd.Env.
func (h HookEnv) Environ() []string {
	return []string{
		"FORGE_BEAD_ID=" + h.BeadID,
		"FORGE_WORKTREE_PATH=" + h.WorktreePath,
		"FORGE_BRANCH=" + h.Branch,
		"FORGE_ANVIL_NAME=" + h.AnvilName,
		"FORGE_ANVIL_PATH=" + h.AnvilPath,
		"FORGE_STAGE=" + h.Stage,
		"FORGE_ITERATION=" + strconv.Itoa(h.Iteration),
	}
}

// FilterForgeEnv returns a copy of environ with any existing FORGE_* variables
// removed so that hook-specific values are not shadowed or duplicated.
func FilterForgeEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, e := range environ {
		if !strings.HasPrefix(e, "FORGE_") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// ShellArgs returns the platform-appropriate shell and flag for executing a
// command string. On Windows it uses "cmd /c"; elsewhere "sh -c".
func ShellArgs() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/c"
	}
	return "sh", "-c"
}

// RunHook executes a shell command with the given hook environment. It returns
// nil when cmd is empty (no hook configured). The command is run via a
// platform-appropriate shell (sh -c on Unix, cmd /c on Windows) with the
// worktree as the working directory. A dedicated timeout of HookTimeout is
// applied to prevent hooks from blocking the pipeline indefinitely.
func RunHook(ctx context.Context, workerID, hookName, cmd string, env HookEnv) error {
	if cmd == "" {
		return nil
	}
	hookCtx, cancel := context.WithTimeout(ctx, HookTimeout)
	defer cancel()

	log.Printf("[hooks:%s] Running hook %s", workerID, hookName)
	shell, flag := ShellArgs()
	c := executil.HideWindow(exec.CommandContext(hookCtx, shell, flag, cmd))
	c.Dir = env.WorktreePath
	c.Env = append(FilterForgeEnv(os.Environ()), env.Environ()...)
	out, err := c.CombinedOutput()
	if err != nil {
		log.Printf("[hooks:%s] Hook %s failed: %v (output omitted)", workerID, hookName, err)
		return fmt.Errorf("hook %s failed: %w", hookName, err)
	}
	log.Printf("[hooks:%s] Hook %s completed (%d bytes output)", workerID, hookName, len(out))
	return nil
}

// HookCmd resolves the command string for a named hook from the anvil config.
// Returns "" when hooks are not configured or the named hook is not set.
func HookCmd(hooks *config.HooksConfig, name string) string {
	if hooks == nil {
		return ""
	}
	switch name {
	case "before_schematic":
		return hooks.BeforeSchematic
	case "after_schematic":
		return hooks.AfterSchematic
	case "before_smith":
		return hooks.BeforeSmith
	case "after_smith":
		return hooks.AfterSmith
	case "before_temper":
		return hooks.BeforeTemper
	case "after_temper":
		return hooks.AfterTemper
	case "before_warden":
		return hooks.BeforeWarden
	case "after_warden":
		return hooks.AfterWarden
	default:
		return ""
	}
}
