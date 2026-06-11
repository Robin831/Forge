package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushForgeBranch creates a forge/<bead> branch on origin carrying one commit.
// When withFragment is true the commit includes changelog.d/<bead>.md, the
// per-PR completion signal openPRForExistingBranch requires.
func pushForgeBranch(t *testing.T, anvilPath, beadID string, withFragment bool) {
	t.Helper()
	branch := worktree.BranchName(beadID)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = anvilPath
		cmd.Env = cleanGitTestEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git("checkout", "-b", branch)
	require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "work.txt"), []byte("work\n"), 0o644))
	git("add", "work.txt")
	if withFragment {
		require.NoError(t, os.MkdirAll(filepath.Join(anvilPath, "changelog.d"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(anvilPath, "changelog.d", beadID+".md"),
			[]byte("category: Added\n- **Thing** - did a thing. ("+beadID+")\n"), 0o644))
		git("add", filepath.Join("changelog.d", beadID+".md"))
	}
	git("commit", "-m", "stranded work")
	git("push", "origin", branch)
	git("checkout", "main")
}

// newCreatePRTestDaemon builds a Daemon wired to a real temp git repo and state
// DB, a mock VCS provider, and a canned bead fetcher so openPRForExistingBranch
// can be exercised without bd or a live GitHub.
func newCreatePRTestDaemon(t *testing.T, anvil, anvilPath string, mock *mockVCSProvider) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	d := &Daemon{
		db:           db,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:  worktree.NewManager(),
		vcsProviders: map[string]vcs.Provider{anvil: mock},
		beadFetcher: func(_ context.Context, beadID, _ string) (poller.Bead, error) {
			return poller.Bead{ID: beadID, Title: "Recover me", Description: "desc", IssueType: "task"}, nil
		},
	}
	d.cfg.Store(&config.Config{Anvils: map[string]config.AnvilConfig{
		anvil: {Path: anvilPath},
	}})
	return d, db
}

func TestOpenPRForExistingBranch_Success(t *testing.T) {
	const anvil = "test-anvil"
	const beadID = "Forge-rec1"
	anvilPath := initTestGitRepo(t)
	pushForgeBranch(t, anvilPath, beadID, true)

	mock := &mockVCSProvider{
		createPRResult: &vcs.PR{Number: 77, URL: "https://example.test/pr/77", Title: "Recover me"},
	}
	d, db := newCreatePRTestDaemon(t, anvil, anvilPath, mock)

	// Pre-seed needs_human so we can assert it is cleared on recovery.
	require.NoError(t, db.MarkNeedsHuman(beadID, anvil, "PR creation failed"))

	prNum, prURL, err := d.openPRForExistingBranch(context.Background(), beadID, anvil)
	require.NoError(t, err)
	assert.Equal(t, 77, prNum)
	assert.Equal(t, "https://example.test/pr/77", prURL, "the freshly created PR's URL should be returned for the Create PR button link")
	assert.Equal(t, int32(1), mock.createPRCalls.Load(), "CreatePR should be called exactly once")

	// PR registered in state.db.
	pr, err := db.GetPRByNumber(anvil, 77)
	require.NoError(t, err)
	require.NotNil(t, pr, "PR should be registered for bellows to own")
	assert.Equal(t, beadID, pr.BeadID)

	// needs_human cleared.
	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	assert.Nil(t, r, "needs_human/retry record should be cleared after recovery")

	// Recovery event logged.
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	assert.True(t, hasEventType(events, state.EventPRCreateRecovered), "expected pr_create_recovered event")
}

func TestOpenPRForExistingBranch_MissingBranch(t *testing.T) {
	const anvil = "test-anvil"
	const beadID = "Forge-nob"
	anvilPath := initTestGitRepo(t) // no forge branch pushed

	mock := &mockVCSProvider{}
	d, _ := newCreatePRTestDaemon(t, anvil, anvilPath, mock)

	_, _, err := d.openPRForExistingBranch(context.Background(), beadID, anvil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.Equal(t, int32(0), mock.createPRCalls.Load(), "CreatePR must not be called when branch is absent")
}

func TestOpenPRForExistingBranch_MissingChangelogFragment(t *testing.T) {
	const anvil = "test-anvil"
	const beadID = "Forge-nofrag"
	anvilPath := initTestGitRepo(t)
	pushForgeBranch(t, anvilPath, beadID, false) // ahead of base but no fragment

	mock := &mockVCSProvider{}
	d, db := newCreatePRTestDaemon(t, anvil, anvilPath, mock)
	require.NoError(t, db.MarkNeedsHuman(beadID, anvil, "PR creation failed"))

	_, _, err := d.openPRForExistingBranch(context.Background(), beadID, anvil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changelog fragment")
	assert.Equal(t, int32(0), mock.createPRCalls.Load(), "CreatePR must not be called without a completion signal")

	// needs_human must remain set on a refused recovery.
	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "needs_human must remain set when recovery is refused")
}

func TestOpenPRForExistingBranch_ExistingOpenPR(t *testing.T) {
	const anvil = "test-anvil"
	const beadID = "Forge-haspr"
	anvilPath := initTestGitRepo(t)
	pushForgeBranch(t, anvilPath, beadID, true)

	branch := worktree.BranchName(beadID)
	mock := &mockVCSProvider{
		openPRs: []vcs.OpenPR{{Number: 99, Title: "Already open", Branch: branch}},
	}
	d, db := newCreatePRTestDaemon(t, anvil, anvilPath, mock)
	require.NoError(t, db.MarkNeedsHuman(beadID, anvil, "PR creation failed"))

	prNum, _, err := d.openPRForExistingBranch(context.Background(), beadID, anvil)
	require.NoError(t, err, "an existing open PR should be treated as a successful recovery")
	assert.Equal(t, 99, prNum)
	assert.Equal(t, int32(0), mock.createPRCalls.Load(), "CreatePR must not be called when a PR already exists")

	// The existing PR is registered and needs_human cleared.
	pr, err := db.GetPRByNumber(anvil, 99)
	require.NoError(t, err)
	require.NotNil(t, pr)
	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	assert.Nil(t, r, "needs_human should be cleared once the existing PR is registered")
}

func TestOpenPRForExistingBranch_CreatePRFailureLeavesNeedsHuman(t *testing.T) {
	const anvil = "test-anvil"
	const beadID = "Forge-ghfail"
	anvilPath := initTestGitRepo(t)
	pushForgeBranch(t, anvilPath, beadID, true)

	mock := &mockVCSProvider{
		createPRErr: fmt.Errorf("gh: 422 protected branch"),
	}
	d, db := newCreatePRTestDaemon(t, anvil, anvilPath, mock)
	require.NoError(t, db.MarkNeedsHuman(beadID, anvil, "PR creation failed"))

	_, _, err := d.openPRForExistingBranch(context.Background(), beadID, anvil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected branch")

	// needs_human must remain set so the bead stays in the attention view.
	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "needs_human must remain set when gh pr create fails")
}

// pushEpicChildBranch creates an epic feature branch on origin and a forge child
// branch that is one commit ahead of it (carrying the bead's changelog fragment).
// This mirrors a Crucible child whose PR must target the feature branch, not main.
func pushEpicChildBranch(t *testing.T, anvilPath, beadID, featureBranch string) {
	t.Helper()
	childBranch := worktree.BranchName(beadID)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = anvilPath
		cmd.Env = cleanGitTestEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	// Feature branch off main, pushed to origin so it can serve as the PR base.
	git("checkout", "-b", featureBranch)
	git("push", "origin", featureBranch)
	// Child branch off the feature branch, one commit ahead, with the fragment.
	git("checkout", "-b", childBranch)
	require.NoError(t, os.MkdirAll(filepath.Join(anvilPath, "changelog.d"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(anvilPath, "changelog.d", beadID+".md"),
		[]byte("category: Added\n- **Thing** - did a thing. ("+beadID+")\n"), 0o644))
	git("add", filepath.Join("changelog.d", beadID+".md"))
	git("commit", "-m", "child work")
	git("push", "origin", childBranch)
	git("checkout", "main")
}

// TestOpenPRForExistingBranch_ResolvesEpicBranch verifies that a Crucible child
// recovered via openPRForExistingBranch gets its PR targeted at the resolved
// epic feature branch rather than the repo default. FetchBead does not populate
// EpicBranch, so the helper must resolve it via poller.ResolveEpicBranches.
func TestOpenPRForExistingBranch_ResolvesEpicBranch(t *testing.T) {
	const anvil = "test-anvil"
	const beadID = "Forge-child1"
	const epicID = "Forge-epic1"
	const featureBranch = "feature/Forge-epic1"
	anvilPath := initTestGitRepo(t)
	pushEpicChildBranch(t, anvilPath, beadID, featureBranch)

	// Resolve the epic branch without shelling out to bd.
	restore := poller.SetEpicBranchLookupForTest(func(_ context.Context, parentID, _ string) string {
		if parentID == epicID {
			return featureBranch
		}
		return ""
	})
	defer restore()

	mock := &mockVCSProvider{
		createPRResult: &vcs.PR{Number: 55, URL: "https://example.test/pr/55", Title: "Recover child"},
	}
	d, db := newCreatePRTestDaemon(t, anvil, anvilPath, mock)
	// Override the canned fetcher with one returning a child that blocks the epic.
	d.beadFetcher = func(_ context.Context, id, _ string) (poller.Bead, error) {
		return poller.Bead{ID: id, Title: "Recover child", Description: "desc", IssueType: "task", Blocks: []string{epicID}}, nil
	}
	require.NoError(t, db.MarkNeedsHuman(beadID, anvil, "PR creation failed"))

	prNum, _, err := d.openPRForExistingBranch(context.Background(), beadID, anvil)
	require.NoError(t, err)
	assert.Equal(t, 55, prNum)
	assert.Equal(t, featureBranch, mock.lastCreateParams.Base,
		"a Crucible child's recovered PR must target the resolved epic feature branch")
}

func TestOpenPRForExistingBranch_UnknownAnvil(t *testing.T) {
	mock := &mockVCSProvider{}
	d, _ := newCreatePRTestDaemon(t, "test-anvil", initTestGitRepo(t), mock)

	_, _, err := d.openPRForExistingBranch(context.Background(), "Forge-x", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func hasEventType(events []state.Event, typ state.EventType) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}
