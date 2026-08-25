package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

func assayTestBool(v bool) *bool { return &v }

// newAssayRunDaemon builds a daemon whose Assay run has no external moving
// parts: the diff fetch and the engine are both stubbed, and shadow mode keeps
// the posting path (the one remaining thing that would reach GitHub) out of it.
func newAssayRunDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.cfg.Store(&config.Config{
		Assay: config.AssayConfig{
			Enabled:    assayTestBool(true),
			ShadowMode: assayTestBool(true),
		},
	})
	d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) {
		return []byte("diff --git a/a.go b/a.go\n"), nil
	}
	return d, db
}

// runTestAssayReview drives one review with the standard arguments; workerID is
// empty so no worker row is touched.
func runTestAssayReview(t *testing.T, d *Daemon) (*state.AssayRun, error) {
	t.Helper()
	return d.runAssayReview(context.Background(), "forge", t.TempDir(), "Forge-abc1", 347, "deadbeef", t.TempDir(), "")
}

// TestRunAssayReviewEmitsExactlyOneTerminalEvent is the invariant this whole
// path exists for: every way out of runAssayReview closes the review in the
// activity feed exactly once, so the pr_review_needed that opened it always has
// a matching resolution — never two rows for one run, never none.
//
// Unit tests of emitAssayTerminalEvent cannot pin this: they prove the emitter
// writes one event when called, not that runAssayReview calls it once on each
// terminal path. An early return added above the emit, or a per-status LogEvent
// added back inside the status switch (which is what the old assay_partial
// double-emit was), would leave those passing and break this.
func TestRunAssayReviewEmitsExactlyOneTerminalEvent(t *testing.T) {
	tests := []struct {
		name string
		// setup installs the outcome under test on the daemon.
		setup       func(t *testing.T, d *Daemon)
		wantType    state.EventType
		wantStatus  string
		wantMessage string
	}{
		{
			name: "every pass reviewed the head",
			setup: func(_ *testing.T, d *Daemon) {
				d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
					return &assay.ReviewResult{
						Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
						Findings: make([]assay.Finding, 7), CostUSD: 2.8,
					}, nil
				}
			},
			wantType:   state.EventAssayCompleted,
			wantStatus: state.AssayStatusComplete,
			// Shadow mode is on, so the row says the findings went nowhere
			// public — the case that used to be invisible end to end. The
			// duration is the daemon's own measured wall clock (0s under a
			// stubbed engine), not anything the engine reported.
			wantMessage: "Assay PR #347: complete — 5/5 passes, 7 findings ($2.80, 0s) (shadow — findings in panel only)",
		},
		{
			name: "some passes never reviewed the head",
			setup: func(_ *testing.T, d *Daemon) {
				d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
					return &assay.ReviewResult{
						Status: assay.RunStatusPartial, CompletedPasses: 3, TotalPasses: 5,
						FailedPasses: []assay.PassFailure{
							{Name: "logic", Reason: "error_max_turns"},
							{Name: "repo-specific", Reason: "error_max_turns"},
						},
						PassErrors: []string{"logic: error_max_turns", "repo-specific: error_max_turns"},
						Findings:   make([]assay.Finding, 4), CostUSD: 1.2,
					}, nil
				}
			},
			wantType:   state.EventAssayPartial,
			wantStatus: state.AssayStatusPartial,
			wantMessage: "Assay PR #347: partial — 3/5 passes (failed: logic, repo-specific — error_max_turns), " +
				"4 findings ($1.20, 0s) (shadow — findings in panel only)",
		},
		{
			name: "the engine returned an error",
			setup: func(_ *testing.T, d *Daemon) {
				d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
					return nil, errors.New("all assay deep passes failed")
				}
			},
			wantType:    state.EventAssayFailed,
			wantStatus:  state.AssayStatusFailed,
			wantMessage: "Assay PR #347: failed — all assay deep passes failed ($0.00, 0s)",
		},
		{
			name: "the diff could not be fetched",
			setup: func(t *testing.T, d *Daemon) {
				d.assayDiffFetch = func(context.Context, string, int) ([]byte, error) {
					return nil, errors.New("exit status 1")
				}
				d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
					t.Error("engine ran despite an unfetchable diff")
					return nil, errors.New("unreachable")
				}
			},
			wantType:    state.EventAssayFailed,
			wantStatus:  state.AssayStatusFailed,
			wantMessage: "Assay PR #347: failed — diff fetch failed: exit status 1 ($0.00, 0s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, db := newAssayRunDaemon(t)
			tt.setup(t, d)

			run, err := runTestAssayReview(t, d)
			require.NoError(t, err)
			require.NotNil(t, run)
			require.Equal(t, tt.wantStatus, run.Status)

			evs := assayEvents(t, db)
			require.Len(t, evs, 1, "expected exactly one terminal event per run")
			require.Equal(t, tt.wantType, evs[0].Type)
			require.Equal(t, tt.wantMessage, evs[0].Message)
			require.Equal(t, "Forge-abc1", evs[0].BeadID)
			require.Equal(t, "forge", evs[0].Anvil)
		})
	}
}

// TestRunAssayReviewEmitsWhenRecordingFails: the review happened, so the feed
// must say so even when the assay_runs row could not be written. Losing both
// the record and the only notice that a review ran is what would leave a
// pr_review_needed open forever.
func TestRunAssayReviewEmitsWhenRecordingFails(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return &assay.ReviewResult{
			Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
			Findings: make([]assay.Finding, 1), CostUSD: 0.5,
		}, nil
	}
	// Break the recording target only — events still write, which is the
	// asymmetry the emit-after-record ordering relies on.
	_, err := db.Conn().Exec(`DROP TABLE assay_runs`)
	require.NoError(t, err)

	run, err := runTestAssayReview(t, d)
	require.Error(t, err, "the record error is still returned to the caller")
	require.NotNil(t, run)

	evs := assayEvents(t, db)
	require.Len(t, evs, 1, "expected exactly one terminal event per run")
	require.Equal(t, state.EventAssayCompleted, evs[0].Type)
}

// TestRunAssayReviewEmitsOncePerInvocation: two runs of the same PR leave two
// rows, not one deduped row and not four. The 1:1 pairing is per dispatched
// review, and a re-review after a new head is an ordinary second dispatch.
func TestRunAssayReviewEmitsOncePerInvocation(t *testing.T) {
	d, db := newAssayRunDaemon(t)
	d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
		return &assay.ReviewResult{
			Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
			CostUSD: 0.5,
		}, nil
	}
	for i := 0; i < 2; i++ {
		_, err := runTestAssayReview(t, d)
		require.NoError(t, err)
	}
	require.Len(t, assayEvents(t, db), 2)
}

// TestRunAssayReviewRecordsCacheTokens: the prompt-cache accounting the engine
// reports reaches the assay_runs row on BOTH terminal paths.
//
// The failure path is the one worth pinning. A run whose passes died still paid
// to write their prefixes — the provider bills a max-turns session for its
// cache write exactly like one that answered — so a failed run recorded with
// zeros reads back as a run that shared everything, which is the opposite of
// what happened and the direction that hides a regression rather than
// surfacing one. It is the same argument that already puts RunCost on this
// path, and the two travel together.
func TestRunAssayReviewRecordsCacheTokens(t *testing.T) {
	readCache := func(t *testing.T, db *state.DB, runID int) (creation, read int) {
		t.Helper()
		require.NoError(t, db.Conn().QueryRow(
			`SELECT cache_creation_tokens, cache_read_tokens FROM assay_runs WHERE id = ?`, runID,
		).Scan(&creation, &read))
		return creation, read
	}

	t.Run("completed run", func(t *testing.T) {
		d, db := newAssayRunDaemon(t)
		d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
			return &assay.ReviewResult{
				Status: assay.RunStatusComplete, CompletedPasses: 5, TotalPasses: 5,
				CostUSD: 2.8, CacheCreationTokens: 44200, CacheReadTokens: 166000,
				Passes: []assay.PassReport{
					{Name: "logic", CacheCreationTokens: 41500, Primer: true},
					{Name: "security", CacheCreationTokens: 900, CacheReadTokens: 41500},
				},
			}, nil
		}

		run, err := runTestAssayReview(t, d)
		require.NoError(t, err)
		require.Equal(t, 44200, run.CacheCreationTokens)
		require.Equal(t, 166000, run.CacheReadTokens)

		creation, read := readCache(t, db, run.ID)
		require.Equal(t, 44200, creation)
		require.Equal(t, 166000, read)
	})

	t.Run("failed run", func(t *testing.T) {
		d, db := newAssayRunDaemon(t)
		d.assayReview = func(context.Context, assay.ReviewRequest, *state.DB, assay.Config) (*assay.ReviewResult, error) {
			return nil, &assay.RunError{
				CostUSD:             1.75,
				CacheCreationTokens: 41500,
				CacheReadTokens:     900,
				Err:                 errors.New("all assay deep passes failed"),
			}
		}

		run, err := runTestAssayReview(t, d)
		require.NoError(t, err)
		require.Equal(t, state.AssayStatusFailed, run.Status)
		require.Equal(t, 1.75, run.CostUSD)
		require.Equal(t, 41500, run.CacheCreationTokens)
		require.Equal(t, 900, run.CacheReadTokens)

		creation, read := readCache(t, db, run.ID)
		require.Equal(t, 41500, creation)
		require.Equal(t, 900, read)
	})
}
