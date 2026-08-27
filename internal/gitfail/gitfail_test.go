package gitfail

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The pattern tables, the cause mapping and the path rendering are pinned in
// depth by internal/depcheck's own tests, which drive them through its aliases.
// What is pinned here is the seam this package added when it became shared: the
// pieces a second caller reaches for directly, and the two guards that only make
// sense once more than one caller exists.

// TestDirtyPathsWithoutARunnerIsANoOp: the runner is the caller's, and a caller
// that has none must get an empty list rather than a panic — the enumeration is
// best-effort at every site that uses it.
func TestDirtyPathsWithoutARunnerIsANoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
		run  RunGitFunc
	}{
		{"no runner", "/tmp/repo", nil},
		{"no directory", "", func(context.Context, string, ...string) ([]byte, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := DirtyPaths(context.Background(), tc.dir, tc.run)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if len(paths) != 0 {
				t.Fatalf("paths = %v, want none", paths)
			}
		})
	}
}

// TestDirtyPathsAsksForPorcelainZ pins the command, because the parser is only
// correct for that exact format: the plain short format C-quotes a path with a
// space in it and separates a rename's two paths with ` -> `, which is itself a
// legal substring of a filename.
func TestDirtyPathsAsksForPorcelainZ(t *testing.T) {
	var got []string
	_, err := DirtyPaths(context.Background(), "/repo", func(_ context.Context, dir string, args ...string) ([]byte, error) {
		got = append([]string{dir}, args...)
		return []byte("A  added.go\x00"), nil
	})
	if err != nil {
		t.Fatalf("DirtyPaths: %v", err)
	}
	if want := "/repo status --porcelain -z"; strings.Join(got, " ") != want {
		t.Errorf("ran %q, want %q", strings.Join(got, " "), want)
	}
}

// TestDirtyPathsPropagatesTheRunnersFailure: `git status` in a checkout already
// known to be broken is exactly the command that may also fail, and the caller
// decides what to do about that — this must not swallow it into an empty list,
// which reads as a clean tree.
func TestDirtyPathsPropagatesTheRunnersFailure(t *testing.T) {
	boom := errors.New("not a git repository")
	paths, err := DirtyPaths(context.Background(), "/repo", func(context.Context, string, ...string) ([]byte, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the runner's own error", err)
	}
	if len(paths) != 0 {
		t.Fatalf("paths = %v, want none alongside an error", paths)
	}
}

// TestSanitizeFlattensAndBounds: every caller renders git's words into a
// daemon.log line and an activity-feed row Hearth wraps without stripping, so
// escape sequences must be gone and the whole thing must be one line inside the
// bound it was given — the marker included, not added on top of it.
func TestSanitizeFlattensAndBounds(t *testing.T) {
	raw := "  fatal: \x1b[31mcannot lock ref\x1b[0m\n  'refs/heads/main': is at 0000\n"
	got := Sanitize(raw, 200)
	if strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("sanitized text still holds escapes or line breaks: %q", got)
	}
	if want := "fatal: cannot lock ref 'refs/heads/main': is at 0000"; got != want {
		t.Errorf("Sanitize = %q, want %q", got, want)
	}

	long := strings.Repeat("x", 500)
	if bounded := Sanitize(long, 100); len(bounded) > 100 {
		t.Errorf("Sanitize returned %d bytes for a bound of 100", len(bounded))
	}
	// A bound smaller than the marker still returns something legible rather
	// than a slice that cuts the marker in half.
	if got := Bound("abcdef", 1); got == "" || len(got) > len("…")+1 {
		t.Errorf("Bound with a degenerate bound = %q", got)
	}
}

// TestClassifyTreatsAnUnmodelledMessageAsUnknown is the fail-safe direction
// both callers rely on: an unrecognised message keeps the noisy-but-honest
// behaviour rather than raising an escalation nobody can act on and then muting
// the only signal that anything is wrong.
func TestClassifyTreatsAnUnmodelledMessageAsUnknown(t *testing.T) {
	if got := Classify("error: something nobody has modelled", errors.New("exit status 1")); got != Unknown {
		t.Errorf("Classify = %v, want Unknown", got)
	}
	if got := Classify("", nil); got != Unknown {
		t.Errorf("Classify of nothing = %v, want Unknown", got)
	}
}

// TestAnUnconcludedSequencerOperationIsBlocked pins the one refusal a checkout
// reaches once its conflicts have been STAGED. The index is back at stage 0 by
// then, so nothing about it is unmerged and none of the conflict patterns match
// git's sentence — yet the condition reproduces on every run until somebody
// concludes or abandons the operation, which is exactly what Blocked means. Read
// as Unknown it would be treated as transient and retried silently forever.
//
// The three sentences are git's own, verbatim, for the three operations that
// leave a sequencer ref behind.
func TestAnUnconcludedSequencerOperationIsBlocked(t *testing.T) {
	for _, msg := range []string{
		"error: You have not concluded your merge (MERGE_HEAD exists).",
		"error: You have not concluded your cherry-pick (CHERRY_PICK_HEAD exists).",
		"error: You have not concluded your revert (REVERT_HEAD exists).",
	} {
		if got := Classify(msg, errors.New("exit status 128")); got != Blocked {
			t.Errorf("Classify(%q) = %v, want Blocked", msg, got)
		}
		// And it is the same condition to an operator as an unmerged index, so
		// it earns the same recoverable `merge --abort` remediation rather than
		// the diagnostic fallback.
		if got := CauseOf(msg); got != CauseUnmerged {
			t.Errorf("CauseOf(%q) = %v, want CauseUnmerged", msg, got)
		}
	}
}
