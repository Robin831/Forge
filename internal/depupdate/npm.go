package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/Robin831/Forge/internal/executil"
)

// InstallNpmGroup runs `npm install pkg1@v1 pkg2@v2 ...` with all packages in
// the group at once. Returns an error if the install fails (peer dep conflict,
// network error, etc.).
func InstallNpmGroup(ctx context.Context, projectDir string, group UpdateGroup) error {
	if len(group.Updates) == 0 {
		return nil
	}

	args := []string{"install"}
	for _, u := range group.Updates {
		args = append(args, u.Path+"@"+u.Latest)
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "npm", args...))
	cmd.Dir = projectDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install for group %q: %w\nstderr: %s", group.Name, err, stderr.String())
	}
	return nil
}
