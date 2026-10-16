package rebase

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// newPushRepo: main and a PR branch on a bare origin, main moved on after the
// PR branched (so there is something to rebase onto), and a worktree on the
// PR branch.
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
	r.write(seed, "pr.txt", "the PR")
	r.git(seed, "push", "origin", r.branch)
	r.git(seed, "checkout", "main")
	r.write(seed, "main.txt", "main moved on")
	r.git(seed, "push", "origin", "main")
	r.git(r.worktree, "clone", "--branch", r.branch, r.origin, ".")
	return r
}

func (r pushRepo) write(dir, name, msg string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(msg+"\n"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git(dir, "add", name)
	r.git(dir, "commit", "-m", msg)
}

func (r pushRepo) pushFromElsewhere(name, msg string) string {
	other := r.t.TempDir()
	r.git(other, "clone", "--branch", r.branch, r.origin, ".")
	r.write(other, name, msg)
	r.git(other, "push", "origin", r.branch)
	return r.head()
}

func (r pushRepo) head() string { return r.git(r.origin, "rev-parse", "refs/heads/"+r.branch) }

func (r pushRepo) params() *Params {
	return &Params{WorktreePath: r.worktree, Branch: r.branch, BaseBranch: "main", PRNumber: 7}
}

// What Smith now does, and nothing more: rebase, no push.
func (r pushRepo) rebase() {
	r.git(r.worktree, "fetch", "origin")
	r.git(r.worktree, "rebase", "origin/main")
}

func TestPushRebased_PushesTheRebaseLeasedToTheStartHead(t *testing.T) {
	r := newPushRepo(t)
	p := r.params()
	start, err := p.startFromRemote()
	if err != nil {
		t.Fatal(err)
	}
	r.rebase()
	if err := p.pushRebased(start); err != nil {
		t.Fatalf("an unchallenged rebase must push: %v", err)
	}
	if got := r.head(); got != r.git(r.worktree, "rev-parse", "HEAD") {
		t.Errorf("origin head = %s, want the rebased HEAD", got)
	}
}

// The case the plain --force fallback broke: a lane pushed while the rebase ran.
func TestPushRebased_NeverOverwritesAPushMadeMeanwhile(t *testing.T) {
	r := newPushRepo(t)
	p := r.params()
	start, err := p.startFromRemote()
	if err != nil {
		t.Fatal(err)
	}
	theirs := r.pushFromElsewhere("lane.txt", "a lane's fix")
	r.rebase()
	if err := p.pushRebased(start); err == nil {
		t.Fatal("the push must be refused")
	}
	if got := r.head(); got != theirs {
		t.Errorf("origin head = %s, want the lane's commit %s", got, theirs)
	}
}

func TestPushRebased_StoppedBellowsSkipsThePush(t *testing.T) {
	r := newPushRepo(t)
	p := r.params()
	p.Stopped = func() bool { return true }
	before := r.head()
	start, err := p.startFromRemote()
	if err != nil {
		t.Fatal(err)
	}
	r.rebase()
	if err := p.pushRebased(start); !errors.Is(err, ErrBellowsStopped) {
		t.Fatalf("error = %v, want ErrBellowsStopped", err)
	}
	if got := r.head(); got != before {
		t.Errorf("origin head moved to %s after the stop", got)
	}
}

// A worktree behind origin (a commit pushed before the job) is fast-forwarded,
// so the rebase carries that commit instead of dropping it.
func TestStartFromRemote_BehindIsFastForwarded(t *testing.T) {
	r := newPushRepo(t)
	earlier := r.pushFromElsewhere("earlier.txt", "pushed before the job")
	r.git(r.worktree, "fetch", "origin")
	start, err := r.params().startFromRemote()
	if err != nil {
		t.Fatal(err)
	}
	if start != earlier || r.git(r.worktree, "rev-parse", "HEAD") != earlier {
		t.Errorf("start=%s HEAD=%s, want both %s", start, r.git(r.worktree, "rev-parse", "HEAD"), earlier)
	}
}

func TestStartFromRemote_DivergedIsRefused(t *testing.T) {
	r := newPushRepo(t)
	r.write(r.worktree, "local.txt", "only in the worktree")
	r.pushFromElsewhere("remote.txt", "only on origin")
	r.git(r.worktree, "fetch", "origin")
	if _, err := r.params().startFromRemote(); err == nil {
		t.Fatal("a diverged worktree must be refused")
	}
}

func TestBuildRebasePrompt_SmithDoesNotPush(t *testing.T) {
	prompt := buildRebasePrompt("/wt", "forge/x", "main")
	for _, bad := range []string{"--force origin", "push --force-with-lease"} {
		if strings.Contains(prompt, bad) {
			t.Errorf("prompt still contains %q", bad)
		}
	}
	if !strings.Contains(prompt, "Do NOT push") {
		t.Error("prompt must tell Smith not to push")
	}
}

// Smith pushed despite the prompt: origin already holds HEAD, which is success,
// not a refused lease.
func TestPushRebased_SmithAlreadyPushedIsDone(t *testing.T) {
	r := newPushRepo(t)
	p := r.params()
	start, err := p.startFromRemote()
	if err != nil {
		t.Fatal(err)
	}
	r.rebase()
	r.git(r.worktree, "push", "--force", "origin", "HEAD:refs/heads/"+r.branch)
	if err := p.pushRebased(start); err != nil {
		t.Fatalf("origin already holds HEAD; want nil, got %v", err)
	}
}
