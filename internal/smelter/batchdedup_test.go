package smelter

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logFilenameRestatements builds n restatements of ONE check — "the
// documented log filename must match what the code produces" — in the shape
// PR #682 landed them: one source PR, and CATEGORIES THAT DISAGREE, because
// the category is whatever the distillation session that produced the rule
// happened to pick.
//
// The disagreement is the point. Before this bead the smelter's only
// consolidation pass partitioned by category before clustering, so eight
// restatements labelled across four categories were four groups of two or
// three, none of which cleared a similarity threshold. All eight shipped, all
// eight went into every Warden prompt, and all eight ate slots out of the
// MaxRules cap.
func logFilenameRestatements(n int) []warden.Rule {
	categories := []string{"style", "other", "documentation", "testing"}
	out := make([]warden.Rule, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, warden.Rule{
			ID:       fmt.Sprintf("log-filename-doc-drift-%d", i),
			Category: categories[i%len(categories)],
			Pattern: "A doc comment describes the generated log filename format " +
				"produced by the surrounding code that builds that filename",
			Check: "Verify the documented log filename format matches exactly what the code " +
				"produces, including the timestamp unit and any sequence suffix",
			Source: warden.SourceList{fmt.Sprintf("copilot:PR#%d", 708+i)},
			Added:  fmt.Sprintf("2026-08-%02d", 10+i),
		})
	}
	return out
}

// distinctRules returns n rules about genuinely different concerns. Their
// vocabularies barely intersect, so no similarity pass may touch them.
func distinctRules(n int) []warden.Rule {
	subjects := []struct{ id, category, pattern, check string }{
		{"sql-injection", "security", "A query is assembled by concatenating request parameters into SQL text", "Verify the query binds parameters rather than concatenating strings"},
		{"react-key-index", "ui", "A React list is keyed by the index of a filtered array", "Verify the key comes from a stable identity so reordering does not remount rows"},
		{"unchecked-flush", "error-handling", "A buffered writer is flushed in a deferred call whose error is discarded", "Verify the flush error is captured so a disk-full failure does not drop data"},
		{"hoist-invariant-query", "performance", "An aggregate figure is recomputed inside a loop over open pull requests", "Verify loop-invariant queries are hoisted out and computed once per cycle"},
		{"tar-entry-type", "security", "A tar archive is extracted without inspecting each entry's type header", "Verify symlink and device entries are rejected before extraction"},
		{"table-driven-subtests", "testing", "A table of cases is asserted inside one monolithic test body", "Verify each case runs as its own subtest so a failure names the case"},
		{"context-deadline-propagated", "concurrency", "A goroutine is spawned with a background context inside a request handler", "Verify the caller's context is propagated so cancellation reaches the goroutine"},
		{"trim-config-strings", "style", "A configuration string is compared without trimming surrounding whitespace", "Verify the value is trimmed before comparison so a padded entry still matches"},
	}
	out := make([]warden.Rule, 0, n)
	for i := 0; i < n; i++ {
		s := subjects[i%len(subjects)]
		r := warden.Rule{
			ID: s.id, Category: s.category, Pattern: s.pattern, Check: s.check,
			Source: warden.SourceList{fmt.Sprintf("copilot:PR#%d", 900+i)},
			Added:  "2026-08-01",
		}
		if i >= len(subjects) {
			// Repeats get a distinct identity; the fixture never needs more
			// than the eight above, so this is only a safety net.
			r.ID = fmt.Sprintf("%s-%d", s.id, i)
		}
		out = append(out, r)
	}
	return out
}

// learnAndFlush drives the real learn -> flush path for one anvil: it inserts
// the rules through warden.InsertRulesAsPending (the entry point the learner
// uses when the smelter is enabled), reads them back out of the pending
// queue, runs every in-memory flush pass, and persists the result to disk.
//
// Only the git half of flushAnvil — the batch worktree, the force-push and
// the gh invocation — is skipped, and none of it can change which rules are
// written.
func learnAndFlush(t *testing.T, s *Smelter, db *state.DB, anvil, dir string, rules []warden.Rule) (PassResults, *warden.RulesFile) {
	t.Helper()

	// The paths backfill is a separate pass with its own tests, and left live
	// it shells out to gh once per source PR — which makes what these tests
	// assert depend on a network round trip and on whether PR #709 happens to
	// exist in whatever repository the checkout points at.
	withStubFetcher(t, func(context.Context, string, int) ([]string, error) {
		return nil, fmt.Errorf("paths backfill is stubbed out in this test")
	})

	require.NoError(t, warden.InsertRulesAsPending(rules, anvil, db.InsertPendingRule))

	byAnvil, err := db.QueryPendingRulesByAnvil()
	require.NoError(t, err)
	pending := byAnvil[anvil]
	require.Len(t, pending, len(rules), "every learned rule reaches the pending queue")

	built, err := s.buildFlushRules(context.Background(), dir, anvil, pending)
	require.NoError(t, err)

	if built.passes.HasChanges() {
		require.NoError(t, persistRulesAndArchive(dir, built.rules, built.archived,
			built.passes.Consolidated, built.passes.Archived))
	}

	onDisk, err := warden.LoadRules(dir)
	require.NoError(t, err)
	return built.passes, onDisk
}

// batchSmelter builds a Smelter wired the way the daemon wires it, with the
// AI consolidation call stubbed.
func batchSmelter(t *testing.T, db *state.DB, mergedID string, calls *int) *Smelter {
	t.Helper()
	return New(db, time.Hour, map[string]string{},
		WithConsolidator(func(context.Context, string, string) ([]byte, error) {
			*calls++
			body, err := json.Marshal(map[string]string{
				"id":      mergedID,
				"pattern": "A doc comment, changelog entry or log message describes a generated log filename",
				"check":   "Verify the documented filename matches what the code produces, including the timestamp unit and any sequence suffix",
			})
			require.NoError(t, err)
			return body, nil
		}),
		WithDedupThreshold(func() float64 { return 0.6 }),
		WithOverlapThreshold(func() float64 { return warden.DefaultOverlapThreshold }),
	)
}

// TestFlush_EightRestatementsBecomeOneRule is the regression test for this
// bead: eight near-identical learned rules go in through the learn -> flush
// path, and one merged rule comes out on disk.
//
// Before the intra-batch pass existed all eight were written, because the
// only consolidation pass grouped by category first and these eight disagree
// about their category — which is exactly what PR #682 shipped.
func TestFlush_EightRestatementsBecomeOneRule(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := batchSmelter(t, db, "log-filename-doc-drift", &calls)

	learned := logFilenameRestatements(8)
	passes, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)

	require.Len(t, onDisk.Rules, 1, "eight restatements of one check must be written as one rule")
	merged := onDisk.Rules[0]
	assert.Equal(t, "log-filename-doc-drift", merged.ID)

	// Provenance survives the merge: every source PR is still named, and the
	// merged rule keeps the oldest Added date so the staleness sweep does not
	// treat it as brand new.
	wantSources := make([]string, 0, len(learned))
	for _, r := range learned {
		wantSources = append(wantSources, r.Source[0])
	}
	assert.ElementsMatch(t, wantSources, []string(merged.Source))
	assert.Equal(t, "2026-08-10", merged.Added)

	assert.Equal(t, 1, calls, "one cluster costs one distillation call, not eight")
	require.Len(t, passes.Consolidated, 1)
	assert.Len(t, passes.Consolidated[0].ReplacedIDs, 8)
	assert.Equal(t, []string{"log-filename-doc-drift"}, passes.Added,
		"the reported Added list names what is in the file, not the raw queue")

	// The superseded rules are recoverable: each is archived as a duplicate,
	// pointing at the rule that replaced it.
	archive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, archive.Rules, 8)
	for _, ar := range archive.Rules {
		assert.Equal(t, warden.ArchiveReasonDuplicate, ar.ArchiveReason)
		assert.Equal(t, "log-filename-doc-drift", ar.SupersededBy)
	}
}

// TestFlush_DistinctRulesPassThroughUnchanged is the other half of the
// contract. A pass that collapses a batch of genuinely different rules is not
// deduplication, it is silent loss of coverage — and it fails quietly,
// because a rule that was never written is a review that simply does not
// mention the thing.
func TestFlush_DistinctRulesPassThroughUnchanged(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := batchSmelter(t, db, "should-not-be-used", &calls)

	learned := distinctRules(8)
	passes, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)

	assert.Zero(t, calls, "nothing clustered, so no distillation call was made")
	assert.Empty(t, passes.Consolidated)
	require.Len(t, onDisk.Rules, 8)

	gotIDs := make([]string, 0, len(onDisk.Rules))
	for _, r := range onDisk.Rules {
		gotIDs = append(gotIDs, r.ID)
	}
	wantIDs := make([]string, 0, len(learned))
	for _, r := range learned {
		wantIDs = append(wantIDs, r.ID)
	}
	assert.ElementsMatch(t, wantIDs, gotIDs)
}

// TestFlush_ConsolidationRunsBeforeTheMaxRulesCap states what the duplicates
// actually cost. The review-time checklist is capped at MaxRules; a batch
// whose duplicates are collapsed first fits under the cap with every distinct
// check intact, and the same batch uncollapsed spends the cap on restatements
// and drops real ones.
func TestFlush_ConsolidationRunsBeforeTheMaxRulesCap(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := batchSmelter(t, db, "log-filename-doc-drift", &calls)

	// A cap small enough to bite: 6 slots against 8 restatements + 8 distinct
	// rules. The numbers are the shipped shape in miniature — PR #682 put 16
	// clusters against a cap of 30.
	const cap = 6
	cfg := warden.ReviewFilterConfig{MaxRules: cap, UseAllRules: true}

	learned := append(logFilenameRestatements(8), distinctRules(8)...)

	// Uncollapsed, the cap is spent before the distinct rules are reached.
	uncollapsed := warden.FilterRules(learned, "", nil, cfg)
	require.Len(t, uncollapsed, cap)
	for _, r := range uncollapsed {
		assert.True(t, strings.HasPrefix(r.ID, "log-filename-doc-drift-"),
			"without consolidation every slot goes to a restatement")
	}

	// Collapsed, all nine distinct checks are inside the cap.
	_, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)
	require.Len(t, onDisk.Rules, 9, "8 distinct + 1 merged")
	collapsed := warden.FilterRules(onDisk.Rules, "", nil, warden.ReviewFilterConfig{MaxRules: 30, UseAllRules: true})
	assert.Len(t, collapsed, 9)
	assert.LessOrEqual(t, len(onDisk.Rules), 30)
}

// TestFlush_ContradictionsAreReportedNotResolved: the two rules stay in the
// file (nothing silently picks a winner) and the pair is named in the commit
// message the batch PR carries.
func TestFlush_ContradictionsAreReportedNotResolved(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := batchSmelter(t, db, "unused", &calls)

	learned := []warden.Rule{
		{
			ID: "invoke-cancel-under-lock", Category: "concurrency",
			Pattern: "A registry stores a cancel function that is invoked from a method which already holds the registry mutex",
			Check:   "Verify the cancel function is invoked under the lock so a concurrent clear cannot swap the handle between the lookup and the call",
			Source:  warden.SourceList{"copilot:PR#709"}, Added: "2026-08-18",
		},
		{
			ID: "unlock-before-callback", Category: "concurrency",
			Pattern: "A cancel function or callback stored in a registry is invoked while the registry mutex is held",
			Check:   "Verify the code unlocks before invoking the callback so a callback that re-enters the registry cannot deadlock",
			Source:  warden.SourceList{"copilot:PR#709"}, Added: "2026-08-18",
		},
	}
	passes, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)

	require.Len(t, passes.Contradictions, 1)
	assert.Equal(t, warden.ContradictionLockScope, passes.Contradictions[0].Kind)
	assert.Len(t, onDisk.Rules, 2, "neither rule is dropped and neither is merged")
	assert.Empty(t, passes.Consolidated)

	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "Contradictions: 1 pair(s)")
	assert.Contains(t, msg, "NOT resolved")
	assert.Contains(t, msg, "invoke-cancel-under-lock")

	body := buildPRBody(passes)
	assert.Contains(t, body, "need a human decision")
	assert.Contains(t, body, "unlock-before-callback")
}

// TestPassResults_ContradictionsAloneAreNotAChange: a run whose only finding
// is a contradiction has an unchanged rules file. Counting it as a change
// would send the smelter into `git commit` with nothing staged.
func TestPassResults_ContradictionsAloneAreNotAChange(t *testing.T) {
	p := PassResults{Contradictions: []warden.Contradiction{{A: warden.Rule{ID: "a"}, B: warden.Rule{ID: "b"}}}}
	assert.False(t, p.HasChanges())
	assert.Contains(t, passResultsSummary(p), "contradiction")
}

// TestFlush_DedupDisabledWritesEveryRestatement pins the off switch end to
// end: with dedup_threshold at 0, the smelter behaves exactly as it did
// before this bead.
func TestFlush_DedupDisabledWritesEveryRestatement(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := New(db, time.Hour, map[string]string{},
		WithConsolidator(func(context.Context, string, string) ([]byte, error) {
			calls++
			return nil, fmt.Errorf("must not be called")
		}),
		WithDedupThreshold(func() float64 { return 0 }),
		WithOverlapThreshold(func() float64 { return warden.DefaultOverlapThreshold }),
	)

	_, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, logFilenameRestatements(8))
	assert.Zero(t, calls)
	assert.Len(t, onDisk.Rules, 8)
	assert.FileExists(t, filepath.Join(dir, warden.RulesFileName))
}
