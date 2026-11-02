package smelter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// batchRepo is a bare origin with main and, optionally, a batch branch that
// holds an earlier batch's rule and a fix commit somebody else pushed, plus an
// anvil clone on main for the Smelter to work from.
type batchRepo struct {
	t      *testing.T
	origin string
	anvil  string
	branch string
}

func (r batchRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = filteredEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func newBatchRepo(t *testing.T, withBatch bool) batchRepo {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}
	r := batchRepo{t: t, origin: t.TempDir(), anvil: t.TempDir(), branch: branchForAnvil("test-anvil")}
	r.git(r.origin, "init", "--bare", "--initial-branch=main")
	seed := t.TempDir()
	r.git(seed, "clone", r.origin, ".")
	r.git(seed, "config", "user.email", "test@example.com")
	r.git(seed, "config", "user.name", "Test")
	require.NoError(t, os.MkdirAll(filepath.Join(seed, ".forge"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seed, warden.RulesFileName), []byte("rules: []\n"), 0o644))
	r.git(seed, "add", ".")
	r.git(seed, "commit", "-m", "init")
	r.git(seed, "push", "origin", "main")
	if withBatch {
		r.git(seed, "checkout", "-b", r.branch)
		require.NoError(t, os.WriteFile(filepath.Join(seed, warden.RulesFileName),
			[]byte("rules:\n  - id: earlier-rule\n    category: style\n    pattern: foo\n    check: bar\n"), 0o644))
		r.git(seed, "commit", "-am", "forge: learn 1 warden rule(s) [no-changelog]")
		require.NoError(t, os.WriteFile(filepath.Join(seed, "FIX"), []byte("a lane's fix\n"), 0o644))
		r.git(seed, "add", "FIX")
		r.git(seed, "commit", "-m", "fix: a lane's fix on the batch branch")
		r.git(seed, "push", "origin", r.branch)
	}
	r.git(r.anvil, "clone", r.origin, ".")
	r.git(r.anvil, "config", "user.email", "test@example.com")
	r.git(r.anvil, "config", "user.name", "Test")
	return r
}

func smelterWithPR(t *testing.T, anvil, state string, err error) *Smelter {
	return smelterWithPRHead(t, anvil, state, "", err)
}

func smelterWithPRHead(t *testing.T, anvil, state, head string, err error) *Smelter {
	s := New(openTestDB(t), time.Hour, map[string]string{"test-anvil": anvil})
	s.findPR = func(context.Context, string, string) (batchPR, error) {
		if state == "" {
			return batchPR{}, err
		}
		return batchPR{Number: 7, State: state, HeadOID: head}, err
	}
	return s
}

func TestPrepareBatchBase_OpenPRAppends(t *testing.T) {
	r := newBatchRepo(t, true)
	base, err := smelterWithPR(t, r.anvil, "OPEN", nil).prepareBatchBase(context.Background(), r.anvil, r.branch)
	require.NoError(t, err)
	assert.Equal(t, appendToBatch, base.mode)
	assert.Empty(t, base.archivedAs)
}

func TestPrepareBatchBase_UnknownPRStateRefusesToReplace(t *testing.T) {
	r := newBatchRepo(t, true)
	_, err := smelterWithPR(t, r.anvil, "", errors.New("gh: rate limited")).prepareBatchBase(context.Background(), r.anvil, r.branch)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not replaced")
}

func TestPrepareBatchBase_NoBranchOrMergedPRReplaces(t *testing.T) {
	r := newBatchRepo(t, false)
	base, err := smelterWithPR(t, r.anvil, "", nil).prepareBatchBase(context.Background(), r.anvil, r.branch)
	require.NoError(t, err)
	assert.Equal(t, batchBase{mode: replaceBatch}, base)

	merged := newBatchRepo(t, true)
	head := merged.git(merged.origin, "rev-parse", "refs/heads/"+merged.branch)
	base, err = smelterWithPRHead(t, merged.anvil, "MERGED", head, nil).prepareBatchBase(context.Background(), merged.anvil, merged.branch)
	require.NoError(t, err)
	assert.Equal(t, batchBase{mode: replaceBatch, head: head}, base, "merged with this very head: in main, nothing to keep")
}

// An old merged PR says nothing about a later batch pushed to the reused
// branch: its head is kept before the replace (Copilot on #944).
func TestPrepareBatchBase_MergedPRWithAnotherHeadStillKeepsTheBranch(t *testing.T) {
	r := newBatchRepo(t, true)
	head := r.git(r.origin, "rev-parse", "refs/heads/"+r.branch)
	base, err := smelterWithPRHead(t, r.anvil, "MERGED", "0123456789abcdef0123456789abcdef01234567", nil).prepareBatchBase(context.Background(), r.anvil, r.branch)
	require.NoError(t, err)
	assert.Equal(t, r.branch+"-replaced-"+head[:12], base.archivedAs)
	assert.Equal(t, head, base.head)
}

// The replace is leased to the head that was inspected and kept: a push made
// after the inspection is never overwritten (Copilot on #944).
func TestCommitAndPush_ReplaceIsLeasedToTheInspectedHead(t *testing.T) {
	r := newBatchRepo(t, true)
	ctx := context.Background()
	s := smelterWithPR(t, r.anvil, "CLOSED", nil)
	base, err := s.prepareBatchBase(ctx, r.anvil, r.branch)
	require.NoError(t, err)

	other := t.TempDir()
	r.git(other, "clone", "--branch", r.branch, r.origin, ".")
	r.git(other, "-c", "user.email=l@example.com", "-c", "user.name=Lane", "commit", "--allow-empty", "-m", "pushed after the inspection")
	r.git(other, "push", "origin", r.branch)
	theirs := r.git(r.origin, "rev-parse", "refs/heads/"+r.branch)

	r.git(r.anvil, "checkout", "-b", r.branch, "origin/main")
	r.git(r.anvil, "fetch", "origin")
	require.NoError(t, os.WriteFile(filepath.Join(r.anvil, warden.RulesFileName), []byte("rules:\n  - id: x\n    category: style\n    pattern: p\n    check: c\n"), 0o644))
	require.Error(t, s.commitAndPush(ctx, r.anvil, r.branch, PassResults{Added: []string{"x"}}, false, base.head))
	assert.Equal(t, theirs, r.git(r.origin, "rev-parse", "refs/heads/"+r.branch))
}

// A closed, unmerged batch (or a branch with no PR) is replaced, but its head
// is pushed to a -replaced- branch first, so nothing it held is lost.
func TestPrepareBatchBase_ClosedUnmergedBranchIsKeptBeforeReplace(t *testing.T) {
	r := newBatchRepo(t, true)
	head := r.git(r.origin, "rev-parse", "refs/heads/"+r.branch)
	for _, state := range []string{"CLOSED", ""} {
		base, err := smelterWithPR(t, r.anvil, state, nil).prepareBatchBase(context.Background(), r.anvil, r.branch)
		require.NoError(t, err)
		assert.Equal(t, replaceBatch, base.mode)
		require.Equal(t, r.branch+"-replaced-"+head[:12], base.archivedAs)
		assert.Equal(t, head, r.git(r.origin, "rev-parse", "refs/heads/"+base.archivedAs), "the old head is on origin")
	}
}

// The incident end to end: an open batch PR with an earlier rule and a lane's
// fix commit. The next flush builds on the branch, keeps both, and its push is
// a fast-forward.
func TestFlushBase_AppendKeepsEarlierRulesAndForeignCommits(t *testing.T) {
	r := newBatchRepo(t, true)
	ctx := context.Background()
	s := smelterWithPR(t, r.anvil, "OPEN", nil)
	before := r.git(r.origin, "rev-parse", "refs/heads/"+r.branch)

	base, err := s.prepareBatchBase(ctx, r.anvil, r.branch)
	require.NoError(t, err)
	wt, err := worktree.NewManager().CreateWithOptions(ctx, r.anvil, "smelter-batch-test-anvil",
		worktree.CreateOptions{Branch: r.branch, ResetBranch: true, BaseBranch: r.branch})
	require.NoError(t, err)
	t.Cleanup(func() { _ = worktree.NewManager().Remove(context.Background(), r.anvil, wt) })

	rf, err := warden.LoadRules(wt.Path)
	require.NoError(t, err)
	require.Equal(t, []string{"earlier-rule"}, ruleIDs(rf.Rules), "the earlier batch's rule is what this run starts from")
	_, statErr := os.Stat(filepath.Join(wt.Path, "FIX"))
	require.NoError(t, statErr, "the lane's fix commit is in the worktree")

	rf.Rules = append(rf.Rules, warden.Rule{ID: "new-rule", Category: "style", Pattern: "p", Check: "c"})
	require.NoError(t, warden.SaveRules(wt.Path, rf))
	require.NoError(t, s.commitAndPush(ctx, wt.Path, r.branch, PassResults{Added: []string{"new-rule"}}, base.mode == appendToBatch, base.head))

	r.git(r.origin, "merge-base", "--is-ancestor", before, "refs/heads/"+r.branch)
	assert.Contains(t, r.git(r.origin, "show", "refs/heads/"+r.branch+":"+warden.RulesFileName), "earlier-rule")
}

// An append never overwrites what someone pushed after the Smelter read the
// branch: the push is refused and the flush fails, keeping its queue.
func TestCommitAndPush_AppendRefusesToOverwriteANewerPush(t *testing.T) {
	r := newBatchRepo(t, true)
	ctx := context.Background()
	s := smelterWithPR(t, r.anvil, "OPEN", nil)
	r.git(r.anvil, "fetch", "origin")
	r.git(r.anvil, "checkout", "-b", r.branch, "origin/"+r.branch)

	other := t.TempDir()
	r.git(other, "clone", "--branch", r.branch, r.origin, ".")
	r.git(other, "-c", "user.email=l@example.com", "-c", "user.name=Lane", "commit", "--allow-empty", "-m", "fix: pushed meanwhile")
	r.git(other, "push", "origin", r.branch)
	theirs := r.git(r.origin, "rev-parse", "refs/heads/"+r.branch)

	require.NoError(t, os.WriteFile(filepath.Join(r.anvil, warden.RulesFileName), []byte("rules: []\n"), 0o644))
	err := s.commitAndPush(ctx, r.anvil, r.branch, PassResults{Added: []string{"x"}}, true, "")
	require.Error(t, err)
	assert.Equal(t, theirs, r.git(r.origin, "rev-parse", "refs/heads/"+r.branch), "their commit is still the head")
}
