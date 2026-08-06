package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubQuestReporter records the reports the daemon hands it instead of touching
// GitHub, so the wiring (which PR, which head, which result) can be asserted on.
type stubQuestReporter struct {
	mu   sync.Mutex
	reqs []questgiver.ReportRequest
	err  error
}

func (s *stubQuestReporter) ReportPreviewQuestResults(_ context.Context, req questgiver.ReportRequest) (*questgiver.ReportResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return &questgiver.ReportResult{Created: true, CommentID: 7}, nil
}

func (s *stubQuestReporter) recorded() []questgiver.ReportRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]questgiver.ReportRequest(nil), s.reqs...)
}

// awaitReports polls until the reporter has been called want times, or fails.
func awaitReports(t *testing.T, rep *stubQuestReporter, want int) []questgiver.ReportRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := rep.recorded(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("reporter was called %d times, wanted %d", len(rep.recorded()), want)
	return nil
}

// reportingQuestDaemon is questDaemon plus a real state.DB and a stub reporter,
// which is the minimum needed to exercise "run finishes → report lands on PR".
func reportingQuestDaemon(t *testing.T, runner previewQuestRunner) (*Daemon, *stubQuestReporter) {
	t.Helper()
	d, _ := questDaemon(t, questConfig(true), "Forge-abc1",
		state.PreviewRunning, "http://box:42001/", runner)
	d.db = newTestDB(t)
	rep := &stubQuestReporter{}
	d.previewQuestReporter = rep
	return d, rep
}

// insertOpenPR registers an open PR for the bead so the reporter has somewhere
// to post.
func insertOpenPR(t *testing.T, d *Daemon, number int, beadID string) {
	t.Helper()
	require.NoError(t, d.db.InsertPR(&state.PR{
		Number:    number,
		Anvil:     "forge",
		BeadID:    beadID,
		Branch:    "forge/" + beadID,
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}))
}

// TestPreviewQuestRun_ReportsToOpenPR is the hand-off this bead adds: a run that
// reached a verdict is handed to the reporter with the bead's open PR and the
// head the preview was at.
func TestPreviewQuestRun_ReportsToOpenPR(t *testing.T) {
	runner := newStubQuestRunner()
	runner.result = &questgiver.QuestRunResult{
		Anvil:  "forge",
		Passed: false,
		Quests: []questgiver.QuestOutcome{
			{Name: "checkout", Passed: false, FailedStep: 1, ErrorMessage: "boom"},
		},
	}
	close(runner.release)

	d, rep := reportingQuestDaemon(t, runner)
	insertOpenPR(t, d, 42, "Forge-abc1")

	out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	require.True(t, out.Started)
	awaitQuestRun(t, d, out.RunID, questgiver.RunFailed)

	reqs := awaitReports(t, rep, 1)
	require.Len(t, reqs, 1)
	assert.Equal(t, 42, reqs[0].PRNumber)
	assert.Equal(t, "Forge-abc1", reqs[0].BeadID)
	assert.Equal(t, "forge", reqs[0].Anvil)
	assert.Equal(t, "/tmp/forge", reqs[0].WorktreePath, "gh runs from the anvil checkout")
	require.NotNil(t, reqs[0].Result)
	assert.False(t, reqs[0].Result.Passed)
}

// TestPreviewQuestRun_SkippedAndUnreportableRunsPostNothing covers the three
// ways the report is silently declined: the run was gated, the bead has no open
// PR, or the run never reached a verdict.
func TestPreviewQuestRun_SkippedAndUnreportableRunsPostNothing(t *testing.T) {
	t.Run("skipped run", func(t *testing.T) {
		runner := newStubQuestRunner()
		runner.result = &questgiver.QuestRunResult{
			Anvil: "forge", Skipped: true, SkipReason: questgiver.SkipReasonNoQuests,
		}
		close(runner.release)
		d, rep := reportingQuestDaemon(t, runner)
		insertOpenPR(t, d, 42, "Forge-abc1")

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		awaitQuestRun(t, d, out.RunID, questgiver.RunSkipped)
		assert.Empty(t, rep.recorded(), "a gated run says nothing about the branch")
	})

	t.Run("no open PR", func(t *testing.T) {
		runner := newStubQuestRunner()
		runner.result = &questgiver.QuestRunResult{Anvil: "forge", Passed: true}
		close(runner.release)
		d, rep := reportingQuestDaemon(t, runner)

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		awaitQuestRun(t, d, out.RunID, questgiver.RunPassed)
		assert.Empty(t, rep.recorded())
	})

	t.Run("run errored", func(t *testing.T) {
		runner := newStubQuestRunner()
		runner.err = errors.New("quest files unreadable")
		close(runner.release)
		d, rep := reportingQuestDaemon(t, runner)
		insertOpenPR(t, d, 42, "Forge-abc1")

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		awaitQuestRun(t, d, out.RunID, questgiver.RunError)
		assert.Empty(t, rep.recorded(), "a run that fell over never exercised the branch")
	})
}

// TestPreviewQuestRun_ReportFailureIsSwallowed: a report that could not be
// posted leaves the run exactly as it was. Nothing about a preview quest result
// is allowed to propagate into the run record, the preview or the PR state.
func TestPreviewQuestRun_ReportFailureIsSwallowed(t *testing.T) {
	runner := newStubQuestRunner()
	runner.result = &questgiver.QuestRunResult{
		Anvil:  "forge",
		Passed: true,
		Quests: []questgiver.QuestOutcome{{Name: "login", Passed: true, FailedStep: -1}},
	}
	close(runner.release)

	d, rep := reportingQuestDaemon(t, runner)
	rep.err = errors.New("gh: 503")
	insertOpenPR(t, d, 42, "Forge-abc1")

	out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	run := awaitQuestRun(t, d, out.RunID, questgiver.RunPassed)
	awaitReports(t, rep, 1)

	assert.Empty(t, run.Error, "a failed report must not mark the run as errored")
}

// TestOpenPRForBead pins which PR a report is aimed at: the bead's own, on the
// right anvil, latest first, and never a merged or closed one.
func TestOpenPRForBead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &Daemon{logger: testLogger(), runCtx: ctx, db: newTestDB(t)}
	d.cfg.Store(&config.Config{})

	insert := func(number int, anvil, bead string, status state.PRStatus) {
		require.NoError(t, d.db.InsertPR(&state.PR{
			Number: number, Anvil: anvil, BeadID: bead, Branch: "b",
			Status: status, CreatedAt: time.Now(),
		}))
	}
	insert(10, "forge", "Forge-abc1", state.PROpen)
	insert(11, "forge", "Forge-abc1", state.PROpen)
	insert(12, "other", "Forge-abc1", state.PROpen)
	insert(13, "forge", "Forge-zzzz", state.PROpen)
	insert(14, "forge", "Forge-merged", state.PRMerged)

	got := d.openPRForBead("forge", "Forge-abc1")
	require.NotNil(t, got)
	assert.Equal(t, 11, got.Number, "the newest open PR wins")

	assert.Nil(t, d.openPRForBead("forge", "Forge-merged"), "a merged PR is not reported on")
	assert.Nil(t, d.openPRForBead("forge", "Forge-nope"))
	assert.Nil(t, d.openPRForBead("forge", ""))

	// A daemon with no DB answers rather than panicking.
	assert.Nil(t, (&Daemon{logger: testLogger()}).openPRForBead("forge", "Forge-abc1"))
}
