package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// previewQuestRunTimeout bounds one on-demand preview quest run. Quests drive a
// real browser through a whole flow, and an anvil may declare several, so this
// is generous — it exists to stop a wedged browser holding a goroutine and a
// "running" row forever, not to hurry a legitimate run.
const previewQuestRunTimeout = 30 * time.Minute

// previewQuestRunner is the slice of *questgiver.Monitor the preview quest path
// uses. It is an interface so the dispatch wiring can be exercised without a
// browser, an anvil checkout or a live preview behind it.
type previewQuestRunner interface {
	RunQuestsForPreview(ctx context.Context, anvilID, headSHA, baseURL string) (*questgiver.QuestRunResult, error)
}

var _ previewQuestRunner = (*questgiver.Monitor)(nil)

// previewQuestAnvilNames returns the sorted names of the anvils that opted into
// running their quests against a preview. It is the gating list a client reads
// off the previews payload.
func previewQuestAnvilNames(cfg *config.Config) []string {
	opted := previewQuestAnvils(cfg)
	names := make([]string, 0, len(opted))
	for name := range opted {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// questRuns returns the daemon's preview quest run store, lazily created so a
// daemon assembled in a test (or one where previews are off) still answers
// status reads with "no runs" rather than a nil dereference.
func (d *Daemon) questRuns() *questgiver.RunStore {
	d.questRunMu.Lock()
	defer d.questRunMu.Unlock()
	if d.questRunStore == nil {
		d.questRunStore = questgiver.NewRunStore(0)
	}
	return d.questRunStore
}

// questRunner returns the monitor preview quest runs are dispatched to, or nil
// when QuestGiver is not wired up in this daemon.
func (d *Daemon) questRunner() previewQuestRunner {
	if d.previewQuestRunner != nil {
		return d.previewQuestRunner
	}
	if d.questgiverMonitor == nil {
		return nil
	}
	return d.questgiverMonitor
}

// handlePreviewQuestRun serves the "preview_quest_run" IPC command: run the
// anvil's E2E quests against a bead's live preview.
//
// It answers immediately with a run id and executes on its own goroutine —
// quests are minutes of browser work, and the caller is a dashboard that wants
// to render "running" rather than hold a socket open. Progress is read back
// through "preview_quest_status".
//
// Every gate answers with a rejection reason instead of an IPC error, because
// the two mean different things to a client: a reason is "the button should not
// have been offered", an error is "the daemon broke". Nothing here — and
// nothing downstream of the run — feeds a merge or pipeline decision: quest
// results against a preview are informational, and the only thing that ever
// reads them back is this command's status sibling.
func (d *Daemon) handlePreviewQuestRun(p ipc.PreviewQuestRunPayload) ipc.Response {
	beadID := strings.TrimSpace(p.BeadID)
	if beadID == "" {
		return errorResponse("bead_id is required")
	}
	reject := func(reason, message string) ipc.Response {
		d.logger.Debug("preview quest run rejected", "bead", beadID, "reason", reason)
		return okResponse(ipc.PreviewQuestRunResponse{
			BeadID:  beadID,
			Reason:  reason,
			Message: message,
		})
	}

	mgr := d.previews()
	if mgr == nil {
		return reject(ipc.PreviewQuestRejectDisabled,
			"preview environments are disabled (settings.preview_enabled)")
	}
	env, ok := mgr.Get(beadID)
	if !ok {
		return reject(ipc.PreviewQuestRejectNoPreview,
			fmt.Sprintf("no preview running for bead %s", beadID))
	}
	if !d.cfg.Load().IsPreviewQuestsEnabledForAnvil(env.Anvil) {
		return reject(ipc.PreviewQuestRejectNotEnabled,
			fmt.Sprintf("anvil %q has not opted into preview quests (preview_quests)", env.Anvil))
	}
	// A preview that is starting, degraded or failed produces browser failures
	// that say nothing about the branch, so it is refused rather than reported
	// as a failing run.
	if status := env.Status(); status != state.PreviewRunning {
		return reject(ipc.PreviewQuestRejectNotHealthy,
			fmt.Sprintf("preview for %s is %s, not healthy", beadID, status))
	}
	baseURL := strings.TrimSpace(env.EntryURL())
	if baseURL == "" {
		return reject(ipc.PreviewQuestRejectNoEntryURL,
			fmt.Sprintf("preview for %s has no entry service URL to point quests at", beadID))
	}
	runner := d.questRunner()
	if runner == nil {
		return reject(ipc.PreviewQuestRejectUnavailable,
			"QuestGiver is not running in this daemon")
	}
	runs := d.questRuns()
	if runs.Running(beadID) {
		return reject(ipc.PreviewQuestRejectAlreadyRunning,
			fmt.Sprintf("a quest run for %s is already in flight", beadID))
	}

	// The head the preview's detached checkout is actually at. It is what
	// disambiguates this bead's preview from a sibling of the same anvil inside
	// RunQuestsForPreview's own lookup; an unresolvable HEAD falls back to the
	// empty string, which matches the anvil's only preview.
	headSHA := d.previewHeadSHA(env)

	run := runs.Begin(questgiver.BeginOptions{
		BeadID:    beadID,
		Anvil:     env.Anvil,
		PreviewID: kiln.SanitizePreviewID(beadID),
		HeadSHA:   headSHA,
		BaseURL:   baseURL,
	})
	anvil := env.Anvil
	d.logger.Info("preview quest run started",
		"bead", beadID, "anvil", anvil, "run", run.RunID, "base_url", baseURL)

	go func() {
		// Detached from the IPC command's context (which ends with the
		// response) and bounded on its own, since the browser work outlives
		// every request that could have carried it.
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(d.runCtx), previewQuestRunTimeout)
		defer cancel()
		res, err := runner.RunQuestsForPreview(runCtx, anvil, headSHA, baseURL)
		runs.Complete(run.RunID, res, err)
		switch {
		case err != nil:
			d.logger.Warn("preview quest run failed to complete",
				"bead", beadID, "anvil", anvil, "run", run.RunID, "error", err)
		case res != nil && res.Skipped:
			d.logger.Info("preview quest run skipped",
				"bead", beadID, "anvil", anvil, "run", run.RunID, "reason", res.SkipReason)
		default:
			d.logger.Info("preview quest run finished",
				"bead", beadID, "anvil", anvil, "run", run.RunID,
				"passed", res != nil && res.Passed, "quests", len(res.Quests))
		}
		if d.db != nil {
			_ = d.db.LogEvent(state.EventPreviewQuestRunDone,
				fmt.Sprintf("preview quest run %s for %s: %s", run.RunID, beadID, questRunStatus(runs, run.RunID)),
				beadID, anvil)
		}
	}()

	started := previewQuestRun(run)
	return okResponse(ipc.PreviewQuestRunResponse{
		Started: true,
		BeadID:  beadID,
		RunID:   run.RunID,
		Message: fmt.Sprintf("running %s quests against the preview for %s", anvil, beadID),
		Run:     &started,
	})
}

// questRunStatus reads a run's current status for logging, tolerating a run
// that was evicted while it executed.
func questRunStatus(runs *questgiver.RunStore, runID string) string {
	if run, ok := runs.Get(runID); ok {
		return run.Status
	}
	return "unknown"
}

// previewHeadSHA resolves the commit a preview's detached checkout is at, or ""
// when it cannot be determined. It is best-effort by design: an unknown head
// only widens the preview lookup to "this anvil's preview", which for the bead
// we already resolved a preview for is the same answer.
func (d *Daemon) previewHeadSHA(env *kiln.Environment) string {
	if env == nil || env.WorktreePath == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.runCtx), 10*time.Second)
	defer cancel()
	head, err := worktree.HeadSHA(ctx, env.WorktreePath)
	if err != nil {
		d.logger.Debug("could not resolve preview checkout HEAD for quest run",
			"bead", env.BeadID, "path", env.WorktreePath, "error", err)
		return ""
	}
	return strings.TrimSpace(head)
}

// handlePreviewQuestStatus serves the "preview_quest_status" IPC command: one
// run by id, or a bead's most recent run. A miss is a successful "not found"
// rather than an error, because "this bead has never had a quest run" is the
// normal state of most beads.
func (d *Daemon) handlePreviewQuestStatus(p ipc.PreviewQuestStatusPayload) ipc.Response {
	runID := strings.TrimSpace(p.RunID)
	beadID := strings.TrimSpace(p.BeadID)
	if runID == "" && beadID == "" {
		return errorResponse("run_id or bead_id is required")
	}

	runs := d.questRuns()
	var (
		run questgiver.Run
		ok  bool
	)
	if runID != "" {
		run, ok = runs.Get(runID)
		// A run id that belongs to a different bead is treated as a miss: the
		// caller asked about this bead's run and must not be handed another's.
		if ok && beadID != "" && run.BeadID != beadID {
			ok = false
		}
	} else {
		run, ok = runs.Latest(beadID)
	}
	if !ok {
		return okResponse(ipc.PreviewQuestStatusResponse{})
	}
	out := previewQuestRun(run)
	return okResponse(ipc.PreviewQuestStatusResponse{Found: true, Run: &out})
}

// previewQuestRun maps a stored run onto the IPC payload: durations become
// seconds (JSON has no duration) and a zero finish time becomes null (the run
// is still going) rather than the zero instant.
func previewQuestRun(run questgiver.Run) ipc.PreviewQuestRun {
	out := ipc.PreviewQuestRun{
		RunID:           run.RunID,
		BeadID:          run.BeadID,
		Anvil:           run.Anvil,
		PreviewID:       run.PreviewID,
		HeadSHA:         run.HeadSHA,
		BaseURL:         run.BaseURL,
		Status:          run.Status,
		SkipReason:      run.SkipReason,
		Error:           run.Error,
		StartedAt:       run.StartedAt,
		DurationSeconds: run.Duration.Seconds(),
		Quests:          make([]ipc.PreviewQuestOutcome, 0, len(run.Quests)),
	}
	if !run.FinishedAt.IsZero() {
		finished := run.FinishedAt
		out.FinishedAt = &finished
	}
	for _, q := range run.Quests {
		out.Quests = append(out.Quests, ipc.PreviewQuestOutcome{
			Name:            q.Name,
			Passed:          q.Passed,
			FailedStep:      q.FailedStep,
			ErrorMessage:    q.ErrorMessage,
			DurationSeconds: q.Duration.Seconds(),
			FilePath:        q.FilePath,
			Screenshots:     q.Screenshots,
		})
	}
	return out
}
