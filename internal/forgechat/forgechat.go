// Package forgechat backs the per-turn AI loop for the Hearth 2.0
// "Beads-Forge" page.
//
// Each Forge session moves through three stages — drafting, grilling, ready —
// and at each stage a "turn" is one round-trip with claude. The package
// exposes a Runner abstraction so the daemon can plug in a real
// smith.SpawnWithProvider implementation while tests can swap in a fake
// without standing up the claude CLI.
//
// Output contract:
//   - drafting: claude returns plain markdown text ("kind:text").
//   - drafting + plan request: claude returns a markdown plan ("kind:plan");
//     callers persist the body in session.plan as well.
//   - grilling: claude returns a JSON envelope of structured questions or a
//     done signal ("kind:question" / status update).
//
// The grilling-stage system prompt is the user's grill-me skill, baked in
// verbatim so behaviour matches the rest of the user's tooling.
package forgechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Stage identifies the current Beads-Forge stage. Mirrors the constants in
// internal/state to avoid an import cycle when the state layer references
// these names.
type Stage string

const (
	StageDrafting Stage = "drafting"
	StageGrilling Stage = "grilling"
	StageReady    Stage = "ready"
)

// Mode is the per-turn intent. Each mode produces a slightly different prompt
// even within the same stage.
type Mode string

const (
	// ModeChat is the default drafting-stage turn: respond conversationally.
	ModeChat Mode = "chat"
	// ModePlan is a drafting-stage turn that asks claude to emit a single
	// markdown plan as its full response.
	ModePlan Mode = "plan"
	// ModeGrill is a grilling-stage turn that asks claude to emit the next
	// batch of structured questions (or signal done).
	ModeGrill Mode = "grill"
)

// HistoryMessage is one prior turn fed into the next prompt. Kind matches
// the message kind constants in internal/state — kept as plain strings so
// this package does not need to import state.
type HistoryMessage struct {
	Role     string
	Kind     string
	Content  string
	Metadata string
}

// TurnRequest is the input to Runner.Turn. The Runner uses it to assemble a
// stage-appropriate prompt that includes the session title, current plan,
// and the prior conversation.
type TurnRequest struct {
	Stage    Stage
	Mode     Mode
	Title    string
	Plan     string
	History  []HistoryMessage
	UserText string
	// Anvils is the set of registered anvils the AI may target when emitting
	// beads (ModeEmit). Keys are anvil names; values are short hints. Unused
	// for chat / plan / grill modes.
	Anvils AnvilContext
	// Anvil is the session-scoped target for drafting / plan / grilling turns
	// — the bead being designed already has an anvil association, so the AI
	// is told the resolved name + absolute path up-front and uses it to read
	// the codebase via Read / Grep / Glob. Nil leaves the anvil context out
	// of the prompt (the AI then has to ask, which is what we're fixing).
	Anvil *AnvilTarget
}

// AnvilTarget is the resolved name + on-disk path of the anvil that owns a
// drafting / grilling session. The path is the absolute filesystem path the
// daemon would use when launching tools against that anvil — we render it
// into the prompt so the agent never has to ask the user where the code
// lives.
type AnvilTarget struct {
	Name string
	Path string
}

// EmittedMessage is one assistant message produced by a turn. The kind
// determines how the caller renders it in the chat view.
type EmittedMessage struct {
	Kind     string
	Content  string
	Metadata string
}

// TurnResponse is the output of Runner.Turn. NewStage is non-empty when
// the turn requested a stage transition (e.g. grilling reports "done").
// NewPlan is non-empty when the turn produced a fresh plan that should
// replace session.plan.
type TurnResponse struct {
	Messages []EmittedMessage
	NewStage Stage
	NewPlan  string
	// CostUSD is the cumulative cost of this turn (best-effort — real
	// runners read it from the claude stream, mocks may set 0).
	CostUSD float64
	// Emission is set when Mode == ModeEmit and parsing succeeded. Callers
	// inspect this to materialise beads via bd; the returned Messages slice
	// is empty in that case (the handler decides what to persist after
	// materialisation).
	Emission *EmissionEnvelope
}

// Runner abstracts the AI session driver. The daemon supplies a real
// implementation that spawns the claude CLI; tests use a mock.
type Runner interface {
	Turn(ctx context.Context, req TurnRequest) (*TurnResponse, error)
}

// QuestionOption is one selectable option attached to a grilling question.
type QuestionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionPayload is the JSON shape stored in metadata for kind=question.
type QuestionPayload struct {
	Options        []QuestionOption `json:"options"`
	Recommendation string           `json:"recommendation,omitempty"`
	Rationale      string           `json:"rationale,omitempty"`
}

// AnswerPayload is the JSON shape stored in metadata for kind=answer. The
// QuestionID points at the message ID of the question this answer addresses;
// OptionID is empty when the user wrote a free-form response instead of
// picking one of the options.
type AnswerPayload struct {
	QuestionID int64  `json:"question_id"`
	OptionID   string `json:"option_id,omitempty"`
}

// grillingVerdict is the JSON envelope claude is asked to emit during the
// grilling stage. Either Questions is non-empty (more interrogation) or
// Done is true (decision tree exhausted, transition to ready).
type grillingVerdict struct {
	Questions []struct {
		Prompt         string           `json:"prompt"`
		Options        []QuestionOption `json:"options"`
		Recommendation string           `json:"recommendation,omitempty"`
		Rationale      string           `json:"rationale,omitempty"`
	} `json:"questions"`
	Done    bool   `json:"done"`
	Summary string `json:"summary,omitempty"`
}

// BuildPrompt is the public-but-low-level entry point that turns a
// TurnRequest into the prompt string sent to claude. Real Runners use it
// directly; tests sometimes inspect the prompt to assert behaviour.
func BuildPrompt(req TurnRequest) string {
	var b strings.Builder

	switch {
	case req.Mode == ModeEmit:
		b.WriteString(systemPromptEmit)
	case req.Stage == StageDrafting && req.Mode == ModePlan:
		b.WriteString(systemPromptPlan)
	case req.Stage == StageDrafting:
		b.WriteString(systemPromptDrafting)
	case req.Stage == StageGrilling:
		b.WriteString(systemPromptGrilling)
	default:
		b.WriteString(systemPromptDrafting)
	}

	if t := strings.TrimSpace(req.Title); t != "" {
		b.WriteString("\n\n## Session title\n\n")
		b.WriteString(t)
	}

	if p := strings.TrimSpace(req.Plan); p != "" {
		b.WriteString("\n\n## Current plan\n\n")
		b.WriteString(p)
	}

	if req.Mode == ModeEmit {
		b.WriteString(formatAnvilContext(req.Anvils))
	} else if req.Anvil != nil {
		b.WriteString(formatSingleAnvilContext(*req.Anvil))
	}

	if len(req.History) > 0 {
		b.WriteString("\n\n## Conversation so far\n")
		for _, h := range req.History {
			label := h.Role
			if h.Kind != "" && h.Kind != "text" {
				label = h.Role + " (" + h.Kind + ")"
			}
			b.WriteString("\n### ")
			b.WriteString(label)
			b.WriteString("\n\n")
			b.WriteString(h.Content)
			if h.Metadata != "" && h.Kind == "question" {
				b.WriteString("\n\n_options metadata_:\n")
				b.WriteString(h.Metadata)
			}
		}
	}

	if u := strings.TrimSpace(req.UserText); u != "" {
		b.WriteString("\n\n## User's latest message\n\n")
		b.WriteString(u)
	}

	switch {
	case req.Mode == ModeEmit:
		b.WriteString("\n\n" + tailEmit)
	case req.Stage == StageDrafting && req.Mode == ModePlan:
		b.WriteString("\n\n" + tailPlan)
	case req.Stage == StageDrafting:
		b.WriteString("\n\n" + tailDrafting)
	case req.Stage == StageGrilling:
		b.WriteString("\n\n" + tailGrilling)
	}

	return b.String()
}

// ParseGrillingResponse extracts the JSON verdict claude was asked to emit
// during the grilling stage. Returns the structured envelope on success.
func ParseGrillingResponse(output string) (*grillingVerdict, error) {
	if v, err := tryParse(output); err == nil {
		return v, nil
	}
	// Fallback: scan for any JSON object containing "questions" or "done".
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		depth := 0
		for j := i; j < len(output); j++ {
			switch output[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := output[i : j+1]
					if strings.Contains(block, `"questions"`) || strings.Contains(block, `"done"`) {
						var v grillingVerdict
						if err := json.Unmarshal([]byte(block), &v); err == nil {
							return &v, nil
						}
					}
					i = j
					break
				}
			}
			if depth == 0 {
				break
			}
		}
	}
	return nil, errors.New("no valid grilling verdict JSON found in output")
}

// tryParse looks for a fenced ```json block first, then a plain ``` block.
func tryParse(output string) (*grillingVerdict, error) {
	if idx := strings.Index(output, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(output[start:], "```"); end >= 0 {
			var v grillingVerdict
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &v); err == nil {
				return &v, nil
			}
		}
	}
	if idx := strings.Index(output, "```"); idx >= 0 {
		start := idx + 3
		if nl := strings.Index(output[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(output[start:], "```"); end >= 0 {
			block := strings.TrimSpace(output[start : start+end])
			var v grillingVerdict
			if err := json.Unmarshal([]byte(block), &v); err == nil {
				return &v, nil
			}
		}
	}
	return nil, fmt.Errorf("no fenced JSON block")
}

// VerdictToMessages converts a parsed grilling verdict into the assistant
// messages the caller should persist. When the verdict signals done, the
// returned slice contains a single status message and NewStage is set to
// ready by the caller. Otherwise each question becomes a kind=question
// message with the options in metadata.
//
// Mixed verdicts (done=true AND questions present) are contradictory — the
// contract is "either ask more or call it done." We resolve the ambiguity in
// favour of the questions: an unanswered question is more important to
// surface than a possibly-premature stage transition, and a follow-up turn
// can still emit done with no questions if the AI is genuinely exhausted.
// We also emit a status note so the user sees the AI signalled completion
// at the same time.
func VerdictToMessages(v *grillingVerdict) ([]EmittedMessage, bool) {
	if v == nil {
		return nil, false
	}
	if v.Done && len(v.Questions) == 0 {
		summary := strings.TrimSpace(v.Summary)
		if summary == "" {
			summary = "Grilling complete — no further questions."
		}
		return []EmittedMessage{{
			Kind:    "status",
			Content: summary,
		}}, true
	}
	out := make([]EmittedMessage, 0, len(v.Questions)+1)
	if v.Done && len(v.Questions) > 0 {
		note := "AI signalled done but also emitted questions; staying in grilling and asking the questions first."
		if s := strings.TrimSpace(v.Summary); s != "" {
			note = note + " Summary: " + s
		}
		out = append(out, EmittedMessage{Kind: "status", Content: note})
	}
	for _, q := range v.Questions {
		payload := QuestionPayload{
			Options:        q.Options,
			Recommendation: q.Recommendation,
			Rationale:      q.Rationale,
		}
		md, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		out = append(out, EmittedMessage{
			Kind:     "question",
			Content:  q.Prompt,
			Metadata: string(md),
		})
	}
	return out, false
}

// System-level prompts. These are deliberately verbose so callers don't need
// to add boilerplate. The grilling prompt embeds the user's grill-me skill
// (per the bead description) verbatim.
//
// Filesystem access: drafting / plan / grilling turns may use the Read,
// Grep, and Glob tools against the target anvil (whose absolute path is
// rendered into the prompt). The agent must not modify files, write code,
// run destructive commands, or ask the user where the anvil lives — that
// information is provided up-front. The capability is intentionally read-
// only: this is a design conversation, not an implementation session.
const systemPromptDrafting = `You are a senior software architect helping the user shape a new bead (work item) into a clear, actionable engineering plan.

You are in the DRAFTING stage. Respond conversationally:
- ask clarifying questions when the user is vague,
- propose approaches and trade-offs,
- identify risks and missing pieces.

You have READ-ONLY access to the target anvil's codebase via the Read, Grep, and Glob tools. The anvil's absolute path is rendered below — use it directly; do NOT ask the user where the codebase lives or for filesystem permission. Before grilling the user with design questions, answer the ones you can answer yourself by reading the code (call sites, API shapes, existing tests, conventions). Do NOT modify any files. Do NOT run destructive shell commands. Do NOT create new files.

Respond with prose only — markdown is fine. Keep responses focused; aim for one or two short paragraphs unless the user explicitly asks for more.`

const systemPromptPlan = `You are a senior software architect summarising a Beads-Forge design conversation into a focused implementation plan.

The user has been iterating on an idea with you and now wants a concrete plan they can hand to a coding agent. Output ONLY the plan as markdown. Do NOT include conversational preamble or sign-off.

You may use the Read, Grep, and Glob tools against the target anvil (path rendered below) to ground the plan in the real codebase — concrete file paths and function signatures beat made-up ones. Do NOT modify files, write new code, or run destructive commands.

The plan must include:
- a one-sentence problem statement,
- the files to create or modify (paths if known, otherwise area names),
- key types, function signatures, or API shapes,
- the implementation sequence,
- how to verify the change works (tests, manual checks, etc.).

Keep it tight — one page of markdown, not a novel.`

const systemPromptGrilling = `Interview me relentlessly about every aspect of this plan until we reach a shared understanding. Walk down each branch of the design tree resolving dependencies between decisions one by one. If a question can be answered by exploring the codebase, explore the codebase instead. For each question, provide your recommended answer.

Questions should have options and recommendation(s); the user responds by either writing or picking an option.

You are in the GRILLING stage. The user has agreed on a high-level plan (above) and now wants you to interrogate every decision until the design tree is exhausted. Each turn, emit ONE structured JSON envelope of the next set of questions — pick the most decision-blocking ones first, do not dump the entire tree at once. When you genuinely cannot find another question worth asking, return done:true.

You have READ-ONLY access to the target anvil via the Read, Grep, and Glob tools (the anvil's absolute path is rendered below). Use those tools to answer your own questions about the code before asking the user — never ask the user where the codebase lives or for filesystem permission. Do NOT modify any files.`

const tailDrafting = `Respond now with your conversational reply. Plain markdown text only.`

const tailPlan = `Output the markdown plan now. No preamble, no sign-off, just the plan body.`

const tailGrilling = "Output your next batch of questions (or done) NOW as a JSON block in fenced ```json ... ``` form.\n\nShape:\n\n```json\n{\n  \"questions\": [\n    {\n      \"prompt\": \"Should the API accept paginated results?\",\n      \"options\": [\n        {\"id\": \"yes_cursor\", \"label\": \"Yes, cursor-based\", \"description\": \"Stable across writes\"},\n        {\"id\": \"yes_offset\", \"label\": \"Yes, offset-based\", \"description\": \"Simpler client\"},\n        {\"id\": \"no\", \"label\": \"No, single page\", \"description\": \"Bound result count\"}\n      ],\n      \"recommendation\": \"yes_cursor\",\n      \"rationale\": \"Cursor avoids drift on large datasets\"\n    }\n  ]\n}\n```\n\nLimit each turn to 1–3 questions. Pick the most decision-blocking unresolved questions first; do NOT re-ask anything the user already answered above. Each option needs an id, label, and (optionally) a short description.\n\nWhen you have nothing left worth asking, emit instead:\n\n```json\n{\"done\": true, \"summary\": \"Brief recap of the agreed design.\"}\n```"
