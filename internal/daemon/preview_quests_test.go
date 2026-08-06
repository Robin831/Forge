package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreviewQuestAnvils_Gating covers the set QuestGiver's preview run path is
// handed: the per-anvil opt-in plus the preview gates it depends on.
func TestPreviewQuestAnvils_Gating(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"quests":   {Path: "/tmp/quests", PreviewQuests: true},
			"plain":    {Path: "/tmp/plain"},
			"optedOut": {Path: "/tmp/optedout", PreviewQuests: true, PreviewEnabled: boolPtr(false)},
			"pathless": {Path: "", PreviewQuests: true},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
	require.Equal(t, map[string]string{"quests": "/tmp/quests"}, previewQuestAnvils(cfg))

	// The global gate wins, and a nil config answers rather than panics.
	cfg.Settings.PreviewEnabled = false
	require.Empty(t, previewQuestAnvils(cfg))
	require.Empty(t, previewQuestAnvils(nil))
}

// TestShaMatches pins how a PR head (often abbreviated) is matched against the
// full SHA a preview checkout reports.
func TestShaMatches(t *testing.T) {
	const full = "abcdef1234567890abcdef1234567890abcdef12"

	require.True(t, shaMatches(full, full))
	require.True(t, shaMatches(full, "abcdef1"), "a 7-char abbreviation matches by prefix")
	require.True(t, shaMatches(full, "ABCDEF1234"), "matching is case-insensitive")
	require.True(t, shaMatches(full, "  abcdef1  "), "surrounding whitespace is ignored")

	require.False(t, shaMatches(full, "abcdef"), "a fragment shorter than 7 must match exactly")
	require.False(t, shaMatches(full, "beefbeef"))
	require.False(t, shaMatches(full, ""))
	require.False(t, shaMatches("", full))
}

// --- on-demand quest runs against a preview --------------------------------

// fakePreviewInstance is a kiln.Instance that reports a fixed status and entry
// URL, which is what the quest-run gates read off an environment.
type fakePreviewInstance struct {
	status   string
	entryURL string
}

func (f fakePreviewInstance) Stop() error      { return nil }
func (f fakePreviewInstance) Status() string   { return f.status }
func (f fakePreviewInstance) EntryURL() string { return f.entryURL }
func (f fakePreviewInstance) Ports() []int     { return nil }
func (f fakePreviewInstance) Record() state.Preview {
	return state.Preview{Status: f.status}
}

// stubQuestRunner stands in for the QuestGiver monitor. It blocks on `release`
// until the test lets it finish, which is how the dispatch test proves the IPC
// handler answered without waiting for the browser work.
type stubQuestRunner struct {
	mu      sync.Mutex
	calls   []questCall
	release chan struct{}
	entered chan struct{}
	result  *questgiver.QuestRunResult
	err     error
}

type questCall struct {
	anvil   string
	headSHA string
	baseURL string
}

func newStubQuestRunner() *stubQuestRunner {
	return &stubQuestRunner{
		release: make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
}

func (s *stubQuestRunner) RunQuestsForPreview(ctx context.Context, anvilID, headSHA, baseURL string) (*questgiver.QuestRunResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, questCall{anvil: anvilID, headSHA: headSHA, baseURL: baseURL})
	s.mu.Unlock()
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return s.result, s.err
}

func (s *stubQuestRunner) recorded() []questCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]questCall(nil), s.calls...)
}

// questDaemon builds a daemon with a live preview for beadID whose status and
// entry URL are what the gates will read, plus the given runner.
func questDaemon(t *testing.T, cfg *config.Config, beadID, status, entryURL string, runner previewQuestRunner) (*Daemon, *fakePreviewManager) {
	t.Helper()
	mgr := newFakePreviewManager()
	if beadID != "" {
		env := &kiln.Environment{BeadID: beadID, Anvil: "forge", Branch: "forge/" + beadID}
		env.AttachInstance(fakePreviewInstance{status: status, entryURL: entryURL})
		mgr.envs = map[string]*kiln.Environment{beadID: env}
	}
	d := newPreviewAPIDaemon(t, cfg, mgr)
	d.previewQuestRunner = runner
	return d, mgr
}

// questConfig is previewConfig plus the per-anvil preview_quests opt-in.
func questConfig(optedIn bool) *config.Config {
	return &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"forge": {Path: "/tmp/forge", PreviewQuests: optedIn},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
}

func decodeRunResponse(t *testing.T, resp ipc.Response) ipc.PreviewQuestRunResponse {
	t.Helper()
	require.Equal(t, "ok", resp.Type, "payload: %s", string(resp.Payload))
	var out ipc.PreviewQuestRunResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	return out
}

// TestHandlePreviewQuestRun_Gates covers every reason the action is refused.
// Each is a successful command carrying a reason code, not an IPC error: the
// web layer turns the two opt-in/health gates into a 403, and it can only do
// that if it can tell a refusal from a broken daemon.
func TestHandlePreviewQuestRun_Gates(t *testing.T) {
	t.Run("anvil never opted in", func(t *testing.T) {
		d, _ := questDaemon(t, questConfig(false), "Forge-abc1",
			state.PreviewRunning, "http://box:42001/", newStubQuestRunner())

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		assert.False(t, out.Started)
		assert.Equal(t, ipc.PreviewQuestRejectNotEnabled, out.Reason)
		assert.Empty(t, out.RunID)
	})

	t.Run("preview is not healthy", func(t *testing.T) {
		for _, status := range []string{state.PreviewStarting, state.PreviewDegraded, state.PreviewFailed} {
			runner := newStubQuestRunner()
			d, _ := questDaemon(t, questConfig(true), "Forge-abc1", status, "http://box:42001/", runner)

			out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
			assert.False(t, out.Started, "status %s", status)
			assert.Equal(t, ipc.PreviewQuestRejectNotHealthy, out.Reason, "status %s", status)
			assert.Empty(t, runner.recorded(), "no browser should be driven at a %s preview", status)
		}
	})

	t.Run("bead has no preview", func(t *testing.T) {
		d, _ := questDaemon(t, questConfig(true), "", "", "", newStubQuestRunner())

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		assert.Equal(t, ipc.PreviewQuestRejectNoPreview, out.Reason)
	})

	t.Run("previews are disabled", func(t *testing.T) {
		d := newPreviewAPIDaemon(t, questConfig(true), nil)

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		assert.Equal(t, ipc.PreviewQuestRejectDisabled, out.Reason)
	})

	t.Run("questgiver is not wired up", func(t *testing.T) {
		d, _ := questDaemon(t, questConfig(true), "Forge-abc1",
			state.PreviewRunning, "http://box:42001/", nil)

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		assert.Equal(t, ipc.PreviewQuestRejectUnavailable, out.Reason)
	})

	t.Run("preview has no entry URL", func(t *testing.T) {
		d, _ := questDaemon(t, questConfig(true), "Forge-abc1", state.PreviewRunning, "", newStubQuestRunner())

		out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
		assert.Equal(t, ipc.PreviewQuestRejectNoEntryURL, out.Reason)
	})

	t.Run("bead id is required", func(t *testing.T) {
		d, _ := questDaemon(t, questConfig(true), "Forge-abc1",
			state.PreviewRunning, "http://box:42001/", newStubQuestRunner())

		resp := d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{})
		assert.Equal(t, "error", resp.Type)
	})
}

// TestHandlePreviewQuestRun_DispatchesAsynchronously is the core of the
// feature: the handler answers with a run id while the browser work is still
// going. The stub blocks until the test releases it, so a handler that waited
// would deadlock the assertions below rather than merely being slow.
func TestHandlePreviewQuestRun_DispatchesAsynchronously(t *testing.T) {
	runner := newStubQuestRunner()
	runner.result = &questgiver.QuestRunResult{
		Anvil:  "forge",
		Passed: true,
		Quests: []questgiver.QuestOutcome{{Name: "login", Passed: true, FailedStep: -1}},
	}
	d, _ := questDaemon(t, questConfig(true), "Forge-abc1",
		state.PreviewRunning, "http://box:42001/", runner)

	out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	require.True(t, out.Started)
	require.NotEmpty(t, out.RunID)
	require.NotNil(t, out.Run)
	assert.Equal(t, questgiver.RunRunning, out.Run.Status,
		"the handler returned before the quests could have finished")

	// The status command agrees while the stub is still blocked.
	status := decodeQuestStatus(t, d.handlePreviewQuestStatus(ipc.PreviewQuestStatusPayload{BeadID: "Forge-abc1"}))
	require.True(t, status.Found)
	assert.Equal(t, questgiver.RunRunning, status.Run.Status)

	// A second dispatch while one is in flight is refused rather than starting
	// a second browser against the same preview.
	second := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	assert.Equal(t, ipc.PreviewQuestRejectAlreadyRunning, second.Reason)

	select {
	case <-runner.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the run goroutine never reached the runner")
	}
	close(runner.release)

	final := awaitQuestRun(t, d, out.RunID, questgiver.RunPassed)
	assert.Len(t, final.Quests, 1)

	calls := runner.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, "forge", calls[0].anvil)
	assert.Equal(t, "http://box:42001/", calls[0].baseURL,
		"the run targets the preview's resolved entry URL")
}

// TestHandlePreviewQuestRun_FailedRunIsRecordedNotBlocking: a failing run lands
// as a `failed` record and nothing else. There is no gate to flip, which is the
// point — the assertion is that the outcome exists only in the run store.
func TestHandlePreviewQuestRun_FailedRunIsRecordedNotBlocking(t *testing.T) {
	runner := newStubQuestRunner()
	runner.result = &questgiver.QuestRunResult{
		Anvil:  "forge",
		Passed: false,
		Quests: []questgiver.QuestOutcome{
			{Name: "login", Passed: true, FailedStep: -1},
			{Name: "checkout", Passed: false, FailedStep: 3, ErrorMessage: "assert failed",
				Screenshots: []string{"/tmp/shot.png"}},
		},
	}
	close(runner.release)
	d, mgr := questDaemon(t, questConfig(true), "Forge-abc1",
		state.PreviewRunning, "http://box:42001/", runner)

	out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	require.True(t, out.Started)

	run := awaitQuestRun(t, d, out.RunID, questgiver.RunFailed)
	require.Len(t, run.Quests, 2)
	assert.False(t, run.Quests[1].Passed)
	assert.Equal(t, 3, run.Quests[1].FailedStep)
	assert.Equal(t, []string{"/tmp/shot.png"}, run.Quests[1].Screenshots)

	// Nothing was done to the preview or the bead: a red run tears nothing
	// down, retries nothing and blocks nothing.
	assert.Empty(t, mgr.stoppedBeads())
	_, stillUp := mgr.Get("Forge-abc1")
	assert.True(t, stillUp)
}

// TestHandlePreviewQuestRun_SkippedRunIsNotAFailure keeps a gate that fired
// inside QuestGiver (the anvil declares no quests, say) out of the red states.
func TestHandlePreviewQuestRun_SkippedRunIsNotAFailure(t *testing.T) {
	runner := newStubQuestRunner()
	runner.result = &questgiver.QuestRunResult{
		Anvil:      "forge",
		Skipped:    true,
		SkipReason: questgiver.SkipReasonNoQuests,
	}
	close(runner.release)
	d, _ := questDaemon(t, questConfig(true), "Forge-abc1",
		state.PreviewRunning, "http://box:42001/", runner)

	out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	run := awaitQuestRun(t, d, out.RunID, questgiver.RunSkipped)
	assert.Equal(t, questgiver.SkipReasonNoQuests, run.SkipReason)
}

// TestHandlePreviewQuestStatus_Lookups pins the two ways a run is addressed and
// the misses that are not errors.
func TestHandlePreviewQuestStatus_Lookups(t *testing.T) {
	runner := newStubQuestRunner()
	runner.result = &questgiver.QuestRunResult{Anvil: "forge", Passed: true}
	close(runner.release)
	d, _ := questDaemon(t, questConfig(true), "Forge-abc1",
		state.PreviewRunning, "http://box:42001/", runner)

	out := decodeRunResponse(t, d.handlePreviewQuestRun(ipc.PreviewQuestRunPayload{BeadID: "Forge-abc1"}))
	awaitQuestRun(t, d, out.RunID, questgiver.RunPassed)

	byID := decodeQuestStatus(t, d.handlePreviewQuestStatus(ipc.PreviewQuestStatusPayload{RunID: out.RunID}))
	require.True(t, byID.Found)
	assert.Equal(t, out.RunID, byID.Run.RunID)

	// A bead that never ran quests is a successful miss.
	none := decodeQuestStatus(t, d.handlePreviewQuestStatus(ipc.PreviewQuestStatusPayload{BeadID: "Forge-zzzz"}))
	assert.False(t, none.Found)

	// A run id belonging to another bead is a miss too, never another bead's run.
	crossed := decodeQuestStatus(t, d.handlePreviewQuestStatus(ipc.PreviewQuestStatusPayload{
		RunID: out.RunID, BeadID: "Forge-zzzz",
	}))
	assert.False(t, crossed.Found)

	assert.Equal(t, "error", d.handlePreviewQuestStatus(ipc.PreviewQuestStatusPayload{}).Type)
}

func decodeQuestStatus(t *testing.T, resp ipc.Response) ipc.PreviewQuestStatusResponse {
	t.Helper()
	require.Equal(t, "ok", resp.Type, "payload: %s", string(resp.Payload))
	var out ipc.PreviewQuestStatusResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	return out
}

// awaitQuestRun polls the status command until the run reaches wantStatus.
func awaitQuestRun(t *testing.T, d *Daemon, runID, wantStatus string) ipc.PreviewQuestRun {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out := decodeQuestStatus(t, d.handlePreviewQuestStatus(ipc.PreviewQuestStatusPayload{RunID: runID}))
		if out.Found && out.Run != nil {
			last = out.Run.Status
			if last == wantStatus {
				return *out.Run
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %q (last %q)", runID, wantStatus, last)
	return ipc.PreviewQuestRun{}
}

// TestPreviewQuestAnvilNames is what the previews payload hands a client to
// gate the "Run quests" action with.
func TestPreviewQuestAnvilNames(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"zeta":  {Path: "/tmp/zeta", PreviewQuests: true},
			"alpha": {Path: "/tmp/alpha", PreviewQuests: true},
			"plain": {Path: "/tmp/plain"},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
	require.Equal(t, []string{"alpha", "zeta"}, previewQuestAnvilNames(cfg))
	require.Empty(t, previewQuestAnvilNames(nil))
}
