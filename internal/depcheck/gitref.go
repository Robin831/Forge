package depcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// ErrBlobNotFound is returned by showBlob when the path does not exist at the
// requested ref. It is a distinct error rather than an empty result because
// "this repository has no go.mod" and "git could not be run" are opposite
// answers: the first skips an ecosystem, the second must abandon the scan.
var ErrBlobNotFound = errors.New("path not found at ref")

// ErrNoUpstream is returned by resolveUpstream when the checkout has neither a
// configured upstream nor a named branch to fall back on (a detached HEAD).
var ErrNoUpstream = errors.New("no upstream tracking ref")

// defaultRemote is the remote assumed when a branch has no configured upstream.
const defaultRemote = "origin"

// gitTimeout bounds each individual git invocation. Only the fetch talks to the
// network; the rest are local object reads.
const gitTimeout = 30 * time.Second

// upstream names the remote branch an anvil's checkout tracks, and the
// remote-tracking ref its contents are read from.
type upstream struct {
	Remote string // e.g. "origin"
	Branch string // e.g. "main" — the branch name ON the remote
	Ref    string // e.g. "origin/main" — the local remote-tracking ref
}

// runGit executes one git command in repoDir and returns its stdout. Errors
// carry the combined output so the caller (and the failure classifier that
// reads these messages) sees git's own words rather than "exit status 1".
func runGit(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = repoDir
	// An ambient GIT_DIR would answer for a repository other than repoDir.
	cmd.Env = executil.CleanGitEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return stdout.Bytes(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return stdout.Bytes(), nil
}

// resolveUpstream reports the remote branch this checkout tracks. The upstream
// is read from the branch's own configuration rather than assumed to be
// origin/main: an anvil may track a release branch, or a fork.
//
// A branch with no configured upstream falls back to the same branch name on
// origin, which is the overwhelmingly common arrangement for a checkout that
// was simply never `--set-upstream`'d. A detached HEAD has no branch to fall
// back on and returns ErrNoUpstream.
func resolveUpstream(ctx context.Context, repoDir string) (upstream, error) {
	out, err := runGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err == nil {
		if up, ok := parseUpstreamRef(string(out)); ok {
			return up, nil
		}
	}

	// No upstream configured (or an unparseable one) — fall back to the
	// current branch name on the default remote.
	branchOut, branchErr := runGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if branchErr != nil {
		return upstream{}, fmt.Errorf("resolving upstream for %s: %w", repoDir, branchErr)
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "" || branch == "HEAD" {
		return upstream{}, fmt.Errorf("resolving upstream for %s: %w", repoDir, ErrNoUpstream)
	}
	return upstream{
		Remote: defaultRemote,
		Branch: branch,
		Ref:    defaultRemote + "/" + branch,
	}, nil
}

// parseUpstreamRef splits an `@{upstream}` answer ("origin/main",
// "origin/release/2.x") into its remote and branch halves. The remote is the
// first segment; everything after it is the branch, since branch names may
// themselves contain slashes.
func parseUpstreamRef(raw string) (upstream, bool) {
	ref := strings.TrimSpace(raw)
	remote, branch, ok := strings.Cut(ref, "/")
	if !ok || remote == "" || branch == "" {
		return upstream{}, false
	}
	return upstream{Remote: remote, Branch: branch, Ref: ref}, true
}

// fetchUpstream updates the remote-tracking ref for up and returns the ref the
// scan should read blobs from.
//
// A fetch writes only to .git — it can neither be refused by local
// modifications nor leave the checkout half-merged, which is the whole reason
// depcheck fetches instead of pulling. The tracked files an anvil must keep
// permanently modified (a pod-local `.beads/config.yaml`, bd's own additions to
// `.beads/.gitignore`) are untouched and stay that way.
//
// The ref returned is the remote-tracking ref when there is one, and FETCH_HEAD
// otherwise — a remote configured without the standard fetch refspec writes only
// FETCH_HEAD, and demanding a tracking ref there would fail a scan the fetch
// itself answered fine. The tracking ref is preferred rather than always reading
// FETCH_HEAD because FETCH_HEAD is one file per repository: an anvil's own
// worker fetching concurrently overwrites it, while `origin/<branch>` is
// per-branch and cannot be rewritten out from under this scan.
//
// What refExists establishes is only that the ref RESOLVES to a commit, not that
// this fetch is what moved it. A tracking ref left behind by an earlier fetch of
// a remote whose refspec no longer writes it would be read at the commit it last
// held. Proving otherwise needs the ref's pre-fetch value compared against
// FETCH_HEAD, which is deliberately not done: the arrangement is rare and its
// worst outcome is a scan of a slightly older commit of the right branch, which
// reconciliation then treats exactly as it treats any other committed state.
func fetchUpstream(ctx context.Context, repoDir string, up upstream) (string, error) {
	if _, err := runGit(ctx, repoDir, "fetch", up.Remote, up.Branch); err != nil {
		return "", err
	}
	if refExists(ctx, repoDir, up.Ref) {
		return up.Ref, nil
	}
	if refExists(ctx, repoDir, "FETCH_HEAD") {
		return "FETCH_HEAD", nil
	}
	return "", fmt.Errorf("fetched %s %s but neither %s nor FETCH_HEAD resolves to a commit",
		up.Remote, up.Branch, up.Ref)
}

// refExists reports whether ref names a commit in repoDir.
func refExists(ctx context.Context, repoDir, ref string) bool {
	_, err := runGit(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// showBlob reads one repo-relative path out of a ref without touching the
// working tree. A path absent from that ref returns ErrBlobNotFound, which
// callers use exactly as they used os.IsNotExist.
func showBlob(ctx context.Context, repoDir, ref, path string) ([]byte, error) {
	// git show takes repo-root-relative, forward-slashed paths regardless of
	// the host's separator.
	spec := ref + ":" + strings.ReplaceAll(path, "\\", "/")
	out, err := runGit(ctx, repoDir, "show", spec)
	if err != nil {
		if isPathNotInRef(err) {
			return nil, fmt.Errorf("%s@%s: %w", path, ref, ErrBlobNotFound)
		}
		return nil, err
	}
	return out, nil
}

// notInRefMarkers are the phrases git uses to say "that PATH is not in this
// ref": `fatal: path 'x' does not exist in 'origin/main'`, and the variant it
// uses when the path is present in the checkout but untracked.
//
// The match is on text because git reports every `git show` failure as exit
// 128. Only path-level wording belongs here: an unknown ref is
// `fatal: invalid object name 'origin/nope'`, and reading that as
// ErrBlobNotFound would report a repository whose ref could not be resolved as
// one that simply has no go.mod — a silent "nothing to update" in place of a
// failure.
var notInRefMarkers = []string{
	"exists on disk, but not in",
	"does not exist in",
}

func isPathNotInRef(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range notInRefMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// listTreePaths returns every path tracked at ref, repo-root-relative and
// forward-slashed. Discovery from the tree rather than the filesystem is what
// keeps untracked directories — .workers, .worktrees, .previews, node_modules,
// bin/obj — out of the scan without a skip list to keep in step with them.
func listTreePaths(ctx context.Context, repoDir, ref string) ([]string, error) {
	out, err := runGit(ctx, repoDir, "ls-tree", "-r", "--name-only", "-z", ref)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		paths = append(paths, string(p))
	}
	return paths, nil
}
