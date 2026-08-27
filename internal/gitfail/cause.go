package gitfail

import "strings"

// Cause is what git's words say is holding the command up. It is derived from
// the SAME text Classify already read, but for a different question: that one
// decides whether to escalate at all, this one decides what to tell the
// operator to run. Deriving it separately rather than widening Kind keeps that
// separation — a new remediation is a change to a caller's message, and can
// never change whether a condition is escalated or suppressed.
type Cause int

const (
	// CauseUnknown is a blocked condition none of the patterns below model. It
	// is the zero value and gets a diagnostic remediation, never a guess.
	CauseUnknown Cause = iota
	CauseDirtyTree
	CauseUnmerged
	CauseDetachedHead
	CauseRefLock
	CauseNotARepo
)

// CausePatterns map git's own words onto a cause. They are matched lowercased,
// as substrings, in this order — the first match wins, so a message naming both
// an unmerged tree and the modifications it cannot overwrite is reported as the
// merge it is.
//
// Every pattern here is also a BlockedPatterns entry, because a cause is only
// ever asked for a failure already classified Blocked. The reverse does not
// hold: a blocked pattern with no specific remedy — "have diverged" — is
// deliberately left to CauseUnknown rather than given an invented command.
var CausePatterns = []struct {
	Pattern string
	Cause   Cause
}{
	{"you have unmerged paths", CauseUnmerged},
	{"fix conflicts and run", CauseUnmerged},
	{"unmerged files", CauseUnmerged},
	{"needs merge", CauseUnmerged},
	{"you have not concluded your", CauseUnmerged},

	{"local changes to the following files would be overwritten", CauseDirtyTree},
	{"would be overwritten by merge", CauseDirtyTree},
	{"would be overwritten by checkout", CauseDirtyTree},
	{"please commit your changes or stash them", CauseDirtyTree},
	{"you have unstaged changes", CauseDirtyTree},
	{"your index contains uncommitted changes", CauseDirtyTree},
	{"cannot pull with rebase", CauseDirtyTree},

	{"you are not currently on a branch", CauseDetachedHead},
	{"detached head", CauseDetachedHead},
	// The sentence a caller raises for a checkout with no upstream to resolve.
	// A detached HEAD reaches a report as that sentinel rather than as git text
	// when resolving the upstream is what failed, and the two are one condition
	// to an operator.
	{"no upstream tracking ref", CauseDetachedHead},

	{"cannot lock ref", CauseRefLock},
	{"unable to update local ref", CauseRefLock},
	{".lock': file exists", CauseRefLock},
	{"another git process seems to be running", CauseRefLock},
	{"refusing to fetch into branch", CauseRefLock},

	{"not a git repository", CauseNotARepo},
}

// CauseOf reads git's evidence for the specific condition. The argument is the
// already-sanitized evidence string rather than the error, so the cause is
// derived from exactly the text the operator is shown — a remedy naming a
// condition the message does not quote would be unverifiable.
func CauseOf(evidence string) Cause {
	text := strings.ToLower(evidence)
	for _, c := range CausePatterns {
		if strings.Contains(text, c.Pattern) {
			return c.Cause
		}
	}
	return CauseUnknown
}

// InvolvesWorkingTree reports whether the checkout's own modifications are what
// is blocking. Only then are they enumerated: a detached HEAD or a stale ref
// lock is unaffected by what the tree holds, and listing a pod-local
// `.beads/config.yaml` under "blocking paths" for one of those sends the
// operator after a file that is not the problem.
func (c Cause) InvolvesWorkingTree() bool {
	return c == CauseDirtyTree || c == CauseUnmerged || c == CauseUnknown
}
