package gitfail

import (
	"context"
	"path/filepath"
	"strings"
)

// OriginURL reads a checkout's `origin` URL, or "" when it has none.
//
// It is read from the repository's own config FILE rather than by resolving
// `origin` the way a fetch does, because the answer is wanted precisely when
// the checkout is in a state that makes a normal git command fail. A failure is
// returned but should never be fatal: the diagnosis is worth more with the URL
// in it and is still worth sending without one.
func OriginURL(ctx context.Context, repoDir string, run RunGitFunc) (string, error) {
	if repoDir == "" || run == nil {
		return "", nil
	}
	out, err := run(ctx, repoDir, "config", "--get", "remote.origin.url")
	if err != nil {
		// `git config --get` exits 1 for a key that is simply unset, which is
		// an answer and not a failure. The caller cannot tell those apart from
		// the error alone, so an unset key comes back as "" either way.
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SelfReferentialRemote reports whether a remote URL names a path inside
// repoPath — the repository's own directory, which for a Forge anvil is where
// its `.workers/<bead>/` worktrees live.
//
// A repository is never its own upstream. That is what makes this decidable
// without knowing the correct URL, which Forge does not: an anvil's config
// carries a path, not a repo, and only the chart's bootstrap knows the address
// to heal to. So the invariant is stated as the thing that is always wrong
// rather than as the set of things that are right.
//
// The case it was written for: an anvil's origin found pointing at
// `<anvil>/.workers/<bead>/.git`, a path inside a worker's worktree that was
// torn down when the worker finished. Every fetch afterwards failed with
// "does not appear to be a git repository", which reads like a remote outage
// and is not one — nothing that repeats retrying gets past a remote that is a
// deleted directory.
//
// Only a LOCAL path can be self-referential, so anything carrying a transport
// is not this condition: an `https://`/`ssh://`/`git://` URL, or git's scp-like
// `user@host:path` form. `file://` is unwrapped, since it names a local path
// with a scheme on it.
func SelfReferentialRemote(url, repoPath string) bool {
	path, ok := localRemotePath(url)
	if !ok || repoPath == "" {
		return false
	}

	base, err := filepath.Abs(repoPath)
	if err != nil {
		return false
	}
	// A relative remote path resolves against the REPOSITORY, not against this
	// process's working directory: that is what git does for a command run in
	// the repository, and every git command Forge runs sets cmd.Dir to it.
	// Resolved against the daemon's cwd instead, an origin of
	// `.workers/<bead>/.git` — exactly what a shell inside the worktree would
	// write — lands somewhere unrelated and reads as healthy.
	if !rooted(path) {
		path = filepath.Join(base, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(base, abs)
	if err != nil {
		// Different volumes on Windows: not inside, whatever else it is.
		return false
	}
	// Rel returns "." for the directory itself and a path whose first SEGMENT is
	// ".." for anything outside it. The segment matters: a bare HasPrefix("..")
	// also matches a directory named `..foo`, which is inside. Both the
	// repository itself and its `.git` are inside by this test, and both are
	// equally invalid as an upstream.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rooted reports whether a path starts from a root rather than from a working
// directory, and is deliberately wider than filepath.IsAbs.
//
// A remote URL is data, not a path this host produced: an anvil's config is
// written in the pod and may be read anywhere, and `/home/forge/anvils/x` is
// absolute on Linux but merely rooted on Windows, where IsAbs wants a drive
// letter. Joined onto the repository because IsAbs said no, an unrelated
// absolute path would be reported as inside the anvil — the one direction this
// must never get wrong.
func rooted(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return p != "" && (p[0] == '/' || p[0] == '\\')
}

// localRemotePath extracts the filesystem path a remote URL names, and reports
// whether the URL names one at all.
func localRemotePath(url string) (string, bool) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", false
	}
	if rest, ok := strings.CutPrefix(url, "file://"); ok {
		return filepath.FromSlash(rest), true
	}
	// Checked before the colon rule, which would otherwise read "C:" as git's
	// scp-like separator — which is exactly what it IS on a host with no
	// volumes, and why this branch is platform-gated.
	if hasDriveLetter(url) {
		return url, true
	}
	if colonBeforeSeparator(url) {
		// A transport URL, or git's scp-like user@host:path. Neither names a
		// path on this machine.
		return "", false
	}
	return filepath.FromSlash(url), true
}

// hasDriveLetter reports whether url opens with a volume root ("C:\" or "C:/"),
// and only on a host whose filesystem HAS volumes.
//
// The platform gate is a real fix rather than a tidy-up. Accepted as a local
// path on Linux, `C:\anvils\munin\.git` is not ROOTED there — IsAbs wants a
// leading slash — so it was joined onto the anvil and came back as a single
// element inside it. Every drive path in the world then read as this anvil's
// own worker worktree: the false positive that sends an operator to repoint a
// remote which is fine. On a host with no volumes git reads that same string as
// scp-like host "C", which is the answer colonBeforeSeparator now gives.
func hasDriveLetter(url string) bool {
	if filepath.Separator != '\\' {
		return false
	}
	return len(url) > 2 && isASCIILetter(url[0]) && url[1] == ':' &&
		(url[2] == '\\' || url[2] == '/')
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// colonBeforeSeparator is git's own test for the scp-like host:path form: a
// colon reached before any path separator.
//
// It replaces a bare strings.Contains(url, ":"), which is wider than git. A
// colon is a legal character in a path segment, so `/srv/a:b/repo.git` is a
// local path git resolves as one — dismissed as a transport, a genuinely
// self-referential remote under such a directory would go unreported.
func colonBeforeSeparator(url string) bool {
	for i := 0; i < len(url); i++ {
		switch c := url[i]; {
		case c == '/' || c == filepath.Separator:
			return false
		case c == ':':
			return true
		}
	}
	return false
}
