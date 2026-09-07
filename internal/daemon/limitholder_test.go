package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

func holder(id string) state.Worker {
	return state.Worker{
		ID: id, BeadID: "BD-" + id, Anvil: "anvil-1",
		Status: state.WorkerRunning, Phase: "smith", PID: 4242,
		StartedAt: time.Now().Add(-90 * time.Minute),
	}
}

// TestLimitHolderTracker_WarnsOnlyPastTheThreshold pins the shape of the check:
// nothing is reported while a worker has held the limit for threshold polls or
// fewer, the crossing is reported once, and the polls after it stay silent so a
// wedged worker does not re-announce itself every poll interval forever.
func TestLimitHolderTracker_WarnsOnlyPastTheThreshold(t *testing.T) {
	var tr limitHolderTracker
	const threshold = 3

	for poll := 1; poll <= threshold; poll++ {
		assert.Empty(t, tr.observe([]state.Worker{holder("w-1")}, threshold),
			"poll %d is still within the threshold", poll)
	}

	wedged := tr.observe([]state.Worker{holder("w-1")}, threshold)
	require.Len(t, wedged, 1, "the poll past the threshold must report the holder")
	assert.Equal(t, "w-1", wedged[0].Worker.ID)
	assert.Equal(t, threshold+1, wedged[0].Polls)

	for poll := 0; poll < 5; poll++ {
		assert.Empty(t, tr.observe([]state.Worker{holder("w-1")}, threshold),
			"an already-announced holder must not be re-announced every poll")
	}
}

// TestLimitHolderTracker_RotatingHoldersNeverWarn is the healthy-saturation
// case: a busy forge fills the cap on every poll, but with a different worker
// each time, so no single one has held it and nothing is reported. This is the
// whole reason the tracker keys on identity and not on the blocked condition.
func TestLimitHolderTracker_RotatingHoldersNeverWarn(t *testing.T) {
	var tr limitHolderTracker
	const threshold = 3

	for poll := 0; poll < 10; poll++ {
		id := "w-" + string(rune('a'+poll))
		assert.Empty(t, tr.observe([]state.Worker{holder(id)}, threshold),
			"a forge whose slots turn over every poll is saturated, not wedged")
	}
}

// TestLimitHolderTracker_ReleaseResetsTheRun proves a worker that stops holding
// the limit starts a fresh count, so an intermittently-full forge never
// accumulates its way to a WARN, and a recurrence after the condition clears is
// announced again rather than being swallowed as already-reported.
func TestLimitHolderTracker_ReleaseResetsTheRun(t *testing.T) {
	var tr limitHolderTracker
	const threshold = 2

	for poll := 0; poll < 5; poll++ {
		tr.observe([]state.Worker{holder("w-1")}, threshold)
	}
	tr.release()

	for poll := 1; poll <= threshold; poll++ {
		assert.Empty(t, tr.observe([]state.Worker{holder("w-1")}, threshold),
			"the count must restart from zero after a release")
	}
	assert.Len(t, tr.observe([]state.Worker{holder("w-1")}, threshold), 1,
		"a recurrence after the limit cleared is news again")
}

// TestLimitHolderTracker_DisabledThresholdKeepsCounting covers the off switch:
// a threshold of 0 or less reports nothing, but the counting continues, so
// hot-reloading the setting back on does not restart every run from zero.
func TestLimitHolderTracker_DisabledThresholdKeepsCounting(t *testing.T) {
	var tr limitHolderTracker

	for poll := 0; poll < 5; poll++ {
		assert.Empty(t, tr.observe([]state.Worker{holder("w-1")}, 0),
			"a disabled threshold must report nothing")
	}
	wedged := tr.observe([]state.Worker{holder("w-1")}, 3)
	require.Len(t, wedged, 1, "re-enabling must use the count that was accruing all along")
	assert.Equal(t, 6, wedged[0].Polls)
}

// TestLimitHolderTracker_OneHolderAmongManyIsStillWedged: the limit can be
// filled by several workers, only one of which has stopped moving. The one that
// repeats is reported; the ones that turn over are not.
func TestLimitHolderTracker_OneHolderAmongManyIsStillWedged(t *testing.T) {
	var tr limitHolderTracker
	const threshold = 2

	for poll := 0; poll < 4; poll++ {
		holders := []state.Worker{holder("w-stuck"), holder("w-" + string(rune('a'+poll)))}
		wedged := tr.observe(holders, threshold)
		if poll < threshold {
			assert.Empty(t, wedged, "poll %d is still within the threshold", poll)
			continue
		}
		if poll == threshold {
			require.Len(t, wedged, 1)
			assert.Equal(t, "w-stuck", wedged[0].Worker.ID,
				"only the worker that repeats across polls is wedged")
			continue
		}
		assert.Empty(t, wedged, "no holder should be announced twice")
	}
}

// TestReportGlobalLimitReached_KeepsINFOAndAddsWARN checks the two log lines
// against the daemon's own logger: ordinary saturation stays at INFO on every
// poll (nothing that reads that line changes), and the WARN naming the worker,
// its bead, phase and age is additional rather than a replacement.
func TestReportGlobalLimitReached_KeepsINFOAndAddsWARN(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	d.cfg.Store(&config.Config{Settings: config.SettingsConfig{WedgedLimitPolls: 2}})

	holders := []state.Worker{holder("w-1")}

	for poll := 0; poll < 2; poll++ {
		d.reportGlobalLimitReached(holders, 1)
	}
	assert.Equal(t, 2, strings.Count(buf.String(), "global smith limit reached, skipping dispatch"),
		"ordinary saturation must keep its INFO line on every poll")
	assert.NotContains(t, buf.String(), "it may be wedged",
		"nothing should be escalated while the threshold has not been crossed")

	buf.Reset()
	d.reportGlobalLimitReached(holders, 1)
	out := buf.String()
	assert.Contains(t, out, "global smith limit reached, skipping dispatch",
		"the INFO line is not replaced by the WARN")
	assert.Contains(t, out, "it may be wedged")
	assert.Contains(t, out, "worker=w-1")
	assert.Contains(t, out, "bead=BD-w-1")
	assert.Contains(t, out, "phase=smith")
	assert.Contains(t, out, "age=")
}

// TestReportGlobalLimitReached_NegativeSettingDisablesTheWARN covers the
// explicit off switch. 0 is the field's zero value and therefore means "unset"
// (take the default), so only a negative value can turn the check off.
func TestReportGlobalLimitReached_NegativeSettingDisablesTheWARN(t *testing.T) {
	var buf bytes.Buffer
	d := &Daemon{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	d.cfg.Store(&config.Config{Settings: config.SettingsConfig{WedgedLimitPolls: -1}})

	for poll := 0; poll < 20; poll++ {
		d.reportGlobalLimitReached([]state.Worker{holder("w-1")}, 1)
	}
	assert.NotContains(t, buf.String(), "it may be wedged")
}

// TestLimitHolderTracker_ConcurrentObserveIsSafe: observe and release are
// reached from the poll goroutine, but pollAndDispatch is only try-locked
// against itself, so the tracker owns its own mutex. Exercised so the race
// detector has something to fail on if that ever stops being true.
func TestLimitHolderTracker_ConcurrentObserveIsSafe(t *testing.T) {
	var tr limitHolderTracker
	var done atomic.Int64
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				tr.observe([]state.Worker{holder("w-1")}, 3)
				tr.release()
			}
			done.Add(1)
		}()
	}
	require.Eventually(t, func() bool { return done.Load() == 8 }, 10*time.Second, 5*time.Millisecond)
}
