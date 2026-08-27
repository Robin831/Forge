package selfdeploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/gitfail"
	"github.com/Robin831/Forge/internal/textfmt"
)

// maxDeployDetailBytes bounds the WHOLE operator-facing description of a
// blocked deploy, not one component of it: the string is persisted as one
// state.DeployFailure.Detail and emitted as one event message, and a bound
// applied per component adds up to a multiple of itself — a row nobody reads to
// the end of.
const maxDeployDetailBytes = 2000

// maxEvidenceBytes bounds git's own words wherever they are quoted on their own
// (a log field, a clause inside a larger detail). The assembled detail budgets
// the trailing evidence separately, against what the rest of it left.
const maxEvidenceBytes = 600

// minEvidenceBytes is the floor on git's own words inside the assembled detail.
// The evidence is rendered last and gets whatever maxDeployDetailBytes has left,
// but a message that spent its budget on paths and still shows no evidence is
// unverifiable — the operator cannot check the claim against what git said.
const minEvidenceBytes = 200

// failPull reports a git command that stopped the deploy before the rebuild,
// classifying it first because the two classes need opposite handling.
//
// A TRANSIENT failure — the remote was unreachable, the fetch timed out — keeps
// the behaviour this package has always had: an event, and the next qualifying
// merge tries again. Nothing is escalated, because there is nothing for an
// operator to do.
//
// A BLOCKED one will reproduce on every merge until somebody changes the
// checkout, and the deploy quietly deferring each time is precisely how the
// running binary fell weeks behind main with the evidence in the event log the
// whole time. It is escalated into Needs Attention instead — once, because the
// row is keyed by (anvil, reason) and a recurrence refreshes it rather than
// stacking another — and withdrawn by the first deploy that pulls successfully.
func (d *Deployer) failPull(ctx context.Context, what string, err error, out ...string) error {
	raw := firstNonEmpty(append(append([]string{}, out...), err.Error())...)
	evidence := gitfail.Sanitize(raw, maxEvidenceBytes)

	if gitfail.Classify(raw, err) != gitfail.Blocked {
		// %w, so the daemon's own switch still recognises a deploy abandoned by
		// a cancelled context as an abort rather than reporting it as a failure.
		return d.fail("%s: %w: %s", what, err, evidence)
	}

	return d.failPullBlocked(ctx, what, gitfail.CauseOf(evidence), evidence)
}

// failPullBlocked escalates a pull stopped by the checkout's own state, with the
// cause supplied rather than re-derived.
//
// It is separate from failPull because a caller can KNOW the condition without
// git having said so. refuseUnmergedTree has probed the index and the sequencer
// refs, which is stronger evidence than any sentence — and git's sentence for a
// staged-but-uncommitted merge ("You have not concluded your merge") is one no
// pattern set is guaranteed to model. Routing that through Classify would let a
// wording decide whether a condition that reproduces on every merge is escalated
// or retried quietly forever, which is what ReasonPullBlocked exists to prevent.
//
// evidence may be empty: a caller that established the condition itself has
// nothing of git's to quote, and blockedPullDetail simply omits the section.
func (d *Deployer) failPullBlocked(ctx context.Context, what string, cause gitfail.Cause, evidence string) error {
	detail := d.blockedPullDetail(ctx, what, cause, evidence)
	d.emit(EventFailed, detail)
	d.raiseAttention(DeployEvent{Reason: ReasonPullBlocked, Detail: detail})
	return fmt.Errorf("selfdeploy: %w: %s", ErrPullBlocked, detail)
}

// failStashRetained reports the one outcome that leaves the operator's work
// somewhere Forge put it and could not take it back out.
//
// It is always escalated, whatever git's words classify as: the condition is not
// something a later deploy clears, and the message is the only record of where
// the work went. The deploy is abandoned before the rebuild — a binary built
// from a tree still carrying a half-applied stash is not a build of main.
func (d *Deployer) failStashRetained(stash string, paths []string, detail string) error {
	msg := d.stashRetainedDetail(stash, paths, detail)
	d.emit(EventFailed, msg)
	d.raiseAttention(DeployEvent{Reason: ReasonStashRetained, Detail: msg})
	if stash != "" {
		return fmt.Errorf("selfdeploy: %w (stash %s in %s): %s", ErrStashRetained, stash, d.cfg.RepoPath, detail)
	}
	return fmt.Errorf("selfdeploy: %w in %s: %s", ErrStashRetained, d.cfg.RepoPath, detail)
}

// blockedPullDetail describes a deploy blocked by the checkout's own state.
//
// The order is the same one depcheck's blocked escalation settled on, for the
// same reason: what has stopped and where comes first, then the paths an
// operator can recognise the condition by, then the one command to run, then the
// sentence saying the entry withdraws itself — and git's raw output LAST, under
// its own label. git's sentence is evidence for a claim, not the claim, and
// "fatal: You are not currently on a branch" as the opening of a Needs Attention
// row says nothing about what is no longer being deployed or what to do.
func (d *Deployer) blockedPullDetail(ctx context.Context, what string, cause gitfail.Cause, evidence string) string {
	var paths []string
	if cause.InvolvesWorkingTree() {
		paths = d.blockingPaths(ctx)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Forge is no longer self-deploying: %s, so the daemon keeps running the binary it already has "+
		"while merges keep landing on %s. This repeats identically on every deploy until the checkout is fixed. "+
		"Checkout: %s.", what, d.cfg.Branch, d.cfg.RepoPath)

	if cause.InvolvesWorkingTree() && len(paths) > 0 {
		label := "Blocking paths"
		if cause == gitfail.CauseUnknown {
			// Nothing has established that the tree is what is blocking, so the
			// list is context rather than a diagnosis and is labelled as such.
			label = "Working tree"
		}
		fmt.Fprintf(&b, " %s (%s): %s.", label, textfmt.Count(len(paths), "path"), gitfail.RenderPaths(paths))
	}

	fmt.Fprintf(&b, " %s", d.pullRemediation(cause, paths))
	b.WriteString(" Forge clears this entry automatically once a deploy pulls successfully.")

	appendEvidence(&b, evidence)
	return b.String()
}

// pullRemediation is the one command an operator can run, chosen by cause.
//
// Every command is recoverable — `stash push` rather than `checkout --`,
// `merge --abort` named alongside resolving the conflicts — because this is read
// by somebody who has not yet looked at the checkout, and a destructive
// one-liner in that position throws away work the message never claimed to have
// inspected.
func (d *Deployer) pullRemediation(cause gitfail.Cause, paths []string) string {
	at := d.cfg.RepoPath
	if at == "" {
		at = "<checkout>"
	}
	switch cause {
	case gitfail.CauseUnmerged:
		return fmt.Sprintf("To resolve: finish the merge in the checkout, or abandon it with `git -C %s merge --abort` "+
			"(`rebase --abort` if it is a rebase).", at)

	case gitfail.CauseDirtyTree:
		// Reaching this means the stash itself was refused: a tree Forge could
		// set aside is a tree the pull is not blocked by. So the command is the
		// same one, for the operator to run by hand at a moment when the
		// checkout is not also mid-something-else.
		unexpected := gitfail.UnexpectedPaths(paths)
		if len(unexpected) == 0 {
			// Either nothing was enumerated (`git status` in a checkout already
			// known to be broken is exactly the command that may also fail) or
			// everything modified is a file this checkout is meant to carry.
			// Neither supports a claim about which files to move, so this says
			// what to look at instead of naming a set nobody inspected.
			return fmt.Sprintf("To resolve: inspect the checkout with `git -C %s status` and set the unexpected "+
				"changes aside (recoverable) with `git -C %s stash push -u -- <path>`, leaving %s in place.",
				at, at, gitfail.ExpectedPathNames())
		}
		keep := ""
		// Only claim that annotated paths are being left in place when one the
		// operator can SEE was annotated: the rendered list is itself capped, and
		// a clause resting on an entry past the cap names a set the message does
		// not contain.
		if shown := gitfail.ListedPaths(paths); len(gitfail.UnexpectedPaths(shown)) != len(shown) {
			keep = ", leaving the paths annotated as expected in place"
		}
		spec, covered := gitfail.Pathspec(unexpected)
		switch {
		case spec != "" && covered == len(unexpected):
			return fmt.Sprintf("To resolve: set the unexpected changes aside (recoverable) with "+
				"`git -C %s stash push -u -- %s`%s.", at, spec, keep)
		case spec != "":
			// "the first one", not "the first 1": both of Pathspec's caps can
			// leave a single path covered, and a sentence an operator reads aloud
			// has to survive it.
			first := fmt.Sprintf("%d", covered)
			if covered == 1 {
				first = "one"
			}
			return fmt.Sprintf("To resolve: %s are unexpectedly modified, more than fit one command — "+
				"`git -C %s stash push -u -- %s` sets the first %s aside (recoverable)%s; repeat it for the rest, "+
				"which `git -C %s status` lists in full.",
				textfmt.Count(len(unexpected), "path"), at, spec, first, keep, at)
		default:
			return fmt.Sprintf("To resolve: set the unexpected changes aside (recoverable) with "+
				"`git -C %s stash push -u -- <path>` for each of the %s that `git -C %s status` reports and this "+
				"message does not annotate as expected.", at, textfmt.Count(len(unexpected), "path"), at)
		}

	case gitfail.CauseDetachedHead:
		return fmt.Sprintf("To resolve: put the checkout back on %s, with `git -C %s checkout %s`.",
			d.cfg.Branch, at, d.cfg.Branch)

	case gitfail.CauseRefLock:
		// "below": git's output is the LAST section of this message, under its
		// own label.
		return fmt.Sprintf("To resolve: no git process should be running here — remove the stale lock file git names "+
			"below under `git output:` (under `%s/.git`), then let the next deploy retry.", at)

	case gitfail.CauseNotARepo:
		return fmt.Sprintf("To resolve: %s is not a git checkout — point `self_deploy.repo_path` (or the anvil's "+
			"`path`) in `~/.forge/config.yaml` at one.", at)

	default:
		return fmt.Sprintf("To resolve: inspect the checkout's git state with `git -C %s status` — Forge has no "+
			"specific remedy modelled for this message.", at)
	}
}

// stashRetainedDetail describes work Forge set aside and could not put back.
//
// It leads with the reassurance because it is true and because it is the first
// question an operator asks: the changes are in a named stash, not gone. Then
// what happened, then the paths, then the commands that get the work back — and
// the deploy's own state last, since the binary being unchanged is the least
// urgent part of this particular failure.
//
// The closing sentence is the one that differs from every other escalation's,
// and it has to: this entry does not withdraw itself when deploys start working
// again (they will, immediately — the recovery reset leaves the tree clean),
// only when the stash stack no longer holds an entry this package labelled. An
// operator told "cleared once a deploy pulls successfully" would watch the only
// record of where their work went disappear within the hour.
func (d *Deployer) stashRetainedDetail(stash string, paths []string, detail string) string {
	at := d.cfg.RepoPath
	if at == "" {
		at = "<checkout>"
	}

	var b strings.Builder
	switch stash {
	case "":
		fmt.Fprintf(&b, "Self-deploy may have left local changes in a stash in %s and cannot name the entry.", at)
	default:
		fmt.Fprintf(&b, "Self-deploy set the local changes in %s aside to fast-forward %s and could not put them "+
			"back. The work is not lost: it is in stash %s.", at, d.cfg.Branch, stash)
	}
	if detail != "" {
		fmt.Fprintf(&b, " Cause: %s.", detail)
	}
	if len(paths) > 0 {
		fmt.Fprintf(&b, " Affected paths (%s): %s.", textfmt.Count(len(paths), "path"), gitfail.RenderPaths(paths))
	}

	switch stash {
	case "":
		fmt.Fprintf(&b, " To recover: list the stack with `git -C %s stash list` and restore any entry labelled "+
			"`%s` with `git -C %s stash pop`.", at, stashMessage, at)
	default:
		// `stash apply <sha>` rather than `pop`, and the drop named separately:
		// apply addresses the entry by the SHA this message quotes, while pop
		// takes whatever is on top — which, on a stack shared with every worker
		// worktree, is not necessarily this one.
		fmt.Fprintf(&b, " To recover: inspect it with `git -C %s stash show -p %s`, then restore it with "+
			"`git -C %s stash apply %s` and resolve any conflict against the newly pulled commits. The entry stays on "+
			"the stack until you drop it (`git -C %s stash list` names its position).", at, stash, at, stash, at)
	}
	b.WriteString(" The deploy was abandoned before the rebuild, so the daemon still runs the binary it had. " +
		"This entry is NOT withdrawn by the next deploy that works: the checkout was left clean, so deploys resume " +
		"immediately and say nothing about where this work went. Forge withdraws it once no entry labelled " +
		"`" + stashMessage + "` is left on the stash stack — i.e. once you have restored it and dropped the entry.")
	return b.String()
}

// appendEvidence writes git's own words as the last section, under their own
// label, with whatever of the detail budget the rest of the message left.
//
// Trimming the evidence rather than the assembled string is the one safe order:
// a blind cut of the whole message lands mid-command, and a command truncated to
// a shorter pathspec is a pasted line that names a different set of files.
func appendEvidence(b *strings.Builder, evidence string) {
	if evidence == "" {
		return
	}
	const label = " git output: "
	budget := maxDeployDetailBytes - b.Len() - len(label)
	if budget < minEvidenceBytes {
		budget = minEvidenceBytes
	}
	b.WriteString(label)
	b.WriteString(gitfail.Bound(evidence, budget))
}
