package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/selfdeploy"
	"github.com/Robin831/Forge/internal/state"
)

// Periodic version-skew check.
//
// handleSelfDeploy is triggered by a bellows pr_merged event, and that event is
// not emitted for every merge: an `ext-*` PR is not bellows-managed, so a PR
// merged by hand produces nothing for the handler to accept. Between the deploy
// of 2026-09-02 and 2026-09-07, PRs #891-#896 were all merged that way and the
// running daemon fell seven commits and five days behind origin/main with no
// deploy, no failure and no entry anywhere — the trigger did exactly what it was
// written to do, and what it was written to do was read "a PR merged" as "main
// moved". A direct push, a merge from the GitHub UI and a merge landed while the
// daemon was down are all the same hole.
//
// So the branch tip is compared against the running build directly, on a timer,
// and a difference takes the SAME drain-and-rebuild path a merge event takes
// (triggerSelfDeploy): same single-flight guard, same drain, same stash
// discipline, same rollback. Never a bare restart.
//
// What the check adds beyond triggering is the report of its own failure: a
// deploy that does not go live leaves the skew in place, so once one has been
// attempted for a given tip and the daemon is still running the old build, the
// skew itself becomes a Needs Attention entry (ReasonVersionSkew). That is the
// state nothing could previously express — every deploy surface reports what a
// deploy did, and here the problem was that no deploy had happened at all.

const (
	// selfDeploySkewStartupDelay keeps the first check out of the startup burst.
	// A deploy that has just restarted the daemon is at the tip anyway, so the
	// first check has nothing to find; what the delay buys is that the log's
	// first page belongs to the poll and the first Bellows cycle.
	selfDeploySkewStartupDelay = 2 * time.Minute

	// selfDeploySkewDisabledPoll is how long the loop idles between config reads
	// while the check is disabled (a negative skew_check_interval). The loop
	// does not exit on a disabled check — an exited goroutine could only be
	// brought back by the restart this feature exists to perform — but a
	// disabled check is the one case with no configured cadence of its own, so
	// waking on the 15m default would be an operator's `-1` having no
	// observable effect on the loop at all. The price is that re-enabling by hot
	// reload takes effect within the hour rather than within the quarter.
	selfDeploySkewDisabledPoll = time.Hour

	// selfDeploySkewGitTimeout bounds one check's git work (a fetch plus a few
	// reads). It is its own deadline rather than the daemon's run context alone
	// because a fetch against an unreachable remote otherwise holds the loop
	// open until shutdown, and the next tick would find it still running.
	selfDeploySkewGitTimeout = 5 * time.Minute
)

// selfDeploySkewAttempt records one skew-triggered deploy: the branch tip it was
// dispatched for and when. Both halves are needed — the tip because a deploy
// that cannot go live for one commit will not go live for it on the next tick
// either, and the time because a drain that timed out might.
type selfDeploySkewAttempt struct {
	head string
	at   time.Time
}

// selfDeploySkewLoop carries the loop's seams so its enable gate is reachable
// from a test: the loop is the only thing that ever calls checkSelfDeploySkew,
// and a regression in that gate is silent by construction — the feature simply
// never fires, which is the very failure (a daemon quietly behind main with
// nothing said anywhere) the check exists to report. A zero field takes the
// production value.
type selfDeploySkewLoop struct {
	// startupDelay keeps the first check out of the startup burst.
	startupDelay time.Duration
	// disabledPoll is the idle cadence while the check is disabled.
	disabledPoll time.Duration
	// check measures and acts on the skew once.
	check func(context.Context, time.Time)
}

// runSelfDeploySkewCheck is the blocking loop behind the periodic check. Launch
// it as a goroutine; it returns only when ctx is done.
func (d *Daemon) runSelfDeploySkewCheck(ctx context.Context) {
	d.runSelfDeploySkewCheckWith(ctx, selfDeploySkewLoop{})
}

// runSelfDeploySkewCheckWith is runSelfDeploySkewCheck with its seams supplied.
//
// The interval is read from the live config on every iteration rather than
// captured once, so a hot-reloaded interval — or self-deploy being enabled at
// all — takes effect without a restart. While the check is disabled the loop
// idles at selfDeploySkewDisabledPoll instead of exiting, since an exited
// goroutine could only be brought back by the restart this feature exists to
// perform.
func (d *Daemon) runSelfDeploySkewCheckWith(ctx context.Context, loop selfDeploySkewLoop) {
	if loop.startupDelay <= 0 {
		loop.startupDelay = selfDeploySkewStartupDelay
	}
	if loop.disabledPoll <= 0 {
		loop.disabledPoll = selfDeploySkewDisabledPoll
	}
	if loop.check == nil {
		loop.check = d.checkSelfDeploySkew
	}

	if !waitOrDone(ctx, loop.startupDelay) {
		return
	}

	for {
		sd := d.config().SelfDeploy
		// One read, two readings of the same value: ResolvedSkewCheckInterval
		// returns 0 only for an explicitly negative setting (the off switch,
		// since 0 is the field's zero value and has to mean "unset"), so it
		// gates the check AND supplies the cadence when it is positive.
		interval := sd.ResolvedSkewCheckInterval()
		if interval > 0 && sd.Enabled && sd.Anvil != "" {
			loop.check(ctx, time.Now())
		}
		wait := interval
		if wait <= 0 {
			wait = loop.disabledPoll
		}
		if !waitOrDone(ctx, wait) {
			return
		}
	}
}

// waitOrDone sleeps for d, reporting false when ctx ended first.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// checkSelfDeploySkew measures the running build against the deploy branch once
// and acts on the result. Measurement failures are reported and never acted on:
// a skew that could not be measured is not a skew of zero (which would restore
// the silence the check exists to break) and not a skew of one (which would
// restart the daemon on a number nobody computed).
func (d *Daemon) checkSelfDeploySkew(ctx context.Context, now time.Time) {
	sd := d.config().SelfDeploy
	if !sd.Enabled || sd.Anvil == "" {
		return
	}
	anvilCfg, ok := d.config().Anvils[sd.Anvil]
	if !ok || anvilCfg.Path == "" {
		d.logger.Warn("self-deploy skew check: configured anvil not found or missing path",
			"anvil", sd.Anvil)
		return
	}

	gitCtx, cancel := context.WithTimeout(ctx, selfDeploySkewGitTimeout)
	defer cancel()

	checker := selfdeploy.NewSkewChecker(selfdeploy.SkewConfig{
		RepoPath: sd.ResolvedRepoPath(anvilCfg.Path),
		Branch:   sd.ResolvedBranch(),
		// The build stamp of the process making the comparison IS the question:
		// whatever is on disk or in the checkout, this is the code running.
		BuildSHA: forge.Build,
	}, selfdeploy.ExecCommander{})

	skew, err := checker.Check(gitCtx)
	if err != nil {
		d.reportSelfDeploySkewError(err, sd)
		return
	}
	d.applySelfDeploySkew(sd, skew, now)
}

// reportSelfDeploySkewError logs a failed measurement at the level its cause
// deserves. None of them deploy and none of them escalate: an unidentified build
// or a checkout that has never seen it is a fact about how the binary was made,
// and a fetch failure is transient by nature. A cancelled context is the daemon
// shutting down and is not a failure at all.
func (d *Daemon) reportSelfDeploySkewError(err error, sd config.SelfDeployConfig) {
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, selfdeploy.ErrBuildUnidentified):
		// A binary built without a VCS stamp cannot be placed in the branch's
		// history at all, so the check is inert for it. Said once per tick it
		// would be wallpaper; said at debug it is there when somebody asks why
		// the check never fires.
		d.logger.Debug("self-deploy skew check: the running build carries no commit id; skipping",
			"anvil", sd.Anvil, "build", forge.Build, "error", err)
	case errors.Is(err, selfdeploy.ErrBuildNotInCheckout), errors.Is(err, selfdeploy.ErrBuildNotAncestor):
		d.logger.Warn("self-deploy skew check: the running build cannot be placed on the deploy branch",
			"anvil", sd.Anvil, "build", forge.Build, "branch", sd.ResolvedBranch(), "error", err)
	default:
		d.logger.Warn("self-deploy skew check failed", "anvil", sd.Anvil, "error", err)
	}
}

// applySelfDeploySkew decides what a measured skew means. It is separate from
// the measurement so the whole decision — trigger, back off, escalate, clear —
// is reachable without git.
func (d *Daemon) applySelfDeploySkew(sd config.SelfDeployConfig, skew selfdeploy.Skew, now time.Time) {
	if !skew.Behind() {
		// Current. Drop the backoff (the next skew is a fresh question) and
		// withdraw any stalled entry: a deploy that went live clears it on its
		// own, but an operator who rebuilt by hand leaves nothing else to.
		d.storeSelfDeploySkewAttempt(nil)
		d.clearSelfDeploySkewAttention(sd)
		return
	}

	d.logger.Info("self-deploy: the running build is behind the deploy branch",
		"anvil", sd.Anvil, "branch", skew.Branch, "build", skew.BuildSHA,
		"head", skew.HeadSHA, "commits", skew.Commits,
		"behind_for", skew.Age(now).Round(time.Minute))

	if prev := d.loadSelfDeploySkewAttempt(); prev != nil && prev.head == skew.HeadSHA &&
		!skewRetryDue(prev.at, now, sd.ResolvedSkewRetryInterval()) {
		// A deploy for this exact tip has already run and the daemon is still
		// the old build, so re-dispatching now buys the same outcome at the cost
		// of pausing dispatch for another drain window. Report it instead —
		// which is the whole difference between a deploy that failed loudly and
		// a merge that was never deployed at all.
		d.escalateSelfDeploySkew(sd, skew, prev, now)
		return
	}

	// The record is written from inside the trigger, under the CAS that claims
	// the single-flight guard and before the deploy goroutine exists. Written
	// after the trigger returned it would race the deploy it dispatched: one
	// that fails fast — a build error, a blocked pull, an unresolvable repo path,
	// which is exactly the case the backoff exists for — can finish, and one
	// that restarts the daemon can take the process down, before the store
	// lands.
	attempt := &selfDeploySkewAttempt{head: skew.HeadSHA, at: now}
	if !d.triggerSelfDeploy(sd, selfDeployReasonSkew, func() { d.storeSelfDeploySkewAttempt(attempt) }) {
		// A deploy is already in flight; it pulls this tip or a newer one. The
		// attempt is deliberately NOT recorded — this path dispatched nothing,
		// and recording it would start the backoff for a deploy somebody else's
		// trigger owns.
		return
	}
}

// loadSelfDeploySkewAttempt returns the attempt on record, reading it back from
// state.db when this process has none in memory.
//
// It is read back rather than kept in memory alone because the outcome the
// record guards against is a deploy that restarts the daemon without putting the
// merged code live — a binary_path that is not the one the unit runs is the
// obvious way — and that restart is exactly what clears an in-memory record.
// Without persistence such a host redeploys itself every few minutes forever.
// An unreadable or malformed row is treated as no attempt, which fails towards
// deploying: attempting a deploy that has already run is a bounded cost, while
// declining one that never did is the silence this whole check exists to break.
func (d *Daemon) loadSelfDeploySkewAttempt() *selfDeploySkewAttempt {
	if att := d.selfDeploySkewAttempt.Load(); att != nil {
		return att
	}
	if d.db == nil {
		return nil
	}
	raw, ok, err := d.db.GetSetting(state.SettingSelfDeploySkewAttempt)
	if err != nil || !ok || raw == "" {
		if err != nil {
			d.logger.Warn("self-deploy skew check: could not read the last attempt", "error", err)
		}
		return nil
	}
	head, ts, found := strings.Cut(raw, " ")
	if !found || head == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return nil
	}
	att := &selfDeploySkewAttempt{head: head, at: at}
	d.selfDeploySkewAttempt.Store(att)
	return att
}

// storeSelfDeploySkewAttempt records a dispatched deploy in memory and in
// state.db. A failed write costs the backoff its persistence and nothing else,
// so it is logged rather than returned.
func (d *Daemon) storeSelfDeploySkewAttempt(att *selfDeploySkewAttempt) {
	d.selfDeploySkewAttempt.Store(att)
	if d.db == nil {
		return
	}
	value := ""
	if att != nil {
		value = att.head + " " + att.at.UTC().Format(time.RFC3339)
	}
	if err := d.db.SetSetting(state.SettingSelfDeploySkewAttempt, value); err != nil {
		d.logger.Warn("self-deploy skew check: could not persist the attempt", "error", err)
	}
}

// skewRetryDue reports whether enough time has passed since an attempt for the
// same tip to try it again. A retry interval of 0 disables the retry entirely,
// leaving one attempt per tip.
func skewRetryDue(last, now time.Time, retry time.Duration) bool {
	if retry <= 0 {
		return false
	}
	return !now.Before(last.Add(retry))
}

// escalateSelfDeploySkew raises (or refreshes) the Needs Attention entry for a
// skew a deploy has already been attempted for and failed to close.
//
// It fires only past a threshold, because a skew below one is ordinary: a merge
// lands, the next tick deploys it, and between those two moments the daemon is
// legitimately a commit behind. Two thresholds rather than one, since they catch
// different shapes of the same failure — a burst of merges is many commits and
// minutes old, one merge nothing ever deployed is a single commit and a week
// old, and either threshold alone leaves the other case silent.
func (d *Daemon) escalateSelfDeploySkew(sd config.SelfDeployConfig, skew selfdeploy.Skew, attempt *selfDeploySkewAttempt, now time.Time) {
	if !selfDeploySkewIsStalled(sd, skew, now) {
		return
	}
	if d.selfDeployInFlight.Load() {
		// The deploy dispatched for this tip has not finished. Its drain alone
		// can run for max_drain_wait (30m by default), which is longer than the
		// check interval, so the very next tick after a dispatch finds the same
		// skew for the same tip and takes this branch while the deploy is
		// working exactly as intended. What the entry asserts — that a deploy
		// was dispatched and the daemon is STILL the old build — is not decided
		// until that deploy returns, so nothing is raised and nothing is
		// withdrawn: an entry a previous, finished attempt left standing is
		// still true, and the in-flight deploy is what will clear it.
		d.logger.Debug("self-deploy skew check: a deploy for this tip is still running; not escalating",
			"anvil", sd.Anvil, "head", skew.HeadSHA, "commits", skew.Commits)
		return
	}
	if d.db == nil {
		return
	}
	detail := fmt.Sprintf("%s; a deploy for that commit was dispatched and the daemon is still running %s — see the other self-deploy entries and daemon.log for why it did not go live",
		skew.Summary(now), shortSHA(skew.BuildSHA))
	sink := selfDeployAttentionSink{db: d.db, anvil: sd.Anvil, unit: sd.ResolvedUnitName()}
	if err := sink.EmitNeedsAttention(selfdeploy.DeployEvent{
		Reason: selfdeploy.ReasonVersionSkew,
		// The commit that should be live. RestoredSHA is deliberately left
		// empty: nothing was rolled back, and the running build is named in the
		// detail instead of in a field whose meaning is "after a rollback".
		AttemptedSHA: skew.HeadSHA,
		Detail:       detail,
		Unit:         sd.ResolvedUnitName(),
		BinaryPath:   sd.ResolvedBinaryPath(),
		// The dispatch time and not now: the entry is refreshed on every tick
		// it survives, and a timestamp that moves with the refresh would report
		// a five-day-old skew as one observed a moment ago.
		Timestamp: attempt.at,
	}); err != nil {
		d.logger.Warn("self-deploy skew check: could not record the needs-attention item",
			"anvil", sd.Anvil, "error", err)
		return
	}
	d.logger.Warn("self-deploy: version skew persists after a deploy attempt",
		"anvil", sd.Anvil, "branch", skew.Branch, "build", skew.BuildSHA,
		"head", skew.HeadSHA, "commits", skew.Commits,
		"behind_for", skew.Age(now).Round(time.Minute))
}

// selfDeploySkewIsStalled reports whether a skew has passed either escalation
// threshold. Both disabled means no escalation, which is a deliberate
// configuration and not an oversight to work around.
func selfDeploySkewIsStalled(sd config.SelfDeployConfig, skew selfdeploy.Skew, now time.Time) bool {
	if commits := sd.ResolvedSkewAttentionCommits(); commits > 0 && skew.Commits >= commits {
		return true
	}
	if age := sd.ResolvedSkewAttentionAge(); age > 0 && skew.Age(now) >= age {
		return true
	}
	return false
}

// clearSelfDeploySkewAttention withdraws the stalled-skew entry. It is scoped to
// that one reason: every other deploy entry describes a step of a deploy, and a
// build that happens to be current says nothing about a stash an earlier deploy
// left behind.
func (d *Daemon) clearSelfDeploySkewAttention(sd config.SelfDeployConfig) {
	if d.db == nil {
		return
	}
	n, err := d.db.ClearDeployFailures(sd.Anvil, state.DeployReasonVersionSkew)
	if err != nil {
		d.logger.Warn("self-deploy skew check: could not clear the needs-attention item",
			"anvil", sd.Anvil, "error", err)
		return
	}
	if n > 0 {
		d.logger.Info("self-deploy: the running build is current again; version-skew entry withdrawn",
			"anvil", sd.Anvil, "build", forge.Build)
	}
}
