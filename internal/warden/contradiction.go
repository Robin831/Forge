package warden

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Contradiction is a pair of rules learned from the SAME source that
// prescribe opposite orderings for the same operations. Both rules end up in
// the Warden's checklist, so whichever convention an implementation follows,
// one of them flags it — the review is guaranteed to produce a finding and
// the finding carries no information.
//
// PR #682 shipped two of these: `invoke-cancel-under-lock` against
// `unlock-before-callback`, and `persist-before-clearing-guard-flag` against
// `persist-before-inmemory-flag`. Both were caught by a human reading 90
// rules; neither was caught by anything in the pipeline.
//
// A contradiction is REPORTED and never resolved automatically. Deciding
// which ordering a codebase actually wants is a reading of the code, not of
// the two sentences — merging the pair would silently pick a winner, and
// dropping both would silently delete coverage. Detection exists to put the
// pair in front of whoever reviews the batch PR.
type Contradiction struct {
	// A and B are the two conflicting rules, ordered by ID so a pair is
	// reported the same way whichever order it was found in.
	A, B Rule
	// Source is the shared source reference (e.g. "copilot:PR#708") that
	// makes the pair worth reporting: two rules distilled from one PR are
	// about one piece of code.
	Source string
	// Kind names which detector fired — see ContradictionKind.
	Kind ContradictionKind
	// Detail is a one-line human-readable statement of the conflict,
	// suitable for a log line or a commit-message bullet.
	Detail string
}

// ContradictionKind identifies the axis on which two rules disagree.
type ContradictionKind string

const (
	// ContradictionLockScope: one rule says the operation happens while the
	// lock is held, the other says it happens after the lock is released.
	ContradictionLockScope ContradictionKind = "lock-scope"
	// ContradictionSequence: one rule says X happens before Y, the other
	// says Y happens before X.
	ContradictionSequence ContradictionKind = "sequence"
)

// minTopicOverlap is how alike two rules from one source must be before an
// opposing ordering claim is reported as a contradiction. One PR can
// legitimately produce a "flush before close" rule and a "close before
// unregister" one; those are different subjects that happen to share the
// word "close". Requiring a shared vocabulary keeps the report to pairs that
// are plausibly about the same code, which is what makes it worth a human's
// attention.
const minTopicOverlap = 0.20

// sentenceSplit breaks rule prose into clauses. Ordering claims are
// clause-local: "persist the row, then clear the flag; log the event before
// returning" holds two independent claims, and reading them as one produces
// a subject pair that appears in neither.
var sentenceSplit = regexp.MustCompile(`[.;:!?\n]+`)

// beforeRe and afterRe capture the two sides of an explicit ordering claim.
// The capture windows are bounded to claimWindow characters so a claim
// cannot absorb the whole clause: fifteen words away from the ordering word
// are no longer the operations being ordered, and letting them in is what
// makes two unrelated rules from one PR look like they cross.
//
// `after` is the only synonym on the second list. `once` and `following`
// were tried and removed: both are far more often adverb and preposition
// than temporal conjunction in review prose ("computed once per poll cycle",
// "the following fields"), and each one they matched produced a claim whose
// operands were the two halves of a sentence that ordered nothing.
var (
	beforeRe = regexp.MustCompile(`(?i)(.{0,80})\b(?:before|prior to|ahead of)\b(.{0,80})`)
	afterRe  = regexp.MustCompile(`(?i)(.{0,80})\bafter\b(.{0,80})`)
)

// claimBoilerplate is dropped from a sequence claim's operands. Every check
// in the corpus opens with "Verify" or "Ensure", so leaving those tokens in
// makes any two claims share an operand — which is exactly one half of the
// crossing test, and enough on its own to report two rules that order
// entirely different things.
var claimBoilerplate = map[string]struct{}{
	"verify": {}, "verifies": {}, "verified": {},
	"ensure": {}, "ensures": {}, "ensured": {},
	"check": {}, "checks": {}, "checked": {},
	"confirm": {}, "confirms": {}, "confirmed": {},
	"code": {}, "rule": {}, "reviewer": {}, "review": {},
}

// insideLockRe and outsideLockRe recognise the two answers to "is the lock
// held across this call?". They are their own axis rather than a sequence
// claim because both phrasings put the same words in the same order —
// "invoke the callback under the lock" and "unlock before invoking the
// callback" are opposite instructions that a before/after parse reads as
// agreeing.
var (
	insideLockRe  = regexp.MustCompile(`(?i)(?:under the (?:lock|mutex)|while holding the (?:lock|mutex)|holding the (?:lock|mutex)|with the (?:lock|mutex) held|while [^,]{0,40}?(?:lock|mutex) is held|inside the (?:lock|critical section))`)
	outsideLockRe = regexp.MustCompile(`(?i)(?:outside the (?:lock|mutex|critical section)|after releasing the (?:lock|mutex)|releas\w* the (?:lock|mutex) (?:first|before)|unlock\w* (?:first|before)|without holding the (?:lock|mutex)|not (?:while )?holding the (?:lock|mutex)|never (?:while )?holding the (?:lock|mutex))`)
)

// orderClaim is one normalized ordering statement extracted from a rule.
// A sequence claim carries the token sets on either side of the ordering
// word; a lock-scope claim carries only which side of the lock the call
// sits on.
type orderClaim struct {
	kind   ContradictionKind
	early  TokenSet
	late   TokenSet
	inside bool
}

// extractClaims pulls every ordering claim out of a rule's Check — and out
// of Check ALONE.
//
// Pattern is deliberately not read: by the schema's own definition it
// describes the code shape that TRIGGERS the rule, which for an ordering
// rule is routinely the anti-pattern the Check tells you to avoid. The
// pair that motivated this detector demonstrates it — `unlock-before-callback`
// has the pattern "a callback is invoked while the registry mutex is held"
// and the check "unlock before invoking the callback". Reading both fields
// makes that one rule contradict itself, and every ordering rule with a
// well-written pattern along with it.
func extractClaims(r Rule) []orderClaim {
	var out []orderClaim
	for _, clause := range sentenceSplit.Split(r.Check, -1) {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		out = append(out, clauseClaims(clause)...)
	}
	return out
}

// clauseClaims extracts the claims held by a single clause. A clause can
// yield both a lock-scope claim and a sequence claim; they are separate
// readings and either may be the one that conflicts with another rule.
func clauseClaims(clause string) []orderClaim {
	var out []orderClaim

	// Lock scope is tested first and, when it matches, the clause yields no
	// sequence claim: "unlock before invoking" is a lock-scope statement
	// whose before/after reading ("unlocking precedes invoking") agrees with
	// its own opposite and would mask the real conflict.
	inside := insideLockRe.MatchString(clause)
	outside := outsideLockRe.MatchString(clause)
	switch {
	case inside && outside:
		// A clause asserting both (e.g. "hold the lock, do not invoke the
		// callback while holding the mutex") cannot be reduced to one side.
		return nil
	case inside || outside:
		return []orderClaim{{kind: ContradictionLockScope, inside: inside}}
	}

	if m := beforeRe.FindStringSubmatch(clause); m != nil {
		if c, ok := sequenceClaim(m[1], m[2]); ok {
			out = append(out, c)
		}
	}
	if m := afterRe.FindStringSubmatch(clause); m != nil {
		// "X after Y" is "Y before X".
		if c, ok := sequenceClaim(m[2], m[1]); ok {
			out = append(out, c)
		}
	}
	return out
}

// sequenceClaim builds a claim from the two sides of an ordering word,
// dropping the pair when either side carries no significant token — an
// ordering with an unnamed operand orders nothing.
func sequenceClaim(earlyText, lateText string) (orderClaim, bool) {
	early := stripTokens(Tokenize(earlyText), claimBoilerplate)
	late := stripTokens(Tokenize(lateText), claimBoilerplate)
	if len(early) == 0 || len(late) == 0 {
		return orderClaim{}, false
	}
	return orderClaim{kind: ContradictionSequence, early: early, late: late}, true
}

// stripTokens returns set minus drop, leaving set untouched.
func stripTokens(set TokenSet, drop map[string]struct{}) TokenSet {
	out := make(TokenSet, len(set))
	for t := range set {
		if _, skip := drop[t]; skip {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

// intersects reports whether two token sets share at least one token.
func intersects(a, b TokenSet) bool {
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	for t := range small {
		if _, ok := large[t]; ok {
			return true
		}
	}
	return false
}

// opposed reports whether two claims prescribe opposite orderings.
func opposed(a, b orderClaim) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case ContradictionLockScope:
		// No subject test: a lock-scope claim carries no operands to cross,
		// and the two gates that matter have already run — the rules were
		// distilled from the same PR and their vocabularies overlap past
		// minTopicOverlap, so they are about the same code. Demanding a
		// shared token on top of that loses the real pair, whose checks say
		// "invoked under the lock" and "unlocks before invoking the
		// callback" and share nothing but review boilerplate.
		return a.inside != b.inside
	case ContradictionSequence:
		// The operands must cross: what one rule puts first, the other puts
		// second, and vice versa. Requiring BOTH crossings is what separates
		// "persist before clearing the flag" / "set the flag before
		// persisting" from two rules that merely both mention persisting.
		return intersects(a.early, b.late) && intersects(a.late, b.early)
	}
	return false
}

// DetectContradictions reports pairs of rules from the same source that
// prescribe opposite orderings. It is deliberately conservative — a pair is
// reported only when the two rules share a source reference, share enough
// vocabulary to be about the same code (minTopicOverlap), and hold claims
// that cross on both operands — because the output is read by a human and a
// false report costs more attention than the check saves.
//
// Results are ordered by source, then by the two rule IDs, so a batch
// produces the same report every run.
func DetectContradictions(rules []Rule) []Contradiction {
	if len(rules) < 2 {
		return nil
	}

	// Precompute per rule: the ordering claims, the word bag and the
	// normalized source set. The pair loop is O(n²) over the whole rules
	// file, so anything derived from a single rule is derived once.
	claims := make([][]orderClaim, len(rules))
	bags := make([]TokenSet, len(rules))
	sources := make([]map[string]string, len(rules))
	for i, r := range rules {
		claims[i] = extractClaims(r)
		bags[i] = RuleWordBag(r)
		sources[i] = normalizedSources(r)
	}

	// Deduplicate on the ID pair: two rules sharing several sources must not
	// be reported once per shared source.
	seen := make(map[string]struct{})
	var out []Contradiction

	for i := 0; i < len(rules); i++ {
		if len(claims[i]) == 0 {
			continue
		}
		for j := i + 1; j < len(rules); j++ {
			if len(claims[j]) == 0 {
				continue
			}
			shared := sharedSource(sources[i], sources[j])
			if shared == "" {
				continue
			}
			if Overlap(bags[i], bags[j]) < minTopicOverlap {
				continue
			}
			kind, ok := opposingKind(claims[i], claims[j])
			if !ok {
				continue
			}
			a, b := rules[i], rules[j]
			if b.ID < a.ID {
				a, b = b, a
			}
			key := a.ID + "\x00" + b.ID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, Contradiction{
				A: a, B: b, Source: shared, Kind: kind,
				Detail: contradictionDetail(kind, a, b, shared),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].A.ID != out[j].A.ID {
			return out[i].A.ID < out[j].A.ID
		}
		return out[i].B.ID < out[j].B.ID
	})
	return out
}

// opposingKind returns the kind of the first opposing claim pair found.
// Lock-scope conflicts are reported in preference to sequence ones when a
// pair holds both: the lock reading is the more specific statement about
// what the code must do.
func opposingKind(a, b []orderClaim) (ContradictionKind, bool) {
	var seqHit bool
	for _, ca := range a {
		for _, cb := range b {
			if !opposed(ca, cb) {
				continue
			}
			if ca.kind == ContradictionLockScope {
				return ContradictionLockScope, true
			}
			seqHit = true
		}
	}
	if seqHit {
		return ContradictionSequence, true
	}
	return "", false
}

// normalizedSources folds a rule's Source list into a lookup keyed by the
// case-folded, whitespace-trimmed reference, holding the original spelling
// as the value. A source token is model-authored text, so two rules can name
// one PR with different casing or padding.
func normalizedSources(r Rule) map[string]string {
	if len(r.Source) == 0 {
		return nil
	}
	out := make(map[string]string, len(r.Source))
	for _, s := range r.Source {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		out[strings.ToLower(trimmed)] = trimmed
	}
	return out
}

// sharedSource returns a source reference the two rules have in common, or
// "" when they share none. When several are shared the lexicographically
// smallest key wins, so the reported source does not depend on map order.
func sharedSource(a, b map[string]string) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	best := ""
	for key := range small {
		if _, ok := large[key]; !ok {
			continue
		}
		if best == "" || key < best {
			best = key
		}
	}
	if best == "" {
		return ""
	}
	return small[best]
}

// contradictionDetail renders the one-line statement carried on the record.
func contradictionDetail(kind ContradictionKind, a, b Rule, source string) string {
	switch kind {
	case ContradictionLockScope:
		return fmt.Sprintf("%s and %s (both from %s) disagree on whether the lock is held across the call", a.ID, b.ID, source)
	default:
		return fmt.Sprintf("%s and %s (both from %s) prescribe opposite orderings for the same operations", a.ID, b.ID, source)
	}
}

// FormatContradictions renders the pairs as a bullet list for a commit
// message or PR body. Returns "" when there are none so callers can omit the
// section entirely.
//
// It states plainly that nothing was resolved: a section that merely listed
// the pairs under a "Consolidated" heading would read as work already done.
func FormatContradictions(cs []Contradiction) string {
	if len(cs) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Contradictions: %d pair(s) — NOT resolved, a human must pick the convention\n", len(cs))
	for _, c := range cs {
		fmt.Fprintf(&sb, "- [%s] %s\n", c.Kind, c.Detail)
	}
	return strings.TrimRight(sb.String(), "\n")
}
