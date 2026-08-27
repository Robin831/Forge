package selfdeploy

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/gitfail"
)

// These are the operator-facing sentences, and they are pure functions of a
// cause, a path set and git's evidence — so they are tested as such rather than
// only through the git-backed pull tests, where each one is exercised by
// whichever branch that scenario happens to take. What is asserted is the
// content an operator acts on: the command names the right paths and leaves the
// expected ones alone, the claim a command makes never outruns what it covers,
// and no message promises a withdrawal it cannot perform.

// msgDeployer builds a Deployer that only ever renders messages: no command is
// run and no failure raised, so the fakes below are never reached.
func msgDeployer(t *testing.T, repo, branch string) *Deployer {
	t.Helper()
	return New(
		Config{RepoPath: repo, Branch: branch},
		&fakeCommander{}, &fakeRestarter{}, &fakeSink{}, nil,
		WithEmitter(&fakeEmitter{}),
		WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))),
	)
}

func TestPullRemediationNamesOneCommandPerCause(t *testing.T) {
	d := msgDeployer(t, "/srv/forge", "main")

	tests := []struct {
		name  string
		cause gitfail.Cause
		paths []string
		want  []string
		avoid []string
	}{
		{
			name:  "unmerged names both ways out of a half-finished merge",
			cause: gitfail.CauseUnmerged,
			want:  []string{"git -C /srv/forge merge --abort", "rebase --abort"},
		},
		{
			name:  "detached head puts the checkout back on the deploy branch",
			cause: gitfail.CauseDetachedHead,
			want:  []string{"git -C /srv/forge checkout main"},
		},
		{
			name:  "ref lock points at the lock file git named, never at a delete-everything",
			cause: gitfail.CauseRefLock,
			want:  []string{"git output:", "/srv/forge/.git"},
			avoid: []string{"rm -rf"},
		},
		{
			name:  "not-a-repo points at the setting, not at the checkout",
			cause: gitfail.CauseNotARepo,
			want:  []string{"self_deploy.repo_path", "config.yaml"},
		},
		{
			name:  "an unmodelled cause gets a diagnostic, never an invented command",
			cause: gitfail.CauseUnknown,
			want:  []string{"git -C /srv/forge status", "no "},
			avoid: []string{"stash push", "reset --hard"},
		},
		{
			// Nothing was enumerated, so no sentence may name a set of files:
			// `git status` in a checkout already known to be broken is exactly
			// the command that also fails.
			name:  "a dirty tree with nothing enumerated falls back to the template",
			cause: gitfail.CauseDirtyTree,
			want:  []string{"git -C /srv/forge status", "stash push -u -- <path>", ".beads/config.yaml"},
		},
		{
			// Every path is one the checkout is meant to carry, so there is
			// nothing to tell the operator to move.
			name:  "a tree holding only expected paths names no pathspec",
			cause: gitfail.CauseDirtyTree,
			paths: []string{".beads/config.yaml", ".beads/.gitignore"},
			want:  []string{"stash push -u -- <path>"},
		},
		{
			name:  "a mixed tree stashes the unexpected path and says the rest stays",
			cause: gitfail.CauseDirtyTree,
			paths: []string{".beads/config.yaml", "internal/daemon/daemon.go"},
			want: []string{
				"git -C /srv/forge stash push -u -- internal/daemon/daemon.go",
				"leaving the paths annotated as expected in place",
			},
			avoid: []string{".beads/config.yaml"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := d.pullRemediation(tc.cause, tc.paths)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("remediation does not mention %q:\n%s", w, got)
				}
			}
			for _, a := range tc.avoid {
				if strings.Contains(got, a) {
					t.Errorf("remediation must not mention %q:\n%s", a, got)
				}
			}
		})
	}
}

// TestPullRemediationWithoutARepoPathStillReads: the path is config-supplied and
// can be empty (a misconfigured self_deploy block is one of the ways this
// message gets raised at all), and `git -C  status` is not a command.
func TestPullRemediationWithoutARepoPathStillReads(t *testing.T) {
	d := msgDeployer(t, "", "main")
	got := d.pullRemediation(gitfail.CauseUnknown, nil)
	if !strings.Contains(got, "git -C <checkout> status") {
		t.Errorf("remediation does not fall back to a placeholder checkout:\n%s", got)
	}
}

// TestPullRemediationNeverClaimsMoreThanTheCommandCovers is the rule that makes
// a capped remediation safe: past the pathspec's own caps the sentence has to
// say how many paths the pasted line actually deals with and where to find the
// rest. Described as "the unexpected changes" it would read as the whole fix
// while leaving the deploy blocked on the paths it never named.
func TestPullRemediationNeverClaimsMoreThanTheCommandCovers(t *testing.T) {
	d := msgDeployer(t, "/srv/forge", "main")

	paths := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go"}
	got := d.pullRemediation(gitfail.CauseDirtyTree, paths)
	if !strings.Contains(got, "more than fit one command") {
		t.Errorf("a capped pathspec must say so:\n%s", got)
	}
	if !strings.Contains(got, "sets the first 5 aside") {
		t.Errorf("a capped pathspec must name how many it covers:\n%s", got)
	}
	if !strings.Contains(got, "git -C /srv/forge status` lists in full") {
		t.Errorf("a capped pathspec must say where the rest are:\n%s", got)
	}
	if strings.Contains(got, "g.go") {
		t.Errorf("the command names a path past the cap:\n%s", got)
	}

	// Two paths, one of which alone exhausts the byte budget: the sentence has
	// to survive being read aloud, so it says "the first one" and not "the
	// first 1".
	long := strings.Repeat("x", gitfail.MaxPathLen)
	other := strings.Repeat("y", gitfail.MaxPathLen)
	got = d.pullRemediation(gitfail.CauseDirtyTree, []string{long, other})
	if !strings.Contains(got, "sets the first one aside") {
		t.Errorf("a single-path pathspec must read as prose:\n%s", got)
	}
}

// TestPullRemediationRefusesToPasteAPathItHadToRewrite: a path is only written
// into a command when sanitization left it byte-identical. A truncated or
// escape-stripped path names a DIFFERENT file, and this line is meant to be
// pasted and run.
func TestPullRemediationRefusesToPasteAPathItHadToRewrite(t *testing.T) {
	d := msgDeployer(t, "/srv/forge", "main")

	hostile := "internal/\x1b[31mdaemon\x1b[0m.go"
	got := d.pullRemediation(gitfail.CauseDirtyTree, []string{hostile})
	if strings.Contains(got, "\x1b") {
		t.Errorf("the remediation carries an escape sequence through:\n%q", got)
	}
	if !strings.Contains(got, "stash push -u -- <path>") {
		t.Errorf("an unpasteable path must fall back to the template:\n%s", got)
	}
}

// TestStashRetainedDetailNamesTheEntryAndItsRecovery is the message an operator
// reads when their work is in a stash Forge could not put back — the only record
// of where it went.
func TestStashRetainedDetailNamesTheEntryAndItsRecovery(t *testing.T) {
	d := msgDeployer(t, "/srv/forge", "main")
	const sha = "0123456789abcdef0123456789abcdef01234567"

	got := d.stashRetainedDetail(sha, []string{"app.go"}, "the pop conflicted")
	for _, want := range []string{
		"not lost",
		sha,
		"/srv/forge",
		"the pop conflicted",
		"app.go",
		"stash show -p " + sha,
		"stash apply " + sha,
		"abandoned before the rebuild",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail does not mention %q:\n%s", want, got)
		}
	}
	// `stash pop` takes whatever is on top of a stack shared with every worker
	// worktree, which is not necessarily this entry.
	if strings.Contains(got, "stash pop") {
		t.Errorf("the recovery must address the entry by SHA, not pop the top:\n%s", got)
	}
}

// TestStashRetainedDetailPromisesNoAutomaticWithdrawal is the correction this
// message exists in this shape for. Recovering from a failed pop leaves the tree
// clean, so the next deploy succeeds by construction — an entry advertised as
// "cleared once a deploy pulls successfully" would therefore vanish within one
// cycle while the work was still on the stack.
func TestStashRetainedDetailPromisesNoAutomaticWithdrawal(t *testing.T) {
	d := msgDeployer(t, "/srv/forge", "main")

	for _, got := range []string{
		d.stashRetainedDetail("deadbeef", nil, "the pop conflicted"),
		d.stashRetainedDetail("", nil, "the stack could not be read"),
	} {
		if strings.Contains(got, "clears this entry automatically once a deploy pulls successfully") {
			t.Errorf("the message promises a withdrawal a later deploy must not perform:\n%s", got)
		}
		if !strings.Contains(got, "NOT withdrawn by the next deploy that works") {
			t.Errorf("the message does not say the entry outlives a successful deploy:\n%s", got)
		}
		if !strings.Contains(got, stashMessage) {
			t.Errorf("the message does not name the label the withdrawal is keyed on:\n%s", got)
		}
	}
}

// TestStashRetainedDetailWithoutASHASaysHowToFindTheEntry: the probe that names
// the entry is itself one of the things that can fail, and a message that cannot
// quote a SHA must not pretend to — it says how to find the entry by its label
// instead.
func TestStashRetainedDetailWithoutASHASaysHowToFindTheEntry(t *testing.T) {
	d := msgDeployer(t, "/srv/forge", "main")
	got := d.stashRetainedDetail("", nil, "the stack could not be read")

	for _, want := range []string{"cannot name the entry", "stash list", stashMessage, "stash pop"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail does not mention %q:\n%s", want, got)
		}
	}
}

// TestAppendEvidenceKeepsGitsWordsLast bounds the assembled message by trimming
// the evidence rather than the whole string: a blind cut of the assembled detail
// lands mid-command, and a command truncated to a shorter pathspec is a pasted
// line that names a different set of files.
func TestAppendEvidenceKeepsGitsWordsLast(t *testing.T) {
	t.Run("nothing to say adds no label", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("headline.")
		appendEvidence(&b, "")
		if got := b.String(); got != "headline." {
			t.Errorf("appendEvidence with no evidence wrote %q", got)
		}
	})

	t.Run("short evidence survives whole", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("headline.")
		appendEvidence(&b, "fatal: You are not currently on a branch")
		got := b.String()
		if !strings.HasSuffix(got, "fatal: You are not currently on a branch") {
			t.Errorf("evidence is not the last section:\n%s", got)
		}
		if !strings.Contains(got, "git output: ") {
			t.Errorf("evidence is not labelled:\n%s", got)
		}
	})

	t.Run("long evidence is trimmed to the budget the message left", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(strings.Repeat("h", 1500))
		appendEvidence(&b, strings.Repeat("g", 4000))
		if b.Len() > maxDeployDetailBytes {
			t.Errorf("assembled detail is %d bytes, over the %d bound", b.Len(), maxDeployDetailBytes)
		}
	})

	t.Run("a message that spent its budget still shows some evidence", func(t *testing.T) {
		var b strings.Builder
		// A headline that alone exceeds the whole bound: the evidence still gets
		// its floor, because a claim with nothing to check it against is worth
		// less than an over-long message.
		b.WriteString(strings.Repeat("h", maxDeployDetailBytes+500))
		appendEvidence(&b, strings.Repeat("g", 4000))
		got := b.String()
		if !strings.Contains(got, "git output: ") {
			t.Error("evidence was dropped entirely from an over-budget message")
		}
		tail := got[strings.Index(got, "git output: ")+len("git output: "):]
		if len(tail) < minEvidenceBytes/2 {
			t.Errorf("evidence trimmed to %d bytes, below the %d floor", len(tail), minEvidenceBytes)
		}
	})
}
