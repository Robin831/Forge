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

	// flushAnvil drains the queue once the passes have run; the helper does
	// too, so a test can flush twice and have the second flush see only what
	// it just learned.
	require.NoError(t, db.DeletePendingRules(built.flushedIDs))

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

// TestFlush_TwoPendingRulesUnderOneIDBothReachTheFile is the queue end of the
// same identity problem the batch pass has at the file end.
//
// A learned rule's ID is written by whichever distillation session produced
// it, so two sessions reading two comments on one PR routinely label them the
// same thing. AddRule skipped the second by ID: its content was deleted from
// the queue with nothing in the log, no archive entry and no line in the
// commit message — and it was precisely the rule the intra-batch pass exists
// to fold into the first, which it can only do if both are in the file to be
// clustered.
//
// Both now reach the file, so the pass sees the cluster and merges it: eight
// restatements arriving under two IDs still commit as one rule.
func TestFlush_TwoPendingRulesUnderOneIDBothReachTheFile(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := batchSmelter(t, db, "log-filename-doc-drift", &calls)

	learned := logFilenameRestatements(8)
	// Two sessions, one ID: every rule keeps its own wording and its own
	// source PR, but they arrive named from a pool of two.
	for i := range learned {
		learned[i].ID = fmt.Sprintf("log-filename-doc-drift-%d", i%2)
		learned[i].Check = fmt.Sprintf("%s (variant %d)", learned[i].Check, i)
	}

	passes, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)

	require.Len(t, onDisk.Rules, 1, "eight restatements under two IDs still commit as one rule")
	require.Len(t, passes.Consolidated, 1)
	assert.Len(t, passes.Consolidated[0].ReplacedIDs, 8,
		"all eight are accounted for — none was dropped for sharing an ID")

	// Every source PR is still named, which is the check that says no rule's
	// content was silently discarded on the way in.
	wantSources := make([]string, 0, len(learned))
	for _, r := range learned {
		wantSources = append(wantSources, r.Source[0])
	}
	assert.ElementsMatch(t, wantSources, []string(onDisk.Rules[0].Source))

	// And each is recoverable from the archive.
	archive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	assert.Len(t, archive.Rules, 8)
}

// TestFlush_ReLearningTheSameRuleIsStillANoOp is the other side of it. The
// by-ID skip was right about one thing — a rule already on file must not be
// added a second time — and keeping distinct rules must not cost that.
func TestFlush_ReLearningTheSameRuleIsStillANoOp(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	calls := 0
	s := batchSmelter(t, db, "unused", &calls)

	learned := distinctRules(3)
	_, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)
	require.Len(t, onDisk.Rules, 3)

	passes, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, learned)
	assert.Len(t, onDisk.Rules, 3, "re-learning the identical rules adds nothing")
	assert.Empty(t, passes.Added)
	assert.False(t, passes.HasChanges(), "an unchanged file is not a commit")
}

// TestFlush_AddedNamesTheCommittedFileAfterTheWholeFilePass covers the
// hand-off between the two consolidation passes.
//
// The batch pass runs first and reports the merged rule it produced as an
// added rule. The whole-file pass then runs and can merge that very rule into
// an established one — after which the reported Added list named a rule that
// is not in the file being committed. Added is a statement about the
// committed rules file, so it is resolved against rf after every pass rather
// than by either pass alone, which only knows about its own merges.
func TestFlush_AddedNamesTheCommittedFileAfterTheWholeFilePass(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	// An established rule on the same check as the batch, already committed.
	established := logFilenameRestatements(1)[0]
	established.ID = "established-log-filename-rule"
	established.Category = "style" // the category the batch merge will land in
	established.Added = "2026-01-01"
	established.Source = warden.SourceList{"copilot:PR#100"}
	require.NoError(t, warden.SaveRules(dir, &warden.RulesFile{Rules: []warden.Rule{established}}))

	// The batch merge produces "log-filename-doc-drift"; the whole-file pass
	// then merges that with the established rule into "final-log-filename".
	merges := 0
	s := New(db, time.Hour, map[string]string{},
		WithConsolidator(func(context.Context, string, string) ([]byte, error) {
			merges++
			id := "log-filename-doc-drift"
			if merges > 1 {
				id = "final-log-filename"
			}
			return json.Marshal(map[string]string{
				"id":      id,
				"pattern": "A doc comment describes the generated log filename format produced by the surrounding code",
				"check":   "Verify the documented log filename format matches exactly what the code produces, including the timestamp unit and any sequence suffix",
			})
		}),
		WithDedupThreshold(func() float64 { return 0.6 }),
		WithOverlapThreshold(func() float64 { return warden.DefaultOverlapThreshold }),
	)

	passes, onDisk := learnAndFlush(t, s, db, "anvil-a", dir, logFilenameRestatements(8))

	require.Equal(t, 2, merges, "the batch pass merges, then the whole-file pass merges its output with the established rule")
	require.Len(t, onDisk.Rules, 1)
	assert.Equal(t, "final-log-filename", onDisk.Rules[0].ID)

	assert.NotContains(t, passes.Added, "log-filename-doc-drift",
		"Added must not name a rule the next pass superseded")
	for _, id := range passes.Added {
		assert.Equal(t, "final-log-filename", id)
	}
	assert.True(t, passes.HasChanges(), "the consolidations are still a change worth committing")

	// The intermediate merged rule left the file, so it is in the archive
	// too — nothing is removed without a record of what replaced it.
	archive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	var sawIntermediate bool
	for _, ar := range archive.Rules {
		if ar.Rule.ID == "log-filename-doc-drift" {
			sawIntermediate = true
			assert.Equal(t, "final-log-filename", ar.SupersededBy)
		}
	}
	assert.True(t, sawIntermediate, "the batch pass's output is archived when the whole-file pass replaces it")
}

// The two passes chain: the batch pass merges A and B into M and puts M in
// the file, and the whole-file pass then clusters M with an established C.
// Both merges happened and all four originals are archived, but announcing
// them as two independent entries puts M in the commit message as a rule
// that was created and is not in the file the message describes.
func TestFoldSupersededMerges(t *testing.T) {
	mkResult := func(mergedID string, replaced ...string) warden.MergeResult {
		return warden.MergeResult{Merged: warden.Rule{ID: mergedID}, ReplacedIDs: replaced}
	}

	t.Run("splices a superseded batch merge into the whole-file one", func(t *testing.T) {
		summary := []warden.MergeResult{
			mkResult("M", "A", "B"),
			mkResult("N", "M", "C"),
		}
		rf := &warden.RulesFile{Rules: []warden.Rule{{ID: "N"}}}

		out := foldSupersededMerges(summary, rf)

		require.Len(t, out, 1, "M is not in the file, so it must not be announced as created")
		assert.Equal(t, "N", out[0].Merged.ID)
		// M is kept in the list because M IS in the archive write — the
		// bullet and the archive must name the same set.
		assert.Equal(t, []string{"M", "A", "B", "C"}, out[0].ReplacedIDs)
	})

	t.Run("leaves an untouched pair of merges alone", func(t *testing.T) {
		summary := []warden.MergeResult{mkResult("M", "A", "B"), mkResult("N", "C", "D")}
		rf := &warden.RulesFile{Rules: []warden.Rule{{ID: "M"}, {ID: "N"}}}

		out := foldSupersededMerges(summary, rf)

		require.Len(t, out, 2)
		assert.Equal(t, []string{"A", "B"}, out[0].ReplacedIDs)
		assert.Equal(t, []string{"C", "D"}, out[1].ReplacedIDs)
	})

	t.Run("keeps an entry whose merged rule vanished for some other reason", func(t *testing.T) {
		// Nothing in the flush removes a rule between the two passes, so an
		// absence no later entry accounts for is not something to silently
		// drop — it is the thing the fold exists to make legible.
		summary := []warden.MergeResult{mkResult("M", "A", "B"), mkResult("N", "C", "D")}
		rf := &warden.RulesFile{Rules: []warden.Rule{{ID: "N"}}}

		out := foldSupersededMerges(summary, rf)

		require.Len(t, out, 2)
		assert.Equal(t, "M", out[0].Merged.ID)
	})

	t.Run("only a LATER entry can absorb an earlier one", func(t *testing.T) {
		// An entry can only be replaced by a pass that ran after the one
		// that made it; matching backwards would fold a merge into its own
		// predecessor.
		summary := []warden.MergeResult{mkResult("N", "M", "C"), mkResult("M", "A", "B")}
		rf := &warden.RulesFile{Rules: []warden.Rule{{ID: "N"}}}

		out := foldSupersededMerges(summary, rf)

		require.Len(t, out, 2)
		assert.Equal(t, []string{"M", "C"}, out[0].ReplacedIDs)
	})
}

// A rule ID is model output — distillRule keeps whatever `id` the JSON
// carried, and the distillation prompt is built from Copilot's comments on a
// contributor's PR — and these bullets are published under Forge's own
// GitHub identity. So the ID reaches the body through a closed alphabet, not
// an escape: the injection never needs to break the code span, only to be
// read as an instruction.
func TestSafeRuleID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary kebab-case id passes through", "log-filename-doc-drift", "log-filename-doc-drift"},
		{"dots, slashes and underscores are kept", "pkg/sub_dir.v2-rule", "pkg/sub_dir.v2-rule"},
		{"backticks cannot break out of the code span", "id`; rm -rf /", "id?rm?-rf?/"},
		{"a newline cannot forge a further body section", "id\n\n## Approved", "id?##?Approved"},
		{"a team mention cannot notify anybody", "@org/team-rule", "?org/team-rule"},
		{"an HTML comment cannot hide content", "id<!-- hidden -->", "id?--?hidden?--?"},
		{"empty stays empty for the (no id) placeholder", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, safeRuleID(tc.in))
		})
	}

	t.Run("bounded", func(t *testing.T) {
		assert.Len(t, safeRuleID(strings.Repeat("a", maxRuleIDBytes*3)), maxRuleIDBytes)
	})

	t.Run("displayID renders an unnamed rule rather than a lone marker", func(t *testing.T) {
		assert.Equal(t, "(no id)", displayID(""))
		assert.Equal(t, "(no id)", displayID("\t "))
	})
}

// The contradiction bullets are the first identifier from a learned rule to
// reach the published PR body; every field but Kind is model-authored.
func TestBuildPRBodySanitizesContradictionIDs(t *testing.T) {
	body := buildPRBody(PassResults{
		Contradictions: []warden.Contradiction{{
			A:      warden.Rule{ID: "good-rule"},
			B:      warden.Rule{ID: "bad`rule\n@org/team"},
			Source: "copilot:PR#682\n- forged bullet",
			Kind:   warden.ContradictionLockScope,
		}},
	})

	assert.Contains(t, body, "- `good-rule` vs `bad?rule?org/team` (copilot:PR#682?-?forged?bullet, lock-scope)")
	assert.NotContains(t, body, "@org/team-")
	for _, line := range strings.Split(body, "\n") {
		assert.NotContains(t, line, "forged bullet", "model text must not survive as its own line")
	}
}

// A contradiction is reported and never resolved, and the scan is over the
// whole rules file, so the set found on each flush is monotonic: without
// suppression every flush re-announces everything the last one did, one
// WARNING line and one feed row per pair, forever, with no verb to dismiss
// one.
func TestContradictionAnnouncer(t *testing.T) {
	pair := func(a, b string) warden.Contradiction {
		return warden.Contradiction{A: warden.Rule{ID: a}, B: warden.Rule{ID: b}}
	}
	first, second := pair("a", "b"), pair("c", "d")

	var ann contradictionAnnouncer

	assert.Len(t, ann.unannounced("munin", []warden.Contradiction{first, second}), 2)
	assert.Empty(t, ann.unannounced("munin", []warden.Contradiction{first, second}),
		"a second flush finding the same pairs must announce nothing")

	third := pair("e", "f")
	fresh := ann.unannounced("munin", []warden.Contradiction{first, second, third})
	require.Len(t, fresh, 1, "only the newly discovered pair is news")
	assert.Equal(t, "e", fresh[0].A.ID)

	// The memory is per anvil: two anvils holding the same pair of rule IDs
	// are two conditions, each with its own operator.
	assert.Len(t, ann.unannounced("hugin", []warden.Contradiction{first}), 1)

	// Source is not part of the identity — a pair does not become new
	// because a later session added another shared source reference.
	resourced := first
	resourced.Source = "copilot:PR#999"
	assert.Empty(t, ann.unannounced("munin", []warden.Contradiction{resourced}))
}

// The configured off switch, end to end through the closure the daemon
// wires. `dedup_threshold: 0` cannot carry this meaning — it is the
// setting's zero value, so an unset config and an explicit zero are one
// number by the time they reach here — which is why the switch is negative.
func TestDedupParamsOffSwitch(t *testing.T) {
	db := openTestDB(t)

	cases := []struct {
		name    string
		opts    []Option
		wantOK  bool
		wantJac float64
	}{
		{"no closure at all", nil, false, 0},
		{"negative threshold disables both passes",
			[]Option{WithDedupThreshold(func() float64 { return -1 })}, false, 0},
		{"a positive threshold enables them",
			[]Option{WithDedupThreshold(func() float64 { return 0.6 })}, true, 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(db, time.Hour, map[string]string{}, tc.opts...)
			params, ok := s.dedupParams()
			assert.Equal(t, tc.wantOK, ok)
			assert.InDelta(t, tc.wantJac, params.Jaccard, 1e-9)
			if ok {
				assert.InDelta(t, warden.DefaultOverlapThreshold, params.Overlap, 1e-9,
					"overlap falls back to the shipped default when no closure supplies one")
			} else {
				assert.True(t, params.IsZero(), "a disabled pass must hand out no active criterion at all")
			}
		})
	}
}
