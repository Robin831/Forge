package pipeline

import (
	"context"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hooks"
)

// hookEnv is a pipeline-local alias for hooks.HookEnv.
type hookEnv = hooks.HookEnv

// runHook delegates to hooks.RunHook.
func runHook(ctx context.Context, workerID, hookName, cmd string, env hookEnv) error {
	return hooks.RunHook(ctx, workerID, hookName, cmd, env)
}

// hookCmd delegates to hooks.HookCmd.
func hookCmd(hc *config.HooksConfig, name string) string {
	return hooks.HookCmd(hc, name)
}
