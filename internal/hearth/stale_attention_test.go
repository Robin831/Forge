package hearth

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

func staleAttentionItem() NeedsAttentionItem {
	return NeedsAttentionItem{
		Anvil:          "explorer",
		Title:          "Anvil explorer: PR reconcile has not completed in 2 days",
		Reason:         "The PR reconcile for anvil explorer last completed 2 days ago, against a threshold of 2 hours.",
		ReasonCategory: AttentionCheckStale,
		Kind:           state.AttentionKindStale,
	}
}

// Without the branch in classifyAttentionReason a stale entry falls through to
// the generic needs-human path and renders "⚠ UNKNOWN" — its reason text names
// neither a circuit breaker nor a stall, so nothing else would catch it.
func TestClassifyAttentionReason_CheckStale(t *testing.T) {
	got := classifyAttentionReason(state.NeedsAttentionBead{
		Anvil:      "explorer",
		Kind:       state.AttentionKindStale,
		NeedsHuman: true,
		Reason:     "The PR reconcile for anvil explorer last completed 2 days ago.",
	})
	if got != AttentionCheckStale {
		t.Fatalf("classifyAttentionReason = %v, want AttentionCheckStale", got)
	}
	if icon := attentionReasonIcon(got); !strings.Contains(icon, "STALE") {
		t.Errorf("expected a STALE label, got %q", icon)
	}
}

// The branch that matters most: nonBeadAttentionHint has no default-by-kind, so
// a stale entry without its own branch silently renders the wedged-beads
// remediation — an instruction to resolve a merge conflict that does not exist,
// for a condition Forge has explicitly declined to diagnose.
func TestStaleAttentionItem_NotBeadScopedAndHasItsOwnHint(t *testing.T) {
	item := staleAttentionItem()
	if item.IsBeadScoped() {
		t.Fatal("a stale checker carries no bead")
	}
	if !item.IsCheckStale() || item.IsAnvil() || item.IsDeploy() || item.IsDepcheckBlocked() {
		t.Fatalf("kind predicates disagree: %+v", item)
	}

	hint := nonBeadAttentionHint(item)
	if strings.Contains(hint, "merge conflict") {
		t.Errorf("hint %q is the wedged-anvil remediation, which does not apply here", hint)
	}
	if !strings.Contains(hint, "not saying why") {
		t.Errorf("hint %q should decline to name a cause", hint)
	}
	if !strings.Contains(hint, "completed cycle") {
		t.Errorf("hint %q should say what clears it", hint)
	}
}

// A stale entry clears when the checker completes a cycle, never because a bead
// or PR action happened to run on the same anvil.
func TestRemoveNeedsAttentionItem_SkipsStaleEntries(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		staleAttentionItem(),
		{Anvil: "explorer", PRID: 7, PRNumber: 42, Title: "PR #42"},
	}

	m.removeNeedsAttentionItem("", "explorer", 7)
	if len(m.needsAttention) != 1 {
		t.Fatalf("expected exactly the PR entry to be removed, got %d items", len(m.needsAttention))
	}
	if !m.needsAttention[0].IsCheckStale() {
		t.Fatalf("the surviving entry must be the stale checker, got %+v", m.needsAttention[0])
	}

	m.removeNeedsAttentionItem("", "explorer")
	if len(m.needsAttention) != 1 {
		t.Fatalf("a stale entry must not be removable by a bead/PR action, got %d items", len(m.needsAttention))
	}
}

func TestRenderNeedsAttention_CheckStale(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{staleAttentionItem()}
	m.focused = PanelNeedsAttention

	rendered := m.renderNeedsAttention(120, 20)
	if !strings.Contains(rendered, "STALE") {
		t.Errorf("expected the STALE label:\n%s", rendered)
	}
	if !strings.Contains(rendered, "PR reconcile has not completed") {
		t.Errorf("expected the headline:\n%s", rendered)
	}
	if strings.Contains(rendered, "STALE  Anvil") {
		t.Errorf("empty bead id must not leave a double space before the title:\n%s", rendered)
	}
}
