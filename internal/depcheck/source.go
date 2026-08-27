package depcheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// manifestSource supplies the repository's tracked manifest files to the
// ecosystem scanners. There are two: the working tree as it stands, and a git
// ref read through `git ls-tree`/`git show`.
//
// The scheduled scan uses the ref, so that an anvil is never required to have a
// clean, fast-forwardable working tree in order to be scanned — depcheck's
// answer depends on what is committed upstream, not on what happens to be
// checked out. Paths are repo-root-relative and forward-slashed in both
// implementations, so a caller cannot tell them apart by shape.
type manifestSource interface {
	// Root is the working-tree directory the ecosystem tools run in.
	Root() string
	// Paths lists every candidate manifest file, repo-relative.
	Paths(ctx context.Context) ([]string, error)
	// Read returns one file's contents, or a wrapped ErrBlobNotFound.
	Read(ctx context.Context, path string) ([]byte, error)
	// Describe names the source for log lines.
	Describe() string
}

// worktreeSource reads the checkout as it stands on disk.
type worktreeSource struct{ root string }

func (w worktreeSource) Root() string { return w.root }

func (w worktreeSource) Describe() string { return "working tree" }

func (w worktreeSource) Paths(context.Context) ([]string, error) {
	return walkWorktreePaths(w.root), nil
}

func (w worktreeSource) Read(_ context.Context, path string) ([]byte, error) {
	full := filepath.Join(w.root, filepath.FromSlash(path))
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", path, ErrBlobNotFound)
		}
		return nil, err
	}
	return data, nil
}

// refSource reads the blobs of a git ref. Nothing it does touches the working
// tree, so an anvil carrying permanent local modifications scans exactly like a
// clean one and keeps those modifications afterwards.
type refSource struct {
	repoDir string
	ref     string
}

func (r refSource) Root() string { return r.repoDir }

func (r refSource) Describe() string { return r.ref }

func (r refSource) Paths(ctx context.Context) ([]string, error) {
	return listTreePaths(ctx, r.repoDir, r.ref)
}

func (r refSource) Read(ctx context.Context, path string) ([]byte, error) {
	return showBlob(ctx, r.repoDir, r.ref, path)
}

// skippedWalkDirs are the directories a checkout carries but git does not
// track. They matter only to worktreeSource: a ref listing never contains them.
//
// .previews holds Kiln preview checkouts, whose node_modules are junctions into
// the main checkout — discovering a project there means running `npm ci`
// through the junction, which deletes the main checkout's node_modules out from
// under every worktree linked to it (observed 2026-08-07).
var skippedWalkDirs = map[string]bool{
	"node_modules": true,
	".workers":     true,
	".worktrees":   true,
	".previews":    true,
	"bin":          true,
	"obj":          true,
	".git":         true,
}

// walkWorktreePaths returns every file under root as a repo-relative,
// forward-slashed path, skipping the untracked directories above.
func walkWorktreePaths(root string) []string {
	var paths []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skippedWalkDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	return paths
}

// localDir maps a repo-relative path's directory onto the working tree.
func localDir(root, relPath string) string {
	return filepath.Join(root, filepath.FromSlash(pathDir(relPath)))
}

// pathDir is filepath.Dir for the forward-slashed, repo-relative paths this
// package passes around; it returns "" rather than "." for a root-level file.
func pathDir(relPath string) string {
	idx := strings.LastIndex(relPath, "/")
	if idx < 0 {
		return ""
	}
	return relPath[:idx]
}
