package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skewCmd is a Commander that answers git invocations from a table keyed by the
// joined argument list, recording what it was asked. An unmatched command is a
// test failure rather than a silent empty answer, since a skew derived from an
// unanswered probe is exactly the wrong number.
type skewCmd struct {
	t        *testing.T
	replies  map[string]skewReply
	commands []string
}

type skewReply struct {
	out string
	err error
}

func (c *skewCmd) Run(_ context.Context, _, name string, args ...string) ([]byte, error) {
	key := name + " " + strings.Join(args, " ")
	c.commands = append(c.commands, key)
	r, ok := c.replies[key]
	if !ok {
		c.t.Fatalf("unexpected command: %s", key)
	}
	return []byte(r.out), r.err
}

func (c *skewCmd) ran(key string) bool {
	for _, got := range c.commands {
		if got == key {
			return true
		}
	}
	return false
}

const (
	buildFull = "1111111111111111111111111111111111111111"
	headFull  = "2222222222222222222222222222222222222222"
	fetchCmd  = "git fetch origin +main:refs/remotes/origin/main"
)

// baseReplies is the happy-path answer set: fetch, resolve both ends, confirm
// the build is an ancestor.
func baseReplies() map[string]skewReply {
	return map[string]skewReply{
		fetchCmd: {},
		"git rev-parse --verify --quiet refs/remotes/origin/main^{commit}":     {out: headFull + "\n"},
		"git rev-parse --verify --quiet 1111111^{commit}":                      {out: buildFull + "\n"},
		fmt.Sprintf("git merge-base --is-ancestor %s %s", buildFull, headFull): {},
	}
}

func newChecker(t *testing.T, replies map[string]skewReply, build string) (*SkewChecker, *skewCmd) {
	t.Helper()
	cmd := &skewCmd{t: t, replies: replies}
	return NewSkewChecker(SkewConfig{RepoPath: "/repo", Branch: "main", BuildSHA: build}, cmd), cmd
}

// TestSkewCheck_CountsCommitsBehind is the case the whole check exists for: the
// branch moved without anything telling the daemon, and the distance plus the
// age of the oldest undeployed commit are what say so.
func TestSkewCheck_CountsCommitsBehind(t *testing.T) {
	oldest := time.Now().Add(-5 * 24 * time.Hour).Truncate(time.Second)
	replies := baseReplies()
	replies[fmt.Sprintf("git rev-list --count %s..%s", buildFull, headFull)] = skewReply{out: "7\n"}
	// git log is newest-first, so the LAST line is the oldest undeployed commit.
	replies[fmt.Sprintf("git log --format=%%ct %s..%s", buildFull, headFull)] = skewReply{
		out: fmt.Sprintf("%d\n%d\n", time.Now().Unix(), oldest.Unix()),
	}

	checker, _ := newChecker(t, replies, "1111111")
	skew, err := checker.Check(context.Background())
	require.NoError(t, err)

	assert.True(t, skew.Behind())
	assert.Equal(t, 7, skew.Commits)
	assert.Equal(t, headFull, skew.HeadSHA)
	assert.Equal(t, buildFull, skew.BuildSHA)
	assert.Equal(t, "main", skew.Branch)
	assert.Equal(t, oldest.Unix(), skew.Since.Unix(), "the age is dated from the oldest undeployed commit, not the newest")
	assert.InDelta(t, (5 * 24 * time.Hour).Hours(), skew.Age(time.Now()).Hours(), 1)
	assert.Contains(t, skew.Summary(time.Now()), "7 commit(s) behind origin/main")
}

// TestSkewCheck_UpToDateIsZeroAndCostsNoWalk pins that a current build reports
// zero without asking git to count or date anything: the two SHAs being equal
// is the whole answer.
func TestSkewCheck_UpToDateIsZeroAndCostsNoWalk(t *testing.T) {
	replies := map[string]skewReply{
		fetchCmd: {},
		"git rev-parse --verify --quiet refs/remotes/origin/main^{commit}": {out: headFull},
		"git rev-parse --verify --quiet 2222222^{commit}":                  {out: headFull},
	}
	checker, cmd := newChecker(t, replies, "2222222")

	skew, err := checker.Check(context.Background())
	require.NoError(t, err)
	assert.False(t, skew.Behind())
	assert.Equal(t, 0, skew.Commits)
	assert.Zero(t, skew.Age(time.Now()))
	for _, c := range cmd.commands {
		assert.NotContains(t, c, "rev-list")
	}
}

// TestSkewCheck_DirtyBuildStampIsStillMeasurable: a build made from a modified
// tree was still made from that commit, and the distance from it is exactly as
// meaningful as a clean build's.
func TestSkewCheck_DirtyBuildStampIsStillMeasurable(t *testing.T) {
	replies := baseReplies()
	replies[fmt.Sprintf("git rev-list --count %s..%s", buildFull, headFull)] = skewReply{out: "2"}
	replies[fmt.Sprintf("git log --format=%%ct %s..%s", buildFull, headFull)] = skewReply{out: "0"}

	checker, _ := newChecker(t, replies, "1111111-dirty")
	skew, err := checker.Check(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, skew.Commits)
}

// TestSkewCheck_UnidentifiedBuildNeverReachesGit covers the build stamps that
// name no commit. They must not be handed to git at all: `main` and `v1.2.0`
// both RESOLVE, and a binary built from an old commit but stamped with a moving
// name would report a skew of zero forever.
func TestSkewCheck_UnidentifiedBuildNeverReachesGit(t *testing.T) {
	for _, build := range []string{"", "unknown", "dev", "1.0.0", "main", "v1.2.0", "zzzzzzz"} {
		t.Run(build, func(t *testing.T) {
			checker, cmd := newChecker(t, map[string]skewReply{}, build)
			_, err := checker.Check(context.Background())
			require.ErrorIs(t, err, ErrBuildUnidentified)
			assert.Empty(t, cmd.commands, "an unidentifiable build must not reach git")
		})
	}
}

// TestSkewCheck_FetchFailureIsAnErrorNotAZeroSkew: the caller deploys on
// Commits > 0, so a failure to measure has to arrive as an error. Reported as
// "not behind" it would restore the silence the check exists to break.
func TestSkewCheck_FetchFailureIsAnErrorNotAZeroSkew(t *testing.T) {
	replies := map[string]skewReply{
		fetchCmd: {out: "fatal: could not read from remote repository", err: errors.New("exit 128")},
	}
	checker, _ := newChecker(t, replies, "1111111")
	_, err := checker.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not read from remote repository")
}

// TestSkewCheck_BuildNotInCheckout distinguishes "built somewhere else" from
// "the branch moved past it".
func TestSkewCheck_BuildNotInCheckout(t *testing.T) {
	replies := map[string]skewReply{
		fetchCmd: {},
		"git rev-parse --verify --quiet refs/remotes/origin/main^{commit}": {out: headFull},
		"git rev-parse --verify --quiet 1111111^{commit}":                  {err: errors.New("exit 1")},
	}
	checker, _ := newChecker(t, replies, "1111111")
	_, err := checker.Check(context.Background())
	require.ErrorIs(t, err, ErrBuildNotInCheckout)
}

// TestSkewCheck_ForcePushedBranchIsNotADistance: with the build no longer on the
// branch, "behind by N" is a claim about a history that no longer contains it,
// so the condition is reported rather than counted.
func TestSkewCheck_ForcePushedBranchIsNotADistance(t *testing.T) {
	replies := baseReplies()
	replies[fmt.Sprintf("git merge-base --is-ancestor %s %s", buildFull, headFull)] = skewReply{err: errors.New("exit 1")}

	checker, cmd := newChecker(t, replies, "1111111")
	skew, err := checker.Check(context.Background())
	require.ErrorIs(t, err, ErrBuildNotAncestor)
	assert.Equal(t, 0, skew.Commits)
	assert.False(t, cmd.ran(fmt.Sprintf("git rev-list --count %s..%s", buildFull, headFull)),
		"nothing is counted once the build is off the branch")
}

// TestSkewCheck_AgeIsBestEffort pins that the count survives a failure to date
// it: the distance is the measurement and the age is a courtesy.
func TestSkewCheck_AgeIsBestEffort(t *testing.T) {
	replies := baseReplies()
	replies[fmt.Sprintf("git rev-list --count %s..%s", buildFull, headFull)] = skewReply{out: "3"}
	replies[fmt.Sprintf("git log --format=%%ct %s..%s", buildFull, headFull)] = skewReply{err: errors.New("exit 1")}

	checker, _ := newChecker(t, replies, "1111111")
	skew, err := checker.Check(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, skew.Commits)
	assert.True(t, skew.Since.IsZero())
	assert.Zero(t, skew.Age(time.Now()), "an unknown age reads as zero, so a threshold on it never fires on a guess")
}

// TestSkewCheck_FetchUsesAnExplicitForcedRefspec: the next step reads
// refs/remotes/origin/<branch>, and only the explicit refspec guarantees the
// fetch wrote it. Reading FETCH_HEAD instead is unsafe here because worker
// worktrees share this repository and fetch into it too.
func TestSkewCheck_FetchUsesAnExplicitForcedRefspec(t *testing.T) {
	replies := baseReplies()
	replies[fmt.Sprintf("git rev-list --count %s..%s", buildFull, headFull)] = skewReply{out: "1"}
	replies[fmt.Sprintf("git log --format=%%ct %s..%s", buildFull, headFull)] = skewReply{out: "0"}

	checker, cmd := newChecker(t, replies, "1111111")
	_, err := checker.Check(context.Background())
	require.NoError(t, err)
	assert.True(t, cmd.ran(fetchCmd), "commands run: %v", cmd.commands)
}

// TestSkewCheck_BranchThatLooksLikeAFlagIsRefused: the branch is operator config
// and reaches git as a positional argument.
func TestSkewCheck_BranchThatLooksLikeAFlagIsRefused(t *testing.T) {
	cmd := &skewCmd{t: t, replies: map[string]skewReply{}}
	checker := NewSkewChecker(SkewConfig{RepoPath: "/repo", Branch: "--upload-pack=x", BuildSHA: "1111111"}, cmd)
	_, err := checker.Check(context.Background())
	require.Error(t, err)
	assert.Empty(t, cmd.commands)
}

// TestSkewCheck_DefaultsToMain mirrors Config.ResolvedBranch, so an unset branch
// means the same thing to the check and to the deploy it triggers.
func TestSkewCheck_DefaultsToMain(t *testing.T) {
	checker := NewSkewChecker(SkewConfig{RepoPath: "/repo"}, &skewCmd{t: t})
	assert.Equal(t, "main", checker.cfg.Branch)
}

// TestVersionSkewIsClearedByASuccessfulDeploy: a deploy that goes live puts the
// running build at the tip, so the stalled-skew entry must be among the ones its
// terminal resolve withdraws — otherwise the row outlives the condition.
func TestVersionSkewIsClearedByASuccessfulDeploy(t *testing.T) {
	assert.Contains(t, AllReasons, ReasonVersionSkew)
	assert.False(t, ReasonVersionSkew.IsSticky(),
		"a deploy that reaches the tip proves the skew gone, unlike a stash it says nothing about")
}
