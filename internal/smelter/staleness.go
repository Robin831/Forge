package smelter

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/warden"
)

// This file holds the one staleness-sweep implementation the scheduled flush
// (Smelter.runStaleness) and the off-cycle `forge warden consolidate`
// (ConsolidateAnvil) both run.
//
// It was two: each built its own warden.StaleConfig, read the archive, built
// the supersession index, logged the byte-identical failure sentence and
// emitted the byte-identical archived line — and the copies had already come
// apart in shape, one gating rf.Rules = sweep.Active on a condition the other
// expressed as an early return. That is the arrangement applyFileCap was
// extracted to remove, and it matters more here: the thing the two paths
// share is a GUARD, and a guard that quietly stops guarding on one of its two
// call sites is the failure it exists to prevent.

// supersessionIndex builds the reverse supersession index the terminus guard
// reads (warden.BuildSupersededByIndex) from two sources: the archive file
// beside the rules file, and the supersessions THIS run has decided on but
// not yet written.
//
// The second half is not an optimisation. Consolidation retires a rule by
// merging it into another and recording superseded_by on the archive entry,
// but archiveRules persists those entries only after the passes have all run,
// so a sweep reading the archive alone cannot see a chain the same run just
// created. And warden.MergeRule dates a merged rule from the OLDEST member it
// folded, carrying the newest member's emission — empty when the members were
// never emitted, which is exactly the population of old, unused near-duplicates
// Pass 1 exists to fold. So a merged rule can be aged AND inactive on the day
// it is created: without the pending entries, Pass 1 merges two 400-day-old
// rules into M, records superseded_by: M for both, and Pass 2 archives M in
// the same run — the whole chain retired at once with nothing left on the
// active file to say what went, which is the one shape of staleness the guard
// was written to refuse.
//
// The pending supersessions are folded in as synthetic archive entries rather
// than merged into the map by hand, so BuildSupersededByIndex's own rules
// (an empty target names no successor; an entry naming ITSELF is evidence of
// its own retirement) apply to them exactly as they do to the file's.
//
// The archive read is best-effort. Its only consumer is the terminus guard,
// which sits on top of the age test rather than gating it, so an unreadable
// archive costs the protection and never the sweep — the alternative is a
// sweep that stops archiving anything because a file it does not write could
// not be parsed. It is logged, since a guard that quietly stops guarding is
// the failure the guard exists to prevent.
func supersessionIndex(dir, anvilName string, pending []warden.MergeResult) map[string][]string {
	var entries []warden.ArchivedRule
	if archive, err := warden.LoadArchive(warden.ArchivePath(dir)); err != nil {
		log.Printf("[smelter] reading archive for %s to protect supersession termini: %v", anvilName, err)
	} else if archive != nil {
		entries = archive.Rules
	}
	for _, m := range pending {
		for _, id := range m.ReplacedIDs {
			entries = append(entries, warden.ArchivedRule{
				Rule:         warden.Rule{ID: id},
				SupersededBy: m.Merged.ID,
			})
		}
	}
	return warden.BuildSupersededByIndex(entries)
}

// runStaleSweep runs warden.ArchiveStale over rf, removing the stale rules in
// place and reporting both halves of the outcome: the entries to archive, and
// the rules the terminus guard held back. A caller that renders only the first
// turns the guard into a sweep that quietly keeps rules and says nothing.
//
// emit publishes one activity-feed message (nil for a caller with no feed).
// ann suppresses repeat announcements of the protected set — see
// terminusAnnouncer — and is nil for a one-shot operator command, where every
// run is a fresh artifact read once.
//
// When cfg disables the sweep (ArchiveAfterDays <= 0) or nothing was archived
// and nothing protected, rf is left untouched and both slices are empty.
func runStaleSweep(anvilName string, rf *warden.RulesFile, cfg warden.StaleConfig, now time.Time,
	emit func(message string), ann *terminusAnnouncer) ([]warden.ArchivedRule, []warden.Rule) {

	if rf == nil || cfg.ArchiveAfterDays <= 0 {
		return nil, nil
	}
	sweep := warden.ArchiveStale(rf.Rules, cfg, now)
	if len(sweep.Archived) == 0 && len(sweep.Protected) == 0 {
		return nil, nil
	}
	rf.Rules = sweep.Active

	if len(sweep.Archived) > 0 {
		log.Printf("[smelter] archived %d stale rule(s) for %s (age=%dd, inactive=%dd)",
			len(sweep.Archived), anvilName, cfg.ArchiveAfterDays, cfg.InactiveAfterDays)
		if emit != nil {
			emit(fmt.Sprintf("Archived %d stale rule(s) for %s", len(sweep.Archived), anvilName))
		}
	}

	// The full set is returned whatever the announcer says: the commit body,
	// the PR body and the CLI summary are fresh artifacts read once, and only
	// the log line and the feed row accumulate. Same split as
	// reportContradictions, which returns `found` and announces `fresh`.
	//
	// What the announcer decides is WHETHER this run says anything, never what
	// the sentence counts: the line is rendered from the whole protected set
	// with fresh named inside it, so the log and the feed never report a
	// smaller sweep than the commit body of the same run does.
	if len(sweep.Protected) > 0 {
		fresh := sweep.Protected
		if ann != nil {
			fresh = ann.unannounced(anvilName, sweep.Protected)
		}
		if len(fresh) > 0 {
			line := protectedTerminiLine(anvilName, sweep.Protected, fresh)
			log.Printf("[smelter] %s", line)
			if emit != nil {
				emit(line)
			}
		}
	}
	return sweep.Archived, sweep.Protected
}

// terminusAnnouncer remembers which protected supersession termini each anvil
// has already been told about, so a condition only a human can clear is
// announced once rather than on every flush.
//
// A rule reaches ReasonProtectedTerminus only after crossing BOTH staleness
// thresholds, and the sweep then leaves it on the file untouched — nothing
// about it changes between flushes, so the next flush finds the same rule in
// the same state. The only ways out are a review re-emitting it (at which
// point it reads recent-activity) or an operator setting
// warden.allow_archive_terminus; there is no verb to dismiss one. Without
// suppression a single protected terminus produces one log line and one feed
// row per flush cycle, forever, burying the smelter_flushed rows that report
// actual changes. That is the same failure contradictionAnnouncer exists to
// prevent, on the same code path.
//
// The memory is per process and deliberately not persisted, on
// contradictionAnnouncer's argument: a restart re-announcing each rule once is
// a fair price for keeping a rules-file concern out of state.db, and the set
// is bounded by the number of protected rules rather than by the number of
// flushes, which is the unbounded axis. The key is the anvil plus the rule ID,
// so one rule protected on two anvils is two announcements.
type terminusAnnouncer struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// unannounced returns the subset of rules this anvil has not been told about
// yet, in the order given, and records them as told.
func (a *terminusAnnouncer) unannounced(anvilName string, rules []warden.Rule) []warden.Rule {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = make(map[string]struct{})
	}
	var fresh []warden.Rule
	for _, r := range rules {
		key := anvilName + "\x00" + r.ID
		if _, dup := a.seen[key]; dup {
			continue
		}
		a.seen[key] = struct{}{}
		fresh = append(fresh, r)
	}
	return fresh
}
