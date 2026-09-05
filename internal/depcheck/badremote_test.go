package depcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// badRemoteEvidence is git's answer for a remote that is a path with no
// repository at it — both lines, because git prints both and the second one is
// a transient pattern that used to decide the classification.
const badRemoteEvidence = "fatal: '/srv/anvils/heimdall/.workers/bd-42/.git' does not appear to be a git repository " +
	"fatal: Could not read from remote repository."

// TestBadRemoteIsBlocked is the classification the 24-hour silence turned on.
// depcheck escalates blocked failures and stays quiet about transient ones, and
// this message used to land on the quiet side — so an anvil that could not be
// scanned at all retried every ten minutes for a day, raising nothing.
func TestBadRemoteIsBlocked(t *testing.T) {
	assert.Equal(t, gitFailureBlocked, classifyGitFailure(badRemoteEvidence, nil))
}

// TestBlockedMessage_SelfReferentialOriginNamesTheInvariant: the value of the
// escalation is that it says what "could not read from remote repository" never
// did — which path origin holds, and that a path inside the anvil is never a
// valid upstream.
func TestBlockedMessage_SelfReferentialOriginNamesTheInvariant(t *testing.T) {
	origin := "/srv/anvils/heimdall/.workers/bd-42/.git"
	msg := blockedMessage("heimdall", "/srv/anvils/heimdall", nil, badRemoteEvidence, origin)

	assert.Contains(t, msg, origin, "the operator cannot repoint a remote the message does not name")
	assert.Contains(t, msg, "never its own upstream")
	assert.Contains(t, msg, "remote set-url origin")
	assert.Contains(t, msg, "Forge never writes this value",
		"the message has to send the reader after the writer, since Forge is not it")
}

// A remote that is broken but NOT inside the anvil gets the same remedy without
// the invariant sentence, which would be a claim the message cannot support.
func TestBlockedMessage_BadRemoteElsewhere(t *testing.T) {
	msg := blockedMessage("heimdall", "/srv/anvils/heimdall", nil,
		"fatal: '/srv/mirrors/gone.git' does not appear to be a git repository", "/srv/mirrors/gone.git")

	assert.Contains(t, msg, "/srv/mirrors/gone.git")
	assert.Contains(t, msg, "remote set-url origin")
	assert.NotContains(t, msg, "never its own upstream")
}

// The origin read is best-effort, so the message still has to be worth sending
// when it came back empty.
func TestBlockedMessage_BadRemoteWithNoURLRead(t *testing.T) {
	msg := blockedMessage("heimdall", "/srv/anvils/heimdall", nil, badRemoteEvidence, "")

	assert.Contains(t, msg, "remote -v")
	assert.Contains(t, msg, "remote set-url origin")
}

// A bad remote is not a dirty tree: a working-tree path list here would send the
// operator after files that are not the problem.
func TestBlockedMessage_BadRemoteListsNoPaths(t *testing.T) {
	msg := blockedMessage("heimdall", "/srv/anvils/heimdall",
		[]string{"internal/foo.go"}, badRemoteEvidence, "/srv/anvils/heimdall/.workers/bd-42/.git")

	assert.NotContains(t, msg, "Blocking paths")
	assert.NotContains(t, msg, "internal/foo.go")
}
