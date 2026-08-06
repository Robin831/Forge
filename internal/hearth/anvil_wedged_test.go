package hearth

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// TestClassifyAttentionReason_AnvilWedged verifies that an anvil-level entry is
// classified by kind rather than by matching its reason text.
func TestClassifyAttentionReason_AnvilWedged(t *testing.T) {
	got := classifyAttentionReason(state.NeedsAttentionBead{
		Anvil:      "munin",
		Kind:       state.AttentionKindAnvil,
		NeedsHuman: true,
		Reason:     "Beads database is mid-merge with unresolved conflicts. Conflicted tables: issues (3).",
	})
	if got != AttentionAnvilWedged {
		t.Fatalf("classifyAttentionReason = %v, want AttentionAnvilWedged", got)
	}

	// A bead entry must be unaffected by the new branch.
	got = classifyAttentionReason(state.NeedsAttentionBead{
		BeadID:     "bd-1",
		Anvil:      "munin",
		NeedsHuman: true,
		Reason:     "circuit breaker: too many dispatch failures",
	})
	if got != AttentionDispatchExhausted {
		t.Fatalf("classifyAttentionReason = %v, want AttentionDispatchExhausted", got)
	}
}

// TestRenderNeedsAttention_WedgedAnvil verifies the wedged anvil renders with its
// own label and no leading-space artifact from the empty bead ID.
func TestRenderNeedsAttention_WedgedAnvil(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{{
		Anvil:          "munin",
		Title:          "Anvil munin: beads merge conflict",
		Reason:         "Conflicted tables: issues (3). Total conflicts: 3. Divergence: beads-sync ahead 1 / behind 10.",
		ReasonCategory: AttentionAnvilWedged,
		Kind:           state.AttentionKindAnvil,
	}}
	m.focused = PanelNeedsAttention

	rendered := m.renderNeedsAttention(100, 20)
	if !strings.Contains(rendered, "WEDGED") {
		t.Errorf("expected the WEDGED label:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Anvil munin: beads merge conflict") {
		t.Errorf("expected the anvil title:\n%s", rendered)
	}
	if strings.Contains(rendered, "WEDGED  Anvil") {
		t.Errorf("empty bead id must not leave a double space before the title:\n%s", rendered)
	}
}

// TestRemoveNeedsAttentionItem_SkipsAnvilEntries guards the bead-id="" fallback
// in the removal matcher: dismissing a non-bead PR must not silently drop an
// anvil entry that happens to share the anvil.
func TestRemoveNeedsAttentionItem_SkipsAnvilEntries(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		{Anvil: "munin", Title: "Anvil munin: beads merge conflict", Kind: state.AttentionKindAnvil},
		{Anvil: "munin", PRID: 7, PRNumber: 42, Title: "PR #42"},
	}

	m.removeNeedsAttentionItem("", "munin", 7)
	if len(m.needsAttention) != 1 {
		t.Fatalf("expected exactly the PR entry to be removed, got %d items", len(m.needsAttention))
	}
	if !m.needsAttention[0].IsAnvil() {
		t.Fatalf("the surviving entry must be the anvil entry, got %+v", m.needsAttention[0])
	}

	// Without a PR id the matcher must still not fall through to the anvil entry.
	m.removeNeedsAttentionItem("", "munin")
	if len(m.needsAttention) != 1 {
		t.Fatalf("the anvil entry must not be removable by a bead/PR action, got %d items", len(m.needsAttention))
	}
}
