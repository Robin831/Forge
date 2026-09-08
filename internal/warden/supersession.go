package warden

// This file answers one question about the archive: which active rules other
// rules were merged INTO. It is the reverse of ArchivedRule.SupersededBy,
// which points forward from a retired rule to the one that replaced it, and
// the reverse direction is the one nothing could ask before — given a rule on
// the active file, is anything standing behind it?
//
// The staleness sweep is the reason it has to be askable. Consolidation
// retires a rule by merging its content into another and writing
// superseded_by on the archive entry, so the surviving rule carries the
// coverage of every rule behind it while looking, to an age test, exactly
// like any other rule. Archiving THAT rule retires the whole chain at once,
// and every member of it is already archived, so nothing left on the file
// says what went. That is the one shape of staleness worth refusing by
// default, and refusing it needs this index.

// BuildSupersededByIndex reads an archive into the reverse supersession map:
// an active rule's ID mapped to the IDs of the archived rules that name it in
// their superseded_by. A rule with no entry has nothing behind it.
//
// It is built once per sweep rather than queried per rule because it is a
// read of the whole archive, which is the file that only ever grows.
//
// Two kinds of pointer are dropped rather than indexed. An entry with an
// empty superseded_by was archived for staleness or the ceiling, not merged
// into anything, so it names no successor. An entry naming ITSELF is a
// degenerate record — the merged rule kept a replaced rule's ID (see
// ConsolidateWithParams, which guards against producing one) — and it is
// evidence that a rule was archived, not that anything stands behind the
// active rule that happens to share the ID. Indexing it would protect a rule
// on the strength of its own retirement.
func BuildSupersededByIndex(archived []ArchivedRule) map[string][]string {
	if len(archived) == 0 {
		return nil
	}
	idx := make(map[string][]string)
	for _, ar := range archived {
		target := ar.SupersededBy
		if target == "" || target == ar.Rule.ID {
			continue
		}
		idx[target] = append(idx[target], ar.Rule.ID)
	}
	if len(idx) == 0 {
		return nil
	}
	return idx
}

// IsSupersessionTerminus reports whether the rule is the end of a supersession
// chain: archived rules were merged into it, and it has no successor of its
// own.
//
// The second half needs no test here, and that is a property of where the
// rule came from rather than an omission. SupersededBy is archive bookkeeping
// — it lives on ArchivedRule and not on Rule — so a rule read off the ACTIVE
// file has, by construction, not been superseded by anything: it is still on
// the file. Being pointed at is therefore the whole test, and the caller
// (IsStale) is only ever handed active rules.
//
// A nil index yields false, which is the same answer as an index that
// contains nothing for this rule. The distinction — "no archive was read"
// versus "the archive names nothing behind this rule" — is deliberately not
// drawn: the guard sits on top of the age test rather than gating it, so a
// sweep on an anvil that has never archived anything behaves exactly as it
// did before this existed.
func IsSupersessionTerminus(rule Rule, idx map[string][]string) bool {
	if len(idx) == 0 || rule.ID == "" {
		return false
	}
	return len(idx[rule.ID]) > 0
}
