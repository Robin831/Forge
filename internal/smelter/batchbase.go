package smelter

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// batchMode is how a flush treats the batch branch it is about to write.
type batchMode int

const (
	// replaceBatch starts the batch from main and force-pushes over the
	// branch: there is no open PR on it, so nothing under review is lost.
	replaceBatch batchMode = iota
	// appendToBatch builds on top of origin/<branch> and pushes without
	// force: the branch has an open PR, so its earlier batch, any commit a
	// person or a lane pushed to fix it, and the review threads anchored to
	// its lines all stay.
	appendToBatch
)

// batchBase is what prepareBatchBase decided about the batch branch.
type batchBase struct {
	mode batchMode
	// archivedAs names the branch the previous head was pushed to before a
	// replace, when that head carried commits main does not have and no
	// merged PR accounts for them. Empty when nothing needed keeping.
	archivedAs string
	// head is the branch head on origin when it was inspected, "" when there
	// was no branch. A replace is leased to it, and a backup is pushed from it.
	head string
}

// prepareBatchBase decides, before the worktree exists, whether this flush may
// replace the batch branch. A regeneration used to reset to main and
// force-push unconditionally, which dropped the rules an open batch PR had
// learned (they were neither in main nor archived), orphaned that PR's review
// threads, and could erase fix commits pushed to the branch.
//
// The rule: an open PR on the branch means append. No open PR means replace,
// but a branch head that main does not contain and no merged PR accounts for
// (a closed PR, or a branch with no PR) is first pushed to a "-replaced-<sha>"
// branch so it can be recovered. When the PR lookup fails the flush stops
// rather than guess: the pending queue is kept for the next cycle.
func (s *Smelter) prepareBatchBase(ctx context.Context, anvilPath, branch string) (batchBase, error) {
	pr, err := s.findPR(ctx, anvilPath, branch)
	if err != nil {
		return batchBase{}, fmt.Errorf("cannot tell whether %s has an open PR, so it is not replaced: %w", branch, err)
	}

	exists, err := fetchRemoteBranch(ctx, anvilPath, branch)
	if err != nil {
		return batchBase{}, err
	}
	if !exists {
		return batchBase{mode: replaceBatch}, nil
	}
	if pr.State == "OPEN" {
		return batchBase{mode: appendToBatch}, nil
	}

	ahead, head, err := commitsAheadOfMain(ctx, anvilPath, branch)
	if err != nil {
		return batchBase{}, err
	}
	// A merged PR accounts for the branch only when it was merged with this
	// very head: a later batch pushed to the reused branch (whose PR was never
	// opened, say) is still unmerged work.
	if ahead == 0 || (pr.State == "MERGED" && pr.HeadOID != "" && pr.HeadOID == head) {
		return batchBase{mode: replaceBatch, head: head}, nil
	}
	archive := fmt.Sprintf("%s-replaced-%s", branch, head[:min(len(head), 12)])
	if _, err := runGit(ctx, anvilPath, "push", "origin", head+":refs/heads/"+archive); err != nil {
		return batchBase{}, fmt.Errorf("keeping the unmerged head of %s as %s before replacing it: %w", branch, archive, err)
	}
	log.Printf("[smelter] %s had %d commit(s) main does not have and no merged PR for that head; kept them as %s before starting a new batch", branch, ahead, archive)
	return batchBase{mode: replaceBatch, archivedAs: archive, head: head}, nil
}

// fetchRemoteBranch updates refs/remotes/origin/<branch> and reports whether
// the branch exists on origin. A missing branch also clears a stale tracking
// ref, so nothing later reads a head that is gone.
func fetchRemoteBranch(ctx context.Context, dir, branch string) (bool, error) {
	stderr, err := runGit(ctx, dir, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	if err == nil {
		return true, nil
	}
	if strings.Contains(stderr, "couldn't find remote ref") {
		_, _ = runGit(ctx, dir, "update-ref", "-d", "refs/remotes/origin/"+branch)
		return false, nil
	}
	return false, fmt.Errorf("git fetch origin %s: %w\nstderr: %s", branch, err, stderr)
}

// commitsAheadOfMain counts the commits on origin/<branch> that origin/main
// (or origin/master) does not have, and returns the branch head.
func commitsAheadOfMain(ctx context.Context, dir, branch string) (int, string, error) {
	base := "origin/main"
	if _, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", base); err != nil {
		base = "origin/master"
	}
	_, _ = runGit(ctx, dir, "fetch", "origin", "+refs/heads/"+strings.TrimPrefix(base, "origin/")+":refs/remotes/"+base)
	count, err := gitOutput(ctx, dir, "rev-list", "--count", base+"..refs/remotes/origin/"+branch)
	if err != nil {
		return 0, "", err
	}
	head, err := gitOutput(ctx, dir, "rev-parse", "refs/remotes/origin/"+branch)
	if err != nil {
		return 0, "", err
	}
	var n int
	if _, err := fmt.Sscanf(count, "%d", &n); err != nil {
		return 0, "", fmt.Errorf("counting commits ahead of %s: %q", base, count)
	}
	return n, head, nil
}

// runGit runs git in dir with the cleaned environment and returns its stderr.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = dir
	cmd.Env = executil.CleanGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = dir
	cmd.Env = executil.CleanGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
