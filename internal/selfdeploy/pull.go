package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/gitfail"
)

// stashMessage labels the entry the deploy's own stash created. It is only ever
// read by a human — the entry is addressed by SHA everywhere below — but it is
// the first thing an operator sees in `git stash list` if a deploy ever leaves
// one behind, so it says who made it and why.
const stashMessage = "forge-selfdeploy: local changes set aside for a fast-forward pull"

// stashRef is the ref the stash stack's top entry lives on.
const stashRef = "refs/stash"

// restoreTimeout bounds the restore sequence, which runs on a context that
// deliberately ignores the deploy's own cancellation (see restoreCtx). Without
// a bound of its own a hung git there would hold up a shutdown indefinitely.
const restoreTimeout = 2 * time.Minute

// ErrStashRetained is the sentinel behind every outcome that leaves the
// operator's local changes in a stash Forge did not put back. It is its own
// class because the remedy is unlike any other deploy failure's: nothing Forge
// does later restores those changes, so the deploy must stop and say where the
// work is rather than carry on and rebuild.
var ErrStashRetained = errors.New("local changes left in a stash")

// ErrPullBlocked is the sentinel for a fast-forward pull refused by a condition
// that reproduces on every deploy — an unmerged tree, a detached HEAD, a ref the
// pull cannot lock. Unlike a network failure it will not clear on its own, so it
// is escalated to an operator rather than retried quietly on the next merge.
var ErrPullBlocked = errors.New("pull blocked by the checkout's own state")

// pullSource fast-forwards the checkout the deploy builds from, setting any
// local modifications aside for the duration and putting them back afterwards.
//
// Self-deploy genuinely needs the tree updated — it is about to compile it — so
// it cannot take depcheck's way out of reading the upstream ref's blobs and
// leaving the checkout alone. But `git pull --ff-only` is refused outright
// whenever a tracked file has local modifications the incoming commits touch,
// and Forge's own checkout is not reliably clean: a pod-local
// `.beads/config.yaml`, bd's additions to `.beads/.gitignore`, or an operator's
// half-finished edit are all permanent as far as a nightly deploy is concerned.
// Both sides of that condition are permanent, so one stray edit deferred every
// deploy from then on and the running binary silently fell weeks behind main —
// the exact version skew this package exists to close.
//
// The sequence is an explicit stash/pull/pop rather than `pull --ff-only
// --autostash` because the autostash's failure mode is the one thing that must
// not happen quietly: when its pop conflicts, git prints a warning, leaves the
// entry on the stack and exits 0, so the deploy would carry on, rebuild, restart
// — and the operator's work would sit in a stash nobody mentioned. Done by hand,
// every step is checked: the entry is addressed by the SHA the push produced, it
// is only popped while it is still the top of the stack, the pop is verified to
// have CONSUMED it, and anything else aborts the deploy with the ref in the
// message.
//
// The tree is left exactly as it was found on every failure path: either the
// changes are back in the working tree, or they are in a named stash and the
// error says so. It never both fails and leaves a stash unmentioned.
func (d *Deployer) pullSource(ctx context.Context) error {
	stash, err := d.stashLocalChanges(ctx)
	if err != nil {
		return err
	}
	if stash == "" {
		// A clean tree (or one holding nothing but ignored files): the pull is
		// the plain fast-forward it has always been, with nothing to restore.
		return d.fastForward(ctx)
	}

	if err := d.fastForward(ctx); err != nil {
		// The pull is the failure being reported, but the tree must not be left
		// stripped of the operator's work while we report it. Restore first; a
		// restore that also fails outranks the pull error, because it is the one
		// that leaves work somewhere the operator has to be told about.
		if rErr := d.restoreStash(ctx, stash); rErr != nil {
			return rErr
		}
		return err
	}
	return d.restoreStash(ctx, stash)
}

// stashLocalChanges sets the working tree's modifications aside and returns the
// SHA of the entry it created, or "" when there was nothing to stash.
//
// The entry is identified by comparing refs/stash before and after the push
// rather than by reading the push's own output, for two reasons. git says "No
// local changes to save" on a clean tree and nothing identifying on a dirty one,
// so the output answers only half the question; and the stash stack is shared
// with every other worktree of this repository — Forge's own workers each hold
// one — so "there is a stash" was never the same claim as "we made it". The SHA
// is what makes the later pop provably about this deploy's entry.
func (d *Deployer) stashLocalChanges(ctx context.Context) (string, error) {
	before, err := d.stashTop(ctx)
	if err != nil {
		return "", d.failPull(ctx, "could not read the stash stack before the pull", err)
	}

	// --include-untracked, not --all: an incoming commit that ADDS a file the
	// tree already holds untracked refuses the fast-forward exactly as a
	// modified tracked file does. Ignored files are deliberately left alone —
	// they are build output, worktrees and local config, none of which git will
	// overwrite and all of which would make the stash enormous.
	out, pushErr := d.git(ctx, "stash", "push", "--include-untracked", "--message", stashMessage)
	if pushErr != nil {
		return "", d.failPull(ctx, "could not set local changes aside before the pull", pushErr, out)
	}

	after, err := d.stashTop(ctx)
	if err != nil {
		// The push reported success, so there may now be an entry holding the
		// operator's work that we can no longer name. Abandoning the deploy here
		// is the whole point: pulling on would leave that work stashed and
		// unmentioned, which is the failure this sequence exists to prevent.
		return "", d.failStashRetained("", nil,
			fmt.Sprintf("local changes were set aside but the stash stack could not be read back: %v", err))
	}
	if after == before {
		// Nothing was stashed: a clean tree, or one holding only ignored files.
		return "", nil
	}
	return after, nil
}

// restoreStash puts the entry back and proves it was consumed.
//
// Two things are checked that `git stash pop` does not check for itself. The
// entry must still be the top of the stack — the stack is shared, and popping
// after somebody else pushed would restore THEIR work over the pulled tree and
// leave ours behind. And the pop must have actually removed the entry: pop
// degrades to apply on conflict, so a stack that still holds our SHA afterwards
// means the tree does not.
func (d *Deployer) restoreStash(ctx context.Context, stash string) error {
	// The restore runs free of the deploy's cancellation, for the same reason
	// the Restarter does: a daemon shutting down mid-deploy cancels this
	// context, every git command below then fails instantly, and the operator's
	// work is left in a stash by a deploy that was merely interrupted. Putting
	// it back is cleanup, not part of the work being cancelled — so it is bounded
	// by its own deadline instead.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()

	top, err := d.stashTop(ctx)
	if err != nil {
		return d.failStashRetained(stash, nil,
			fmt.Sprintf("the stash stack could not be read back before restoring the local changes: %v", err))
	}
	if top != stash {
		return d.failStashRetained(stash, nil,
			"another process pushed onto the stash stack while the pull ran, so this deploy's entry is no longer "+
				"the top one and popping it would restore the wrong work")
	}

	out, popErr := d.git(ctx, "stash", "pop")
	if popErr != nil {
		return d.recoverFailedPop(ctx, stash, out, popErr)
	}

	// pop exited 0. Prove the entry is gone rather than trusting it: a stack
	// that still holds this SHA is a tree that does not hold the changes.
	top, err = d.stashTop(ctx)
	if err != nil {
		return d.failStashRetained(stash, nil,
			fmt.Sprintf("restoring the local changes reported success but the stash stack could not be read back to "+
				"confirm the entry was consumed: %v", err))
	}
	if top == stash {
		return d.failStashRetained(stash, nil,
			"restoring the local changes reported success but the entry is still on top of the stash stack, so the "+
				"working tree does not hold them")
	}
	return nil
}

// recoverFailedPop deals with a pop that conflicted with the commits just
// pulled.
//
// The order matters. The conflicting paths are enumerated first, because
// clearing the conflict is what destroys the evidence for them. The stash is
// then re-checked: `git stash pop` keeps its entry when it degrades to an apply,
// and that entry holding the work is the ONLY thing that makes clearing the tree
// safe — with it gone the half-applied changes in the tree are the last copy and
// must be left exactly where they are. Only once the work is provably still in
// the stash is the tree reset back to the commit the pull landed on, so the
// checkout is left clean and buildable rather than full of conflict markers.
//
// The deploy is abandoned either way. A rebuild from a tree carrying somebody
// else's half-merged edit is not a build of main.
func (d *Deployer) recoverFailedPop(ctx context.Context, stash, popOut string, popErr error) error {
	paths := d.blockingPaths(ctx)

	top, err := d.stashTop(ctx)
	if err != nil || top != stash {
		return d.failStashRetained(stash, paths,
			fmt.Sprintf("restoring the local changes failed (%s) and the entry is no longer on the stash stack, so the "+
				"working tree holds the only copy and has been left untouched — it is mid-merge and must be resolved by "+
				"hand", gitfail.Sanitize(firstNonEmpty(popOut, popErr.Error()), maxEvidenceBytes)))
	}

	detail := fmt.Sprintf("restoring the local changes conflicts with the commits that were pulled (%s)",
		gitfail.Sanitize(firstNonEmpty(popOut, popErr.Error()), maxEvidenceBytes))
	if out, err := d.git(ctx, "reset", "--hard"); err != nil {
		// The tree is still conflicted. Say so: the next deploy will find a tree
		// mid-merge, and an operator reading this needs to know the reset was
		// attempted and refused rather than never tried.
		detail += fmt.Sprintf("; clearing the conflict from the working tree also failed (%s), so the checkout is "+
			"still mid-merge", gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
	}
	return d.failStashRetained(stash, paths, detail)
}

// fastForward runs the pull itself. A diverged tree is deliberately not merged
// or rebased: that is an operator's decision about Forge's own checkout, not a
// deploy's.
func (d *Deployer) fastForward(ctx context.Context) error {
	out, err := d.git(ctx, "pull", "--ff-only", "origin", d.cfg.Branch)
	if err != nil {
		return d.failPull(ctx, "git pull failed", err, out)
	}
	return nil
}

// git runs one git command in the deploy's checkout and returns its output as a
// trimmed string. The Commander captures combined output, so what comes back on
// a failure is git's own words — which is what the classification and the
// operator-facing message are both built from.
func (d *Deployer) git(ctx context.Context, args ...string) (string, error) {
	out, err := d.cmd.Run(ctx, d.cfg.RepoPath, "git", args...)
	return strings.TrimSpace(string(out)), err
}

// stashTop resolves the top of the stash stack, or "" when the stack is empty.
//
// `for-each-ref` rather than `rev-parse --verify --quiet`, because those two
// answers have to be told apart and rev-parse spells them the same way: it exits
// non-zero and says NOTHING for a ref that does not exist, which is
// byte-identical to what a command killed by a cancelled context produces. Read
// as an empty stack, a cancelled probe lets the deploy conclude nothing was
// stashed and pull on past work it had just set aside — the exact silent loss
// this whole sequence exists to prevent.
//
// for-each-ref exits 0 whether or not the ref exists, printing the SHA or
// nothing, so an empty result means an empty stack and ANY non-zero exit is a
// real failure to report.
func (d *Deployer) stashTop(ctx context.Context) (string, error) {
	out, err := d.git(ctx, "for-each-ref", "--format=%(objectname)", stashRef)
	if err != nil {
		return "", fmt.Errorf("git for-each-ref %s: %v: %s", stashRef, err,
			gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
	}
	return strings.TrimSpace(out), nil
}

// blockingPaths enumerates what the working tree is holding, for the message.
//
// Best-effort by construction: `git status` in a checkout already known to be in
// a bad state is exactly the command that may also fail, and a message with no
// path list is worth far more than no message at all — so a failure here is
// logged and the message is built without it.
func (d *Deployer) blockingPaths(ctx context.Context) []string {
	paths, err := gitfail.DirtyPaths(ctx, d.cfg.RepoPath, func(c context.Context, dir string, args ...string) ([]byte, error) {
		return d.cmd.Run(c, dir, "git", args...)
	})
	if err != nil {
		d.logger.Warn("self-deploy: could not enumerate the checkout's blocking paths",
			"repo", d.cfg.RepoPath, "error", gitfail.Sanitize(err.Error(), maxEvidenceBytes))
		return nil
	}
	return paths
}

// firstNonEmpty returns the first non-empty of its arguments. git reports some
// refusals on stdout and some through the exit error alone, and the message
// wants whichever one actually carries words.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
