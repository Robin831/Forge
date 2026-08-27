package depcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

// fakeFailureStore is the escalation memory, in memory. It records what was
// asked of it so a test can assert on the suppression decision itself rather
// than on its symptoms.
type fakeFailureStore struct {
	rows       map[string]state.DepcheckFailure
	recordErr  error
	clearErr   error
	pruneKeep  []string
	pruneCalls int
}

func newFakeStore() *fakeFailureStore {
	return &fakeFailureStore{rows: map[string]state.DepcheckFailure{}}
}

func (f *fakeFailureStore) RecordDepcheckFailure(rec state.DepcheckFailure) (bool, error) {
	if f.recordErr != nil {
		return false, f.recordErr
	}
	prev, existed := f.rows[rec.Anvil]
	fresh := !existed || prev.Signature != rec.Signature
	if !fresh {
		rec.Occurrences = prev.Occurrences + 1
	} else {
		rec.Occurrences = 1
	}
	f.rows[rec.Anvil] = rec
	return fresh, nil
}

func (f *fakeFailureStore) ClearDepcheckFailure(anvil string) (bool, error) {
	if f.clearErr != nil {
		return false, f.clearErr
	}
	_, existed := f.rows[anvil]
	delete(f.rows, anvil)
	return existed, nil
}

func (f *fakeFailureStore) PruneDepcheckFailures(keep []string) error {
	f.pruneCalls++
	f.pruneKeep = keep
	kept := map[string]struct{}{}
	for _, k := range keep {
		kept[k] = struct{}{}
	}
	for anvil := range f.rows {
		if _, ok := kept[anvil]; !ok {
			delete(f.rows, anvil)
		}
	}
	return nil
}

// recordedEvent is one activity-feed entry.
type recordedEvent struct {
	Type    state.EventType
	Message string
	Anvil   string
}

type fakeEvents struct{ events []recordedEvent }

func (f *fakeEvents) LogEvent(typ state.EventType, message, _, anvil string) error {
	f.events = append(f.events, recordedEvent{Type: typ, Message: message, Anvil: anvil})
	return nil
}

func (f *fakeEvents) ofType(typ state.EventType) []recordedEvent {
	var out []recordedEvent
	for _, e := range f.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func newTestScanner() (*Scanner, *fakeFailureStore, *fakeEvents) {
	store := newFakeStore()
	events := &fakeEvents{}
	return &Scanner{failures: store, events: events}, store, events
}

// blockedErr is a fetch that will fail the same way every night.
func blockedErr() error {
	return &gitError{
		Args:   []string{"fetch", "origin", "main"},
		Stderr: "fatal: You are not currently on a branch.",
		Err:    errors.New("exit status 128"),
	}
}

// TestBlockedEscalatesOnce is the bead's headline behaviour: an anvil blocked by
// the same condition on three consecutive runs produces ONE event and one
// needs-attention row, not three identical events.
func TestBlockedEscalatesOnce(t *testing.T) {
	s, store, events := newTestScanner()

	for i := 0; i < 3; i++ {
		s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
	}

	failed := events.ofType(state.EventDepcheckFailed)
	require.Len(t, failed, 1, "a repeating condition must be announced once")
	assert.Equal(t, "heimdall", failed[0].Anvil)
	assert.Contains(t, failed[0].Message, "dependency scan blocked")
	assert.Contains(t, failed[0].Message, "You are not currently on a branch")

	row, ok := store.rows["heimdall"]
	require.True(t, ok, "the anvil must be on the attention panel for as long as it is blocked")
	assert.Equal(t, state.DepcheckKindBlocked, row.Kind)
	assert.Equal(t, 3, row.Occurrences, "the silent runs are still counted")
	assert.NotEmpty(t, row.Signature)
}

// TestBlockedReescalatesAfterChange: silence is per CONDITION, not per anvil. An
// operator who fixes the detached HEAD and lands on a locked ref has a
// different problem and a different next action, so they are told again.
func TestBlockedReescalatesAfterChange(t *testing.T) {
	s, store, events := newTestScanner()

	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
	first := store.rows["heimdall"].Signature

	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", &gitError{
		Args:   []string{"fetch", "origin", "main"},
		Stderr: "error: cannot lock ref 'refs/remotes/origin/main'",
		Err:    errors.New("exit status 1"),
	})

	failed := events.ofType(state.EventDepcheckFailed)
	require.Len(t, failed, 2, "a different condition is a new escalation")
	assert.Contains(t, failed[1].Message, "cannot lock ref")
	assert.NotEqual(t, first, store.rows["heimdall"].Signature)
	assert.Equal(t, 1, store.rows["heimdall"].Occurrences, "the new condition starts its own count")
}

// TestTransientReportsEveryRun pins that classification changed nothing for the
// failures a retry can actually get past: they keep the per-run depcheck_failed
// event and never reach the attention panel, because there is nothing for an
// operator to do about a DNS blip.
func TestTransientReportsEveryRun(t *testing.T) {
	s, store, events := newTestScanner()

	transient := &gitError{
		Args:   []string{"fetch", "origin", "main"},
		Stderr: "ssh: Could not resolve hostname github.com: Temporary failure in name resolution",
		Err:    errors.New("exit status 128"),
	}
	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", transient)
	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", transient)

	failed := events.ofType(state.EventDepcheckFailed)
	require.Len(t, failed, 2, "a transient failure keeps reporting per run")
	for _, e := range failed {
		assert.Contains(t, e.Message, "skipping depcheck to avoid stale results")
		assert.NotContains(t, e.Message, "dependency scan blocked")
	}
	assert.Empty(t, store.rows, "a transient failure must not raise a needs-attention entry")
}

// TestUnrecognisedFailureKeepsTheOldBehaviour: gitFailureUnknown is the
// fail-safe direction. An unmodelled message must not mute the only signal that
// anything is wrong.
func TestUnrecognisedFailureKeepsTheOldBehaviour(t *testing.T) {
	s, store, events := newTestScanner()

	unknown := &gitError{Args: []string{"fetch"}, Stderr: "error: nobody has modelled this", Err: errors.New("exit status 3")}
	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", unknown)
	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", unknown)

	assert.Len(t, events.ofType(state.EventDepcheckFailed), 2)
	assert.Empty(t, store.rows)
}

// TestSuccessClearsSignature: the entry is withdrawn by a scan that reads the
// manifests, and the withdrawal re-arms the escalation — the same condition
// recurring after a fix is news again.
func TestSuccessClearsSignature(t *testing.T) {
	s, store, events := newTestScanner()

	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
	require.Len(t, events.ofType(state.EventDepcheckFailed), 1)

	s.clearBlocked("heimdall")
	assert.Empty(t, store.rows, "a successful scan withdraws the entry")
	passed := events.ofType(state.EventDepcheckPassed)
	require.Len(t, passed, 1, "the recovery is announced, like every other self-clearing condition")
	assert.Contains(t, passed[0].Message, "unblocked")

	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
	assert.Len(t, events.ofType(state.EventDepcheckFailed), 2, "a recurrence after a fix escalates again")
}

// TestClearOnAnUnblockedAnvilIsSilent: the success path calls clearBlocked on
// every scan of every anvil, so the overwhelmingly common case must produce no
// event at all.
func TestClearOnAnUnblockedAnvilIsSilent(t *testing.T) {
	s, _, events := newTestScanner()
	s.clearBlocked("heimdall")
	assert.Empty(t, events.events)
}

// TestEscalationSurvivesAnUnwritableStore: an operator told twice is a
// nuisance, an operator never told is the bug being fixed — so a store that
// cannot answer escalates rather than suppresses.
func TestEscalationSurvivesAnUnwritableStore(t *testing.T) {
	s, store, events := newTestScanner()
	store.recordErr = errors.New("database is locked")

	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())

	assert.Len(t, events.ofType(state.EventDepcheckFailed), 2)
}

// TestScannerWithoutAStoreStillReports pins the nil-seam: a scanner built
// without a database (every unit test that drives a scan, and a Forge whose DB
// handle is absent) must not panic and must not lose the failure.
func TestScannerWithoutAStoreStillReports(t *testing.T) {
	s := &Scanner{}
	assert.NotPanics(t, func() {
		s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
		s.clearBlocked("heimdall")
		s.pruneBlocked(map[string]string{"heimdall": "/srv/anvils/heimdall"})
	})
}

// TestPruneBlockedDropsDeregisteredAnvils: nothing else clears these rows — the
// clear happens on a successful scan of that anvil, which a deregistered anvil
// never gets — so an anvil removed from the config would otherwise keep an
// entry no action can resolve.
func TestPruneBlockedDropsDeregisteredAnvils(t *testing.T) {
	s, store, _ := newTestScanner()
	s.reportScanFailure("heimdall", "/srv/anvils/heimdall", blockedErr())
	s.reportScanFailure("retired", "/srv/anvils/retired", blockedErr())

	s.pruneBlocked(map[string]string{"heimdall": "/srv/anvils/heimdall"})

	assert.Contains(t, store.rows, "heimdall")
	assert.NotContains(t, store.rows, "retired")

	// An empty anvil set is a config that has not loaded, not a config with no
	// anvils: pruning against it would wipe every outstanding entry.
	before := store.pruneCalls
	s.pruneBlocked(nil)
	assert.Equal(t, before, store.pruneCalls)
	assert.Contains(t, store.rows, "heimdall")
}

// TestBlockedDetailNamesTheAnvilAndTheEvidence covers the seam the sibling bead
// (Forge-0uvl) takes over: what the escalation SAYS. It must already carry the
// checkout, git's own words, and the fact that the anvil is unscanned rather
// than up to date.
func TestBlockedDetailNamesTheAnvilAndTheEvidence(t *testing.T) {
	detail := blockedFailureDetail("heimdall", "/srv/anvils/heimdall", blockedErr())

	assert.Contains(t, detail, "/srv/anvils/heimdall")
	assert.Contains(t, detail, "You are not currently on a branch")
	assert.Contains(t, detail, "not being scanned")
	assert.Contains(t, detail, "clears this entry automatically")
}

// TestFailureEvidenceIsSanitisedAndBounded: the text is git's, and git's is
// partly the remote's — a server-side hook's rejection is echoed verbatim — so
// it reaches a rendered attention row and a feed line as text Forge did not
// write.
func TestFailureEvidenceIsSanitisedAndBounded(t *testing.T) {
	hostile := &gitError{
		Args:   []string{"fetch"},
		Stderr: "remote: \x1b[31mrejected\x1b[0m\nremote: line two\r\nfatal: could not fetch",
		Err:    errors.New("exit status 1"),
	}
	got := failureEvidence(hostile)
	assert.NotContains(t, got, "\x1b")
	assert.NotContains(t, got, "\n")
	assert.Contains(t, got, "rejected")
	assert.Contains(t, got, "could not fetch")

	long := &gitError{Args: []string{"fetch"}, Stderr: strings.Repeat("x", maxFailureDetailBytes*2), Err: errors.New("exit status 1")}
	assert.LessOrEqual(t, len(failureEvidence(long)), maxFailureDetailBytes+len("…"))

	// An error that never went through runGit still has to say something.
	assert.Equal(t, "resolving upstream: no upstream tracking ref",
		failureEvidence(fmt.Errorf("resolving upstream: %w", ErrNoUpstream)))
	assert.Empty(t, failureEvidence(nil))
}

// TestScanAnvilEscalatesAndClearsAgainstARealCheckout drives the whole path
// through scanAnvil with git actually running: a detached HEAD has no upstream
// to fetch and no branch to fall back on, which is a real, permanent, operator-
// fixable condition — and reattaching it is what withdraws the entry.
func TestScanAnvilEscalatesAndClearsAgainstARealCheckout(t *testing.T) {
	requireGit(t)

	clone := newOriginAndClone(t, map[string]string{"README.md": "seed\n"})
	s, store, events := newTestScanner()
	s.timeout = 30 * time.Second

	git(t, clone, "checkout", "--detach")

	s.scanAnvil(context.Background(), "heimdall", clone)
	s.scanAnvil(context.Background(), "heimdall", clone)

	require.Len(t, events.ofType(state.EventDepcheckFailed), 1,
		"a detached HEAD reproduces every run and is announced once")
	require.Contains(t, store.rows, "heimdall")

	git(t, clone, "checkout", "main")
	s.scanAnvil(context.Background(), "heimdall", clone)

	assert.NotContains(t, store.rows, "heimdall", "a scan that read the manifests withdraws the entry")
	assert.Len(t, events.ofType(state.EventDepcheckFailed), 1)
}
