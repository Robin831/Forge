package smelter

import (
	"fmt"
	"log"
	"sync"

	"github.com/Robin831/Forge/internal/textfmt"
	"github.com/Robin831/Forge/internal/warden"
)

// reportContradictions is the one detect-and-announce implementation the
// scheduled flush and `forge warden consolidate` share.
//
// It was two: both call sites ran warden.DetectContradictions and then
// emitted the byte-identical "[smelter] WARNING contradictory rules for %s"
// line, and the copies had already come apart — only the flush emitted an
// activity-feed event, so the CLI path reported a contradiction to a
// terminal and to nothing else.
//
// It returns EVERY pair found, not just the newly announced ones: the batch
// PR's commit message and body must list the full set (each PR is a fresh
// artifact a reviewer reads once), while the log line and the feed row are
// the surfaces that accumulate. Which of those two a caller gets is decided
// by the announcer it passes.
func reportContradictions(anvilName string, rules []warden.Rule, ann *contradictionAnnouncer, event func(name, message string)) []warden.Contradiction {
	found := warden.DetectContradictions(rules)
	if len(found) == 0 {
		return nil
	}

	fresh := found
	if ann != nil {
		fresh = ann.unannounced(anvilName, found)
	}
	for _, c := range fresh {
		log.Printf("[smelter] WARNING contradictory rules for %s: %s", anvilName, c.Detail)
	}
	if len(fresh) > 0 && event != nil {
		event("smelter_flushed",
			fmt.Sprintf("%s for %s — not resolved automatically",
				textfmt.Count(len(fresh), "contradictory rule pair"), anvilName))
	}
	return found
}

// contradictionAnnouncer remembers which pairs an anvil has already been
// told about, so a condition only a human can clear is announced once rather
// than on every flush.
//
// A contradiction is reported and never resolved, and the rules file only
// changes when somebody edits it, so the set found on each flush is
// monotonic: without suppression every flush of an anvil re-emits every pair
// ever discovered — one WARNING line and one feed row per pair, forever,
// with no verb to dismiss one. That is the failure depcheck's blocked-scan
// escalation and selfdeploy's sticky stash reason both exist to prevent, on
// the stated grounds that the identical event every night is what buries the
// one that matters.
//
// The memory is per process and deliberately not persisted. A restart
// re-announcing each pair once is a fair price for keeping a rules-file
// concern out of state.db, and it is bounded by the number of pairs rather
// than by the number of flushes, which is the unbounded axis. The key is the
// ordered ID pair — warden.ContradictionKey, the same key
// DetectContradictions dedupes a single scan on — so one pair reported under
// two shared sources is still one announcement.
type contradictionAnnouncer struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// unannounced returns the subset of cs this anvil has not been told about
// yet, in the order given, and records them as told.
func (a *contradictionAnnouncer) unannounced(anvilName string, cs []warden.Contradiction) []warden.Contradiction {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = make(map[string]struct{})
	}
	var fresh []warden.Contradiction
	for _, c := range cs {
		key := anvilName + "\x00" + warden.ContradictionKey(c)
		if _, dup := a.seen[key]; dup {
			continue
		}
		a.seen[key] = struct{}{}
		fresh = append(fresh, c)
	}
	return fresh
}
