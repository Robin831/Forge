package bellows

import (
	"fmt"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/vcs"
)

// stuckRunThreshold is how long a check may sit queued on the PR head before
// Bellows stops calling CI "pending" and starts calling it stuck. It is
// deliberately generous: GitHub Actions runners routinely queue for several
// minutes, but a run that has not started after half an hour is a platform
// incident. Only queued time counts — see ciResult.OldestQueued.
const stuckRunThreshold = 30 * time.Minute

// ciState is the actionable verdict for a PR's CI checks on its current head.
//
// The distinction that matters for dispatch is pending/stuck vs failed: only
// ciFailed may spawn a quench (CI-fix) worker. A conclusion that belongs to a
// superseded commit, or a head with nothing but unfinished runs, must never
// reach ciFailed — during the GitHub Actions outage of 2026-08-06 that is
// exactly what put four quench workers on a failure that did not exist on the
// PR head (Forge-81kg).
type ciState int

const (
	// ciPassed means every check attributable to the head finished acceptably
	// (or the repo has no checks at all).
	ciPassed ciState = iota
	// ciPending means the head's verdict is not knowable yet: checks are still
	// running, or the only results available belong to superseded commits.
	ciPending
	// ciFailed means at least one completed check on the head failed.
	ciFailed
	// ciStuck means a check on the head has been queued/running past
	// stuckRunThreshold — CI is wedged or the platform is degraded.
	ciStuck
)

func (s ciState) String() string {
	switch s {
	case ciPassed:
		return "passed"
	case ciPending:
		return "pending"
	case ciFailed:
		return "failed"
	case ciStuck:
		return "stuck"
	default:
		return "unknown"
	}
}

// ciResult is the outcome of evaluateCI, carrying enough context for logging
// and for the "CI appears stuck" operator note.
type ciResult struct {
	State ciState
	// HeadSHA is the PR head the verdict was computed for ("" when the
	// platform did not report one).
	HeadSHA string
	// HeadChecks is the number of checks attributed to the head, StaleChecks
	// the number discarded as belonging to a superseded commit.
	HeadChecks  int
	StaleChecks int
	// InFlight is the number of head checks that have not completed.
	InFlight int
	// OldestInFlight is how long the longest-waiting unfinished head check has
	// been queued or running, and OldestInFlightName which check that is. Zero
	// when nothing is in flight or the platform reported no timestamps.
	OldestInFlight     time.Duration
	OldestInFlightName string
	// OldestQueued is the same measure restricted to checks that have not
	// started yet. Only this drives ciStuck: a job that has been *running* for
	// an hour is a slow build, while one that has not been picked up in an hour
	// is a wedged run or a platform incident.
	OldestQueued     time.Duration
	OldestQueuedName string
	// FailedChecks names the completed head checks that failed.
	FailedChecks []string
	// Reason is a human-readable summary for logs and operator notes.
	Reason string
}

// passing reports whether CI is green for the head.
func (r ciResult) passing() bool { return r.State == ciPassed }

// inProgress reports whether the head's CI verdict is still unsettled, which
// includes the stuck case. Callers use it to suppress failure evaluation: a
// stuck or pending head must not look like a failure to the quench dispatch
// path.
func (r ciResult) inProgress() bool { return r.State == ciPending || r.State == ciStuck }

// evaluateCI reduces a PR's check rollup to an actionable verdict for headSHA.
//
// Checks that report a head SHA different from the PR head are discarded: a
// conclusion from a superseded commit says nothing about the code that is
// actually up for merge. Checks with no reported SHA are kept — GitHub's
// statusCheckRollup is already scoped to the head commit and leaves the field
// empty, so treating "unknown" as a mismatch would discard everything.
//
// now is injected so the stuck threshold is testable.
func evaluateCI(headSHA string, checks []vcs.CheckRun, now time.Time) ciResult {
	res := ciResult{HeadSHA: headSHA}

	head := make([]vcs.CheckRun, 0, len(checks))
	for _, c := range checks {
		if headSHA != "" && c.HeadSHA != "" && !strings.EqualFold(c.HeadSHA, headSHA) {
			res.StaleChecks++
			continue
		}
		head = append(head, c)
	}
	res.HeadChecks = len(head)

	if len(head) == 0 {
		if res.StaleChecks > 0 {
			// The head has no runs of its own yet — a push that did not trigger
			// a workflow, or a platform that has not created the runs. Anything
			// we know is about older code, so the head is simply unknown.
			res.State = ciPending
			res.Reason = fmt.Sprintf("no checks for head %s yet; %d result(s) belong to superseded commits",
				shortSHA(headSHA), res.StaleChecks)
			return res
		}
		// No checks at all: the repo has no CI, mirroring PRStatus.CIsPassing.
		res.State = ciPassed
		res.Reason = "no CI checks reported"
		return res
	}

	for _, c := range head {
		if c.InProgress() {
			res.InFlight++
			age := checkAge(c, now)
			if age > res.OldestInFlight {
				res.OldestInFlight = age
				res.OldestInFlightName = c.DisplayName()
			}
			if c.Queued() && age > res.OldestQueued {
				res.OldestQueued = age
				res.OldestQueuedName = c.DisplayName()
			}
			continue
		}
		if !c.Passing() {
			res.FailedChecks = append(res.FailedChecks, c.DisplayName())
		}
	}

	if res.InFlight > 0 {
		if res.OldestQueued >= stuckRunThreshold {
			res.State = ciStuck
			res.Reason = fmt.Sprintf("check %q has been queued for %s without starting on head %s (%d of %d checks unfinished)",
				res.OldestQueuedName, res.OldestQueued.Round(time.Minute), shortSHA(headSHA), res.InFlight, res.HeadChecks)
			return res
		}
		res.State = ciPending
		res.Reason = fmt.Sprintf("%d of %d checks unfinished on head %s", res.InFlight, res.HeadChecks, shortSHA(headSHA))
		return res
	}

	if len(res.FailedChecks) > 0 {
		res.State = ciFailed
		res.Reason = fmt.Sprintf("failing checks on head %s: %s", shortSHA(headSHA), strings.Join(res.FailedChecks, ", "))
		return res
	}

	res.State = ciPassed
	res.Reason = fmt.Sprintf("all %d checks passed on head %s", res.HeadChecks, shortSHA(headSHA))
	return res
}

// checkAge returns how long an unfinished check has been waiting. It is zero
// when the platform reported no start time (GitLab job lists, Gitea statuses)
// so a missing timestamp can never be mistaken for a stuck run, and zero for a
// start time in the future (clock skew between us and the platform).
func checkAge(c vcs.CheckRun, now time.Time) time.Duration {
	started := c.Started()
	if started.IsZero() {
		return 0
	}
	age := now.Sub(started)
	if age < 0 {
		return 0
	}
	return age
}
