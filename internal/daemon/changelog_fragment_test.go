package daemon

import "testing"

// TestChangelogFragmentMatches pins the completion-signal matcher. It must
// recognize the single-file form plus the two decorations repositories put on
// it: a language split (Munin's changelog.d/<bead>.en.md + <bead>.nb.md, which
// stranded completed work in needs_human as Fhi.Metadata-15ed9) and a
// hyphen-delimited fragment kind (<bead>-technical.<lang>.md, the whole set a
// technical-only change carries, which stranded Fhi.Metadata-hwbwz the same
// way). The bead ids used here carry a hyphen of their own, so the prefix test
// is proved anchored at the full id rather than at the first hyphen.
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
		{"changelog.d/Fhi.Metadata-15ed9-technical.en.md", true}, // kind + language (en)
		{"changelog.d/Fhi.Metadata-15ed9-technical.nb.md", true}, // kind + language (nb)
		{"changelog.d/Fhi.Metadata-15ed9-technical.md", true},    // kind, no language
		{"changelog.d/Fhi.Metadata-other.md", false},             // different bead
		{"changelog.d/Fhi.Metadata-15ed9x.md", false},            // prefix collision, not a fragment
		{"changelog.d/Fhi.Metadata-15ed91.md", false},            // longer id sharing this one's prefix
		{"changelog.d/Fhi.Metadata-15ed91.en.md", false},         // ...and its language split
		{"changelog.d/Fhi.Metadata-15ed91-technical.en.md", false},
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
