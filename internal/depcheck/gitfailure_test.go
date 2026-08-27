package depcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyGitFailure pins the classification for the messages git actually
// emits. It is a table over git's own words rather than over categories,
// because the classifier's only input IS those words: a paraphrase that passes
// here while the real message does not is the failure mode.
func TestClassifyGitFailure(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		err    error
		want   gitFailureKind
	}{
		// --- blocked: repeats identically until a human intervenes ---
		{
			name:   "local changes would be overwritten",
			stderr: "error: Your local changes to the following files would be overwritten by merge:\n\t.beads/config.yaml\nPlease commit your changes or stash them before you merge.",
			want:   gitFailureBlocked,
		},
		{
			name:   "unmerged paths",
			stderr: "error: you have unmerged paths.\nhint: Fix them up in the work tree, and then use 'git add/rm <file>'",
			want:   gitFailureBlocked,
		},
		{
			name:   "fix conflicts and run",
			stderr: "hint: Fix conflicts and run \"git commit\".",
			want:   gitFailureBlocked,
		},
		{
			name:   "cannot pull with rebase",
			stderr: "error: cannot pull with rebase: You have unstaged changes.",
			want:   gitFailureBlocked,
		},
		{
			name:   "not currently on a branch",
			stderr: "fatal: You are not currently on a branch.",
			want:   gitFailureBlocked,
		},
		{
			name:   "diverged branches",
			stderr: "hint: You have divergent branches; your branch and 'origin/main' have diverged.",
			want:   gitFailureBlocked,
		},
		{
			name:   "cannot lock ref",
			stderr: "error: cannot lock ref 'refs/remotes/origin/main': is at 0123456789abcdef0123456789abcdef01234567 but expected 89abcdef",
			want:   gitFailureBlocked,
		},
		{
			name:   "stale index lock",
			stderr: "fatal: Unable to create '/srv/anvil/.git/index.lock': File exists.",
			want:   gitFailureBlocked,
		},
		{
			name:   "not a git repository",
			stderr: "fatal: not a git repository (or any of the parent directories): .git",
			want:   gitFailureBlocked,
		},
		{
			name: "no upstream sentinel with no git text at all",
			err:  fmt.Errorf("resolving upstream for /srv/anvil: %w", ErrNoUpstream),
			want: gitFailureBlocked,
		},

		// --- transient: a later run has a real chance ---
		{
			name:   "dns failure",
			stderr: "ssh: Could not resolve hostname github.com: Temporary failure in name resolution",
			want:   gitFailureTransient,
		},
		{
			name:   "connection timed out",
			stderr: "ssh: connect to host github.com port 22: Connection timed out",
			want:   gitFailureTransient,
		},
		{
			name:   "connection refused",
			stderr: "fatal: unable to access 'https://github.com/org/repo.git/': Failed to connect to github.com port 443: Connection refused",
			want:   gitFailureTransient,
		},
		{
			name:   "auth failure",
			stderr: "remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/org/repo.git/'",
			want:   gitFailureTransient,
		},
		{
			name:   "publickey denied",
			stderr: "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.",
			want:   gitFailureTransient,
		},
		{
			name:   "private repo reads as an auth failure, not a deleted remote",
			stderr: "remote: Repository not found.\nfatal: repository 'https://github.com/org/repo.git/' not found",
			want:   gitFailureTransient,
		},
		{
			name:   "remote hung up",
			stderr: "fatal: The remote end hung up unexpectedly",
			want:   gitFailureTransient,
		},
		{
			name: "killed by the command deadline, no git output",
			err:  fmt.Errorf("git fetch origin main: %w", context.DeadlineExceeded),
			want: gitFailureTransient,
		},
		{
			name: "net timeout",
			err:  fmt.Errorf("dialing: %w", timeoutError{}),
			want: gitFailureTransient,
		},

		// --- unknown: kept as the old nightly-event behaviour ---
		{
			name:   "unrecognised message",
			stderr: "error: something nobody has modelled yet",
			err:    errors.New("exit status 128"),
			want:   gitFailureUnknown,
		},
		{
			name: "no evidence at all",
			want: gitFailureUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyGitFailure(tc.stderr, tc.err),
				"stderr: %q", tc.stderr)
		})
	}
}

// TestClassifyGitFailureBlockedWinsOverTransient pins the ordering. A fetch
// that cannot lock a ref names the remote it was fetching from in the same
// breath, so the two pattern sets overlap in practice — and reading such a
// failure as transient restores the nightly noise the escalation exists to end.
func TestClassifyGitFailureBlockedWinsOverTransient(t *testing.T) {
	stderr := "fatal: unable to access 'https://github.com/org/repo.git/': error: cannot lock ref 'refs/remotes/origin/main'"
	assert.Equal(t, gitFailureBlocked, classifyGitFailure(stderr, nil))
}

// TestClassifyGitErrorReadsStderrOffTheError is why gitError carries stderr as
// a field: a caller several wraps away from runGit must still classify on git's
// own words.
func TestClassifyGitErrorReadsStderrOffTheError(t *testing.T) {
	inner := &gitError{
		Args:   []string{"fetch", "origin", "main"},
		Stderr: "fatal: You are not currently on a branch.",
		Err:    errors.New("exit status 128"),
	}
	wrapped := fmt.Errorf("reading manifests: %w", fmt.Errorf("fetching: %w", inner))

	assert.Equal(t, "fatal: You are not currently on a branch.", gitStderr(wrapped))
	assert.Equal(t, gitFailureBlocked, classifyGitError(wrapped))
	assert.Equal(t, gitFailureUnknown, classifyGitError(nil))
	assert.Empty(t, gitStderr(errors.New("not from runGit")))
}

// TestGitErrorRendersTheSameSentence pins that turning runGit's formatted error
// into a type did not change a single log line or event message.
func TestGitErrorRendersTheSameSentence(t *testing.T) {
	withDetail := &gitError{Args: []string{"show", "origin/main:go.mod"}, Stderr: "fatal: bad object", Err: errors.New("exit status 128")}
	assert.Equal(t, "git show origin/main:go.mod: exit status 128: fatal: bad object", withDetail.Error())

	silent := &gitError{Args: []string{"fetch", "origin", "main"}, Err: errors.New("signal: killed")}
	assert.Equal(t, "git fetch origin main: signal: killed", silent.Error())

	require.ErrorIs(t, &gitError{Err: context.DeadlineExceeded}, context.DeadlineExceeded)
}

// TestGitFailureSignatureStable is the property the once-only escalation rests
// on: the same condition twice is one signature, a different condition is a
// different one, and the noise that varies between two runs of the SAME
// condition is normalised away.
func TestGitFailureSignatureStable(t *testing.T) {
	const stderr = "error: cannot lock ref 'refs/remotes/origin/main'"

	first := gitFailureSignature("heimdall", gitFailureBlocked, stderr)
	second := gitFailureSignature("heimdall", gitFailureBlocked, stderr)
	assert.Equal(t, first, second, "the same condition must not re-escalate")

	assert.NotEqual(t, first,
		gitFailureSignature("heimdall", gitFailureBlocked, "fatal: You are not currently on a branch."),
		"a different condition is a new escalation")
	assert.NotEqual(t, first, gitFailureSignature("metadata", gitFailureBlocked, stderr),
		"a signature must not carry across anvils")
	assert.NotEqual(t, first, gitFailureSignature("heimdall", gitFailureTransient, stderr),
		"the classification is part of the condition")
}

// TestGitFailureSignatureNormalisesNoise covers the three things that change on
// every run of one unchanging condition. Left in, each would make every night a
// "new" condition and re-escalate — the exact bug, arriving via the normaliser.
func TestGitFailureSignatureNormalisesNoise(t *testing.T) {
	sig := func(detail string) string { return gitFailureSignature("heimdall", gitFailureBlocked, detail) }

	assert.Equal(t,
		sig("fatal: Unable to create '/srv/anvils/heimdall/.git/index.lock': File exists."),
		sig("fatal: Unable to create 'C:\\source\\heimdall\\.git\\index.lock': File exists."),
		"an absolute path is the host's, not the condition's")

	assert.Equal(t,
		sig("error: cannot lock ref: is at 0123456789abcdef0123456789abcdef01234567 but expected aaaa000011112222333344445555666677778888"),
		sig("error: cannot lock ref: is at 99887766554433221100ffeeddccbbaa99887766 but expected 1234abcd5678ef901234abcd5678ef901234abcd"),
		"object names move on every push while the condition does not")

	assert.Equal(t,
		sig("2026-08-27T03:00:01Z error: you have unmerged paths."),
		sig("2026-08-28T03:00:02Z error: you have unmerged paths."),
		"a timestamp changes by definition")

	assert.Equal(t,
		sig("error: you have unmerged paths."),
		sig("  ERROR:   you   have\n\tunmerged paths.  "),
		"case and whitespace are not the condition")

	// The other side of the path rule: a ref name is not a filesystem path, and
	// two anvils blocked on two different refs are two conditions.
	assert.NotEqual(t,
		sig("error: cannot lock ref 'refs/remotes/origin/main'"),
		sig("error: cannot lock ref 'refs/remotes/origin/release'"),
		"a ref name must survive normalisation")
}

// timeoutError is a net.Error that reports a timeout, standing in for a dial
// that never completed.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }
func (timeoutError) Temporary() bool {
	return true
}

var _ net.Error = timeoutError{}
