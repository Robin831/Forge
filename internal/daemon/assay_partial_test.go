package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// TestAssayWorkerStatus pins the worker row's terminal status for each run
// outcome. The one that matters is partial: findings were produced and posted,
// so "failed" would bury them — and passes never ran, so "done" would sell an
// incomplete review as a full one.
func TestAssayWorkerStatus(t *testing.T) {
	tests := []struct {
		name   string
		run    *state.AssayRun
		recErr error
		want   state.WorkerStatus
	}{
		{"clean run", &state.AssayRun{Status: state.AssayStatusComplete}, nil, state.WorkerDone},
		{
			"partial run",
			&state.AssayRun{
				Status: state.AssayStatusPartial,
				Error:  "assay pass logic: provider claude failed (exit 1, subtype error_max_turns)",
			},
			nil,
			state.WorkerPartial,
		},
		{
			"failed run",
			&state.AssayRun{Status: state.AssayStatusFailed, Error: "all assay deep passes failed"},
			nil,
			state.WorkerFailed,
		},
		{
			"record error",
			&state.AssayRun{Status: state.AssayStatusComplete},
			errors.New("recording assay run: disk full"),
			state.WorkerFailed,
		},
		{"nil run", nil, nil, state.WorkerFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, assayWorkerStatus(tt.run, tt.recErr))
		})
	}
}

// TestStatePassFailuresMirrorsEngine verifies the two failed-pass types stay in
// step: what the engine reports is what the run record persists, name for name
// and reason for reason.
func TestStatePassFailuresMirrorsEngine(t *testing.T) {
	got := statePassFailures([]assay.PassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "rate_limited"},
	})
	require.Equal(t, []state.AssayPassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "rate_limited"},
	}, got)
	require.Nil(t, statePassFailures(nil))
}

// TestStateAssayStatusMirrorsEngine pins the assignment the daemon makes with a
// bare conversion — run.Status = string(result.Status). Nothing else asserts
// that the two constant sets carry identical literals, and a drift on the
// engine side would be silent rather than red: a partial run would persist an
// unrecognised status, fall through the run.Error != "" branch and report as a
// failure while every existing test still passed.
func TestStateAssayStatusMirrorsEngine(t *testing.T) {
	require.Equal(t, state.AssayStatusComplete, string(assay.RunStatusComplete))
	require.Equal(t, state.AssayStatusPartial, string(assay.RunStatusPartial))
	require.Equal(t, state.AssayStatusFailed, string(assay.RunStatusFailed))
}
