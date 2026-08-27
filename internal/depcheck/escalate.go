package depcheck

import (
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
func (s *Scanner) reportScanFailure(name, path string, err error) {
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

	s.escalateBlocked(name, gitFailureSignature(name, kind, failureEvidence(err)), blockedFailureDetail(name, path, err))
}

// escalateBlocked raises the anvil's blocking condition with the operator, or
// stays silent when it has already been raised.
//
// sig is what "already been raised" means: two runs stopped by the same
// condition produce the same signature, and the store answers whether it has
// seen it. detail is the human-readable content of the escalation and is
// deliberately the only thing this function does not decide — it is built by
// blockedFailureDetail, which is the seam the sibling bead (Forge-0uvl)
// replaces with enumerated blocking paths and a remediation command without
// touching classification or suppression.
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
// scan: what is blocked, what git said, and what state it leaves the anvil in.
//
// This is the seam the message-detail sibling bead (Forge-0uvl) owns. It is a
// single function taking the anvil, its checkout and the error precisely so
// that enumerating the blocking paths and naming the remediation command is a
// change to this body alone — the classification above it and the suppression
// beside it read only its output, never its shape.
func blockedFailureDetail(anvil, path string, err error) string {
	evidence := failureEvidence(err)
	var b strings.Builder
	fmt.Fprintf(&b, "Its dependency manifests cannot be read from the upstream ref, so the anvil is not being scanned at all "+
		"(it is unscanned, not up to date). This will repeat identically on every scheduled run until the checkout is fixed. ")
	if path != "" {
		fmt.Fprintf(&b, "Checkout: %s. ", path)
	}
	if evidence != "" {
		fmt.Fprintf(&b, "git said: %s. ", evidence)
	}
	b.WriteString("Forge clears this entry automatically once the anvil scans again.")
	return b.String()
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
	if len(clean) > maxFailureDetailBytes {
		// The marker is inside the bound, not on top of it: maxFailureDetailBytes
		// is what a row and a feed line are sized in, so a cut that then appends
		// three more bytes overshoots the very number it is enforcing.
		clean = textfmt.TruncateRunes(clean, maxFailureDetailBytes-len(evidenceEllipsis)) + evidenceEllipsis
	}
	return clean
}
