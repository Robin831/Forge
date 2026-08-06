package selfdeploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDrainClock advances exactly one poll interval per drain check, so the
// number of checks a wait performs is fully determined by the max wait and the
// interval — no sleeping and no dependence on goroutine scheduling.
type fakeDrainClock struct {
	mu       sync.Mutex
	start    time.Time
	interval time.Duration
	checks   int
}

func newFakeDrainClock(interval time.Duration) *fakeDrainClock {
	return &fakeDrainClock{start: time.Unix(1_700_000_000, 0), interval: interval}
}

func (c *fakeDrainClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.start.Add(time.Duration(c.checks) * c.interval)
}

// tick records that a drain check ran and returns its 1-based number.
func (c *fakeDrainClock) tick() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	return c.checks
}

func (c *fakeDrainClock) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checks
}

// alwaysReadyTicker stands in for time.Ticker: it fires as fast as the drain
// loop consumes it, since the fake clock — not wall time — decides when the wait
// expires. It also records the interval the loop asked for.
type alwaysReadyTicker struct {
	mu        sync.Mutex
	requested time.Duration
}

func (f *alwaysReadyTicker) new(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	f.requested = d
	f.mu.Unlock()

	ch := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case ch <- time.Time{}:
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return ch, func() { once.Do(func() { close(done) }) }
}

func (f *alwaysReadyTicker) interval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requested
}

// drainDeployer wires a Deployer to a fake clock/ticker and the given activity
// source, over a temp dir holding a "live" binary.
func drainDeployer(t *testing.T, cfg Config, active ActiveWorkersFunc) (*Deployer, *fakeDrainClock, *alwaysReadyTicker, *fakeRestarter, *fakeSink, string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "forge")
	if err := os.WriteFile(binPath, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.RepoPath = dir
	cfg.BinaryPath = binPath
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = 10 * time.Second
	}

	rest := &fakeRestarter{}
	sink := &fakeSink{}
	d := New(cfg, &fakeCommander{}, rest, sink, active)

	clock := newFakeDrainClock(cfg.DrainInterval)
	ticker := &alwaysReadyTicker{}
	d.now = clock.now
	d.newTicker = ticker.new
	return d, clock, ticker, rest, sink, binPath
}

// TestDeploy_WaitsForDrainThenDeploys is the core of the bounded wait: workers
// stay busy across several polls and the deploy lands in the gap the moment they
// go idle, instead of being skipped on the first busy sample.
func TestDeploy_WaitsForDrainThenDeploys(t *testing.T) {
	const busyPolls = 3
	var clock *fakeDrainClock
	active := func() ([]string, error) {
		if clock.tick() <= busyPolls {
			return []string{"Forge-busy"}, nil
		}
		return nil, nil
	}
	d, c, _, rest, sink, binPath := drainDeployer(t,
		Config{MaxDrainWait: time.Hour}, active)
	clock = c

	if err := d.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got := clock.count(); got != busyPolls+1 {
		t.Fatalf("expected %d drain checks (busy then idle), got %d", busyPolls+1, got)
	}
	if rest.called != 1 {
		t.Fatalf("expected the deploy to reach the restart, got %d calls", rest.called)
	}
	if got := readFile(t, binPath); got != "#!stub\n" {
		t.Fatalf("expected the new binary to be swapped in, got %q", got)
	}
	if sink.has(EventSkipped) {
		t.Fatalf("a deploy that drained must not emit a skipped event: %v", sink.events)
	}
}

// TestDeploy_DrainTimeoutDefersWithoutSwapping covers the give-up path: the wait
// expires, the error carries the elapsed time and the active worker set, and
// nothing is built, swapped or restarted.
func TestDeploy_DrainTimeoutDefersWithoutSwapping(t *testing.T) {
	var clock *fakeDrainClock
	active := func() ([]string, error) {
		clock.tick()
		return []string{"Forge-aaa1", "Forge-bbb2"}, nil
	}
	// 25s budget over a 10s interval: checks at +0s, +10s, +20s, then give up.
	d, c, _, rest, sink, binPath := drainDeployer(t,
		Config{MaxDrainWait: 25 * time.Second, DrainInterval: 10 * time.Second}, active)
	clock = c

	err := d.Deploy(context.Background())
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}
	var timeout *DrainTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("want a *DrainTimeoutError, got %T", err)
	}
	if timeout.Elapsed != 30*time.Second {
		t.Errorf("elapsed = %s, want 30s", timeout.Elapsed)
	}
	if timeout.Max != 25*time.Second {
		t.Errorf("max = %s, want 25s", timeout.Max)
	}
	if len(timeout.Workers) != 2 {
		t.Errorf("workers = %v, want the two active workers", timeout.Workers)
	}
	if !strings.Contains(err.Error(), "Forge-aaa1") || !strings.Contains(err.Error(), "30s") {
		t.Errorf("error must carry the worker set and elapsed time, got %q", err)
	}
	if clock.count() != 3 {
		t.Errorf("expected 3 drain checks within the budget, got %d", clock.count())
	}
	if rest.called != 0 {
		t.Errorf("restart must not run after a drain timeout")
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Errorf("live binary must be untouched after a drain timeout, got %q", got)
	}
	if !sink.has(EventSkipped) {
		t.Errorf("expected a skipped event, got %v", sink.events)
	}
	if sink.has(EventSuccess) {
		t.Errorf("a deferred deploy must not report success: %v", sink.events)
	}
}

// TestDeploy_DrainWaitCancelled verifies a cancelled context aborts the wait
// (returning the ctx error, not a drain timeout) without touching the binary.
func TestDeploy_DrainWaitCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var clock *fakeDrainClock
	active := func() ([]string, error) {
		if clock.tick() >= 2 {
			cancel() // shutdown arrives mid-wait
		}
		return []string{"Forge-busy"}, nil
	}
	d, c, _, rest, _, binPath := drainDeployer(t, Config{MaxDrainWait: time.Hour}, active)
	clock = c

	err := d.Deploy(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("cancellation must not be reported as a drain timeout: %v", err)
	}
	if rest.called != 0 {
		t.Errorf("restart must not run after cancellation")
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Errorf("live binary must be untouched, got %q", got)
	}
}

// TestDeploy_IdleForgeDeploysUnderCancelledContext guards the other half of the
// cancellation rule: cancellation aborts *waiting*, not a deploy that never had
// to wait. Deploy is deliberately callable with an already-cancelled caller
// context, so an idle forge must fall straight through to the swap and restart.
func TestDeploy_IdleForgeDeploysUnderCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var clock *fakeDrainClock
	active := func() ([]string, error) {
		clock.tick()
		return nil, nil
	}
	d, c, _, rest, _, _ := drainDeployer(t, Config{MaxDrainWait: time.Hour}, active)
	clock = c

	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy with a cancelled caller context: %v", err)
	}
	if got := clock.count(); got != 1 {
		t.Errorf("expected exactly one drain check, got %d", got)
	}
	if rest.called != 1 {
		t.Errorf("expected the restart to run, got %d calls", rest.called)
	}
}

// TestWaitForDrain_MaxShorterThanInterval verifies a max wait below the poll
// interval still gets one check and then times out promptly, rather than
// sleeping a full interval (or hanging) before noticing the budget is spent.
func TestWaitForDrain_MaxShorterThanInterval(t *testing.T) {
	var clock *fakeDrainClock
	active := func() ([]string, error) {
		clock.tick()
		return []string{"Forge-busy"}, nil
	}
	d, c, ticker, _, _, _ := drainDeployer(t,
		Config{MaxDrainWait: time.Hour, DrainInterval: time.Minute}, active)
	clock = c

	err := d.waitForDrain(context.Background(), time.Second)
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}
	if clock.count() != 1 {
		t.Errorf("expected exactly one check, got %d", clock.count())
	}
	if got := ticker.interval(); got != time.Second {
		t.Errorf("poll interval must be clamped to the max wait, got %s", got)
	}
}

// TestWaitForDrain_CheckErrorsAreRetriedThenFail verifies a failing worker query
// neither aborts the deploy immediately nor is mistaken for an idle forge: it is
// retried for the whole budget and then reported as a failure (not a timeout, so
// the caller can tell "could not tell" from "still busy").
func TestWaitForDrain_CheckErrorsAreRetriedThenFail(t *testing.T) {
	var clock *fakeDrainClock
	boom := errors.New("state.db is locked")
	active := func() ([]string, error) {
		clock.tick()
		return nil, boom
	}
	d, c, _, _, sink, _ := drainDeployer(t,
		Config{MaxDrainWait: 25 * time.Second, DrainInterval: 10 * time.Second}, active)
	clock = c

	err := d.waitForDrain(context.Background(), 25*time.Second)
	if err == nil || errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want a drain check failure, got %v", err)
	}
	if !strings.Contains(err.Error(), boom.Error()) {
		t.Errorf("error must carry the check failure, got %q", err)
	}
	if clock.count() != 3 {
		t.Errorf("expected the check to be retried across the budget, got %d calls", clock.count())
	}
	if !sink.has(EventFailed) {
		t.Errorf("expected a failed event, got %v", sink.events)
	}
}

// TestWaitForDrain_RecoversAfterCheckError verifies a transient query failure is
// only fatal when it persists: once a later check succeeds and reports an idle
// forge, the wait returns cleanly.
func TestWaitForDrain_RecoversAfterCheckError(t *testing.T) {
	var clock *fakeDrainClock
	active := func() ([]string, error) {
		if clock.tick() == 1 {
			return nil, errors.New("transient")
		}
		return nil, nil
	}
	d, c, _, _, _, _ := drainDeployer(t, Config{MaxDrainWait: time.Hour}, active)
	clock = c

	if err := d.waitForDrain(context.Background(), time.Hour); err != nil {
		t.Fatalf("waitForDrain after a transient error: %v", err)
	}
	if clock.count() != 2 {
		t.Errorf("expected the check to be retried once, got %d calls", clock.count())
	}
}

// TestWaitForDrain_DisabledCheck verifies a nil activity source skips the wait
// entirely (used by tests and by callers that manage draining themselves).
func TestWaitForDrain_DisabledCheck(t *testing.T) {
	d, _, _, _, _, _ := drainDeployer(t, Config{MaxDrainWait: time.Hour}, nil)
	if err := d.waitForDrain(context.Background(), time.Hour); err != nil {
		t.Fatalf("nil activity source must not block: %v", err)
	}
}
