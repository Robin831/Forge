package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
)

// InstallDotnetGroup runs `dotnet add <csproj> package <name> -v <version>` for
// each package in the group. It searches the project directory for the .csproj
// file that contains a PackageReference for each package.
func InstallDotnetGroup(ctx context.Context, projectDir string, group UpdateGroup) error {
	if len(group.Updates) == 0 {
		return nil
	}

	csprojFiles, err := findCsprojFiles(projectDir)
	if err != nil {
		return fmt.Errorf("searching for csproj files in %s: %w", projectDir, err)
	}
	if len(csprojFiles) == 0 {
		return fmt.Errorf("no .csproj files found in %s", projectDir)
	}

	for _, u := range group.Updates {
		csproj, err := findCsprojForPackage(csprojFiles, u.Path)
		if err != nil {
			return fmt.Errorf("finding csproj for package %s: %w", u.Path, err)
		}

		cmd := executil.HideWindow(exec.CommandContext(ctx,
			"dotnet", "add", csproj, "package", u.Path, "-v", u.Latest))
		cmd.Dir = projectDir

		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("dotnet add package %s@%s to %s: %w\nstderr: %s",
				u.Path, u.Latest, filepath.Base(csproj), err, stderr.String())
		}
	}
	return nil
}

// findCsprojFiles walks the project directory for .csproj files, skipping
// bin, obj, .git, node_modules, .workers, and .worktrees directories.
func findCsprojFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "bin" || name == "obj" || name == ".git" || name == "node_modules" || name == ".workers" || name == ".worktrees" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.ToLower(filepath.Ext(d.Name())) == ".csproj" {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// findCsprojForPackage searches through the given .csproj files for one that
// contains a PackageReference to the named package. If only one .csproj exists,
// it is returned directly. If no matching PackageReference is found, the first
// .csproj in the list is returned and the caller relies on `dotnet add` to
// handle adding a new or implicit reference.
func findCsprojForPackage(csprojFiles []string, packageName string) (string, error) {
	if len(csprojFiles) == 1 {
		return csprojFiles[0], nil
	}

	// Search for a PackageReference that includes this specific package name.
	// We look for Include="<packageName>" (case-insensitive) to avoid false
	// positives from comments, property values, or similarly-named packages.
	needle := fmt.Sprintf(`include="%s"`, strings.ToLower(packageName))
	for _, path := range csprojFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "packagereference") && strings.Contains(lower, needle) {
			return path, nil
		}
	}

	// Fall back to the first csproj if not found — dotnet add will handle the
	// case where the package is new or the reference is implicit.
	return csprojFiles[0], nil
}
