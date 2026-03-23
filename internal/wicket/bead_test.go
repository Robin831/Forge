package wicket

import (
	"context"
	"errors"
	"strings"
	"testing"
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

	wantTitle := "Fix login bug"
	wantDescContains := "The login form crashes on empty password."
	wantURLContains := "https://github.com/owner/myrepo/issues/42"

	assertFlag(t, args, "--title", wantTitle)

	desc := flagValue(args, "--description")
	if !strings.Contains(desc, wantDescContains) {
		t.Errorf("--description missing original text; got %q", desc)
	}
	if !strings.Contains(desc, wantURLContains) {
		t.Errorf("--description missing source URL; got %q", desc)
	}

	assertFlag(t, args, "--type", "task")
	assertFlag(t, args, "--priority", "2")
	assertContainsFlag(t, args, "--tag", "wicket")
	assertContainsFlag(t, args, "--tag", "github-issue")

	if !containsArg(args, "--silent") {
		t.Error("expected --silent flag in args")
	}
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
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
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
	if err != nil {
		t.Fatalf("CreateBead returned error: %v", err)
	}
	if id != "Forge-test1" {
		t.Errorf("got id %q, want %q", id, "Forge-test1")
	}

	stored, ok := BeadIDFor("org/repo", 7)
	if !ok {
		t.Fatal("BeadIDFor returned false; expected mapping to be stored")
	}
	if stored != "Forge-test1" {
		t.Errorf("stored id %q, want %q", stored, "Forge-test1")
	}
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
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestIssueURL verifies the URL helper.
func TestIssueURL(t *testing.T) {
	got := issueURL("owner/repo", 99)
	want := "https://github.com/owner/repo/issues/99"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
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
	if got != want {
		t.Errorf("flag %s: got %q, want %q", flag, got, want)
	}
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
	t.Errorf("expected %s %s in args %v", flag, value, args)
}

// containsArg reports whether arg appears in args.
func containsArg(args []string, arg string) bool {
	for _, a := range args {
		if a == arg {
			return true
		}
	}
	return false
}
