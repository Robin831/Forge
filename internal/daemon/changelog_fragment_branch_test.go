package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitInFragmentTestRepo runs one git command in the test anvil with the outer
// worker's GIT_* worktree vars stripped.
func gitInFragmentTestRepo(t *testing.T, anvilPath string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = anvilPath
	cmd.Env = cleanGitTestEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// pushTechnicalOnlyFragmentBranch pushes a stranded forge/<bead> branch whose
// only changelog fragments are the -technical language pair — the exact shape
// Fhi.Metadata-hwbwz carried on 2026-09-07, and the normal shape for a bead
// whose change is technical-only.
func pushTechnicalOnlyFragmentBranch(t *testing.T, anvilPath, branch, beadID string) {
	t.Helper()
	gitInFragmentTestRepo(t, anvilPath, "checkout", "-b", branch)
	require.NoError(t, os.MkdirAll(filepath.Join(anvilPath, "changelog.d"), 0o755))
	for _, lang := range []string{"en", "nb"} {
		require.NoError(t, os.WriteFile(
			filepath.Join(anvilPath, "changelog.d", beadID+"-technical."+lang+".md"),
			[]byte("category: Fixed\n- **Test infra** - fixed it. ("+beadID+")\n"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "impl.txt"), []byte("prior\n"), 0o644))
	gitInFragmentTestRepo(t, anvilPath, "add", "changelog.d", "impl.txt")
	gitInFragmentTestRepo(t, anvilPath, "commit", "-m", "technical-only work")
	gitInFragmentTestRepo(t, anvilPath, "push", "origin", branch)
	gitInFragmentTestRepo(t, anvilPath, "checkout", "main")
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
	pushTechnicalOnlyFragmentBranch(t, anvilPath, branch, beadID)

	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	revParse := exec.Command("git", "rev-parse", branch)
	revParse.Dir = anvilPath
	revParse.Env = cleanGitTestEnv()
	out, err := revParse.Output()
	require.NoError(t, err)
	sha := strings.TrimSpace(string(out))

	found, err := d.branchHasChangelogFragment(context.Background(), anvilPath, sha, beadID)
	require.NoError(t, err)
	assert.True(t, found, "a -technical language pair is a complete fragment set and must read as a completion signal")

	// A different bead sharing this one's id as a prefix must not be satisfied
	// by these files.
	found, err = d.branchHasChangelogFragment(context.Background(), anvilPath, sha, beadID+"1")
	require.NoError(t, err)
	assert.False(t, found, "a bead whose id merely extends another's must not match its fragments")
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
	branch := worktree.BranchName(bead.ID)
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

	pushTechnicalOnlyFragmentBranch(t, anvilPath, branch, bead.ID)

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
