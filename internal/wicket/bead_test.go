package wicket

import (
	"context"
	"errors"
	"testing"

	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestDB creates a temporary state.DB for testing and registers cleanup.
func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestBuildBDArgs verifies that buildBDArgs produces the correct argument slice
// for the bd create command.
func TestBuildBDArgs(t *testing.T) {
	decision := TriageDecision{
		Action:          ActionCreateBead,
		Reason:          "clear task",
		BeadTitle:       "Fix login bug",
		BeadDescription: "The login form crashes on empty password.",
	}
	issue := Issue{
		Repo:   "owner/myrepo",
		Number: 42,
	}

	args := buildBDArgs(decision, issue, 2, "my-anvil")

	assertFlag(t, args, "--title", "Fix login bug")

	desc := flagValue(args, "--description")
	assert.Contains(t, desc, "The login form crashes on empty password.", "--description missing original text")
	assert.Contains(t, desc, "https://github.com/owner/myrepo/issues/42", "--description missing source URL")

	assertFlag(t, args, "--type", "task")
	assertFlag(t, args, "--priority", "2")
	assertContainsFlag(t, args, "--labels", "wicket,github-issue")

	assert.Contains(t, args, "--silent", "expected --silent flag in args")

	meta := flagValue(args, "--metadata")
	assert.Contains(t, meta, `"anvil_name"`, "--metadata missing anvil_name key")
	assert.Contains(t, meta, "my-anvil", "--metadata missing anvil name value")
}

// TestBuildBDArgs_NoAnvil verifies that buildBDArgs omits --metadata when
// anvilName is empty.
func TestBuildBDArgs_NoAnvil(t *testing.T) {
	decision := TriageDecision{
		Action:          ActionCreateBead,
		BeadTitle:       "T",
		BeadDescription: "D",
	}
	issue := Issue{Repo: "owner/repo", Number: 1}

	args := buildBDArgs(decision, issue, 2, "")
	assert.NotContains(t, args, "--metadata", "expected no --metadata when anvilName is empty")
}

// TestParseBDOutput covers the bead ID extraction from bd output.
func TestParseBDOutput(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name:   "plain bead ID",
			output: "Forge-abc1\n",
			want:   "Forge-abc1",
		},
		{
			name:   "bead ID with surrounding whitespace",
			output: "  Forge-xyz9  \n",
			want:   "Forge-xyz9",
		},
		{
			name:   "bead ID preceded by blank line",
			output: "\nForge-q1w2\n",
			want:   "Forge-q1w2",
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			output:  "   \n\n   ",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBDOutput(tc.output)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCreateBead_StoresMapping verifies that CreateBead persists the
// issue→bead mapping to state.db after a successful bd create invocation.
func TestCreateBead_StoresMapping(t *testing.T) {
	db := openTestDB(t)

	// Pre-insert the wicket_issues row as the monitor would.
	err := db.InsertWicketIssue(state.WicketIssue{
		Repo:        "org/repo",
		IssueNumber: 7,
		Title:       "Some issue",
		State:       "pending",
	})
	require.NoError(t, err)

	original := bdRunner
	defer func() { bdRunner = original }()

	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-test1\n", nil
	}

	decision := TriageDecision{
		Action:          ActionCreateBead,
		BeadTitle:       "Do something",
		BeadDescription: "Details here.",
	}
	issue := Issue{Repo: "org/repo", Number: 7}

	id, err := CreateBead(context.Background(), db, decision, issue, 2, "test-anvil")
	require.NoError(t, err)
	assert.Equal(t, "Forge-test1", id)

	// Verify the DB row was updated with the bead ID and correct state.
	wi, err := db.GetWicketIssue("org/repo", 7)
	require.NoError(t, err)
	require.NotNil(t, wi, "expected wicket_issues row to exist")
	assert.Equal(t, "Forge-test1", wi.BeadID)
	assert.Equal(t, "bead_created", wi.State)
	assert.NotNil(t, wi.ProcessedAt)
}

// TestCreateBead_InsertsRowWhenMissing verifies that CreateBead inserts a
// wicket_issues row when none exists yet.
func TestCreateBead_InsertsRowWhenMissing(t *testing.T) {
	db := openTestDB(t)

	original := bdRunner
	defer func() { bdRunner = original }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-new1\n", nil
	}

	decision := TriageDecision{
		Action:          ActionCreateBead,
		BeadTitle:       "New issue",
		BeadDescription: "No prior row.",
	}
	issue := Issue{Repo: "org/repo", Number: 99}

	id, err := CreateBead(context.Background(), db, decision, issue, 1, "test-anvil")
	require.NoError(t, err)
	assert.Equal(t, "Forge-new1", id)

	wi, err := db.GetWicketIssue("org/repo", 99)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "Forge-new1", wi.BeadID)
	assert.Equal(t, "bead_created", wi.State)
}

// TestCreateBead_RunnerError verifies that CreateBead propagates runner errors.
func TestCreateBead_RunnerError(t *testing.T) {
	original := bdRunner
	defer func() { bdRunner = original }()

	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "", errors.New("bd not found")
	}

	decision := TriageDecision{
		Action:          ActionCreateBead,
		BeadTitle:       "Title",
		BeadDescription: "Desc",
	}
	issue := Issue{Repo: "org/repo", Number: 1}

	_, err := CreateBead(context.Background(), nil, decision, issue, 2, "")
	assert.Error(t, err)
}

// TestCreateBead_ValidationErrors checks that invalid inputs are rejected.
func TestCreateBead_ValidationErrors(t *testing.T) {
	baseDecision := TriageDecision{
		Action:          ActionCreateBead,
		BeadTitle:       "Title",
		BeadDescription: "Desc",
	}
	issue := Issue{Repo: "org/repo", Number: 1}

	tests := []struct {
		name     string
		decision TriageDecision
		priority int
	}{
		{
			name:     "wrong action",
			decision: TriageDecision{Action: ActionAskClarify, BeadTitle: "T", BeadDescription: "D"},
			priority: 2,
		},
		{
			name:     "missing title",
			decision: TriageDecision{Action: ActionCreateBead, BeadDescription: "D"},
			priority: 2,
		},
		{
			name:     "missing description",
			decision: TriageDecision{Action: ActionCreateBead, BeadTitle: "T"},
			priority: 2,
		},
		{
			name:     "priority too low",
			decision: baseDecision,
			priority: -1,
		},
		{
			name:     "priority too high",
			decision: baseDecision,
			priority: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateBead(context.Background(), nil, tc.decision, issue, tc.priority, "")
			assert.Error(t, err, "expected validation error")
		})
	}
}

// TestIssueURL verifies the URL helper.
func TestIssueURL(t *testing.T) {
	got := issueURL("owner/repo", 99)
	assert.Equal(t, "https://github.com/owner/repo/issues/99", got)
}

// ---- helpers ----------------------------------------------------------------

// flagValue returns the value immediately following the first occurrence of
// flag in args, or "" if not found.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// assertFlag fails if flag is not followed by want in args.
func assertFlag(t *testing.T, args []string, flag, want string) {
	t.Helper()
	got := flagValue(args, flag)
	assert.Equal(t, want, got, "flag %s", flag)
}

// assertContainsFlag fails if flag is not followed by value anywhere in args
// (handles repeated flags like --tag).
func assertContainsFlag(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return
		}
	}
	assert.Failf(t, "flag not found", "expected %s %s in args %v", flag, value, args)
}
