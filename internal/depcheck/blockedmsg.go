package depcheck

import (
	"context"
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/gitfail"
	"github.com/Robin831/Forge/internal/textfmt"
)

// The bounds, the cause table and the path enumeration/rendering primitives
// live in internal/gitfail. What is depcheck's is the prose below — which anvil
// stopped being scanned, and what to run about it. The machinery underneath it
// is not: selfdeploy describes a blocked pull with the same caps, the same
// annotations and the same byte-identity guard on a pasted pathspec, and two
// copies of that would drift by a cap and start naming different files.
const (
	maxBlockingPathsListed  = gitfail.MaxPathsListed
	maxBlockingPathsBytes   = gitfail.MaxPathsBytes
	maxCommandPathsListed   = gitfail.MaxCommandPathsListed
	maxCommandPathspecBytes = gitfail.MaxCommandPathspecBytes
	maxBlockingPathLen      = gitfail.MaxPathLen
)

// minEvidenceBytes is the floor on git's own words inside the assembled detail.
// The evidence is rendered last and is given whatever maxFailureDetailBytes has
// left, but a message that spent its budget on paths and still shows no evidence
// is unverifiable — the operator cannot check the claim against what git said.
const minEvidenceBytes = 200

// knownBlockingPaths annotates the paths a Forge-managed anvil is EXPECTED to
// carry modified. It is the shared table: the same two files are the ones an
// operator must not clean up in Forge's own checkout either.
var knownBlockingPaths = gitfail.KnownPaths

type blockedCause = gitfail.Cause

const (
	causeUnknown      = gitfail.CauseUnknown
	causeDirtyTree    = gitfail.CauseDirtyTree
	causeUnmerged     = gitfail.CauseUnmerged
	causeDetachedHead = gitfail.CauseDetachedHead
	causeRefLock      = gitfail.CauseRefLock
	causeNotARepo     = gitfail.CauseNotARepo
	causeBadRemote    = gitfail.CauseBadRemote
)

var (
	causePatterns          = gitfail.CausePatterns
	causeOf                = gitfail.CauseOf
	parsePorcelainZ        = gitfail.ParsePorcelainZ
	safeBlockingPath       = gitfail.SafePath
	annotateBlockingPath   = gitfail.Annotate
	expectedPathNames      = gitfail.ExpectedPathNames
	isExpectedBlockingPath = gitfail.IsExpectedPath
	renderBlockingPath     = gitfail.RenderPath
	listedPaths            = gitfail.ListedPaths
	renderBlockingPaths    = gitfail.RenderPaths
	shellQuotePath         = gitfail.ShellQuote
	commandPathspec        = gitfail.Pathspec
	unexpectedPaths        = gitfail.UnexpectedPaths
)

// dirtyPaths enumerates the working-tree paths that differ from HEAD in
// repoDir, read through this package's own runGit so the enumeration inherits
// the same deadline, stripped environment and LC_ALL=C pin every other git
// command here does.
func dirtyPaths(ctx context.Context, repoDir string) ([]string, error) {
	return gitfail.DirtyPaths(ctx, repoDir, runGit)
}

// remediation is the one command an operator can run, chosen by cause.
//
// Every command here is recoverable. `stash push` rather than `checkout --`,
// `merge --abort` named alongside "or resolve the conflicts": an escalation is
// read by somebody who has not yet looked at the checkout, and a destructive
// one-liner in that position throws away work the message never claimed to have
// inspected.
func remediation(cause blockedCause, anvil, checkout, origin string, paths []string) string {
	at := checkout
	if at == "" {
		at = "<checkout>"
	}
	switch cause {
	case causeBadRemote:
		// Forge cannot name the URL to restore. An anvil's config carries a
		// path, not a repository address; only the deployment's bootstrap knows
		// the address, which is why a pod restart fixes this and a scan does
		// not. So the remedy names the command and leaves the value to the
		// operator rather than inventing one.
		if gitfail.SelfReferentialRemote(origin, checkout) {
			// Worth its own sentence: this is not a remote that moved, it is a
			// remote pointing back inside the anvil at a worker's worktree,
			// which is deleted when the worker finishes. Nothing retries past
			// it, and nothing in Forge writes it — so the operator is being
			// told where to look for the writer as well as what to run.
			return fmt.Sprintf("To resolve: `origin` points at %s, a path inside this anvil's own checkout — a "+
				"repository is never its own upstream, and a worker worktree under `.workers/` is deleted when its "+
				"bead finishes. Repoint it with `git -C %s remote set-url origin <url>`. Forge never writes this "+
				"value, so something with a shell in the checkout did.", origin, at)
		}
		if origin != "" {
			return fmt.Sprintf("To resolve: `origin` is %s, which is not a repository git can read. Repoint it with "+
				"`git -C %s remote set-url origin <url>`.", origin, at)
		}
		return fmt.Sprintf("To resolve: the checkout's `origin` is not a repository git can read — inspect it with "+
			"`git -C %s remote -v` and repoint it with `git -C %s remote set-url origin <url>`.", at, at)

	case causeUnmerged:
		return fmt.Sprintf("To resolve: finish the merge in the checkout, or abandon it with `git -C %s merge --abort` "+
			"(`rebase --abort` if it is a rebase).", at)

	case causeDirtyTree:
		if len(paths) == 0 {
			// Nothing was enumerated: `git status` failed — the likeliest
			// outcome in the checkout an escalation is raised about — or there
			// was no checkout to run it in. Every sentence below is a claim
			// about a set that was inspected, and the "nothing unexpected is
			// modified" one is the worst of them to make here: it tells the
			// operator the checkout is fine while the entry exists precisely
			// because it is not. What a message claims must not outrun what it
			// did, so this says what to look at instead.
			return fmt.Sprintf("To resolve: Forge could not list what is modified here — inspect it with "+
				"`git -C %s status` and set the unexpected changes aside (recoverable) with "+
				"`git -C %s stash push -u -- <path>`, leaving %s in place.", at, at, expectedPathNames())
		}
		unexpected := unexpectedPaths(paths)
		if len(unexpected) == 0 {
			// Everything modified is a file this anvil is meant to carry, so
			// there is nothing to clean up and the blockage is elsewhere in the
			// checkout — say so rather than sending the operator to stash the
			// files that make bd work.
			return fmt.Sprintf("To resolve: nothing unexpected is modified — the paths above are the ones this anvil is "+
				"meant to carry, so inspect the rest of the checkout's git state with `git -C %s status`.", at)
		}
		// The trailing clause is only true when a path the operator can SEE was
		// annotated as expected. Read off the enumerated list rather than off the
		// whole set, since that list is itself capped: an annotation past the cap
		// is one the reader cannot check, so a clause resting on it names a set
		// the message does not contain.
		keep := ""
		if len(unexpectedPaths(listedPaths(paths))) != len(listedPaths(paths)) {
			keep = ", leaving the paths annotated as expected in place"
		}
		spec, covered := commandPathspec(unexpected)
		switch {
		case spec != "" && covered == len(unexpected):
			return fmt.Sprintf("To resolve: set the unexpected changes aside (recoverable) with "+
				"`git -C %s stash push -u -- %s`%s.", at, spec, keep)
		case spec != "":
			// "the first one", not "the first 1": both caps can leave a single
			// path covered — one deep path fills maxCommandPathspecBytes on its
			// own — and a sentence an operator reads aloud has to survive it.
			first := fmt.Sprintf("%d", covered)
			if covered == 1 {
				first = "one"
			}
			// More unexpected paths than fit one readable command. The command is
			// still worth pasting, but it is described as the partial step it is
			// and the operator is pointed at the full set — reported as "the
			// unexpected changes" it would read as the whole fix and leave the
			// next scan blocked on the paths it never named.
			return fmt.Sprintf("To resolve: %s are unexpectedly modified, more than fit one command — "+
				"`git -C %s stash push -u -- %s` sets the first %s aside (recoverable)%s; repeat it for the rest, "+
				"which `git -C %s status` lists in full.",
				textfmt.Count(len(unexpected), "path"), at, spec, first, keep, at)
		default:
			return fmt.Sprintf("To resolve: set the unexpected changes aside (recoverable) with "+
				"`git -C %s stash push -u -- <path>` for each of the %s that `git -C %s status` reports and this "+
				"message does not annotate as expected.", at, textfmt.Count(len(unexpected), "path"), at)
		}

	case causeDetachedHead:
		return fmt.Sprintf("To resolve: put the checkout back on a branch that tracks a remote, with "+
			"`git -C %s checkout <branch> && git -C %s branch --set-upstream-to=origin/<branch>`.", at, at)

	case causeRefLock:
		// "below": git's output is the LAST section of this message, under its
		// own label. Pointing the reader backwards past the headline is what the
		// whole reordering was against.
		return fmt.Sprintf("To resolve: no git process should be running here — remove the stale lock file git names "+
			"below under `git output:` (under `%s/.git`), then let the next scan retry.", at)

	case causeNotARepo:
		// The add is paired with the remove because this only ever fires for an
		// anvil that IS registered — the scan iterates the configured ones — and
		// `forge anvil add` refuses a name that already exists. Named alone it is
		// a command that cannot succeed as written, which leaves the operator
		// exactly where the escalation found them.
		return fmt.Sprintf("To resolve: the registered path is not a git checkout — point the anvil at one, either by "+
			"editing its `path` in `~/.forge/config.yaml` or with "+
			"`forge anvil remove %s && forge anvil add %s <path-to-checkout>`.", anvil, anvil)

	default:
		return fmt.Sprintf("To resolve: inspect the checkout's git state with `git -C %s status` — Forge has no specific "+
			"remedy modelled for this message.", at)
	}
}

// blockedMessage builds the operator-facing description of a blocked scan.
//
// The order is the point. What is blocked and where comes first, then the paths
// an operator can recognise the condition by, then the one command to run, then
// the sentence saying the entry withdraws itself — and git's raw output LAST,
// under its own label. It used to be the headline, which is exactly backwards:
// git's sentence is evidence for a claim, not the claim, and reading
// "fatal: You are not currently on a branch" as the first thing in a Needs
// Attention row says nothing about which anvil stopped being scanned or what to
// do about it.
//
// gitOut is expected to be already sanitized (failureEvidence); paths are not,
// and are sanitized here.
//
// The WHOLE assembled string is what maxFailureDetailBytes sizes — it is
// persisted as one DepcheckFailure.Detail and emitted as one event message — so
// each component is budgeted against it rather than beside it. The path list and
// the remediation's pathspec have their own caps above; git's evidence is
// rendered last and gets what those left, floored at minEvidenceBytes. Trimming
// the evidence rather than the assembled string is the one order that is safe:
// a blind cut of the whole message lands mid-command, and a command truncated to
// a shorter pathspec is a pasted line that names a different set of files.
func blockedMessage(anvil, checkout string, paths []string, gitOut, origin string) string {
	cause := causeOf(gitOut)

	var b strings.Builder
	fmt.Fprintf(&b, "Anvil %s is not being scanned for dependency updates at all — it is unscanned, not up to date. "+
		"Its manifests cannot be read from the upstream ref, and this repeats identically on every scheduled run "+
		"until the checkout is fixed.", anvil)
	if checkout != "" {
		fmt.Fprintf(&b, " Checkout: %s.", checkout)
	}

	if cause.InvolvesWorkingTree() && len(paths) > 0 {
		label := "Blocking paths"
		if cause == causeUnknown {
			// Nothing has established that the tree is what is blocking, so the
			// list is context rather than a diagnosis and is labelled as such.
			label = "Working tree"
		}
		fmt.Fprintf(&b, " %s (%s): %s.", label, textfmt.Count(len(paths), "path"), renderBlockingPaths(paths))
	}

	fmt.Fprintf(&b, " %s", remediation(cause, anvil, checkout, origin, paths))
	b.WriteString(" Forge clears this entry automatically once the anvil scans again.")

	if gitOut != "" {
		const label = " git output: "
		budget := maxFailureDetailBytes - b.Len() - len(label)
		if budget < minEvidenceBytes {
			budget = minEvidenceBytes
		}
		b.WriteString(label)
		b.WriteString(boundEvidence(gitOut, budget))
	}
	return b.String()
}
