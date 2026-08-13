package daemon

// Tests for the incremental (delta) Assay review scope: a repeat review reads
// only the changes since the last reviewed commit, falls back to the full diff
// when the delta is unavailable, and skips the run entirely when nothing
// reviewable changed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

const incrFullDiff = "diff --git a/a.go b/a.go\n" +
	"--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n old\n+new\n"

const incrDeltaDiff = "diff --git a/a.go b/a.go\n" +
	"--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n old\n+delta-only\n"

// deltaOutsideNetDiff touches only a file absent from the net diff (upstream
// merge churn / a revert): scoping it against the full diff leaves nothing.
const deltaOutsideNetDiff = "diff --git a/vendor.go b/vendor.go\n" +
	"--- a/vendor.go\n+++ b/vendor.go\n@@ -1 +1 @@\n-x\n+y\n"

// seedReviewedRun records a completed run of sinceSHA so LastReviewedSHA
// answers with it.
func seedReviewedRun(t *testing.T, db *state.DB, sinceSHA string) {
	t.Helper()
	require.NoError(t, db.RecordAssayRun(&state.AssayRun{
		Anvil: "forge", PRNumber: 347, HeadSHA: sinceSHA,
		StartedAt: time.Now(), Status: state.AssayStatusComplete,
	}))
}

// captureAssayReview installs an engine stub that records the request and
// reports a clean run.
func captureAssayReview(d *Daemon) *assay.ReviewRequest {
	var captured assay.ReviewRequest
	d.assayReview = func(_ context.Context, req assay.ReviewRequest, _ *state.DB, _ assay.Config) (*assay.ReviewResult, error) {
		captured = req
		return &assay.ReviewResult{Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5}, nil
	}
	return &captured
}

func TestRunAssayReviewUsesDeltaOnRepeatReview(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) { return []byte(incrFullDiff), nil }
	d.assayDeltaFetch = func(_ context.Context, _, since, head string) ([]byte, error) {
		require.Equal(t, "oldsha", since)
		require.Equal(t, "deadbeef", head)
		return []byte(incrDeltaDiff), nil
	}
	captured := captureAssayReview(d)
	seedReviewedRun(t, db, "oldsha")

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.Equal(t, state.AssayStatusComplete, run.Status)

	require.True(t, captured.Incremental, "repeat review should be incremental")
	require.Equal(t, "oldsha", captured.BaselineSHA)
	require.Contains(t, captured.Diff, "delta-only", "engine should read the delta, not the full diff")
	require.NotContains(t, captured.Diff, "+new\n", "full-diff content should not reach the engine on a delta review")
}

func TestRunAssayReviewFullDiffOnFirstReview(t *testing.T) {
	d, _ := newAssayRunDaemon(t)
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) { return []byte(incrFullDiff), nil }
	d.assayDeltaFetch = func(context.Context, string, string, string) ([]byte, error) {
		t.Fatal("no delta fetch expected on a first review")
		return nil, nil
	}
	captured := captureAssayReview(d)

	_, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.False(t, captured.Incremental)
	require.Contains(t, captured.Diff, "+new")
}

func TestRunAssayReviewFallsBackToFullDiffWhenDeltaUnavailable(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) { return []byte(incrFullDiff), nil }
	d.assayDeltaFetch = func(context.Context, string, string, string) ([]byte, error) {
		return nil, errors.New("not an ancestor (force-push)")
	}
	captured := captureAssayReview(d)
	seedReviewedRun(t, db, "oldsha")

	_, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.False(t, captured.Incremental, "an unavailable delta must fall back to a full review")
	require.Contains(t, captured.Diff, "+new")
}

func TestRunAssayReviewSkipsWhenDeltaHasNothingReviewable(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) { return []byte(incrFullDiff), nil }
	d.assayDeltaFetch = func(context.Context, string, string, string) ([]byte, error) {
		return []byte(deltaOutsideNetDiff), nil
	}
	engineRan := false
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		engineRan = true
		return &assay.ReviewResult{Status: assay.RunStatusComplete}, nil
	}
	seedReviewedRun(t, db, "oldsha")

	run, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.False(t, engineRan, "nothing reviewable changed; the engine must not run")
	require.Equal(t, state.AssayStatusComplete, run.Status)
	require.Equal(t, "no reviewable changes since last review", run.SkippedReason)

	// The skipped-complete run marks the head reviewed (no re-trigger loop)...
	sha, err := db.LastReviewedSHA("forge", 347)
	require.NoError(t, err)
	require.Equal(t, "deadbeef", sha)
	// ...but never consumes the per-PR executed-run cap.
	n, err := db.CountAssayRuns("forge", 347)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the seeded real run should count")
}

func TestRunAssayReviewIncrementalDisabledByConfig(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	incremental := false
	d.cfg.Store(&config.Config{
		Assay: config.AssayConfig{
			Enabled:     assayTestBool(true),
			ShadowMode:  assayTestBool(true),
			Incremental: &incremental,
		},
	})
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) { return []byte(incrFullDiff), nil }
	d.assayDeltaFetch = func(context.Context, string, string, string) ([]byte, error) {
		t.Fatal("no delta fetch expected with incremental disabled")
		return nil, nil
	}
	captured := captureAssayReview(d)
	seedReviewedRun(t, db, "oldsha")

	_, err := runTestAssayReview(t, d)
	require.NoError(t, err)
	require.False(t, captured.Incremental)
}
