package changelog

import (
	"strings"
	"testing"
)

const ruleTestBead = "Fhi.Metadata-15ed9"

// TestZeroRuleIsTheBuiltInConvention pins the default. An anvil that configures
// nothing must keep exactly the behaviour it had before the setting existed —
// every shape the two live escalations widened the grammar to, and none of the
// ones the grammar deliberately refuses.
func TestZeroRuleIsTheBuiltInConvention(t *testing.T) {
	var rule FragmentRule
	cases := []struct {
		path string
		want bool
	}{
		{"changelog.d/Fhi.Metadata-15ed9.md", true},
		{"changelog.d/Fhi.Metadata-15ed9.en.md", true},
		{"changelog.d/Fhi.Metadata-15ed9-technical.nb.md", true},
		{" changelog.d/Fhi.Metadata-15ed9.md ", true},
		{"changelog.d/Fhi.Metadata-15ed9.7.md", false},      // bd child bead
		{"changelog.d/Fhi.Metadata-15ed91.md", false},       // longer id
		{"docs/Fhi.Metadata-15ed9.md", false},               // wrong directory
		{"changelog.d/nested/Fhi.Metadata-15ed9.md", false}, // not a flat name
		{"changelog.d/Fhi.Metadata-15ed9.txt", false},
	}
	for _, tc := range cases {
		if got := rule.MatchesPath(tc.path, ruleTestBead); got != tc.want {
			t.Errorf("MatchesPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if rule.ResolvedDir() != DefaultFragmentDir {
		t.Errorf("ResolvedDir() = %q, want %q", rule.ResolvedDir(), DefaultFragmentDir)
	}
}

// TestConfiguredGlobsReplaceTheBuiltInGrammar is the escape hatch: an anvil
// whose own CI gate accepts an underscore-delimited kind says so, and gets it.
// The replacement is total rather than additive — a list must name every shape
// the repository uses — which is the property this asserts in both directions.
func TestConfiguredGlobsReplaceTheBuiltInGrammar(t *testing.T) {
	rule := FragmentRule{Globs: []string{"{bead}.md", "{bead}_*.md"}}
	if !rule.MatchesPath("changelog.d/Fhi.Metadata-15ed9_technical.md", ruleTestBead) {
		t.Error("a configured underscore glob must match the shape it names")
	}
	if !rule.MatchesPath("changelog.d/Fhi.Metadata-15ed9.md", ruleTestBead) {
		t.Error("the bare form was configured and must match")
	}
	if rule.MatchesPath("changelog.d/Fhi.Metadata-15ed9-technical.md", ruleTestBead) {
		t.Error("a configured list replaces the built-in grammar rather than extending it")
	}
	// A configured list must not let a sibling bead's fragment through either.
	if rule.MatchesPath("changelog.d/Fhi.Metadata-other_technical.md", ruleTestBead) {
		t.Error("a glob anchored on {bead} must not match another bead's fragment")
	}
}

// TestConfiguredDirIsHonoured covers the second half of the convention: a
// repository that keeps fragments somewhere other than changelog.d/ needs only
// to say where, without restating the naming grammar to keep it.
func TestConfiguredDirIsHonoured(t *testing.T) {
	rule := FragmentRule{Dir: "docs/changes/"}
	if rule.ResolvedDir() != "docs/changes" {
		t.Errorf("ResolvedDir() = %q, want %q", rule.ResolvedDir(), "docs/changes")
	}
	if !rule.MatchesPath("docs/changes/Fhi.Metadata-15ed9-technical.en.md", ruleTestBead) {
		t.Error("the built-in grammar must still apply under a configured directory")
	}
	if rule.MatchesPath("changelog.d/Fhi.Metadata-15ed9.md", ruleTestBead) {
		t.Error("the default directory is not searched once another one is configured")
	}
}

// TestBeadIDIsMatchedAsALiteral guards the substitution: a bead id is a value
// Forge did not write, so one carrying glob syntax must anchor the pattern
// rather than widen it.
func TestBeadIDIsMatchedAsALiteral(t *testing.T) {
	rule := FragmentRule{Globs: []string{"{bead}.md"}}
	const odd = "weird*bead"
	if !rule.MatchesName("weird*bead.md", odd) {
		t.Error("the bead id must match itself literally")
	}
	if rule.MatchesName("weirdOTHERbead.md", odd) {
		t.Error("a `*` in a bead id must not be read as glob syntax")
	}
}

// TestNearMissesReportsDriftAndNotCorrectRejections is the drift signal, and
// the line it has to draw. A shape the repository plainly chose for this bead
// is drift and must be reported; a bd child bead's <parent>.<n>.md is a
// rejection that is CORRECT, and reporting it would put a permanent false alarm
// on every parent escalation — the noise that buries the report it exists to
// make.
func TestNearMissesReportsDriftAndNotCorrectRejections(t *testing.T) {
	var rule FragmentRule
	paths := []string{
		"changelog.d/Fhi.Metadata-15ed9_technical.md",   // drift: underscore kind
		"changelog.d/Fhi.Metadata-15ed9-2fa.md",         // drift: neither language nor word
		"changelog.d/Fhi.Metadata-15ed9-technical.md",   // matches — not a near miss
		"changelog.d/Fhi.Metadata-15ed9.7.md",           // a child bead's, correctly refused
		"changelog.d/Fhi.Metadata-15ed9.7-technical.md", // ditto
		"changelog.d/Fhi.Metadata-15ed91.md",            // a longer id, not this bead at all
		"changelog.d/Fhi.Metadata-other.md",             // another bead entirely
		"docs/Fhi.Metadata-15ed9_technical.md",          // outside the directory
	}
	got := rule.NearMisses(paths, ruleTestBead)
	want := []string{
		"changelog.d/Fhi.Metadata-15ed9-2fa.md",
		"changelog.d/Fhi.Metadata-15ed9_technical.md",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("NearMisses = %v, want %v", got, want)
	}
}

// TestNearMissesFollowTheConfiguredRule: once an anvil states its convention,
// drift is measured against THAT and not against the built-in grammar — a
// repository that configured the underscore form has no drift to report on it,
// and now has some on the hyphen form it stopped using.
func TestNearMissesFollowTheConfiguredRule(t *testing.T) {
	rule := FragmentRule{Globs: []string{"{bead}.md", "{bead}_*.md"}}
	got := rule.NearMisses([]string{
		"changelog.d/Fhi.Metadata-15ed9_technical.md",
		"changelog.d/Fhi.Metadata-15ed9-technical.md",
	}, ruleTestBead)
	if len(got) != 1 || got[0] != "changelog.d/Fhi.Metadata-15ed9-technical.md" {
		t.Errorf("NearMisses = %v, want only the hyphen form", got)
	}
}

// TestDescribeNamesTheRuleInForce: a refusal quotes the shapes the matcher
// actually uses, so a message can never describe a convention that moved.
func TestDescribeNamesTheRuleInForce(t *testing.T) {
	def := FragmentRule{}.Describe(ruleTestBead)
	if !strings.Contains(def, "changelog.d/"+ruleTestBead+".md") {
		t.Errorf("default Describe should name the bare form: %q", def)
	}
	cfg := FragmentRule{Dir: "docs/changes", Globs: []string{"{bead}_*.md"}}.Describe(ruleTestBead)
	if !strings.Contains(cfg, "docs/changes/"+ruleTestBead+"_*.md") {
		t.Errorf("configured Describe should name the configured shape: %q", cfg)
	}
	if strings.Contains(cfg, "alphabetic kind") {
		t.Errorf("configured Describe must not quote the built-in grammar: %q", cfg)
	}
}

// TestValidateFragmentGlobs covers the two silent, opposite failures the check
// exists for: a pattern that matches nothing (every stranded branch reads as
// unfinished) and one with no {bead} in it (every stranded branch reads as
// finished, on a sibling's fragment).
func TestValidateFragmentGlobs(t *testing.T) {
	if errs := ValidateFragmentGlobs([]string{"{bead}.md", "{bead}-*.md"}); len(errs) != 0 {
		t.Errorf("valid globs reported errors: %v", errs)
	}
	for _, bad := range []string{"", "*.md", "changelog.d/{bead}.md", "{bead}[.md"} {
		if errs := ValidateFragmentGlobs([]string{bad}); len(errs) == 0 {
			t.Errorf("ValidateFragmentGlobs(%q) accepted an invalid pattern", bad)
		}
	}
}
