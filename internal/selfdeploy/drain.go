package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultMaxDrainWait bounds the drain wait when Config.MaxDrainWait is unset.
	DefaultMaxDrainWait = 30 * time.Minute
	// DefaultDrainInterval is how often the drain check is re-run while waiting.
	DefaultDrainInterval = 10 * time.Second
)

// ErrDrainTimeout is the sentinel returned (and an EventSkipped emitted) when
// workers were still active after the whole drain wait elapsed.
//
// A drain timeout defers the deploy rather than failing it: nothing has been
// touched at that point, so the next merge — or the next manual rebuild — simply
// tries again. Callers should treat it as "not now" and match it with
// errors.Is; the concrete error is a *DrainTimeoutError carrying the elapsed
// time and the workers that held the deploy up.
var ErrDrainTimeout = errors.New("selfdeploy: workers did not drain within the maximum drain wait")

// DrainTimeoutError reports why a drain wait gave up: how long it waited, the
// budget it was given, and which workers were still active on the final check.
// The worker set is what an operator needs to decide whether to wait longer,
// stop a wedged worker, or raise max_drain_wait.
type DrainTimeoutError struct {
	// Elapsed is the wall time actually spent waiting.
	Elapsed time.Duration
	// Max is the budget the wait was given.
	Max time.Duration
	// Workers identifies the workers still active on the last check.
	Workers []string
}

func (e *DrainTimeoutError) Error() string {
	return ErrDrainTimeout.Error() + ": " + e.Summary()
}

// Summary describes the give-up without the sentinel prefix, for log and event
// text that already says the deploy was deferred.
func (e *DrainTimeoutError) Summary() string {
	return fmt.Sprintf("%d worker(s) still active after %s (max %s): %s",
		len(e.Workers), e.Elapsed.Round(time.Second), e.Max, strings.Join(e.Workers, ", "))
}

// Unwrap lets callers match the sentinel with errors.Is(err, ErrDrainTimeout).
func (e *DrainTimeoutError) Unwrap() error { return ErrDrainTimeout }

// waitForDrain blocks until no worker is active, the max wait elapses, or ctx is
// cancelled while it is still waiting. It returns nil once the forge is idle
// (including on the very first check, whatever ctx's state), a
// *DrainTimeoutError (which
// unwraps to ErrDrainTimeout) when the budget is spent with workers still
// running, a wrapped ctx error on cancellation, or a wrapped check error when
// the worker query never succeeded.
//
// The check is retried on a ticker rather than sampled once: a deploy is
// triggered by a merge, which is exactly when a Smith is most likely to still be
// mid-run, and a single busy sample would throw away a deploy that only needed a
// few more minutes. The caller pauses dispatch first so the active set can only
// shrink, and the check that lets the deploy through is the last thing to run
// before the pull/build/swap — so it doubles as the old atomic guard.
func (d *Deployer) waitForDrain(ctx context.Context, max time.Duration) error {
	if d.activeWorkers == nil {
		return nil // check disabled (tests)
	}
	if max <= 0 {
		max = DefaultMaxDrainWait
	}
	interval := d.cfg.DrainInterval
	if interval <= 0 {
		interval = DefaultDrainInterval
	}
	// Never sleep past the deadline: with a max wait shorter than the poll
	// interval we still want one check, then a prompt give-up.
	if interval > max {
		interval = max
	}

	start := d.now()
	deadline := start.Add(max)

	tick, stop := d.newTicker(interval)
	defer stop()

	var (
		lastActive []string
		lastErr    error
	)
	for {
		// The check runs before ctx is consulted: cancellation only aborts the
		// *waiting*, never a deploy that had nothing to wait for. Deploy is
		// deliberately callable with an already-cancelled caller context (the
		// daemon shutting itself down is how a restart is requested), so an
		// idle forge must still fall straight through to the swap.
		active, err := d.activeWorkers()
		switch {
		case err != nil:
			// A failed check is not evidence that the forge is idle, so keep
			// polling: a transient state.db error must not either abort the
			// deploy or wave it through.
			lastErr = err
		case len(active) == 0:
			return nil
		default:
			lastActive, lastErr = active, nil
		}

		if now := d.now(); !now.Before(deadline) {
			if lastErr != nil {
				return d.fail("worker drain check failed for the whole %s drain wait: %v", max, lastErr)
			}
			timeout := &DrainTimeoutError{Elapsed: now.Sub(start), Max: max, Workers: lastActive}
			d.emit(EventSkipped, "deploy deferred: "+timeout.Summary())
			// A deferral leaves the binary untouched, so there is nothing to roll
			// back — but it also means the merged change is still not running,
			// which is invisible unless it is said out loud.
			d.raiseAttention(DeployEvent{
				Reason:    ReasonDrainTimeout,
				Detail:    timeout.Summary(),
				Timestamp: now,
			})
			return timeout
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("selfdeploy: drain wait cancelled: %w", ctx.Err())
		case <-tick:
		}
	}
}

// realTicker is the production newTicker: a plain time.Ticker behind the
// channel/stop pair the drain loop consumes, so tests can drive polls
// deterministically without sleeping.
func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
