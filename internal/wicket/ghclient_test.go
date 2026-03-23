package wicket

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// recordedListIssues is a representative JSON snippet as returned by
// `gh issue list --json number,title,body,state,createdAt,author,labels`.
const recordedListIssues = `[
  {
    "number": 42,
    "title": "Support dark mode",
    "body": "It would be great if the UI had a dark mode option.",
    "state": "OPEN",
    "createdAt": "2026-01-15T10:00:00Z",
    "author": {"login": "alice"},
    "labels": [{"name": "enhancement"}, {"name": "ui"}]
  },
  {
    "number": 43,
    "title": "Fix login crash",
    "body": "App crashes when the password field is empty.",
    "state": "OPEN",
    "createdAt": "2026-01-16T08:30:00Z",
    "author": {"login": "bob"},
    "labels": [{"name": "bug"}]
  }
]`

// recordedGetIssue is a representative JSON snippet as returned by
// `gh issue view <number> --json number,title,body,state,createdAt,author,labels`.
const recordedGetIssue = `{
  "number": 42,
  "title": "Support dark mode",
  "body": "It would be great if the UI had a dark mode option.",
  "state": "OPEN",
  "createdAt": "2026-01-15T10:00:00Z",
  "author": {"login": "alice"},
  "labels": [{"name": "enhancement"}, {"name": "ui"}]
}`

func TestParseListIssues(t *testing.T) {
	var raw []ghIssue
	if err := json.Unmarshal([]byte(recordedListIssues), &raw); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(raw))
	}

	issues := make([]Issue, len(raw))
	for i, r := range raw {
		var err error
		issues[i], err = toIssue(r, "owner/repo")
		if err != nil {
			t.Fatalf("toIssue: %v", err)
		}
	}

	first := issues[0]
	if first.Number != 42 {
		t.Errorf("Number: want 42, got %d", first.Number)
	}
	if first.Title != "Support dark mode" {
		t.Errorf("Title: want %q, got %q", "Support dark mode", first.Title)
	}
	if first.Author != "alice" {
		t.Errorf("Author: want %q, got %q", "alice", first.Author)
	}
	if first.Repo != "owner/repo" {
		t.Errorf("Repo: want %q, got %q", "owner/repo", first.Repo)
	}
	if len(first.Labels) != 2 || first.Labels[0] != "enhancement" || first.Labels[1] != "ui" {
		t.Errorf("Labels: want [enhancement ui], got %v", first.Labels)
	}
	wantTime, err := time.Parse(time.RFC3339, "2026-01-15T10:00:00Z")
	if err != nil {
		t.Fatalf("parse wantTime: %v", err)
	}
	if !first.CreatedAt.Equal(wantTime) {
		t.Errorf("CreatedAt: want %v, got %v", wantTime, first.CreatedAt)
	}

	second := issues[1]
	if second.Number != 43 {
		t.Errorf("second Number: want 43, got %d", second.Number)
	}
	if len(second.Labels) != 1 || second.Labels[0] != "bug" {
		t.Errorf("second Labels: want [bug], got %v", second.Labels)
	}
}

func TestParseGetIssue(t *testing.T) {
	var raw ghIssue
	if err := json.Unmarshal([]byte(recordedGetIssue), &raw); err != nil {
		t.Fatalf("unmarshal single: %v", err)
	}

	issue, err := toIssue(raw, "owner/repo")
	if err != nil {
		t.Fatalf("toIssue: %v", err)
	}
	if issue.Number != 42 {
		t.Errorf("Number: want 42, got %d", issue.Number)
	}
	if issue.Body != "It would be great if the UI had a dark mode option." {
		t.Errorf("Body mismatch: %q", issue.Body)
	}
}

func TestMockGitHubClient_ListIssues(t *testing.T) {
	want := []Issue{{Number: 1, Repo: "r/r", Title: "Test"}}
	m := &MockGitHubClient{
		OnListIssues: func(_ context.Context, repo string, labels []string) ([]Issue, error) {
			return want, nil
		},
	}
	got, err := m.ListIssues(context.Background(), "r/r", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Number != 1 {
		t.Errorf("unexpected result: %v", got)
	}
}

func TestMockGitHubClient_CommentOnIssue_RecordsCall(t *testing.T) {
	m := &MockGitHubClient{}
	if err := m.CommentOnIssue(context.Background(), "owner/repo", 7, "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.CommentCalls) != 1 {
		t.Fatalf("expected 1 comment call, got %d", len(m.CommentCalls))
	}
	c := m.CommentCalls[0]
	if c.Repo != "owner/repo" || c.Number != 7 || c.Body != "hello" {
		t.Errorf("wrong call recorded: %+v", c)
	}
}

func TestMockGitHubClient_AddLabels_RecordsCall(t *testing.T) {
	m := &MockGitHubClient{}
	if err := m.AddLabels(context.Background(), "owner/repo", 5, []string{"triage", "bug"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.AddLabelCalls) != 1 {
		t.Fatalf("expected 1 add-label call, got %d", len(m.AddLabelCalls))
	}
	call := m.AddLabelCalls[0]
	if call.Number != 5 || len(call.Labels) != 2 {
		t.Errorf("wrong call recorded: %+v", call)
	}
}

func TestMockGitHubClient_RemoveLabel_RecordsCall(t *testing.T) {
	m := &MockGitHubClient{}
	if err := m.RemoveLabel(context.Background(), "owner/repo", 3, "stale"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.RemoveCalls) != 1 {
		t.Fatalf("expected 1 remove call, got %d", len(m.RemoveCalls))
	}
	if m.RemoveCalls[0].Label != "stale" {
		t.Errorf("wrong label recorded: %q", m.RemoveCalls[0].Label)
	}
}

func TestMockGitHubClient_CloseIssue_RecordsCall(t *testing.T) {
	m := &MockGitHubClient{}
	if err := m.CloseIssue(context.Background(), "owner/repo", 99); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.CloseCalls) != 1 || m.CloseCalls[0].Repo != "owner/repo" || m.CloseCalls[0].Number != 99 {
		t.Errorf("wrong close call recorded: %v", m.CloseCalls)
	}
}

func TestMockGitHubClient_GetIssue_NilDefault(t *testing.T) {
	m := &MockGitHubClient{}
	issue, err := m.GetIssue(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issue != nil {
		t.Errorf("expected nil issue from empty mock, got %+v", issue)
	}
}
