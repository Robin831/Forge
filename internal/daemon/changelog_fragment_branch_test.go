package daemon

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/changelog"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// technicalOnlyStems is the fragment set a bead whose change is technical-only
// carries — the exact shape Fhi.Metadata-hwbwz had on 2026-09-07.
var technicalOnlyStems = []string{"-technical.en", "-technical.nb"}

// branchHeadSHA resolves a local branch to its commit SHA.
func branchHeadSHA(t *testing.T, anvilPath, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", branch)
	cmd.Dir = anvilPath
	cmd.Env = cleanGitTestEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// TestBranchHasChangelogFragment_TechnicalOnlyPair drives the probe itself
// against a real branch whose only fragments are <bead>-technical.en.md and
// <bead>-technical.nb.md. This is the level the Fhi.Metadata-hwbwz incident
// failed at: the files were on origin and the probe reported no completion
// signal, so complete work was headed for needs_human.
func TestBranchHasChangelogFragment_TechnicalOnlyPair(t *testing.T) {
	const beadID = "Fhi.Metadata-hwbwz"
	anvilPath := initTestGitRepo(t)
	branch := worktree.BranchName(beadID)
	pushForgeBranch(t, anvilPath, beadID, true, technicalOnlyStems...)

	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sha := branchHeadSHA(t, anvilPath, branch)

	found, _, err := d.branchHasChangelogFragment(context.Background(), anvilPath, sha, beadID, changelog.FragmentRule{})
	require.NoError(t, err)
	assert.True(t, found, "a -technical language pair is a complete fragment set and must read as a completion signal")

	// A different bead sharing this one's id as a prefix must not be satisfied
	// by these files.
	found, _, err = d.branchHasChangelogFragment(context.Background(), anvilPath, sha, beadID+"1", changelog.FragmentRule{})
	require.NoError(t, err)
	assert.False(t, found, "a bead whose id merely extends another's must not match its fragments")
}

// TestBranchHasChangelogFragment_ChildBeadFragment is the mirror case, and the
// reason the delimiter alone is not the rule. bd's hierarchical ids are
// <parent>.<n>, and this probe reads the branch tip's WHOLE tree — so a child's
// fragment merged to main weeks ago is still in changelog.d/ on the parent's
// stranded branch. Read as the parent's own completion signal it would have
// preDispatchRemoteBranchCheck open a PR for a parent whose work never
// happened, which is exactly what the guard exists to prevent.
func TestBranchHasChangelogFragment_ChildBeadFragment(t *testing.T) {
	const parentID = "Fhi.Metadata-n1g"
	childID := parentID + ".7"

	anvilPath := initTestGitRepo(t)
	branch := worktree.BranchName(parentID)
	// The branch carries the CHILD's fragments and nothing for the parent.
	pushForgeBranch(t, anvilPath, parentID, true, ".7", ".7-technical.en")

	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sha := branchHeadSHA(t, anvilPath, branch)

	found, _, err := d.branchHasChangelogFragment(context.Background(), anvilPath, sha, parentID, changelog.FragmentRule{})
	require.NoError(t, err)
	assert.False(t, found, "a child bead's fragment is not the parent's completion signal")

	found, _, err = d.branchHasChangelogFragment(context.Background(), anvilPath, sha, childID, changelog.FragmentRule{})
	require.NoError(t, err)
	assert.True(t, found, "the child's own fragments must still read as its completion signal")
}

// TestPreDispatchRecoversTechnicalOnlyStrandedBranch is the incident end to end
// at the level the acceptance criterion names: the daemon's pre-dispatch guard
// meets a stranded branch carrying only the -technical pair and must open its
// PR rather than escalate to needs_human.
func TestPreDispatchRecoversTechnicalOnlyStrandedBranch(t *testing.T) {
	const anvilName = "test-anvil"
	const beadID = "Fhi.Metadata-hwbwz"

	anvilPath := initTestGitRepo(t)
	db, err := state.Open(filepath.Join(anvilPath, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	bead := poller.Bead{ID: beadID, Anvil: anvilName, Title: "test-infra fix"}
	mockVCS := &mockVCSProvider{
		createPRResult: &vcs.PR{Number: 5635, URL: "https://example.com/pr/5635", Title: bead.Title},
	}
	d := &Daemon{
		db:           db,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:  worktree.NewManager(),
		vcsProviders: map[string]vcs.Provider{anvilName: mockVCS},
	}
	d.cfg.Store(&config.Config{})

	pushForgeBranch(t, anvilPath, bead.ID, true, technicalOnlyStems...)

	if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
		t.Fatal("expected dispatch to be skipped after auto-opening the recovered PR")
	}

	require.Equal(t, int32(1), mockVCS.createPRCalls.Load(),
		"the stranded branch carries a completion signal, so its PR must be opened")

	r, err := db.GetRetry(bead.ID, bead.Anvil)
	require.NoError(t, err)
	if r != nil && r.NeedsHuman {
		t.Error("completed technical-only work must not be escalated to needs_human")
	}

	pr, err := db.GetPRByNumber(bead.Anvil, 5635)
	require.NoError(t, err)
	require.NotNil(t, pr, "the recovered PR must be registered so bellows owns it")
	assert.Equal(t, bead.ID, pr.BeadID)

	events, err := db.RecentEvents(20)
	require.NoError(t, err)
	assert.True(t, hasEventType(events, state.EventDispatchRecoveredStrandedBranch),
		"expected the stranded-branch recovery event")
}
