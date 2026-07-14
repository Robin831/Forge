package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/require"
)

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// When bus_enabled is true, configureEventBus constructs a Bus, wires it into
// the DB, and events flow to subscribers in real time.
func TestConfigureEventBus_Enabled(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{Settings: config.SettingsConfig{
		BusEnabled:    true,
		BusBufferSize: 8,
	}}

	bus := configureEventBus(cfg, db, testLogger())
	require.NotNil(t, bus, "bus should be constructed when enabled")
	require.Same(t, bus, db.Bus(), "constructed bus should be injected into the DB")

	// A logged event must reach a subscriber, confirming the wiring is live.
	ch, unsub := bus.Subscribe()
	defer unsub()
	require.NoError(t, db.LogEvent(state.EventType("test_event"), "hello", "bead-1", "anvil-1"))

	select {
	case ev := <-ch:
		require.Equal(t, "hello", ev.Message)
		require.False(t, ev.GapMarker)
	default:
		t.Fatal("expected published event on the subscriber channel")
	}
}

// When bus_enabled is false, no Bus is constructed, the DB is left without a
// bus (legacy polling), and LogEvent still succeeds without publishing.
func TestConfigureEventBus_Disabled(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{Settings: config.SettingsConfig{BusEnabled: false}}

	bus := configureEventBus(cfg, db, testLogger())
	require.Nil(t, bus, "no bus should be constructed when disabled")
	require.Nil(t, db.Bus(), "DB must have no bus wired when disabled")

	// LogEvent must not panic and must persist even with no bus (Publish no-op).
	require.NoError(t, db.LogEvent(state.EventType("test_event"), "hello", "bead-1", "anvil-1"))
	events, err := db.RecentEvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
}

// An enabled bus with an unset/zero buffer size falls back to the package
// default rather than collapsing to state.NewBus's minimum of 1.
func TestConfigureEventBus_DefaultBufferSize(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{Settings: config.SettingsConfig{
		BusEnabled:    true,
		BusBufferSize: 0,
	}}

	bus := configureEventBus(cfg, db, testLogger())
	require.NotNil(t, bus)

	// Buffer of DefaultBusBufferSize (256) means we can enqueue many events to a
	// non-draining subscriber before any gap marker is emitted. If the buffer had
	// collapsed to 1, gap markers would appear almost immediately.
	ch, unsub := bus.Subscribe()
	defer unsub()
	for i := 0; i < config.DefaultBusBufferSize; i++ {
		require.NoError(t, db.LogEvent(state.EventType("test_event"), "m", "b", "a"))
	}
	require.Len(t, ch, config.DefaultBusBufferSize, "all events should fit before any drop")
	for i := 0; i < config.DefaultBusBufferSize; i++ {
		ev := <-ch
		require.False(t, ev.GapMarker, "no gap marker expected within buffer capacity")
	}
}
