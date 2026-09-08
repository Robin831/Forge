package smelter

import (
	"log"
	"time"

	"github.com/Robin831/Forge/internal/warden"
)

// This file is the anvil run's half of warden.SummarizeArchiveRun: one
// summary line per anvil per run, naming what the run took off the active
// rules file.
//
// It is one function both entry points call — the scheduled flush and the
// off-cycle `forge warden consolidate` — for the reason runStaleSweep and
// applyFileCap are: the two paths archive by the same rules, and a summary
// written twice is two answers to what one run removed, free to disagree the
// next time either is touched.

// duplicateArchiveEntries builds the archive entries for the rules Pass 1
// consolidation superseded: reason "duplicate", superseded_by naming the rule
// each was merged into.
//
// It exists because two callers need the same entries for opposite reasons —
// archiveRules to persist them, and the archive summary to count them and to
// see that their content moved forward into a rule that is still on the file.
// Derived twice, the summary could report a fold as a loss on the strength of
// a superseded_by mapping the persisted entries never carried.
//
// A rule the summary does not name is left with an empty superseded_by, which
// is what BuildSupersededByIndex reads as "merged into nothing" — the same
// reading archiveRules has always given it.
func duplicateArchiveEntries(replaced []warden.Rule, summary []warden.MergeResult, now time.Time) []warden.ArchivedRule {
	if len(replaced) == 0 {
		return nil
	}
	supersededBy := make(map[string]string, len(replaced))
	for _, m := range summary {
		for _, id := range m.ReplacedIDs {
			supersededBy[id] = m.Merged.ID
		}
	}
	entries := make([]warden.ArchivedRule, 0, len(replaced))
	for _, r := range replaced {
		entries = append(entries, warden.ArchivedRule{
			Rule:          r,
			SupersededBy:  supersededBy[r.ID],
			LastSeen:      now,
			ArchivedAt:    now,
			ArchiveReason: warden.ArchiveReasonDuplicate,
		})
	}
	return entries
}

// reportArchiveSummary renders and publishes the per-anvil archive summary for
// one run: before is the active rule set the run started from, after the set
// it is about to write, archived every entry the run is persisting (Pass 1
// duplicates and the stale/over-cap entries alike), and idx the supersession
// index the staleness guard was handed, so the summary names a chain by the
// same reading of the archive that decided whether to protect it.
//
// A run with nothing to say says nothing (ArchiveSummary.HasSubstance): a line
// of zeros on every flush of every anvil is the noise that buries the one
// flush where a class went unrepresented.
//
// The log line always carries the whole summary. The activity-feed event is
// emitted only when a class went unrepresented, because the counts already
// reach the feed from the passes that produced them (runStaleSweep's
// "Archived N stale rule(s)", applyFileCap's eviction row) and a second row
// repeating them would read as a second archive run. What is new here — and
// what nothing else can say — is the class list.
func reportArchiveSummary(anvilName string, before, after []warden.Rule,
	archived []warden.ArchivedRule, idx map[string][]string, emit func(message string)) warden.ArchiveSummary {

	summary := warden.SummarizeArchiveRunWithIndex(before, after, archived, idx)
	if !summary.HasSubstance() {
		return summary
	}
	line := summary.String()
	log.Printf("[smelter] %s for %s", line, anvilName)
	if emit != nil && len(summary.UnrepresentedClasses) > 0 {
		emit(line + " (" + anvilName + ")")
	}
	return summary
}
