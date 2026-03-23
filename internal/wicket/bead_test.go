package wicket

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	args := buildBDArgs(decision, issue, 2)

	assertFlag(t, args, "--title", "Fix login bug")

	desc := flagValue(args, "--description")
	assert.Contains(t, desc, "The login form crashes on empty password.", "--description missing original text")
	assert.Contains(t, desc, "https://github.com/owner/myrepo/issues/42", "--description missing source URL")

	assertFlag(t, args, "--type", "task")
	assertFlag(t, args, "--priority", "2")
	assertContainsFlag(t, args, "--tag", "wicket")
	assertContainsFlag(t, args, "--tag", "github-issue")

	assert.Contains(t, args, "--silent", "expected --silent flag in args")
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

// TestCreateBead_StoresMapping verifies that CreateBead records the
// issue→bead mapping after a successful bd create invocation.
func TestCreateBead_StoresMapping(t *testing.T) {
	// Reset the global store before the test.
	wicketIssues.mu.Lock()
	wicketIssues.mapping = make(map[issueKey]string)
	wicketIssues.mu.Unlock()

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

	id, err := CreateBead(context.Background(), decision, issue, 2)
	require.NoError(t, err)
	assert.Equal(t, "Forge-test1", id)

	stored, ok := BeadIDFor("org/repo", 7)
	require.True(t, ok, "BeadIDFor returned false; expected mapping to be stored")
	assert.Equal(t, "Forge-test1", stored)
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

	_, err := CreateBead(context.Background(), decision, issue, 2)
	assert.Error(t, err)
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

