package warden

import (
	"strings"
	"testing"
)

func TestBuildReviewPrompt_OmitsDesignerSnapshot(t *testing.T) {
	designerHunk := strings.Repeat("+// auto-generated snapshot line\n", 4000) // ~120KB
	diff := "diff --git a/src/Migrations/20260423_AddWidget.Designer.cs b/src/Migrations/20260423_AddWidget.Designer.cs\n" +
		"--- a/src/Migrations/20260423_AddWidget.Designer.cs\n" +
		"+++ b/src/Migrations/20260423_AddWidget.Designer.cs\n" +
		"@@ -1 +1,4000 @@\n" +
		designerHunk +
		"diff --git a/src/Migrations/20260423_AddWidget.cs b/src/Migrations/20260423_AddWidget.cs\n" +
		"--- a/src/Migrations/20260423_AddWidget.cs\n" +
		"+++ b/src/Migrations/20260423_AddWidget.cs\n" +
		"@@ -1 +1 @@\n" +
		"+public partial class AddWidget : Migration {}\n"

	prompt := buildReviewPrompt("Forge-e6wl", "Add widget table", "", diff, t.TempDir(), "")

	if strings.Contains(prompt, "auto-generated snapshot line") {
		t.Error("prompt should not contain the elided designer snapshot content")
	}
	if !strings.Contains(prompt, "AddWidget : Migration") {
		t.Error("prompt should still contain the real migration hunk")
	}
	if !strings.Contains(prompt, "auto-generated files were omitted") {
		t.Error("prompt should note that auto-generated files were omitted")
	}
	if !strings.Contains(prompt, "20260423_AddWidget.Designer.cs") {
		t.Error("prompt should name the elided file so the reviewer sees what was dropped")
	}
	if strings.Contains(prompt, "diff truncated,") {
		t.Error("prompt should not trigger truncation once the designer snapshot is filtered")
	}
}

func TestBuildReviewPrompt_TruncatesWhenFilteredDiffStillTooLarge(t *testing.T) {
	// A legitimate non-auto-generated diff larger than the cap should still be
	// truncated, with both the elision note and the truncation note visible.
	designerHunk := strings.Repeat("+// snapshot\n", 200)
	bigRealHunk := strings.Repeat("+// real code line\n", 20000) // > 250KB
	diff := "diff --git a/src/Migrations/20260423_X.Designer.cs b/src/Migrations/20260423_X.Designer.cs\n" +
		"--- a/src/Migrations/20260423_X.Designer.cs\n" +
		"+++ b/src/Migrations/20260423_X.Designer.cs\n" +
		"@@ -1 +1,200 @@\n" +
		designerHunk +
		"diff --git a/src/Bloat.cs b/src/Bloat.cs\n" +
		"--- a/src/Bloat.cs\n" +
		"+++ b/src/Bloat.cs\n" +
		"@@ -1 +1,20000 @@\n" +
		bigRealHunk

	prompt := buildReviewPrompt("Forge-e6wl", "Big change", "", diff, t.TempDir(), "")

	if !strings.Contains(prompt, "auto-generated files were omitted") {
		t.Error("prompt should note that auto-generated files were omitted")
	}
	if !strings.Contains(prompt, "diff truncated,") {
		t.Error("prompt should still truncate when the filtered diff exceeds the cap")
	}
}
