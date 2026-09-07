package vcs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseIssueRef(t *testing.T) {
	tests := []struct {
		input string
		want  IssueRef
	}{
		{"gh-42", IssueRef{Number: "42"}},
		{"gh-", IssueRef{}},
		{"gh-abc", IssueRef{}},
		{"", IssueRef{}},
		{"jira-123", IssueRef{}},
		{"https://github.com/FHIDev/Munin/issues/5574", IssueRef{Owner: "FHIDev", Repo: "Munin", Number: "5574"}},
		{"https://github.com/org/repo/pull/42", IssueRef{}},
		{"https://gitlab.com/org/repo/issues/42", IssueRef{}},
		{"https://example.com/org/repo/issues/42", IssueRef{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseIssueRef(tt.input))
		})
	}
}

func TestIssueRefString(t *testing.T) {
	assert.Equal(t, "FHIDev/Munin#5574",
		IssueRef{Owner: "FHIDev", Repo: "Munin", Number: "5574"}.String(),
		"a full URL ref must render the qualified cross-repo form unconditionally")
	assert.Equal(t, "#42", IssueRef{Number: "42"}.String())
	assert.Equal(t, "", IssueRef{}.String())
}

func TestIssueRefSameIssue(t *testing.T) {
	qualified := IssueRef{Owner: "FHIDev", Repo: "Munin", Number: "5574"}
	assert.True(t, qualified.SameIssue(IssueRef{Owner: "fhidev", Repo: "munin", Number: "5574"}),
		"owner/repo comparison is case-insensitive")
	assert.False(t, qualified.SameIssue(IssueRef{Owner: "FHIDev", Repo: "Explorer", Number: "5574"}))
	assert.False(t, qualified.SameIssue(IssueRef{Number: "5574"}),
		"a bare #N cannot be proven to be the qualified issue — the PR may live in another repo")
	assert.True(t, IssueRef{Number: "42"}.SameIssue(IssueRef{Number: "42"}))
	assert.False(t, IssueRef{Number: "42"}.SameIssue(IssueRef{Number: "43"}))
	assert.False(t, IssueRef{}.SameIssue(IssueRef{}))
}

func TestEnsureIssueReferences(t *testing.T) {
	munin5499 := IssueRef{Owner: "FHIDev", Repo: "Munin", Number: "5499"}

	t.Run("appends closing reference", func(t *testing.T) {
		got := EnsureIssueReferences("Body text", munin5499, false, nil)
		assert.Contains(t, got, "Closes FHIDev/Munin#5499")
	})

	t.Run("refsOnly appends Refs and demotes every closing ref", func(t *testing.T) {
		got := EnsureIssueReferences("Fix.\n\nCloses FHIDev/Munin#5499", munin5499, true, nil)
		assert.Contains(t, got, "Refs FHIDev/Munin#5499")
		assert.NotContains(t, got, "Closes")
	})

	// Defect 3 (Forge-jhf1): PR #5414 carried "Closes #5411" while the worked
	// bead's issue was #5409 — the wrong model-written line suppressed the
	// correct injection and closed the wrong issue.
	t.Run("wrong-issue closing refs are demoted in every accepted form", func(t *testing.T) {
		body := "Summary.\n\nCloses #5411\nfixes FHIDev/Explorer#7\nResolves https://github.com/FHIDev/Munin/issues/5508"
		got := EnsureIssueReferences(body, munin5499, false, nil)
		assert.Contains(t, got, "Refs #5411")
		assert.Contains(t, got, "Refs FHIDev/Explorer#7")
		assert.Contains(t, got, "Refs https://github.com/FHIDev/Munin/issues/5508")
		assert.Contains(t, got, "Closes FHIDev/Munin#5499")
		assert.NotContains(t, got, "Closes #5411")
		assert.NotContains(t, got, "fixes FHIDev/Explorer#7")
		assert.NotContains(t, got, "Resolves https://")
	})

	t.Run("matching closing ref is kept and not duplicated", func(t *testing.T) {
		body := "Summary.\n\nCloses FHIDev/Munin#5499"
		got := EnsureIssueReferences(body, munin5499, false, nil)
		assert.Equal(t, 1, countOccurrences(got, "Closes FHIDev/Munin#5499"))
	})

	t.Run("bare same-number ref is demoted when close ref is qualified", func(t *testing.T) {
		// In a cross-repo PR a bare "#5499" resolves against the PR's own
		// repository — it cannot be trusted even when the number matches.
		got := EnsureIssueReferences("Closes #5499", munin5499, false, nil)
		assert.Contains(t, got, "Refs #5499")
		assert.Contains(t, got, "Closes FHIDev/Munin#5499")
	})

	t.Run("source refs are appended as Refs", func(t *testing.T) {
		src := IssueRef{Owner: "FHIDev", Repo: "Munin", Number: "5524"}
		got := EnsureIssueReferences("Body", munin5499, false, []IssueRef{src})
		assert.Contains(t, got, "Closes FHIDev/Munin#5499")
		assert.Contains(t, got, "Refs FHIDev/Munin#5524")
	})

	t.Run("source ref equal to close ref is not duplicated", func(t *testing.T) {
		got := EnsureIssueReferences("Body", munin5499, false, []IssueRef{munin5499})
		assert.Equal(t, 1, countOccurrences(got, "FHIDev/Munin#5499"))
	})

	t.Run("zero close ref appends nothing but still demotes", func(t *testing.T) {
		got := EnsureIssueReferences("Closes #12", IssueRef{}, false, nil)
		assert.Equal(t, "Refs #12", got)
	})

	t.Run("empty body gets just the reference", func(t *testing.T) {
		got := EnsureIssueReferences("", munin5499, false, nil)
		assert.Equal(t, "Closes FHIDev/Munin#5499", got)
	})
}

func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func TestParseSourceRefs(t *testing.T) {
	t.Run("wicket source line", func(t *testing.T) {
		desc := "A bug someone reported.\n\nSource: https://github.com/FHIDev/Munin/issues/5524"
		refs := ParseSourceRefs(desc)
		assert.Equal(t, []IssueRef{{Owner: "FHIDev", Repo: "Munin", Number: "5524"}}, refs)
	})

	t.Run("duplicates folded, non-issue and mid-line URLs ignored", func(t *testing.T) {
		desc := "Source: https://github.com/a/b/issues/1\n" +
			"Source: https://github.com/a/b/issues/1\n" +
			"Source: https://github.com/a/b/pull/9\n" +
			"see Source: not-a-url\n"
		refs := ParseSourceRefs(desc)
		assert.Equal(t, []IssueRef{{Owner: "a", Repo: "b", Number: "1"}}, refs)
	})

	t.Run("empty description", func(t *testing.T) {
		assert.Empty(t, ParseSourceRefs(""))
	})
}

func TestNoCloseLabels(t *testing.T) {
	t.Cleanup(func() { SetNoCloseLabels(nil) })

	assert.Equal(t, []string{"innmeldt"}, NoCloseLabels(), "default honours the innmeldt convention")

	SetNoCloseLabels([]string{"do-not-close", "innmeldt"})
	assert.Equal(t, []string{"do-not-close", "innmeldt"}, NoCloseLabels())

	SetNoCloseLabels(nil)
	assert.Equal(t, []string{"innmeldt"}, NoCloseLabels(), "empty set restores the default")
}
