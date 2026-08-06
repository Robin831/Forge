package hearth

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// deployAttentionItem is the item FetchNeedsAttention builds from a rolled-back
// self-deploy row.
func deployAttentionItem() NeedsAttentionItem {
	return NeedsAttentionItem{
		Anvil:          "forge",
		Title:          "Self-deploy rolled back: restart failed (unit forge)",
		Reason:         "attempted cafebabe0123; restored deadbeef9876; restart failed: signal: killed; at 2026-08-06T12:00:00Z",
		ReasonCategory: AttentionSelfDeploy,
		Kind:           state.AttentionKindDeploy,
	}
}

// TestClassifyAttentionReason_SelfDeploy verifies a deploy entry is classified
// by kind rather than by matching its reason text — which mentions "failed" and
// would otherwise fall through to a bead category.
func TestClassifyAttentionReason_SelfDeploy(t *testing.T) {
	got := classifyAttentionReason(state.NeedsAttentionBead{
		Anvil:      "forge",
		Kind:       state.AttentionKindDeploy,
		NeedsHuman: true,
		Reason:     "attempted cafebabe0123; restored deadbeef9876; restart failed: signal: killed",
	})
	if got != AttentionSelfDeploy {
		t.Fatalf("classifyAttentionReason = %v, want AttentionSelfDeploy", got)
	}
}

// TestRenderNeedsAttention_SelfDeploy is the end of the plumbing: the operator
// sees the deploy label, what it tried to run, and what is running instead.
func TestRenderNeedsAttention_SelfDeploy(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{deployAttentionItem()}
	m.focused = PanelNeedsAttention

	rendered := m.renderNeedsAttention(120, 20)
	if !strings.Contains(rendered, "DEPLOY") {
		t.Errorf("expected the DEPLOY label:\n%s", rendered)
	}
	for _, want := range []string{"Self-deploy rolled back", "attempted cafebabe0123", "restored deadbeef9876"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected %q in the row:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "DEPLOY  Self-deploy") {
		t.Errorf("empty bead id must not leave a double space before the title:\n%s", rendered)
	}
}

// TestDeployAttentionItem_NotBeadScoped guards the action paths: a deploy entry
// carries no bead, so the bead-scoped verbs must skip it exactly as they skip a
// wedged anvil.
func TestDeployAttentionItem_NotBeadScoped(t *testing.T) {
	item := deployAttentionItem()
	if item.IsBeadScoped() {
		t.Fatal("a deploy entry is not bead-scoped")
	}
	if !item.IsDeploy() || item.IsAnvil() {
		t.Fatalf("kind predicates disagree: %+v", item)
	}
	if hint := nonBeadAttentionHint(item); !strings.Contains(hint, "self-deploy") {
		t.Errorf("hint %q should explain what resolves a deploy entry", hint)
	}
}

// TestRemoveNeedsAttentionItem_SkipsDeployEntries pins the bead-id="" fallback
// in the removal matcher: dismissing a non-bead PR must not silently drop the
// deploy entry that happens to share the anvil.
func TestRemoveNeedsAttentionItem_SkipsDeployEntries(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		deployAttentionItem(),
		{Anvil: "forge", PRID: 7, PRNumber: 42, Title: "PR #42"},
	}

	m.removeNeedsAttentionItem("", "forge", 7)
	if len(m.needsAttention) != 1 {
		t.Fatalf("expected exactly the PR entry to be removed, got %d items", len(m.needsAttention))
	}
	if !m.needsAttention[0].IsDeploy() {
		t.Fatalf("the surviving entry must be the deploy entry, got %+v", m.needsAttention[0])
	}

	m.removeNeedsAttentionItem("", "forge")
	if len(m.needsAttention) != 1 {
		t.Fatalf("the deploy entry must not be removable by a bead/PR action, got %d items", len(m.needsAttention))
	}
}
