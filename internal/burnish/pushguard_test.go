package burnish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pushRepo is a bare origin with a PR branch, and a worktree cloned from it on
// that branch: what a review fix pushes from.
type pushRepo struct {
	t        *testing.T
	origin   string
	worktree string
	branch   string
}

func (r pushRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newPushRepo(t *testing.T) pushRepo {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}
	r := pushRepo{t: t, origin: t.TempDir(), worktree: t.TempDir(), branch: "forge/test-pr"}
	r.git(r.origin, "init", "--bare", "--initial-branch=main")
	seed := t.TempDir()
	r.git(seed, "clone", r.origin, ".")
	r.git(seed, "commit", "--allow-empty", "-m", "init")
	r.git(seed, "push", "origin", "main")
	r.git(seed, "checkout", "-b", r.branch)
	r.git(seed, "commit", "--allow-empty", "-m", "the PR")
	r.git(seed, "push", "origin", r.branch)
	r.git(r.worktree, "clone", "--branch", r.branch, r.origin, ".")
	return r
}

// pushFromElsewhere is a lane (or a person) pushing to the PR branch.
func (r pushRepo) pushFromElsewhere(msg string) string {
	other := r.t.TempDir()
	r.git(other, "clone", "--branch", r.branch, r.origin, ".")
	if err := os.WriteFile(filepath.Join(other, "lane.txt"), []byte(msg), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git(other, "add", "lane.txt")
	r.git(other, "commit", "-m", msg)
	r.git(other, "push", "origin", r.branch)
	return r.git(r.origin, "rev-parse", "refs/heads/"+r.branch)
}

func (r pushRepo) head() string { return r.git(r.origin, "rev-parse", "refs/heads/"+r.branch) }

func TestGitPush_FastForwardIsPushed(t *testing.T) {
	r := newPushRepo(t)
	r.git(r.worktree, "commit", "--allow-empty", "-m", "review fix")
	fix := r.git(r.worktree, "rev-parse", "HEAD")
	if err := gitPush(context.Background(), r.worktree, r.branch); err != nil {
		t.Fatalf("a fix built on origin's head must push: %v", err)
	}
	if got := r.head(); got != fix {
		t.Errorf("origin head = %s, want the fix %s", got, fix)
	}
}

// The incident (PR 6723): a lane pushed while the fix ran, and the fix was
// force-pushed over it. Now the push is refused and the lane's commit stays.
func TestGitPush_NeverOverwritesACommitTheFixIsNotBuiltOn(t *testing.T) {
	r := newPushRepo(t)
	r.git(r.worktree, "commit", "--allow-empty", "-m", "review fix on a stale base")
	theirs := r.pushFromElsewhere("a lane's fix")

	err := gitPush(context.Background(), r.worktree, r.branch)
	if err == nil || !strings.Contains(err.Error(), "not built on") {
		t.Fatalf("expected a refusal naming the reason, got %v", err)
	}
	if got := r.head(); got != theirs {
		t.Errorf("origin head = %s, want the lane's commit %s still there", got, theirs)
	}
}

// What the force-push was for: a reused worktree behind Forge's own earlier
// push. syncToRemote brings it up before the fix starts, so the fix pushes as
// a fast-forward without force.
func TestSyncToRemote_ThenTheFixPushesWithoutForce(t *testing.T) {
	r := newPushRepo(t)
	earlier := r.pushFromElsewhere("forge's earlier push")
	syncToRemote(context.Background(), r.worktree, r.branch, 42)
	if got := r.git(r.worktree, "rev-parse", "HEAD"); got != earlier {
		t.Fatalf("worktree HEAD = %s, want it brought up to %s", got, earlier)
	}
	r.git(r.worktree, "commit", "--allow-empty", "-m", "review fix")
	if err := gitPush(context.Background(), r.worktree, r.branch); err != nil {
		t.Fatalf("a fix built on the synced head must push: %v", err)
	}
	r.git(r.origin, "merge-base", "--is-ancestor", earlier, "refs/heads/"+r.branch)
}

// A worktree holding commits origin lacks is never reset by the sync.
func TestSyncToRemote_LeavesADivergedWorktreeAlone(t *testing.T) {
	r := newPushRepo(t)
	r.git(r.worktree, "commit", "--allow-empty", "-m", "an unpushed fix kept from a failed run")
	kept := r.git(r.worktree, "rev-parse", "HEAD")
	r.pushFromElsewhere("someone else's commit")
	syncToRemote(context.Background(), r.worktree, r.branch, 42)
	if got := r.git(r.worktree, "rev-parse", "HEAD"); got != kept {
		t.Errorf("worktree HEAD = %s, want the kept commit %s untouched", got, kept)
	}
}

// `forge bellows stop` while the fix ran: the verified fix is not pushed, it
// is kept and named, and the error says why.
func TestFix_StoppedBellowsSkipsThePush(t *testing.T) {
	for _, batch := range []bool{false, true} {
		h := newTestHarness()
		db := openTestDB(t)
		smithSpawnFn = makeSmithStub(0)
		recordTemperConfigs(true)
		gitRevParseFn = fixedRevParse("local-fix", "remote-head")
		pushed := false
		gitPushFn = func(context.Context, string, string) error { pushed = true; return nil }
		stopped := func() bool { return true }

		var result *FixResult
		if batch {
			v := changesRequested()
			result = BatchFix(context.Background(), BatchFixParams{
				WorktreePath: os.TempDir(), BeadID: "test-1", AnvilName: "test-anvil", PRNumber: 42, Branch: "forge/test",
				DB: db, Providers: defaultFixParams(db, v).Providers, Comments: v.comments, VCS: v, Stopped: stopped,
			})
		} else {
			params := defaultFixParams(db, changesRequested())
			params.Stopped = stopped
			result = Fix(context.Background(), params)
		}
		h.restore()

		if pushed {
			t.Errorf("batch=%t: pushed after bellows was stopped", batch)
		}
		if !errors.Is(result.Error, ErrBellowsStopped) {
			t.Errorf("batch=%t: error = %v, want ErrBellowsStopped", batch, result.Error)
		}
		if result.UnpushedHead != "local-fix" {
			t.Errorf("batch=%t: UnpushedHead = %q, want the kept fix named", batch, result.UnpushedHead)
		}
	}
}

// The timeout path pushes the fix unverified; a stop that landed meanwhile
// keeps it instead, on both entry points.
func TestVerifyTimeout_StoppedBellowsSkipsTheUnverifiedPush(t *testing.T) {
	for _, batch := range []bool{false, true} {
		h := newTestHarness()
		db := openTestDB(t)
		smithSpawnFn = makeSmithStub(0)
		temperCalls := 0
		temperRunFn = blockingTemper(&temperCalls)
		gitRevParseFn = fixedRevParse("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		pushed := false
		gitPushFn = func(context.Context, string, string) error { pushed = true; return nil }
		stopped := func() bool { return true }

		var result *FixResult
		v := changesRequested()
		if batch {
			result = BatchFix(context.Background(), BatchFixParams{
				WorktreePath: os.TempDir(), BeadID: "test-1", AnvilName: "test-anvil", PRNumber: 42, Branch: "forge/test",
				DB: db, Providers: defaultFixParams(db, v).Providers, Comments: v.comments, VCS: v, Stopped: stopped,
				VerifyTimeout: 50 * time.Millisecond, VerifyRetries: -1,
			})
		} else {
			params := defaultFixParams(db, v)
			params.Stopped = stopped
			params.VerifyTimeout = 50 * time.Millisecond
			params.VerifyRetries = -1
			result = Fix(context.Background(), params)
		}
		h.restore()

		if pushed {
			t.Errorf("batch=%t: pushed unverified after bellows was stopped", batch)
		}
		if !errors.Is(result.Error, ErrBellowsStopped) {
			t.Errorf("batch=%t: error = %v, want ErrBellowsStopped", batch, result.Error)
		}
		if result.UnpushedHead == "" {
			t.Errorf("batch=%t: the kept fix is not named", batch)
		}
	}
}

// A fix Smith already pushed is on the PR: a stop after that changes nothing
// and the run is not reported as unpushed.
func TestFix_StoppedAfterSmithPushedIsNotUnpushed(t *testing.T) {
	h := newTestHarness()
	defer h.restore()
	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	recordTemperConfigs(true)
	gitRevParseFn = fixedRevParse("same-head", "same-head")
	gitPushFn = func(context.Context, string, string) error { t.Error("no push expected"); return nil }
	params := defaultFixParams(db, changesRequested())
	params.Stopped = func() bool { return true }
	result := Fix(context.Background(), params)
	if errors.Is(result.Error, ErrBellowsStopped) || result.UnpushedHead != "" {
		t.Errorf("error = %v, UnpushedHead = %q; want neither for a fix already on the PR", result.Error, result.UnpushedHead)
	}
}
