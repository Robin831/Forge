package hearth

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
)

// optionKeys flattens a menu to its rendered labels so a test can ask what the
// operator is offered without driving the huh widget.
func optionKeys(item *PRItem) []string {
	opts := prActionOptions(item)
	keys := make([]string, 0, len(opts))
	for _, o := range opts {
		keys = append(keys, o.Key)
	}
	return keys
}

func hasPrefixIn(keys []string, prefix string) bool {
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// TestPRActionOptions_BellowsMuteVisibility pins the menu to the PR's state:
// the mute is one item pointing in whichever direction is available, so an
// attached PR is never offered a resume it would no-op on and a muted one is
// never offered a second mute.
func TestPRActionOptions_BellowsMuteVisibility(t *testing.T) {
	tests := []struct {
		name        string
		item        PRItem
		wantStop    bool
		wantResume  bool
		wantAssign  bool
		wantRelease bool
	}{
		{
			name:     "forge PR attached offers stop",
			item:     PRItem{PRNumber: 1, BeadID: "Forge-abc1", BellowsManaged: true, CIPassing: true},
			wantStop: true,
		},
		{
			name:       "forge PR detached offers resume",
			item:       PRItem{PRNumber: 2, BeadID: "Forge-abc1", BellowsManaged: true, BellowsDetached: true, CIPassing: true},
			wantResume: true,
		},
		{
			name:       "external PR without bellows offers assign only",
			item:       PRItem{PRNumber: 3, BeadID: "ext-3", IsExternal: true, CIPassing: true},
			wantAssign: true,
		},
		{
			name:        "assigned external PR offers stop and unassign",
			item:        PRItem{PRNumber: 4, BeadID: "ext-4", IsExternal: true, BellowsManaged: true, CIPassing: true},
			wantStop:    true,
			wantRelease: true,
		},
		{
			name:        "detached external PR offers resume and unassign",
			item:        PRItem{PRNumber: 5, BeadID: "ext-5", IsExternal: true, BellowsManaged: true, BellowsDetached: true, CIPassing: true},
			wantResume:  true,
			wantRelease: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := tt.item
			keys := optionKeys(&item)

			if got := hasPrefixIn(keys, "Stop bellows"); got != tt.wantStop {
				t.Errorf("Stop bellows present = %v, want %v (menu: %v)", got, tt.wantStop, keys)
			}
			if got := hasPrefixIn(keys, "Resume bellows"); got != tt.wantResume {
				t.Errorf("Resume bellows present = %v, want %v (menu: %v)", got, tt.wantResume, keys)
			}
			if got := hasPrefixIn(keys, "Assign bellows"); got != tt.wantAssign {
				t.Errorf("Assign bellows present = %v, want %v (menu: %v)", got, tt.wantAssign, keys)
			}
			if got := hasPrefixIn(keys, "Unassign bellows"); got != tt.wantRelease {
				t.Errorf("Unassign bellows present = %v, want %v (menu: %v)", got, tt.wantRelease, keys)
			}
			// The menu's fixed items are never displaced by the new ones.
			if !hasPrefixIn(keys, "Open in browser") || !hasPrefixIn(keys, "Close PR") {
				t.Errorf("expected the always-present items to survive, got %v", keys)
			}
		})
	}
}

// prActionCapture is one dispatched pr_action, recorded off the tea.Cmd.
type prActionCapture struct {
	prID     int
	prNumber int
	anvil    string
	beadID   string
	branch   string
	action   string
}

func dispatchPRAction(t *testing.T, item PRItem, choice PRActionMenuChoice) prActionCapture {
	t.Helper()
	var got prActionCapture
	m := &Model{prActionTarget: &item}
	m.OnPRAction = func(prID, prNumber int, anvil, beadID, branch, action string) (string, error) {
		got = prActionCapture{prID: prID, prNumber: prNumber, anvil: anvil, beadID: beadID, branch: branch, action: action}
		return "", nil
	}
	cmd := m.executePRAction(choice)
	if cmd == nil {
		t.Fatalf("executePRAction(%v) returned no command", choice)
	}
	msg, ok := cmd().(PRActionResultMsg)
	if !ok {
		t.Fatalf("expected PRActionResultMsg, got %T", cmd())
	}
	if msg.Err != nil {
		t.Fatalf("unexpected error from PR action: %v", msg.Err)
	}
	if msg.Action != got.action {
		t.Errorf("result message action %q disagrees with the dispatched action %q", msg.Action, got.action)
	}
	return got
}

// TestExecutePRAction_BellowsMuteVerbs checks that the two menu items send the
// wire verbs the daemon actually handles, and that both addressing forms travel
// with them: the daemon resolves the PR through resolvePRTargetPreferID, which
// takes the row id when there is one and falls back to number+anvil — the form
// an externally-opened PR is addressed by.
func TestExecutePRAction_BellowsMuteVerbs(t *testing.T) {
	tests := []struct {
		name       string
		item       PRItem
		choice     PRActionMenuChoice
		wantAction string
	}{
		{
			name:       "forge-authored PR detaches",
			item:       PRItem{PRID: 17, PRNumber: 431, Anvil: "heimdall", BeadID: "Forge-abc1", Branch: "forge/Forge-abc1", BellowsManaged: true},
			choice:     PRActionStopBellows,
			wantAction: ipc.PRActionDetachBellows,
		},
		{
			name:       "forge-authored PR reattaches",
			item:       PRItem{PRID: 17, PRNumber: 431, Anvil: "heimdall", BeadID: "Forge-abc1", Branch: "forge/Forge-abc1", BellowsManaged: true, BellowsDetached: true},
			choice:     PRActionResumeBellows,
			wantAction: ipc.PRActionReattachBellows,
		},
		{
			name:       "external PR detaches",
			item:       PRItem{PRID: 42, PRNumber: 908, Anvil: "munin", BeadID: "ext-908", IsExternal: true, BellowsManaged: true},
			choice:     PRActionStopBellows,
			wantAction: ipc.PRActionDetachBellows,
		},
		{
			name:       "external PR reattaches",
			item:       PRItem{PRID: 42, PRNumber: 908, Anvil: "munin", BeadID: "ext-908", IsExternal: true, BellowsManaged: true, BellowsDetached: true},
			choice:     PRActionResumeBellows,
			wantAction: ipc.PRActionReattachBellows,
		},
		{
			name:       "external PR unassigns",
			item:       PRItem{PRID: 42, PRNumber: 908, Anvil: "munin", BeadID: "ext-908", IsExternal: true, BellowsManaged: true},
			choice:     PRActionUnassignBellows,
			wantAction: "unassign_bellows",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dispatchPRAction(t, tt.item, tt.choice)
			if got.action != tt.wantAction {
				t.Errorf("dispatched action = %q, want %q", got.action, tt.wantAction)
			}
			if got.prID != tt.item.PRID {
				t.Errorf("pr id = %d, want %d", got.prID, tt.item.PRID)
			}
			if got.prNumber != tt.item.PRNumber || got.anvil != tt.item.Anvil {
				t.Errorf("target = #%d on %q, want #%d on %q: the number+anvil pair is what addresses a PR with no row id",
					got.prNumber, got.anvil, tt.item.PRNumber, tt.item.Anvil)
			}
			if got.beadID != tt.item.BeadID {
				t.Errorf("bead id = %q, want %q", got.beadID, tt.item.BeadID)
			}
		})
	}
}

// TestExecutePRAction_NoDaemonDoesNotDispatch pins the guard: with no IPC hook
// the mute reports itself unavailable rather than looking like it took.
func TestExecutePRAction_NoDaemonDoesNotDispatch(t *testing.T) {
	item := PRItem{PRID: 1, PRNumber: 2, Anvil: "a", BellowsManaged: true}
	m := &Model{prActionTarget: &item}
	if cmd := m.executePRAction(PRActionStopBellows); cmd != nil {
		t.Fatalf("expected no command when OnPRAction is nil")
	}
	if !m.statusMsgIsError {
		t.Errorf("expected an error status, got %q", m.statusMsg)
	}
}

// TestPRActionStatusWording keeps the TUI's vocabulary in step with
// `forge bellows stop|resume`: the wire verb names the call, not what changed.
func TestPRActionStatusWording(t *testing.T) {
	if got := prActionLabel(ipc.PRActionDetachBellows); got != "bellows stop" {
		t.Errorf("prActionLabel(detach) = %q", got)
	}
	if got := prActionLabel(ipc.PRActionReattachBellows); got != "bellows resume" {
		t.Errorf("prActionLabel(reattach) = %q", got)
	}
	if got := prActionLabel("merge"); got != "merge" {
		t.Errorf("prActionLabel must pass unknown verbs through, got %q", got)
	}

	stopped := prActionDoneStatus(431, ipc.PRActionDetachBellows)
	if !strings.Contains(stopped, "bellows stopped") || strings.Contains(stopped, ipc.PRActionDetachBellows) {
		t.Errorf("detach confirmation should say what changed, got %q", stopped)
	}
	resumed := prActionDoneStatus(431, ipc.PRActionReattachBellows)
	if !strings.Contains(resumed, "bellows resumed") || strings.Contains(resumed, ipc.PRActionReattachBellows) {
		t.Errorf("reattach confirmation should say what changed, got %q", resumed)
	}
	if got := prActionDoneStatus(431, "merge"); !strings.Contains(got, "merge dispatched") {
		t.Errorf("unknown verbs keep the generic confirmation, got %q", got)
	}
}

// TestRenderPRPanel_DetachedRowIsDistinct is the rendering half: a muted PR
// must not read like one that is actively being worked.
func TestRenderPRPanel_DetachedRowIsDistinct(t *testing.T) {
	m := &Model{width: 120, height: 40}
	m.prItems = []PRItem{
		{PRID: 1, PRNumber: 101, Anvil: "heimdall", BeadID: "Forge-aaaa", Status: "open", CIPassing: true, BellowsManaged: true},
		{PRID: 2, PRNumber: 202, Anvil: "heimdall", BeadID: "Forge-bbbb", Status: "open", CIPassing: true, BellowsManaged: true, BellowsDetached: true},
	}

	out := m.renderPRPanel()
	lines := strings.Split(out, "\n")

	var attached, detached string
	for _, l := range lines {
		if strings.Contains(l, "#101") {
			attached = l
		}
		if strings.Contains(l, "#202") {
			detached = l
		}
	}
	if attached == "" || detached == "" {
		t.Fatalf("expected both PR rows in the panel, got:\n%s", out)
	}
	if !strings.Contains(detached, "[detached]") {
		t.Errorf("detached PR row should be marked, got %q", detached)
	}
	if strings.Contains(attached, "[detached]") {
		t.Errorf("attached PR row must not be marked, got %q", attached)
	}
}

// TestPRDetachedTag covers the marker in isolation, including that an attached
// PR contributes nothing at all to the row.
func TestPRDetachedTag(t *testing.T) {
	if got := prDetachedTag(PRItem{}); got != "" {
		t.Errorf("attached PR should render no tag, got %q", got)
	}
	if got := prDetachedTag(PRItem{BellowsDetached: true}); !strings.Contains(got, "detached") {
		t.Errorf("detached PR should render a tag, got %q", got)
	}
}
