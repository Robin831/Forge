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
// deliberately ignores the deploy's own cancellation (see restoreStash). Being
// uncancellable is exactly why it needs a deadline of its own: without one a git
// command hung there would hold up a shutdown indefinitely.
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
// The one tree that is never stashed is a conflicted one (conflictedState +
// refuseUnmergedTree): setting somebody's half-finished merge aside is not a
// thing a deploy may do quietly. Which is not a claim about unmerged INDEX
// entries — those are only the visible half. Once the conflicts are staged the
// index reads clean and it is the stash's own reset that destroys the merge, on
// every git there is.
//
// The tree is left exactly as it was found on every failure path: either the
// changes are back in the working tree, or they are in a named stash and the
// error says so. It never both fails and leaves a stash unmentioned.
func (d *Deployer) pullSource(ctx context.Context) error {
	state, err := d.conflictedState(ctx)
	if err != nil {
		return err
	}
	if state != "" {
		return d.refuseUnmergedTree(ctx, state)
	}

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

// sequencerRefs are the pseudo-refs git writes for an operation that has begun
// and not been concluded. They are what remains once the conflicts have been
// staged: `git add` clears the unmerged index entries, and from that moment
// these are the only record that a merge, cherry-pick or revert is still
// half-finished.
//
// REBASE_HEAD is deliberately NOT among them, even though a conflicted rebase
// writes it. Unlike the three below, git does not remove it when the rebase
// concludes — it is left pointing at the last commit replayed — so a checkout
// where a rebase has ever run carries it forever, and probing it would refuse
// every deploy from then on. A rebase's own conflicts keep the index unmerged,
// so `ls-files --unmerged` covers the state that matters; and a rebase stopped
// mid-way leaves HEAD detached, which git refuses the pull over and gitfail
// already classifies as blocked.
var sequencerRefs = []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"}

// conflictedState reports what the checkout is in the middle of, as a phrase for
// the operator-facing message, or "" when it is in the middle of nothing.
//
// Two probes, because one conflicted checkout can be in either of two shapes and
// only the first is visible in the index:
//
//   - `ls-files --unmerged` lists stage 1/2/3 entries — a merge, rebase or
//     cherry-pick whose conflicts are still unresolved in the tree.
//   - `rev-list --ignore-missing` over the sequencer refs catches the same
//     operation once its conflicts have been STAGED but not committed, which is
//     the ordinary state between `git add` and `git commit`. There the index is
//     back at stage 0 and the first probe reports nothing, so the tree used to
//     read as a plain dirty one and be stashed — and `git stash push` resets the
//     tree, which DELETES MERGE_HEAD. The fast-forward then goes through, the
//     pop puts the resolved contents back as ordinary modifications, and the
//     merge the operator was part-way through has quietly become a plain diff:
//     committing it produces a commit with one parent where they were building
//     one with two, and nothing anywhere says it happened.
//
// Both are facts about `.git` rather than sentences git prints, so neither
// depends on git's version or the host's locale — which is the property the
// whole refusal rests on. And both exit 0 whether or not they find anything, so
// any non-zero exit is a real failure to report rather than an absence to read
// past: the same property stashTop relies on for-each-ref for.
func (d *Deployer) conflictedState(ctx context.Context) (string, error) {
	out, err := d.git(ctx, "ls-files", "--unmerged")
	if err != nil {
		return "", d.failPull(ctx, "could not read the checkout's merge state before the pull", err, out)
	}
	if out != "" {
		return "left mid-merge, with conflicts still unresolved in the index", nil
	}

	args := append([]string{"rev-list", "--no-walk", "--ignore-missing"}, sequencerRefs...)
	out, err = d.git(ctx, args...)
	if err != nil {
		return "", d.failPull(ctx, "could not read the checkout's merge state before the pull", err, out)
	}
	if out != "" {
		return "in the middle of a merge, cherry-pick or revert whose conflicts are resolved but not committed", nil
	}
	return "", nil
}

// refuseUnmergedTree abandons a deploy whose checkout is mid-conflict, without
// touching the stash.
//
// A half-finished merge is not a local modification the deploy is entitled to
// set aside. The resolution somebody is part-way through lives in the index and
// the sequencer state and nowhere else — no commit holds it — and what a stash
// makes of it is a matter of git's version: git 2.43 refuses to stash an
// unmerged index at all ("<path>: needs merge"), while 2.55 stashes it happily,
// which drops the merge state, lets the fast-forward through, and pops conflict
// markers back over the pulled tree. That deploy then rebuilds Forge from a tree
// full of `<<<<<<<`, and the operator's half-finished merge is a stash entry
// nobody mentioned. A tree whose conflicts are already staged needs no version
// disagreement at all: every git stashes it, and the reset that stash performs
// is what deletes MERGE_HEAD.
//
// So the stash is skipped entirely and the pull is attempted as the tree stands,
// because git's own refusal is the best evidence to quote back — "Pulling is not
// possible because you have unmerged files", or "You have not concluded your
// merge (MERGE_HEAD exists)".
//
// What is NOT left to git's wording is whether this is escalated. conflictedState
// has already established a fact that gitfail classifies as blocked by
// definition, and it reproduces on every merge until an operator touches the
// checkout; letting a sentence decide would mean a message no BlockedPatterns
// entry happens to match is retried quietly forever while the daemon runs its
// old binary — the exact failure ReasonPullBlocked exists to close. So both
// exits raise it, with the cause this function knows rather than one re-derived
// from text.
func (d *Deployer) refuseUnmergedTree(ctx context.Context, state string) error {
	what := fmt.Sprintf("the checkout is %s, which a deploy may not set aside to pull over", state)

	out, err := d.git(ctx, "pull", "--ff-only", "origin", d.cfg.Branch)
	if err == nil {
		// git accepted a fast-forward over a checkout it has refused one for
		// since long before any version Forge supports. Nothing concluded that
		// operation, so the tree still holds the half-finished work and must not
		// be built from — and there is no git output to quote, which is exactly
		// why the escalation rests on the probe rather than on the message.
		return d.failPullBlocked(ctx, what+", and git did not refuse the fast-forward, so the tree it "+
			"left cannot be trusted to build from", gitfail.CauseUnmerged, "")
	}
	return d.failPullBlocked(ctx, what, gitfail.CauseUnmerged,
		gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
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
//
// A moved top is not on its own proof that the new top is OURS, which is the
// second half of the same shared-stack problem: a worker worktree pushing an
// entry in the window between our push and our read-back puts THEIR SHA there,
// and adopting it would mean popping their work over the pulled tree while
// leaving ours on the stack. So the entry's message has to name this package
// too — the one thing about it that a concurrent entry cannot coincidentally
// match.
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

	after, subject, err := d.stashTopEntry(ctx)
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
	if !strings.Contains(subject, stashMessage) {
		// The top moved to an entry somebody else pushed. If our own push saved
		// nothing then this is simply a busy stack and there is nothing of ours
		// to restore — the pull can go ahead untouched.
		if strings.Contains(out, gitNothingToStash) {
			return "", nil
		}
		// Otherwise our entry exists but is no longer the top, and this sequence
		// only ever pops the top. Say where the work is and stop, rather than
		// popping an entry that belongs to another worktree.
		return "", d.failStashRetained("", nil,
			"local changes were set aside but another process pushed onto the shared stash stack immediately "+
				"afterwards, so this deploy's entry is neither the top one nor safely identifiable")
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
	sha, _, err := d.stashTopEntry(ctx)
	return sha, err
}

// stashTopEntry is stashTop plus the entry's message, which is what tells an
// entry this deploy made apart from one that merely arrived while it was
// working. Both come out of one command, so the two answers cannot describe two
// different tops.
func (d *Deployer) stashTopEntry(ctx context.Context) (sha, subject string, err error) {
	// A NUL separator: the subject is a message this deploy did not necessarily
	// write (another worktree's entry can be on top), and every printable
	// separator is legal inside one.
	const format = "--format=%(objectname)%00%(contents:subject)"
	out, err := d.git(ctx, "for-each-ref", format, stashRef)
	if err != nil {
		return "", "", fmt.Errorf("git for-each-ref %s: %v: %s", stashRef, err,
			gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", "", nil
	}
	sha, subject, _ = strings.Cut(out, "\x00")
	return strings.TrimSpace(sha), strings.TrimSpace(subject), nil
}

// gitNothingToStash is git's answer on a tree with nothing to set aside. It is
// matched as text, which is sound for the same reason gitfail's patterns are:
// ExecCommander pins LC_ALL=C, so git's diagnostics are not translated. And it
// is only ever a SECOND opinion — the identity of an entry is decided by SHA and
// by message above — so a future rewording costs nothing but a needlessly
// cautious abort in the rare window it covers.
const gitNothingToStash = "No local changes to save"

// resolveStashAttention withdraws a ReasonStashRetained entry, but only on the
// one piece of evidence that can justify it: no entry labelled by this package
// is left on the stash stack.
//
// Nothing else may withdraw it. A deploy reaching any later step proves only
// that the DEPLOY is healthy, and the failure being reported is that the
// OPERATOR's work is sitting in a stash — which recoverFailedPop's clean reset
// guarantees does not stop the next deploy from succeeding. So the entry has to
// outlive successful deploys until the stack itself says the work has been taken
// back out (or dropped, which is the operator's decision to make).
//
// A probe that cannot run leaves the entry standing: an unreadable stack is not
// evidence of an empty one, and this is the direction in which being wrong costs
// only a stale row rather than the record of where somebody's work went.
func (d *Deployer) resolveStashAttention(ctx context.Context) {
	if d.attention == nil {
		return
	}
	// `stash list` rather than the top entry alone: a retained entry is pushed
	// down the stack by every later stash, this deploy's own included.
	out, err := d.git(ctx, "stash", "list")
	if err != nil {
		d.logger.Warn("self-deploy: could not read the stash stack, so any retained-stash entry stands",
			"repo", d.cfg.RepoPath, "error", gitfail.Sanitize(firstNonEmpty(out, err.Error()), maxEvidenceBytes))
		return
	}
	if strings.Contains(out, stashMessage) {
		return
	}
	d.clearAttention(ReasonStashRetained)
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
