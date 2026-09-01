package daemon

// Tests for the per-PR Assay run cap on the Burnish coordination path.
//
// Bellows stops re-firing Assay once a PR has been reviewed max_runs times, so
// the Assay -> Burnish -> new-head loop terminates. ensureAssayReviewedHead is
// reached from the Burnish worker instead of from Bellows, and used to bypass
// that cap: a PR Bellows had already stopped reviewing still got a fresh Assay
// run on every Burnish round. Munin#5423 logged "run cap reached (3/2)" from
// Bellows while Assay ran a third and fourth time from here.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

// runCapStubVCS reports a fixed head SHA. The embedded nil interface satisfies
// vcs.Provider without implementing it: ensureAssayReviewedHead calls only
// CheckStatusLight, and any other method would panic loudly rather than
// silently returning a zero value.
type runCapStubVCS struct {
	vcs.Provider
	headSHA string
}

func (s *runCapStubVCS) CheckStatusLight(context.Context, string, int) (*vcs.PRStatus, error) {
	return &vcs.PRStatus{HeadSHA: s.headSHA}, nil
}

// newRunCapDaemon builds a daemon on the Burnish coordination path: Assay live
// (not shadow, or ensureAssayReviewedHead returns before the cap gate) with the
// given per-PR cap, and a VCS whose head is always unreviewed.
func newRunCapDaemon(t *testing.T, maxRuns int) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(t.TempDir() + "/state.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.cfg.Store(&config.Config{
		Assay: config.AssayConfig{
			Enabled:    assayTestBool(true),
			ShadowMode: assayTestBool(false),
			MaxRuns:    &maxRuns,
		},
	})
	d.vcsProviders = map[string]vcs.Provider{"forge": &runCapStubVCS{headSHA: "cafebabe"}}
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) {
		return []byte("diff --git a/a.go b/a.go\n"), nil
	}
	return d, db
}

// recordAssayRuns writes n completed (non-skipped) runs for the PR, which is
// what CountAssayRuns counts.
func recordAssayRuns(t *testing.T, db *state.DB, anvil string, prNumber, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(t, db.RecordAssayRun(&state.AssayRun{
			Anvil:     anvil,
			PRNumber:  prNumber,
			HeadSHA:   "head" + string(rune('a'+i)),
			StartedAt: time.Now().UTC(),
			Status:    state.AssayStatusComplete,
		}))
	}
}

// callEnsureAssayReviewedHead drives the coordination path and reports whether
// it reached the Assay engine. The engine seam returns an error so the run ends
// before the posting block, which would otherwise shell out to gh.
func callEnsureAssayReviewedHead(t *testing.T, d *Daemon) bool {
	t.Helper()
	reached := false
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		reached = true
		return nil, errors.New("stub: engine not exercised by this test")
	}
	d.ensureAssayReviewedHead(context.Background(), "forge", t.TempDir(), "Forge-abc1", 5423, t.TempDir())
	return reached
}

// TestEnsureAssayReviewedHeadHonoursRunCap is the regression guard for the
// bypass: at or past the cap the Burnish path must not start another review.
func TestEnsureAssayReviewedHeadHonoursRunCap(t *testing.T) {
	tests := []struct {
		name        string
		maxRuns     int
		priorRuns   int
		wantReached bool
	}{
		{name: "under the cap reviews", maxRuns: 2, priorRuns: 1, wantReached: true},
		{name: "at the cap does not review", maxRuns: 2, priorRuns: 2, wantReached: false},
		// Bellows logged 3/2 on Munin#5423 — the count can already exceed the
		// cap by the time this path is reached, so the gate is >=, not ==.
		{name: "past the cap does not review", maxRuns: 2, priorRuns: 3, wantReached: false},
		// max_runs <= 0 means no cap, matching AssayConfig.GetMaxRuns and the
		// Bellows gate; an uncapped PR must still be reviewable.
		{name: "cap disabled always reviews", maxRuns: 0, priorRuns: 9, wantReached: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, db := newRunCapDaemon(t, tt.maxRuns)
			recordAssayRuns(t, db, "forge", 5423, tt.priorRuns)
			require.Equal(t, tt.wantReached, callEnsureAssayReviewedHead(t, d))
		})
	}
}

// TestEnsureAssayReviewedHeadRunCapCountsOnlyThisPR pins the cap to the PR it
// is capping: reviews of other PRs on the same anvil must not exhaust it.
func TestEnsureAssayReviewedHeadRunCapCountsOnlyThisPR(t *testing.T) {
	d, db := newRunCapDaemon(t, 2)
	recordAssayRuns(t, db, "forge", 9999, 5)
	require.True(t, callEnsureAssayReviewedHead(t, d))
}
