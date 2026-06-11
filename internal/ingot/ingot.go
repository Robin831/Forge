// Package ingot provides the data model and persistence layer for Ingots.
//
// An Ingot is a compound record that bundles a bead, PR, worker lifecycle,
// and structured test results into a single queryable record.
package ingot

import "time"

// Status represents the lifecycle stage of an ingot.
type Status string

const (
	StatusInit     Status = "init"
	StatusSmith    Status = "smith"
	StatusTemper   Status = "temper"
	StatusWarden   Status = "warden"
	StatusApproved Status = "approved"
	StatusPROpen   Status = "pr_open"
	StatusPRMerged Status = "pr_merged"
	StatusFailed   Status = "failed"
	StatusStalled  Status = "stalled"
	// StatusPRCreateFailed marks an ingot whose branch was pushed but for which
	// the final PR creation failed (after transient retries were exhausted). The
	// pushed branch, head SHA, and classified error are persisted alongside it so
	// the bead can be recovered via the manual create-PR-from-existing-branch path
	// without re-running Smith.
	StatusPRCreateFailed Status = "pr_create_failed"
)

// Ingot bundles a bead, PR, worker lifecycle, and test results into a
// single queryable record.
type Ingot struct {
	ID               int    `json:"id"`
	BeadID           string `json:"bead_id"`
	Anvil            string `json:"anvil"`
	PRID             *int   `json:"pr_id,omitempty"` // FK to prs.id, NULL until PR created
	WorkerID         string `json:"worker_id"`
	Status           Status `json:"status"`
	TemperPassed     bool   `json:"temper_passed"`
	TemperFailedStep string `json:"temper_failed_step,omitempty"`
	TemperDurationMs int    `json:"temper_duration_ms"`
	PRNumber         *int   `json:"pr_number,omitempty"` // GitHub PR number
	PRURL            string `json:"pr_url,omitempty"`
	Title            string `json:"title"`
	Branch           string `json:"branch"`
	// HeadSHA is the tip commit of the pushed forge branch, recorded when PR
	// creation fails so a recovery can reference the exact pushed work.
	HeadSHA string `json:"head_sha,omitempty"`
	// PRCreateError is the classified error string from the last failed PR
	// creation attempt. Empty when the ingot is not in a pr_create_failed state.
	PRCreateError string       `json:"pr_create_error,omitempty"`
	TestResults   []TestResult `json:"test_results,omitempty"` // eager-loaded
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

// TestResult stores the outcome of a single temper step for an ingot.
type TestResult struct {
	ID         int    `json:"id"`
	IngotID    int    `json:"ingot_id"`
	StepIndex  int    `json:"step_index"`
	StepName   string `json:"step_name"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int    `json:"duration_ms"`
	Passed     bool   `json:"passed"`
	Optional   bool   `json:"optional"`
	// Skipped is true when the step was path-gated off (no changed files matched its Paths globs).
	Skipped        bool      `json:"skipped"`
	OutputSummary  string    `json:"output_summary,omitempty"` // first ~1000 chars or key errors
	FullOutputPath string    `json:"full_output_path,omitempty"`
	RecordedAt     time.Time `json:"recorded_at"`
}
