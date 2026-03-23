package wicket

import (
	"testing"
)

func TestParseTriageDecision_CreateBead(t *testing.T) {
	input := `{"action":"create_bead","title":"Fix login timeout","description":"The login session expires too quickly.","type":"bug","priority":1,"reasoning":"Clear actionable bug"}`

	d, err := parseTriageDecision(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != ActionCreateBead {
		t.Errorf("expected action %q, got %q", ActionCreateBead, d.Action)
	}
	if d.Title != "Fix login timeout" {
		t.Errorf("expected title %q, got %q", "Fix login timeout", d.Title)
	}
	if d.Priority != 1 {
		t.Errorf("expected priority 1, got %d", d.Priority)
	}
}

func TestParseTriageDecision_AskClarify(t *testing.T) {
	input := `{"action":"ask_clarify","question":"What is the expected behavior?","reasoning":"Missing details"}`

	d, err := parseTriageDecision(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != ActionAskClarify {
		t.Errorf("expected action %q, got %q", ActionAskClarify, d.Action)
	}
	if d.Question != "What is the expected behavior?" {
		t.Errorf("unexpected question: %q", d.Question)
	}
}

func TestParseTriageDecision_FlagHuman(t *testing.T) {
	input := `{"action":"flag_human","reasoning":"Requires strategic decision"}`

	d, err := parseTriageDecision(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != ActionFlagHuman {
		t.Errorf("expected action %q, got %q", ActionFlagHuman, d.Action)
	}
}

func TestParseTriageDecision_WrappedInMarkdown(t *testing.T) {
	// AI sometimes wraps JSON in markdown code fences or adds prose.
	input := "Here is my decision:\n\n```json\n{\"action\":\"flag_human\",\"reasoning\":\"Discussion thread\"}\n```\n\nLet me know if you need anything else."

	d, err := parseTriageDecision(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Action != ActionFlagHuman {
		t.Errorf("expected action %q, got %q", ActionFlagHuman, d.Action)
	}
}

func TestParseTriageDecision_UnknownAction(t *testing.T) {
	input := `{"action":"do_nothing","reasoning":"whatever"}`

	_, err := parseTriageDecision(input)
	if err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
}

func TestParseTriageDecision_EmptyOutput(t *testing.T) {
	_, err := parseTriageDecision("")
	if err == nil {
		t.Fatal("expected error for empty output, got nil")
	}
}

func TestParseTriageDecision_MalformedJSON(t *testing.T) {
	_, err := parseTriageDecision("this is not json at all")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestParseTriageDecision_CreateBeadMissingTitle(t *testing.T) {
	input := `{"action":"create_bead","reasoning":"ok"}`

	_, err := parseTriageDecision(input)
	if err == nil {
		t.Fatal("expected error for create_bead without title, got nil")
	}
}

func TestParseTriageDecision_PriorityBoundsEnforced(t *testing.T) {
	// Priority outside 0-4 should be clamped to 2.
	input := `{"action":"create_bead","title":"Test","priority":99,"reasoning":"ok"}`

	d, err := parseTriageDecision(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Priority != 2 {
		t.Errorf("expected priority 2 (clamped), got %d", d.Priority)
	}
}

func TestBuildTriagePrompt_ContainsIssueFields(t *testing.T) {
	issue := &Issue{
		Number: 42,
		Title:  "Login is broken",
		Body:   "Steps to reproduce...",
		Author: IssueAuthor{Login: "alice"},
	}

	prompt := buildTriagePrompt("owner/repo", issue, "some context")

	checks := []string{
		"owner/repo",
		"42",
		"Login is broken",
		"Steps to reproduce",
		"alice",
	}
	for _, want := range checks {
		if !contains(prompt, want) {
			t.Errorf("expected prompt to contain %q", want)
		}
	}
}

func TestBuildTriagePrompt_WithComments(t *testing.T) {
	issue := &Issue{
		Number: 1,
		Title:  "Test",
		Body:   "Body",
		Author: IssueAuthor{Login: "bob"},
		Comments: []IssueComment{
			{Author: IssueAuthor{Login: "alice"}, Body: "Can you reproduce this?"},
		},
	}

	prompt := buildTriagePrompt("org/repo", issue, "")
	if !contains(prompt, "alice") {
		t.Error("expected prompt to contain commenter login")
	}
	if !contains(prompt, "Can you reproduce this?") {
		t.Error("expected prompt to contain comment body")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && (s == sub || len(s) > 0 && containsAt(s, sub))
}

func containsAt(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
