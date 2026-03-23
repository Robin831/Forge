package wicket

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
)

// mockGHClient implements GitHubClient for testing without external gh CLI calls.
type mockGHClient struct {
	issues      map[string][]Issue  // repo -> issues
	comments    []string            // posted comments
	addedLabels map[string][]string // "repo#num" -> labels
	listError   error
}

func newMockGHClient() *mockGHClient {
	return &mockGHClient{
		issues:      make(map[string][]Issue),
		addedLabels: make(map[string][]string),
	}
}

func (m *mockGHClient) ListIssues(_ context.Context, repo string, _ int) ([]Issue, error) {
	if m.listError != nil {
		return nil, m.listError
	}
	return m.issues[repo], nil
}

func (m *mockGHClient) GetIssue(_ context.Context, repo string, number int) (*Issue, error) {
	for _, iss := range m.issues[repo] {
		if iss.Number == number {
			cp := iss
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockGHClient) CommentOnIssue(_ context.Context, _ string, _ int, body string) error {
	m.comments = append(m.comments, body)
	return nil
}

func (m *mockGHClient) AddLabels(_ context.Context, repo string, number int, labels []string) error {
	key := fmt.Sprintf("%s#%d", repo, number)
	m.addedLabels[key] = append(m.addedLabels[key], labels...)
	return nil
}

func (m *mockGHClient) RemoveLabel(_ context.Context, _ string, _ int, _ string) error {
	return nil
}

// stubTriageFn returns a predetermined decision for all issues.
func stubTriageFn(action TriageAction) TriageFn {
	return func(_ context.Context, _ string, _ *Issue, _ string) (*TriageDecision, error) {
		return &TriageDecision{
			Action:    action,
			Title:     "Stub bead",
			Reasoning: "stub",
			Priority:  2,
		}, nil
	}
}

// openTestDB opens a SQLite DB in a temp directory for testing.
func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("failed to open test DB: %v", err)
	}
	return db
}

// makeTestCfg creates a minimal MonitorConfig for testing.
func makeTestCfg(repo string, trustedUsers []string) MonitorConfig {
	return MonitorConfig{
		Anvils: map[string]config.AnvilConfig{
			"myrepo": {
				Path:               ".",
				WicketTrustedUsers: trustedUsers,
				WicketRepos:        []string{repo},
			},
		},
		Settings: config.SettingsConfig{
			WicketInterval:       5 * time.Minute,
			WicketBatchSize:      10,
			WicketProcessedLabel: "forge-triaged",
		},
		Provider: provider.Provider{},
	}
}

func TestMonitor_FiltersPullRequests(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	mc := newMockGHClient()
	mc.issues["owner/repo"] = []Issue{
		{Number: 1, Title: "Bug report", State: "open", Author: IssueAuthor{Login: "alice"}, IsPR: false},
		{Number: 2, Title: "PR: fix thing", State: "open", Author: IssueAuthor{Login: "alice"}, IsPR: true},
	}

	cfg := makeTestCfg("owner/repo", []string{"alice"})
	m := newWithTriageFn(db, cfg, mc, stubTriageFn(ActionFlagHuman))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.pollAll(ctx)

	// Only issue #1 should be tracked (not the PR).
	tracked, err := db.IsIssueTracked("owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !tracked {
		t.Error("expected issue #1 to be tracked")
	}

	tracked2, err := db.IsIssueTracked("owner/repo", 2)
	if err != nil {
		t.Fatal(err)
	}
	if tracked2 {
		t.Error("expected PR #2 to NOT be tracked")
	}
}

func TestMonitor_FiltersAlreadyProcessed(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	mc := newMockGHClient()
	mc.issues["owner/repo"] = []Issue{
		{
			Number: 5,
			Title:  "Already done",
			State:  "open",
			Author: IssueAuthor{Login: "alice"},
			Labels: []IssueLabel{{Name: "forge-triaged"}},
		},
	}

	cfg := makeTestCfg("owner/repo", []string{"alice"})
	m := newWithTriageFn(db, cfg, mc, stubTriageFn(ActionFlagHuman))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.pollAll(ctx)

	// Issue should not be tracked because it already has the processed label.
	tracked, err := db.IsIssueTracked("owner/repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if tracked {
		t.Error("expected already-processed issue to NOT be tracked")
	}
}

func TestMonitor_SkipsUntrustedUsers(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	mc := newMockGHClient()
	mc.issues["owner/repo"] = []Issue{
		{Number: 10, Title: "Feature request", State: "open", Author: IssueAuthor{Login: "unknown-outsider"}},
	}

	cfg := makeTestCfg("owner/repo", []string{"alice", "bob"})
	m := newWithTriageFn(db, cfg, mc, stubTriageFn(ActionFlagHuman))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.pollAll(ctx)

	// Issue should be inserted in DB (seen but not triaged).
	wi, err := db.GetWicketIssue("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if wi == nil {
		t.Fatal("expected issue to be recorded in wicket_issues")
	}
	if wi.IsTrusted {
		t.Error("expected isTrusted to be false for untrusted user")
	}
	// No AI comment should have been posted.
	if len(mc.comments) > 0 {
		t.Errorf("expected no comments for untrusted user, got %d", len(mc.comments))
	}
}

func TestMonitor_DeduplicatesAlreadyTracked(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Pre-insert the issue into the DB.
	err := db.InsertWicketIssue(&state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 7,
		AnvilName:   "myrepo",
		Author:      "alice",
		IsTrusted:   true,
		Status:      "bead_created",
	})
	if err != nil {
		t.Fatal(err)
	}

	mc := newMockGHClient()
	mc.issues["owner/repo"] = []Issue{
		{Number: 7, Title: "Existing issue", State: "open", Author: IssueAuthor{Login: "alice"}},
	}

	cfg := makeTestCfg("owner/repo", []string{"alice"})
	m := newWithTriageFn(db, cfg, mc, stubTriageFn(ActionFlagHuman))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.pollAll(ctx)

	// No new comments — issue was already tracked.
	if len(mc.comments) > 0 {
		t.Errorf("expected no comments for already-tracked issue, got %d", len(mc.comments))
	}
}

func TestMonitor_TrustDetection_CaseInsensitive(t *testing.T) {
	trusted := buildTrustedSet([]string{"Alice", "BOB"})

	cases := []struct {
		login  string
		expect bool
	}{
		{"alice", true},
		{"ALICE", true},
		{"Alice", true},
		{"bob", true},
		{"BOB", true},
		{"charlie", false},
	}
	for _, tc := range cases {
		got := isTrustedUser(tc.login, trusted)
		if got != tc.expect {
			t.Errorf("isTrustedUser(%q) = %v, want %v", tc.login, got, tc.expect)
		}
	}
}

func TestMonitor_FiltersTriggerLabel(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	mc := newMockGHClient()
	mc.issues["owner/repo"] = []Issue{
		{Number: 20, Title: "No trigger label", State: "open", Author: IssueAuthor{Login: "alice"}},
		{
			Number: 21,
			Title:  "Has trigger label",
			State:  "open",
			Author: IssueAuthor{Login: "alice"},
			Labels: []IssueLabel{{Name: "forge-triage"}},
		},
	}

	cfg := makeTestCfg("owner/repo", []string{"alice"})
	cfg.Settings.WicketTriggerLabel = "forge-triage"
	m := newWithTriageFn(db, cfg, mc, stubTriageFn(ActionFlagHuman))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.pollAll(ctx)

	// Issue #20 (no trigger label) should not be tracked.
	tracked20, err := db.IsIssueTracked("owner/repo", 20)
	if err != nil {
		t.Fatal(err)
	}
	if tracked20 {
		t.Error("expected issue without trigger label to NOT be tracked")
	}

	// Issue #21 (has trigger label) should be tracked.
	tracked21, err := db.IsIssueTracked("owner/repo", 21)
	if err != nil {
		t.Fatal(err)
	}
	if !tracked21 {
		t.Error("expected issue with trigger label to be tracked")
	}
}

func TestMonitor_TriagedIssuePostsComment(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	mc := newMockGHClient()
	mc.issues["owner/repo"] = []Issue{
		{Number: 30, Title: "Clear bug", State: "open", Author: IssueAuthor{Login: "alice"}},
	}

	cfg := makeTestCfg("owner/repo", []string{"alice"})
	flagFn := func(_ context.Context, _ string, _ *Issue, _ string) (*TriageDecision, error) {
		return &TriageDecision{
			Action:    ActionFlagHuman,
			Reasoning: "requires strategic decision",
		}, nil
	}
	m := newWithTriageFn(db, cfg, mc, flagFn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.pollAll(ctx)

	// A comment should have been posted.
	if len(mc.comments) == 0 {
		t.Error("expected a comment to be posted for flag_human decision")
	}

	// The issue should be in the DB with the correct action.
	wi, err := db.GetWicketIssue("owner/repo", 30)
	if err != nil {
		t.Fatal(err)
	}
	if wi == nil {
		t.Fatal("expected issue to be tracked")
	}
	if wi.Action != string(ActionFlagHuman) {
		t.Errorf("expected action %q, got %q", ActionFlagHuman, wi.Action)
	}
}

func TestHasAnyLabel(t *testing.T) {
	issue := &Issue{
		Labels: []IssueLabel{{Name: "bug"}, {Name: "help wanted"}},
	}

	if !hasAnyLabel(issue, []string{"bug"}) {
		t.Error("expected hasAnyLabel to return true for 'bug'")
	}
	if !hasAnyLabel(issue, []string{"wontfix", "help wanted"}) {
		t.Error("expected hasAnyLabel to return true when second label matches")
	}
	if hasAnyLabel(issue, []string{"wontfix", "duplicate"}) {
		t.Error("expected hasAnyLabel to return false when no labels match")
	}
}

func TestParseOwnerRepo(t *testing.T) {
	cases := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{"git@github.com:owner/repo.git", "owner/repo", false},
		{"git@github.com:org/my-repo.git", "org/my-repo", false},
		{"https://github.com/owner/repo.git", "owner/repo", false},
		{"https://github.com/owner/repo", "owner/repo", false},
		{"not-a-url", "", true},
	}
	for _, tc := range cases {
		got, err := parseOwnerRepo(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseOwnerRepo(%q) expected error, got %q", tc.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOwnerRepo(%q) unexpected error: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseOwnerRepo(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
