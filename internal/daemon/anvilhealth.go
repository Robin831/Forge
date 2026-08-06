package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/anvilhealth"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/notify"
	"github.com/Robin831/Forge/internal/state"
)

// wedgedWarnInterval rate-limits the per-anvil WARN line emitted while an anvil
// stays wedged. Full polls are minutes apart, so this mostly matters for anvils
// that stay broken for hours (the 2026-08-05 munin incident held for ~3h40m):
// the condition must remain discoverable in the log without flooding it.
const wedgedWarnInterval = 15 * time.Minute

// checkAnvilHealth probes every configured anvil for an unresolved beads merge
// conflict and reconciles the needs-attention flag accordingly. It runs once per
// FULL poll — a wedge is a minutes-to-hours condition, so paying for it on every
// fast poll would buy nothing.
//
// A healthy anvil costs exactly one dolt_conflicts query and produces no log
// output and no needs-attention entry. Anvils are probed concurrently so the
// added latency is one query, not one per anvil.
//
// The three outcomes are deliberately distinct:
//   - conflicts found  → raise/refresh the flag, WARN
//   - no conflicts     → clear the flag (automatically; no operator action)
//   - query failed     → state UNKNOWN: leave the existing flag untouched, DEBUG
//
// Treating a failed probe as "healthy" would clear a real wedge the moment the
// beads backend hiccuped, so it is never done.
func (d *Daemon) checkAnvilHealth(ctx context.Context, cfg *config.Config) {
	// Probing is the only thing that ever clears a flag, so turning the check
	// off (or removing every anvil) must first release whatever it raised —
	// otherwise a resolved wedge stays pinned in needs-attention and `forge
	// status` forever while dispatch, gated on the same setting, proceeds.
	if !cfg.Settings.IsAnvilHealthCheckEnabled() || len(cfg.Anvils) == 0 {
		d.releaseAllWedgedAnvils()
		return
	}
	checker := d.anvilHealth
	if checker == nil {
		checker = anvilhealth.New()
	}

	// Drop rows for anvils that are no longer configured so a removed anvil
	// cannot keep a stale needs-attention entry alive forever. Only anvils we
	// actually probe are kept: one with no path is never checked, so keeping its
	// row would strand its flag exactly like a removed anvil would.
	names := make([]string, 0, len(cfg.Anvils))
	for name, anvil := range cfg.Anvils {
		if anvil.Path == "" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		// Nothing probeable this cycle; same reasoning as the disabled case.
		d.releaseAllWedgedAnvils()
		return
	}
	if err := d.db.PruneAnvilHealth(names); err != nil {
		d.logger.Debug("anvil health: pruning stale rows failed", "error", err)
	}

	var wg sync.WaitGroup
	for name, anvil := range cfg.Anvils {
		if anvil.Path == "" {
			continue
		}
		wg.Add(1)
		go func(name, path string) {
			defer wg.Done()
			d.checkOneAnvilHealth(ctx, checker, name, path)
		}(name, anvil.Path)
	}
	wg.Wait()
}

// releaseAllWedgedAnvils clears every wedged flag and forgets the WARN
// rate-limit clocks. Used when the check stops running at all, so no flag it
// raised can outlive it. Silent when there was nothing to release.
func (d *Daemon) releaseAllWedgedAnvils() {
	n, err := d.db.ClearAllAnvilWedged()
	if err != nil {
		d.logger.Error("failed to clear wedged anvil flags", "error", err)
		return
	}
	d.wedgedWarned.Range(func(key, _ any) bool {
		d.wedgedWarned.Delete(key)
		return true
	})
	if n > 0 {
		d.logger.Info("anvil health check is not running — cleared wedged flags it can no longer reconcile",
			"cleared", n)
	}
}

// checkOneAnvilHealth probes a single anvil and reconciles its wedged flag.
func (d *Daemon) checkOneAnvilHealth(ctx context.Context, checker *anvilhealth.Checker, name, path string) {
	rep, err := checker.Check(ctx, path)
	if err != nil {
		// Unknown, not healthy. Debug-level because an anvil whose beads backend
		// is not Dolt (or is briefly unreachable) must not turn every poll into
		// a warning; genuine poll failures are already surfaced by the poller.
		d.logger.Debug("anvil health check inconclusive — leaving previous state untouched",
			"anvil", name, "error", err)
		return
	}

	if !rep.Wedged() {
		d.clearWedgedAnvil(name)
		return
	}
	d.recordWedgedAnvil(name, rep)
}

// recordWedgedAnvil persists the wedged state, logs it, and emits an event on
// first detection.
func (d *Daemon) recordWedgedAnvil(name string, rep anvilhealth.Report) {
	health := state.AnvilHealth{
		Anvil:           name,
		Wedged:          true,
		ConflictTables:  rep.TablesSummary(),
		ConflictCount:   int(rep.TotalConflicts),
		Branch:          rep.Branch,
		Ahead:           rep.Ahead,
		Behind:          rep.Behind,
		DivergenceKnown: rep.DivergenceKnown,
		Detail:          rep.Detail(),
	}
	first, detectedAt, err := d.db.MarkAnvilWedged(health)
	if err != nil {
		d.logger.Error("failed to record wedged anvil", "anvil", name, "error", err)
		return
	}

	wedgedFor := time.Duration(0)
	if !detectedAt.IsZero() {
		wedgedFor = time.Since(detectedAt).Round(time.Second)
	}

	// shouldWarnWedged must still run on first detection so the rate-limit clock
	// starts now rather than firing again on the very next full poll.
	if due := d.shouldWarnWedged(name); first || due {
		d.logger.Warn("anvil is wedged — beads database is mid-merge with unresolved conflicts; every bd write against it fails",
			"anvil", name,
			"tables", rep.TablesSummary(),
			"conflicts", rep.TotalConflicts,
			"branch", rep.Branch,
			"divergence", rep.DivergenceSummary(),
			"wedged_for", wedgedFor.String())
	}

	if first {
		msg := fmt.Sprintf("Anvil %s is wedged: %s", name, rep.Detail())
		_ = d.db.LogEvent(state.EventAnvilWedged, msg, "", name)
		// Notify once per wedge, like every other operator-attention condition
		// (bead_failed, orphan_recovery_failed, daily_cost). A wedge blocks every
		// dispatch into the anvil until a human resolves it, so an operator who
		// watches the webhook channel rather than the logs must hear about it —
		// the 2026-08-05 incident was precisely a silent one.
		d.dispatchAnvilNotification(notify.EventAnvilWedged, name, msg)
	}
}

// dispatchAnvilNotification fires an anvil-scoped webhook event. Dispatch itself
// is fire-and-forget (one detached goroutine per target), so this does not block
// the poll and needs no goroutine of its own; a nil dispatcher is a no-op, so an
// unconfigured webhook costs nothing.
func (d *Daemon) dispatchAnvilNotification(event notify.EventType, anvil, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d.dispatcher.Load().Dispatch(ctx, event, "", anvil, msg)
}

// clearWedgedAnvil clears the flag for an anvil whose conflicts are resolved.
// This is fully automatic — a flag that needed manual dismissal would end up
// permanently set and ignored, which is the same as not having it.
func (d *Daemon) clearWedgedAnvil(name string) {
	cleared, err := d.db.ClearAnvilWedged(name)
	if err != nil {
		d.logger.Error("failed to clear wedged anvil flag", "anvil", name, "error", err)
		return
	}
	d.wedgedWarned.Delete(name)
	if !cleared {
		// Already healthy: no entry, no log line.
		return
	}
	d.logger.Info("anvil recovered — beads merge conflicts resolved", "anvil", name)
	msg := fmt.Sprintf("Anvil %s recovered: beads merge conflicts resolved", name)
	_ = d.db.LogEvent(state.EventAnvilRecovered, msg, "", name)
	// Symmetric with the wedge notification: an operator told an anvil was
	// unusable needs to be told when it is usable again.
	d.dispatchAnvilNotification(notify.EventAnvilRecovered, name, msg)
}

// shouldWarnWedged reports whether the periodic "still wedged" WARN is due for
// this anvil, and records the emission when it is.
func (d *Daemon) shouldWarnWedged(name string) bool {
	now := time.Now()
	if last, ok := d.wedgedWarned.Load(name); ok {
		if ts, ok := last.(time.Time); ok && now.Sub(ts) < wedgedWarnInterval {
			return false
		}
	}
	d.wedgedWarned.Store(name, now)
	return true
}

// wedgedAnvilSet returns the currently wedged anvils keyed by name. It is read
// once per dispatch cycle so the gate below costs a single query, not one per
// bead. An empty map is returned when the check is disabled or the lookup fails
// — dispatch is never blocked by a state.db read error.
func (d *Daemon) wedgedAnvilSet() map[string]state.AnvilHealth {
	cfg := d.cfg.Load()
	if cfg != nil && !cfg.Settings.IsAnvilHealthCheckEnabled() {
		return nil
	}
	rows, err := d.db.WedgedAnvils()
	if err != nil {
		d.logger.Debug("wedged-anvil lookup failed", "error", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]state.AnvilHealth, len(rows))
	for _, r := range rows {
		out[r.Anvil] = r
	}
	return out
}

// wedgedAnvilReason renders the dispatch-blocked explanation for a wedged anvil.
func wedgedAnvilReason(h state.AnvilHealth) string {
	msg := fmt.Sprintf("anvil %q is wedged: %s", h.Anvil, h.Detail)
	if dur := h.WedgedFor(); dur > 0 {
		msg += fmt.Sprintf(" (wedged for %s)", dur.Round(time.Second))
	}
	return msg
}

// wedgedAnvilError returns a human-readable reason when the named anvil is
// currently flagged as wedged, or "" when it is usable (or unknown). Callers use
// it to fast-fail a single dispatch with a real explanation instead of accepting
// work that is guaranteed to fail at its first bd write.
func (d *Daemon) wedgedAnvilError(anvil string) string {
	if anvil == "" {
		return ""
	}
	if h, ok := d.wedgedAnvilSet()[anvil]; ok {
		return wedgedAnvilReason(h)
	}
	return ""
}
