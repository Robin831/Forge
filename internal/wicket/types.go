// Package wicket implements the GitHub issue triage monitor. It polls
// configured repositories for new issues, runs an AI triage pass to decide
// whether each issue should become a bead, needs clarification, or should be
// flagged for a human, and then acts on the decision.
package wicket

import "time"

// TriageAction is the outcome of a triage decision for a single issue.
type TriageAction string

const (
	// ActionCreateBead instructs Wicket to create a bead from the issue.
	ActionCreateBead TriageAction = "create_bead"
	// ActionAskClarify instructs Wicket to post a clarification comment on
	// the issue asking the author for more detail before proceeding.
	ActionAskClarify TriageAction = "ask_clarify"
	// ActionFlagHuman instructs Wicket to label the issue for human review
	// and skip automated processing.
	ActionFlagHuman TriageAction = "flag_human"
	// ActionReject instructs Wicket to silently discard the issue without
	// posting any public comment. Used for obvious spam or off-topic issues.
	ActionReject TriageAction = "reject"
)

// defaultBotIgnoreList is a hardcoded set of well-known bot account patterns
// that Wicket always skips regardless of configuration. Comparison is
// case-insensitive.
var defaultBotIgnoreList = []string{
	"dependabot[bot]",
	"renovate[bot]",
	"github-actions[bot]",
	"codecov[bot]",
	"snyk-bot",
	"greenkeeper[bot]",
	"imgbot[bot]",
	"allcontributors[bot]",
	"semantic-release-bot",
	"stale[bot]",
	"copilot[bot]",
	"github-advanced-security[bot]",
}

// TriageDecision is the structured output from the AI triage step.
type TriageDecision struct {
	// Action is the chosen triage outcome.
	Action TriageAction
	// Reason is a human-readable explanation of why this action was chosen.
	Reason string
	// BeadTitle is the proposed bead title; only populated when Action is
	// ActionCreateBead.
	BeadTitle string
	// BeadDescription is the proposed bead description; only populated when
	// Action is ActionCreateBead.
	BeadDescription string
}

// Issue represents a GitHub issue retrieved during a Wicket scan.
type Issue struct {
	// Number is the issue number within the repository.
	Number int
	// Repo is the full repository name in "owner/repo" format.
	Repo string
	// Title is the issue title.
	Title string
	// Body is the issue body (description).
	Body string
	// Author is the GitHub login of the issue author.
	Author string
	// Labels is the list of label names attached to the issue.
	Labels []string
	// CreatedAt is when the issue was opened.
	CreatedAt time.Time
}

// Comment represents a single comment on a GitHub issue.
type Comment struct {
	// ID is the platform-assigned comment identifier.
	ID int64
	// Author is the GitHub login of the comment author.
	Author string
	// Body is the comment text.
	Body string
	// CreatedAt is when the comment was posted.
	CreatedAt time.Time
}

// Reaction represents an aggregated emoji reaction on a GitHub issue or comment.
type Reaction struct {
	// Content is the reaction emoji identifier (e.g. "+1", "heart").
	Content string
	// Count is the number of users who reacted with this emoji.
	Count int
}

// AnvilWicketConfig holds the per-anvil Wicket configuration. Fields use the
// same yaml/mapstructure tags as config.AnvilConfig so that config loading
// can populate them automatically via mapstructure. Consumers in the wicket
// package should read from this struct rather than calling config.AnvilConfig
// directly, to keep the dependency surface narrow.
type AnvilWicketConfig struct {
	// Enabled controls whether Wicket scans this anvil. When nil, the
	// global WicketEnabled setting is used. Set to false to opt out.
	Enabled *bool `yaml:"wicket_enabled,omitempty" mapstructure:"wicket_enabled"`
	// TrustedUsers is the list of GitHub logins whose issues are
	// auto-dispatched without extra review. Issues from other authors
	// follow the normal triage flow.
	TrustedUsers []string `yaml:"wicket_trusted_users,omitempty" mapstructure:"wicket_trusted_users"`
	// AutoDispatch, when true, automatically dispatches triaged beads
	// without waiting for a human to approve the queue entry.
	AutoDispatch bool `yaml:"wicket_auto_dispatch,omitempty" mapstructure:"wicket_auto_dispatch"`
	// IssueLabels is the list of GitHub label names that must be present
	// on an issue for Wicket to consider it. An empty list means all
	// unlabelled/labelled issues are eligible (subject to WicketTriggerLabel).
	IssueLabels []string `yaml:"wicket_issue_labels,omitempty" mapstructure:"wicket_issue_labels"`
	// Repos is the list of "owner/repo" strings to scan. When empty, the
	// anvil's primary repository (derived from its git remote) is used.
	Repos []string `yaml:"wicket_repos,omitempty" mapstructure:"wicket_repos"`
	// TriagePrompt is an optional freeform prompt suffix appended to the
	// default Wicket triage system prompt. Use it to add project-specific
	// context or constraints (e.g. "This is a public API — be conservative
	// about accepting feature requests from external contributors.").
	TriagePrompt string `yaml:"wicket_triage_prompt,omitempty" mapstructure:"wicket_triage_prompt"`
	// IgnoreUsers is a list of GitHub logins to skip entirely when triaging
	// issues. In addition to this list, defaultBotIgnoreList is always
	// applied. Comparison is case-insensitive.
	IgnoreUsers []string `yaml:"wicket_ignore_users,omitempty" mapstructure:"wicket_ignore_users"`
}
