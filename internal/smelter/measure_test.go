package smelter

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
)

// repoWidePathCount is the acceptance metric for the Pass 3 narrowing: how many
// of a rules file's rules carry three or more repo-wide globs — a rule that
// names a language three times over and a location not at all, which is the
// shape that joins the Warden checklist on nearly every diff.
//
// It is computed with the package's own repoWideGlob rather than a grep, so the
// number quoted for a run is counted by the same definition the rewrite acts
// on; a script with its own idea of "repo-wide" would report a before/after
// about something else.
func repoWidePathCount(rf *warden.RulesFile, atLeast int) int {
	var n int
	for _, r := range rf.Rules {
		wide := 0
		for _, p := range r.Paths {
			if repoWideGlob(p) {
				wide++
			}
		}
		if wide >= atLeast {
			n++
		}
	}
	return n
}

// TestMeasureRepoWidePaths reports the before/after of the narrowing over a
// REAL rules file and the REAL pull requests behind it. It is skipped unless
// both env vars are set, because it makes one `gh api` call per distinct source
// PR (258 for this repository's own 727-rule file, 529 for a 2295-rule one) and
// nothing about the network belongs in the ordinary test run.
//
//	FORGE_MEASURE_ANVIL=<path to the anvil checkout> \
//	go test ./internal/smelter -run TestMeasureRepoWidePaths -v -timeout 60m
//
// The anvil path is both where the rules file is read from
// (<anvil>/.forge/warden-rules.yaml) and the directory gh resolves
// {owner}/{repo} against, which is what keeps a run from counting one
// repository's rules against another repository's pull requests.
//
// It writes nothing: the file is loaded, narrowed in memory and counted.
func TestMeasureRepoWidePaths(t *testing.T) {
	anvilPath := os.Getenv("FORGE_MEASURE_ANVIL")
	if anvilPath == "" {
		t.Skip("set FORGE_MEASURE_ANVIL to the anvil checkout to run the measurement")
	}

	rf, err := warden.LoadRules(anvilPath)
	if err != nil {
		t.Fatalf("loading rules from %s: %v", anvilPath, err)
	}

	report := func(label string) {
		total := len(rf.Rules)
		if total == 0 {
			t.Fatalf("%s: rules file holds no rules", label)
		}
		for _, atLeast := range []int{1, 2, 3} {
			n := repoWidePathCount(rf, atLeast)
			t.Logf("%s: %d/%d rules (%.1f%%) carry >=%d repo-wide glob(s)",
				label, n, total, 100*float64(n)/float64(total), atLeast)
		}
	}

	report("before")
	result := pathsBackfill(context.Background(), anvilPath, "measurement", rf)
	t.Log(fmt.Sprintf("pass 3: %s", result.summary("measurement")))
	report("after")
}
