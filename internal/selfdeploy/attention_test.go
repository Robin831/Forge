package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedTime is the clock every escalation test stamps its events with.
var fixedTime = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// fakeEmitter records the needs-attention items a deploy raises and resolves.
// emitErr makes the escalation itself fail, which must never change what the
// deploy does.
type fakeEmitter struct {
	mu       sync.Mutex
	events   []DeployEvent
	cleared  [][]FailureReason
	emitErr  error
	clearErr error
}

func (f *fakeEmitter) EmitNeedsAttention(ev DeployEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.emitErr
}

func (f *fakeEmitter) ClearNeedsAttention(reasons ...FailureReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, reasons)
	return f.clearErr
}

// only returns the single recorded event, failing the test when the count is
// anything but one. Every failure path must escalate exactly once — a restart
// failure that rolls back is one incident, not two.
func (f *fakeEmitter) only(t *testing.T) DeployEvent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) != 1 {
		t.Fatalf("want exactly one needs-attention event, got %d: %+v", len(f.events), f.events)
	}
	return f.events[0]
}

// clock advances by step on every read, so a drain wait can time out
// deterministically without sleeping. A zero step pins time completely.
type clock struct {
	t    time.Time
	step time.Duration
}

func (c *clock) now() time.Time {
	cur := c.t
	c.t = c.t.Add(c.step)
	return cur
}

// setupWithEmitter builds a Deployer with the escalation wired up. workers is
// the number of permanently-active workers the drain check reports, and step is
// how far the fake clock jumps per read (non-zero to force a drain timeout).
func setupWithEmitter(t *testing.T, workers int, step time.Duration) (*Deployer, *fakeRestarter, *fakeEmitter, string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "forge")
	if err := os.WriteFile(binPath, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	rest := &fakeRestarter{}
	em := &fakeEmitter{}
	var active ActiveWorkersFunc
	if workers > 0 {
		ids := make([]string, workers)
		for i := range ids {
			ids[i] = fmt.Sprintf("Forge-w%d", i)
		}
		active = func() ([]string, error) { return ids, nil }
	}
	d := New(
		Config{
			RepoPath:     dir,
			BinaryPath:   binPath,
			CurrentSHA:   "prev123456789",
			MaxDrainWait: 30 * time.Minute,
			UnitName:     "forge",
		},
		&fakeCommander{}, rest, &fakeSink{}, active,
		WithEmitter(em),
	)
	d.now = (&clock{t: fixedTime, step: step}).now
	return d, rest, em, binPath
}

// TestDeploy_DrainTimeoutRaisesNeedsAttention pins the deferral escalation: the
// binary is untouched, so there is no rollback, but the merged change is still
// not running and that has to be visible.
func TestDeploy_DrainTimeoutRaisesNeedsAttention(t *testing.T) {
	d, _, em, _ := setupWithEmitter(t, 2, time.Hour)

	err := d.Deploy(context.Background())
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}

	ev := em.only(t)
	if ev.Reason != ReasonDrainTimeout {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonDrainTimeout)
	}
	if ev.RolledBack {
		t.Error("a deferred deploy never rolls back")
	}
	if ev.RestoredSHA != "" {
		t.Errorf("RestoredSHA = %q, want empty when nothing was swapped", ev.RestoredSHA)
	}
	if ev.AttemptedSHA != "" {
		t.Errorf("AttemptedSHA = %q, want empty: the deploy never pulled", ev.AttemptedSHA)
	}
	if !strings.Contains(ev.Detail, "Forge-w0") {
		t.Errorf("Detail must name the workers that held the deploy up, got %q", ev.Detail)
	}
	if ev.Unit != "forge" {
		t.Errorf("Unit = %q, want forge", ev.Unit)
	}
	// The clock is read once for the wait's start, then once per check; the
	// timestamp is the give-up moment, not the start.
	if want := fixedTime.Add(time.Hour); !ev.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %s, want %s", ev.Timestamp, want)
	}
}

// TestDeploy_RestartFailureRaisesOneRollbackEvent is the core case: the restart
// fails, the previous binary goes back, and the operator gets exactly one item
// carrying both builds.
func TestDeploy_RestartFailureRaisesOneRollbackEvent(t *testing.T) {
	d, rest, em, binPath := setupWithEmitter(t, 0, 0)
	rest.err = errors.New("systemctl restart failed")

	if err := d.Deploy(context.Background()); err == nil {
		t.Fatal("expected an error when the restart fails")
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("expected the rollback to restore the OLD binary, got %q", got)
	}

	ev := em.only(t)
	if ev.Reason != ReasonRestartFailed {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonRestartFailed)
	}
	if !ev.RolledBack {
		t.Error("RolledBack must be true once the previous binary is back in place")
	}
	if ev.AttemptedSHA != fakeHeadSHA {
		t.Errorf("AttemptedSHA = %q, want %q", ev.AttemptedSHA, fakeHeadSHA)
	}
	if ev.RestoredSHA != "prev123456789" {
		t.Errorf("RestoredSHA = %q, want the build that is live again", ev.RestoredSHA)
	}
	if !strings.Contains(ev.Detail, "systemctl restart failed") {
		t.Errorf("Detail must carry the restart error, got %q", ev.Detail)
	}
	if ev.BinaryPath != binPath {
		t.Errorf("BinaryPath = %q, want %q", ev.BinaryPath, binPath)
	}
	if !ev.Timestamp.Equal(fixedTime) {
		t.Errorf("Timestamp = %s, want %s", ev.Timestamp, fixedTime)
	}
}

// TestDeploy_RollbackFailureIsItsOwnReason separates the worst state — the new
// binary is on disk, never started, and the old one could not be put back —
// from an ordinary rolled-back restart.
func TestDeploy_RollbackFailureIsItsOwnReason(t *testing.T) {
	d, rest, em, _ := setupWithEmitter(t, 0, 0)
	rest.err = errors.New("systemctl restart failed")
	// Fail only the rollback rename (prev -> live), not the swap that precedes it.
	realRename := d.rename
	d.rename = func(from, to string) error {
		if from == d.cfg.PrevPath {
			return errors.New("permission denied")
		}
		return realRename(from, to)
	}

	if err := d.Deploy(context.Background()); err == nil {
		t.Fatal("expected an error when the restart fails")
	}

	ev := em.only(t)
	if ev.Reason != ReasonRollbackFailed {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonRollbackFailed)
	}
	if ev.RolledBack {
		t.Error("RolledBack must be false when the restore itself failed")
	}
	if ev.RestoredSHA != "" {
		t.Errorf("RestoredSHA = %q, want empty: nothing was restored", ev.RestoredSHA)
	}
	if !strings.Contains(ev.Detail, "rollback failed") {
		t.Errorf("Detail must say the rollback failed, got %q", ev.Detail)
	}
}

// TestDeploy_SwapFailureRaisesRollbackEvent covers the other rollback site: the
// new binary could not be moved into place and the previous one was restored.
func TestDeploy_SwapFailureRaisesRollbackEvent(t *testing.T) {
	d, rest, em, _ := setupWithEmitter(t, 0, 0)
	realRename := d.rename
	d.rename = func(from, to string) error {
		if strings.HasSuffix(from, ".new") {
			return errors.New("cross-device link")
		}
		return realRename(from, to)
	}

	if err := d.Deploy(context.Background()); err == nil {
		t.Fatal("expected an error when the swap fails")
	}
	if rest.called != 0 {
		t.Fatalf("the restart must not run after a failed swap, called=%d", rest.called)
	}

	ev := em.only(t)
	if ev.Reason != ReasonSwapFailed {
		t.Errorf("Reason = %q, want %q", ev.Reason, ReasonSwapFailed)
	}
	if !ev.RolledBack {
		t.Error("RolledBack must be true: the previous binary was restored")
	}
	if ev.AttemptedSHA != fakeHeadSHA {
		t.Errorf("AttemptedSHA = %q, want %q", ev.AttemptedSHA, fakeHeadSHA)
	}
}

// TestDeploy_SuccessResolvesEverything pins the other half of the contract: a
// deploy that reaches the restart clears any item an earlier attempt left
// behind, so a resolved failure cannot linger in Needs Attention.
func TestDeploy_SuccessResolvesEverything(t *testing.T) {
	d, rest, em, _ := setupWithEmitter(t, 0, 0)
	// A drain check that reports an idle forge exercises the drain-timeout
	// resolution as well as the final one.
	d.activeWorkers = func() ([]string, error) { return nil, nil }

	if err := d.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if rest.called != 1 {
		t.Fatalf("expected one restart, got %d", rest.called)
	}
	if len(em.events) != 0 {
		t.Fatalf("a successful deploy must raise nothing, got %+v", em.events)
	}
	if len(em.cleared) != 2 {
		t.Fatalf("want a drain-timeout resolve then a full resolve, got %+v", em.cleared)
	}
	if len(em.cleared[0]) != 1 || em.cleared[0][0] != ReasonDrainTimeout {
		t.Errorf("first resolve = %v, want only %q", em.cleared[0], ReasonDrainTimeout)
	}
	if len(em.cleared[1]) != 0 {
		t.Errorf("final resolve = %v, want every reason (no arguments)", em.cleared[1])
	}
}

// TestDeploy_EmitterFailureDoesNotAbortRollback guards the escalation's own
// error path: recording an item is best-effort, and must never cost the
// rollback that keeps the host on a working binary.
func TestDeploy_EmitterFailureDoesNotAbortRollback(t *testing.T) {
	d, rest, em, binPath := setupWithEmitter(t, 0, 0)
	rest.err = errors.New("systemctl restart failed")
	em.emitErr = errors.New("state.db is locked")

	err := d.Deploy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "systemctl restart failed") {
		t.Fatalf("Deploy must still report the restart failure, got %v", err)
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("the rollback must still restore the OLD binary, got %q", got)
	}
	// The item was still offered exactly once, even though recording it failed.
	em.only(t)
}

// TestDeploy_NoEmitterIsSafe keeps the escalation optional: callers that do not
// wire an emitter (including the existing tests) must behave exactly as before.
func TestDeploy_NoEmitterIsSafe(t *testing.T) {
	d, _, _, _, binPath := setup(t, 0, nil)
	if d.attention != nil {
		t.Fatal("no emitter should be configured by default")
	}
	if err := d.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy without an emitter: %v", err)
	}
	if got := readFile(t, binPath); got != "#!stub\n" {
		t.Fatalf("live binary should be the newly built stub, got %q", got)
	}
}
