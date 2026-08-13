package diff

import (
	"strings"
	"testing"
)

const keepFilesFixture = "diff --git a/keep.go b/keep.go\n" +
	"--- a/keep.go\n+++ b/keep.go\n@@ -1 +1 @@\n-old\n+new\n" +
	"diff --git a/drop.go b/drop.go\n" +
	"--- a/drop.go\n+++ b/drop.go\n@@ -1 +1 @@\n-old\n+other\n"

func TestKeepFilesFiltersBlocks(t *testing.T) {
	got := KeepFiles(keepFilesFixture, []string{"keep.go"})
	if !strings.Contains(got, "b/keep.go") {
		t.Errorf("expected keep.go block to survive:\n%s", got)
	}
	if strings.Contains(got, "drop.go") {
		t.Errorf("drop.go block should have been removed:\n%s", got)
	}
}

func TestKeepFilesEmptyWhenNothingMatches(t *testing.T) {
	if got := KeepFiles(keepFilesFixture, []string{"absent.go"}); got != "" {
		t.Errorf("no matching files must yield an empty diff, got:\n%s", got)
	}
	if got := KeepFiles(keepFilesFixture, nil); got != "" {
		t.Errorf("an empty file list must yield an empty diff, got:\n%s", got)
	}
	if got := KeepFiles("not a diff at all", []string{"keep.go"}); got != "" {
		t.Errorf("a diff with no block headers must yield empty, got:\n%s", got)
	}
}
