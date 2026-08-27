package depcheck

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/termtext"
	"github.com/Robin831/Forge/internal/textfmt"
)

// maxFailureDetailBytes bounds what reaches a persisted needs-attention row and
// a rendered event message. git is normally terse, but a fetch against a remote
// with hundreds of refs can answer with one line per ref, and that whole wall
// would otherwise become the operator's headline.
//
// It bounds the WHOLE detail, not one component of it. failureEvidence applies
// it to git's words on the transient exit, where they are the message; on the
// blocked exit blockedMessage assembles a headline, a path list, a remediation
// command and that same evidence, and budgets each against this number so the
// assembled string still fits it. A bound applied per component adds up to a
// multiple of itself, which is a row nobody reads to the end of.
const maxFailureDetailBytes = 2000

// evidenceEllipsis marks a truncated evidence string. It is named because
// failureEvidence budgets for its length rather than appending it on top of the
// bound above.
const evidenceEllipsis = "…"

// reportScanFailure is the one place a failure to read an anvil's manifests is
// reported, and the only place the transient/blocked distinction changes what
// happens.
//
// A TRANSIENT (or unclassifiable) failure keeps the behaviour depcheck has
// always had: a log line and a depcheck_failed event, and the next scheduled
// run tries again. Nothing is persisted, because there is nothing for an
// operator to do.
//
// A BLOCKED failure is escalated exactly once. The condition will reproduce on
// every run until somebody changes the checkout, so re-emitting depcheck_failed
// nightly buries the one event that mattered under a hundred identical ones —
// which is how an anvil went unscanned for weeks with the evidence in the log
// the whole time. Instead the anvil gets a needs-attention entry, one event
// naming the condition, and silence until the condition CHANGES or clears.
func (s *Scanner) reportScanFailure(ctx context.Context, name, path string, err error) {
	kind := classifyGitError(err)

	if kind != gitFailureBlocked {
		// git's words go through failureEvidence on this exit too, not just the
		// blocked one below. The text is equally untrusted on both — a
		// server-side hook's rejection is echoed verbatim by the fetch — and it
		// reaches the same two rendered surfaces (daemon.log and an activity
		// feed row Hearth wraps without stripping), so escape sequences or
		// embedded newlines in it would forge feed and log lines. Classification
		// is by substring match, which means a message that avoids every blocked
		// pattern lands here by construction.
		msg := fmt.Sprintf("could not read dependency manifests for anvil %s from its upstream ref — skipping depcheck to avoid stale results: %s",
			name, failureEvidence(err))
		log.Printf("[depcheck] %s", msg)
		s.emit(state.EventDepcheckFailed, msg, "", name)
		return
	}

	// The signature is still taken from git's evidence alone, never from the
	// enumerated detail: the blocking paths are what the operator READS, and a
	// tree whose modified set shifts by one file between two runs is the same
	// condition, which a signature over the detail would re-escalate nightly.
	s.escalateBlocked(name, gitFailureSignature(name, kind, failureEvidence(err)), blockedFailureDetail(ctx, name, path, err))
}

// escalateBlocked raises the anvil's blocking condition with the operator, or
// stays silent when it has already been raised.
//
// sig is what "already been raised" means: two runs stopped by the same
// condition produce the same signature, and the store answers whether it has
// seen it. detail is the human-readable content of the escalation and is
// deliberately the only thing this function does not decide — it is built by
// blockedFailureDetail, which enumerates the blocking paths and names the
// remediation command without touching classification or suppression.
//
// A store that cannot answer escalates: an operator told twice about a real
// blockage is a nuisance, an operator never told is the bug being fixed.
func (s *Scanner) escalateBlocked(name, sig, detail string) {
	failure := state.DepcheckFailure{
		Anvil:     name,
		Kind:      state.DepcheckKindBlocked,
		Signature: sig,
		Detail:    detail,
	}
	// The headline is the record's to render (state.DepcheckFailure.Title), the
	// same way a deploy failure renders its own: written here as well it would
	// be one sentence in two packages, and a row escalated before an edit would
	// keep rendering the other one.
	title := failure.Title()

	fresh := true
	if s.failures != nil {
		var err error
		fresh, err = s.failures.RecordDepcheckFailure(failure)
		if err != nil {
			log.Printf("[depcheck] %s: could not record blocked dependency scan — escalating anyway: %v", name, err)
			fresh = true
		}
	}

	if !fresh {
		// Suppressed, not forgotten: the needs-attention entry raised by the
		// first occurrence is still there, and the row's last_seen and
		// occurrence count have just been refreshed by the record above.
		log.Printf("[depcheck] %s: dependency scan still blocked by the same condition (%s) — already escalated, staying quiet", name, sig)
		return
	}

	log.Printf("[depcheck] %s: dependency scan BLOCKED — %s", name, detail)
	s.emit(state.EventDepcheckFailed, fmt.Sprintf("%s. %s", title, detail), "", name)
}

// clearBlocked withdraws an anvil's blocked entry after a scan that read its
// manifests. It is called on the success path rather than on any particular
// remedy because the scan succeeding IS the proof the condition is gone —
// and because a flag nothing clears automatically ends up permanently set and
// ignored, which is the same as not having it.
//
// Clearing also re-arms the escalation: the same condition recurring after a
// fix is a new occurrence an operator has not been told about, so it escalates
// again rather than being suppressed by a signature from weeks ago.
func (s *Scanner) clearBlocked(name string) {
	if s.failures == nil {
		return
	}
	cleared, err := s.failures.ClearDepcheckFailure(name)
	if err != nil {
		log.Printf("[depcheck] %s: could not clear blocked dependency scan flag: %v", name, err)
		return
	}
	if !cleared {
		return
	}
	msg := fmt.Sprintf("Anvil %s: dependency scan unblocked — manifests read successfully", name)
	log.Printf("[depcheck] %s", msg)
	s.emit(state.EventDepcheckPassed, msg, "", name)
}

// pruneBlocked drops entries for anvils that are no longer registered. Their
// entries are cleared by a successful scan of that anvil, which a deregistered
// anvil never gets, so without this a removed anvil keeps a needs-attention row
// no action can resolve.
func (s *Scanner) pruneBlocked(anvils map[string]string) {
	if s.failures == nil || len(anvils) == 0 {
		return
	}
	keep := make([]string, 0, len(anvils))
	for name := range anvils {
		keep = append(keep, name)
	}
	if err := s.failures.PruneDepcheckFailures(keep); err != nil {
		log.Printf("[depcheck] could not prune blocked dependency scan flags: %v", err)
	}
}

// blockedFailureDetail renders the operator-facing description of a blocked
// scan: which anvil stopped being scanned, the paths an operator can recognise
// the condition by, the one command that resolves it, and — last, under its own
// label — git's raw output as the evidence for all of it.
//
// This is the one seam the message content lives on. It takes the anvil, its
// checkout and the error precisely so that enumerating the blocking paths and
// naming the remediation command is a change to this body alone: the
// classification above it and the suppression beside it read only its output,
// never its shape.
//
// The path enumeration is best-effort by construction. `git status` in a
// checkout already known to be in a bad state is exactly the command that might
// also fail, and an escalation with no path list is worth far more than no
// escalation at all — so a failure here is logged and the message is built
// without it.
func blockedFailureDetail(ctx context.Context, anvil, path string, err error) string {
	evidence := failureEvidence(err)

	var paths []string
	if dirty, dirtyErr := dirtyPaths(ctx, path); dirtyErr != nil {
		// failureEvidence rather than %v: this is git's own words reaching
		// daemon.log, which is the surface, and so the treatment, the escalated
		// detail beside it already gets. A gitError interpolates stderr verbatim,
		// and `git status` is being run in a checkout Forge did not author whose
		// diagnostics routinely echo paths and ref names out of it — unbounded
		// and unstripped, a multi-line stderr becomes several apparent log
		// records and an escape sequence in a filename is executed by whatever
		// tails the log.
		log.Printf("[depcheck] %s: could not enumerate blocking paths for the escalation: %s", anvil, failureEvidence(dirtyErr))
	} else {
		paths = dirty
	}

	return blockedMessage(anvil, path, paths, evidence)
}

// failureEvidence renders git's own words for a failure, bounded and stripped
// of anything a terminal would interpret.
//
// The text is git's, and git's is partly the remote's (a server-side hook's
// rejection message is echoed verbatim), so it is text Forge did not write
// reaching a rendered needs-attention row and an activity-feed line —
// termtext.Line is what every such surface goes through.
func failureEvidence(err error) string {
	if err == nil {
		return ""
	}
	raw := gitStderr(err)
	if raw == "" {
		raw = err.Error()
	}
	// Newlines are the reason this is not just a trim: git writes one line per
	// ref, and a feed row is one line.
	clean := termtext.Line(strings.TrimSpace(raw))
	clean = strings.Join(strings.Fields(clean), " ")
	return boundEvidence(clean, maxFailureDetailBytes)
}

// boundEvidence truncates already-sanitized evidence to a byte bound.
//
// The marker is inside the bound, not on top of it: the bound is what a row and
// a feed line are sized in, so a cut that then appends three more bytes
// overshoots the very number it is enforcing. It is a parameter rather than
// maxFailureDetailBytes directly because the blocked exit gives git's words
// whatever the rest of the assembled detail left of that same total.
func boundEvidence(s string, maxBytes int) string {
	if maxBytes <= len(evidenceEllipsis) {
		maxBytes = len(evidenceEllipsis) + 1
	}
	if len(s) <= maxBytes {
		return s
	}
	return textfmt.TruncateRunes(s, maxBytes-len(evidenceEllipsis)) + evidenceEllipsis
}
