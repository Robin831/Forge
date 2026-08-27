package depcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Robin831/Forge/internal/gitfail"
)

// gitFailureKind, and the pattern sets behind it, live in internal/gitfail.
// They are a claim about git's behaviour rather than about depcheck's — a
// pattern set saying "a detached HEAD never fixes itself" is equally true of a
// self-deploy's pull, which classifies its own failures through the same tables
// — so they are one definition with aliases here rather than a copy per caller
// that stops agreeing with the other the next time git's wording moves.
type gitFailureKind = gitfail.Kind

const (
	gitFailureUnknown   = gitfail.Unknown
	gitFailureTransient = gitfail.Transient
	gitFailureBlocked   = gitfail.Blocked
)

// blockedPatterns is the shared table, named here because the invariant that
// every causePatterns entry is also a blocked one is checked against it.
var blockedPatterns = gitfail.BlockedPatterns

// classifyGitFailure decides how a failed git command should be reported.
//
// It is gitfail.Classify plus the one condition only depcheck can raise:
// resolveUpstream reports a checkout with no upstream and no named branch as
// ErrNoUpstream rather than as git text, and a checkout with no branch is a
// state, not an outage — no number of retries gives HEAD a name.
//
// Only the blocked/not-blocked split is acted on today: reportScanFailure is
// the sole consumer and branches on kind == gitFailureBlocked, so transient and
// unknown take the identical path (a log line, a depcheck_failed event, a retry
// next run). The two are still distinguished because they are different claims
// — transient means the message was RECOGNISED as a passing condition, unknown
// means nothing has modelled it — and it is the second that says the pattern
// sets need a new entry. Anything that starts branching on them must treat
// unknown as transient, never as its own class.
func classifyGitFailure(stderr string, err error) gitFailureKind {
	if kind := gitfail.Classify(stderr, err); kind != gitfail.Unknown {
		return kind
	}
	if errors.Is(err, ErrNoUpstream) {
		return gitfail.Blocked
	}
	return gitfail.Unknown
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
