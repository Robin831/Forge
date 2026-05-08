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
