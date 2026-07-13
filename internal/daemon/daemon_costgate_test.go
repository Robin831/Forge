package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCostGateDaemon builds a minimal Daemon with a fresh state DB and the given
// cost settings, suitable for exercising the daily_cost_limit gate helpers.
func newCostGateDaemon(t *testing.T, limit, perWorkerEstimate float64) (*Daemon, *state.DB) {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{
		Settings: config.SettingsConfig{
			DailyCostLimit:        limit,
			PerWorkerCostEstimate: perWorkerEstimate,
		},
	}
	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(cfg)
	return d, db
}

// TestCostGate_ConcurrentDispatchStaysWithinLimit simulates the poll-loop
// dispatch loop: it repeatedly consults costGateAllows and, when allowed,
// reserves a per-worker estimate — exactly what pollAndDispatch does before each
// dispatch. It asserts that dispatch halts so the projected (recorded + reserved)
// spend stays at/near the limit, overshooting by at most one per-worker estimate,
// instead of every ready bead dispatching off a single stale once-per-poll check.
func TestCostGate_ConcurrentDispatchStaysWithinLimit(t *testing.T) {
	const limit = 10.0
	const estimate = 2.0
	d, _ := newCostGateDaemon(t, limit, estimate)

	today := time.Now().Format("2006-01-02")

	var reservations []uint64
	// Simulate a large batch of ready beads; the gate must cut dispatch off.
	for i := 0; i < 100; i++ {
		allowed, projected, err := d.costGateAllows(d.cfg.Load(), today)
		require.NoError(t, err)
		if !allowed {
			// Blocking projection must never exceed the limit by more than one
			// estimate (the estimate for the worker we declined to dispatch).
			assert.LessOrEqual(t, projected, limit+estimate)
			break
		}
		reservations = append(reservations, d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load())))
	}

	// With a $10 limit and a $2 estimate, at most 5 workers may be reserved:
	// projection = 0 + reserved + 2 must stay <= 10, so reserved tops out at $8
	// before the check, then the 5th reservation brings it to $10.
	assert.Equal(t, 5, len(reservations), "dispatch should halt at limit/estimate workers")
	assert.InDelta(t, limit, d.totalReservedCost(), 0.0001,
		"total reserved spend should sit at (not far past) the limit")
}

// TestCostGate_RecordedSpendBlocksImmediately verifies that already-recorded
// spend at/over the limit blocks dispatch on the very first check, with no
// reservations made.
func TestCostGate_RecordedSpendBlocksImmediately(t *testing.T) {
	d, db := newCostGateDaemon(t, 10.0, 2.0)
	today := time.Now().Format("2006-01-02")
	require.NoError(t, db.AddDailyCost(today, 0, 0, 0, 0, 9.50))

	// recorded 9.50 + reserved 0 + estimate 2 = 11.50 > 10 → blocked.
	allowed, projected, err := d.costGateAllows(d.cfg.Load(), today)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.InDelta(t, 11.50, projected, 0.0001)
}

// TestCostGate_ReservationReleasedResumesDispatch verifies that releasing
// reservations (which happens on both worker completion and failure via
// dispatchBead's defer) lets the gate allow dispatch again.
func TestCostGate_ReservationReleasedResumesDispatch(t *testing.T) {
	const limit = 10.0
	const estimate = 5.0
	d, _ := newCostGateDaemon(t, limit, estimate)
	today := time.Now().Format("2006-01-02")

	// Reserve two workers ($5 each) → reserved $10; next projection is
	// 0 + 10 + 5 = 15 > 10 → blocked.
	r1 := d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load()))
	r2 := d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load()))
	allowed, _, err := d.costGateAllows(d.cfg.Load(), today)
	require.NoError(t, err)
	assert.False(t, allowed, "gate must block while both reservations are in flight")

	// One worker completes/fails → its reservation is released. Now projection
	// is 0 + 5 + 5 = 10 <= 10 → allowed again.
	d.releaseWorkerCost(r1)
	allowed, _, err = d.costGateAllows(d.cfg.Load(), today)
	require.NoError(t, err)
	assert.True(t, allowed, "gate must resume dispatch once a reservation is released")

	d.releaseWorkerCost(r2)
	assert.Equal(t, 0.0, d.totalReservedCost(), "all reservations released")

	// Releasing an unknown / zero key is a harmless no-op.
	d.releaseWorkerCost(0)
	d.releaseWorkerCost(r1)
	assert.Equal(t, 0.0, d.totalReservedCost())
}

// TestPerWorkerCostEstimate_FloorAndRollingAverage verifies the estimate is
// never zero (uses the configured floor / package default before any data) and
// tracks the rolling average of recorded per-bead cost once it exceeds the floor.
func TestPerWorkerCostEstimate_FloorAndRollingAverage(t *testing.T) {
	// No configured floor → package default is used before any samples.
	d, _ := newCostGateDaemon(t, 10.0, 0)
	assert.InDelta(t, config.DefaultPerWorkerCostEstimate, d.perWorkerCostEstimate(d.cfg.Load()), 0.0001)

	// Configured floor overrides the package default.
	d.cfg.Store(&config.Config{Settings: config.SettingsConfig{DailyCostLimit: 10.0, PerWorkerCostEstimate: 3.0}})
	assert.InDelta(t, 3.0, d.perWorkerCostEstimate(d.cfg.Load()), 0.0001)

	// Samples below the floor do not drag the estimate below it.
	d.recordBeadCostSample(1.0)
	d.recordBeadCostSample(2.0)
	assert.InDelta(t, 3.0, d.perWorkerCostEstimate(d.cfg.Load()), 0.0001)

	// A high sample pulls the rolling average above the floor and the estimate
	// tracks it. Average of (1, 2, 9) = 4.0 > floor 3.0.
	d.recordBeadCostSample(9.0)
	assert.InDelta(t, 4.0, d.perWorkerCostEstimate(d.cfg.Load()), 0.0001)

	// Non-positive samples are ignored (no divide-by-nothing, no drift).
	d.recordBeadCostSample(0)
	d.recordBeadCostSample(-5)
	assert.InDelta(t, 4.0, d.perWorkerCostEstimate(d.cfg.Load()), 0.0001)
}

// TestCostGate_DisabledLimitAlwaysAllows verifies a zero/negative limit disables
// the gate regardless of reserved spend.
func TestCostGate_DisabledLimitAlwaysAllows(t *testing.T) {
	d, db := newCostGateDaemon(t, 0, 2.0)
	today := time.Now().Format("2006-01-02")
	require.NoError(t, db.AddDailyCost(today, 0, 0, 0, 0, 1000.0))
	d.reserveWorkerCost(500)

	allowed, _, err := d.costGateAllows(d.cfg.Load(), today)
	require.NoError(t, err)
	assert.True(t, allowed, "a limit of 0 disables the cost gate")
}
