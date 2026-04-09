package pipeline

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
)

// hookEnv holds the environment variables passed to pipeline hook commands.
type hookEnv struct {
	BeadID       string
	WorktreePath string
	Branch       string
	AnvilName    string
	AnvilPath    string
	Stage        string
	Iteration    int
}

// environ returns the hook context as a slice of KEY=VALUE strings suitable
// for appending to exec.Cmd.Env.
func (h hookEnv) environ() []string {
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

// runHook executes a shell command with the given hook environment. It returns
// nil when cmd is empty (no hook configured). The command is run via "sh -c"
// with the worktree as the working directory.
func runHook(ctx context.Context, workerID, hookName, cmd string, env hookEnv) error {
	if cmd == "" {
		return nil
	}
	log.Printf("[pipeline:%s] Running hook %s: %s", workerID, hookName, cmd)
	c := executil.HideWindow(exec.CommandContext(ctx, "sh", "-c", cmd))
	c.Dir = env.WorktreePath
	c.Env = append(c.Environ(), env.environ()...)
	out, err := c.CombinedOutput()
	if err != nil {
		log.Printf("[pipeline:%s] Hook %s failed: %v\n%s", workerID, hookName, err, out)
		return fmt.Errorf("hook %s failed: %w", hookName, err)
	}
	if len(out) > 0 {
		log.Printf("[pipeline:%s] Hook %s output: %s", workerID, hookName, out)
	}
	return nil
}

// hookCmd resolves the command string for a named hook from the anvil config.
// Returns "" when hooks are not configured or the named hook is not set.
func hookCmd(hooks *config.HooksConfig, name string) string {
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
