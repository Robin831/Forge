package kiln

import (
	"context"
	"time"
)

const (
	// maxReaperInterval caps how often the idle reaper wakes up. Once a minute
	// is fine-grained enough for a timeout measured in tens of minutes, and it
	// keeps a long timeout from being polled pointlessly often.
	maxReaperInterval = time.Minute
	// minReaperInterval floors it, so a deliberately tiny idle timeout (a test,
	// or an operator experimenting) cannot turn the loop into a spin.
	minReaperInterval = time.Second
)

// reaperInterval is how long the reaper sleeps between sweeps for a given idle
// timeout: a quarter of the timeout, so a preview is torn down within ~25% of
// its deadline, clamped to [minReaperInterval, maxReaperInterval].
func reaperInterval(timeout time.Duration) time.Duration {
	interval := timeout / 4
	if interval > maxReaperInterval {
		interval = maxReaperInterval
	}
	if interval < minReaperInterval {
		interval = minReaperInterval
	}
	return interval
}

// RunReaper tears down previews that have gone untouched for longer than
// preview_idle_timeout. A preview holds a worktree, a slice of the port range
// and one of the few concurrency slots, so an operator who opened a link and
// then wandered off must not keep all three forever.
//
// It blocks until ctx is cancelled — the daemon runs it in its own goroutine
// with the run context, so shutdown stops it without any extra plumbing. A zero
// or negative idle timeout disables reaping entirely and RunReaper returns
// immediately.
func (m *Manager) RunReaper(ctx context.Context) {
	timeout := m.cfg.IdleTimeout
	if timeout <= 0 {
		m.logger.Info("kiln: idle preview reaper disabled (preview_idle_timeout is not set)")
		return
	}

	interval := reaperInterval(timeout)
	m.logger.Info("kiln: idle preview reaper started", "idle_timeout", timeout, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.logger.Info("kiln: idle preview reaper stopped")
			return
		case <-ticker.C:
			m.reapOnce(ctx)
		}
	}
}

// reapOnce performs one sweep. It is separate from the ticker loop so tests can
// drive a sweep directly against a fake clock instead of waiting for real time
// to pass.
//
// The registry is snapshotted first (List takes the lock and releases it), and
// Stop is called outside that lock: teardown runs git and a teardown script, and
// holding the registry lock across either would block every other preview
// operation for its duration. A failed teardown is logged and the sweep
// continues — one wedged preview must not keep the others alive.
func (m *Manager) reapOnce(ctx context.Context) {
	timeout := m.cfg.IdleTimeout
	if timeout <= 0 {
		return
	}
	now := m.now()
	for _, env := range m.List() {
		idle := now.Sub(env.LastActive())
		if idle < timeout {
			continue
		}
		m.logger.Info("kiln: reaping idle preview",
			"bead", env.BeadID, "anvil", env.Anvil,
			"idle", idle.Round(time.Second), "idle_timeout", timeout)
		if err := m.Stop(ctx, env.BeadID); err != nil {
			m.logger.Warn("kiln: reaping idle preview failed", "bead", env.BeadID, "error", err)
		}
	}
}
