package wicket

import (
	"context"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

func TestParseLabelComment(t *testing.T) {
	tests := []struct {
		body    string
		wantTag string
		wantOK  bool
	}{
		{"label forge-priority", "forge-priority", true},
		{"Label  my-tag", "my-tag", true},
		{"LABEL urgent", "urgent", true},
		{"dispatch", "", false},
		{"label", "", false},
		{"label ", "", false},
		{"not a label command", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		tag, ok := parseLabelComment(tt.body)
		if ok != tt.wantOK {
			t.Errorf("parseLabelComment(%q): ok=%v, want %v", tt.body, ok, tt.wantOK)
		}
		if tag != tt.wantTag {
			t.Errorf("parseLabelComment(%q): tag=%q, want %q", tt.body, tag, tt.wantTag)
		}
	}
}

func TestCheckDispatchRocketReaction(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{
		OnListReactions: func(_ context.Context, repo string, number int) ([]Reaction, error) {
			return []Reaction{{Content: "rocket", User: "alice"}}, nil
		},
		OnListComments: func(_ context.Context, repo string, number int) ([]Comment, error) {
			return nil, nil
		},
	}

	// Seed a bead_created wicket issue.
	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 42,
		Title:       "Test issue",
		Author:      "alice",
		State:       StateBeadCreated,
		BeadID:      "test-bead-1",
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	dispatched := false
	var bdUpdateArgs []string
	bdUpdateRunner = func(_ context.Context, beadID string, args []string) error {
		dispatched = true
		bdUpdateArgs = args
		return nil
	}
	t.Cleanup(func() { bdUpdateRunner = defaultBDUpdateRunner })

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	settings := config.SettingsConfig{}
	anvilCfg := config.AnvilConfig{
		WicketAutoDispatch: false,
		WicketRepos:        []string{"owner/repo"},
	}

	m.checkDispatch(context.Background(), "test-anvil", anvilCfg, settings)

	if !dispatched {
		t.Error("expected bead to be dispatched via rocket reaction")
	}
	if len(bdUpdateArgs) < 2 || bdUpdateArgs[0] != "--add-label" || bdUpdateArgs[1] != "auto-dispatch" {
		t.Errorf("unexpected bd update args: %v", bdUpdateArgs)
	}
	if len(mock.CommentCalls) == 0 {
		t.Error("expected dispatch confirmation comment to be posted")
	}

	// Verify state updated to dispatched.
	updated, err := db.GetWicketIssue("owner/repo", 42)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.State != StateDispatched {
		t.Errorf("expected state %q, got %q", StateDispatched, updated.State)
	}
}

func TestCheckDispatchCommentKeyword(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{
		OnListReactions: func(_ context.Context, repo string, number int) ([]Reaction, error) {
			return nil, nil // no rocket
		},
		OnListComments: func(_ context.Context, repo string, number int) ([]Comment, error) {
			return []Comment{
				{Author: "alice", Body: "Please dispatch this"},
			}, nil
		},
	}

	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 7,
		Title:       "Another issue",
		Author:      "alice",
		State:       StateBeadCreated,
		BeadID:      "test-bead-2",
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	dispatched := false
	bdUpdateRunner = func(_ context.Context, beadID string, args []string) error {
		dispatched = true
		return nil
	}
	t.Cleanup(func() { bdUpdateRunner = defaultBDUpdateRunner })

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	settings := config.SettingsConfig{}
	anvilCfg := config.AnvilConfig{
		WicketAutoDispatch: false,
		WicketRepos:        []string{"owner/repo"},
	}

	m.checkDispatch(context.Background(), "test-anvil", anvilCfg, settings)

	if !dispatched {
		t.Error("expected bead to be dispatched via dispatch comment")
	}
}

func TestCheckDispatchLabelComment(t *testing.T) {
	db := openTestDB(t)
	var appliedTag string
	mock := &MockGitHubClient{
		OnListReactions: func(_ context.Context, repo string, number int) ([]Reaction, error) {
			return nil, nil
		},
		OnListComments: func(_ context.Context, repo string, number int) ([]Comment, error) {
			return []Comment{
				{Author: "alice", Body: "label priority-1"},
			}, nil
		},
	}

	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 99,
		Title:       "Label test issue",
		Author:      "alice",
		State:       StateBeadCreated,
		BeadID:      "test-bead-3",
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	bdUpdateRunner = func(_ context.Context, beadID string, args []string) error {
		if len(args) >= 2 && args[0] == "--add-label" {
			appliedTag = args[1]
		}
		return nil
	}
	t.Cleanup(func() { bdUpdateRunner = defaultBDUpdateRunner })

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	settings := config.SettingsConfig{}
	anvilCfg := config.AnvilConfig{
		WicketAutoDispatch: false,
		WicketRepos:        []string{"owner/repo"},
	}

	m.checkDispatch(context.Background(), "test-anvil", anvilCfg, settings)

	if appliedTag != "priority-1" {
		t.Errorf("expected label %q to be applied, got %q", "priority-1", appliedTag)
	}
	// Label command posts a comment re-asking about dispatch.
	if len(mock.CommentCalls) == 0 {
		t.Error("expected a follow-up comment after label command")
	}
}

func TestCheckDispatchSkipsAutoDispatchAnvils(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	anvilCfg := config.AnvilConfig{
		WicketAutoDispatch: true, // should skip dispatch confirmation
		WicketRepos:        []string{"owner/repo"},
	}

	m.checkDispatch(context.Background(), "test-anvil", anvilCfg, config.SettingsConfig{})

	// No GitHub calls should be made for auto-dispatch anvils.
	if len(mock.CommentCalls) != 0 {
		t.Error("expected no GitHub calls for auto-dispatch anvil")
	}
}

func TestCheckClarificationReTriageNewAuthorReply(t *testing.T) {
	db := openTestDB(t)

	mock := &MockGitHubClient{
		OnListComments: func(_ context.Context, repo string, number int) ([]Comment, error) {
			return []Comment{
				{Author: "bot", Body: "clarification needed"},
				{Author: "alice", Body: "Here is more detail: the button is on the home screen"},
			}, nil
		},
	}

	wi := state.WicketIssue{
		Repo:         "owner/repo",
		IssueNumber:  55,
		Title:        "Needs clarification",
		Author:       "alice",
		State:        StateAskClarify,
		CommentCount: 1, // one comment already seen (the bot's)
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg: &config.Config{
			Settings: config.SettingsConfig{},
		},
		// Inject a fast mock so the test doesn't spawn a real AI subprocess.
		triageFunc: func(_ context.Context, _ Issue, _ []Comment, _ TriageConfig) TriageDecision {
			return TriageDecision{Action: ActionFlagHuman, Reason: "test mock"}
		},
	}

	settings := config.SettingsConfig{}
	anvilCfg := config.AnvilConfig{
		WicketRepos: []string{"owner/repo"},
	}

	m.checkClarificationReTriage(context.Background(), "test-anvil", anvilCfg, settings)

	// The comment count must be updated regardless of triage outcome.
	updated, err := db.GetWicketIssue("owner/repo", 55)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.CommentCount != 2 {
		t.Errorf("expected comment_count=2, got %d", updated.CommentCount)
	}
}

func TestCheckClarificationReTriageNoNewAuthorReply(t *testing.T) {
	db := openTestDB(t)

	mock := &MockGitHubClient{
		OnListComments: func(_ context.Context, repo string, number int) ([]Comment, error) {
			return []Comment{
				{Author: "bot", Body: "clarification needed"},
			}, nil
		},
	}

	wi := state.WicketIssue{
		Repo:         "owner/repo",
		IssueNumber:  56,
		Title:        "Awaiting clarification",
		Author:       "alice",
		State:        StateAskClarify,
		CommentCount: 1, // same count as current
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	m.checkClarificationReTriage(context.Background(), "test-anvil", config.AnvilConfig{
		WicketRepos: []string{"owner/repo"},
	}, config.SettingsConfig{})

	// State should remain unchanged.
	updated, err := db.GetWicketIssue("owner/repo", 56)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.State != StateAskClarify {
		t.Errorf("expected state unchanged %q, got %q", StateAskClarify, updated.State)
	}
}
