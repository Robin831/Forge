package depupdate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// DetectBilingual reports whether the changelog.d/ directory in anvilPath
// contains bilingual fragments (files ending in .en.md or .nb.md). This
// mirrors the convention used by other projects that maintain separate English
// and Norwegian changelog files.
func DetectBilingual(anvilPath string) bool {
	changelogDir := filepath.Join(anvilPath, "changelog.d")
	entries, err := os.ReadDir(changelogDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".en.md") || strings.HasSuffix(name, ".nb.md") {
			return true
		}
	}
	return false
}

// GenerateChangelog writes one or two changelog fragment files for the given
// set of dependency update groups into <anvilPath>/changelog.d/, then
// git-adds and commits them.
//
// Fragment naming:
//   - Monolingual: deps-batch-<YYYY-MM-DD-HHmmss>.md
//   - Bilingual:   deps-batch-<YYYY-MM-DD-HHmmss>.en.md  (English)
//                  deps-batch-<YYYY-MM-DD-HHmmss>.nb.md  (Norwegian — same content)
//
// The timestamp suffix makes each invocation collision-safe so that multiple
// runs on the same day do not overwrite each other.
//
// The isBilingual flag can be set explicitly by the caller, or the caller can
// use DetectBilingual to derive it from the existing changelog directory.
func GenerateChangelog(anvilPath string, groups []UpdateGroup, isBilingual bool) error {
	if len(groups) == 0 {
		return nil
	}

	stamp := time.Now().Format("2006-01-02-150405")
	tag := "deps-batch-" + stamp

	changelogDir := filepath.Join(anvilPath, "changelog.d")
	if err := os.MkdirAll(changelogDir, 0o755); err != nil {
		return fmt.Errorf("depupdate: creating changelog.d: %w", err)
	}

	// relFiles holds repo-relative paths for git add (more portable than
	// absolute paths, especially on Windows with drive letters).
	var relFiles []string

	if isBilingual {
		enName := fmt.Sprintf("%s.en.md", tag)
		nbName := fmt.Sprintf("%s.nb.md", tag)

		enContent := buildFragmentContent(groups, tag, false)
		nbContent := buildFragmentContent(groups, tag, true)

		if err := writeFragment(filepath.Join(changelogDir, enName), enContent); err != nil {
			return err
		}
		if err := writeFragment(filepath.Join(changelogDir, nbName), nbContent); err != nil {
			return err
		}
		relFiles = []string{
			"changelog.d/" + enName,
			"changelog.d/" + nbName,
		}
	} else {
		mdName := fmt.Sprintf("%s.md", tag)
		content := buildFragmentContent(groups, tag, false)
		if err := writeFragment(filepath.Join(changelogDir, mdName), content); err != nil {
			return err
		}
		relFiles = []string{"changelog.d/" + mdName}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	addArgs := append([]string{"add", "--"}, relFiles...)
	addCmd := executil.HideWindow(exec.CommandContext(ctx, "git", addArgs...))
	addCmd.Dir = anvilPath
	addCmd.Stdout = io.Discard

	var addStderr bytes.Buffer
	addCmd.Stderr = &addStderr
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("depupdate: git add changelog fragment: %w\nstderr: %s", err, addStderr.String())
	}

	commitMsg := fmt.Sprintf("chore(deps): add changelog fragment for dependency batch %s", stamp)
	commitCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "commit", "-m", commitMsg))
	commitCmd.Dir = anvilPath
	commitCmd.Stdout = io.Discard

	var commitStderr bytes.Buffer
	commitCmd.Stderr = &commitStderr
	if err := commitCmd.Run(); err != nil {
		// Treat "nothing to commit" as a no-op — this can happen if the
		// fragment content is identical to a previously staged file.
		if strings.Contains(commitStderr.String(), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("depupdate: git commit changelog fragment: %w\nstderr: %s", err, commitStderr.String())
	}

	return nil
}

// buildFragmentContent constructs the changelog fragment body listing every
// updated package across all groups in the standard bold-title format with
// a traceability tag.
func buildFragmentContent(groups []UpdateGroup, tag string, norwegian bool) string {
	var sb strings.Builder
	sb.WriteString("category: Changed\n")
	for _, g := range groups {
		for _, u := range g.Updates {
			if norwegian {
				fmt.Fprintf(&sb, "- **Oppdatert %s** - Bumpet fra %s til %s. (%s)\n", u.Path, u.Current, u.Latest, tag)
			} else {
				fmt.Fprintf(&sb, "- **Updated %s** - Bumped from %s to %s. (%s)\n", u.Path, u.Current, u.Latest, tag)
			}
		}
	}
	return sb.String()
}

// writeFragment writes content to path, returning a wrapped error on failure.
func writeFragment(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("depupdate: writing changelog fragment %s: %w", filepath.Base(path), err)
	}
	return nil
}
