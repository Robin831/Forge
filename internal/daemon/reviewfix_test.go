package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/burnish"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

const reviewFixAnvil = "munin"

// headVCS is a VCS provider whose only job is to report a PR head SHA, which
// is the single input the review-fix circuit breaker reads.
type headVCS struct {
	*mockVCSProvider
	head string
	err  error
}

func (h *headVCS) CheckStatusLight(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	if h.err != nil {
		return nil, h.err
	}
	return &vcs.PRStatus{HeadSHA: h.head}, nil
}

func newReviewFixDaemon(t *testing.T, v *headVCS) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:           db,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders: map[string]vcs.Provider{reviewFixAnvil: v},
	}
	d.cfg.Store(&config.Config{
		Anvils:   map[string]config.AnvilConfig{reviewFixAnvil: {Path: t.TempDir()}},
		Settings: config.SettingsConfig{MaxSameHeadReviewFixes: 2},
	})
	return d
}

func reviewFixRequest() lifecycle.ActionRequest {
	return lifecycle.ActionRequest{
		Action:   lifecycle.ActionFixReview,
		PRNumber: 4727,
		BeadID:   "ext-4727",
		Anvil:    reviewFixAnvil,
		Branch:   "forge/ext-4727",
	}
}

// TestReviewFixDispatchAllowed_TripsOnUnchangedHead replays the observed loop
// (Munin PR #4727): every cycle re-detects the same changes-requested review
// against the same head and dispatches another full Smith run. The breaker must
// stop it at the budget and escalate instead of dispatching again.
func TestReviewFixDispatchAllowed_TripsOnUnchangedHead(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: "7729aad29ffffffffffffffffffffffffffffff"}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()
	anvilPath := d.cfg.Load().Anvils[reviewFixAnvil].Path

	for i := 1; i <= 2; i++ {
		if !d.reviewFixDispatchAllowed(context.Background(), req, anvilPath) {
			t.Fatalf("dispatch %d was refused; the budget is 2", i)
		}
	}
	if d.reviewFixDispatchAllowed(context.Background(), req, anvilPath) {
		t.Fatal("expected the third same-head dispatch to be refused")
	}

	r, err := d.db.GetRetry(req.BeadID, req.Anvil)
	if err != nil {
		t.Fatalf("GetRetry: %v", err)
	}
	if r == nil || !r.NeedsHuman {
		t.Fatal("expected a needs-attention entry when the breaker trips")
	}
	// The entry has to name the PR and the head so the operator can act on it.
	for _, want := range []string{"#4727", "7729aad29"} {
		if !strings.Contains(r.LastError, want) {
			t.Errorf("needs-attention message %q does not mention %q", r.LastError, want)
		}
	}
}

// TestReviewFixDispatchAllowed_ResetsOnNewHead: a PR that is genuinely
// progressing pushes a new head each round and must never be circuit-broken.
func TestReviewFixDispatchAllowed_ResetsOnNewHead(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: "head-1"}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()
	anvilPath := d.cfg.Load().Anvils[reviewFixAnvil].Path

	for i := 0; i < 6; i++ {
		if !d.reviewFixDispatchAllowed(context.Background(), req, anvilPath) {
			t.Fatalf("dispatch %d refused although the head moves every round", i+1)
		}
		// Each fix lands: the next Bellows cycle sees a new head.
		v.head = string(rune('a'+i)) + "-head"
	}

	r, err := d.db.GetRetry(req.BeadID, req.Anvil)
	if err != nil {
		t.Fatalf("GetRetry: %v", err)
	}
	if r != nil && r.NeedsHuman {
		t.Errorf("a progressing PR was escalated: %q", r.LastError)
	}
}

// TestReviewFixDispatchAllowed_FailsOpen: the breaker must never block a fix
// because the head lookup failed. A briefly unreachable `gh` is not a loop.
func TestReviewFixDispatchAllowed_FailsOpen(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: ""}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()
	anvilPath := d.cfg.Load().Anvils[reviewFixAnvil].Path

	for i := 0; i < 5; i++ {
		if !d.reviewFixDispatchAllowed(context.Background(), req, anvilPath) {
			t.Fatalf("dispatch %d refused although the head SHA is unknown", i+1)
		}
	}
}

// TestReviewFixDispatchAllowed_ManualOverride: the breaker is exactly what an
// operator hitting "fix comments" by hand is overriding.
func TestReviewFixDispatchAllowed_ManualOverride(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: "stuck-head"}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()
	anvilPath := d.cfg.Load().Anvils[reviewFixAnvil].Path

	for i := 0; i < 3; i++ {
		d.reviewFixDispatchAllowed(context.Background(), req, anvilPath)
	}
	req.IsManual = true
	if !d.reviewFixDispatchAllowed(context.Background(), req, anvilPath) {
		t.Error("a manual review fix must not be circuit-broken")
	}
}

// TestClearReviewFixDispatch_OnPRExit: a merged or closed PR leaves the loop
// for good, so its bookkeeping must not outlive it.
func TestClearReviewFixDispatch_OnPRExit(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: "h"}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()
	anvilPath := d.cfg.Load().Anvils[reviewFixAnvil].Path

	d.reviewFixDispatchAllowed(context.Background(), req, anvilPath)
	d.clearReviewFixDispatch(req)

	rec, err := d.db.GetReviewFixDispatch(req.Anvil, req.PRNumber)
	if err != nil {
		t.Fatalf("GetReviewFixDispatch: %v", err)
	}
	if rec != nil {
		t.Errorf("expected the bookkeeping to be cleared, got %+v", rec)
	}
}

func TestRecordReviewFixOutcome(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: "h"}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()
	anvilPath := d.cfg.Load().Anvils[reviewFixAnvil].Path

	cases := []struct {
		name string
		res  *burnish.FixResult
		want string
	}{
		{"verified push", &burnish.FixResult{Addressed: true}, state.ReviewFixResultPushed},
		{"unverified push", &burnish.FixResult{Addressed: true, Unverified: true}, state.ReviewFixResultUnverifiedPush},
		{"preserved", &burnish.FixResult{UnpushedHead: "abc"}, state.ReviewFixResultPreserved},
		{"failed", &burnish.FixResult{}, state.ReviewFixResultFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d.reviewFixDispatchAllowed(context.Background(), req, anvilPath)
			d.recordReviewFixOutcome(req, tc.res)
			rec, err := d.db.GetReviewFixDispatch(req.Anvil, req.PRNumber)
			if err != nil || rec == nil {
				t.Fatalf("GetReviewFixDispatch: %v (rec=%v)", err, rec)
			}
			if rec.LastResult != tc.want {
				t.Errorf("LastResult = %q, want %q", rec.LastResult, tc.want)
			}
		})
	}
}

// TestClearReviewFixAttention_KeepsPreservedWork: a merge says the branch
// landed, not that a preserved unpushed commit did. Clearing that entry would
// delete the only pointer to work nobody has looked at.
func TestClearReviewFixAttention_KeepsPreservedWork(t *testing.T) {
	v := &headVCS{mockVCSProvider: &mockVCSProvider{}, head: "h"}
	d := newReviewFixDaemon(t, v)
	req := reviewFixRequest()

	cases := []struct {
		name      string
		reason    string
		wantClear bool
	}{
		{"unverified push", burnish.AttentionUnverified + "pushed 7729aad unverified", true},
		{"circuit breaker", reviewFixLoopPrefix + "PR #4727 head abc dispatched 3", true},
		{"preserved commit", burnish.AttentionUnpushed + "commit 7729aad kept in worktree", false},
		{"unrelated escalation", "crucible child failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := d.db.MarkNeedsHuman(req.BeadID, req.Anvil, tc.reason); err != nil {
				t.Fatalf("MarkNeedsHuman: %v", err)
			}
			d.clearReviewFixDispatch(req)

			r, err := d.db.GetRetry(req.BeadID, req.Anvil)
			if err != nil || r == nil {
				t.Fatalf("GetRetry: %v (r=%v)", err, r)
			}
			if r.NeedsHuman == tc.wantClear {
				t.Errorf("NeedsHuman = %t after clear, want %t", r.NeedsHuman, !tc.wantClear)
			}
			// Reset for the next case.
			_ = d.db.ClearNeedsAttention(req.BeadID, req.Anvil)
		})
	}
}
