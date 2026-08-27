package selfdeploy

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
)

// The pull tests drive real git repositories rather than a fake Commander.
// Every claim being made here — that a fast-forward survives a dirty tree, that
// a conflicting restore leaves the tree clean and the work in a stash, that the
// stash is provably consumed on the happy path — is a claim about what git
// actually does with a stash, and a fake that returns whatever the test needs
// would assert only that the code calls the commands the test expected it to.

// gitBin runs one git command in dir, failing the test on a non-zero exit.
func gitBin(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// executil.CleanGitEnv, not os.Environ: the test binary itself routinely runs
	// inside a git worktree with GIT_DIR/GIT_WORK_TREE exported, which would
	// answer for that repository rather than for the fixture in dir. It is the
	// same strip set ExecCommander applies, for the same reason.
	cmd.Env = append(executil.CleanGitEnv(),
		"GIT_AUTHOR_NAME=forge", "GIT_AUTHOR_EMAIL=forge@example.com",
		"GIT_COMMITTER_NAME=forge", "GIT_COMMITTER_EMAIL=forge@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"LC_ALL=C", "LANG=C",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// tryGit is gitBin for a command that is allowed to fail.
func tryGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// upstreamRepo builds a bare "origin" with one commit on main plus a clone of
// it, and returns (clone, origin-worktree) — the second being a second checkout
// the test pushes new upstream commits from.
func upstreamRepo(t *testing.T) (clone, upstream string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	gitBin(t, root, "init", "--bare", "--initial-branch=main", bare)

	upstream = filepath.Join(root, "upstream")
	gitBin(t, root, "clone", bare, upstream)
	write(t, upstream, "app.go", "package main\n\nconst version = 1\n")
	write(t, upstream, "other.go", "package main\n\nconst other = 1\n")
	gitBin(t, upstream, "add", ".")
	gitBin(t, upstream, "commit", "-m", "initial")
	gitBin(t, upstream, "push", "origin", "main")

	clone = filepath.Join(root, "forge")
	gitBin(t, root, "clone", bare, clone)
	return clone, upstream
}

// pushUpstream commits a change on the upstream checkout and pushes it, so the
// clone has something to fast-forward to.
func pushUpstream(t *testing.T, upstream, name, content string) {
	t.Helper()
	write(t, upstream, name, content)
	gitBin(t, upstream, "add", ".")
	gitBin(t, upstream, "commit", "-m", "upstream change to "+name)
	gitBin(t, upstream, "push", "origin", "main")
}

// pullDeployer wires a Deployer that only ever runs the pull half: the build,
// verify, swap and restart steps are not exercised, so it is built with the real
// ExecCommander against a real checkout.
func pullDeployer(t *testing.T, repo string) (*Deployer, *fakeSink, *fakeEmitter) {
	t.Helper()
	sink := &fakeSink{}
	em := &fakeEmitter{}
	d := New(
		Config{RepoPath: repo, BinaryPath: filepath.Join(t.TempDir(), "forge"), Branch: "main"},
		ExecCommander{}, &fakeRestarter{}, sink, nil,
		WithEmitter(em),
		WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))),
	)
	return d, sink, em
}

// stashCount reports how many entries the checkout's stash stack holds.
func stashCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := tryGit(t, repo, "stash", "list")
	if err != nil {
		t.Fatalf("git stash list: %v: %s", err, out)
	}
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// TestPullCleanTreeFastForwards is the baseline: nothing about the stash
// sequence may change what a clean checkout does. The pull still fast-forwards,
// and — the part worth pinning — no stash entry is left behind, because an entry
// created on a clean tree would be one nobody ever pops.
func TestPullCleanTreeFastForwards(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	before := gitBin(t, repo, "rev-parse", "HEAD")
	pushUpstream(t, upstream, "app.go", "package main\n\nconst version = 2\n")

	d, _, em := pullDeployer(t, repo)
	if err := d.pullSource(context.Background()); err != nil {
		t.Fatalf("pullSource on a clean tree: %v", err)
	}

	if after := gitBin(t, repo, "rev-parse", "HEAD"); after == before {
		t.Errorf("HEAD did not advance: still %s", after)
	}
	if got := read(t, repo, "app.go"); !strings.Contains(got, "version = 2") {
		t.Errorf("app.go = %q, want the upstream version", got)
	}
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("a clean pull left %d stash entries behind, want 0", n)
	}
	if len(em.events) != 0 {
		t.Errorf("a clean pull escalated %+v", em.events)
	}
}

// TestPullDirtyTreeDeploysAndRestoresTheEdit is the bug this exists to fix: a
// stray local edit used to refuse `git pull --ff-only` on every deploy from
// then on, since both sides of that condition are permanent. The pull must now
// go through AND the edit must survive it — a deploy that got the commits by
// discarding somebody's work would be a worse failure than the one it replaced.
func TestPullDirtyTreeDeploysAndRestoresTheEdit(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	before := gitBin(t, repo, "rev-parse", "HEAD")

	// Local edit to one file, upstream change to a different one: the pull is
	// refused by the local modification, but restoring it cannot conflict.
	write(t, repo, "app.go", "package main\n\nconst version = 1 // local tweak\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")

	d, _, em := pullDeployer(t, repo)
	if err := d.pullSource(context.Background()); err != nil {
		t.Fatalf("pullSource on a dirty tree: %v", err)
	}

	if after := gitBin(t, repo, "rev-parse", "HEAD"); after == before {
		t.Errorf("HEAD did not advance: still %s", after)
	}
	if got := read(t, repo, "other.go"); !strings.Contains(got, "other = 2") {
		t.Errorf("other.go = %q, want the upstream version", got)
	}
	if got := read(t, repo, "app.go"); !strings.Contains(got, "local tweak") {
		t.Errorf("app.go = %q, want the local edit restored", got)
	}
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("the deploy left %d stash entries behind, want 0", n)
	}
	if len(em.events) != 0 {
		t.Errorf("a successful dirty-tree pull escalated %+v", em.events)
	}
}

// TestPullDirtyTreeRestoresAnUntrackedFile pins the --include-untracked half:
// an incoming commit that ADDS a file the tree already holds untracked refuses
// the fast-forward exactly as a modified tracked file does, so the untracked one
// has to be set aside too — and put back.
func TestPullDirtyTreeRestoresAnUntrackedFile(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	write(t, repo, "scratch.txt", "local scratch\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")

	d, _, _ := pullDeployer(t, repo)
	if err := d.pullSource(context.Background()); err != nil {
		t.Fatalf("pullSource with an untracked file: %v", err)
	}
	if got := read(t, repo, "scratch.txt"); got != "local scratch\n" {
		t.Errorf("scratch.txt = %q, want the untracked file restored", got)
	}
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("the deploy left %d stash entries behind, want 0", n)
	}
}

// TestPullIgnoredFilesAreNotStashed keeps the deploy off the checkout's build
// output, worktrees and local config: they are ignored, git will never overwrite
// them, and stashing them would make the entry enormous for no benefit. A tree
// holding nothing but ignored files must read as clean, so no entry is created
// at all.
func TestPullIgnoredFilesAreNotStashed(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	pushUpstream(t, upstream, ".gitignore", "forge.yaml\n")
	gitBin(t, repo, "pull", "--ff-only", "origin", "main")
	write(t, repo, "forge.yaml", "settings: {}\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")

	d, _, _ := pullDeployer(t, repo)
	if err := d.pullSource(context.Background()); err != nil {
		t.Fatalf("pullSource with only ignored files modified: %v", err)
	}
	if got := read(t, repo, "forge.yaml"); got != "settings: {}\n" {
		t.Errorf("forge.yaml = %q, want the ignored file untouched", got)
	}
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("an ignored-only tree left %d stash entries, want 0", n)
	}
}

// TestPullConflictingRestoreAbortsWithTheWorkInAStash is the failure `git pull
// --ff-only --autostash` gets wrong: its pop conflicts, it warns, leaves the
// entry on the stack and exits 0, so the deploy rebuilds and restarts while the
// operator's work sits in a stash nobody mentioned.
//
// Four things must hold instead: the deploy is abandoned, the working tree is
// clean (no conflict markers) and on the pulled commit, the stash still holds
// the work, and the error names the entry so recovery is a `stash apply` rather
// than an excavation.
func TestPullConflictingRestoreAbortsWithTheWorkInAStash(t *testing.T) {
	repo, upstream := upstreamRepo(t)

	// Local and upstream edits to the SAME line: the pull succeeds off a stashed
	// clean tree, and putting the edit back is what conflicts.
	write(t, repo, "app.go", "package main\n\nconst version = 99\n")
	pushUpstream(t, upstream, "app.go", "package main\n\nconst version = 2\n")

	d, sink, em := pullDeployer(t, repo)
	err := d.pullSource(context.Background())
	if err == nil {
		t.Fatal("pullSource must abort when the restore conflicts")
	}
	if !errors.Is(err, ErrStashRetained) {
		t.Fatalf("error = %v, want it to unwrap to ErrStashRetained", err)
	}

	// The tree is clean and on the pulled commit: a build from a tree full of
	// conflict markers would not be a build of main.
	status, gitErr := tryGit(t, repo, "status", "--porcelain")
	if gitErr != nil {
		t.Fatalf("git status: %v: %s", gitErr, status)
	}
	if status != "" {
		t.Errorf("working tree not restored, git status reports:\n%s", status)
	}
	if got := read(t, repo, "app.go"); !strings.Contains(got, "version = 2") {
		t.Errorf("app.go = %q, want the pulled version with no conflict markers", got)
	}
	if strings.Contains(read(t, repo, "app.go"), "<<<<<<<") {
		t.Error("app.go still holds conflict markers")
	}

	// The work is not lost.
	if n := stashCount(t, repo); n != 1 {
		t.Fatalf("stash entries = %d, want the operator's work still stashed", n)
	}
	stashSHA := gitBin(t, repo, "rev-parse", "--verify", stashRef)
	if !strings.Contains(err.Error(), stashSHA) {
		t.Errorf("error does not name the stash entry %s: %v", stashSHA, err)
	}

	// And it is escalated with the ref and the path an operator can act on.
	ev := em.only(t)
	if ev.Reason != ReasonStashRetained {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonStashRetained)
	}
	for _, want := range []string{stashSHA, "app.go", "stash apply", repo} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("escalation detail does not mention %q:\n%s", want, ev.Detail)
		}
	}
	if !sink.has(EventFailed) {
		t.Error("a retained stash must also reach the event log")
	}
}

// TestPullFailureRestoresTheLocalEdit covers the other order: the stash is
// taken, and then the PULL is what fails. The reported failure is the pull's,
// but the tree must be left exactly as it was found — the edit back in place and
// no stash entry behind it — because a deploy that fails is not a deploy that
// gets to keep somebody's work.
func TestPullFailureRestoresTheLocalEdit(t *testing.T) {
	repo, _ := upstreamRepo(t)
	write(t, repo, "app.go", "package main\n\nconst version = 1 // local tweak\n")

	// A branch that does not exist upstream makes the pull itself fail, without
	// touching the tree.
	d, _, em := pullDeployer(t, repo)
	d.cfg.Branch = "no-such-branch"

	if err := d.pullSource(context.Background()); err == nil {
		t.Fatal("pullSource must report the failed pull")
	} else if errors.Is(err, ErrStashRetained) {
		t.Fatalf("the stash was restored, so this must not be a retained-stash failure: %v", err)
	}

	if got := read(t, repo, "app.go"); !strings.Contains(got, "local tweak") {
		t.Errorf("app.go = %q, want the local edit put back after the failed pull", got)
	}
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("a failed pull left %d stash entries behind, want 0", n)
	}
	if len(em.events) != 0 {
		// The remote refusing an unknown branch is not a condition of the
		// checkout, so it is retried rather than escalated.
		t.Errorf("a missing upstream branch escalated %+v", em.events)
	}
}

// TestPullBlockedByAnUnmergedTreeEscalates pins the classification half. A tree
// left mid-merge cannot even be stashed, and it reproduces identically on every
// deploy — so it is escalated to an operator, with the blocking path named and a
// recoverable command to run, rather than deferring quietly forever.
func TestPullBlockedByAnUnmergedTreeEscalates(t *testing.T) {
	repo, _ := upstreamRepo(t)

	// Build a real merge conflict in the checkout and leave it unresolved.
	gitBin(t, repo, "checkout", "-b", "side")
	write(t, repo, "app.go", "package main\n\nconst version = 3\n")
	gitBin(t, repo, "commit", "-am", "side change")
	gitBin(t, repo, "checkout", "main")
	write(t, repo, "app.go", "package main\n\nconst version = 4\n")
	gitBin(t, repo, "commit", "-am", "main change")
	if out, err := tryGit(t, repo, "merge", "side"); err == nil {
		t.Fatalf("expected the merge to conflict, got: %s", out)
	}

	d, sink, em := pullDeployer(t, repo)
	err := d.pullSource(context.Background())
	if err == nil {
		t.Fatal("pullSource must refuse to deploy from a tree left mid-merge")
	}
	if !errors.Is(err, ErrPullBlocked) {
		t.Fatalf("error = %v, want it to unwrap to ErrPullBlocked", err)
	}

	ev := em.only(t)
	if ev.Reason != ReasonPullBlocked {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonPullBlocked)
	}
	// The headline names what stopped and where before it quotes git, and the
	// remediation is recoverable.
	if !strings.HasPrefix(ev.Detail, "Forge is no longer self-deploying:") {
		t.Errorf("detail does not lead with what stopped:\n%s", ev.Detail)
	}
	for _, want := range []string{repo, "merge --abort", "git output:"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("escalation detail does not mention %q:\n%s", want, ev.Detail)
		}
	}
	if len(ev.Detail) > maxDeployDetailBytes {
		t.Errorf("detail is %d bytes, over the %d bound", len(ev.Detail), maxDeployDetailBytes)
	}
	if !sink.has(EventFailed) {
		t.Error("a blocked pull must also reach the event log")
	}

	// Nothing was touched: the conflicted tree is exactly as the operator left
	// it, so their resolution is still there to finish.
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("a blocked pull left %d stash entries behind, want 0", n)
	}
}

// TestPullRefusesToPopSomebodyElsesEntry: the stash stack is shared with every
// worktree of this repository, and Forge's own workers each hold one. An entry
// pushed while the pull ran means the top of the stack is no longer this
// deploy's, and popping it would restore the wrong work over the pulled tree
// while leaving ours behind.
func TestPullRefusesToPopSomebodyElsesEntry(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	write(t, repo, "app.go", "package main\n\nconst version = 1 // local tweak\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")

	d, _, em := pullDeployer(t, repo)
	// Push a second entry between the pull and the restore, which is what a
	// concurrent worktree does.
	d.cmd = afterCommander{
		Commander: ExecCommander{},
		after: func(args []string) {
			if len(args) > 1 && args[0] == "git" && args[1] == "pull" {
				write(t, repo, "interloper.txt", "another worktree's work\n")
				gitBin(t, repo, "stash", "push", "-u", "-m", "someone else")
			}
		},
	}

	err := d.pullSource(context.Background())
	if !errors.Is(err, ErrStashRetained) {
		t.Fatalf("error = %v, want it to unwrap to ErrStashRetained", err)
	}
	if n := stashCount(t, repo); n != 2 {
		t.Errorf("stash entries = %d, want both this deploy's and the interloper's kept", n)
	}
	ev := em.only(t)
	if ev.Reason != ReasonStashRetained {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonStashRetained)
	}
	if !strings.Contains(ev.Detail, "stash stack") {
		t.Errorf("detail does not explain the stack moved:\n%s", ev.Detail)
	}
}

// afterCommander runs a hook after each command, so a test can perturb the
// checkout between two steps of the pull sequence.
type afterCommander struct {
	Commander
	after func(args []string)
}

func (a afterCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	out, err := a.Commander.Run(ctx, dir, name, args...)
	a.after(append([]string{name}, args...))
	return out, err
}

// TestPullRestoresTheEditEvenWhenTheDeployIsCancelled: a daemon shutting down
// mid-deploy cancels the context the pull runs on. Every later git command would
// then fail instantly, which on the naive reading leaves the operator's work in a
// stash created by a deploy that was merely interrupted. Restoring it is cleanup
// rather than part of the work being cancelled, so it runs regardless.
func TestPullRestoresTheEditEvenWhenTheDeployIsCancelled(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	write(t, repo, "app.go", "package main\n\nconst version = 1 // local tweak\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, _, em := pullDeployer(t, repo)
	d.cmd = afterCommander{
		Commander: ExecCommander{},
		after: func(args []string) {
			// Cancel between the pull and the restore, which is where a
			// shutdown lands most of the time: the pull is the long command.
			if len(args) > 1 && args[0] == "git" && args[1] == "pull" {
				cancel()
			}
		},
	}

	if err := d.pullSource(ctx); err != nil {
		t.Fatalf("pullSource: %v", err)
	}
	if got := read(t, repo, "app.go"); !strings.Contains(got, "local tweak") {
		t.Errorf("app.go = %q, want the local edit restored despite the cancellation", got)
	}
	if n := stashCount(t, repo); n != 0 {
		t.Errorf("a cancelled deploy left %d stash entries behind, want 0", n)
	}
	if len(em.events) != 0 {
		t.Errorf("a cancelled deploy that restored cleanly escalated %+v", em.events)
	}
}

// failingProbeCommander fails the stash probe once the stash push has run,
// which is the moment at which mistaking a failed probe for an empty stack
// loses work.
type failingProbeCommander struct {
	Commander
	pushed bool
}

func (f *failingProbeCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if name == "git" && len(args) > 0 && args[0] == "for-each-ref" && f.pushed {
		return []byte("fatal: not a git repository"), errors.New("exit status 128")
	}
	out, err := f.Commander.Run(ctx, dir, name, args...)
	if name == "git" && len(args) > 1 && args[0] == "stash" && args[1] == "push" {
		f.pushed = true
	}
	return out, err
}

// TestPullAbortsWhenTheStashProbeFails is the invariant a silent probe failure
// would break. The push has already set the operator's work aside; if reading
// the stack back is then mistaken for "nothing was stashed", the deploy pulls
// on, rebuilds, restarts, and the work sits in a stash nobody ever mentions.
// So an unreadable stack aborts the deploy and escalates with the recovery
// instructions, even though it cannot name the entry.
func TestPullAbortsWhenTheStashProbeFails(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	write(t, repo, "app.go", "package main\n\nconst version = 1 // local tweak\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")
	before := gitBin(t, repo, "rev-parse", "HEAD")

	d, _, em := pullDeployer(t, repo)
	d.cmd = &failingProbeCommander{Commander: ExecCommander{}}

	err := d.pullSource(context.Background())
	if !errors.Is(err, ErrStashRetained) {
		t.Fatalf("error = %v, want it to unwrap to ErrStashRetained", err)
	}
	if after := gitBin(t, repo, "rev-parse", "HEAD"); after != before {
		t.Errorf("the deploy pulled on past an unreadable stash stack: HEAD moved to %s", after)
	}
	if n := stashCount(t, repo); n != 1 {
		t.Fatalf("stash entries = %d, want the work still stashed", n)
	}

	ev := em.only(t)
	if ev.Reason != ReasonStashRetained {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonStashRetained)
	}
	// It cannot name the entry, so it must say how to find it instead of
	// quoting a SHA it does not have.
	for _, want := range []string{"stash list", stashMessage, repo} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("escalation detail does not mention %q:\n%s", want, ev.Detail)
		}
	}
}

// TestRetainedStashSurvivesLaterSuccessfulDeploys is the invariant the
// retained-stash escalation rests on. Recovering from a failed pop leaves the
// checkout clean, so the very NEXT deploy fast-forwards without incident — which
// means any withdrawal rule keyed on "a deploy got past the pull" deletes the
// only record of where the operator's work went, reliably, within one deploy
// cycle. It is withdrawn on evidence about the stash stack instead, so the entry
// stands for exactly as long as the work is still on it.
func TestRetainedStashSurvivesLaterSuccessfulDeploys(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	write(t, repo, "app.go", "package main\n\nconst version = 99\n")
	pushUpstream(t, upstream, "app.go", "package main\n\nconst version = 2\n")

	d, _, em := pullDeployer(t, repo)
	if err := d.pullSource(context.Background()); !errors.Is(err, ErrStashRetained) {
		t.Fatalf("error = %v, want it to unwrap to ErrStashRetained", err)
	}
	if n := stashCount(t, repo); n != 1 {
		t.Fatalf("stash entries = %d, want the operator's work still stashed", n)
	}

	// A second deploy: the tree the recovery left is clean, so this pull is an
	// ordinary success. It must not touch the entry.
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")
	em.cleared = nil
	if err := d.pullSource(context.Background()); err != nil {
		t.Fatalf("the second pull must succeed off the tree the recovery left: %v", err)
	}
	d.resolveStashAttention(context.Background())
	if len(em.cleared) != 0 {
		t.Fatalf("a successful pull withdrew the retained-stash entry while the work was still stashed: %+v",
			em.cleared)
	}

	// An unrelated entry from another worktree is not this deploy's work, so it
	// must not hold the escalation open either — only an entry this package
	// labelled counts.
	gitBin(t, repo, "stash", "drop")
	write(t, repo, "unrelated.txt", "another worktree's work\n")
	gitBin(t, repo, "stash", "push", "-u", "-m", "someone else")

	em.cleared = nil
	d.resolveStashAttention(context.Background())
	if len(em.cleared) != 1 || len(em.cleared[0]) != 1 || em.cleared[0][0] != ReasonStashRetained {
		t.Fatalf("cleared = %+v, want the retained-stash entry withdrawn once the work is off the stack",
			em.cleared)
	}
}

// TestRetainedStashStandsWhenTheStackCannotBeRead: an unreadable stack is not
// evidence of an empty one. Being wrong in that direction would delete the only
// record of where somebody's work went; being wrong the other way costs one
// stale row.
func TestRetainedStashStandsWhenTheStackCannotBeRead(t *testing.T) {
	repo, _ := upstreamRepo(t)
	d, _, em := pullDeployer(t, repo)
	d.cmd = failingListCommander{Commander: ExecCommander{}}

	d.resolveStashAttention(context.Background())
	if len(em.cleared) != 0 {
		t.Fatalf("an unreadable stash stack withdrew the entry anyway: %+v", em.cleared)
	}
}

// failingListCommander fails `git stash list` and nothing else.
type failingListCommander struct {
	Commander
}

func (f failingListCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if name == "git" && len(args) > 1 && args[0] == "stash" && args[1] == "list" {
		return []byte("fatal: not a git repository"), errors.New("exit status 128")
	}
	return f.Commander.Run(ctx, dir, name, args...)
}

// TestPullRefusesToAdoptAnEntryItDidNotCreate closes the other half of the
// shared-stack problem. The stack moving between our push and our read-back does
// not make the new top OURS: a worker worktree pushing in that window puts its
// own entry there, and adopting it by SHA alone would pop that worktree's work
// over the pulled tree while leaving ours behind. The entry's message is what
// tells the two apart.
func TestPullRefusesToAdoptAnEntryItDidNotCreate(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	write(t, repo, "app.go", "package main\n\nconst version = 1 // local tweak\n")
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")
	before := gitBin(t, repo, "rev-parse", "HEAD")

	d, _, em := pullDeployer(t, repo)
	// Push a second entry between our stash push and the read-back that names
	// it, which is the whole window the identity check covers.
	d.cmd = afterCommander{
		Commander: ExecCommander{},
		after: func(args []string) {
			if len(args) > 2 && args[0] == "git" && args[1] == "stash" && args[2] == "push" {
				write(t, repo, "interloper.txt", "another worktree's work\n")
				gitBin(t, repo, "stash", "push", "-u", "-m", "someone else")
			}
		},
	}

	err := d.pullSource(context.Background())
	if !errors.Is(err, ErrStashRetained) {
		t.Fatalf("error = %v, want it to unwrap to ErrStashRetained", err)
	}
	if after := gitBin(t, repo, "rev-parse", "HEAD"); after != before {
		t.Errorf("the deploy pulled on past an entry it could not identify: HEAD moved to %s", after)
	}
	if n := stashCount(t, repo); n != 2 {
		t.Errorf("stash entries = %d, want both this deploy's and the interloper's kept", n)
	}
	ev := em.only(t)
	if ev.Reason != ReasonStashRetained {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonStashRetained)
	}
	// It cannot name the entry, so it must say how to find it.
	for _, want := range []string{"stash list", stashMessage, repo} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("escalation detail does not mention %q:\n%s", want, ev.Detail)
		}
	}
}

// TestPullTreatsAnInterloperOnACleanTreeAsNothingToRestore is the same window
// with nothing of ours in it: the tree was clean, so our push saved nothing and
// the entry that appeared belongs to somebody else. The pull proceeds — refusing
// here would defer every deploy that happened to race a worker's stash.
func TestPullTreatsAnInterloperOnACleanTreeAsNothingToRestore(t *testing.T) {
	repo, upstream := upstreamRepo(t)
	pushUpstream(t, upstream, "other.go", "package main\n\nconst other = 2\n")

	d, _, em := pullDeployer(t, repo)
	d.cmd = afterCommander{
		Commander: ExecCommander{},
		after: func(args []string) {
			if len(args) > 2 && args[0] == "git" && args[1] == "stash" && args[2] == "push" {
				write(t, repo, "interloper.txt", "another worktree's work\n")
				gitBin(t, repo, "stash", "push", "-u", "-m", "someone else")
			}
		},
	}

	if err := d.pullSource(context.Background()); err != nil {
		t.Fatalf("pullSource on a clean tree racing another worktree's stash: %v", err)
	}
	if got := read(t, repo, "other.go"); !strings.Contains(got, "other = 2") {
		t.Errorf("other.go = %q, want the pulled content", got)
	}
	if len(em.events) != 0 {
		t.Errorf("nothing of ours was stashed, so nothing may be escalated: %+v", em.events)
	}
}
