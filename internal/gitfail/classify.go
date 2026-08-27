// Package gitfail is the one classification and description of a git command
// that failed in a checkout Forge did not author.
//
// Two callers need the same three answers about such a failure, for opposite
// reasons. depcheck asks them about a scheduled dependency scan, which it must
// not report identically every night; selfdeploy asks them about the pull that
// precedes a rebuild, which it must not defer quietly every cycle. The answers
// are:
//
//   - Kind — will a later run get past this on its own (transient), or does it
//     reproduce identically until an operator changes something (blocked)?
//   - Cause — what specifically is holding it up, which is what decides which
//     command to put in front of the operator.
//   - The blocking paths — enumerated, annotated, bounded and sanitized, so a
//     message names the files rather than describing them.
//
// It is one package rather than a copy per caller because every part of it is
// a claim about git's behaviour, not about the caller's: a pattern set that
// says "detached HEAD never fixes itself" is equally true of a dependency scan
// and a self-deploy, and a second copy is one that stops agreeing with the
// first the next time git's wording moves.
//
// Everything here is sound only for git invoked with LC_ALL=C. git's
// diagnostics are gettext-translated and every pattern below is English, so a
// caller that lets the host's locale through gets Unknown for conditions this
// package models precisely.
package gitfail

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
)

// Kind separates the two things a failed git command can mean, which need
// opposite responses.
//
// A TRANSIENT failure will very likely succeed on its own: the DNS server was
// down, the remote refused the connection, the fetch timed out. The right
// response is to log it and try again on the next run.
//
// A BLOCKED failure will not. A detached HEAD has no upstream to fetch, a tree
// left mid-merge stays unmerged, a ref the fetch cannot lock stays locked. Every
// run reproduces it exactly, which is what makes the naive response useless:
// the same failure line every night is indistinguishable from noise, and the
// work silently stops happening for weeks. A blocked failure is escalated ONCE
// and then suppressed until the condition changes.
//
// Unknown is the zero value and must be treated as transient at every call
// site. That is the fail-safe direction: an unrecognised message keeps the
// noisy-but-honest behaviour rather than raising a needs-attention entry nobody
// can act on and then muting the only signal that anything is wrong.
type Kind int

const (
	Unknown Kind = iota
	Transient
	Blocked
)

func (k Kind) String() string {
	switch k {
	case Transient:
		return "transient"
	case Blocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// BlockedPatterns are git's own words for a condition that repeats until an
// operator changes something. Matching git's TEXT is unavoidable here — git
// reports every one of these with a bare non-zero exit and nothing else — and
// it is only sound because the caller pins LC_ALL=C, so these messages are the
// English ones on every host. Anything a locale could still reshape lands in
// Unknown, which is treated as transient.
//
// Matched lowercased, as substrings, so the surrounding path or ref name in
// git's line does not have to be modelled.
var BlockedPatterns = []string{
	// A pull or a stash pop refused by, or leaving behind, local work.
	"local changes to the following files would be overwritten",
	"would be overwritten by merge",
	"would be overwritten by checkout",
	"please commit your changes or stash them",
	// A working tree left mid-merge.
	"you have unmerged paths",
	"fix conflicts and run",
	"unmerged files",
	"needs merge",
	// The same tree once the conflicts have been staged but not committed:
	// the index is back at stage 0 and only the sequencer state says the
	// operation is unfinished. git covers merge, cherry-pick and revert with
	// one sentence ("You have not concluded your merge (MERGE_HEAD exists)"),
	// so one substring covers all three.
	"you have not concluded your",
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

// TransientPatterns are the failures a later run has a real chance of getting
// past: name resolution, connectivity, TLS, and the credential-shaped refusals
// that are indistinguishable from an expired token.
//
// "repository not found" is here rather than in BlockedPatterns on purpose:
// GitHub returns it for a private repository the caller is not authenticated
// for, which is far more often an expired credential than a deleted remote.
var TransientPatterns = []string{
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

// Classify decides how a failed git command should be reported.
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
// next successful run withdraws.
func Classify(stderr string, err error) Kind {
	text := strings.ToLower(stderr)
	if err != nil {
		// The error text carries what the caller's git runner folded into it,
		// which for a caller that only has the wrapped error is the whole of
		// the evidence.
		text += "\n" + strings.ToLower(err.Error())
	}

	for _, p := range BlockedPatterns {
		if strings.Contains(text, p) {
			return Blocked
		}
	}
	for _, p := range TransientPatterns {
		if strings.Contains(text, p) {
			return Transient
		}
	}
	if IsTimeout(err) {
		return Transient
	}
	return Unknown
}

// IsTimeout reports whether err is a deadline or a network timeout — the
// failures that produce little or no git output because the command was killed
// or the connection never completed.
//
// It reaches the deadline case only for a caller that attaches the expired
// context's error to the one exec reports: a command killed by a deadline comes
// back as a bare *exec.ExitError ("signal: killed"), which matches no pattern
// above and would otherwise be classified Unknown.
func IsTimeout(err error) bool {
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
