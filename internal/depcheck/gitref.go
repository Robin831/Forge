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

// gitError is a failed git invocation: which command, what git said, and the
// underlying exec error.
//
// Its Error() renders exactly the sentence runGit used to build by hand, so
// every existing log line and event message is byte-identical. What it adds is
// Stderr as a FIELD: the failure classifier decides between a transient and a
// blocked condition by matching git's own words, and reading them back out of a
// formatted sentence means re-parsing a string this package wrote — the
// arrangement that breaks the first time a wrapper adds a prefix.
type gitError struct {
	Args   []string // the git arguments, without the leading "git"
	Stderr string   // git's own output, trimmed; stdout when stderr was empty
	Err    error    // the exec error (exit status, context deadline, ...)
}

func (e *gitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

// Unwrap exposes the exec error so errors.Is finds context.DeadlineExceeded and
// friends through the wrapper.
func (e *gitError) Unwrap() error { return e.Err }

// gitStderr returns git's own output for a failed command, or "" for an error
// that did not come from runGit. It is the one reader of gitError.Stderr, so a
// caller never has to know whether the error it holds is wrapped.
func gitStderr(err error) string {
	var ge *gitError
	if errors.As(err, &ge) {
		return ge.Stderr
	}
	return ""
}

// runGit executes one git command in repoDir and returns its stdout. A failure
// comes back as a *gitError carrying git's own output as a field, so the caller
// (and classifyGitFailure, which reads it) sees git's words rather than
// "exit status 1".
func runGit(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = repoDir
	// An ambient GIT_DIR would answer for a repository other than repoDir.
	// CleanGitEnv strips only the repo-location variables, so the host's locale
	// survives and git's diagnostics are gettext-translated: pin them to C so an
	// error this package logs or an event it writes reads the same on every host.
	// classifyGitFailure now BRANCHES on that text as well — git reports a
	// blocked tree with a bare non-zero exit and nothing else — so this pin is
	// what makes matching it sound, rather than only what keeps a German-locale
	// daemon's event log legible. Where an exit code can answer instead, it
	// still does: see blobExists.
	cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C", "LANG=C")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), &gitError{Args: args, Stderr: detail, Err: err}
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
		// The path is absent only if the REF resolves and the object under it
		// does not; anything else is git failing and must stay a failure.
		if refExists(ctx, repoDir, ref) && !blobExists(ctx, repoDir, spec) {
			return nil, fmt.Errorf("%s@%s: %w", path, ref, ErrBlobNotFound)
		}
		return nil, err
	}
	return out, nil
}

// blobExists reports whether spec ("<ref>:<path>") names an object in repoDir.
//
// It answers by EXIT CODE, which is the whole reason it exists. This is the most
// consequential distinction the source layer makes — "this repository has no
// go.mod" versus "git failed" — and it used to be decided by substring-matching
// git's `fatal:` text. Those diagnostics are gettext-translated and runGit
// inherits the host's locale, so on a `LANG=de_DE.UTF-8` host with git's message
// catalogs installed the match failed and every scheduled scan of a .NET-only or
// Node-only anvil logged a Go manifest-read failure and wrote a depcheck_failed
// event, forever, for an anvil that is simply not a Go project. `git cat-file -e`
// says the same thing in a number.
func blobExists(ctx context.Context, repoDir, spec string) bool {
	_, err := runGit(ctx, repoDir, "cat-file", "-e", spec)
	return err == nil
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
