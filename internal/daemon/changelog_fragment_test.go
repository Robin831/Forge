package daemon

import "testing"

// TestChangelogFragmentMatches pins the completion-signal matcher: it must
// recognize both the single-file form and the language-split form (Munin uses
// changelog.d/<bead>.en.md + <bead>.nb.md). Matching only <bead>.md stranded
// completed language-split work in needs_human (Fhi.Metadata-15ed9).
func TestChangelogFragmentMatches(t *testing.T) {
	const bead = "Fhi.Metadata-15ed9"
	cases := []struct {
		path string
		want bool
	}{
		{"changelog.d/Fhi.Metadata-15ed9.md", true},      // single-file form
		{"changelog.d/Fhi.Metadata-15ed9.en.md", true},   // language-split (en)
		{"changelog.d/Fhi.Metadata-15ed9.nb.md", true},   // language-split (nb)
		{" changelog.d/Fhi.Metadata-15ed9.en.md ", true}, // ls-tree line whitespace
		{"changelog.d/Fhi.Metadata-15ed9.fr-CA.md", true},
		{"changelog.d/Fhi.Metadata-other.md", false},     // different bead
		{"changelog.d/Fhi.Metadata-15ed9x.md", false},    // prefix collision, not a fragment
		{"changelog.d/Fhi.Metadata-15ed9.en.txt", false}, // not .md
		{"docs/Fhi.Metadata-15ed9.md", false},            // wrong dir
		{"changelog.d/Fhi.Metadata-15ed9", false},        // no .md suffix
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := changelogFragmentMatches(tc.path, bead); got != tc.want {
				t.Errorf("changelogFragmentMatches(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
