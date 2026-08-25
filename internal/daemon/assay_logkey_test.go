package daemon

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// TestRunAssayReviewStampsLogKey pins the join the bead Logs panel groups on:
// the key the passes stamp into their session log filenames is the same key the
// run record persists. Two derivations of it — one for the request, one for the
// row — is exactly the arrangement that would silently stop matching and leave
// the panel back at six unrelated "assay" rows.
func TestRunAssayReviewStampsLogKey(t *testing.T) {
	d, _ := newAssayRunDaemon(t)

	var seen string
	d.assayReview = func(_ context.Context, req assay.ReviewRequest, _ *state.DB, _ assay.Config) (*assay.ReviewResult, error) {
		seen = req.LogKey
		return &assay.ReviewResult{
			Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
			Passes: []assay.PassReport{
				{Name: "triage"},
				{Name: "logic", Findings: 1},
				{Name: "security"},
			},
			Findings: make([]assay.Finding, 1),
		}, nil
	}

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.NotEmpty(t, run.LogKey)
	require.Equal(t, run.LogKey, seen, "the engine and the run record must carry the same log key")

	// All-digits is what keeps the reader's filename parse able to tell the key
	// from a pass name; the value is the run's start in milliseconds.
	ms, perr := strconv.ParseInt(run.LogKey, 10, 64)
	require.NoError(t, perr, "log key must be all-digits")
	require.Equal(t, run.StartedAt.UnixMilli(), ms)

	// The per-pass findings breakdown rides on the same record, which is what
	// lets the expanded run row tell the pass that found something from the
	// ones that did not.
	require.Equal(t, []state.AssayPassFindings{
		{Name: "triage", Findings: 0},
		{Name: "logic", Findings: 1},
		{Name: "security", Findings: 0},
	}, run.PassFindings)
}

// TestRunAssayReviewKeepsLogKeyOnFailure — a run that died still wrote session
// logs, and they are still the operator's evidence. Dropping the key on the
// error path would leave exactly those files ungrouped.
func TestRunAssayReviewKeepsLogKeyOnFailure(t *testing.T) {
	d, _ := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return nil, errors.New("all assay deep passes failed")
	}

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.Equal(t, state.AssayStatusFailed, run.Status)
	require.NotEmpty(t, run.LogKey)
}
