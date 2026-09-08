package warden

import (
	"log"
	"sync"
	"time"

	diffpkg "github.com/Robin831/Forge/internal/diff"
)

// emitMu serialises the stamp below across the reviews running in one daemon.
// The stamp is a read-modify-write of a whole file, so two concurrent reviews
// of two beads in the same anvil would otherwise race and the loser's
// emissions would be silently overwritten — an undercount, and undercounting
// is the direction the consumers tolerate, but there is no reason to accept it
// when the window is one small file read and one write.
//
// What it does NOT serialise against is every other writer of the same file:
// in this process the learner (LearnFromCIFix saves the anvil's rules file
// directly) and the smelter's flush, out of it a `forge warden consolidate`
// run or a second daemon on the same anvil. That is why recordEmissions
// re-reads the file immediately before writing it rather than saving the copy
// the checklist was built from. A stamp can still be lost to one of those
// writers, which is the tolerated undercount; a RULE cannot, which the stale
// copy would have cost — the learner's window between its own load and its
// save is a Claude session, minutes long, so a review saving the rules it
// loaded before that save would drop the newly learned rule outright, with no
// error, no log line and no distillation left to re-derive it.
var emitMu sync.Mutex

// learnedRulesSection builds the "Learned Review Rules" section of a review
// prompt and records the emission against the rules that reached it.
//
// The whole cycle — load, select, stamp, save — is one function and one lock
// hold because the stamp addresses rules by POSITION in the file it was
// selected from, and a position is only meaningful against a known slice.
// recordEmissions is what makes that safe against the writers emitMu cannot
// cover: it re-reads the file and verifies each position still holds the rule
// that was selected before stamping it.
//
// now is a parameter so every rule in one checklist carries the same
// timestamp and a test can assert on it rather than on the clock.
//
// Nothing here fails a review. A rules file that cannot be read yields no
// section (the behaviour before any of this existed), and a stamp that cannot
// be written is logged: the review has already been built, and losing a
// telemetry write is not a reason to refuse to review the diff.
func learnedRulesSection(beadID, anvilPath, diff string, now time.Time) string {
	emitMu.Lock()
	defer emitMu.Unlock()

	rf, err := LoadRules(anvilPath)
	if err != nil {
		log.Printf("[warden:%s] loading learned review rules for %s failed: %v", beadID, anvilPath, err)
		return ""
	}

	changedFiles := diffpkg.ChangedFiles(diff)
	checklist, stats, emitted := rf.FormatChecklistForDiffWithSelection(diff, changedFiles, GetActiveFilterConfig())
	// One line per review saying how far the rules file got. Without it the
	// only observable is the checklist, which looks identical whether the
	// cap discarded nothing or nine hundred candidates.
	log.Printf("[warden:%s] %s", beadID, stats.Line())

	if checklist == "" {
		return ""
	}

	// Only the rules that actually reached the checklist are stamped. The
	// unfiltered FormatChecklist — the Smith self-review checklist in combined
	// mode — deliberately stamps nothing: it renders every rule on file, so
	// crediting an emission there would mark the whole file alive on every run
	// and leave the counter unable to say which rules were CHOSEN, which is
	// the only question the staleness sweep asks it. Under-counting is the
	// safe direction, because an unobserved rule reads as unknown rather than
	// as unused.
	if err := recordEmissions(anvilPath, rf, emitted, now); err != nil {
		log.Printf("[warden:%s] recording rule emission failed: %v", beadID, err)
	}

	return "\n## Learned Review Rules\n\nThese are domain-specific patterns learned from past reviews. Check each one against the diff:\n\n" + checklist
}

// recordEmissions stamps the rules at the given positions of selected as
// emitted at now and persists the result, and it does so against a FRESH read
// of the file rather than against selected itself.
//
// selected is the file as it stood when the checklist was built, which may be
// several seconds and one Claude session old by the time the section is
// assembled. Writing that copy back is a lost update: a learner or a
// consolidation that saved in the meantime has its work — a rule it spent a
// distillation on, an eviction, a whole archive sweep — overwritten by a
// review that only ever meant to touch three telemetry fields.
//
// So the file is re-read and each position is checked to still hold the rule
// that was selected, compared by ruleTieKey — the same content key the
// ordering's last tie-break uses, which reads a rule's identity and none of
// its usage fields, so a concurrent stamp of the same rule is not mistaken for
// a different rule and our increment lands on top of theirs. A position whose
// rule moved or changed is skipped, which costs one stamp; nothing is written
// when no position survives that check, so a review whose selection was
// entirely superseded rewrites nothing at all.
//
// The residual race is the window between this read and this write, which is
// two file operations rather than a model session, and its cost is back to the
// undercount the telemetry's consumers already treat as unknown.
func recordEmissions(anvilPath string, selected *RulesFile, positions []int, now time.Time) error {
	if len(positions) == 0 {
		return nil
	}
	current, err := LoadRules(anvilPath)
	if err != nil {
		return err
	}

	live := make([]int, 0, len(positions))
	for _, i := range positions {
		if i < 0 || i >= len(selected.Rules) || i >= len(current.Rules) {
			continue
		}
		if ruleTieKey(selected.Rules[i]) != ruleTieKey(current.Rules[i]) {
			continue
		}
		live = append(live, i)
	}

	if n := current.MarkEmittedAt(live, now); n == 0 {
		return nil
	}
	return SaveRules(anvilPath, current)
}
