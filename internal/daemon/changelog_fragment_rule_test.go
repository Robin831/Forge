package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/changelog"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anvilWithFragmentGlobs builds a config whose one anvil states its own
// fragment convention, the way a repository whose CI gate accepts a shape the
// built-in grammar does not would.
func anvilWithFragmentGlobs(anvil, path string, globs ...string) *config.Config {
	return &config.Config{Anvils: map[string]config.AnvilConfig{
		anvil: {Path: path, Changelog: &config.ChangelogConfig{FragmentGlobs: globs}},
	}}
}

// TestConfiguredFragmentGlobsAreReadFromTheAnvil is the bead's substance: the
// convention belongs to the repository, so an anvil that accepts an
// underscore-delimited kind in its own CI gate says so in config and Forge
// reads a branch carrying only that shape as finished work — no change in
// Forge required the next time a repository adds a kind.
func TestConfiguredFragmentGlobsAreReadFromTheAnvil(t *testing.T) {
	const anvilName = "munin"
	const beadID = "Fhi.Metadata-hwbwz"

	anvilPath := initTestGitRepo(t)
	pushForgeBranch(t, anvilPath, beadID, true, "_technical")
	sha := branchHeadSHA(t, anvilPath, worktree.BranchName(beadID))

	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Unconfigured, the built-in grammar refuses the underscore form — and
	// reports it as drift rather than as an absence, which is the whole point.
	d.cfg.Store(&config.Config{Anvils: map[string]config.AnvilConfig{anvilName: {Path: anvilPath}}})
	found, near, err := d.branchHasChangelogFragment(context.Background(), anvilPath, sha, beadID,
		d.changelogFragmentRule(anvilName))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, []string{"changelog.d/" + beadID + "_technical.md"}, near,
		"a plausible fragment the rule refuses must be reported, not silently counted as absent")

	// Configured, the same tree is a completion signal.
	d.cfg.Store(anvilWithFragmentGlobs(anvilName, anvilPath, "{bead}.md", "{bead}_*.md"))
	found, near, err = d.branchHasChangelogFragment(context.Background(), anvilPath, sha, beadID,
		d.changelogFragmentRule(anvilName))
	require.NoError(t, err)
	assert.True(t, found, "the anvil's own convention must be what decides")
	assert.Empty(t, near, "a run that found the fragment has no drift to report")
}

// TestStrandedBranchEscalationNamesTheDrift is the failure mode the bead
// describes, seen from where it actually surfaces. A false negative here is
// indistinguishable from genuinely unfinished work, and until now its only
// symptom was an escalation an operator had to read and disbelieve — so the
// escalation must now name the files it refused and the rule it refused them
// under.
func TestStrandedBranchEscalationNamesTheDrift(t *testing.T) {
	const anvilName = "munin"
	const beadID = "Fhi.Metadata-hwbwz"

	anvilPath := initTestGitRepo(t)
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer db.Close()

	mockVCS := &mockVCSProvider{}
	d := &Daemon{
		db:           db,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:  worktree.NewManager(),
		vcsProviders: map[string]vcs.Provider{anvilName: mockVCS},
	}
	d.cfg.Store(&config.Config{Anvils: map[string]config.AnvilConfig{anvilName: {Path: anvilPath}}})

	bead := poller.Bead{ID: beadID, Anvil: anvilName, Title: "test-infra fix"}
	pushForgeBranch(t, anvilPath, beadID, true, "_technical")

	if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
		t.Fatal("a branch whose fragment the rule refuses must not be dispatched over")
	}
	assert.Equal(t, int32(0), mockVCS.createPRCalls.Load(),
		"an unrecognised fragment shape is not a completion signal — the guard still holds")

	r, err := db.GetRetry(bead.ID, bead.Anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.True(t, r.NeedsHuman)
	assert.Contains(t, r.LastError, beadID+"_technical.md",
		"the escalation must name the file it refused")
	assert.Contains(t, r.LastError, "changelog.fragment_globs",
		"the escalation must name the setting that resolves the disagreement")
}

// TestDriftNoteIsSilentWhenThereIsNothingToReport: the note is a report of a
// specific disagreement, so a branch that simply carries no fragment at all —
// the ordinary unfinished-work case — must escalate in exactly the words it
// always did.
func TestDriftNoteIsSilentWhenThereIsNothingToReport(t *testing.T) {
	var rule changelog.FragmentRule
	if note := changelogFragmentDriftNote(rule, "Forge-3jyi5", nil); note != "" {
		t.Errorf("expected no note with no near misses, got %q", note)
	}
	note := changelogFragmentDriftNote(rule, "Forge-3jyi5", []string{"changelog.d/Forge-3jyi5_x.md"})
	if !strings.Contains(note, "changelog.d/Forge-3jyi5_x.md") || !strings.Contains(note, "changelog.d/Forge-3jyi5.md") {
		t.Errorf("note should name both the refused file and the rule in force: %q", note)
	}
}

// TestOpenPRRefusalQuotesTheAnvilsOwnRule: the create-pr precondition used to
// spell the accepted shapes out in its own error string, which is a second copy
// of the convention in the one message an operator reads when it disagrees with
// them. It now renders the rule in force, so a configured anvil is told what it
// configured.
func TestOpenPRRefusalQuotesTheAnvilsOwnRule(t *testing.T) {
	const anvilName = "munin"
	const beadID = "Fhi.Metadata-hwbwz"

	anvilPath := initTestGitRepo(t)
	mockVCS := &mockVCSProvider{}
	d, _ := newCreatePRTestDaemon(t, anvilName, anvilPath, mockVCS)
	d.cfg.Store(anvilWithFragmentGlobs(anvilName, anvilPath, "{bead}_*.md"))

	// The branch carries the BUILT-IN shape, which this anvil has configured
	// itself out of — so it is refused, and the refusal quotes the anvil's rule.
	pushForgeBranch(t, anvilPath, beadID, true, "-technical.en")

	_, _, err := d.openPRForExistingBranch(context.Background(), beadID, anvilName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changelog.d/"+beadID+"_*.md",
		"the refusal must quote the shapes this anvil's rule accepts")
	assert.NotContains(t, err.Error(), "alphabetic kind",
		"it must not quote the built-in grammar the anvil replaced")
	assert.Equal(t, int32(0), mockVCS.createPRCalls.Load())
}
