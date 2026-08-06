package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// approvingWarden returns a WardenReviewer that always approves.
func approvingWarden() func(context.Context, string, string, string, string, string, *state.DB, string, ...provider.Provider) (*warden.ReviewResult, error) {
	return func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "Looks good"}, nil
	}
}

// initBranchRepo creates a git repo holding an origin/main remote-tracking ref
// and a checked-out forge branch, mirroring the shape of a real worker
// worktree. commitsAhead extra commits are added on the branch.
func initBranchRepo(t *testing.T, branch string, commitsAhead int) string {
	t.Helper()
	dir, _ := initGitRepo(t)
	env := cleanGitEnv()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	// A remote-tracking ref is enough: resolveBaseRef only rev-parses it.
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("checkout", "-b", branch)
	for i := 0; i < commitsAhead; i++ {
		name := filepath.Join(dir, "work"+strconv.Itoa(i)+".txt")
		require.NoError(t, os.WriteFile(name, []byte("work"), 0o644))
		git("add", ".")
		git("commit", "-m", "feat: work "+strconv.Itoa(i))
	}
	return dir
}

// emptyDiffParams builds an approved-run Params whose worktree is a real repo
// with the given number of commits ahead of origin/main.
func emptyDiffParams(t *testing.T, db *state.DB, commitsAhead int) Params {
	t.Helper()
	params, _, _ := baseParams(t, db)
	params.WardenReviewer = approvingWarden()

	repo := initBranchRepo(t, "forge/test-bead", commitsAhead)
	params.WorktreeCreator = func(_ context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
		return &worktree.Worktree{
			BeadID:    beadID,
			AnvilPath: anvilPath,
			Path:      repo,
			Branch:    "forge/" + beadID,
		}, nil
	}
	return params
}

// hasEvent reports whether an event of the given type was logged for the bead.
func hasEvent(t *testing.T, db *state.DB, evType state.EventType) (string, bool) {
	t.Helper()
	events, err := db.RecentEvents(100)
	require.NoError(t, err)
	for _, e := range events {
		if e.Type == evType {
			return e.Message, true
		}
	}
	return "", false
}

// TestEmptyBranch_CloseAction verifies that an approved run whose branch has no
// commits against the base short-circuits before PR creation and resolves as a
// close-the-bead outcome.
func TestEmptyBranch_CloseAction(t *testing.T) {
	db := newTestDB(t)
	params := emptyDiffParams(t, db, 0)
	params.EmptyDiffAction = config.EmptyDiffActionClose

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.EmptyDiff, "empty branch must be reported as EmptyDiff")
	assert.False(t, outcome.Success, "an empty branch is not a PR-able success")
	assert.Nil(t, outcome.Error, "an empty branch is terminal, not an error")
	assert.False(t, outcome.NeedsHuman, "the daemon decides escalation from EmptyDiffAction, not NeedsHuman")
	assert.Equal(t, config.EmptyDiffActionClose, outcome.EmptyDiffAction)
	assert.Equal(t, "origin/main", outcome.EmptyDiffBase)
	assert.Equal(t, warden.VerdictApprove, outcome.Verdict)

	msg, ok := hasEvent(t, db, state.EventSmithEmptyResult)
	assert.True(t, ok, "smith_empty_result event must be recorded")
	assert.Contains(t, msg, "no commits vs origin/main")
}

// TestEmptyBranch_AttentionAction verifies the attention action is carried on
// the outcome and the run still stops before PR creation.
func TestEmptyBranch_AttentionAction(t *testing.T) {
	db := newTestDB(t)
	params := emptyDiffParams(t, db, 0)
	params.EmptyDiffAction = config.EmptyDiffActionAttention

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.EmptyDiff)
	assert.False(t, outcome.Success)
	assert.Equal(t, config.EmptyDiffActionAttention, outcome.EmptyDiffAction)

	_, ok := hasEvent(t, db, state.EventSmithEmptyResult)
	assert.True(t, ok, "smith_empty_result event must be recorded")
}

// TestEmptyBranch_UnsetActionDefaultsToAttention verifies that an unset (or
// unrecognised) empty_diff_action never auto-closes a bead.
func TestEmptyBranch_UnsetActionDefaultsToAttention(t *testing.T) {
	for _, configured := range []string{"", "   ", "nonsense"} {
		t.Run("action="+configured, func(t *testing.T) {
			db := newTestDB(t)
			params := emptyDiffParams(t, db, 0)
			params.EmptyDiffAction = configured

			outcome := Run(context.Background(), params)

			assert.True(t, outcome.EmptyDiff)
			assert.Equal(t, config.EmptyDiffActionAttention, outcome.EmptyDiffAction)
		})
	}
}

// TestNonEmptyBranch_ProceedsToPR verifies the normal path is untouched when
// the branch actually carries commits.
func TestNonEmptyBranch_ProceedsToPR(t *testing.T) {
	db := newTestDB(t)
	params := emptyDiffParams(t, db, 3)
	params.EmptyDiffAction = config.EmptyDiffActionClose

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success, "a branch with commits must reach the PR path")
	assert.False(t, outcome.EmptyDiff)
	assert.Empty(t, outcome.EmptyDiffAction)

	_, ok := hasEvent(t, db, state.EventSmithEmptyResult)
	assert.False(t, ok, "no empty-result event for a branch with commits")
}

// TestCommitCountError_FallsThroughToPR verifies that a git failure is not
// mistaken for an empty branch — the run continues down the normal PR path.
func TestCommitCountError_FallsThroughToPR(t *testing.T) {
	db := newTestDB(t)
	params := emptyDiffParams(t, db, 0)
	params.EmptyDiffAction = config.EmptyDiffActionClose
	params.CommitCounter = func(_ context.Context, _, _, _ string) (int, error) {
		return 0, errors.New("rev-list exploded")
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.Success, "an unknown commit count must not short-circuit the pipeline")
	assert.False(t, outcome.EmptyDiff)

	_, ok := hasEvent(t, db, state.EventSmithEmptyResult)
	assert.False(t, ok)
}

// TestEmptyBranch_ComparesBranchAgainstResolvedBase verifies which refs the
// commit count is asked about: the branch under review vs the resolved base.
func TestEmptyBranch_ComparesBranchAgainstResolvedBase(t *testing.T) {
	db := newTestDB(t)
	params := emptyDiffParams(t, db, 0)

	var calls []string
	params.CommitCounter = func(_ context.Context, _, base, branch string) (int, error) {
		calls = append(calls, base+".."+branch)
		return 0, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.EmptyDiff)
	assert.Equal(t, []string{"origin/main..forge/test-bead"}, calls,
		"the count runs exactly once, against the resolved base")
}

// TestEmptyBranch_EpicChildComparesAgainstEpicBranch verifies that a Crucible
// child is compared against its epic branch, not main.
func TestEmptyBranch_EpicChildComparesAgainstEpicBranch(t *testing.T) {
	db := newTestDB(t)
	params := emptyDiffParams(t, db, 0)
	params.BaseBranch = "feature/epic-1"

	// Give the repo the epic's remote-tracking ref so it resolves.
	repo := ""
	wtCreator := params.WorktreeCreator
	params.WorktreeCreator = func(ctx context.Context, anvilPath, beadID string) (*worktree.Worktree, error) {
		wt, err := wtCreator(ctx, anvilPath, beadID)
		if err != nil {
			return nil, err
		}
		repo = wt.Path
		cmd := exec.Command("git", "-C", repo, "update-ref", "refs/remotes/origin/feature/epic-1", "HEAD")
		cmd.Env = cleanGitEnv()
		require.NoError(t, cmd.Run())
		return wt, nil
	}

	outcome := Run(context.Background(), params)

	assert.True(t, outcome.EmptyDiff)
	assert.Equal(t, "origin/feature/epic-1", outcome.EmptyDiffBase)
}

// TestUnresolvableBaseRef_SkipsCheck verifies that a worktree with no
// resolvable base ref (no BaseBranch, no origin/main) skips the check entirely
// rather than counting against a bogus ref.
func TestUnresolvableBaseRef_SkipsCheck(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WardenReviewer = approvingWarden()

	counted := false
	params.CommitCounter = func(_ context.Context, _, _, _ string) (int, error) {
		counted = true
		return 0, nil
	}

	outcome := Run(context.Background(), params)

	assert.False(t, counted, "the commit count must not run without a resolved base ref")
	assert.True(t, outcome.Success)
	assert.False(t, outcome.EmptyDiff)
}

// TestCountCommitsAhead verifies the real git plumbing against a temporary repo:
// zero for a branch that has not moved off the base, and the exact count once
// commits are added.
func TestCountCommitsAhead(t *testing.T) {
	dir, _ := initGitRepo(t)
	env := cleanGitEnv()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	// Name the starting point so it can act as the base ref, then branch off it.
	git("branch", "base-ref")
	git("checkout", "-b", "forge/test-bead")

	ctx := context.Background()

	count, err := countCommitsAhead(ctx, dir, "base-ref", "forge/test-bead")
	require.NoError(t, err)
	assert.Equal(t, 0, count, "a branch with no commits of its own is zero ahead")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("work"), 0o644))
	git("add", ".")
	git("commit", "-m", "feat: real work")

	count, err = countCommitsAhead(ctx, dir, "base-ref", "forge/test-bead")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestCountCommitsAhead_Error verifies that an unknown ref surfaces as an error
// rather than a zero count (which would be read as "empty branch").
func TestCountCommitsAhead_Error(t *testing.T) {
	dir, _ := initGitRepo(t)

	_, err := countCommitsAhead(context.Background(), dir, "origin/does-not-exist", "also-missing")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "rev-list"), "error should name the failing command: %v", err)
}
