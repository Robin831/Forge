package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/selfdeploy"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	skewBuild = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	skewHead  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// skewDaemon builds a daemon whose deploy body is a stub, so the trigger,
// backoff and escalation decisions are exercised without a checkout, a compiler
// or systemd. The channel receives one value per dispatched deploy.
func skewDaemon(t *testing.T, sd config.SelfDeployConfig) (*Daemon, *state.DB, chan config.SelfDeployConfig) {
	t.Helper()
	d, db := newSelfDeployDaemon(t, sd, nil)
	dispatched := make(chan config.SelfDeployConfig, 8)
	d.setSelfDeployRun(func(cfg config.SelfDeployConfig) { dispatched <- cfg })
	return d, db, dispatched
}

func skewOf(commits int, since time.Time) selfdeploy.Skew {
	return selfdeploy.Skew{
		BuildSHA: skewBuild,
		HeadSHA:  skewHead,
		Branch:   "main",
		Commits:  commits,
		Since:    since,
	}
}

func enabledSkewConfig() config.SelfDeployConfig {
	return config.SelfDeployConfig{Enabled: true, Anvil: "forge", Branch: "main"}
}

// waitDispatched drains one dispatched deploy, failing if none arrives.
func waitDispatched(t *testing.T, ch chan config.SelfDeployConfig) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a deploy to be dispatched")
	}
}

// waitDeployIdle blocks until the dispatched deploy's goroutine has released the
// single-flight guard. The stub body returns as soon as its send lands, but the
// flag is cleared by a deferred store afterwards — and escalation is now
// suppressed while a deploy is in flight, so a test that asserts on the entry
// has to know the deploy is over rather than merely dispatched.
func waitDeployIdle(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for d.selfDeployInFlight.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the dispatched deploy never released the single-flight guard")
		}
		time.Sleep(time.Millisecond)
	}
}

func deployFailureFor(t *testing.T, db *state.DB, reason string) *state.DeployFailure {
	t.Helper()
	rows, err := db.DeployFailures()
	require.NoError(t, err)
	for i := range rows {
		if rows[i].Reason == reason {
			return &rows[i]
		}
	}
	return nil
}

// TestApplySelfDeploySkew_BehindTriggersADeploy is the bug: nothing emitted a
// merge event, so nothing deployed. The skew alone has to be enough.
func TestApplySelfDeploySkew_BehindTriggersADeploy(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, dispatched := skewDaemon(t, sd)
	now := time.Now()

	d.applySelfDeploySkew(sd, skewOf(7, now.Add(-5*24*time.Hour)), now)

	waitDispatched(t, dispatched)
	att := d.selfDeploySkewAttempt.Load()
	require.NotNil(t, att)
	assert.Equal(t, skewHead, att.head, "the attempt is recorded against the tip it was dispatched for")
}

// TestApplySelfDeploySkew_AttemptSurvivesARestart: the record guards against a
// deploy that restarts the daemon without putting the merged code live, and an
// in-memory record is cleared by exactly that restart — which would leave a host
// whose binary_path is not the one the unit runs redeploying every few minutes.
func TestApplySelfDeploySkew_AttemptSurvivesARestart(t *testing.T) {
	sd := enabledSkewConfig()
	d, db, dispatched := skewDaemon(t, sd)
	now := time.Now()

	d.applySelfDeploySkew(sd, skewOf(7, now.Add(-5*24*time.Hour)), now)
	waitDispatched(t, dispatched)

	// A fresh daemon over the same state.db is the restarted process.
	restarted := &Daemon{db: db, logger: d.logger}
	restarted.cfg.Store(d.config())
	restartedDispatch := make(chan config.SelfDeployConfig, 4)
	restarted.setSelfDeployRun(func(cfg config.SelfDeployConfig) { restartedDispatch <- cfg })

	restarted.applySelfDeploySkew(sd, skewOf(7, now.Add(-5*24*time.Hour)), now.Add(2*time.Minute))

	assert.Empty(t, restartedDispatch, "the same tip is not re-deployed by the process the last deploy started")
	require.NotNil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew),
		"and the skew that outlived a deploy is reported")
}

// TestApplySelfDeploySkew_CurrentBuildDoesNothing: zero skew must not restart a
// daemon that is already running the merged code.
func TestApplySelfDeploySkew_CurrentBuildDoesNothing(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, dispatched := skewDaemon(t, sd)

	d.applySelfDeploySkew(sd, skewOf(0, time.Time{}), time.Now())

	assert.Empty(t, dispatched)
	assert.Nil(t, d.selfDeploySkewAttempt.Load())
}

// TestApplySelfDeploySkew_SameTipIsNotRedeployedEveryTick: a deploy that could
// not go live for one commit will not go live for it fifteen minutes later
// either, and each attempt pauses dispatch for a whole drain window.
func TestApplySelfDeploySkew_SameTipIsNotRedeployedEveryTick(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, dispatched := skewDaemon(t, sd)
	now := time.Now()

	d.applySelfDeploySkew(sd, skewOf(1, now), now)
	waitDispatched(t, dispatched)

	// A tick later, the same tip: no second dispatch.
	d.applySelfDeploySkew(sd, skewOf(1, now), now.Add(15*time.Minute))
	assert.Empty(t, dispatched)

	// Past the retry interval the same tip is worth one more attempt.
	d.applySelfDeploySkew(sd, skewOf(1, now), now.Add(config.DefaultSelfDeploySkewRetryInterval+time.Minute))
	waitDispatched(t, dispatched)
}

// TestApplySelfDeploySkew_NewTipDeploysImmediately: the backoff is keyed to the
// commit, so a branch that moves again is a new question.
func TestApplySelfDeploySkew_NewTipDeploysImmediately(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, dispatched := skewDaemon(t, sd)
	now := time.Now()

	d.applySelfDeploySkew(sd, skewOf(1, now), now)
	waitDispatched(t, dispatched)

	moved := skewOf(2, now)
	moved.HeadSHA = "cccccccccccccccccccccccccccccccccccccccc"
	d.applySelfDeploySkew(sd, moved, now.Add(time.Minute))
	waitDispatched(t, dispatched)
	assert.Equal(t, moved.HeadSHA, d.selfDeploySkewAttempt.Load().head)
}

// TestApplySelfDeploySkew_InFlightDeployIsNotRecordedAsAnAttempt: the in-flight
// deploy is somebody else's trigger, and recording it here would start this
// path's backoff for a deploy it never dispatched.
func TestApplySelfDeploySkew_InFlightDeployIsNotRecordedAsAnAttempt(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, dispatched := skewDaemon(t, sd)
	require.True(t, d.selfDeployInFlight.CompareAndSwap(false, true))

	d.applySelfDeploySkew(sd, skewOf(4, time.Now()), time.Now())

	assert.Empty(t, dispatched)
	assert.Nil(t, d.selfDeploySkewAttempt.Load())
}

// TestApplySelfDeploySkew_StalledSkewEscalates covers the state nothing could
// previously express: a deploy was attempted, the daemon is still the old build,
// and the skew is past a threshold.
func TestApplySelfDeploySkew_StalledSkewEscalates(t *testing.T) {
	sd := enabledSkewConfig()
	d, db, dispatched := skewDaemon(t, sd)
	now := time.Now()
	since := now.Add(-5 * 24 * time.Hour)

	d.applySelfDeploySkew(sd, skewOf(7, since), now)
	waitDispatched(t, dispatched)
	waitDeployIdle(t, d)
	assert.Nil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew),
		"the first detection deploys; it does not also escalate")

	d.applySelfDeploySkew(sd, skewOf(7, since), now.Add(15*time.Minute))
	row := deployFailureFor(t, db, state.DeployReasonVersionSkew)
	require.NotNil(t, row)
	assert.Equal(t, skewHead, row.AttemptedSHA)
	assert.Contains(t, row.Detail, "7 commit(s) behind origin/main")
	assert.Contains(t, row.Title(), "the running build is behind")
	assert.WithinDuration(t, now, row.FailedAt, time.Second,
		"the entry is stamped with the dispatch, so a refresh does not report an old skew as new")
}

// TestApplySelfDeploySkew_InFlightDeployIsNotEscalated: the entry claims a
// deploy was dispatched and the daemon is STILL the old build, and that is not
// decided while the deploy is running. Its drain alone can take max_drain_wait
// (30m by default) against a 15m check interval, so the tick right after a
// dispatch reliably finds the same skew for the same tip with the deploy working
// exactly as intended — escalating there reports a stall that is not one.
func TestApplySelfDeploySkew_InFlightDeployIsNotEscalated(t *testing.T) {
	sd := enabledSkewConfig()
	d, db, dispatched := skewDaemon(t, sd)
	release := make(chan struct{})
	d.setSelfDeployRun(func(cfg config.SelfDeployConfig) {
		dispatched <- cfg
		<-release
	})
	now := time.Now()
	since := now.Add(-5 * 24 * time.Hour)

	d.applySelfDeploySkew(sd, skewOf(7, since), now)
	waitDispatched(t, dispatched)

	// Still draining: same tip, same skew, well past both thresholds.
	d.applySelfDeploySkew(sd, skewOf(7, since), now.Add(15*time.Minute))
	assert.Nil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew),
		"a deploy that is still running has not failed to close the skew")

	// Once it is over and the build is still behind, the claim holds.
	close(release)
	waitDeployIdle(t, d)
	d.applySelfDeploySkew(sd, skewOf(7, since), now.Add(30*time.Minute))
	assert.NotNil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew))
}

// TestApplySelfDeploySkew_BelowThresholdIsNotEscalated: between a merge and the
// tick that deploys it, being a commit behind is ordinary.
func TestApplySelfDeploySkew_BelowThresholdIsNotEscalated(t *testing.T) {
	sd := enabledSkewConfig()
	d, db, dispatched := skewDaemon(t, sd)
	now := time.Now()

	d.applySelfDeploySkew(sd, skewOf(1, now), now)
	waitDispatched(t, dispatched)
	waitDeployIdle(t, d)
	d.applySelfDeploySkew(sd, skewOf(1, now), now.Add(15*time.Minute))

	assert.Nil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew))
}

// TestApplySelfDeploySkew_AgeAloneEscalates: one merge nothing ever deployed is
// a single commit and a week old, which the commit threshold alone never sees.
func TestApplySelfDeploySkew_AgeAloneEscalates(t *testing.T) {
	sd := enabledSkewConfig()
	d, db, dispatched := skewDaemon(t, sd)
	now := time.Now()
	since := now.Add(-7 * 24 * time.Hour)

	d.applySelfDeploySkew(sd, skewOf(1, since), now)
	waitDispatched(t, dispatched)
	waitDeployIdle(t, d)
	d.applySelfDeploySkew(sd, skewOf(1, since), now.Add(15*time.Minute))

	require.NotNil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew))
}

// TestApplySelfDeploySkew_CurrentBuildWithdrawsTheEntry: a deploy that goes live
// clears it on its own, but an operator who rebuilt by hand leaves nothing else
// to, and a resolved row left standing trains operators to ignore the list.
func TestApplySelfDeploySkew_CurrentBuildWithdrawsTheEntry(t *testing.T) {
	sd := enabledSkewConfig()
	d, db, dispatched := skewDaemon(t, sd)
	now := time.Now()
	since := now.Add(-5 * 24 * time.Hour)

	d.applySelfDeploySkew(sd, skewOf(7, since), now)
	waitDispatched(t, dispatched)
	waitDeployIdle(t, d)
	d.applySelfDeploySkew(sd, skewOf(7, since), now.Add(15*time.Minute))
	require.NotNil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew))

	// Another deploy failure must survive: a current build says nothing about a
	// stash an earlier deploy left behind.
	require.NoError(t, db.RecordDeployFailure(state.DeployFailure{
		Anvil: "forge", Reason: state.DeployReasonStashRetained, Detail: "work in stash@{0}",
	}))

	d.applySelfDeploySkew(sd, skewOf(0, time.Time{}), now.Add(30*time.Minute))

	assert.Nil(t, deployFailureFor(t, db, state.DeployReasonVersionSkew))
	assert.NotNil(t, deployFailureFor(t, db, state.DeployReasonStashRetained))
}

// TestSelfDeploySkewIsStalled_ThresholdsAreIndependentlyDisablable pins the
// negative-disables reading both thresholds share, including that disabling both
// is a configuration and not an oversight the code works around.
func TestSelfDeploySkewIsStalled_ThresholdsAreIndependentlyDisablable(t *testing.T) {
	now := time.Now()
	big := skewOf(9, now.Add(-9*24*time.Hour))

	assert.True(t, selfDeploySkewIsStalled(enabledSkewConfig(), big, now))

	noCommits := enabledSkewConfig()
	noCommits.SkewAttentionCommits = -1
	assert.True(t, selfDeploySkewIsStalled(noCommits, big, now), "the age threshold still applies")

	noAge := enabledSkewConfig()
	noAge.SkewAttentionAge = -1
	assert.True(t, selfDeploySkewIsStalled(noAge, big, now), "the commit threshold still applies")

	off := enabledSkewConfig()
	off.SkewAttentionCommits = -1
	off.SkewAttentionAge = -1
	assert.False(t, selfDeploySkewIsStalled(off, big, now))
}

// TestTriggerSelfDeploy_SingleFlightAcrossBothPaths: the merge event and the
// skew check share one guard, so a skew tick cannot start a second deploy
// alongside the one a merge started.
func TestTriggerSelfDeploy_SingleFlightAcrossBothPaths(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, dispatched := skewDaemon(t, sd)
	release := make(chan struct{})
	d.setSelfDeployRun(func(cfg config.SelfDeployConfig) {
		dispatched <- cfg
		<-release
	})

	assert.True(t, d.triggerSelfDeploy(sd, selfDeployReasonPRMerged, nil))
	waitDispatched(t, dispatched)
	assert.False(t, d.triggerSelfDeploy(sd, selfDeployReasonSkew, nil), "a second trigger is a no-op while one runs")
	close(release)
}

// TestTriggerSelfDeploy_NoRecordWhenNothingIsDispatched: onClaimed is the
// caller's "I dispatched" record — the skew check's retry backoff is keyed on it
// — so the trigger must fire it only on the path that actually launches a
// deploy. A refusal reached after the single-flight CAS releases the guard and
// leaves the record unwritten, exactly as an already-in-flight refusal does;
// firing it there would start a whole retry interval of silence for a deploy
// nobody ran.
func TestTriggerSelfDeploy_NoRecordWhenNothingIsDispatched(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, _ := skewDaemon(t, sd)

	// A substituted body that is nil is the refusal available past the CAS: the
	// deploy goroutine would panic on it, so the trigger declines instead.
	var nilRun func(config.SelfDeployConfig)
	d.selfDeployRun.Store(&nilRun)

	claimed := 0
	assert.False(t, d.triggerSelfDeploy(sd, selfDeployReasonSkew, func() { claimed++ }))
	assert.Zero(t, claimed, "a trigger that dispatched nothing must not record an attempt")
	assert.False(t, d.selfDeployInFlight.Load(), "a refusal past the CAS must release the guard")

	// And the guard it released is genuinely free: the next trigger runs.
	dispatched := make(chan struct{}, 1)
	d.setSelfDeployRun(func(config.SelfDeployConfig) { dispatched <- struct{}{} })
	assert.True(t, d.triggerSelfDeploy(sd, selfDeployReasonSkew, func() { claimed++ }))
	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the next trigger to dispatch")
	}
	assert.Equal(t, 1, claimed, "the dispatching trigger records exactly once")
}

// TestSelfDeploySkewConfigDefaults pins the unset/negative reading: zero is the
// field's zero value, and a deployment that never configured the check is
// exactly the one that silently falls behind.
func TestSelfDeploySkewConfigDefaults(t *testing.T) {
	var sd config.SelfDeployConfig
	assert.Equal(t, config.DefaultSelfDeploySkewCheckInterval, sd.ResolvedSkewCheckInterval())
	assert.Equal(t, config.DefaultSelfDeploySkewRetryInterval, sd.ResolvedSkewRetryInterval())
	assert.Equal(t, config.DefaultSelfDeploySkewAttentionCommits, sd.ResolvedSkewAttentionCommits())
	assert.Equal(t, config.DefaultSelfDeploySkewAttentionAge, sd.ResolvedSkewAttentionAge())

	off := config.SelfDeployConfig{SkewCheckInterval: -time.Second}
	assert.Zero(t, off.ResolvedSkewCheckInterval())

	custom := config.SelfDeployConfig{SkewCheckInterval: time.Hour, SkewAttentionCommits: 10}
	assert.Equal(t, time.Hour, custom.ResolvedSkewCheckInterval())
	assert.Equal(t, 10, custom.ResolvedSkewAttentionCommits())
}

// TestApplySelfDeploySkew_AttemptIsRecordedBeforeTheDeployRuns: the record is
// written under the CAS that claims the single-flight guard, before the deploy
// goroutine exists. Written after triggerSelfDeploy returned it would race the
// deploy it dispatched — a deploy that fails fast (a build error, a blocked
// pull) is exactly the case the backoff exists for, and one that restarts the
// daemon can take the process down before the store lands.
func TestApplySelfDeploySkew_AttemptIsRecordedBeforeTheDeployRuns(t *testing.T) {
	sd := enabledSkewConfig()
	d, _, _ := skewDaemon(t, sd)
	seen := make(chan *selfDeploySkewAttempt, 1)
	d.setSelfDeployRun(func(config.SelfDeployConfig) { seen <- d.selfDeploySkewAttempt.Load() })

	now := time.Now()
	d.applySelfDeploySkew(sd, skewOf(3, now.Add(-time.Hour)), now)

	select {
	case att := <-seen:
		require.NotNil(t, att, "the deploy body must already see the attempt on record")
		assert.Equal(t, skewHead, att.head)
	case <-time.After(2 * time.Second):
		t.Fatal("expected a deploy to be dispatched")
	}
}

// skewLoopProbe runs the loop with sub-second seams and a counting check, and
// returns the counter plus a channel closed when the loop returns.
func skewLoopProbe(t *testing.T, d *Daemon, ctx context.Context) (*atomic.Int32, chan struct{}, chan struct{}) {
	t.Helper()
	var checks atomic.Int32
	fired := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runSelfDeploySkewCheckWith(ctx, selfDeploySkewLoop{
			startupDelay: time.Millisecond,
			disabledPoll: time.Millisecond,
			check: func(context.Context, time.Time) {
				checks.Add(1)
				select {
				case fired <- struct{}{}:
				default:
				}
			},
		})
	}()
	return &checks, fired, done
}

// TestRunSelfDeploySkewCheck_EnableGate covers the three-way conjunction that
// decides whether the check ever runs. A regression in it is silent by
// construction — the feature simply never fires, which is the failure (a daemon
// quietly behind main with nothing said anywhere) the check exists to report.
func TestRunSelfDeploySkewCheck_EnableGate(t *testing.T) {
	tests := []struct {
		name  string
		sd    config.SelfDeployConfig
		check bool
	}{
		{
			name:  "an unset interval runs the check",
			sd:    enabledSkewConfig(),
			check: true,
		},
		{
			name: "a negative interval disables it",
			sd: func() config.SelfDeployConfig {
				sd := enabledSkewConfig()
				sd.SkewCheckInterval = -time.Second
				return sd
			}(),
		},
		{
			name: "self-deploy disabled",
			sd: func() config.SelfDeployConfig {
				sd := enabledSkewConfig()
				sd.Enabled = false
				return sd
			}(),
		},
		{
			name: "no anvil configured",
			sd: func() config.SelfDeployConfig {
				sd := enabledSkewConfig()
				sd.Anvil = ""
				return sd
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := skewDaemon(t, tc.sd)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checks, fired, done := skewLoopProbe(t, d, ctx)

			if tc.check {
				select {
				case <-fired:
				case <-time.After(2 * time.Second):
					t.Fatal("expected the skew check to run")
				}
			} else {
				time.Sleep(50 * time.Millisecond)
				assert.Zero(t, checks.Load(), "the gate must keep the check from running")
			}

			// Whatever the gate decided, the loop idles rather than exiting: an
			// exited goroutine could only be brought back by the restart this
			// feature exists to perform.
			select {
			case <-done:
				t.Fatal("the loop must not exit while its context is live")
			default:
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the loop must return once its context is done")
			}
		})
	}
}

// TestRunSelfDeploySkewCheck_EnabledByHotReload: the loop re-reads the config
// every iteration, which is what makes enabling self-deploy without a restart
// possible — and the thing this loop deploys is the daemon that would otherwise
// need restarting to pick the setting up.
func TestRunSelfDeploySkewCheck_EnabledByHotReload(t *testing.T) {
	off := enabledSkewConfig()
	off.Enabled = false
	// A short interval so the loop re-reads the config promptly; production
	// picks a reload up on the ordinary 15m cadence.
	off.SkewCheckInterval = time.Millisecond
	d, _, _ := skewDaemon(t, off)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks, fired, _ := skewLoopProbe(t, d, ctx)

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, checks.Load())

	on := enabledSkewConfig()
	on.SkewCheckInterval = time.Millisecond
	d.cfg.Store(&config.Config{SelfDeploy: on})

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("a hot-reloaded enable must start the check without a restart")
	}
}
