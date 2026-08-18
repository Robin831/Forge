package daemon

import (
	"fmt"

	"github.com/Robin831/Forge/internal/lifecycle"
)

// Detaching a PR from Bellows means "stop doing automatic work on it", not
// "brick it". The mute itself lives in the monitor — checkPR returns before its
// first emit for a PR carrying prs.bellows_detached — but that only closes the
// door new events walk through. This file closes the other two the daemon owns:
// the dispatch path, which also runs actions parked before the detach and
// drained after it, and the fix workers already in flight when the operator
// detached.

// detachSuppressedActions are the lifecycle actions a detached PR refuses:
// exactly the ones that spend tokens and push commits to the PR's branch.
//
// ActionCloseBead and ActionCleanup are deliberately absent. They are the
// bookkeeping that follows a merge or a close, and a muted PR still merges —
// bellows keeps persisting its terminal state precisely so that stays true — so
// refusing them would leave a merged bead open and its dependents blocked,
// which is a different failure from the one detaching is meant to stop.
var detachSuppressedActions = map[lifecycle.Action]bool{
	lifecycle.ActionFixCI:       true,
	lifecycle.ActionFixReview:   true,
	lifecycle.ActionRebase:      true,
	lifecycle.ActionAssayReview: true,
}

// detachKillPhases are the worker phases a detach tears down: the fix workers
// bellows dispatches against a PR's branch. The synthetic bellows monitor row
// ('bellows'/'ready_to_merge') is not one of them — it is a row, not a process,
// and SetBellowsWorkerDetached already moves it to the detached state — and an
// Assay pass is left to finish: it only reads the diff and posts findings, so
// killing it mid-run buys nothing and loses the run it already paid for.
var detachKillPhases = map[string]bool{
	"quench":  true,
	"burnish": true,
	"rebase":  true,
}

// actionBlockedByDetach reports whether req must be dropped because its PR is
// detached from Bellows. It is the single check behind both dispatch sites —
// handleLifecycleAction and drainPendingAction — so an action parked while the
// PR was attached cannot slip through by being drained after it was detached.
//
// A manual action always passes: detach mutes the automatic loop, so
// `forge assay run`, `forge queue run` and the dashboard's fix buttons remain
// the operator's way of running one pass by hand on a muted PR. Detach means
// "stop automatic work", not "brick the PR".
//
// It fails OPEN — a PR row that cannot be read dispatches unchecked rather than
// silently dropping automatic work. Bellows read the same flag before emitting
// this action, so a wrong answer here needs two faults in a row, and the flag
// is re-read on the next cycle either way.
func (d *Daemon) actionBlockedByDetach(req lifecycle.ActionRequest) bool {
	if req.IsManual || !detachSuppressedActions[req.Action] {
		return false
	}
	if d.db == nil || req.Anvil == "" || req.PRNumber <= 0 {
		return false
	}
	pr, err := d.db.GetPRByNumber(req.Anvil, req.PRNumber)
	if err != nil {
		d.logger.Warn("could not read bellows_detached for lifecycle action; dispatching unchecked",
			"action", req.Action, "pr", req.PRNumber, "anvil", req.Anvil, "error", err)
		return false
	}
	if pr == nil {
		return false
	}
	return pr.BellowsDetached
}

// killLifecycleWorkersForPR terminates every in-flight quench/burnish/rebase
// worker for the PR and returns the ids it killed. Detaching a PR whose fix
// worker is mid-session would otherwise still push a commit to the branch the
// operator just took off the automatic loop — the mute would only take effect
// once that session happened to end.
//
// A PR with nothing running is a success, not an error: the caller wants the PR
// quiet, and it already is. Each kill goes through killWorkerProcess, the same
// path the kill_worker verb uses — interrupt the process group, wait out the
// grace period, force-kill, mark the row failed — so a detached worker dies the
// way an operator-killed one does rather than being marked failed while its
// claude session keeps running.
func (d *Daemon) killLifecycleWorkersForPR(anvil string, prNumber int) []string {
	if d.db == nil || anvil == "" || prNumber <= 0 {
		return nil
	}
	workers, err := d.db.ActiveWorkers()
	if err != nil {
		d.logger.Warn("detach: could not list active workers to stop",
			"pr", prNumber, "anvil", anvil, "error", err)
		return nil
	}
	var killed []string
	for _, w := range workers {
		if w.Anvil != anvil || w.PRNumber != prNumber || !detachKillPhases[w.Phase] {
			continue
		}
		if err := d.killWorkerProcess(w.ID); err != nil {
			d.logger.Warn("detach: failed to stop in-flight fix worker",
				"worker", w.ID, "phase", w.Phase, "pr", prNumber, "anvil", anvil, "error", err)
			continue
		}
		d.logger.Info("detach: stopped in-flight fix worker",
			"worker", w.ID, "phase", w.Phase, "pr", prNumber, "anvil", anvil)
		killed = append(killed, w.ID)
	}
	return killed
}

// detachKillSummary renders the event message for the workers a detach stopped.
// Returns "" when nothing was running, which is the common case and not worth a
// row in the activity feed.
func detachKillSummary(prNumber int, killed []string) string {
	if len(killed) == 0 {
		return ""
	}
	return fmt.Sprintf("PR #%d: stopped %d in-flight fix worker(s) on detach: %v", prNumber, len(killed), killed)
}
