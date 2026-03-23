package wicket

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestBuildTriagePrompt_ContainsIssueDetails(t *testing.T) {
	issue := Issue{
		Number: 42,
		Repo:   "owner/repo",
		Title:  "Support dark mode",
		Body:   "It would be great if the UI had a dark mode option.",
		Author: "alice",
		Labels: []string{"enhancement", "ui"},
	}
	prompt := buildTriagePrompt(issue, "")

	for _, want := range []string{
		"Support dark mode",
		"owner/repo",
		"42",
		"alice",
		"enhancement",
		"dark mode",
		"create_bead",
		"ask_clarify",
		"flag_human",
		"<issue>",
		"<title>",
		"<description>",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildTriagePrompt_ExtraPromptAppended(t *testing.T) {
	issue := Issue{Number: 1, Repo: "r/r", Title: "Test"}
	prompt := buildTriagePrompt(issue, "Be conservative about feature requests.")
	if !strings.Contains(prompt, "Be conservative about feature requests.") {
		t.Error("extra prompt not included")
	}
}

func TestBuildTriagePrompt_EmptyBodyPlaceholder(t *testing.T) {
	issue := Issue{Number: 1, Repo: "r/r", Title: "Test", Body: ""}
	prompt := buildTriagePrompt(issue, "")
	if !strings.Contains(prompt, "no description provided") {
		t.Error("expected placeholder for empty body")
	}
}

func TestParseTriageDecision_FencedJSONBlock(t *testing.T) {
	output := "Analysis:\n```json\n{\"action\": \"create_bead\", \"reason\": \"Clear task\", \"bead_title\": \"Add dark mode\", \"bead_description\": \"Implement dark mode toggle.\"}\n```\n"
	dec, ok := parseTriageDecision(output)
	if !ok {
		t.Fatal("expected parse success")
	}
	if dec.Action != ActionCreateBead {
		t.Errorf("action: got %q, want create_bead", dec.Action)
	}
	if dec.Reason != "Clear task" {
		t.Errorf("reason: %q", dec.Reason)
	}
	if dec.BeadTitle != "Add dark mode" {
		t.Errorf("bead_title: %q", dec.BeadTitle)
	}
	if dec.BeadDescription != "Implement dark mode toggle." {
		t.Errorf("bead_description: %q", dec.BeadDescription)
	}
}

func TestParseTriageDecision_RawJSON(t *testing.T) {
	output := `Here is my decision: {"action": "ask_clarify", "reason": "Need more details"} end.`
	dec, ok := parseTriageDecision(output)
	if !ok {
		t.Fatal("expected parse success")
	}
	if dec.Action != ActionAskClarify {
		t.Errorf("action: got %q, want ask_clarify", dec.Action)
	}
	if dec.Reason != "Need more details" {
		t.Errorf("reason: %q", dec.Reason)
	}
}

func TestParseTriageDecision_FlagHuman(t *testing.T) {
	output := `{"action": "flag_human", "reason": "Too complex"}`
	dec, ok := parseTriageDecision(output)
	if !ok {
		t.Fatal("expected parse success")
	}
	if dec.Action != ActionFlagHuman {
		t.Errorf("action: got %q, want flag_human", dec.Action)
	}
}

func TestParseTriageDecision_MalformedJSON(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "create_bead" this is broken`)
	if ok {
		t.Fatal("expected parse failure for malformed JSON")
	}
}

func TestParseTriageDecision_UnknownAction(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "do_something_else", "reason": "test"}`)
	if ok {
		t.Fatal("expected parse failure for unknown action")
	}
}

func TestParseTriageDecision_NoJSON(t *testing.T) {
	_, ok := parseTriageDecision("I'm sorry, I cannot make a decision about this issue.")
	if ok {
		t.Fatal("expected parse failure for output with no JSON")
	}
}

func TestParseTriageDecision_JSONWithoutActionKey(t *testing.T) {
	_, ok := parseTriageDecision(`{"verdict": "approve", "reason": "looks fine"}`)
	if ok {
		t.Fatal("expected parse failure when action key is absent")
	}
}

func TestRunTriage_ValidResponse(t *testing.T) {
	cfg := TriageConfig{
		runner: func(_ context.Context, _ string) (string, error) {
			return `{"action": "ask_clarify", "reason": "Need more details"}`, nil
		},
	}
	dec := RunTriage(context.Background(), Issue{Number: 1, Repo: "r/r", Title: "Test"}, cfg)
	if dec.Action != ActionAskClarify {
		t.Errorf("got %q, want ask_clarify", dec.Action)
	}
	if dec.Reason != "Need more details" {
		t.Errorf("reason: %q", dec.Reason)
	}
}

func TestRunTriage_MalformedJSON_RetriesAndFallsBack(t *testing.T) {
	calls := 0
	cfg := TriageConfig{
		runner: func(_ context.Context, _ string) (string, error) {
			calls++
			return "this is not json at all", nil
		},
	}
	dec := RunTriage(context.Background(), Issue{Number: 1, Repo: "r/r", Title: "Test"}, cfg)
	if dec.Action != ActionFlagHuman {
		t.Errorf("got %q, want flag_human", dec.Action)
	}
	if calls != 2 {
		t.Errorf("expected 2 runner calls (initial + one retry), got %d", calls)
	}
}

func TestRunTriage_RunnerError_FallsBackToFlagHuman(t *testing.T) {
	calls := 0
	cfg := TriageConfig{
		runner: func(_ context.Context, _ string) (string, error) {
			calls++
			return "", fmt.Errorf("connection refused")
		},
	}
	dec := RunTriage(context.Background(), Issue{Number: 1, Repo: "r/r", Title: "Test"}, cfg)
	if dec.Action != ActionFlagHuman {
		t.Errorf("got %q, want flag_human", dec.Action)
	}
	// Runner errors must NOT be retried.
	if calls != 1 {
		t.Errorf("expected 1 runner call on runner error (no retry), got %d", calls)
	}
}

func TestParseTriageDecision_CreateBeadMissingTitle(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "create_bead", "reason": "ok", "bead_description": "do the thing"}`)
	if ok {
		t.Fatal("expected parse failure when bead_title is empty")
	}
}

func TestParseTriageDecision_CreateBeadMissingDescription(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "create_bead", "reason": "ok", "bead_title": "My title"}`)
	if ok {
		t.Fatal("expected parse failure when bead_description is empty")
	}
}

func TestRunTriage_FirstCallFailsSecondSucceeds(t *testing.T) {
	calls := 0
	cfg := TriageConfig{
		runner: func(_ context.Context, _ string) (string, error) {
			calls++
			if calls == 1 {
				return "garbage output", nil
			}
			return `{"action": "create_bead", "reason": "ok", "bead_title": "T", "bead_description": "D"}`, nil
		},
	}
	dec := RunTriage(context.Background(), Issue{Number: 1, Repo: "r/r", Title: "Test"}, cfg)
	if dec.Action != ActionCreateBead {
		t.Errorf("got %q, want create_bead", dec.Action)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestRunTriage_PassesPromptToRunner(t *testing.T) {
	var capturedPrompt string
	cfg := TriageConfig{
		runner: func(_ context.Context, prompt string) (string, error) {
			capturedPrompt = prompt
			return `{"action": "flag_human", "reason": "test"}`, nil
		},
	}
	issue := Issue{Number: 7, Repo: "org/proj", Title: "Crash on startup", Body: "App crashes."}
	RunTriage(context.Background(), issue, cfg)

	if !strings.Contains(capturedPrompt, "Crash on startup") {
		t.Error("prompt does not contain issue title")
	}
	if !strings.Contains(capturedPrompt, "org/proj") {
		t.Error("prompt does not contain repo")
	}
}
