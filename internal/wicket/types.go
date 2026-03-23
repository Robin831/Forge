// Package wicket polls GitHub issues, triages them with AI for trusted users,
// and creates beads or posts comments based on the AI decision.
package wicket

import "time"

// TriageAction represents the decision the AI makes about a GitHub issue.
type TriageAction string

const (
	// ActionCreateBead means the issue is clear and actionable: create a bead.
	ActionCreateBead TriageAction = "create_bead"
	// ActionAskClarify means the issue needs more information before acting.
	ActionAskClarify TriageAction = "ask_clarify"
	// ActionFlagHuman means the issue requires human judgment or strategic input.
	ActionFlagHuman TriageAction = "flag_human"
)

// TriageDecision is the structured output from the AI triage step.
type TriageDecision struct {
	Action      TriageAction `json:"action"`
	Title       string       `json:"title,omitempty"`       // Proposed bead title (ActionCreateBead)
	Description string       `json:"description,omitempty"` // Proposed bead description (ActionCreateBead)
	IssueType   string       `json:"type,omitempty"`        // "bug", "feature", "task" (ActionCreateBead)
	Priority    int          `json:"priority,omitempty"`    // 0-4 (ActionCreateBead)
	Question    string       `json:"question,omitempty"`    // Clarification question (ActionAskClarify)
	Reasoning   string       `json:"reasoning"`             // Explanation for the decision
}

// IssueAuthor holds information about the author of an issue.
type IssueAuthor struct {
	Login string `json:"login"`
}

// IssueLabel represents a label on a GitHub issue.
type IssueLabel struct {
	Name string `json:"name"`
}

// IssueComment is a single comment on a GitHub issue.
type IssueComment struct {
	Author    IssueAuthor `json:"author"`
	Body      string      `json:"body"`
	CreatedAt time.Time   `json:"createdAt"`
}

// Issue represents a GitHub issue returned by the gh CLI.
type Issue struct {
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	State     string         `json:"state"`
	Author    IssueAuthor    `json:"author"`
	Labels    []IssueLabel   `json:"labels"`
	Comments  []IssueComment `json:"comments"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	URL       string         `json:"url"`
	IsPR      bool           `json:"isPullRequest,omitempty"`
}

// HasLabel reports whether the issue has the named label (case-insensitive).
func (i *Issue) HasLabel(name string) bool {
	for _, lbl := range i.Labels {
		if equalFold(lbl.Name, name) {
			return true
		}
	}
	return false
}

// equalFold is a simple ASCII case-insensitive comparison helper.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
