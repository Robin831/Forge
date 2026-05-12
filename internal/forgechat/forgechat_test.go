package forgechat

import (
	"strings"
	"testing"
)

func TestBuildPrompt_Drafting_IncludesGrillingPromptOnlyInGrillingStage(t *testing.T) {
	draft := BuildPrompt(TurnRequest{Stage: StageDrafting, Mode: ModeChat, UserText: "hi"})
	if strings.Contains(draft, "Interview me relentlessly") {
		t.Fatal("drafting prompt must not include the grilling skill prompt")
	}
	if !strings.Contains(draft, "DRAFTING stage") {
		t.Fatal("drafting prompt should announce the drafting stage")
	}
	grill := BuildPrompt(TurnRequest{Stage: StageGrilling, Mode: ModeGrill, UserText: "answer"})
	if !strings.Contains(grill, "Interview me relentlessly") {
		t.Fatal("grilling prompt must embed the user's grill-me skill prompt verbatim")
	}
	if !strings.Contains(grill, "GRILLING stage") {
		t.Fatal("grilling prompt should announce the grilling stage")
	}
}

func TestBuildPrompt_PlanModeAsksForMarkdownOnly(t *testing.T) {
	p := BuildPrompt(TurnRequest{Stage: StageDrafting, Mode: ModePlan})
	if !strings.Contains(p, "ONLY the plan as markdown") {
		t.Fatal("plan prompt should instruct markdown-only output")
	}
	if !strings.Contains(p, "files to create or modify") {
		t.Fatal("plan prompt should require files-to-modify list")
	}
}

func TestBuildPrompt_DraftingPromptsEnableReadOnlyTools(t *testing.T) {
	for _, mode := range []Mode{ModeChat, ModePlan, ModeGrill} {
		stage := StageDrafting
		if mode == ModeGrill {
			stage = StageGrilling
		}
		p := BuildPrompt(TurnRequest{Stage: stage, Mode: mode})
		if !strings.Contains(p, "Read") || !strings.Contains(p, "Grep") || !strings.Contains(p, "Glob") {
			t.Fatalf("mode %q prompt should advertise Read/Grep/Glob tools, got:\n%s", mode, p)
		}
		if strings.Contains(p, "Do NOT use any tools") {
			t.Fatalf("mode %q prompt must no longer forbid tool use", mode)
		}
	}
}

func TestBuildPrompt_AnvilContextRendersWhenProvided(t *testing.T) {
	target := &AnvilTarget{Name: "munin", Path: "/repos/munin"}
	p := BuildPrompt(TurnRequest{
		Stage: StageDrafting,
		Mode:  ModeChat,
		Anvil: target,
	})
	if !strings.Contains(p, "## Target anvil") {
		t.Fatalf("drafting prompt should include the anvil context block, got:\n%s", p)
	}
	if !strings.Contains(p, "`munin`") {
		t.Fatalf("drafting prompt should name the anvil, got:\n%s", p)
	}
	if !strings.Contains(p, "/repos/munin") {
		t.Fatalf("drafting prompt should embed the anvil absolute path, got:\n%s", p)
	}
}

func TestBuildPrompt_NilAnvilOmitsContextBlock(t *testing.T) {
	p := BuildPrompt(TurnRequest{Stage: StageDrafting, Mode: ModeChat})
	if strings.Contains(p, "## Target anvil") {
		t.Fatalf("nil Anvil should not render the target-anvil block, got:\n%s", p)
	}
}

func TestBuildPrompt_HalfResolvedAnvilOmitsContextBlock(t *testing.T) {
	// Path missing — emitting "name: munin / path: " is worse than nothing
	// because the AI will fabricate the missing piece.
	p := BuildPrompt(TurnRequest{
		Stage: StageDrafting,
		Mode:  ModeChat,
		Anvil: &AnvilTarget{Name: "munin"},
	})
	if strings.Contains(p, "## Target anvil") {
		t.Fatalf("half-resolved Anvil should be omitted, got:\n%s", p)
	}
}

func TestBuildPrompt_EmitModeStillUsesAnvilsListNotTargetAnvil(t *testing.T) {
	// Emission mode lists every registered anvil; the session-scoped Anvil is
	// not meaningful there (the AI may target a different anvil per bead).
	p := BuildPrompt(TurnRequest{
		Stage:  StageReady,
		Mode:   ModeEmit,
		Anvils: AnvilContext{"munin": "/repos/munin", "other": "/repos/other"},
		Anvil:  &AnvilTarget{Name: "munin", Path: "/repos/munin"},
	})
	if !strings.Contains(p, "## Registered anvils") {
		t.Fatalf("emit prompt should list registered anvils, got:\n%s", p)
	}
	if strings.Contains(p, "## Target anvil") {
		t.Fatalf("emit prompt must not include the single-anvil block (it picks per bead)")
	}
}

func TestParseGrillingResponse_FencedJSON(t *testing.T) {
	out := "Some preamble\n```json\n" +
		`{"questions":[{"prompt":"Sync or async?","options":[{"id":"sync","label":"Sync"},{"id":"async","label":"Async"}],"recommendation":"async","rationale":"Avoids blocking the daemon"}]}` +
		"\n```\nTrailing chatter."
	v, err := ParseGrillingResponse(out)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(v.Questions) != 1 {
		t.Fatalf("expected 1 question, got %d", len(v.Questions))
	}
	if v.Questions[0].Recommendation != "async" {
		t.Fatalf("recommendation lost: %q", v.Questions[0].Recommendation)
	}
	if len(v.Questions[0].Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(v.Questions[0].Options))
	}
}

func TestParseGrillingResponse_DoneSignal(t *testing.T) {
	v, err := ParseGrillingResponse(`{"done":true,"summary":"All resolved"}`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !v.Done {
		t.Fatal("expected done=true")
	}
	if v.Summary != "All resolved" {
		t.Fatalf("summary lost: %q", v.Summary)
	}
}

func TestVerdictToMessages_DoneEmitsStatusAndSignals(t *testing.T) {
	v := &grillingVerdict{Done: true, Summary: "wrap"}
	msgs, done := VerdictToMessages(v)
	if !done {
		t.Fatal("expected done=true")
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 status message, got %d", len(msgs))
	}
	if msgs[0].Kind != "status" {
		t.Fatalf("expected status kind, got %q", msgs[0].Kind)
	}
}

func TestVerdictToMessages_DoneWithQuestionsPrioritizesQuestions(t *testing.T) {
	v := &grillingVerdict{
		Done:    true,
		Summary: "almost there",
		Questions: []struct {
			Prompt         string           `json:"prompt"`
			Options        []QuestionOption `json:"options"`
			Recommendation string           `json:"recommendation,omitempty"`
			Rationale      string           `json:"rationale,omitempty"`
		}{
			{Prompt: "One last thing?", Options: []QuestionOption{{ID: "y", Label: "Yes"}}},
		},
	}
	msgs, done := VerdictToMessages(v)
	if done {
		t.Fatal("done must be false when questions are still pending — questions take priority")
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (status note + question), got %d", len(msgs))
	}
	if msgs[0].Kind != "status" {
		t.Fatalf("first message should be status note about the contradiction, got %q", msgs[0].Kind)
	}
	if msgs[1].Kind != "question" {
		t.Fatalf("second message should be the question, got %q", msgs[1].Kind)
	}
}

func TestVerdictToMessages_QuestionsEmitJSONMetadata(t *testing.T) {
	v := &grillingVerdict{
		Questions: []struct {
			Prompt         string           `json:"prompt"`
			Options        []QuestionOption `json:"options"`
			Recommendation string           `json:"recommendation,omitempty"`
			Rationale      string           `json:"rationale,omitempty"`
		}{
			{
				Prompt:         "Q1",
				Options:        []QuestionOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
				Recommendation: "a",
				Rationale:      "because",
			},
		},
	}
	msgs, done := VerdictToMessages(v)
	if done {
		t.Fatal("expected done=false")
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 question message, got %d", len(msgs))
	}
	if msgs[0].Kind != "question" {
		t.Fatalf("expected question kind, got %q", msgs[0].Kind)
	}
	if !strings.Contains(msgs[0].Metadata, `"id":"a"`) {
		t.Fatalf("metadata should embed option ids: %q", msgs[0].Metadata)
	}
	if !strings.Contains(msgs[0].Metadata, `"recommendation":"a"`) {
		t.Fatalf("metadata should embed recommendation: %q", msgs[0].Metadata)
	}
}

func TestInterpretResponse_DraftingChat(t *testing.T) {
	resp, err := interpretResponse(TurnRequest{Stage: StageDrafting, Mode: ModeChat}, "Sure, here is my reply.", 0.01)
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Kind != "text" {
		t.Fatalf("expected one text message, got %+v", resp.Messages)
	}
	if resp.NewPlan != "" {
		t.Fatal("chat mode must not set NewPlan")
	}
}

func TestInterpretResponse_DraftingPlan(t *testing.T) {
	resp, err := interpretResponse(TurnRequest{Stage: StageDrafting, Mode: ModePlan}, "# Plan\n\n- step 1\n- step 2\n", 0.02)
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if resp.NewPlan == "" {
		t.Fatal("plan mode should populate NewPlan")
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Kind != "plan" {
		t.Fatalf("expected one plan message, got %+v", resp.Messages)
	}
}

func TestInterpretResponse_GrillingTransitionsToReadyOnDone(t *testing.T) {
	resp, err := interpretResponse(
		TurnRequest{Stage: StageGrilling, Mode: ModeGrill},
		`{"done": true, "summary": "ok"}`,
		0,
	)
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if resp.NewStage != StageReady {
		t.Fatalf("expected ready, got %q", resp.NewStage)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Kind != "status" {
		t.Fatalf("expected status message, got %+v", resp.Messages)
	}
}

func TestInterpretResponse_GrillingProducesQuestions(t *testing.T) {
	out := "```json\n" +
		`{"questions":[{"prompt":"Q?","options":[{"id":"a","label":"A"}],"recommendation":"a"}]}` +
		"\n```"
	resp, err := interpretResponse(TurnRequest{Stage: StageGrilling, Mode: ModeGrill}, out, 0)
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if resp.NewStage != "" {
		t.Fatal("not done — stage must not transition")
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Kind != "question" {
		t.Fatalf("expected one question message, got %+v", resp.Messages)
	}
}

func TestTruncate_DoesNotSplitMultiByteRunes(t *testing.T) {
	// Three Japanese chars (each 3 bytes in UTF-8) — naive byte slicing at
	// n=4 would produce invalid UTF-8.
	in := "あいう"
	got := truncate(in, 2)
	want := "あい…"
	if got != want {
		t.Fatalf("truncate(%q, 2) = %q, want %q", in, got, want)
	}
	// No-op when input fits within the rune budget.
	if got := truncate("abc", 5); got != "abc" {
		t.Fatalf("truncate kept short string unchanged: got %q", got)
	}
	// Zero / negative budget returns empty rather than panicking.
	if got := truncate("abc", 0); got != "" {
		t.Fatalf("truncate(_, 0) should be empty, got %q", got)
	}
}

func TestStripFences_RemovesSingleWrapper(t *testing.T) {
	in := "```markdown\n# Plan\n\n- one\n```"
	got := stripFences(in)
	want := "# Plan\n\n- one"
	if got != want {
		t.Fatalf("stripFences = %q, want %q", got, want)
	}
}

func TestStripFences_LeavesUnfencedAlone(t *testing.T) {
	in := "# Plan\n\n- step\n"
	got := stripFences(in)
	want := "# Plan\n\n- step"
	if got != want {
		t.Fatalf("stripFences = %q, want %q", got, want)
	}
}
