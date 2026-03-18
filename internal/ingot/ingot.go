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
)

// Ingot bundles a bead, PR, worker lifecycle, and test results into a
// single queryable record.
type Ingot struct {
	ID               int
	BeadID           string
	Anvil            string
	PRID             *int       // FK to prs.id, NULL until PR created
	WorkerID         string
	Status           Status
	TemperPassed     bool
	TemperFailedStep string
	TemperDurationMs int
	PRNumber         *int // GitHub PR number
	PRURL            string
	Title            string
	Branch           string
	TestResults      []TestResult // eager-loaded
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TestResult stores the outcome of a single temper step for an ingot.
type TestResult struct {
	ID             int
	IngotID        int
	StepIndex      int
	StepName       string
	Command        string
	ExitCode       int
	DurationMs     int
	Passed         bool
	Optional       bool
	OutputSummary  string // first ~1000 chars or key errors
	FullOutputPath string
	RecordedAt     time.Time
}
