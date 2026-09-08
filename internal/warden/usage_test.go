package warden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func day(s string) time.Time {
	t, err := time.Parse(usageDateLayout, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestRulesWithoutTelemetryLoadUnchanged is the backward-compatibility floor:
// every warden-rules.yaml in existence predates these fields, and a load that
// invented a value for them would hand the staleness sweep a date nobody
// measured.
func TestRulesWithoutTelemetryLoadUnchanged(t *testing.T) {
	const src = `rules:
  - id: no-telemetry
    category: style
    pattern: a pattern
    check: a check
    source: copilot:PR#1
    added: "2026-01-02"
`
	var rf RulesFile
	if err := yaml.Unmarshal([]byte(src), &rf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rf.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rf.Rules))
	}
	r := rf.Rules[0]
	if r.LastEmitted != "" || r.LastFinding != "" || r.EmitCount != 0 {
		t.Fatalf("telemetry should be zero, got %+v", r)
	}

	// And re-marshalling must not introduce the keys: an untouched rule has to
	// round-trip byte-identically or every anvil's next flush is a diff of the
	// whole file.
	out, err := yaml.Marshal(&rf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"last_emitted", "emit_count", "last_finding"} {
		if strings.Contains(string(out), key) {
			t.Errorf("re-marshalled rule carries %q:\n%s", key, out)
		}
	}
}

func TestTelemetryRoundTrips(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{{
		ID:          "used",
		Category:    "style",
		Pattern:     "p",
		Check:       "c",
		Added:       "2026-01-02",
		LastEmitted: "2026-03-04",
		LastFinding: "2026-03-09",
		EmitCount:   7,
	}}}
	data, err := yaml.Marshal(rf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RulesFile
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	got := back.Rules[0]
	if got.LastEmitted != "2026-03-04" || got.LastFinding != "2026-03-09" || got.EmitCount != 7 {
		t.Fatalf("round trip lost telemetry: %+v\n%s", got, data)
	}
	if !got.LastActivityIn(time.UTC).Equal(day("2026-03-09")) {
		t.Fatalf("LastActivityIn = %v", got.LastActivityIn(time.UTC))
	}
}

func TestLastActivityIn(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want time.Time
	}{
		{"nothing recorded", Rule{}, time.Time{}},
		{"added only", Rule{Added: "2026-01-02"}, day("2026-01-02")},
		{"emitted after added", Rule{Added: "2026-01-02", LastEmitted: "2026-02-03"}, day("2026-02-03")},
		{"finding newest", Rule{Added: "2026-01-02", LastEmitted: "2026-02-03", LastFinding: "2026-04-05"}, day("2026-04-05")},
		{"added newest of the three", Rule{Added: "2026-05-06", LastEmitted: "2026-02-03", LastFinding: "2026-04-05"}, day("2026-05-06")},
		{"unparseable dates are not evidence", Rule{Added: "yesterday", LastEmitted: "soon"}, time.Time{}},
		{"emitted readable, added not", Rule{Added: "yesterday", LastEmitted: "2026-02-03"}, day("2026-02-03")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.rule
			if got := r.LastActivityIn(time.UTC); !got.Equal(tc.want) {
				t.Fatalf("LastActivityIn(UTC) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLastActivityInReadsDatesInTheGivenZone is the guard on the one thing a
// date-only value can get wrong: LastActivityIn's consumer is the staleness
// sweep, which subtracts it from a local now, and a date parsed in UTC and
// subtracted from a local clock reading is off by the host's offset — which is
// a whole day at the boundary IsStale counts in.
func TestLastActivityInReadsDatesInTheGivenZone(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*60*60)
	r := Rule{Added: "2026-01-02", LastEmitted: "2026-02-03"}

	got := r.LastActivityIn(loc)
	want := time.Date(2026, 2, 3, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("LastActivityIn(%v) = %v, want %v", loc, got, want)
	}
	if _, offset := got.Zone(); offset != 5*60*60 {
		t.Fatalf("parsed in the wrong zone: %v", got)
	}
	// Same calendar date, so the two readings differ by exactly the offset —
	// which is the error the sweep would inherit from a UTC reading.
	if diff := r.LastActivityIn(time.UTC).Sub(got); diff != 5*time.Hour {
		t.Fatalf("UTC and zoned readings differ by %v, want 5h", diff)
	}
	// A nil location is UTC, not a panic.
	if !r.LastActivityIn(nil).Equal(r.LastActivityIn(time.UTC)) {
		t.Fatalf("nil location did not read as UTC: %v", r.LastActivityIn(nil))
	}
}

func TestLastActivityNilReceiver(t *testing.T) {
	var r *Rule
	if got := r.LastActivityIn(time.UTC); !got.IsZero() {
		t.Fatalf("nil receiver returned %v", got)
	}
}

func TestMarkEmittedAccumulates(t *testing.T) {
	var r Rule
	r.MarkEmitted(day("2026-03-04"))
	if r.EmitCount != 1 || r.LastEmitted != "2026-03-04" {
		t.Fatalf("after first mark: %+v", r)
	}
	r.MarkEmitted(day("2026-03-05"))
	if r.EmitCount != 2 || r.LastEmitted != "2026-03-05" {
		t.Fatalf("after second mark: %+v", r)
	}
}

// TestMarkEmittedStampsByPositionNotID is the property the whole positional
// API exists for: two rules can share an ID, and stamping by ID would credit
// an emission to the one that was never selected.
func TestMarkEmittedStampsByPositionNotID(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		{ID: "dup", Check: "first"},
		{ID: "dup", Check: "second"},
	}}
	if n := rf.MarkEmittedAt([]int{1}, day("2026-03-04")); n != 1 {
		t.Fatalf("stamped %d rules, want 1", n)
	}
	if rf.Rules[0].EmitCount != 0 {
		t.Errorf("the unselected rule sharing the ID was stamped: %+v", rf.Rules[0])
	}
	if rf.Rules[1].EmitCount != 1 {
		t.Errorf("the selected rule was not stamped: %+v", rf.Rules[1])
	}
}

func TestMarkEmittedAtIgnoresOutOfRange(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{{ID: "one"}}}
	if n := rf.MarkEmittedAt([]int{-1, 0, 5}, day("2026-03-04")); n != 1 {
		t.Fatalf("stamped %d rules, want 1", n)
	}
	if rf.Rules[0].EmitCount != 1 {
		t.Fatalf("in-range rule not stamped: %+v", rf.Rules[0])
	}
}

// TestSelectionReportsFilePositions pins that the positions handed back name
// the rules in rf.Rules and not the survivors of the filters, which is what a
// stamp addressed against the ranked order would silently hit instead.
func TestSelectionReportsFilePositions(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		{ID: "backend", Category: "style", Pattern: "controller authorization filter", Check: "backend", Added: "2026-01-01", Paths: []string{"api/**/*.cs"}},
		{ID: "frontend", Category: "style", Pattern: "component render hook", Check: "frontend", Added: "2026-01-01", Paths: []string{"web/**/*.ts"}},
		{ID: "shared", Category: "style", Pattern: "controller authorization filter", Check: "shared", Added: "2026-02-01"},
	}}
	cfg := DefaultReviewFilterConfig()
	diff := "controller authorization filter applied to every request"
	checklist, stats, positions := rf.FormatChecklistForDiffWithSelection(diff, []string{"api/Controllers/Foo.cs"}, cfg)
	if checklist == "" {
		t.Fatalf("expected a checklist, stats: %s", stats.Line())
	}
	if len(positions) != stats.Emitted {
		t.Fatalf("positions=%d emitted=%d", len(positions), stats.Emitted)
	}
	for _, p := range positions {
		if p == 1 {
			t.Errorf("frontend rule (position 1) was reported as emitted; positions=%v", positions)
		}
		if p < 0 || p >= len(rf.Rules) {
			t.Fatalf("position %d out of range", p)
		}
	}
	// The checklist text names the selected rules' checks, and the positions
	// must name the same rules.
	for _, p := range positions {
		if !strings.Contains(checklist, rf.Rules[p].Check) {
			t.Errorf("position %d (%s) is not in the checklist:\n%s", p, rf.Rules[p].Check, checklist)
		}
	}
}

// TestLearnedRulesSectionStampsEmittedRules is the end-to-end case: the review
// path builds its section, and the rules that reached it come back off DISK
// with the emission recorded.
func TestLearnedRulesSectionStampsEmittedRules(t *testing.T) {
	anvil := t.TempDir()
	rf := &RulesFile{Rules: []Rule{
		{ID: "matches", Category: "style", Pattern: "controller authorization filter", Check: "check the filter", Added: "2026-01-01", Paths: []string{"api/**/*.cs"}},
		{ID: "elsewhere", Category: "style", Pattern: "component render hook", Check: "check the hook", Added: "2026-01-01", Paths: []string{"web/**/*.ts"}},
	}}
	if err := SaveRules(anvil, rf); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}

	diff := "diff --git a/api/Controllers/Foo.cs b/api/Controllers/Foo.cs\n" +
		"--- a/api/Controllers/Foo.cs\n+++ b/api/Controllers/Foo.cs\n" +
		"@@ -1 +1 @@\n+// controller authorization filter\n"

	now := day("2026-03-04")
	section := learnedRulesSection("Forge-test", anvil, diff, now)
	if !strings.Contains(section, "check the filter") {
		t.Fatalf("matching rule missing from section:\n%s", section)
	}

	back, err := LoadRules(anvil)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	var matched, other Rule
	for _, r := range back.Rules {
		switch r.ID {
		case "matches":
			matched = r
		case "elsewhere":
			other = r
		}
	}
	if matched.EmitCount != 1 || matched.LastEmitted != "2026-03-04" {
		t.Errorf("emitted rule not stamped: %+v", matched)
	}
	if other.EmitCount != 0 || other.LastEmitted != "" {
		t.Errorf("rule that never reached the checklist was stamped: %+v", other)
	}

	// A second review of the same diff accumulates rather than resets.
	learnedRulesSection("Forge-test", anvil, diff, day("2026-03-05"))
	back, err = LoadRules(anvil)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	for _, r := range back.Rules {
		if r.ID == "matches" && (r.EmitCount != 2 || r.LastEmitted != "2026-03-05") {
			t.Errorf("second emission not accumulated: %+v", r)
		}
	}
}

// TestLearnedRulesSectionLeavesAnUnreadableFileAlone: a review is not failed
// or silently emptied by telemetry, and a rules file Forge could not parse is
// never rewritten from a partial read.
func TestLearnedRulesSectionLeavesAnUnreadableFileAlone(t *testing.T) {
	anvil := t.TempDir()
	path := RulesPath(anvil)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const broken = "rules: [ this is not yaml\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := learnedRulesSection("Forge-test", anvil, "some diff", time.Now()); got != "" {
		t.Errorf("expected no section, got %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != broken {
		t.Errorf("the unreadable rules file was rewritten:\n%s", data)
	}
}

// TestMergeRuleInheritsUsage: a merged rule stands in for its members, so it
// carries their history — otherwise consolidating two well-used rules produces
// one that reads as never observed and dated to the oldest member.
func TestMergeRuleInheritsUsage(t *testing.T) {
	cluster := []Rule{
		{ID: "a", Added: "2026-01-01", LastEmitted: "2026-03-01", EmitCount: 4},
		{ID: "b", Added: "2026-02-01", LastEmitted: "2026-04-01", LastFinding: "2026-04-02", EmitCount: 3},
		{ID: "c", Added: "2026-02-01", LastEmitted: "not a date", EmitCount: 0},
	}
	merged := MergeRule(cluster, "style", "p", "c", "merged", map[string]struct{}{})
	if merged.EmitCount != 7 {
		t.Errorf("EmitCount = %d, want 7", merged.EmitCount)
	}
	if merged.LastEmitted != "2026-04-01" {
		t.Errorf("LastEmitted = %q, want 2026-04-01", merged.LastEmitted)
	}
	if merged.LastFinding != "2026-04-02" {
		t.Errorf("LastFinding = %q, want 2026-04-02", merged.LastFinding)
	}
}

// TestUseAllRulesReportsFilePositions is TestSelectionReportsFilePositions for
// the bypass branch. Its positions are identity today, which is exactly why
// nothing pinned them: a later change that pre-filters or reorders the rules
// before they are scored would keep every other test green while the stamp
// credited emissions to rules that were never selected — the failure the
// positional API exists to prevent.
func TestUseAllRulesReportsFilePositions(t *testing.T) {
	// Ranked order is deliberately not file order: the last rule is the most
	// specific and the newest, so it heads the checklist.
	rf := &RulesFile{Rules: []Rule{
		{ID: "vague", Category: "style", Pattern: "zzzz", Check: "vague check", Added: "2024-01-01", Paths: []string{"**/*"}},
		{ID: "middling", Category: "testing", Pattern: "controller authorization", Check: "middling check", Added: "2025-01-01", Paths: []string{"**/*.cs"}},
		{ID: "precise", Category: "security", Pattern: "controller authorization filter", Check: "precise check", Added: "2026-06-01", Paths: []string{"api/Controllers/**/*.cs"}},
	}}
	cfg := DefaultReviewFilterConfig()
	cfg.UseAllRules = true
	diff := "controller authorization filter applied to every request"
	changed := []string{"api/Controllers/Foo.cs"}

	selected, stats := FilterRulesIndexed(rf.Rules, diff, changed, cfg)
	if !stats.Bypassed || stats.Matched != len(rf.Rules) {
		t.Fatalf("bypass did not keep every rule: %s", stats.Line())
	}
	if len(selected) != len(rf.Rules) {
		t.Fatalf("selected %d of %d rules under a bypass", len(selected), len(rf.Rules))
	}
	// Every returned index names the rule it was returned with.
	for _, sel := range selected {
		if sel.Index < 0 || sel.Index >= len(rf.Rules) {
			t.Fatalf("index %d out of range", sel.Index)
		}
		if rf.Rules[sel.Index].ID != sel.Rule.ID {
			t.Errorf("index %d names %q but carries %q", sel.Index, rf.Rules[sel.Index].ID, sel.Rule.ID)
		}
	}
	// The ranking really did reorder, so identity positions are being asserted
	// against something other than the order they were emitted in.
	if selected[0].Index == 0 {
		t.Fatalf("expected the ranking to reorder the file; got file order: %+v", selected)
	}

	// And the same under a cap, so truncation-after-sort is covered: the
	// survivors must still name their own rules.
	cfg.MaxRules = 1
	capped, stats := FilterRulesIndexed(rf.Rules, diff, changed, cfg)
	if len(capped) != 1 {
		t.Fatalf("cap emitted %d rules: %s", len(capped), stats.Line())
	}
	if rf.Rules[capped[0].Index].ID != capped[0].Rule.ID {
		t.Errorf("capped index %d names %q but carries %q", capped[0].Index, rf.Rules[capped[0].Index].ID, capped[0].Rule.ID)
	}
	if capped[0].Rule.ID != selected[0].Rule.ID {
		t.Errorf("cap kept %q, ranking's head was %q", capped[0].Rule.ID, selected[0].Rule.ID)
	}
}

// TestEmissionStampDoesNotClobberAConcurrentWrite is the lost-update guard: a
// review loads the rules file, spends a while building its section, and by the
// time it stamps, a learner has added a rule. Saving the copy the review
// loaded would delete that rule outright — no error, no log line, and the
// distillation that produced it already spent.
func TestEmissionStampDoesNotClobberAConcurrentWrite(t *testing.T) {
	anvil := t.TempDir()
	rf := &RulesFile{Rules: []Rule{
		{ID: "matches", Category: "style", Pattern: "controller authorization filter", Check: "check the filter", Added: "2026-01-01", Paths: []string{"api/**/*.cs"}},
	}}
	if err := SaveRules(anvil, rf); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}

	// The copy the checklist was built from, as it stood before the learner.
	selected, err := LoadRules(anvil)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}

	// The learner's save, mid-review.
	learned := &RulesFile{Rules: append(append([]Rule{}, selected.Rules...), Rule{
		ID: "freshly-learned", Category: "style", Pattern: "p", Check: "learned mid-review", Added: "2026-03-04",
	})}
	if err := SaveRules(anvil, learned); err != nil {
		t.Fatalf("SaveRules (learner): %v", err)
	}

	if err := recordEmissions(anvil, selected, []int{0}, day("2026-03-04")); err != nil {
		t.Fatalf("recordEmissions: %v", err)
	}

	back, err := LoadRules(anvil)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(back.Rules) != 2 {
		t.Fatalf("the concurrently learned rule was lost: %+v", back.Rules)
	}
	if back.Rules[1].ID != "freshly-learned" {
		t.Errorf("rule 1 = %q, want freshly-learned", back.Rules[1].ID)
	}
	// The stamp still landed: the selected rule is where it was.
	if back.Rules[0].EmitCount != 1 || back.Rules[0].LastEmitted != "2026-03-04" {
		t.Errorf("stamp lost: %+v", back.Rules[0])
	}
}

// TestEmissionStampSkipsAPositionThatMoved: when the rule at a selected
// position is no longer the rule that was selected, the stamp is dropped
// rather than credited to whatever now sits there. An undercount is the
// tolerated direction; a rule that looks alive and never fires is not.
func TestEmissionStampSkipsAPositionThatMoved(t *testing.T) {
	anvil := t.TempDir()
	selected := &RulesFile{Rules: []Rule{
		{ID: "a", Category: "style", Pattern: "p", Check: "a", Added: "2026-01-01"},
		{ID: "b", Category: "style", Pattern: "p", Check: "b", Added: "2026-01-01"},
	}}
	// A consolidation reordered the file after the checklist was built.
	onDisk := &RulesFile{Rules: []Rule{selected.Rules[1], selected.Rules[0]}}
	if err := SaveRules(anvil, onDisk); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}

	if err := recordEmissions(anvil, selected, []int{0}, day("2026-03-04")); err != nil {
		t.Fatalf("recordEmissions: %v", err)
	}

	back, err := LoadRules(anvil)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	for _, r := range back.Rules {
		if r.EmitCount != 0 || r.LastEmitted != "" {
			t.Errorf("a rule that was not selected was stamped: %+v", r)
		}
	}
}

// TestSaveRulesIsAtomic: no reader ever sees a truncated rules file, because a
// YAML sequence cut at a rule boundary is still valid YAML with fewer rules in
// it — a loss indistinguishable from an archive sweep. The observable is that
// the write lands by rename: the destination's inode changes and no temp file
// is left behind.
func TestSaveRulesIsAtomic(t *testing.T) {
	anvil := t.TempDir()
	first := &RulesFile{Rules: []Rule{{ID: "a", Category: "style", Pattern: "p", Check: "c", Added: "2026-01-01"}}}
	if err := SaveRules(anvil, first); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}
	path := RulesPath(anvil)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// A second, much larger write over the top of it.
	big := &RulesFile{}
	for i := 0; i < 200; i++ {
		big.Rules = append(big.Rules, Rule{ID: "r", Category: "style", Pattern: strings.Repeat("word ", 40), Check: "c", Added: "2026-01-01"})
	}
	if err := SaveRules(anvil, big); err != nil {
		t.Fatalf("SaveRules (big): %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the rules file was written in place; a reader can observe the truncation")
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	back, err := LoadRules(anvil)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(back.Rules) != len(big.Rules) {
		t.Errorf("read back %d rules, wrote %d", len(back.Rules), len(big.Rules))
	}
}
