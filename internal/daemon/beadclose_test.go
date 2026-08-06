package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

const (
	closeTestAnvil = "test-anvil"
	closeTestBead  = "Forge-ir70"
)

// flakyBdCloser is a bd close stand-in that returns a scripted sequence of
// errors before succeeding, recording every call.
type flakyBdCloser struct {
	mu      sync.Mutex
	errs    []error // consumed one per call; nil (or exhausted) means success
	calls   int
	reasons []string
}

func (f *flakyBdCloser) close(_ context.Context, _, _, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.reasons = append(f.reasons, reason)
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *flakyBdCloser) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newCloseTestDaemon builds a Daemon wired for the close-after-merge path with
// a real state DB, an instant-sleep retry policy, and no real bd invocations.
func newCloseTestDaemon(t *testing.T, closer *flakyBdCloser) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			closeTestAnvil: {Path: t.TempDir()},
		},
	})
	d.beadCloser = closer.close
	d.bdCloseRetry = bdRetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   time.Millisecond,
		Multiplier:  2,
		MaxDelay:    time.Millisecond,
		Sleep:       func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
	}
	// Default: two open dependents, so the needs-attention message carries a count.
	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return []byte(`{"status":"in_progress","dependents":[{"id":"Forge-66sn","status":"open"},{"id":"Forge-zzzz","status":"closed"}]}`), "", nil
	}
	return d
}

func anvilPathFor(t *testing.T, d *Daemon) string {
	t.Helper()
	return d.cfg.Load().Anvils[closeTestAnvil].Path
}

func TestIsRetryableBdError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "mysql i/o timeout (Forge-xg4q, 2026-08-06)",
			err:  errors.New("bd close Forge-xg4q --json: exit status 1\nError 1105: read tcp 127.0.0.1:3306: i/o timeout"),
			want: true,
		},
		{
			name: "invalid connection after timeout",
			err:  errors.New("bd close Forge-xg4q --json: exit status 1\ninvalid connection"),
			want: true,
		},
		{
			name: "dolt serialization failure (Forge-7du1, 2026-08-06)",
			err:  errors.New("bd close Forge-7du1 --json: exit status 1\nError 1213 (40001): serialization failure, try restarting transaction"),
			want: true,
		},
		{
			name: "schema migration lock timeout (Forge-ir70, 2026-08-06)",
			err:  errors.New("bd close Forge-ir70 --json: exit status 1\nschema migration lock unavailable: timeout"),
			want: true,
		},
		{
			name: "connection refused",
			err:  errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"),
			want: true,
		},
		{
			name: "unknown bead is permanent",
			err:  errors.New("bd close NOPE-1 --json: exit status 1\nissue not found: NOPE-1"),
			want: false,
		},
		{
			name: "bad flag is permanent",
			err:  errors.New("bd close X --json: exit status 1\nunknown flag: --reason"),
			want: false,
		},
		{
			name: "already closed is not retryable",
			err:  errors.New("bd close X --json: exit status 1\nissue already closed"),
			want: false,
		},
		{name: "nil", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isRetryableBdError(tc.err))
		})
	}
}

// TestCloseMergedBead_SurvivesTransientFailures is the core acceptance case:
// bd fails twice with transient dolt errors, then succeeds. The bead must end
// up closed with no pending row and no needs-attention entry.
func TestCloseMergedBead_SurvivesTransientFailures(t *testing.T) {
	closer := &flakyBdCloser{errs: []error{
		errors.New("Error 1213 (40001): serialization failure, try restarting transaction"),
		errors.New("read tcp 127.0.0.1:3306: i/o timeout"),
	}}
	d := newCloseTestDaemon(t, closer)

	err := d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil)
	require.NoError(t, err)

	assert.Equal(t, 3, closer.callCount(), "should retry twice then succeed")

	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	assert.Empty(t, pending, "successful close must leave no pending entry")

	r, err := d.db.GetRetry(closeTestBead, closeTestAnvil)
	require.NoError(t, err)
	assert.Nil(t, r, "no needs-attention entry should have been raised")
}

// TestCloseMergedBead_ExhaustedRetriesSurfaceNeedsAttention verifies the
// "don't drop it silently" half: the attempts are capped, the close is
// persisted for later cycles, and the operator sees the dependent count.
func TestCloseMergedBead_ExhaustedRetriesSurfaceNeedsAttention(t *testing.T) {
	closer := &flakyBdCloser{}
	for i := 0; i < 10; i++ {
		closer.errs = append(closer.errs, errors.New("schema migration lock unavailable: timeout"))
	}
	d := newCloseTestDaemon(t, closer)

	err := d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil)
	require.Error(t, err)

	assert.Equal(t, 4, closer.callCount(), "attempts must be capped at MaxAttempts")

	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, closeTestBead, pending[0].BeadID)
	assert.Equal(t, closeTestAnvil, pending[0].Anvil)
	assert.Equal(t, 773, pending[0].PRNumber)
	assert.Equal(t, 4, pending[0].Attempts)
	assert.Contains(t, pending[0].LastError, "schema migration lock unavailable")

	r, err := d.db.GetRetry(closeTestBead, closeTestAnvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman)
	assert.Contains(t, r.LastError, beadCloseAttentionPrefix)
	assert.Contains(t, r.LastError,
		fmt.Sprintf("merged but unclosed bead %s (PR #773) blocking 1 dependent", closeTestBead))
}

// TestReconcilePendingBeadCloses_RetriesOnLaterCycle covers the recovery path:
// the next bellows cycle re-attempts the pending close, succeeds, and clears
// both the pending row and the needs-attention entry.
func TestReconcilePendingBeadCloses_RetriesOnLaterCycle(t *testing.T) {
	closer := &flakyBdCloser{}
	for i := 0; i < 4; i++ {
		closer.errs = append(closer.errs, errors.New("Error 1213: serialization failure"))
	}
	d := newCloseTestDaemon(t, closer)

	// Cycle 1: exhaust the burst.
	require.Error(t, d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil))
	require.Equal(t, 4, closer.callCount())

	// Cycle 2: bd is healthy again (errs drained), bead still open.
	d.reconcilePendingBeadCloses(context.Background())

	assert.Equal(t, 5, closer.callCount(), "reconcile should make exactly one more attempt")
	assert.Equal(t, "PR #773 merged", closer.reasons[len(closer.reasons)-1],
		"the original close reason must be reused")

	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	assert.Empty(t, pending)

	r, err := d.db.GetRetry(closeTestBead, closeTestAnvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.False(t, r.NeedsHuman, "needs-attention must be cleared once the bead closes")
}

// TestReconcilePendingBeadCloses_AlreadyClosedExternally verifies the derive-
// don't-trust-the-row behaviour: a bead an operator closed by hand drops its
// pending entry without spending a bd close.
func TestReconcilePendingBeadCloses_AlreadyClosedExternally(t *testing.T) {
	closer := &flakyBdCloser{}
	d := newCloseTestDaemon(t, closer)

	require.NoError(t, d.db.UpsertPendingBeadClose(state.PendingBeadClose{
		BeadID:    closeTestBead,
		Anvil:     closeTestAnvil,
		PRNumber:  773,
		Reason:    "PR #773 merged",
		Attempts:  4,
		LastError: "schema migration lock unavailable: timeout",
	}))
	require.NoError(t, d.db.MarkNeedsHuman(closeTestBead, closeTestAnvil,
		beadCloseAttentionPrefix+"merged but unclosed bead"))

	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return []byte(`{"status":"closed","dependents":[]}`), "", nil
	}

	d.reconcilePendingBeadCloses(context.Background())

	assert.Zero(t, closer.callCount(), "an already-closed bead must not be closed again")

	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	assert.Empty(t, pending)

	r, err := d.db.GetRetry(closeTestBead, closeTestAnvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.False(t, r.NeedsHuman)
}

// TestCloseMergedBead_PermanentErrorDoesNotRetry keeps the budget for failures
// that can actually clear: a missing bead is hopeless and must fail on the
// first attempt.
func TestCloseMergedBead_PermanentErrorDoesNotRetry(t *testing.T) {
	closer := &flakyBdCloser{}
	for i := 0; i < 5; i++ {
		closer.errs = append(closer.errs, errors.New("issue not found: Forge-ir70"))
	}
	d := newCloseTestDaemon(t, closer)

	require.Error(t, d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil))

	assert.Equal(t, 1, closer.callCount(), "permanent errors must not be retried")

	// It is still pending: a permanent-looking error can be a misclassification,
	// and a merged bead that is still open always needs an operator.
	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, 1, pending[0].Attempts)
}

// TestCloseMergedBead_AlreadyClosedIsSuccess guards against churning Dolt
// commits when another path closed the bead first.
func TestCloseMergedBead_AlreadyClosedIsSuccess(t *testing.T) {
	closer := &flakyBdCloser{errs: []error{errors.New("bd close: issue already closed")}}
	d := newCloseTestDaemon(t, closer)

	require.NoError(t, d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil))
	assert.Equal(t, 1, closer.callCount())

	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// TestCloseMergedBead_AttentionMessageOmitsUnknownDependentCount checks the
// fallback when the dependents lookup itself fails — an invented "0" would
// read as harmless.
func TestCloseMergedBead_AttentionMessageOmitsUnknownDependentCount(t *testing.T) {
	closer := &flakyBdCloser{}
	for i := 0; i < 5; i++ {
		closer.errs = append(closer.errs, errors.New("i/o timeout"))
	}
	d := newCloseTestDaemon(t, closer)
	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return nil, "boom", errors.New("bd show failed")
	}

	require.Error(t, d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil))

	r, err := d.db.GetRetry(closeTestBead, closeTestAnvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Contains(t, r.LastError, fmt.Sprintf("merged but unclosed bead %s (PR #773)", closeTestBead))
	assert.NotContains(t, r.LastError, "blocking")
}

// TestClearBeadCloseAttention_LeavesForeignFlags ensures a successful close
// cannot clear a needs_human flag raised by an unrelated subsystem.
func TestClearBeadCloseAttention_LeavesForeignFlags(t *testing.T) {
	closer := &flakyBdCloser{}
	d := newCloseTestDaemon(t, closer)

	require.NoError(t, d.db.MarkNeedsHuman(closeTestBead, closeTestAnvil, "circuit breaker: 3 dispatch failures"))

	require.NoError(t, d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil))

	r, err := d.db.GetRetry(closeTestBead, closeTestAnvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "an unrelated needs_human flag must survive the close")
	assert.Contains(t, r.LastError, "circuit breaker")
}

// TestReconcilePendingBeadCloses_DropsUnconfiguredAnvil stops a removed anvil
// from pinning a pending row (and its needs-attention entry) forever.
func TestReconcilePendingBeadCloses_DropsUnconfiguredAnvil(t *testing.T) {
	closer := &flakyBdCloser{}
	d := newCloseTestDaemon(t, closer)

	require.NoError(t, d.db.UpsertPendingBeadClose(state.PendingBeadClose{
		BeadID: closeTestBead, Anvil: "gone-anvil", PRNumber: 773, Reason: "PR #773 merged",
	}))

	d.reconcilePendingBeadCloses(context.Background())

	pending, err := d.db.PendingBeadCloses()
	require.NoError(t, err)
	assert.Empty(t, pending)
	assert.Zero(t, closer.callCount())
}

// TestHandleBeadCloseOnMerge_RetriesInBackground wires the real bellows event
// through to the retry burst, confirming the handler does not block the poll
// goroutine while it retries.
func TestHandleBeadCloseOnMerge_RetriesInBackground(t *testing.T) {
	closer := &flakyBdCloser{errs: []error{errors.New("Error 1213: serialization failure")}}
	d := newCloseTestDaemon(t, closer)

	d.handleBeadCloseOnMerge(context.Background(), bellows.PREvent{
		EventType: bellows.EventPRMerged,
		BeadID:    closeTestBead,
		Anvil:     closeTestAnvil,
		PRNumber:  773,
		Timestamp: time.Now(),
	})

	require.Eventually(t, func() bool { return closer.callCount() == 2 },
		5*time.Second, 5*time.Millisecond, "close should be retried in the background")

	require.Eventually(t, func() bool {
		pending, err := d.db.PendingBeadCloses()
		return err == nil && len(pending) == 0
	}, 5*time.Second, 5*time.Millisecond)
}

// TestHandleBeadCloseOnMerge_SkipsExternalPRs guards the ext-* carve-out.
func TestHandleBeadCloseOnMerge_SkipsExternalPRs(t *testing.T) {
	closer := &flakyBdCloser{}
	d := newCloseTestDaemon(t, closer)

	d.handleBeadCloseOnMerge(context.Background(), bellows.PREvent{
		EventType: bellows.EventPRMerged,
		BeadID:    "ext-42",
		Anvil:     closeTestAnvil,
		PRNumber:  42,
	})
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, closer.callCount())
}

// TestCloseMergedBead_CollapsesConcurrentAttempts verifies the in-flight guard:
// the bellows event and a reconcile cycle racing on the same bead must not run
// two bursts.
func TestCloseMergedBead_CollapsesConcurrentAttempts(t *testing.T) {
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	d := newCloseTestDaemon(t, &flakyBdCloser{})
	d.beadCloser = func(_ context.Context, _, _, _ string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return nil
	}

	started := make(chan struct{})
	go func() {
		close(started)
		_ = d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
			anvilPathFor(t, d), "PR #773 merged", 773, nil)
	}()
	<-started
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	}, 2*time.Second, 5*time.Millisecond)

	// Second caller while the first is mid-flight: must be a no-op.
	require.NoError(t, d.closeMergedBead(context.Background(), closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773, nil))

	close(release)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls, "concurrent close attempts for one bead must collapse")
}

// TestBellowsCycleHookRunsWithoutOpenPRs pins the placement of the hook: the
// pending sweep must still run on a cycle where no PR is open, because by then
// the merged PR that stranded the bead is long gone from the open set.
func TestBellowsCycleHookRunsWithoutOpenPRs(t *testing.T) {
	closer := &flakyBdCloser{}
	d := newCloseTestDaemon(t, closer)
	require.NoError(t, d.db.UpsertPendingBeadClose(state.PendingBeadClose{
		BeadID: closeTestBead, Anvil: closeTestAnvil, PRNumber: 773, Reason: "PR #773 merged",
	}))
	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return []byte(`{"status":"in_progress","dependents":[]}`), "", nil
	}

	m := bellows.New(d.db, nil, time.Hour, map[string]string{closeTestAnvil: anvilPathFor(t, d)},
		func() bool { return false },
		func() int { return 1 }, func() int { return 1 }, func() int { return 1 })
	m.SetCycleHook(d.kickPendingBeadCloseReconcile)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = m.Run(ctx) }()
	t.Cleanup(cancel)

	require.Eventually(t, func() bool { return closer.callCount() >= 1 },
		5*time.Second, 10*time.Millisecond, "cycle hook must fire with zero open PRs")

	require.Eventually(t, func() bool {
		pending, err := d.db.PendingBeadCloses()
		return err == nil && len(pending) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func TestBeadCloseAttentionMessage(t *testing.T) {
	d := &Daemon{}
	assert.Equal(t, "merged but unclosed bead B (PR #7) blocking 3 dependents",
		d.beadCloseAttentionMessage("B", 7, 3, true))
	assert.Equal(t, "merged but unclosed bead B (PR #7) blocking 1 dependent",
		d.beadCloseAttentionMessage("B", 7, 1, true))
	assert.Equal(t, "merged but unclosed bead B (PR #7) blocking 0 dependents",
		d.beadCloseAttentionMessage("B", 7, 0, true))
	assert.Equal(t, "merged but unclosed bead B (PR #7)",
		d.beadCloseAttentionMessage("B", 7, 0, false))
}

func TestCountOpenDependents(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return []byte(`{"dependents":[{"status":"open"},{"status":"in_progress"},{"status":"closed"}]}`), "", nil
	}
	n, ok := d.countOpenDependents("/tmp", "B")
	assert.True(t, ok)
	assert.Equal(t, 2, n)

	// bd show --json may wrap the object in a single-element array.
	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return []byte(`[{"dependents":[{"status":"open"}]}]`), "", nil
	}
	n, ok = d.countOpenDependents("/tmp", "B")
	assert.True(t, ok)
	assert.Equal(t, 1, n)

	d.beadShower = func(_, _ string) ([]byte, string, error) {
		return []byte("not json"), "", nil
	}
	_, ok = d.countOpenDependents("/tmp", "B")
	assert.False(t, ok)

	d.beadShower = nil
	_, ok = d.countOpenDependents("/tmp", "B")
	assert.False(t, ok)
}

func TestCloseBeadWithRetry_HonoursContextCancellation(t *testing.T) {
	closer := &flakyBdCloser{}
	for i := 0; i < 10; i++ {
		closer.errs = append(closer.errs, errors.New("i/o timeout"))
	}
	d := newCloseTestDaemon(t, closer)

	ctx, cancel := context.WithCancel(context.Background())
	d.bdCloseRetry.Sleep = func(context.Context, time.Duration) bool {
		cancel()
		return false
	}

	attempts, err := d.closeBeadWithRetry(ctx, closeTestBead, closeTestAnvil,
		anvilPathFor(t, d), "PR #773 merged", 773)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled"))
	assert.Equal(t, 1, attempts)
}
