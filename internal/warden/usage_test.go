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
	if !got.LastActivity().Equal(day("2026-03-09")) {
		t.Fatalf("LastActivity = %v", got.LastActivity())
	}
}

func TestLastActivity(t *testing.T) {
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
			if got := r.LastActivity(); !got.Equal(tc.want) {
				t.Fatalf("LastActivity() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLastActivityInReadsDatesInTheGivenZone is the guard on the one thing a
// date-only value can get wrong: LastActivity's stated consumer is the
// staleness sweep, which subtracts it from a local now, and a date parsed in
// UTC and subtracted from a local clock reading is off by the host's offset —
// which is a whole day at the boundary IsStale counts in. LastActivityIn is
// what a caller with a now in hand uses, on the same terms IsStale parses
// Added in now's location.
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
	// which is the error the sweep would inherit from the UTC accessor.
	if diff := r.LastActivity().Sub(got); diff != 5*time.Hour {
		t.Fatalf("UTC and zoned readings differ by %v, want 5h", diff)
	}
	// A nil location is UTC, not a panic.
	if !r.LastActivityIn(nil).Equal(r.LastActivity()) {
		t.Fatalf("nil location did not read as UTC: %v", r.LastActivityIn(nil))
	}
}

func TestLastActivityNilReceiver(t *testing.T) {
	var r *Rule
	if got := r.LastActivity(); !got.IsZero() {
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
	r.MarkFinding(day("2026-03-06"))
	if r.LastFinding != "2026-03-06" || r.EmitCount != 2 {
		t.Fatalf("MarkFinding touched the emit count: %+v", r)
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
