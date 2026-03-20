package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/Robin831/Forge/internal/executil"
)

// InstallGoGroup runs `go get pkg1@v1 pkg2@v2 ...` followed by `go mod tidy`
// in the given module directory. Returns an error if either command fails.
func InstallGoGroup(ctx context.Context, moduleDir string, group UpdateGroup) error {
	if len(group.Updates) == 0 {
		return nil
	}

	args := []string{"get"}
	for _, u := range group.Updates {
		args = append(args, u.Path+"@"+u.Latest)
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "go", args...))
	cmd.Dir = moduleDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go get for group %q: %w\nstderr: %s", group.Name, err, stderr.String())
	}

	// Tidy up the module graph after updating.
	tidyCmd := executil.HideWindow(exec.CommandContext(ctx, "go", "mod", "tidy"))
	tidyCmd.Dir = moduleDir

	var tidyStderr bytes.Buffer
	tidyCmd.Stderr = &tidyStderr

	if err := tidyCmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy after group %q: %w\nstderr: %s", group.Name, err, tidyStderr.String())
	}
	return nil
}
