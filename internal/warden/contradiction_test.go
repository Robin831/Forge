package warden

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two rule pairs below are reconstructions of the contradictions PR #682
// shipped, in the shape the distiller emits them: Pattern describes the code
// that triggers the rule, Check prescribes what the reviewer must confirm.
// Both pairs came from one source PR each, which is what makes them worth
// reporting — two rules distilled from one PR are about one piece of code.
var (
	invokeCancelUnderLock = Rule{
		ID: "invoke-cancel-under-lock", Category: "concurrency",
		Pattern: "A registry stores a cancel function that is invoked from a method which already holds the registry mutex",
		Check:   "Verify the cancel function is invoked under the lock so a concurrent clear cannot swap the handle between the lookup and the call",
		Source:  SourceList{"copilot:PR#709"},
	}
	unlockBeforeCallback = Rule{
		ID: "unlock-before-callback", Category: "concurrency",
		Pattern: "A cancel function or callback stored in a registry is invoked while the registry mutex is held",
		Check:   "Verify the code unlocks before invoking the callback so a callback that re-enters the registry cannot deadlock",
		Source:  SourceList{"copilot:PR#709"},
	}
	persistBeforeClearingGuardFlag = Rule{
		ID: "persist-before-clearing-guard-flag", Category: "error-handling",
		Pattern: "A guard flag is cleared in memory and the change is also written to the state database",
		Check:   "Verify the row is persisted before clearing the in-memory guard flag so a failed write cannot leave the flag cleared with nothing recorded",
		Source:  SourceList{"copilot:PR#706"},
	}
	persistBeforeInMemoryFlag = Rule{
		ID: "persist-before-inmemory-flag", Category: "error-handling",
		Pattern: "A guard flag is written to the state database and mirrored by an in-memory flag",
		Check:   "Verify the in-memory guard flag is set before the row is persisted so a reader cannot observe the database ahead of memory",
		Source:  SourceList{"copilot:PR#706"},
	}
)

// TestDetectContradictions_LockScopePair pins the invoke-under-lock vs
// unlock-before-callback pair from PR #709. It is its own detector axis
// because both phrasings put the same words in the same order — a plain
// before/after parse reads the two as agreeing.
func TestDetectContradictions_LockScopePair(t *testing.T) {
	got := DetectContradictions([]Rule{invokeCancelUnderLock, unlockBeforeCallback})
	require.Len(t, got, 1)
	assert.Equal(t, ContradictionLockScope, got[0].Kind)
	assert.Equal(t, "invoke-cancel-under-lock", got[0].A.ID)
	assert.Equal(t, "unlock-before-callback", got[0].B.ID)
	assert.Equal(t, "copilot:PR#709", got[0].Source)
	assert.Contains(t, got[0].Detail, "whether the lock is held")
}

// TestDetectContradictions_SequencePair pins the persist-ordering pair from
// PR #706: one rule persists first, the other flips the in-memory flag first.
func TestDetectContradictions_SequencePair(t *testing.T) {
	got := DetectContradictions([]Rule{persistBeforeClearingGuardFlag, persistBeforeInMemoryFlag})
	require.Len(t, got, 1)
	assert.Equal(t, ContradictionSequence, got[0].Kind)
	assert.Equal(t, "persist-before-clearing-guard-flag", got[0].A.ID)
	assert.Equal(t, "persist-before-inmemory-flag", got[0].B.ID)
	assert.Equal(t, "copilot:PR#706", got[0].Source)
}

// TestDetectContradictions_AgreeingOrderingIsNotFlagged is the precision half:
// two ordering rules from the same PR about the same subject that happen to
// AGREE must not be reported. The detector's crossing test is what separates
// them, and a report a human cannot act on is worse than none.
func TestDetectContradictions_AgreeingOrderingIsNotFlagged(t *testing.T) {
	agreeing := Rule{
		ID: "persist-before-event", Category: "error-handling",
		Pattern: "A guard flag change is written to the state database and an event is logged for it",
		Check:   "Verify the guard flag row is persisted before the paired event is logged so no event describes a state that was never written",
		Source:  SourceList{"copilot:PR#706"},
	}
	assert.Empty(t, DetectContradictions([]Rule{persistBeforeClearingGuardFlag, agreeing}))
}

// TestDetectContradictions_DifferentSourcesAreNotFlagged: opposite orderings
// learned from two different PRs are two conventions in two places, not a
// contradiction. The shared-source gate is what keeps the report to pairs
// that describe one piece of code.
func TestDetectContradictions_DifferentSourcesAreNotFlagged(t *testing.T) {
	other := persistBeforeInMemoryFlag
	other.ID = "persist-before-inmemory-flag-elsewhere"
	other.Source = SourceList{"copilot:PR#999"}
	assert.Empty(t, DetectContradictions([]Rule{persistBeforeClearingGuardFlag, other}))
}

// TestDetectContradictions_UnrelatedSubjectsFromOneSource: one PR can produce
// several ordering rules about unrelated code. Without the topic-overlap gate
// any two of them that happen to share a word would be reported.
func TestDetectContradictions_UnrelatedSubjectsFromOneSource(t *testing.T) {
	a := Rule{
		ID: "flush-before-close", Category: "error-handling",
		Pattern: "A buffered writer is closed at the end of a function",
		Check:   "Verify the buffered writer is flushed before the underlying file is closed so a mid-write failure is not silently dropped",
		Source:  SourceList{"copilot:PR#706"},
	}
	b := Rule{
		ID: "react-key-before-render", Category: "ui",
		Pattern: "A list is rendered from a filtered array in a React component",
		Check:   "Verify a stable key is assigned before the filtered array is rendered so reordering does not remount rows",
		Source:  SourceList{"copilot:PR#706"},
	}
	assert.Empty(t, DetectContradictions([]Rule{a, b}))
}

// TestDetectContradictions_PatternFieldIsNotRead guards the one field
// boundary the detector depends on. A rule's Pattern describes the code shape
// that TRIGGERS it, which for an ordering rule is usually the anti-pattern —
// unlock-before-callback's own pattern says the callback IS invoked under the
// lock. Reading Pattern would make that single rule contradict itself.
func TestDetectContradictions_PatternFieldIsNotRead(t *testing.T) {
	assert.Empty(t, DetectContradictions([]Rule{unlockBeforeCallback}),
		"a single rule can never contradict itself")

	// Two rules whose Patterns oppose but whose Checks agree must be silent.
	a := unlockBeforeCallback
	b := unlockBeforeCallback
	b.ID = "release-lock-before-callback"
	b.Pattern = "A callback is invoked outside the registry mutex"
	assert.Empty(t, DetectContradictions([]Rule{a, b}))
}

// TestDetectContradictions_IsDeterministic: the report goes into a commit
// message, so two runs over one rule set must produce the same bytes.
func TestDetectContradictions_IsDeterministic(t *testing.T) {
	rules := []Rule{
		persistBeforeInMemoryFlag, unlockBeforeCallback,
		persistBeforeClearingGuardFlag, invokeCancelUnderLock,
	}
	first := FormatContradictions(DetectContradictions(rules))
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, FormatContradictions(DetectContradictions(rules)))
	}
	// Reversing the input must not reverse the report.
	reversed := make([]Rule, len(rules))
	for i, r := range rules {
		reversed[len(rules)-1-i] = r
	}
	assert.Equal(t, first, FormatContradictions(DetectContradictions(reversed)))
}

// TestDetectContradictions_ReportedOncePerPair: two rules sharing several
// source references are one contradiction, not one per shared source.
func TestDetectContradictions_ReportedOncePerPair(t *testing.T) {
	a := persistBeforeClearingGuardFlag
	a.Source = SourceList{"copilot:PR#706", "copilot:PR#707"}
	b := persistBeforeInMemoryFlag
	b.Source = SourceList{"copilot:PR#706", "copilot:PR#707"}
	got := DetectContradictions([]Rule{a, b})
	assert.Len(t, got, 1)
	assert.Equal(t, "copilot:PR#706", got[0].Source, "the smallest shared reference is reported")
}

// TestFormatContradictions_SaysNothingWasResolved: the section lands in a
// commit message beside "Consolidated:" and "Archived:", both of which
// describe completed work. It has to read as the exception.
func TestFormatContradictions_SaysNothingWasResolved(t *testing.T) {
	assert.Equal(t, "", FormatContradictions(nil))

	out := FormatContradictions(DetectContradictions([]Rule{invokeCancelUnderLock, unlockBeforeCallback}))
	assert.Contains(t, out, "NOT resolved")
	assert.Contains(t, out, "invoke-cancel-under-lock")
	assert.Contains(t, out, "unlock-before-callback")
	assert.False(t, strings.HasSuffix(out, "\n"), "sections are joined by the caller")
}
