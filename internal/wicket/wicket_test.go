package wicket

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultSettings returns a SettingsConfig with Wicket defaults for tests.
func defaultSettings() config.SettingsConfig {
	return config.SettingsConfig{
		WicketEnabled:          true,
		WicketInterval:         15 * time.Minute,
		WicketBatchSize:        20,
		WicketProcessedLabel:   "forge-wicket-processed",
		WicketNeedsHumanLabel:  "forge-needs-human",
		WicketBeadCreatedLabel: "forge-bead-created",
		WicketTriggerLabel:     "",
	}
}

// newTestMonitor creates a Monitor wired up for tests: real state.DB in a temp
// dir, MockGitHubClient, and a stub bdRunner so no external processes are spawned.
// triageFunc is set to a fast no-op to avoid spawning real AI subprocesses.
func newTestMonitor(t *testing.T) (*Monitor, *MockGitHubClient, *state.DB) {
	t.Helper()
	db := openTestDB(t)
	mock := &MockGitHubClient{}
	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg: &config.Config{
			Settings: defaultSettings(),
		},
		rl: newRateLimiter(),
		// Stub triageFunc so tests don't spawn real AI providers or call bd list.
		// For trusted users the result is overridden to create_bead anyway; for
		// all other paths triageFunc is not called.
		triageFunc: func(_ context.Context, _ Issue, _ []Comment, _ TriageConfig) TriageDecision {
			return TriageDecision{Action: ActionFlagHuman, Reason: "test mock"}
		},
	}
	return m, mock, db
}

// ---- anvilPathsForRepos -----------------------------------------------------

func TestAnvilPathsForRepos_MultipleAnvilsShareRepo(t *testing.T) {
	// Two anvils ("forge" and "meta") both list "org/Forge" in WicketRepos.
	// A lookup for that single slug must return both paths.
	m := &Monitor{
		cfg: &config.Config{
			Anvils: map[string]config.AnvilConfig{
				"forge": {Path: "/path/forge", WicketRepos: []string{"org/Forge"}},
				"meta":  {Path: "/path/meta", WicketRepos: []string{"org/Forge"}},
				"other": {Path: "/path/other", WicketRepos: []string{"org/Other"}},
			},
		},
		rl: newRateLimiter(),
	}

	// Looking up a single repo that is shared by two anvils should return both paths.
	got := m.anvilPathsForRepos([]string{"org/Forge"})
	require.ElementsMatch(t, []string{"/path/forge", "/path/meta"}, got)
}

func TestAnvilPathsForRepos_CaseInsensitive(t *testing.T) {
	m := &Monitor{
		cfg: &config.Config{
			Anvils: map[string]config.AnvilConfig{
				"forge": {Path: "/path/forge", WicketRepos: []string{"Org/Forge"}},
			},
		},
		rl: newRateLimiter(),
	}

	got := m.anvilPathsForRepos([]string{"org/forge"}) // lowercase lookup
	require.Equal(t, []string{"/path/forge"}, got)
}

func TestAnvilPathsForRepos_EmptyRepos(t *testing.T) {
	m := &Monitor{
		cfg: &config.Config{
			Anvils: map[string]config.AnvilConfig{
				"forge": {Path: "/path/forge", WicketRepos: []string{"org/Forge"}},
			},
		},
		rl: newRateLimiter(),
	}

	got := m.anvilPathsForRepos(nil)
	require.Nil(t, got)
}

func TestAnvilPathsForRepos_NoMatch(t *testing.T) {
	m := &Monitor{
		cfg: &config.Config{
			Anvils: map[string]config.AnvilConfig{
				"forge": {Path: "/path/forge", WicketRepos: []string{"org/Forge"}},
			},
		},
		rl: newRateLimiter(),
	}

	got := m.anvilPathsForRepos([]string{"org/Unrelated"})
	require.Empty(t, got)
}

// ---- isTrustedUser ----------------------------------------------------------

func TestIsTrustedUser(t *testing.T) {
	tests := []struct {
		name    string
		author  string
		trusted []string
		want    bool
	}{
		{name: "exact match", author: "alice", trusted: []string{"alice", "bob"}, want: true},
		{name: "case insensitive", author: "Alice", trusted: []string{"alice"}, want: true},
		{name: "upper in list", author: "alice", trusted: []string{"ALICE"}, want: true},
		{name: "not in list", author: "charlie", trusted: []string{"alice", "bob"}, want: false},
		{name: "empty list", author: "alice", trusted: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTrustedUser(tc.author, tc.trusted))
		})
	}
}

// ---- shouldSkip -------------------------------------------------------------

func TestShouldSkip_ProcessedLabel(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	settings := defaultSettings()

	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{
			name:   "has processed label",
			labels: []string{"bug", "forge-wicket-processed"},
			want:   true,
		},
		{
			name:   "has bead-created label",
			labels: []string{"forge-bead-created"},
			want:   true,
		},
		{
			name:   "has needs-human label",
			labels: []string{"enhancement", "forge-needs-human"},
			want:   true,
		},
		{
			name:   "no wicket labels",
			labels: []string{"bug", "help wanted"},
			want:   false,
		},
		{
			name:   "no labels",
			labels: nil,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{Repo: "org/repo", Number: 100, Labels: tc.labels}
			assert.Equal(t, tc.want, m.shouldSkip(issue, settings))
		})
	}
}

func TestShouldSkip_AlreadyTracked(t *testing.T) {
	m, _, db := newTestMonitor(t)
	settings := defaultSettings()

	issue := Issue{Repo: "org/repo", Number: 42}

	// Not yet tracked — should not be skipped.
	assert.False(t, m.shouldSkip(issue, settings))

	// Insert a row to simulate a previously triaged issue.
	err := db.InsertWicketIssue(state.WicketIssue{
		Repo:        "org/repo",
		IssueNumber: 42,
		Title:       "Some issue",
		State:       "bead_created",
	})
	require.NoError(t, err)

	// Now should be skipped.
	assert.True(t, m.shouldSkip(issue, settings))
}

func TestShouldSkip_DBError(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	settings := defaultSettings()

	// Close the DB to force an error on the next call.
	m.db.Close()

	issue := Issue{Repo: "org/repo", Number: 1}
	// Should skip conservatively when DB is unavailable.
	assert.True(t, m.shouldSkip(issue, settings))
}

// ---- isWicketEnabled --------------------------------------------------------

func TestIsWicketEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name          string
		anvilEnabled  *bool
		globalEnabled bool
		want          bool
	}{
		{name: "global on, no override", anvilEnabled: nil, globalEnabled: true, want: true},
		{name: "global off, no override", anvilEnabled: nil, globalEnabled: false, want: false},
		{name: "global on, anvil off", anvilEnabled: &falseVal, globalEnabled: true, want: false},
		{name: "global off, anvil on", anvilEnabled: &trueVal, globalEnabled: false, want: true},
		{name: "both on", anvilEnabled: &trueVal, globalEnabled: true, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			anvil := config.AnvilConfig{WicketEnabled: tc.anvilEnabled}
			assert.Equal(t, tc.want, isWicketEnabled(anvil, tc.globalEnabled))
		})
	}
}

// ---- cross-anvil dedup (first-anvil-wins) -----------------------------------

// TestTriageIssue_SecondAnvilSkips verifies that when an issue is already
// tracked in state.db (by a first anvil), a second anvil attempting to triage
// the same issue skips without creating a duplicate DB record or posting any
// GitHub comment.
func TestTriageIssue_SecondAnvilSkips(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	issue := Issue{
		Repo:   "org/repo",
		Number: 99,
		Title:  "Duplicate issue",
		Body:   "Body text.",
		Author: "alice",
	}

	// Simulate the first anvil having already claimed the issue.
	err := db.InsertWicketIssue(state.WicketIssue{
		Repo:        issue.Repo,
		IssueNumber: issue.Number,
		Title:       issue.Title,
		State:       "bead_created",
	})
	require.NoError(t, err)

	// Capture log output during the second-anvil triage call.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr) // restore default

	// Second anvil attempts to triage the same issue.
	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"alice"}}
	m.triageIssue(context.Background(), "second-anvil", issue, anvilCfg, settings)

	// The warning log must be emitted so regressions don't silently revert to
	// a generic insert-failure message.
	assert.True(t, strings.Contains(logBuf.String(), "already tracked by another anvil, skipping"),
		"expected 'already tracked by another anvil, skipping' in log output, got: %s", logBuf.String())

	// No GitHub comment should have been posted.
	assert.Empty(t, mock.CommentCalls, "second anvil must not post a comment for a duplicate issue")

	// The DB row should still reflect the first anvil's state (bead_created),
	// not have been overwritten.
	wi, err := db.GetWicketIssue("org/repo", 99)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "bead_created", wi.State, "first-anvil-wins: state must remain bead_created")
}

// ---- full triage dispatch flow ----------------------------------------------

// TestTriageDispatch_CreateBead verifies that the create_bead path creates a
// bead, posts a comment, and adds the correct labels.
func TestTriageDispatch_CreateBead(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	// Override bdRunner to avoid shelling out to the real bd binary.
	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-test1\n", nil
	}

	issue := Issue{
		Repo:   "org/repo",
		Number: 7,
		Title:  "Add dark mode",
		Body:   "Please add a dark mode toggle to the UI.",
		Author: "alice",
	}
	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"alice"}}

	// alice is trusted — no AI call, direct create_bead dispatch.
	m.triageIssue(context.Background(), "testrepo", issue, anvilCfg, settings)

	// Bead should be recorded in state.db.
	wi, err := db.GetWicketIssue("org/repo", 7)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "bead_created", wi.State)
	assert.Equal(t, "Forge-test1", wi.BeadID)

	// A comment should have been posted.
	require.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, "org/repo", mock.CommentCalls[0].Repo)
	assert.Equal(t, 7, mock.CommentCalls[0].Number)
	assert.Contains(t, mock.CommentCalls[0].Body, "Forge-test1")

	// Both labels should have been applied.
	require.Len(t, mock.AddLabelCalls, 1)
	labels := mock.AddLabelCalls[0].Labels
	assert.Contains(t, labels, settings.WicketProcessedLabel)
	assert.Contains(t, labels, settings.WicketBeadCreatedLabel)
}

// TestTriageDispatch_AskClarify verifies the clarification path via the AI
// triage runner.
func TestTriageDispatch_AskClarify(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	issue := Issue{
		Repo:   "org/repo",
		Number: 10,
		Title:  "Something broke",
		Body:   "It does not work.",
		Author: "external-user",
	}
	// Inject a stubbed triage runner that always asks for clarification.
	clarifyDecision := TriageDecision{
		Action: ActionAskClarify,
		Reason: "issue body is too vague",
	}

	// We call dispatchDecision directly so we do not need a real AI provider.
	_ = m.db.InsertWicketIssue(state.WicketIssue{
		Repo:        issue.Repo,
		IssueNumber: issue.Number,
		Title:       issue.Title,
		Body:        issue.Body,
		Author:      issue.Author,
		State:       "pending",
	})
	m.dispatchDecision(context.Background(), "testrepo", issue, clarifyDecision, settings)

	// State should be ask_clarify.
	wi, err := db.GetWicketIssue("org/repo", 10)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "ask_clarify", wi.State)

	// Clarification comment posted.
	require.Len(t, mock.CommentCalls, 1)
	assert.Contains(t, mock.CommentCalls[0].Body, "Clarification needed")

	// Only the processed label (not the needs-human label).
	require.Len(t, mock.AddLabelCalls, 1)
	assert.Equal(t, []string{settings.WicketProcessedLabel}, mock.AddLabelCalls[0].Labels)
}

// TestTriageDispatch_FlagHuman verifies the flag_human path.
func TestTriageDispatch_FlagHuman(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	issue := Issue{
		Repo:   "org/repo",
		Number: 20,
		Title:  "Redesign the entire product",
		Body:   "Please rebuild everything from scratch.",
		Author: "manager",
	}
	flagDecision := TriageDecision{
		Action: ActionFlagHuman,
		Reason: "scope is too large for automation",
	}

	_ = m.db.InsertWicketIssue(state.WicketIssue{
		Repo:        issue.Repo,
		IssueNumber: issue.Number,
		Title:       issue.Title,
		State:       "pending",
	})
	m.dispatchDecision(context.Background(), "testrepo", issue, flagDecision, settings)

	// State should be needs_human.
	wi, err := db.GetWicketIssue("org/repo", 20)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "needs_human", wi.State)

	// Flag comment posted.
	require.Len(t, mock.CommentCalls, 1)
	assert.Contains(t, mock.CommentCalls[0].Body, "Flagged for human review")

	// Both processed and needs-human labels applied.
	require.Len(t, mock.AddLabelCalls, 1)
	labels := mock.AddLabelCalls[0].Labels
	assert.Contains(t, labels, settings.WicketProcessedLabel)
	assert.Contains(t, labels, settings.WicketNeedsHumanLabel)
}

// TestScanRepo_FiltersAlreadyTracked verifies that scanRepo skips issues that
// are already tracked and only triages genuinely new ones.
func TestScanRepo_FiltersAlreadyTracked(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	// Pre-insert issue #1 as already tracked.
	err := db.InsertWicketIssue(state.WicketIssue{
		Repo:        "org/repo",
		IssueNumber: 1,
		Title:       "Old issue",
		State:       "bead_created",
	})
	require.NoError(t, err)

	// Issue #2 is new (not in DB, no processed label).
	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Old issue", Author: "alice"},
		{Repo: "org/repo", Number: 2, Title: "New issue", Author: "bob"},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	// Stub bdRunner so issue #2 can be triaged without real processes.
	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-x1\n", nil
	}

	// bob is trusted so AI is bypassed.
	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"bob"}}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only issue #2 should have been commented on.
	assert.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, 2, mock.CommentCalls[0].Number)
}

// TestScanRepo_ListIssuesError verifies graceful handling when ListIssues fails.
func TestScanRepo_ListIssuesError(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return nil, errors.New("network error")
	}

	// Should not panic or call CommentOnIssue.
	m.scanRepo(context.Background(), "testrepo", "org/repo", config.AnvilConfig{}, settings)
	assert.Empty(t, mock.CommentCalls)
}

// TestScanRepo_BatchSizeLimit verifies that the batch size cap is respected.
func TestScanRepo_BatchSizeLimit(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()
	settings.WicketBatchSize = 2

	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Issue 1", Author: "alice"},
		{Repo: "org/repo", Number: 2, Title: "Issue 2", Author: "alice"},
		{Repo: "org/repo", Number: 3, Title: "Issue 3", Author: "alice"},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	callCount := 0
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		callCount++
		return "Forge-y1\n", nil
	}

	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"alice"}}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only 2 out of 3 issues should have been processed.
	assert.Equal(t, 2, callCount, "expected batch size to limit processing to 2 issues")
}

// TestUpdateConfig verifies that UpdateConfig replaces the configuration in a
// thread-safe manner and the new values are visible in subsequent reads.
func TestUpdateConfig(t *testing.T) {
	m, _, _ := newTestMonitor(t)

	newCfg := &config.Config{
		Settings: config.SettingsConfig{
			WicketEnabled:  true,
			WicketInterval: 30 * time.Minute,
		},
	}
	m.UpdateConfig(newCfg)

	m.mu.RLock()
	got := m.cfg.Settings.WicketInterval
	m.mu.RUnlock()

	assert.Equal(t, 30*time.Minute, got)
}

// TestDeriveRepo_SSHAndHTTPS covers URL parsing in deriveRepo.
func TestDeriveRepo_ParseRemoteURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"git@github.com:owner/myrepo.git", "owner/myrepo"},
		{"git@github.com:owner/myrepo", "owner/myrepo"},
		{"https://github.com/owner/myrepo.git", "owner/myrepo"},
		{"https://github.com/owner/myrepo", "owner/myrepo"},
		{"http://github.com/owner/myrepo", "owner/myrepo"},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			if m := reGitHubSSH.FindStringSubmatch(tc.raw); m != nil {
				assert.Equal(t, tc.want, m[1])
				return
			}
			if m := reGitHubHTTPS.FindStringSubmatch(tc.raw); m != nil {
				assert.Equal(t, tc.want, m[1])
				return
			}
			t.Fatalf("neither SSH nor HTTPS regex matched %q", tc.raw)
		})
	}
}

// ---- isIgnoredUser ----------------------------------------------------------

func TestIsIgnoredUser_BotList(t *testing.T) {
	tests := []struct {
		name   string
		author string
		want   bool
	}{
		{name: "dependabot", author: "dependabot[bot]", want: true},
		{name: "renovate", author: "renovate[bot]", want: true},
		{name: "github-actions", author: "github-actions[bot]", want: true},
		{name: "codecov", author: "codecov[bot]", want: true},
		{name: "case insensitive", author: "Dependabot[Bot]", want: true},
		{name: "real user", author: "alice", want: false},
		{name: "empty", author: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isIgnoredUser(tc.author, nil))
		})
	}
}

func TestIsIgnoredUser_CustomList(t *testing.T) {
	custom := []string{"spambot", "BadActor"}
	assert.True(t, isIgnoredUser("spambot", custom))
	assert.True(t, isIgnoredUser("BADACTOR", custom))   // case-insensitive
	assert.False(t, isIgnoredUser("gooduser", custom))
}

// ---- hasLabel / hasAllLabels ------------------------------------------------

func TestHasLabel(t *testing.T) {
	issue := Issue{Labels: []string{"bug", "enhancement", "Help Wanted"}}
	assert.True(t, hasLabel(issue, "bug"))
	assert.True(t, hasLabel(issue, "BUG"))       // case-insensitive
	assert.True(t, hasLabel(issue, "help wanted")) // case-insensitive
	assert.False(t, hasLabel(issue, "wontfix"))
}

func TestHasAllLabels(t *testing.T) {
	issue := Issue{Labels: []string{"bug", "backend"}}
	assert.True(t, hasAllLabels(issue, []string{"bug", "backend"}))
	assert.True(t, hasAllLabels(issue, []string{"bug"}))
	assert.True(t, hasAllLabels(issue, nil))
	assert.False(t, hasAllLabels(issue, []string{"bug", "frontend"}))
	assert.False(t, hasAllLabels(issue, []string{"nonexistent"}))
}

// ---- isLikelySpam -----------------------------------------------------------

func TestIsLikelySpam(t *testing.T) {
	tests := []struct {
		name  string
		issue Issue
		want  bool
	}{
		{
			name:  "completely empty submission",
			issue: Issue{Title: "", Body: ""},
			want:  true,
		},
		{
			name:  "placeholder title no body",
			issue: Issue{Title: "test", Body: ""},
			want:  true,
		},
		{
			name:  "placeholder title no body case-insensitive",
			issue: Issue{Title: "TESTING", Body: ""},
			want:  true,
		},
		{
			name:  "placeholder title with body is not spam",
			issue: Issue{Title: "TESTING", Body: "some body"},
			want:  false, // body present, "testing" only in no-body placeholder list
		},
		{
			name:  "short legitimate title with no body is not spam",
			issue: Issue{Title: "hi", Body: ""},
			want:  false, // "hi" is not a known placeholder
		},
		{
			name:  "always-spam title no body",
			issue: Issue{Title: "asdfgh", Body: ""},
			want:  true,
		},
		{
			name:  "always-spam title with body",
			issue: Issue{Title: "qwerty", Body: "Please help me with this."},
			want:  true, // always-spam regardless of body
		},
		{
			name:  "real issue",
			issue: Issue{Title: "Login form crashes on empty password", Body: "Steps to reproduce..."},
			want:  false,
		},
		{
			name:  "short title with body",
			issue: Issue{Title: "hi", Body: "This is a detailed issue description."},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isLikelySpam(tc.issue))
		})
	}
}

// ---- trigger label filter ---------------------------------------------------

func TestScanRepo_TriggerLabelFilter(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()
	settings.WicketTriggerLabel = "forge-triage"

	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Has trigger", Author: "alice", Labels: []string{"forge-triage"}},
		{Repo: "org/repo", Number: 2, Title: "No trigger", Author: "alice", Labels: []string{"bug"}},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-tr1\n", nil
	}

	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"alice"}}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only issue #1 (with trigger label) should be processed.
	assert.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, 1, mock.CommentCalls[0].Number)
}

func TestScanRepo_TriggerLabelEmpty_ProcessesAll(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()
	settings.WicketTriggerLabel = "" // no trigger label = push model

	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Issue 1", Author: "alice"},
		{Repo: "org/repo", Number: 2, Title: "Issue 2", Author: "alice"},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-x1\n", nil
	}

	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"alice"}}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Both issues should be processed when no trigger label is required.
	assert.Len(t, mock.CommentCalls, 2)
}

// ---- issue label filter (hasAllLabels) --------------------------------------

func TestScanRepo_IssueLabelFilter(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Has all labels", Author: "alice", Labels: []string{"bug", "backend"}},
		{Repo: "org/repo", Number: 2, Title: "Missing label", Author: "alice", Labels: []string{"bug"}},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-il1\n", nil
	}

	// Require both "bug" and "backend" labels.
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketIssueLabels:  []string{"bug", "backend"},
	}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only issue #1 has both required labels.
	assert.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, 1, mock.CommentCalls[0].Number)
}

// ---- bot ignore list --------------------------------------------------------

func TestScanRepo_BotIgnored(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Bot issue", Author: "dependabot[bot]"},
		{Repo: "org/repo", Number: 2, Title: "Human issue", Author: "alice"},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-b1\n", nil
	}

	anvilCfg := config.AnvilConfig{WicketTrustedUsers: []string{"alice"}}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only issue #2 (human) should be processed.
	assert.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, 2, mock.CommentCalls[0].Number)
}

func TestScanRepo_CustomIgnoreUser(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	issues := []Issue{
		{Repo: "org/repo", Number: 1, Title: "Ignored user issue", Author: "noise-bot"},
		{Repo: "org/repo", Number: 2, Title: "Normal issue", Author: "alice"},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-ci1\n", nil
	}

	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketIgnoreUsers:  []string{"noise-bot"},
	}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only alice's issue should be processed.
	assert.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, 2, mock.CommentCalls[0].Number)
}

// ---- non-trusted user flow --------------------------------------------------

func TestNonTrustedUser_GenericResponse(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	issue := Issue{
		Repo:   "org/repo",
		Number: 30,
		Title:  "Feature request from external user",
		Body:   "Please add support for dark mode with steps to reproduce.",
		Author: "external-contributor",
	}
	// No trusted users configured — external-contributor is non-trusted.
	anvilCfg := config.AnvilConfig{}

	m.triageIssue(context.Background(), "testrepo", issue, anvilCfg, settings)

	// Should have posted a generic response (not flagged-for-human or bead-created).
	require.Len(t, mock.CommentCalls, 1)
	assert.Contains(t, mock.CommentCalls[0].Body, "external-contributor")
	assert.Contains(t, mock.CommentCalls[0].Body, "maintainer will review")

	// Should have applied processed + needs-human labels.
	require.Len(t, mock.AddLabelCalls, 1)
	labels := mock.AddLabelCalls[0].Labels
	assert.Contains(t, labels, settings.WicketProcessedLabel)
	assert.Contains(t, labels, settings.WicketNeedsHumanLabel)

	// State should be needs_human.
	wi, err := db.GetWicketIssue("org/repo", 30)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "needs_human", wi.State)
}

func TestNonTrustedUser_SpamRejectedSilently(t *testing.T) {
	m, mock, db := newTestMonitor(t)
	settings := defaultSettings()

	issue := Issue{
		Repo:   "org/repo",
		Number: 31,
		Title:  "test", // known spam title
		Body:   "",
		Author: "spammer",
	}
	anvilCfg := config.AnvilConfig{}

	m.triageIssue(context.Background(), "testrepo", issue, anvilCfg, settings)

	// No public comment should be posted for spam.
	assert.Empty(t, mock.CommentCalls)

	// Only the processed label (no needs-human label for spam).
	require.Len(t, mock.AddLabelCalls, 1)
	labels := mock.AddLabelCalls[0].Labels
	assert.Contains(t, labels, settings.WicketProcessedLabel)
	assert.NotContains(t, labels, settings.WicketNeedsHumanLabel)

	// State should be rejected.
	wi, err := db.GetWicketIssue("org/repo", 31)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, "rejected", wi.State)
}

// ---- trigger + issue label interaction --------------------------------------

func TestScanRepo_TriggerAndIssueLabelsBothApply(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()
	settings.WicketTriggerLabel = "forge-triage"

	issues := []Issue{
		// Has trigger + all required labels — should be processed.
		{Repo: "org/repo", Number: 1, Title: "OK", Author: "alice", Labels: []string{"forge-triage", "bug"}},
		// Has trigger but missing required label — should be filtered.
		{Repo: "org/repo", Number: 2, Title: "Missing label", Author: "alice", Labels: []string{"forge-triage"}},
		// Has required label but missing trigger — should be filtered.
		{Repo: "org/repo", Number: 3, Title: "Missing trigger", Author: "alice", Labels: []string{"bug"}},
	}
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return issues, nil
	}

	origRunner := bdRunner
	defer func() { bdRunner = origRunner }()
	bdRunner = func(_ context.Context, _ []string) (string, error) {
		return "Forge-tl1\n", nil
	}

	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketIssueLabels:  []string{"bug"},
	}
	m.scanRepo(context.Background(), "testrepo", "org/repo", anvilCfg, settings)

	// Only issue #1 passes both filters.
	require.Len(t, mock.CommentCalls, 1)
	assert.Equal(t, 1, mock.CommentCalls[0].Number)
}

// TestBuildProviders verifies provider selection priority.
func TestBuildProviders(t *testing.T) {
	t.Run("wicket provider takes priority", func(t *testing.T) {
		s := config.SettingsConfig{
			WicketProvider: "gemini",
			Providers:      []string{"claude"},
		}
		pvs := buildProviders(s)
		require.Len(t, pvs, 1)
		assert.Equal(t, "gemini", string(pvs[0].Kind))
	})

	t.Run("falls back to global providers", func(t *testing.T) {
		s := config.SettingsConfig{
			Providers: []string{"claude", "gemini"},
		}
		pvs := buildProviders(s)
		require.Len(t, pvs, 2)
	})

	t.Run("defaults when nothing configured", func(t *testing.T) {
		pvs := buildProviders(config.SettingsConfig{})
		assert.NotEmpty(t, pvs)
	})
}

// ---- Auth error handling in poll loop (Forge-9lta) -------------------------

// TestScanRepo_AuthError_ContinuesPollLoop verifies that when ListIssues
// returns an authentication error (401/403), scanRepo logs a clear actionable
// message including the repo name and returns rateLimited=false so the poll
// loop continues scanning other repos — it does not crash or exit early.
func TestScanRepo_AuthError_ContinuesPollLoop(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{}

	// Capture log output to verify the auth-specific message is emitted.
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	authErr := errors.New("exit status 1\nstderr: HTTP 401 Unauthorized — bad credentials")
	mock.OnListIssues = func(_ context.Context, repo string, _ []string) ([]Issue, error) {
		return nil, authErr
	}

	rateLimited := m.scanRepo(context.Background(), "test-anvil", "org/private-repo", anvilCfg, settings)

	assert.False(t, rateLimited, "auth error should not be treated as rate limited")

	logged := logBuf.String()
	assert.Contains(t, logged, "org/private-repo", "log should include repo name")
	assert.Contains(t, logged, "authentication failure", "log should mention authentication failure")
	assert.Contains(t, logged, "gh auth status", "log should suggest running gh auth status")
}

// TestScanRepo_AuthError_Does_Not_Crash verifies that after an auth error
// the monitor is still usable and can process subsequent repos in the loop.
func TestScanRepo_AuthError_Does_Not_Crash(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	callCount := 0
	mock.OnListIssues = func(_ context.Context, repo string, _ []string) ([]Issue, error) {
		callCount++
		switch repo {
		case "org/private-repo":
			return nil, errors.New("HTTP 403 SAML SSO enforcement")
		default:
			return []Issue{}, nil
		}
	}

	// Two separate scanRepo calls simulating sequential repo scanning.
	r1 := m.scanRepo(context.Background(), "test-anvil", "org/private-repo", config.AnvilConfig{}, settings)
	r2 := m.scanRepo(context.Background(), "test-anvil", "org/public-repo", config.AnvilConfig{}, settings)

	assert.False(t, r1, "auth error repo should return rateLimited=false")
	assert.False(t, r2, "subsequent repo should also succeed")
	assert.Equal(t, 2, callCount, "both repos should be scanned")
}
