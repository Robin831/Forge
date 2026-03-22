package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/Robin831/Forge/internal/executil"
)

// resolveNpmInstallDir returns the directory in which npm install should run
// for the given group. If group.SourceDir is set, it is used; otherwise
// projectDir (the anvil root) is returned as a fallback.
func resolveNpmInstallDir(projectDir string, group UpdateGroup) string {
	if group.SourceDir != "" {
		return group.SourceDir
	}
	return projectDir
}

// InstallNpmGroup runs `npm install pkg1@v1 pkg2@v2 ...` with all packages in
// the group at once. Returns an error if the install fails (peer dep conflict,
// network error, etc.).
//
// If group.SourceDir is set, npm install runs there (the directory containing
// the relevant package.json). Otherwise projectDir (the anvil root) is used as
// a fallback.
func InstallNpmGroup(ctx context.Context, projectDir string, group UpdateGroup) error {
	if len(group.Updates) == 0 {
		return nil
	}

	installDir := resolveNpmInstallDir(projectDir, group)

	args := []string{"install"}
	for _, u := range group.Updates {
		args = append(args, u.Path+"@"+u.Latest)
	}

	cmd := executil.HideWindow(exec.CommandContext(ctx, "npm", args...))
	cmd.Dir = installDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install for group %q: %w\nstderr: %s", group.Name, err, stderr.String())
	}
	return nil
}
