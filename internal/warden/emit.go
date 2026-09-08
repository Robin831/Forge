package warden

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	diffpkg "github.com/Robin831/Forge/internal/diff"
)

// emitMu serialises the load-select-stamp-save cycle below across the reviews
// running in one daemon. The stamp is a read-modify-write of a whole file, so
// two concurrent reviews of two beads in the same anvil would otherwise race
// and the loser's emissions would be silently overwritten — an undercount, and
// undercounting is the direction the consumers tolerate, but there is no
// reason to accept it when the window is one small file read and one write.
//
// What it does NOT serialise against is every other writer of the same file.
// In this process that is the learner (LearnFromCIFix saves the anvil's rules
// file directly) and the smelter's flush; out of it, a `forge warden
// consolidate` run or a second daemon on the same anvil. Each of those is the
// same whole-file read-modify-write, so a stamp can still be lost to one — a
// race those paths have always had with each other, and one nothing here makes
// worse. Nothing is worth a lock file for: the field this protects is
// telemetry, the failure mode is a lost stamp rather than a corrupt rule, and
// the sweep that reads it treats a missing observation as unknown rather than
// as evidence of disuse.
var emitMu sync.Mutex

// learnedRulesSection builds the "Learned Review Rules" section of a review
// prompt and records the emission against the rules that reached it.
//
// The whole cycle — load, select, stamp, save — is one function and one lock
// hold because the stamp addresses rules by POSITION in the file it was
// selected from: re-loading the file to write to it would be addressing a
// slice that a concurrent learn or flush may have reordered underneath the
// selection.
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
		fmt.Fprintf(os.Stderr, "warden: failed to load learned review rules for %s: %v\n", anvilPath, err)
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
	if n := rf.MarkEmittedAt(emitted, now); n > 0 {
		if err := SaveRules(anvilPath, rf); err != nil {
			log.Printf("[warden:%s] recording rule emission failed: %v", beadID, err)
		}
	}

	return "\n## Learned Review Rules\n\nThese are domain-specific patterns learned from past reviews. Check each one against the diff:\n\n" + checklist
}
