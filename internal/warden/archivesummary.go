package warden

import (
	"fmt"
	"sort"
	"strings"

	diffpkg "github.com/Robin831/Forge/internal/diff"
)

// This file answers the question a run's archive counts cannot: WHAT left the
// active file, in the one sense an operator cannot recover from a count.
//
// A rule is retired either on its own — aged out, or evicted for a slot — or
// as a member of a supersession chain, where consolidation folded its content
// into a survivor and recorded superseded_by on the archive entry. The second
// kind is invisible in a count, because the survivor is one rule and the
// coverage behind it is several: archive the survivor and every rule it stood
// in for is already archived, so the file loses the whole class with nothing
// left on it to say what went. `archived 12 rules` reads identically whether
// those twelve were twelve independent checks or one chain twelve deep.
//
// Naming the classes is therefore the point, and counting them is explicitly
// not enough: the summary lists the terminus IDs so the rule that carried the
// chain can be recovered from the archive by name on the day it leaves,
// rather than found by an operator wondering months later why a convention
// stopped being reviewed.

// ArchiveSummary is what one anvil's archive run removed from the active
// file, split by the reason each entry carries, plus the supersession classes
// that ended the run with nothing on the active file to represent them.
//
// The counts and the class list answer different questions and neither
// substitutes for the other. Archived says how much a run took;
// UnrepresentedClasses says what it can no longer review.
type ArchiveSummary struct {
	// Archived is the total number of entries the run wrote to the archive.
	Archived int
	// Stale and Duplicate are that total split by ArchiveReason. OverCap is
	// the third reason this package produces (see ArchiveReasonOverCap), kept
	// apart on archivedByReason's argument: "aged out with no recent
	// activity", "folded into another rule" and "lost a slot to the ceiling"
	// are different claims about a rule.
	//
	// A reason none of the three recognises counts toward Archived and toward
	// none of the buckets, so a reason added later cannot be silently reported
	// as an existing one — the price is that the buckets need not sum to
	// Archived, which is the safe direction.
	Stale     int
	Duplicate int
	OverCap   int
	// UnrepresentedClasses names the supersession termini this run archived
	// while leaving no rule of their chain on the active file. Sorted and
	// deduplicated, so the rendered line is stable however the archive
	// entries were ordered, and one entry per chain rather than one per link
	// (see maximalClasses): a chain broken in a single run answers the
	// terminus test at every intermediate member, and a list of those reads
	// as several classes lost where one was.
	UnrepresentedClasses []string
}

// String renders the one-line form the anvil run logs:
//
//	archived 7 rules (5 stale, 2 duplicate), classes now unrepresented: log-name-matches-code
//
// The over-cap clause is appended only when the run evicted anything, so the
// ordinary line keeps its shape on the deployments where no ceiling bites.
//
// The trailing clause is always present, reading "none" when the run left
// every class represented. Omitted, a line that checked and found nothing
// would be byte-identical to one written before the check existed, which is
// the reading this summary exists to make impossible. It is capped at
// maxNamedClasses IDs with a count of the rest, since this is a one-line
// surface; the whole list stays on UnrepresentedClasses for the surfaces that
// render it in full.
func (s ArchiveSummary) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "archived %d rule(s) (%d stale, %d duplicate", s.Archived, s.Stale, s.Duplicate)
	if s.OverCap > 0 {
		fmt.Fprintf(&sb, ", %d over-cap", s.OverCap)
	}
	sb.WriteString("), classes now unrepresented: ")
	if len(s.UnrepresentedClasses) == 0 {
		sb.WriteString("none")
		return sb.String()
	}
	sb.WriteString(namedClasses(s.UnrepresentedClasses))
	return sb.String()
}

// maxNamedClasses bounds how many IDs the one-line form names. The class list
// is bounded only by how many of a run's archived entries are termini with no
// live chain member, and a single file-ceiling run can evict hundreds of rules
// at once — while this string is a daemon.log record and an activity-feed row
// Hearth renders as one line. It is the bound smelter's protectedTerminiLine
// puts on the same shape of list for the same reason; the multi-line commit
// body, the PR body and `forge warden consolidate` still carry the whole set
// off UnrepresentedClasses.
const maxNamedClasses = 5

// namedClasses renders the class IDs for that line: sanitized, and capped with
// a count of what the cap left out rather than trailing off.
func namedClasses(ids []string) string {
	safe := safeClassNames(ids)
	if len(safe) <= maxNamedClasses {
		return strings.Join(safe, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(safe[:maxNamedClasses], ", "), len(safe)-maxNamedClasses)
}

// HasSubstance reports whether the run has anything to say: it archived
// something, or it left a class unrepresented. A run that archived nothing
// renders a line of zeros, and logging that on every flush of every anvil is
// the noise that buries the line that matters.
//
// The second half is not redundant with the first. A class can only go
// unrepresented by way of an archived entry, so today the two agree — but the
// claim being made is about the class list, and a caller that gated on the
// count alone would silently stop reporting classes the day some other pass
// learns to retire a rule without an archive entry.
func (s ArchiveSummary) HasSubstance() bool {
	return s.Archived > 0 || len(s.UnrepresentedClasses) > 0
}

// safeClassNames sanitizes rule IDs for the rendered line. An ID is whatever
// the distillation JSON returned — nothing validates a character of one — and
// this text reaches daemon.log and an activity-feed row Hearth wraps without
// stripping, so it gets the closed alphabet diff.SafePath already defines for
// every other name Forge did not write but renders into prose it did. A
// second alphabet written here would be one more thing free to drift.
func safeClassNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, diffpkg.SafePath(id))
	}
	return out
}

// SummarizeArchiveRun describes one anvil's archive run: before is the active
// rule set the run started from, after is the set it ends with, and archived
// is the entries the run wrote to the archive store.
//
// The supersession index is derived from archived alone, which is the whole
// picture only for a chain this same run created. A caller that already holds
// the anvil's archive — every run of the scheduled flush and of
// `forge warden consolidate` does — should hand it to
// SummarizeArchiveRunWithIndex instead, since the predecessors that make a
// rule a terminus were, in the ordinary case, archived weeks earlier.
func SummarizeArchiveRun(before, after []Rule, archived []ArchivedRule) ArchiveSummary {
	return SummarizeArchiveRunWithIndex(before, after, archived, BuildSupersededByIndex(archived))
}

// SummarizeArchiveRunWithIndex is SummarizeArchiveRun over a supersession
// index the caller built from the whole archive (see BuildSupersededByIndex,
// and smelter.supersessionIndex for the archive-plus-pending form the passes
// already build for the staleness guard).
//
// It is the same index the terminus guard reads, deliberately: a rule the
// guard would have protected and an operator then archived with --force is
// exactly the rule this summary has to name, and two derivations of "what is
// standing behind this rule" would be free to disagree about which.
func SummarizeArchiveRunWithIndex(before, after []Rule, archived []ArchivedRule, idx map[string][]string) ArchiveSummary {
	summary := ArchiveSummary{Archived: len(archived)}
	for _, ar := range archived {
		switch ar.ArchiveReason {
		case ArchiveReasonStale, "":
			// An empty reason is rendered as "stale" everywhere else it is
			// read back, so it is counted as one here too.
			summary.Stale++
		case ArchiveReasonDuplicate:
			summary.Duplicate++
		case ArchiveReasonOverCap:
			summary.OverCap++
		}
	}

	activeIDs := ruleIDSet(after)
	startedActive := ruleIDSet(before)
	// A run can only supersede forward into a rule that is on the file it
	// wrote, so the successors worth testing are this run's own entries: an
	// older entry's target was resolved when that run reported itself.
	successorOf := make(map[string]string, len(archived))
	for _, ar := range archived {
		if ar.Rule.ID != "" && ar.SupersededBy != "" {
			successorOf[ar.Rule.ID] = ar.SupersededBy
		}
	}

	seen := make(map[string]struct{})
	var candidates []string
	for _, ar := range archived {
		id := ar.Rule.ID
		if id == "" {
			// BuildSupersededByIndex cannot key on an empty ID either, so
			// such an entry is never a terminus and there is nothing to name.
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if _, wasActive := startedActive[id]; !wasActive {
			// The run did not take this rule off the active file it started
			// from, so it is not this run's to report as having left it.
			continue
		}
		if !isUnrepresentedClass(id, activeIDs, successorOf, idx) {
			continue
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	classes := maximalClasses(candidates, seen, successorOf)
	sort.Strings(classes)
	summary.UnrepresentedClasses = classes
	return summary
}

// maximalClasses drops every candidate whose own successor is also a
// candidate, leaving one entry per chain rather than one per link.
//
// A chain broken in a single run reaches the loop above once per intermediate
// terminus: the on-disk archive holds a -> b, this run folds b into m and then
// evicts m, and both b and m answer isUnrepresentedClass — b because its one
// successor is archived rather than live, m because everything behind it is.
// Reported as they stand, the operator-facing count is a count of chain links,
// which is exactly the reading this file's header argues a count cannot be
// trusted for.
//
// The maximal member is the one to keep: m already carries b's merged content,
// so recovering m is recovering the class, and nothing recoverable is lost by
// leaving b off the list. The test is against the candidate set and not
// against the run's archived entries, so an id is only ever suppressed in
// favour of a successor that is itself being reported — a successor archived
// by this run but represented some other way (it was never on the active file,
// or its own chain moved forward into a live rule) leaves the predecessor
// named, which is the safe direction.
func maximalClasses(candidates []string, isCandidate map[string]struct{}, successorOf map[string]string) []string {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		next, ok := successorOf[id]
		if ok {
			if _, forward := isCandidate[next]; forward {
				continue
			}
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isUnrepresentedClass decides whether archiving id left its supersession
// chain with nothing on the active file.
//
// Three things have to hold, and the first is what keeps an ordinary retirement
// off the list: something must have been merged INTO id, which is exactly
// IsSupersessionTerminus' test. A rule nothing points at is a rule that stood
// for itself, and archiving it removes one check rather than a class of them —
// which is why a run that only folds redundant duplicates names nothing here,
// even though its entries are the ones carrying superseded_by.
//
// The second is that the class did not move FORWARD: a rule merged into a
// survivor that is still on the file has lost its ID and kept its coverage,
// so naming it would report a consolidation as a loss.
//
// The third is the chain itself. The walk is over the reverse index, so it
// visits everything that reaches id through any number of merges, and one
// member still on the active file — under its own ID, or through a successor
// this run recorded — is enough to represent the class. Anything the index
// cannot reach is not part of this chain, and the visited set makes a diamond
// or a cycle in the recorded pointers terminate rather than decide the answer.
func isUnrepresentedClass(id string, activeIDs map[string]struct{}, successorOf map[string]string, idx map[string][]string) bool {
	if len(idx[id]) == 0 {
		return false
	}
	visited := map[string]struct{}{id: {}}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, live := activeIDs[cur]; live {
			return false
		}
		if next, ok := successorOf[cur]; ok {
			if _, live := activeIDs[next]; live {
				return false
			}
		}
		for _, pred := range idx[cur] {
			if _, done := visited[pred]; done {
				continue
			}
			visited[pred] = struct{}{}
			queue = append(queue, pred)
		}
	}
	return true
}

// ruleIDSet projects rules onto the set of their non-empty IDs. Empty IDs are
// dropped rather than collected under "": one would otherwise make every
// unnamed archive entry look like it was still on the file.
func ruleIDSet(rules []Rule) map[string]struct{} {
	set := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		if r.ID == "" {
			continue
		}
		set[r.ID] = struct{}{}
	}
	return set
}
