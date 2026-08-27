package depcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

// gitFailureKind separates the two things a failed git command can mean for a
// scheduled scan, which need opposite responses.
//
// A TRANSIENT failure will very likely succeed on its own: the DNS server was
// down, the remote refused the connection, the fetch timed out. The right
// response is the one depcheck already had — log it, write depcheck_failed,
// and try again on the next run.
//
// A BLOCKED failure will not. A detached HEAD has no upstream to fetch, a
// remote that does not resolve does not start resolving, a ref the fetch cannot
// lock stays locked. Every run reproduces it exactly, which is what made the
// old behaviour useless: the same depcheck_failed line every night is
// indistinguishable from noise, and the anvil silently stopped being scanned
// for weeks. A blocked failure is escalated ONCE and then suppressed until the
// condition changes.
//
// gitFailureUnknown is the zero value and is treated as transient at every call
// site. That is the fail-safe direction: an unrecognised message keeps the old
// behaviour (a nightly event) rather than raising a needs-attention entry
// nobody can act on and then muting the only signal that anything is wrong.
type gitFailureKind int

const (
	gitFailureUnknown gitFailureKind = iota
	gitFailureTransient
	gitFailureBlocked
)

func (k gitFailureKind) String() string {
	switch k {
	case gitFailureTransient:
		return "transient"
	case gitFailureBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// blockedPatterns are git's own words for a condition that repeats until an
// operator changes something. Matching git's TEXT is unavoidable here — unlike
// blobExists, which distinguishes "no such object" from "git failed" by exit
// code, git reports every one of these with a bare non-zero exit — and it is
// only sound because runGit pins LC_ALL=C, so these messages are the English
// ones on every host. Anything a locale could still reshape lands in
// gitFailureUnknown, which is treated as transient.
//
// Matched lowercased, as substrings, so the surrounding path or ref name in
// git's line does not have to be modelled.
var blockedPatterns = []string{
	// A pull's refusal. depcheck no longer pulls, but quench, burnish and an
	// operator's own `git pull` in the anvil all produce it, and the classifier
	// is the package's answer to "is this worth escalating" rather than to
	// "which command produced it".
	"local changes to the following files would be overwritten",
	"would be overwritten by merge",
	"would be overwritten by checkout",
	"please commit your changes or stash them",
	// A working tree left mid-merge.
	"you have unmerged paths",
	"fix conflicts and run",
	"unmerged files",
	"needs merge",
	// A rebase/pull refused by the tree's own state.
	"cannot pull with rebase",
	"you have unstaged changes",
	"your index contains uncommitted changes",
	// No branch to resolve an upstream from, or one that cannot be reconciled.
	"you are not currently on a branch",
	"detached head",
	"have diverged",
	// A ref this fetch cannot write. Left alone these persist: a stale lock
	// file is not cleaned up by trying again, and a ref/directory collision
	// (refs/heads/foo vs refs/heads/foo/bar) needs the remote fixed.
	"cannot lock ref",
	"unable to update local ref",
	".lock': file exists",
	"another git process seems to be running",
	"refusing to fetch into branch",
	// The checkout itself is not usable.
	"not a git repository",
}

// transientPatterns are the failures a later run has a real chance of getting
// past: name resolution, connectivity, TLS, and the credential-shaped refusals
// that are indistinguishable from an expired token.
//
// "repository not found" is here rather than in blockedPatterns on purpose:
// GitHub returns it for a private repository the caller is not authenticated
// for, which is far more often an expired credential than a deleted remote.
var transientPatterns = []string{
	"could not resolve host",
	"temporary failure in name resolution",
	"name or service not known",
	"connection timed out",
	"operation timed out",
	"timed out",
	"connection refused",
	"connection reset",
	"network is unreachable",
	"no route to host",
	"unable to access",
	"failed to connect",
	"the remote end hung up unexpectedly",
	"early eof",
	"rpc failed",
	"ssh: connect to host",
	"authentication failed",
	"permission denied (publickey)",
	"could not read from remote repository",
	"repository not found",
	"gnutls_handshake failed",
	"ssl_read",
	"server certificate verification failed",
	"remote end hung up",
	"service unavailable",
	"internal server error",
}

// classifyGitFailure decides how a failed git command should be reported.
//
// stderr is git's own output for the command, err the error the run returned.
// Both are consulted because they answer for different failure modes: git's
// message is the only evidence for a refusal it exits non-zero on, while a
// deadline or a network error that never reached git is visible only in err.
//
// Blocked patterns are tested FIRST. The two sets are not disjoint in practice
// — a fetch that cannot lock a ref may also mention the remote it was fetching
// from — and misreading a blocked condition as transient restores the nightly
// noise this exists to remove, while the reverse only raises an entry that the
// next successful scan withdraws.
func classifyGitFailure(stderr string, err error) gitFailureKind {
	text := strings.ToLower(stderr)
	if err != nil {
		// The error text carries what runGit folded into it, which for a
		// caller that only has the wrapped error is the whole of the evidence.
		text += "\n" + strings.ToLower(err.Error())
	}

	for _, p := range blockedPatterns {
		if strings.Contains(text, p) {
			return gitFailureBlocked
		}
	}
	// A checkout with no branch and no upstream is a state, not an outage: no
	// number of retries gives HEAD a name. It is raised as a sentinel rather
	// than as git text, so it is tested as one.
	if errors.Is(err, ErrNoUpstream) {
		return gitFailureBlocked
	}

	for _, p := range transientPatterns {
		if strings.Contains(text, p) {
			return gitFailureTransient
		}
	}
	if isTimeoutError(err) {
		return gitFailureTransient
	}
	return gitFailureUnknown
}

// classifyGitError is classifyGitFailure for a caller holding only the error.
// It recovers git's stderr from the error when runGit produced it, so the
// classification reads git's own words rather than the sentence they were
// wrapped in.
func classifyGitError(err error) gitFailureKind {
	if err == nil {
		return gitFailureUnknown
	}
	return classifyGitFailure(gitStderr(err), err)
}

// isTimeoutError reports whether err is a deadline or a network timeout — the
// failures that never produce git output because the command was killed or the
// connection never completed.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// Normalisation patterns for the failure signature. Each removes something that
// varies between two runs of the SAME condition: absolute paths differ between
// hosts (and the daemon's log is read on more than one), object names change on
// every push, and a timestamp changes on every run by definition. Left in, each
// would make every run a "new" condition and re-escalate nightly — which is the
// bug this signature exists to prevent, arriving through the normaliser.
// A path is recognised only where one can START — at the beginning, after
// whitespace, or after an opening quote or bracket, which is how git presents
// them. Requiring that boundary is what keeps a REF out of the match:
// `refs/remotes/origin/main` has no separator before any of its slashes, so two
// anvils blocked on two different refs stay two conditions.
var (
	sigAbsPath   = regexp.MustCompile(`(^|[\s'"(\[])(?:[A-Za-z]:)?[\\/](?:[\w.\-]+[\\/])+[\w.\-]*`)
	sigHexToken  = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	sigTimestamp = regexp.MustCompile(`(?i)\b\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(?:\.\d+)?z?|\d{2}:\d{2}:\d{2}`)
	sigSpace     = regexp.MustCompile(`\s+`)
)

// normalizeFailureText reduces a git failure to the part of it that identifies
// the CONDITION, so two runs blocked by the same thing produce the same string
// and two runs blocked by different things do not.
func normalizeFailureText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = sigTimestamp.ReplaceAllString(s, "<time>")
	s = sigHexToken.ReplaceAllString(s, "<sha>")
	s = sigAbsPath.ReplaceAllString(s, "${1}<path>")
	s = sigSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// gitFailureSignature is the value two runs are compared on to decide whether a
// blocked condition has already been escalated.
//
// The anvil and the kind are folded in alongside the normalised text so a
// signature cannot be carried across anvils, and so the same message arriving
// under a different classification counts as a different condition.
//
// It is a digest rather than the text itself because it is compared, never
// read: an operator reads the Detail, which keeps git's words intact.
func gitFailureSignature(anvil string, kind gitFailureKind, detail string) string {
	h := sha256.New()
	// Length-prefixed so no shuffling of the field boundaries can produce a
	// collision between two different (anvil, kind, detail) triples.
	for _, field := range []string{anvil, kind.String(), normalizeFailureText(detail)} {
		fmt.Fprintf(h, "%d:%s", len(field), field)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
