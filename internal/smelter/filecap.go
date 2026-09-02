package smelter

import (
	"fmt"
	"log"
	"time"

	"github.com/Robin831/Forge/internal/warden"
)

// applyFileCap is the ONE eviction pass. Both entry points call it — the
// scheduled flush through Smelter.runFileCap and the off-cycle
// `forge warden consolidate` through ConsolidateAnvil — because the two had a
// copy each: the same call to warden.EvictOverCap, the same in-place mutation
// of rf, and the same two sentences written twice, which is exactly the shape
// that stops agreeing the next time either is touched.
//
// Evicted rules are removed from rf in place and returned as ArchivedRule
// entries so the caller persists them to the archive store. A ceiling of <= 0
// is the disable and makes this a no-op, as does a file already under it.
//
// emit is the optional event sink for the one message this pass produces (the
// scheduled path writes it to state.db as a smelter_flushed event, the CLI may
// pass nil); it is called only when something was actually evicted.
func applyFileCap(anvilName string, rf *warden.RulesFile, max int, now time.Time, emit func(message string)) []warden.ArchivedRule {
	if rf == nil || max <= 0 {
		return nil
	}
	active, evicted := warden.EvictOverCap(rf.Rules, max, now)
	if len(evicted) == 0 {
		return nil
	}
	rf.Rules = active
	log.Printf("[smelter] evicted %d rule(s) over the file ceiling for %s (max=%d, kept=%d)",
		len(evicted), anvilName, max, len(active))
	if emit != nil {
		emit(fmt.Sprintf("Evicted %d rule(s) over the %d-rule ceiling for %s", len(evicted), max, anvilName))
	}
	return evicted
}
