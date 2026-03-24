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
	prompt := buildTriagePromptWithBeads(issue, nil, "", openBeads, closedBeads, "")

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

func TestExtractSourceURL(t *testing.T) {
	tests := []struct {
		desc string
		want string
	}{
		{
			desc: "Implement dark mode.\n\nSource: https://github.com/org/repo/issues/42",
			want: "https://github.com/org/repo/issues/42",
		},
		{
			desc: "No source URL here.",
			want: "",
		},
		{
			desc: "Source: https://github.com/org/repo/issues/1\nExtra line",
			want: "https://github.com/org/repo/issues/1",
		},
		{
			desc: "",
			want: "",
		},
	}
	for _, tc := range tests {
		got := extractSourceURL(tc.desc)
		if got != tc.want {
			t.Errorf("extractSourceURL(%q) = %q; want %q", tc.desc, got, tc.want)
		}
	}
}

func TestFindDuplicateBySourceURL_CrossAnvilMatch(t *testing.T) {
	// Simulate two anvils: "anvil-a" and "anvil-b".
	// The issue's source URL matches a bead in "anvil-b".
	issue := Issue{Number: 99, Repo: "org/other-repo"}
	targetURL := issueURL(issue.Repo, issue.Number)

	beadsByAnvil := map[string][]BeadSummary{
		"/anvil-a": {
			{ID: "Forge-aa1", Title: "Unrelated", Description: "Some work.\n\nSource: https://github.com/org/other-repo/issues/1"},
		},
		"/anvil-b": {
			{ID: "Forge-bb2", Title: "Dark mode", Description: "Implement dark mode.\n\nSource: " + targetURL},
		},
	}

	lister := func(_ context.Context, anvilPath string) []BeadSummary {
		return beadsByAnvil[anvilPath]
	}

	beadID, ok := findDuplicateBySourceURL(context.Background(), issue, []string{"/anvil-a", "/anvil-b"}, lister)
	if !ok {
		t.Fatal("expected duplicate to be found")
	}
	if beadID != "Forge-bb2" {
		t.Errorf("beadID: got %q, want Forge-bb2", beadID)
	}
}

func TestFindDuplicateBySourceURL_NoMatch(t *testing.T) {
	issue := Issue{Number: 7, Repo: "org/repo"}
	lister := func(_ context.Context, anvilPath string) []BeadSummary {
		return []BeadSummary{
			{ID: "Forge-x1", Title: "Other", Description: "Some work.\n\nSource: https://github.com/org/repo/issues/999"},
		}
	}
	_, ok := findDuplicateBySourceURL(context.Background(), issue, []string{"/anvil-a"}, lister)
	if ok {
		t.Fatal("expected no duplicate")
	}
}

func TestRunTriage_CrossAnvilDuplicateBySourceURL(t *testing.T) {
	// Issue #55 in "org/repo-b" has already been triaged in a different anvil.
	issue := Issue{Number: 55, Repo: "org/repo-b", Title: "Add dark mode"}
	targetURL := issueURL(issue.Repo, issue.Number)

	runnerCalled := false
	cfg := TriageConfig{
		runner: func(_ context.Context, _ string) (string, error) {
			runnerCalled = true
			return `{"action": "create_bead", "reason": "ok", "bead_title": "T", "bead_description": "D"}`, nil
		},
		beadLister: noopBeadLister,
		AllAnvilPaths: []string{"/anvil-a", "/anvil-b"},
		crossAnvilLister: func(_ context.Context, anvilPath string) []BeadSummary {
			if anvilPath == "/anvil-b" {
				return []BeadSummary{
					{ID: "Forge-xdup", Title: "Add dark mode", Description: "Implement dark mode.\n\nSource: " + targetURL},
				}
			}
			return nil
		},
	}

	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionDuplicate {
		t.Errorf("action: got %q, want duplicate", dec.Action)
	}
	if dec.DuplicateID != "Forge-xdup" {
		t.Errorf("duplicate_id: got %q, want Forge-xdup", dec.DuplicateID)
	}
	if runnerCalled {
		t.Error("AI runner should not be called when cross-anvil duplicate is detected")
	}
}

func TestRunTriage_CrossAnvilNoMatch_ProceedsToAI(t *testing.T) {
	// When no cross-anvil match is found, the AI runner must still be called.
	issue := Issue{Number: 3, Repo: "org/repo", Title: "New feature"}
	runnerCalled := false
	cfg := TriageConfig{
		runner: func(_ context.Context, _ string) (string, error) {
			runnerCalled = true
			return `{"action": "create_bead", "reason": "ok", "bead_title": "T", "bead_description": "D"}`, nil
		},
		beadLister: noopBeadLister,
		AllAnvilPaths: []string{"/anvil-a"},
		crossAnvilLister: func(_ context.Context, _ string) []BeadSummary {
			return nil // no existing beads
		},
	}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionCreateBead {
		t.Errorf("action: got %q, want create_bead", dec.Action)
	}
	if !runnerCalled {
		t.Error("AI runner should be called when no cross-anvil duplicate is found")
	}
}

func TestRunTriage_MultiRepoPromptContext(t *testing.T) {
	// Verify that when MonitoredAnvilPaths is set, the triage prompt includes
	// beads from ALL mapped repo paths, not just the triggering anvil path.
	beadsByPath := map[string]map[string][]BeadSummary{
		"/anvil-a": {
			"open,in_progress": {{ID: "Forge-aa1", Title: "Feature A", Description: "Work for repo-a"}},
			"closed":           {{ID: "Forge-aa2", Title: "Fixed A", Description: "Old fix in repo-a"}},
		},
		"/anvil-b": {
			"open,in_progress": {{ID: "Forge-bb1", Title: "Feature B", Description: "Work for repo-b"}},
			"closed":           nil,
		},
	}

	cfg := TriageConfig{
		MonitoredAnvilPaths: []string{"/anvil-a", "/anvil-b"},
		monitoredAnvilLister: func(_ context.Context, anvilPath string, status string) []BeadSummary {
			if m, ok := beadsByPath[anvilPath]; ok {
				return m[status]
			}
			return nil
		},
		runner: func(_ context.Context, prompt string) (string, error) {
			// All beads from both paths must appear in the prompt.
			for _, want := range []string{"Forge-aa1", "Feature A", "Forge-bb1", "Feature B", "Forge-aa2", "Fixed A"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q (multi-repo context)", want)
				}
			}
			return `{"action": "create_bead", "reason": "ok", "bead_title": "T", "bead_description": "D"}`, nil
		},
	}
	issue := Issue{Number: 1, Repo: "org/repo-a", Title: "New request"}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionCreateBead {
		t.Errorf("action: got %q, want create_bead", dec.Action)
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

// ---- Anvil context for external repos (Forge-9lta) -------------------------

// TestRunTriage_ExternalRepoIncludesAnvilContext verifies that when an issue
// originates from an external repo (issue.Repo != cfg.AnvilRepo), the triage
// prompt includes the anvil's README and AGENTS.md content under an
// <anvil_context> section.
func TestRunTriage_ExternalRepoIncludesAnvilContext(t *testing.T) {
	var capturedPrompt string
	cfg := TriageConfig{
		runner: func(_ context.Context, prompt string) (string, error) {
			capturedPrompt = prompt
			return `{"action": "create_bead", "reason": "clear task", "bead_title": "Fix X", "bead_description": "Fix issue X"}`, nil
		},
		beadLister:  noopBeadLister,
		AnvilRepo:   "org/myapp",                                          // anvil's own repo
		AnvilPath:   "/fake/anvil",                                        // path used by loader
		anvilContextLoader: func(anvilPath string) (readme, agentsMD string) {
			// Stub filesystem reads so no real disk access is needed.
			if anvilPath == "/fake/anvil" {
				return "# MyApp README content", "# MyApp AGENTS instructions"
			}
			return "", ""
		},
	}
	// Issue is from a different repo — external.
	issue := Issue{Number: 1, Repo: "org/external-lib", Title: "Panic on nil pointer"}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionCreateBead {
		t.Fatalf("unexpected action %q", dec.Action)
	}
	if !strings.Contains(capturedPrompt, "<anvil_context>") {
		t.Error("prompt missing <anvil_context> section")
	}
	if !strings.Contains(capturedPrompt, "MyApp README content") {
		t.Error("prompt missing anvil README content")
	}
	if !strings.Contains(capturedPrompt, "MyApp AGENTS instructions") {
		t.Error("prompt missing anvil AGENTS.md content")
	}
	if !strings.Contains(capturedPrompt, "external repository") {
		t.Error("prompt missing 'external repository' phrase")
	}
}

// TestRunTriage_InternalRepoNoAnvilContext verifies that when an issue
// originates from the anvil's own repo, no <anvil_context> section is added.
func TestRunTriage_InternalRepoNoAnvilContext(t *testing.T) {
	var capturedPrompt string
	cfg := TriageConfig{
		runner: func(_ context.Context, prompt string) (string, error) {
			capturedPrompt = prompt
			return `{"action": "create_bead", "reason": "clear task", "bead_title": "Fix X", "bead_description": "Fix issue X"}`, nil
		},
		beadLister: noopBeadLister,
		AnvilRepo:  "org/myapp",
		AnvilPath:  "/fake/anvil",
		anvilContextLoader: func(anvilPath string) (readme, agentsMD string) {
			return "# MyApp README content", "# AGENTS instructions"
		},
	}
	// Issue is from the same repo as the anvil — internal.
	issue := Issue{Number: 2, Repo: "org/myapp", Title: "Improve performance"}
	dec := RunTriage(context.Background(), issue, cfg)
	if dec.Action != ActionCreateBead {
		t.Fatalf("unexpected action %q", dec.Action)
	}
	if strings.Contains(capturedPrompt, "<anvil_context>") {
		t.Error("prompt should NOT contain <anvil_context> for internal repo issues")
	}
	if strings.Contains(capturedPrompt, "MyApp README content") {
		t.Error("prompt should NOT contain anvil README for internal repo issues")
	}
}

// TestBuildAnvilContext_BothFiles verifies the formatted output includes both
// README and AGENTS.md when both are non-empty.
func TestBuildAnvilContext_BothFiles(t *testing.T) {
	ctx := buildAnvilContext("# README", "# AGENTS")
	if !strings.Contains(ctx, "<anvil_readme>") {
		t.Error("missing <anvil_readme>")
	}
	if !strings.Contains(ctx, "<anvil_agents_md>") {
		t.Error("missing <anvil_agents_md>")
	}
	if !strings.Contains(ctx, "# README") {
		t.Error("missing README content")
	}
	if !strings.Contains(ctx, "# AGENTS") {
		t.Error("missing AGENTS.md content")
	}
}

// TestBuildAnvilContext_Empty returns empty string when both inputs are empty.
func TestBuildAnvilContext_Empty(t *testing.T) {
	if got := buildAnvilContext("", ""); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}
