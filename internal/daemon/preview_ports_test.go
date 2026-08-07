package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/kiln"
)

// newPreviewPortRangeDaemon is newPreviewDaemon with a readable log sink, so a
// test can assert on what the operator would actually see at daemon start.
func newPreviewPortRangeDaemon(t *testing.T, portRange string) (*Daemon, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	d := &Daemon{logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	cfg := previewConfig(true, nil)
	cfg.Settings.PreviewPortRange = portRange
	d.cfg.Store(cfg)
	d.newPreviewManager = func(context.Context, *config.Config, map[string]string) (previewManager, error) {
		return newFakePreviewManager(), nil
	}
	return d, &logs
}

// TestStartPreviews_WarnsOnEphemeralPortRangeOverlap covers the visible half of
// the fix: a preview range inside the kernel's ephemeral range is the
// precondition for a service dying at bind with "address already in use"
// minutes after Kiln bind-tested the port, and that overlap is invisible after
// the fact — so it is named at start.
func TestStartPreviews_WarnsOnEphemeralPortRangeOverlap(t *testing.T) {
	elo, ehi, ok := kiln.EphemeralPortRange()
	if !ok || ehi-elo < 1 {
		t.Skip("this host does not report a usable ephemeral port range")
	}

	overlapping := fmt.Sprintf("%d-%d", elo, min(elo+100, ehi))
	d, logs := newPreviewPortRangeDaemon(t, overlapping)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.startPreviews(ctx)
	require.NotNil(t, d.previews(), "the warning must not stop previews from starting")

	out := logs.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, overlapping, "the warning names the configured range")
	assert.Contains(t, out, fmt.Sprintf("%d-%d", elo, ehi), "the warning names the host ephemeral range")
	assert.Contains(t, out, "address already in use")
	assert.Contains(t, out, config.DefaultPreviewPortRange, "the warning suggests a safe range")

	cancel()
	d.wg.Wait()
}

// TestStartPreviews_NoWarnBelowEphemeralFloor — the shipped default sits below
// every common ephemeral floor, so the common case starts silently.
func TestStartPreviews_NoWarnBelowEphemeralFloor(t *testing.T) {
	elo, _, ok := kiln.EphemeralPortRange()
	if !ok {
		t.Skip("this host does not report an ephemeral port range")
	}
	if _, hi, err := config.ParsePortRange(config.DefaultPreviewPortRange); err != nil || hi >= elo {
		t.Skipf("this host's ephemeral floor (%d) is below the default preview range", elo)
	}

	d, logs := newPreviewPortRangeDaemon(t, config.DefaultPreviewPortRange)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.startPreviews(ctx)
	require.NotNil(t, d.previews())

	assert.NotContains(t, logs.String(), "overlaps the host ephemeral port range")

	cancel()
	d.wg.Wait()
}

// TestWarnPreviewPortRange_MalformedRangeIsSilent — an unparseable range is
// already reported by config validation and by manager construction; the
// overlap check has nothing to compare and must not add a third complaint or
// panic on the zero bounds.
func TestWarnPreviewPortRange_MalformedRangeIsSilent(t *testing.T) {
	d, logs := newPreviewPortRangeDaemon(t, "not-a-range")

	assert.NotPanics(t, func() { d.warnPreviewPortRange(d.config()) })
	assert.False(t, strings.Contains(logs.String(), "ephemeral"))
}
