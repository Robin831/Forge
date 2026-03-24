package wicket

import (
	"context"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

func TestHandlePRCreatedLinksIssue(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 11,
		Title:       "PR link test",
		Author:      "alice",
		State:       StateBeadCreated,
		BeadID:      "bead-pr-link",
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	m.HandlePRCreated(context.Background(), "bead-pr-link", "https://github.com/owner/repo/pull/5", 5)

	// Should post a comment on the issue.
	if len(mock.CommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(mock.CommentCalls))
	}
	if mock.CommentCalls[0].Number != 11 {
		t.Errorf("comment posted on wrong issue number: %d", mock.CommentCalls[0].Number)
	}

	// State should be updated to pr_created.
	updated, err := db.GetWicketIssue("owner/repo", 11)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.State != StatePRCreated {
		t.Errorf("expected state %q, got %q", StatePRCreated, updated.State)
	}
	if updated.PRNumber != 5 {
		t.Errorf("expected pr_number=5, got %d", updated.PRNumber)
	}
	if updated.PRUrl != "https://github.com/owner/repo/pull/5" {
		t.Errorf("unexpected pr_url: %s", updated.PRUrl)
	}
}

func TestHandlePRCreatedUnknownBead(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	// No wicket issue exists for this bead — should be a no-op.
	m.HandlePRCreated(context.Background(), "unknown-bead", "https://github.com/owner/repo/pull/99", 99)

	if len(mock.CommentCalls) != 0 {
		t.Errorf("expected no comments for unknown bead, got %d", len(mock.CommentCalls))
	}
}

func TestHandlePRMergedClosesIssue(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 22,
		Title:       "Merge close test",
		Author:      "bob",
		State:       StatePRCreated,
		BeadID:      "bead-merge-close",
		PRNumber:    7,
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	m.HandlePRMerged(context.Background(), "bead-merge-close", "https://github.com/owner/repo/pull/7", "main", 7)

	// Should post a comment.
	if len(mock.CommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(mock.CommentCalls))
	}
	// Should close the issue.
	if len(mock.CloseCalls) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(mock.CloseCalls))
	}
	if mock.CloseCalls[0].Number != 22 {
		t.Errorf("closed wrong issue: %d", mock.CloseCalls[0].Number)
	}

	// State should be merged.
	updated, err := db.GetWicketIssue("owner/repo", 22)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.State != StateMerged {
		t.Errorf("expected state %q, got %q", StateMerged, updated.State)
	}
}

func TestHandlePRMergedUnknownBead(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	m.HandlePRMerged(context.Background(), "no-such-bead", "https://github.com/owner/repo/pull/1", "main", 1)

	if len(mock.CloseCalls) != 0 {
		t.Errorf("expected no close for unknown bead, got %d", len(mock.CloseCalls))
	}
}

func TestCheckStaleIssuesMarksStale(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	// Insert an ask_clarify issue that is old enough to be stale.
	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 33,
		Title:       "Stale issue",
		Author:      "carol",
		State:       StateAskClarify,
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	// Manually backdate the updated_at so it appears stale.
	// We do this by running a direct SQL update — acceptable in tests.
	if _, err := db.Conn().Exec(
		`UPDATE wicket_issues SET updated_at=? WHERE repo=? AND issue_number=?`,
		time.Now().UTC().AddDate(0, 0, -20).Format("2006-01-02T15:04:05.000000000Z07:00"),
		"owner/repo", 33,
	); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	settings := config.SettingsConfig{WicketStaleDays: 14}
	m.checkStaleIssues(context.Background(), settings)

	// Should post a stale warning.
	if len(mock.CommentCalls) != 1 {
		t.Fatalf("expected 1 stale comment, got %d", len(mock.CommentCalls))
	}

	updated, err := db.GetWicketIssue("owner/repo", 33)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.State != StateStale {
		t.Errorf("expected state %q, got %q", StateStale, updated.State)
	}
}

func TestCheckStaleIssuesSkipsRecent(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 44,
		Title:       "Recent issue",
		Author:      "dave",
		State:       StateAskClarify,
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	settings := config.SettingsConfig{WicketStaleDays: 14}
	m.checkStaleIssues(context.Background(), settings)

	// Recent issue should not be marked stale.
	if len(mock.CommentCalls) != 0 {
		t.Errorf("expected no comments for recent issue, got %d", len(mock.CommentCalls))
	}
}

func TestCheckStaleClosedAutoCloses(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	// Stale issue that has been stale for 8+ days.
	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 55,
		Title:       "Long-stale issue",
		Author:      "eve",
		State:       StateStale,
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	// Backdate to 8 days ago (past the 7-day auto-close window).
	if _, err := db.Conn().Exec(
		`UPDATE wicket_issues SET updated_at=? WHERE repo=? AND issue_number=?`,
		time.Now().UTC().AddDate(0, 0, -8).Format("2006-01-02T15:04:05.000000000Z07:00"),
		"owner/repo", 55,
	); err != nil {
		t.Fatalf("backdate updated_at: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	m.checkStaleClosed(context.Background())

	if len(mock.CloseCalls) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(mock.CloseCalls))
	}
	if mock.CloseCalls[0].Number != 55 {
		t.Errorf("closed wrong issue: %d", mock.CloseCalls[0].Number)
	}

	updated, err := db.GetWicketIssue("owner/repo", 55)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if updated.State != StateClosed {
		t.Errorf("expected state %q, got %q", StateClosed, updated.State)
	}
}

func TestCheckStaleClosedSkipsRecentStale(t *testing.T) {
	db := openTestDB(t)
	mock := &MockGitHubClient{}

	wi := state.WicketIssue{
		Repo:        "owner/repo",
		IssueNumber: 66,
		Title:       "Freshly stale",
		Author:      "frank",
		State:       StateStale,
	}
	if err := db.InsertWicketIssue(wi); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	m := &Monitor{
		ghClient: mock,
		db:       db,
		cfg:      &config.Config{},
	}

	m.checkStaleClosed(context.Background())

	// Freshly stale issue (updated just now) should not be closed yet.
	if len(mock.CloseCalls) != 0 {
		t.Errorf("expected no close for recently stale issue, got %d", len(mock.CloseCalls))
	}
}
