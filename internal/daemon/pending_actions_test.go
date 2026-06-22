package daemon

import (
	"testing"

	"github.com/Robin831/Forge/internal/lifecycle"
)

// TestParkPendingAction_NoClobberAcrossTypes is the regression test for the
// parked-action overwrite bug (PR #4257 / Fhi.Metadata-hyc4g): a CI-fix parked
// while a bead was in flight used to overwrite an already-parked review-fix in
// the single latest-wins slot, silently dropping the action that fixes the PR
// and stranding it in needs_fix. Distinct action types must now coexist;
// latest-wins applies only within a single type.
func TestParkPendingAction_NoClobberAcrossTypes(t *testing.T) {
	d := &Daemon{}
	const bead = "Fhi.Metadata-hyc4g"

	review := lifecycle.ActionRequest{Action: lifecycle.ActionFixReview, BeadID: bead, PRNumber: 4257, Branch: "b1"}
	ci := lifecycle.ActionRequest{Action: lifecycle.ActionFixCI, BeadID: bead, PRNumber: 4257, Branch: "b1"}

	d.parkPendingAction(bead, review)
	d.parkPendingAction(bead, ci) // must NOT clobber the parked review-fix

	v, ok := d.pendingActions.Load(bead)
	if !ok {
		t.Fatal("expected parked actions for bead, found none")
	}
	actions := v.(map[lifecycle.Action]lifecycle.ActionRequest)
	if len(actions) != 2 {
		t.Fatalf("expected 2 parked actions (CI + review), got %d: %v", len(actions), actions)
	}
	if _, ok := actions[lifecycle.ActionFixReview]; !ok {
		t.Error("parked review-fix was clobbered by the CI-fix")
	}
	if _, ok := actions[lifecycle.ActionFixCI]; !ok {
		t.Error("parked CI-fix missing")
	}

	// Latest-wins WITHIN a type: re-parking review-fix replaces, doesn't grow.
	d.parkPendingAction(bead, lifecycle.ActionRequest{Action: lifecycle.ActionFixReview, BeadID: bead, PRNumber: 4257, Branch: "b2"})
	v, _ = d.pendingActions.Load(bead)
	actions = v.(map[lifecycle.Action]lifecycle.ActionRequest)
	if len(actions) != 2 {
		t.Fatalf("re-parking same type should not grow the set; got %d", len(actions))
	}
	if got := actions[lifecycle.ActionFixReview].Branch; got != "b2" {
		t.Errorf("latest-wins within type failed: branch = %q, want b2", got)
	}
}

// TestPopPriorityAction verifies the deterministic drain order: CI fix first,
// then review fix, then rebase, then any other action by ascending value.
func TestPopPriorityAction(t *testing.T) {
	mk := func(a lifecycle.Action) lifecycle.ActionRequest { return lifecycle.ActionRequest{Action: a} }

	cases := []struct {
		name string
		in   map[lifecycle.Action]lifecycle.ActionRequest
		want lifecycle.Action
		ok   bool
	}{
		{"empty", map[lifecycle.Action]lifecycle.ActionRequest{}, lifecycle.ActionNone, false},
		{
			"ci before review",
			map[lifecycle.Action]lifecycle.ActionRequest{lifecycle.ActionFixReview: mk(lifecycle.ActionFixReview), lifecycle.ActionFixCI: mk(lifecycle.ActionFixCI)},
			lifecycle.ActionFixCI, true,
		},
		{
			"review before rebase",
			map[lifecycle.Action]lifecycle.ActionRequest{lifecycle.ActionRebase: mk(lifecycle.ActionRebase), lifecycle.ActionFixReview: mk(lifecycle.ActionFixReview)},
			lifecycle.ActionFixReview, true,
		},
		{
			"unlisted action falls back deterministically",
			map[lifecycle.Action]lifecycle.ActionRequest{lifecycle.ActionCleanup: mk(lifecycle.ActionCleanup)},
			lifecycle.ActionCleanup, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := popPriorityAction(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got.Action != tc.want {
				t.Errorf("popped %v, want %v", got.Action, tc.want)
			}
		})
	}
}
