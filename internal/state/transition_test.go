package state

import "testing"

// TestCanTransitionPause exercises the paused-status state machine: every valid
// transition into and out of paused must be allowed, and representative invalid
// transitions (including ones that never touch paused) must be rejected.
func TestCanTransitionPause(t *testing.T) {
	tests := []struct {
		name string
		from WorkerStatus
		to   WorkerStatus
		want bool
	}{
		// Valid transitions.
		{"running to paused", WorkerRunning, WorkerPaused, true},
		{"paused to running", WorkerPaused, WorkerRunning, true},
		{"paused to killed", WorkerPaused, WorkerKilled, true},

		// Invalid transitions into paused.
		{"pending to paused", WorkerPending, WorkerPaused, false},
		{"reviewing to paused", WorkerReviewing, WorkerPaused, false},
		{"monitoring to paused", WorkerMonitoring, WorkerPaused, false},
		{"done to paused", WorkerDone, WorkerPaused, false},
		{"failed to paused", WorkerFailed, WorkerPaused, false},
		{"stalled to paused", WorkerStalled, WorkerPaused, false},
		{"killed to paused", WorkerKilled, WorkerPaused, false},
		{"paused to paused", WorkerPaused, WorkerPaused, false},

		// Invalid transitions out of paused.
		{"paused to pending", WorkerPaused, WorkerPending, false},
		{"paused to reviewing", WorkerPaused, WorkerReviewing, false},
		{"paused to monitoring", WorkerPaused, WorkerMonitoring, false},
		{"paused to done", WorkerPaused, WorkerDone, false},
		{"paused to failed", WorkerPaused, WorkerFailed, false},
		{"paused to timeout", WorkerPaused, WorkerTimeout, false},
		{"paused to stalled", WorkerPaused, WorkerStalled, false},

		// Transitions that never involve paused are out of scope and rejected.
		{"running to done", WorkerRunning, WorkerDone, false},
		{"pending to running", WorkerPending, WorkerRunning, false},
		{"running to killed", WorkerRunning, WorkerKilled, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransitionPause(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransitionPause(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

// TestWorkerStatusIsMonitorOnly pins which statuses mark a row as bookkeeping
// rather than work in progress. Every status that owns a process, a pipeline
// goroutine or a worktree must answer false, so a caller draining before a
// restart keeps waiting on it (Forge-ti4e).
func TestWorkerStatusIsMonitorOnly(t *testing.T) {
	tests := []struct {
		status WorkerStatus
		want   bool
	}{
		{WorkerMonitoring, true},
		{WorkerDetached, true},

		{WorkerPending, false},
		{WorkerRunning, false},
		{WorkerReviewing, false},
		{WorkerStalled, false},
		{WorkerPaused, false},
		{WorkerDone, false},
		{WorkerFailed, false},
		{WorkerPartial, false},
		{WorkerTimeout, false},
		{WorkerKilled, false},
	}
	for _, tt := range tests {
		if got := tt.status.IsMonitorOnly(); got != tt.want {
			t.Errorf("WorkerStatus(%q).IsMonitorOnly() = %v, want %v", tt.status, got, tt.want)
		}
	}
}
