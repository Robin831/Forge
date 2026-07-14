package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newResumeAnvil builds a bare-remote-backed anvil clone checked out on main,
// with a forge/<beadID> branch carrying one extra commit pushed to origin. It
// returns the anvil path. This simulates the state a needs-attention bead is in
// after its worktree was torn down but the branch survives: the recreate-from-
// branch resume path (worktree.CreateFromBranch) rebuilds the worktree from it.
func newResumeAnvil(t *testing.T, beadID string) string {
	t.Helper()
	env := cleanGitEnv()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	remote := t.TempDir()
	git(remote, "init", "--bare", "--initial-branch=main")

	anvil := t.TempDir()
	git(anvil, "clone", remote, ".")
	git(anvil, "config", "user.email", "test@example.com")
	git(anvil, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(anvil, "README"), []byte("init\n"), 0o644))
	git(anvil, "add", "README")
	git(anvil, "commit", "-m", "init")
	git(anvil, "push", "origin", "main")

	// Create the surviving forge/<bead> branch with a commit, push it, then
	// return to main (the anvil must be on main for worktree creation to proceed).
	branch := worktree.BranchName(beadID)
	git(anvil, "checkout", "-b", branch)
	require.NoError(t, os.WriteFile(filepath.Join(anvil, "work.txt"), []byte("smith work\n"), 0o644))
	git(anvil, "add", "work.txt")
	git(anvil, "commit", "-m", "smith work")
	git(anvil, "push", "-u", "origin", branch)
	git(anvil, "checkout", "main")

	return anvil
}

// deleteBranchEverywhere removes the forge/<bead> branch locally and on origin,
// simulating the case where the surviving branch was merged and pruned so the
// resume must downgrade to a fresh session.
func deleteResumeBranch(t *testing.T, anvil, beadID string) {
	t.Helper()
	env := cleanGitEnv()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", anvil}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	branch := worktree.BranchName(beadID)
	git("branch", "-D", branch)
	git("push", "origin", "--delete", branch)
}

// TestResumeWithMessage_RecreatesWorktreeFromBranch exercises the core
// needs-attention resume path end-to-end with a real git anvil: the worktree
// directory is absent but the forge/<bead> branch survives. The pipeline must
// recreate the worktree at its exact original path from that branch
// (worktree.CreateFromBranch) and resume the recorded session there.
func TestResumeWithMessage_RecreatesWorktreeFromBranch(t *testing.T) {
	const beadID = "test-bead"
	anvil := newResumeAnvil(t, beadID)
	mgr := worktree.NewManager()
	wantPath := mgr.WorktreePath(anvil, beadID)

	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"
	params.Bead = poller.Bead{ID: beadID, Title: "Resumable bead"}
	params.AnvilConfig = config.AnvilConfig{Path: anvil}
	// Use the real worktree manager (no WorktreeCreator override) so the
	// recreate-from-branch path actually runs.
	params.WorktreeCreator = nil
	params.WorktreeRemover = nil
	params.WorktreeManager = mgr

	var mu sync.Mutex
	var resumeWtPath, gotSessionID string
	var worktreeHadWork bool
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, wtPath, _, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		// Observe the worktree WHILE it exists — the pipeline tears it down on the
		// way out, so a post-Run stat would race the cleanup.
		_, statErr := os.Stat(filepath.Join(wtPath, "work.txt"))
		mu.Lock()
		resumeWtPath = wtPath
		gotSessionID = sessionID
		worktreeHadWork = statErr == nil
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-resumed", ResultSubtype: "success"}), nil
	}
	// A fresh spawn must NOT run when the branch survives and the resume succeeds.
	var freshSpawns int32
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&freshSpawns, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, ResultSubtype: "success"}), nil
	}
	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	params.ResumeSession = &ResumeSession{
		SessionID:          "sess-parked",
		Provider:           provider.Provider{Kind: provider.Claude},
		Message:            "finish the job",
		RecreateFromBranch: true,
		Branch:             worktree.BranchName(beadID),
		WorktreePath:       wantPath,
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success, "resume in the recreated worktree should succeed")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the recorded session must be resumed once")
	assert.EqualValues(t, 0, atomic.LoadInt32(&freshSpawns), "a surviving branch + successful resume must not spawn fresh")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, wantPath, resumeWtPath, "resume must run in the worktree recreated at the original path")
	assert.Equal(t, "sess-parked", gotSessionID, "resume must reuse the recorded session_id")
	assert.True(t, worktreeHadWork, "the recreated worktree must be a checkout of the surviving branch (work.txt present)")
}

// TestResumeWithMessage_BranchGoneDowngradesToFresh verifies that when the
// surviving branch is gone, the pipeline recreates a fresh worktree from base
// and downgrades to a fresh Smith session seeded with the operator message,
// rather than attempting an impossible resume.
func TestResumeWithMessage_BranchGoneDowngradesToFresh(t *testing.T) {
	const beadID = "test-bead"
	anvil := newResumeAnvil(t, beadID)
	deleteResumeBranch(t, anvil, beadID)
	mgr := worktree.NewManager()

	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"
	params.Bead = poller.Bead{ID: beadID, Title: "Resumable bead"}
	params.AnvilConfig = config.AnvilConfig{Path: anvil}
	params.WorktreeCreator = nil
	params.WorktreeRemover = nil
	params.WorktreeManager = mgr

	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, ResultSubtype: "success"}), nil
	}
	var mu sync.Mutex
	var freshPrompts []string
	var freshSpawns int32
	params.SmithRunner = func(_ context.Context, _, promptText, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&freshSpawns, 1)
		mu.Lock()
		freshPrompts = append(freshPrompts, promptText)
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-fresh", ResultSubtype: "success"}), nil
	}
	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	params.ResumeSession = &ResumeSession{
		SessionID:          "sess-parked",
		Provider:           provider.Provider{Kind: provider.Claude},
		Message:            "rebuild it cleanly",
		RecreateFromBranch: true,
		Branch:             worktree.BranchName(beadID),
		WorktreePath:       mgr.WorktreePath(anvil, beadID),
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success)
	assert.EqualValues(t, 0, atomic.LoadInt32(&resumeCalls), "a gone branch means no session to resume")
	assert.EqualValues(t, 1, atomic.LoadInt32(&freshSpawns), "a gone branch must downgrade to a fresh session")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, freshPrompts, 1)
	assert.Contains(t, freshPrompts[0], "rebuild it cleanly", "the fresh session must be seeded with the operator message")
}

// TestResumeWithMessage_FallsBackToFreshOnMissingTranscript exercises the
// needs-attention resume path when the recorded session can no longer be
// resumed: `claude --resume` reports the transcript missing. The pipeline must
// NOT fail the worker — it folds the operator message into the fresh bd-context
// prompt and runs a normal Smith session on the same iteration, then continues
// through Temper → Warden to success.
func TestResumeWithMessage_FallsBackToFreshOnMissingTranscript(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	// The resume attempt reports a missing transcript (ResumeUnavailable).
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		return smith.NewProcessForTest(&smith.Result{
			ExitCode:      1,
			ResultSubtype: "error",
			ErrorOutput:   "No conversation found with session ID: sess-gone",
		}), nil
	}

	// The fresh fallback spawn succeeds; capture the prompt it receives.
	var mu sync.Mutex
	var freshPrompts []string
	var freshSpawns int32
	params.SmithRunner = func(_ context.Context, _, promptText, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&freshSpawns, 1)
		mu.Lock()
		freshPrompts = append(freshPrompts, promptText)
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-fresh", ResultSubtype: "success"}), nil
	}

	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	params.ResumeSession = &ResumeSession{
		SessionID: "sess-gone",
		Provider:  provider.Provider{Kind: provider.Claude},
		Message:   "please fix the failing test",
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success, "a failed resume must fall back to a fresh session, not fail the worker")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the resume must be attempted exactly once")
	assert.EqualValues(t, 1, atomic.LoadInt32(&freshSpawns), "the fresh-session fallback must run exactly once")
	assert.Equal(t, 1, outcome.Iterations, "the fresh fallback runs on the same first iteration")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, freshPrompts, 1)
	assert.Contains(t, freshPrompts[0], "please fix the failing test",
		"the fresh fallback prompt must be seeded with the operator message (plus bd context)")

	w, err := db.GetWorker(outcome.WorkerID)
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerFailed, w.Status, "a resume that fell back must not mark the worker failed")
}

// TestResumeWithMessage_SuccessfulResumeDoesNotFallBack verifies that when the
// recorded session resumes cleanly, the fresh-session fallback does NOT run:
// the resume is the sole spawn for the first iteration.
func TestResumeWithMessage_SuccessfulResumeDoesNotFallBack(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	var mu sync.Mutex
	var gotSessionID, gotMsg string
	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, msg, _ string, _ provider.Provider, sessionID string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		mu.Lock()
		gotSessionID = sessionID
		gotMsg = msg
		mu.Unlock()
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-resumed", ResultSubtype: "success"}), nil
	}

	var freshSpawns int32
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&freshSpawns, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, ResultSubtype: "success"}), nil
	}

	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	params.ResumeSession = &ResumeSession{
		SessionID: "sess-live",
		Provider:  provider.Provider{Kind: provider.Claude},
		Message:   "keep going",
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success)
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls), "the session must be resumed exactly once")
	assert.EqualValues(t, 0, atomic.LoadInt32(&freshSpawns), "a successful resume must not trigger the fresh fallback")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "sess-live", gotSessionID)
	assert.Equal(t, "keep going", gotMsg)
}

// TestResumeWithMessage_FallsBackWhenResumeSpawnErrors verifies that a resume
// whose spawn fails to even start (e.g. the provider binary rejects --resume)
// also falls back to a fresh session rather than failing the worker.
func TestResumeWithMessage_FallsBackWhenResumeSpawnErrors(t *testing.T) {
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.WorkerID = "test-worker"

	var resumeCalls int32
	params.SmithResumeRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ string, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&resumeCalls, 1)
		return nil, assert.AnError
	}

	var freshSpawns int32
	params.SmithRunner = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		atomic.AddInt32(&freshSpawns, 1)
		return smith.NewProcessForTest(&smith.Result{ExitCode: 0, SessionID: "sess-fresh", ResultSubtype: "success"}), nil
	}

	params.EmptyDiffChecker = func(_, _ string) bool { return false }
	params.WardenReviewer = func(_ context.Context, _, _, _, _, _ string, _ *state.DB, _ string, _ ...provider.Provider) (*warden.ReviewResult, error) {
		return &warden.ReviewResult{Verdict: warden.VerdictApprove, Summary: "LGTM"}, nil
	}

	params.ResumeSession = &ResumeSession{
		SessionID: "sess-x",
		Provider:  provider.Provider{Kind: provider.Claude},
		Message:   "continue",
	}

	outcome := Run(context.Background(), params)

	require.NoError(t, outcome.Error)
	assert.True(t, outcome.Success, "a resume spawn error must fall back to fresh, not fail the worker")
	assert.EqualValues(t, 1, atomic.LoadInt32(&resumeCalls))
	assert.EqualValues(t, 1, atomic.LoadInt32(&freshSpawns))
}
