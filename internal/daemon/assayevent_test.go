package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

func newAssayEventDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, db
}

// assayEvents returns the terminal Assay events currently in the feed, oldest
// first.
func assayEvents(t *testing.T, db *state.DB) []state.Event {
	t.Helper()
	all, err := db.RecentEvents(50)
	require.NoError(t, err)
	var out []state.Event
	for i := len(all) - 1; i >= 0; i-- {
		switch all[i].Type {
		case state.EventAssayCompleted, state.EventAssayPartial, state.EventAssayFailed:
			out = append(out, all[i])
		}
	}
	return out
}

// TestEmitAssayTerminalEventPerOutcome: every finished run closes itself out in
// the feed with exactly one event of the right type. Before this, a complete
// run — the common case — emitted nothing, so an operator watching Hearth saw
// pr_review_needed and never learned the outcome.
func TestEmitAssayTerminalEventPerOutcome(t *testing.T) {
	tests := []struct {
		name        string
		run         *state.AssayRun
		wantType    state.EventType
		wantMessage string
	}{
		{
			"complete",
			&state.AssayRun{
				Anvil: "forge", PRNumber: 347, Status: state.AssayStatusComplete,
				CompletedPasses: 5, TotalPasses: 5, FindingsCount: 7,
				CostUSD: 2.8, DurationMs: 152000,
			},
			state.EventAssayCompleted,
			"Assay PR #347: complete — 5/5 passes, 7 findings ($2.80, 152s)",
		},
		{
			"complete in shadow mode says so",
			&state.AssayRun{
				Anvil: "forge", PRNumber: 4767, Status: state.AssayStatusComplete,
				CompletedPasses: 5, TotalPasses: 5, FindingsCount: 3,
				CostUSD: 1.5, DurationMs: 100000, ShadowMode: true,
			},
			state.EventAssayCompleted,
			"Assay PR #4767: complete — 5/5 passes, 3 findings ($1.50, 100s) (shadow — findings in panel only)",
		},
		{
			"partial",
			&state.AssayRun{
				Anvil: "forge", PRNumber: 347, Status: state.AssayStatusPartial,
				CompletedPasses: 3, TotalPasses: 5, FindingsCount: 4,
				FailedPasses: []state.AssayPassFailure{
					{Name: "logic", Reason: "error_max_turns"},
					{Name: "repo-specific", Reason: "error_max_turns"},
				},
				CostUSD: 1.2, DurationMs: 90000,
				Error: "assay pass logic: provider claude failed (exit 1, subtype error_max_turns)",
			},
			state.EventAssayPartial,
			"Assay PR #347: partial — 3/5 passes (failed: logic, repo-specific — error_max_turns), 4 findings ($1.20, 90s)",
		},
		{
			"failed carries the cause",
			&state.AssayRun{
				Anvil: "forge", PRNumber: 347, Status: state.AssayStatusFailed,
				CostUSD: 0.4, DurationMs: 30000,
				Error: "all assay deep passes failed",
			},
			state.EventAssayFailed,
			"Assay PR #347: failed — all assay deep passes failed ($0.40, 30s)",
		},
		{
			// The diff-fetch failure path records its cause as a skip reason
			// rather than an error string; the feed must still name it.
			"failed falls back to the skip reason",
			&state.AssayRun{
				Anvil: "forge", PRNumber: 348, Status: state.AssayStatusFailed,
				SkippedReason: "diff fetch failed",
			},
			state.EventAssayFailed,
			"Assay PR #348: failed — diff fetch failed ($0.00, 0s)",
		},
		{
			// A run that died before the engine set a status is still a run
			// that reviewed nothing — never a completion.
			"unset status is failed",
			&state.AssayRun{Anvil: "forge", PRNumber: 349},
			state.EventAssayFailed,
			"Assay PR #349: failed — no passes reviewed the head ($0.00, 0s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, db := newAssayEventDaemon(t)
			d.emitAssayTerminalEvent(tt.run, "Forge-abc1")

			evs := assayEvents(t, db)
			require.Len(t, evs, 1, "expected exactly one terminal event per run")
			require.Equal(t, tt.wantType, evs[0].Type)
			require.Equal(t, tt.wantMessage, evs[0].Message)
			require.Equal(t, "Forge-abc1", evs[0].BeadID)
			require.Equal(t, "forge", evs[0].Anvil)
		})
	}
}

// TestEmitAssayTerminalEventNilRun: a nil run has nothing to report, and a
// fabricated "failed" row for it would be worse than silence.
func TestEmitAssayTerminalEventNilRun(t *testing.T) {
	d, db := newAssayEventDaemon(t)
	d.emitAssayTerminalEvent(nil, "Forge-abc1")
	require.Empty(t, assayEvents(t, db))
}

// TestAssayTerminalEventTypes pins the status→event mapping. Anything that is
// not a recognised complete/partial status must land on failed: a status the
// engine adds later would otherwise be announced as a completion.
func TestAssayTerminalEventTypes(t *testing.T) {
	require.Equal(t, state.EventAssayCompleted, assayTerminalEvent(state.AssayStatusComplete))
	require.Equal(t, state.EventAssayPartial, assayTerminalEvent(state.AssayStatusPartial))
	require.Equal(t, state.EventAssayFailed, assayTerminalEvent(state.AssayStatusFailed))
	require.Equal(t, state.EventAssayFailed, assayTerminalEvent(""))
	require.Equal(t, state.EventAssayFailed, assayTerminalEvent("future"))
}

// TestEnginePassFailuresRoundTrip verifies the two failed-pass types survive the
// round trip the terminal event makes: the engine's list is persisted on the run
// record and converted back to render the event, so the passes the PR comment
// names and the passes the feed names are the same passes.
func TestEnginePassFailuresRoundTrip(t *testing.T) {
	engine := []assay.PassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "rate_limited"},
	}
	require.Equal(t, engine, enginePassFailures(statePassFailures(engine)))
	require.Nil(t, enginePassFailures(nil))
}
