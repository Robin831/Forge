package kiln

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// errStopFailed stands in for a teardown that goes wrong (a service that will
// not die, a teardown script that exits non-zero).
var errStopFailed = errors.New("stopping services failed")

func TestReaperInterval(t *testing.T) {
	cases := []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{30 * time.Minute, time.Minute}, // capped
		{4 * time.Minute, time.Minute},  // exactly the cap
		{40 * time.Second, 10 * time.Second},
		{2 * time.Second, time.Second}, // floored
		{0, time.Second},               // floored (the loop never runs anyway)
	}
	for _, tc := range cases {
		if got := reaperInterval(tc.timeout); got != tc.want {
			t.Errorf("reaperInterval(%s) = %s, want %s", tc.timeout, got, tc.want)
		}
	}
}

func TestReapOnceStopsIdlePreview(t *testing.T) {
	clock := time.Now()
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, IdleTimeout: 10 * time.Minute})
	h.mgr.now = func() time.Time { return clock }

	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Not yet idle: one second short of the timeout.
	clock = clock.Add(10*time.Minute - time.Second)
	h.mgr.reapOnce(context.Background())
	if _, ok := h.mgr.Get("Forge-aaa1"); !ok {
		t.Fatal("preview was reaped before its idle timeout elapsed")
	}

	clock = clock.Add(2 * time.Second)
	h.mgr.reapOnce(context.Background())

	if _, ok := h.mgr.Get("Forge-aaa1"); ok {
		t.Error("idle preview is still registered after a sweep")
	}
	if got := h.runner.instances[0].stopCount(); got != 1 {
		t.Errorf("instance stopped %d times, want 1", got)
	}
	if _, err := os.Stat(env.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("reaped preview left its worktree behind: %v", err)
	}
	row, err := h.store.GetPreview("Forge-aaa1")
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if row != nil {
		t.Errorf("reaped preview left a state row behind: %+v", row)
	}
}

func TestReapOnceKeepsTouchedPreview(t *testing.T) {
	clock := time.Now()
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2, IdleTimeout: 10 * time.Minute})
	h.mgr.now = func() time.Time { return clock }

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("Start idle: %v", err)
	}
	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-bbb2")); err != nil {
		t.Fatalf("Start active: %v", err)
	}

	clock = clock.Add(11 * time.Minute)
	// An operator hitting the preview link keeps it alive.
	h.mgr.Touch("Forge-bbb2")
	h.mgr.reapOnce(context.Background())

	if _, ok := h.mgr.Get("Forge-aaa1"); ok {
		t.Error("the idle preview survived the sweep")
	}
	if _, ok := h.mgr.Get("Forge-bbb2"); !ok {
		t.Error("the touched preview was reaped despite being active")
	}
}

func TestReapOnceContinuesAfterAFailedStop(t *testing.T) {
	clock := time.Now()
	h := newHarness(t, ManagerConfig{MaxConcurrent: 3, IdleTimeout: time.Minute})
	h.mgr.now = func() time.Time { return clock }

	for _, id := range []string{"Forge-aaa1", "Forge-bbb2"} {
		if _, err := h.mgr.Start(context.Background(), h.opts(id)); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}
	// The first preview's teardown fails; the second must still be reaped.
	h.runner.instances[0].stopErr = errStopFailed

	clock = clock.Add(2 * time.Minute)
	h.mgr.reapOnce(context.Background())

	if got := h.runner.instances[1].stopCount(); got != 1 {
		t.Errorf("the second preview was stopped %d times, want 1 — the sweep aborted on the first failure", got)
	}
	if got := len(h.mgr.List()); got != 0 {
		t.Errorf("%d previews left registered after the sweep, want 0", got)
	}
}

func TestRunReaperExitsOnContextCancel(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 1, IdleTimeout: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.mgr.RunReaper(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReaper did not return after its context was cancelled")
	}
}

func TestRunReaperReturnsWhenReapingIsDisabled(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 1})

	done := make(chan struct{})
	go func() {
		// A background context proves it is the zero timeout that stops the
		// loop, not cancellation.
		h.mgr.RunReaper(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReaper kept running with preview_idle_timeout unset")
	}
}

// TestRunReaperSweepsOnItsTicker covers the wiring between the ticker and
// reapOnce. The idle clock is fake — only the tick is real, and it is the
// shortest one the reaper will use.
func TestRunReaperSweepsOnItsTicker(t *testing.T) {
	clock := time.Now()
	h := newHarness(t, ManagerConfig{MaxConcurrent: 1, IdleTimeout: 4 * time.Second})
	h.mgr.now = func() time.Time { return clock }

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clock = clock.Add(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.mgr.RunReaper(ctx)

	deadline := time.After(5 * time.Second)
	for {
		if len(h.mgr.List()) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the reaper never swept the idle preview")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
