package wicket

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// --- new action tests ---

func TestParseTriageDecision_Duplicate(t *testing.T) {
	output := `{"action": "duplicate", "reason": "matches Forge-abc1", "duplicate_id": "Forge-abc1"}`
	dec, ok := parseTriageDecision(output)
	if !ok {
		t.Fatal("expected parse success")
	}
	if dec.Action != ActionDuplicate {
		t.Errorf("action: got %q, want duplicate", dec.Action)
	}
	if dec.DuplicateID != "Forge-abc1" {
		t.Errorf("duplicate_id: got %q, want Forge-abc1", dec.DuplicateID)
	}
}

func TestParseTriageDecision_AlreadyFixed(t *testing.T) {
	output := `{"action": "already_fixed", "reason": "resolved in PR", "reference_pr": "https://github.com/org/repo/pull/42"}`
	dec, ok := parseTriageDecision(output)
	if !ok {
		t.Fatal("expected parse success")
	}
	if dec.Action != ActionAlreadyFixed {
		t.Errorf("action: got %q, want already_fixed", dec.Action)
	}
	if dec.ReferencePR != "https://github.com/org/repo/pull/42" {
		t.Errorf("reference_pr: got %q", dec.ReferencePR)
	}
}

func TestParseTriageDecision_OutOfScope(t *testing.T) {
	output := `{"action": "out_of_scope", "reason": "this project does not handle UI concerns"}`
	dec, ok := parseTriageDecision(output)
	if !ok {
		t.Fatal("expected parse success")
	}
	if dec.Action != ActionOutOfScope {
		t.Errorf("action: got %q, want out_of_scope", dec.Action)
	}
	if dec.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestRunTriage_OpenBeadsDuplicate(t *testing.T) {
	openBeads := []BeadSummary{
		{ID: "Forge-dup1", Title: "Support dark mode", Description: "Implement dark mode toggle", Status: "open"},
	}
	cfg := TriageConfig{
		runner: func(_ context.Context, prompt string) (string, error) {
			// Verify the open beads context was injected into the prompt.
			if !strings.Contains(prompt, "Forge-dup1") {
				t.Error("prompt does not contain open bead ID")
			}
			if !strings.Contains(prompt, "Support dark mode") {
				t.Error("prompt does not contain open bead title")
			}
			return `{"action": "duplicate", "reason": "already tracked", "duplicate_id": "Forge-dup1"}`, nil
		},
		beadLister: func(_ context.Context, status string, _ int) []BeadSummary {
			if strings.Contains(status, "open") {
				return openBeads
			}
			return nil
		},
	}
	issue := Issue{Number: 10, Repo: "org/repo", Title: "Add dark mode support"}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionDuplicate {
		t.Errorf("got %q, want duplicate", dec.Action)
	}
	if dec.DuplicateID != "Forge-dup1" {
		t.Errorf("duplicate_id: got %q, want Forge-dup1", dec.DuplicateID)
	}
}

func TestRunTriage_ClosedBeadsAlreadyFixed(t *testing.T) {
	closedBeads := []BeadSummary{
		{ID: "Forge-old1", Title: "Fix crash on startup", Description: "App crashes at launch", Status: "closed"},
	}
	cfg := TriageConfig{
		runner: func(_ context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "Forge-old1") {
				t.Error("prompt does not contain closed bead ID")
			}
			return `{"action": "already_fixed", "reason": "resolved in Forge-old1", "reference_pr": "Forge-old1"}`, nil
		},
		beadLister: func(_ context.Context, status string, _ int) []BeadSummary {
			if strings.Contains(status, "closed") {
				return closedBeads
			}
			return nil
		},
	}
	issue := Issue{Number: 11, Repo: "org/repo", Title: "Crash on startup"}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionAlreadyFixed {
		t.Errorf("got %q, want already_fixed", dec.Action)
	}
	if dec.ReferencePR != "Forge-old1" {
		t.Errorf("reference_pr: got %q", dec.ReferencePR)
	}
}

func TestRunTriage_CustomTriagePromptOutOfScope(t *testing.T) {
	cfg := TriageConfig{
		ExtraPrompt: "This project only handles backend API changes. Reject all UI feature requests.",
		runner: func(_ context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "This project only handles backend API changes") {
				t.Error("prompt does not contain custom triage prompt")
			}
			return `{"action": "out_of_scope", "reason": "UI concerns are not handled by this project"}`, nil
		},
		beadLister: func(_ context.Context, _ string, _ int) []BeadSummary {
			return nil
		},
	}
	issue := Issue{Number: 12, Repo: "org/repo", Title: "Add button styling"}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionOutOfScope {
		t.Errorf("got %q, want out_of_scope", dec.Action)
	}
}

func TestBuildTriagePromptWithBeads_InjectsBeadContext(t *testing.T) {
	openBeads := []BeadSummary{
		{ID: "Forge-x1", Title: "Feature X", Description: "Implement feature X"},
	}
	closedBeads := []BeadSummary{
		{ID: "Forge-y1", Title: "Bug Y", Description: "Fix bug Y"},
	}
	issue := Issue{Number: 1, Repo: "r/r", Title: "Test"}
	prompt := buildTriagePromptWithBeads(issue, nil, "", openBeads, closedBeads)

	for _, want := range []string{
		"Forge-x1", "Feature X",
		"Forge-y1", "Bug Y",
		"duplicate", "already_fixed", "out_of_scope",
		"existing_work",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestFormatBeadSummaries_Empty(t *testing.T) {
	got := formatBeadSummaries(nil)
	if got != "(none)" {
		t.Errorf("got %q, want (none)", got)
	}
}

func TestFormatBeadSummaries_TruncatesDescription(t *testing.T) {
	longDesc := strings.Repeat("x", 200)
	beads := []BeadSummary{{ID: "Forge-z1", Title: "Long one", Description: longDesc}}
	got := formatBeadSummaries(beads)
	if !strings.Contains(got, "Forge-z1") {
		t.Error("missing bead ID")
	}
	// Description should be truncated at 120 chars + ellipsis
	if strings.Contains(got, longDesc) {
		t.Error("description was not truncated")
	}
}

func TestParseTriageDecision_DuplicateMissingID(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "duplicate", "reason": "matches existing"}`)
	if ok {
		t.Fatal("expected parse failure when duplicate_id is empty")
	}
}

func TestParseTriageDecision_AlreadyFixedMissingRef(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "already_fixed", "reason": "resolved previously"}`)
	if ok {
		t.Fatal("expected parse failure when reference_pr is empty")
	}
}

func TestParseTriageDecision_OutOfScopeMissingReason(t *testing.T) {
	_, ok := parseTriageDecision(`{"action": "out_of_scope"}`)
	if ok {
		t.Fatal("expected parse failure when reason is empty for out_of_scope")
	}
}

// --- end new action tests ---

// noopBeadLister is a beadLister that returns nil without spawning any
// subprocess. Use in tests that provide a runner mock and don't need real
// bead context injection.
var noopBeadLister = func(_ context.Context, _ string, _ int) []BeadSummary {
	return nil
}

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
		beadLister: noopBeadLister,
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
		beadLister: noopBeadLister,
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
		beadLister: noopBeadLister,
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
		beadLister: noopBeadLister,
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
		beadLister: noopBeadLister,
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
