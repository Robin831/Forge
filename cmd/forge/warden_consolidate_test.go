package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// consolidateAnvilFixture builds a real git checkout holding a rules file
// with two near-duplicate clusters. A real repository and not a bare temp
// directory because Pass 1 runs its sessions in an ephemeral worktree of the
// anvil, and a directory git knows nothing about never gets that far — the
// clusters would never be attempted and the counts under test would be zero
// for the wrong reason.
//
// CleanGitEnv rather than os.Environ: these tests run inside a worker
// worktree that exports GIT_DIR/GIT_WORK_TREE, and inherited, `git -C <tmp>
// init` reinitializes THAT repository instead.
func consolidateAnvilFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init", "-b", "main")
	run("config", "user.email", "forge@example.com")
	run("config", "user.name", "Forge Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("anvil\n"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

// twoClusterRules is two disjoint near-duplicate pairs: each pair's terse
// member has its whole vocabulary inside the verbose one, which is what the
// overlap criterion clusters on. Two clusters and not one so the summary has
// a denominator worth printing.
func twoClusterRules(t *testing.T) []warden.Rule {
	t.Helper()
	added := time.Now().UTC().Format("2006-01-02")

	logLong := "the documented log filename must match the filename the code actually produces, " +
		"including the rotation suffix and the directory the writer resolves at startup, " +
		"because a stale document sends an operator to a path that holds nothing"
	logShort := "the documented log filename must match the filename the code produces"

	handleShort := "the handle must be deleted only when the caller still owns it, " +
		"checking the generation counter before the close"
	handleLong := handleShort + ", so that a recycled descriptor belonging to another " +
		"session is never closed and no unrelated request loses its socket"

	return []warden.Rule{
		{ID: "log-verbose", Category: "style", Pattern: "documentation filename", Check: logLong, Source: warden.SourceList{"manual"}, Added: added},
		{ID: "log-terse", Category: "style", Pattern: "documentation filename", Check: logShort, Source: warden.SourceList{"manual"}, Added: added},
		{ID: "handle-verbose", Category: "style", Pattern: "handle ownership", Check: handleLong, Source: warden.SourceList{"manual"}, Added: added},
		{ID: "handle-terse", Category: "style", Pattern: "handle ownership", Check: handleShort, Source: warden.SourceList{"manual"}, Added: added},
	}
}

// runConsolidateCmd executes the real `forge warden consolidate <anvil>`
// RunE against a temporary config, capturing what it writes. Everything the
// command reads is package state, so each piece is restored.
func runConsolidateCmd(t *testing.T, anvilPath string, runner warden.ConsolidationRunner) (stdout, stderr string, err error) {
	t.Helper()

	prevCfg := cfg
	t.Cleanup(func() { cfg = prevCfg })
	cfg = &config.Config{
		Anvils: map[string]config.AnvilConfig{"test": {Path: anvilPath}},
	}

	prevCtx := rootCtx
	t.Cleanup(func() { rootCtx = prevCtx })
	rootCtx = context.Background()

	prevRunner := consolidationRunner
	t.Cleanup(func() { consolidationRunner = prevRunner })
	consolidationRunner = func() warden.ConsolidationRunner { return runner }

	var out, errOut bytes.Buffer
	cmd := wardenConsolidateCmd
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	t.Cleanup(func() {
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	})

	err = cmd.RunE(cmd, []string{"test"})
	return out.String(), errOut.String(), err
}

// The defect, stated as a test: every cluster fails, and the command reports
// it. Forced through the real provider seam rather than by constructing a
// result — the bug was never in the struct, it was that a pass which merged
// nothing rendered byte-for-byte like a file with nothing left to merge, and
// exited 0 either way.
func TestWardenConsolidate_EveryClusterFails_ReportsAndExitsNonZero(t *testing.T) {
	anvil := consolidateAnvilFixture(t)
	require.NoError(t, warden.SaveRules(anvil, &warden.RulesFile{Rules: twoClusterRules(t)}))

	stdout, _, err := runConsolidateCmd(t, anvil, func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("stub provider failure")
	})

	require.Error(t, err, "a run whose every cluster failed must exit non-zero")
	assert.NotContains(t, stdout, "already at steady state",
		"a pass that established nothing must never claim the file is consolidated")
	assert.Contains(t, stdout, "0/2 merged, 2 errored")
	assert.Contains(t, stdout, "stub provider failure",
		"the summary must name why the clusters failed, not just how many did")

	// And the file is untouched: nothing merged, so there was nothing to write.
	active, loadErr := warden.LoadRules(anvil)
	require.NoError(t, loadErr)
	assert.Len(t, active.Rules, 4)
}

// The mirror case, so the guard cannot be satisfied by never printing the
// line at all: a file with nothing to merge still reports steady state and
// still exits 0.
func TestWardenConsolidate_NothingToMerge_ReportsSteadyState(t *testing.T) {
	anvil := consolidateAnvilFixture(t)
	require.NoError(t, warden.SaveRules(anvil, &warden.RulesFile{Rules: []warden.Rule{
		{ID: "lonely", Category: "style", Pattern: "p", Check: "c",
			Source: warden.SourceList{"manual"}, Added: time.Now().UTC().Format("2006-01-02")},
	}}))

	stdout, _, err := runConsolidateCmd(t, anvil, func(context.Context, string, string) ([]byte, error) {
		t.Error("no cluster exists, so the provider must never be called")
		return nil, errors.New("unreachable")
	})

	require.NoError(t, err)
	assert.Contains(t, stdout, "already at steady state")
	assert.NotContains(t, stdout, "errored")
}

// One cluster fails and the other merges: the run is still a failure, but
// the counts have to say which is which rather than collapsing to "some
// clusters failed".
func TestWardenConsolidate_PartialFailure_CountsBothSides(t *testing.T) {
	anvil := consolidateAnvilFixture(t)
	require.NoError(t, warden.SaveRules(anvil, &warden.RulesFile{Rules: twoClusterRules(t)}))

	stdout, _, err := runConsolidateCmd(t, anvil, func(_ context.Context, _, prompt string) ([]byte, error) {
		if bytes.Contains([]byte(prompt), []byte("documentation filename")) {
			return nil, errors.New("stub provider failure")
		}
		body, marshalErr := json.Marshal(map[string]string{
			"id":      "merged-handle",
			"pattern": "handle ownership",
			"check":   "delete the handle only when you still own it",
		})
		require.NoError(t, marshalErr)
		return body, nil
	})

	require.Error(t, err)
	assert.Contains(t, stdout, "1/2 merged, 1 errored")
	assert.NotContains(t, stdout, "already at steady state")
	// The cluster that succeeded is still written — a failed sibling is not
	// a reason to discard work the pass completed.
	assert.Contains(t, stdout, "Wrote ")
}

// distinctErrorLines is the summary's own bound: one dead provider fails
// every cluster with one message, and printing it once per cluster would
// bury the counts it sits under.
func TestDistinctErrorLines_DeduplicatesAndCaps(t *testing.T) {
	same := []error{errors.New("boom"), errors.New("boom"), errors.New("boom")}
	assert.Equal(t, []string{"boom"}, distinctErrorLines(same, 3))

	many := []error{
		errors.New("a"), errors.New("b"), errors.New("c"), errors.New("d"), errors.New("e"),
	}
	assert.Equal(t, []string{"a", "b", "c", "... and 2 more distinct error(s)"},
		distinctErrorLines(many, 3))

	assert.Empty(t, distinctErrorLines(nil, 3))
}
