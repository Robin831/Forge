package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logFilenameCluster reconstructs the largest cluster PR #682 shipped: eight
// separate distillations of one check — "the documented log filename must
// match what the code actually produces" — learned from one source PR.
//
// Two properties of the real batch are reproduced deliberately, because
// together they are why nothing caught it:
//
//   - the categories DISAGREE (style, other, testing, documentation). The
//     category is the model's to pick, and eight sessions picked four; a pass
//     that clusters strictly within a category sees four small groups, none
//     of which has two members that also cross a similarity threshold.
//   - the verbosities disagree. Some variants are one clause, some are four.
//     Jaccard cannot score a terse restatement of a verbose rule above
//     |short|/|long| however complete the containment is, which is why the
//     overlap coefficient is the criterion that does the work here.
func logFilenameCluster() []Rule {
	return []Rule{
		{
			ID: "log-filename-comment-drift", Category: "style",
			Pattern: "A doc comment names a generated log filename such as smith-<ts>.log next to the code that builds it",
			Check:   "Verify the documented log filename matches what the code produces, including the timestamp unit",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-18",
		},
		{
			ID: "stale-doc-comment-units", Category: "other",
			Pattern: "A comment documents a generated log filename whose timestamp unit changed in the code",
			Check:   "Verify the documented timestamp unit matches the code, since time.Now().Unix() is seconds and UnixMilli() is milliseconds",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-18",
		},
		{
			ID: "temper-log-filename-precision-comment", Category: "style",
			Pattern: "The temper stage documents its log filename format in a doc comment near the code that constructs it",
			Check:   "Verify the documented log filename format matches the code, including the timestamp precision and any sequence suffix",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-19",
		},
		{
			ID: "log-filename-timestamp-format-mismatch", Category: "other",
			Pattern: "Documentation shows a log filename example built from a timestamp that the code formats differently",
			Check:   "Verify the documented log filename example matches the format the code produces, including the timestamp unit and any suffix",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-19",
		},
		{
			ID: "stage-log-naming-doc-accuracy", Category: "testing",
			Pattern: "A stage documents its log naming scheme while a sibling stage names its logs differently",
			Check:   "Verify the documented log naming scheme matches what the code produces and stays consistent with sibling stage log filenames",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-20",
		},
		{
			ID: "doc-comment-log-filename-mismatch", Category: "documentation",
			Pattern: "A doc comment describes the log filename format produced by the surrounding code",
			Check:   "Verify the doc comment's log filename format matches the code, including the timestamp unit and sequence suffix",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-20",
		},
		{
			ID: "changelog-log-filename-accuracy", Category: "documentation",
			Pattern: "A changelog fragment quotes a generated log filename example",
			Check:   "Verify the quoted log filename matches what the code produces rather than a stale example",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-20",
		},
		{
			ID: "log-naming-doc-drift", Category: "style",
			Pattern: "Documentation and code disagree about a generated log filename after a naming change",
			Check:   "Verify the documented log filename is updated with the code, including the timestamp unit and any sequence suffix",
			Source:  SourceList{"copilot:PR#708"}, Added: "2026-08-21",
		},
	}
}

// shippedParams are the criteria the daemon runs with out of the box.
func shippedParams() DedupParams {
	return DedupParams{Jaccard: 0.6, Overlap: DefaultOverlapThreshold}
}

// TestJaccardAloneCannotSeeTheBatchCluster is the diagnosis, pinned. Before
// this bead the pass ran with Jaccard alone at 0.6 and found nothing to merge
// — not in this batch and, measured, not in a single pair of the 727 rules in
// this repository's own rules file. If someone drops the overlap criterion,
// this is the test that says the pass is inert again rather than tuned.
func TestJaccardAloneCannotSeeTheBatchCluster(t *testing.T) {
	assert.Empty(t, ClusterNearDuplicates(logFilenameCluster(), DedupParams{Jaccard: 0.6}),
		"eight restatements of one check score below 0.6 on Jaccard")
}

// TestCategoryGroupingSplitsTheBatchCluster pins the other half of the
// diagnosis: even with a criterion that fires, partitioning by category first
// leaves the cluster in pieces.
func TestCategoryGroupingSplitsTheBatchCluster(t *testing.T) {
	_, byCat := GroupRulesByCategory(logFilenameCluster())
	require.Greater(t, len(byCat), 1, "the fixture must span categories, as the real batch did")

	merged := 0
	for _, rules := range byCat {
		for _, c := range ClusterNearDuplicates(rules, shippedParams()) {
			merged += len(c.Rules)
		}
	}
	assert.Less(t, merged, len(logFilenameCluster()),
		"a per-category pass cannot collapse a cluster whose members disagree about their category")
}

// TestClusterNearDuplicates_CollapsesTheBatchClusterAcrossCategories is the
// positive case: with both criteria and no category partition, six of the
// eight land in one cluster.
//
// Six and not eight, and the assertion says so rather than being tuned until
// it says eight. This is a similarity heuristic over prose, and the two that
// stay out are the two whose wording strays furthest from the rest —
// `stage-log-naming-doc-accuracy`, which talks about consistency between
// sibling stages, and `changelog-log-filename-accuracy`, which is about a
// changelog fragment rather than a doc comment. A threshold low enough to
// pull those in is low enough to merge rules that merely share a topic, and
// a wrong merge deletes coverage silently. Eight checklist entries becoming
// three is the win; claiming one would be over-fitting the test to the
// fixture.
func TestClusterNearDuplicates_CollapsesTheBatchClusterAcrossCategories(t *testing.T) {
	clusters := ClusterNearDuplicates(logFilenameCluster(), shippedParams())
	require.Len(t, clusters, 1)
	assert.Len(t, clusters[0].Rules, 6)
	assert.GreaterOrEqual(t, clusters[0].MaxSimilarity, DefaultOverlapThreshold)

	clustered := make(map[string]bool, 6)
	for _, r := range clusters[0].Rules {
		clustered[r.ID] = true
	}
	assert.False(t, clustered["stage-log-naming-doc-accuracy"])
	assert.False(t, clustered["changelog-log-filename-accuracy"])
}

// TestClusterNearDuplicates_LeavesDistinctRulesAlone is the guard against
// over-merging: a batch of rules about genuinely different concerns passes
// through untouched. A pass that collapsed these would be deleting coverage.
func TestClusterNearDuplicates_LeavesDistinctRulesAlone(t *testing.T) {
	distinct := []Rule{
		{ID: "sql-injection", Category: "security",
			Pattern: "A query is assembled by concatenating request parameters into SQL text",
			Check:   "Verify the query uses bound parameters rather than string concatenation"},
		{ID: "react-key-filtered-index", Category: "ui",
			Pattern: "A React list is keyed by the index of a filtered array",
			Check:   "Verify the key is derived from a stable identity so reordering does not remount rows"},
		{ID: "unchecked-writer-flush", Category: "error-handling",
			Pattern: "A buffered writer is flushed in a deferred call whose error is discarded",
			Check:   "Verify the flush error is captured and logged so a disk-full failure does not silently drop data"},
		{ID: "bound-aggregate-fetch", Category: "performance",
			Pattern: "An aggregate figure is recomputed on every iteration of a loop over open pull requests",
			Check:   "Verify loop-invariant queries are hoisted out and computed once per cycle"},
	}
	assert.Empty(t, ClusterNearDuplicates(distinct, shippedParams()))
}

// TestOverlap_SeesContainmentJaccardCannot states the metric difference in
// isolation, so the reason the second criterion exists survives a refactor of
// the callers.
func TestOverlap_SeesContainmentJaccardCannot(t *testing.T) {
	short := Tokenize("documented log filename must match generated code")
	long := Tokenize(
		"documented log filename must match generated code including timestamp unit " +
			"sequence suffix stage naming scheme sibling consistency placeholder example")

	assert.InDelta(t, 1.0, Overlap(short, long), 1e-9, "every token of the short bag is in the long one")
	assert.Less(t, Jaccard(short, long), 0.6, "Jaccard is capped by the union it divides by")
}

// TestNearDuplicate_SmallBagsAreJudgedOnJaccardAlone pins MinOverlapTokens.
// Overlap is meaningless for a handful of tokens: three words that happen to
// appear in a forty-word rule score a perfect 1.0 while saying something
// entirely different.
func TestNearDuplicate_SmallBagsAreJudgedOnJaccardAlone(t *testing.T) {
	tiny := Tokenize("timestamp suffix")
	long := Tokenize(
		"documented log filename must match generated code including timestamp unit " +
			"sequence suffix stage naming scheme sibling consistency placeholder example")
	require.Less(t, len(tiny), MinOverlapTokens)
	require.InDelta(t, 1.0, Overlap(tiny, long), 1e-9)

	hit, _ := NearDuplicate(tiny, long, shippedParams())
	assert.False(t, hit, "a bag below MinOverlapTokens must not merge on containment")
}

// TestDedupParams_ZeroDisablesEverything: both criteria off means the pass
// cannot return a cluster, whatever the input.
func TestDedupParams_ZeroDisablesEverything(t *testing.T) {
	assert.True(t, DedupParams{}.IsZero())
	assert.False(t, DedupParams{Overlap: 0.5}.IsZero())
	assert.Empty(t, ClusterNearDuplicates(logFilenameCluster(), DedupParams{}))
}

// mergeStub returns a ConsolidationRunner that answers with a fixed merged
// rule and records how many clusters it was asked to merge.
func mergeStub(t *testing.T, id, pattern, check string, calls *int) ConsolidationRunner {
	t.Helper()
	return func(context.Context, string, string) ([]byte, error) {
		*calls++
		body, err := json.Marshal(map[string]string{"id": id, "pattern": pattern, "check": check})
		require.NoError(t, err)
		return body, nil
	}
}

// TestConsolidateBatch_CollapsesTheRestatementCluster is the shape of the
// PR #682 failure, run through the batch pass: eight rules in, three out.
func TestConsolidateBatch_CollapsesTheRestatementCluster(t *testing.T) {
	batch := logFilenameCluster()
	rf := &RulesFile{Rules: batch}
	ids := make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}

	calls := 0
	replaced, summary, errs := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids,
		shippedParams(), mergeStub(t, "log-filename-doc-drift", "documented log filename", "verify it matches the code", &calls))

	assert.Empty(t, errs)
	assert.Equal(t, 1, calls, "one cluster, one distillation call")
	require.Len(t, summary, 1)
	assert.Len(t, replaced, 6)
	require.Len(t, rf.Rules, 3, "six restatements become one rule; two outliers survive")

	merged := rf.Rules[2]
	assert.Equal(t, "log-filename-doc-drift", merged.ID)
	assert.Equal(t, SourceList{"copilot:PR#708"}, merged.Source, "provenance is the union of the cluster's sources")
	assert.Equal(t, "2026-08-18", merged.Added, "the merged rule keeps the oldest Added date")
	// The category is the cluster's most common one; ties break by first
	// appearance. style appears three times here, more than any other.
	assert.Equal(t, "style", merged.Category)
	assert.ElementsMatch(t, summary[0].ReplacedIDs, []string{
		"log-filename-comment-drift",
		"stale-doc-comment-units",
		"temper-log-filename-precision-comment",
		"log-filename-timestamp-format-mismatch",
		"doc-comment-log-filename-mismatch",
		"log-naming-doc-drift",
	})
	_ = ids
}

// TestConsolidateBatch_MergedSourcesAreTheUnion covers the multi-PR case:
// a cluster whose members came from different PRs must keep every reference,
// because the paths backfill and the staleness sweep both read Source.
func TestConsolidateBatch_MergedSourcesAreTheUnion(t *testing.T) {
	batch := logFilenameCluster()
	sourceOf := make(map[string]string, len(batch))
	ids := make([]string, len(batch))
	for i := range batch {
		batch[i].Source = SourceList{fmt.Sprintf("copilot:PR#%d", 700+i)}
		sourceOf[batch[i].ID] = batch[i].Source[0]
		ids[i] = batch[i].ID
	}
	rf := &RulesFile{Rules: batch}

	calls := 0
	_, summary, errs := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids,
		shippedParams(), mergeStub(t, "merged", "p", "c", &calls))
	assert.Empty(t, errs)
	require.Len(t, summary, 1)

	want := make([]string, 0, len(summary[0].ReplacedIDs))
	for _, id := range summary[0].ReplacedIDs {
		want = append(want, sourceOf[id])
	}
	require.Greater(t, len(want), 1, "the cluster must span more than one source PR")
	assert.ElementsMatch(t, want, []string(summary[0].Merged.Source))
}

// TestConsolidateBatch_LeavesRulesOutsideTheBatchAlone: deduping the batch
// against the established file is the whole-file pass's job. A rule nobody
// asked about must not be swept into a merge — not even one that is a
// near-duplicate of a batch member, which is exactly what this fixture puts
// in the file.
func TestConsolidateBatch_LeavesRulesOutsideTheBatchAlone(t *testing.T) {
	batch := logFilenameCluster()
	existing := batch[0]
	existing.ID = "already-in-the-file"

	rf := &RulesFile{Rules: append([]Rule{existing}, batch...)}
	ids := make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}

	calls := 0
	replaced, summary, errs := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids,
		shippedParams(), mergeStub(t, "merged", "p", "c", &calls))
	assert.Empty(t, errs)
	require.Len(t, summary, 1)
	for _, id := range summary[0].ReplacedIDs {
		assert.NotEqual(t, "already-in-the-file", id)
	}
	for _, r := range replaced {
		assert.NotEqual(t, "already-in-the-file", r.ID)
	}

	var survived bool
	for _, r := range rf.Rules {
		if r.ID == "already-in-the-file" {
			survived = true
		}
	}
	assert.True(t, survived, "the pre-existing rule survives the batch pass untouched")
}

// TestConsolidateBatch_IsOrderIndependent: the caller hands over IDs in
// whatever order the pending queue returned them, and the merge must not
// depend on it.
func TestConsolidateBatch_IsOrderIndependent(t *testing.T) {
	run := func(ids []string) []string {
		rf := &RulesFile{Rules: logFilenameCluster()}
		calls := 0
		_, _, errs := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids,
			shippedParams(), mergeStub(t, "merged", "p", "c", &calls))
		require.Empty(t, errs)
		out := make([]string, len(rf.Rules))
		for i, r := range rf.Rules {
			out[i] = r.ID
		}
		return out
	}

	forward := make([]string, 0, 8)
	for _, r := range logFilenameCluster() {
		forward = append(forward, r.ID)
	}
	reversed := make([]string, len(forward))
	for i, id := range forward {
		reversed[len(forward)-1-i] = id
	}
	assert.Equal(t, run(forward), run(reversed))
}

// TestConsolidateBatch_NoOpCases covers the guards: nothing to cluster,
// criteria disabled, and a batch naming rules the file does not hold.
func TestConsolidateBatch_NoOpCases(t *testing.T) {
	batch := logFilenameCluster()
	ids := make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}
	calls := 0
	runner := mergeStub(t, "merged", "p", "c", &calls)

	rf := &RulesFile{Rules: batch}
	_, summary, _ := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids, DedupParams{}, runner)
	assert.Empty(t, summary, "both criteria disabled")

	rf = &RulesFile{Rules: batch}
	_, summary, _ = ConsolidateBatch(context.Background(), t.TempDir(), rf, []string{ids[0]}, shippedParams(), runner)
	assert.Empty(t, summary, "a batch of one has nothing to dedupe against")

	rf = &RulesFile{Rules: batch}
	_, summary, _ = ConsolidateBatch(context.Background(), t.TempDir(), rf, []string{"absent-a", "absent-b"}, shippedParams(), runner)
	assert.Empty(t, summary, "IDs the file does not hold match no rules")

	assert.Zero(t, calls, "no distillation call is made for any no-op case")
}

// TestConsolidate_ZeroThresholdStaysOff: dedup_threshold is the documented
// off switch. The overlap criterion must never keep merging behind it.
func TestConsolidate_ZeroThresholdStaysOff(t *testing.T) {
	rf := &RulesFile{Rules: logFilenameCluster()}
	calls := 0
	replaced, summary, errs := Consolidate(context.Background(), t.TempDir(), rf, 0,
		mergeStub(t, "merged", "p", "c", &calls))
	assert.Empty(t, replaced)
	assert.Empty(t, summary)
	assert.Empty(t, errs)
	assert.Zero(t, calls)
	assert.Len(t, rf.Rules, 8)
}

// --- Rule identity: an ID is not a handle on a rule --------------------------

// TestConsolidateBatch_DuplicateIDIsExcludedNotCollapsed pins the identity
// the batch pass acts on.
//
// The rules file is ordinary tracked YAML: a merge, a hand edit or two
// distillation sessions naming their output the same thing can leave two
// distinct rules under one ID. Selected by ID, the batch pass pulled BOTH
// into the batch — and removed both from the file when the cluster merged,
// while archiving only the member the cluster actually held. The unrelated
// rule was deleted with no archive entry, no summary line and nothing in the
// log: one rule of coverage gone per collision, invisible until a review
// stopped mentioning the thing.
//
// A colliding ID is now excluded from the batch and reported. The pass
// cannot tell which of the two it was handed, so it touches neither.
func TestConsolidateBatch_DuplicateIDIsExcludedNotCollapsed(t *testing.T) {
	batch := logFilenameCluster()
	require.GreaterOrEqual(t, len(batch), 3)

	// An established rule about something else entirely, sharing an ID with
	// the first batch member. Its vocabulary does not overlap the cluster's,
	// so it can never be clustered on its own merits.
	bystander := Rule{
		ID:       batch[0].ID,
		Category: "security",
		Pattern:  "A tar archive is extracted without inspecting each entry's type header",
		Check:    "Verify symlink and device entries are rejected before extraction",
		Source:   SourceList{"copilot:PR#4001"},
		Added:    "2026-01-01",
	}

	rf := &RulesFile{Rules: append([]Rule{bystander}, batch...)}
	before := len(rf.Rules)
	ids := make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}

	calls := 0
	replaced, summary, errs := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids,
		shippedParams(), mergeStub(t, "log-filename-doc-drift", "documented log filename", "verify it matches the code", &calls))

	// The collision is reported rather than swallowed.
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), batch[0].ID)

	// The bystander is still in the file, byte for byte.
	var found int
	for _, r := range rf.Rules {
		if r.ID == bystander.ID && r.Check == bystander.Check {
			found++
			assert.Equal(t, bystander, r)
		}
	}
	assert.Equal(t, 1, found, "the rule sharing an ID with a batch member must survive untouched")

	// Nothing left the file without being archived: every rule that is gone
	// is in replaced.
	assert.Equal(t, before-len(replaced)+len(summary), len(rf.Rules),
		"the file shrinks by exactly what was archived, plus the merged rules")
	for _, r := range replaced {
		assert.NotEqual(t, bystander.Check, r.Check, "the bystander is never archived, because it is never removed")
	}

	// The rest of the batch still consolidates — one collision does not
	// disable the pass.
	require.Len(t, summary, 1)
	assert.NotContains(t, summary[0].ReplacedIDs, batch[0].ID)
}

// TestConsolidateWithParams_RemovesByPositionNotID is the same invariant for
// the whole-file pass, which partitions by category and so could remove a
// same-ID rule sitting in a different category from the one it merged.
func TestConsolidateWithParams_RemovesByPositionNotID(t *testing.T) {
	cluster := logFilenameCluster()
	for i := range cluster {
		cluster[i].Category = "style" // one category, so the whole-file pass sees them
	}

	bystander := Rule{
		ID:       cluster[0].ID,
		Category: "security", // a different partition entirely
		Pattern:  "A query is assembled by concatenating request parameters into SQL text",
		Check:    "Verify the query binds parameters rather than concatenating strings",
		Source:   SourceList{"copilot:PR#4002"},
		Added:    "2026-01-01",
	}

	rf := &RulesFile{Rules: append([]Rule{bystander}, cluster...)}
	calls := 0
	_, summary, errs := ConsolidateWithParams(context.Background(), t.TempDir(), rf,
		shippedParams(), mergeStub(t, "log-filename-doc-drift", "documented log filename", "verify it matches the code", &calls))

	assert.Empty(t, errs)
	require.Len(t, summary, 1)

	var survived bool
	for _, r := range rf.Rules {
		if r.Category == "security" && r.Check == bystander.Check {
			survived = true
		}
	}
	assert.True(t, survived, "a rule in another category sharing an ID with a merged one must not be removed")
}

// TestAddRuleDistinct_KeepsTwoRulesUnderOneID covers the other end of the
// same problem: the pending queue.
//
// AddRule skips by ID, so the second of two distinct rules named the same
// thing was dropped from the queue with its content and nothing said about
// it — and it is exactly the rule the intra-batch pass exists to fold into
// the first, which it can only do if both are in the file to be clustered.
func TestAddRuleDistinct_KeepsTwoRulesUnderOneID(t *testing.T) {
	first := Rule{ID: "log-filename", Category: "style", Pattern: "p1", Check: "c1"}
	second := Rule{ID: "log-filename", Category: "documentation", Pattern: "p2", Check: "c2"}

	rf := &RulesFile{}

	id, added := rf.AddRuleDistinct(first)
	assert.True(t, added)
	assert.Equal(t, "log-filename", id)

	id, added = rf.AddRuleDistinct(second)
	assert.True(t, added, "a distinct rule is kept even when its ID is taken")
	assert.Equal(t, "log-filename-2", id)
	require.Len(t, rf.Rules, 2)
	assert.Equal(t, "c2", rf.Rules[1].Check)

	// Re-learning the very same rule is still a no-op, which is what the
	// by-ID skip was right about.
	id, added = rf.AddRuleDistinct(first)
	assert.False(t, added)
	assert.Equal(t, "log-filename", id)
	assert.Len(t, rf.Rules, 2)

	// A third distinct collision keeps counting up rather than clobbering.
	third := Rule{ID: "log-filename", Category: "testing", Pattern: "p3", Check: "c3"}
	id, added = rf.AddRuleDistinct(third)
	assert.True(t, added)
	assert.Equal(t, "log-filename-3", id)
	assert.Len(t, rf.Rules, 3)
}

// securityBoundaryBatch is one check arriving under three model-chosen
// categories — the shape the batch pass exists to collapse — except that one
// of the three is `security`. All three say the same thing at the same
// verbosity, so nothing but the category distinguishes them.
func securityBoundaryBatch() []Rule {
	const pattern = "an HTTP handler returns rows from a shared table without scoping them to the caller"
	const check = "Verify the handler filters the query by the caller's own tenant id, since a shared table returns every tenant's rows to whoever asks"
	return []Rule{
		{ID: "tenant-scope-security", Category: "security", Pattern: pattern, Check: check, Source: SourceList{"copilot:PR#900"}, Added: "2026-08-18"},
		{ID: "tenant-scope-testing", Category: "testing", Pattern: pattern, Check: check, Source: SourceList{"copilot:PR#900"}, Added: "2026-08-19"},
		{ID: "tenant-scope-other", Category: "testing", Pattern: pattern, Check: check, Source: SourceList{"copilot:PR#900"}, Added: "2026-08-20"},
	}
}

// A merged rule's category is a review-time gate, not a label: FilterRules
// drops a rule whose canonical category is outside the set categoriesForFile
// derives for the diff, and a .cs diff selects `security` while excluding
// `testing`. So merging a lone security rule into a two-member testing
// cluster would take the security check out of scope on the next .NET diff
// and report it as a consolidation. The batch pass refuses that crossing.
func TestConsolidateBatch_DoesNotMergeAcrossTheSecurityBoundary(t *testing.T) {
	batch := securityBoundaryBatch()
	rf := &RulesFile{Rules: batch}
	ids := []string{"tenant-scope-security", "tenant-scope-testing", "tenant-scope-other"}

	calls := 0
	replaced, summary, errs := ConsolidateBatch(context.Background(), t.TempDir(), rf, ids,
		shippedParams(), mergeStub(t, "tenant-scope-merged", "tenant scoping", "verify the query is scoped", &calls))

	assert.Empty(t, errs)
	// The two testing rules still collapse — only the boundary is refused.
	require.Len(t, summary, 1)
	assert.Equal(t, "testing", summary[0].Category)
	assert.ElementsMatch(t, []string{"tenant-scope-testing", "tenant-scope-other"}, summary[0].ReplacedIDs)
	assert.Len(t, replaced, 2)

	// The security rule is untouched and still carries its category.
	var kept *Rule
	for i := range rf.Rules {
		if rf.Rules[i].ID == "tenant-scope-security" {
			kept = &rf.Rules[i]
		}
	}
	require.NotNil(t, kept, "the security rule must survive the pass")
	assert.Equal(t, "security", kept.Category, "a merge may not reclassify a security rule out of review scope")
}

// The refusal must not cost the batch its ordinary merges: with the security
// member alone on its side of the boundary, the non-security members are
// clustered exactly as they would have been.
func TestSplitSecurityBoundary(t *testing.T) {
	mk := func(cats ...string) Cluster {
		var c Cluster
		c.MaxSimilarity = 0.9
		for i, cat := range cats {
			c.Rules = append(c.Rules, Rule{ID: fmt.Sprintf("r%d", i), Category: cat})
			c.Indices = append(c.Indices, i)
		}
		return c
	}

	t.Run("no security member is returned unchanged", func(t *testing.T) {
		in := mk("style", "testing", "other")
		out := splitSecurityBoundary(in)
		require.Len(t, out, 1)
		assert.Equal(t, in.Rules, out[0].Rules)
		assert.Equal(t, in.Indices, out[0].Indices)
	})

	t.Run("all security members are returned unchanged", func(t *testing.T) {
		in := mk("security", "security")
		out := splitSecurityBoundary(in)
		require.Len(t, out, 1)
		assert.Len(t, out[0].Rules, 2)
	})

	t.Run("a lone security member drops out and the rest still merge", func(t *testing.T) {
		out := splitSecurityBoundary(mk("security", "style", "testing"))
		require.Len(t, out, 1, "the single security rule cannot form a cluster of its own")
		assert.Equal(t, []int{1, 2}, out[0].Indices)
		assert.Equal(t, 0.9, out[0].MaxSimilarity)
	})

	t.Run("both sides merge when both have two members", func(t *testing.T) {
		out := splitSecurityBoundary(mk("security", "security", "style", "style"))
		require.Len(t, out, 2)
		assert.Equal(t, []int{0, 1}, out[0].Indices)
		assert.Equal(t, []int{2, 3}, out[1].Indices)
	})

	t.Run("category matching is case- and space-insensitive", func(t *testing.T) {
		out := splitSecurityBoundary(mk(" Security ", "style", "testing"))
		require.Len(t, out, 1)
		assert.Equal(t, []int{1, 2}, out[0].Indices, "a padded or capitalised category must not slip past the boundary")
	})
}

// Consolidate is the one-knob form: it applies the Jaccard number it is
// given and nothing else. Pairing it with DefaultOverlapThreshold would
// merge rules on a criterion the caller never opted into — including for an
// operator who raised the number precisely to suppress merging.
func TestConsolidate_AppliesJaccardAlone(t *testing.T) {
	rf := &RulesFile{Rules: logFilenameCluster()}
	before := len(rf.Rules)

	calls := 0
	_, summary, errs := Consolidate(context.Background(), t.TempDir(), rf, 0.6,
		mergeStub(t, "merged", "p", "c", &calls))

	assert.Empty(t, errs)
	assert.Empty(t, summary, "Jaccard at 0.6 clusters nothing on this corpus — the measured baseline this bead is built on")
	assert.Equal(t, 0, calls)
	assert.Len(t, rf.Rules, before)

	// The same rules under both criteria do cluster, which is what makes the
	// assertion above a statement about the criterion and not about the data.
	rf2 := &RulesFile{Rules: logFilenameCluster()}
	_, summary2, errs2 := ConsolidateWithParams(context.Background(), t.TempDir(), rf2,
		DedupParams{Jaccard: 0.6, Overlap: DefaultOverlapThreshold}, mergeStub(t, "merged", "p", "c", &calls))
	assert.Empty(t, errs2)
	assert.NotEmpty(t, summary2)
}
