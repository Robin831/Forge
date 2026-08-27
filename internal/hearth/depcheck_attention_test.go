package hearth

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// depcheckAttentionItem is the item FetchNeedsAttention builds from a blocked
// dependency-scan row.
func depcheckAttentionItem() NeedsAttentionItem {
	return NeedsAttentionItem{
		Anvil: "heimdall",
		Title: "Anvil heimdall: dependency scan blocked",
		Reason: "Its dependency manifests cannot be read from the upstream ref, so the anvil is not being scanned at all " +
			"(it is unscanned, not up to date). Checkout: /srv/anvils/heimdall. " +
			"git said: fatal: You are not currently on a branch.",
		ReasonCategory: AttentionDepcheckBlocked,
		Kind:           state.AttentionKindDepcheck,
	}
}

// TestClassifyAttentionReason_DepcheckBlocked verifies a blocked-scan entry is
// classified by kind. Without the branch it falls through to the generic
// needs-human classification and renders "⚠ UNKNOWN" — its reason text mentions
// neither a circuit breaker nor a stall, so nothing else would catch it.
func TestClassifyAttentionReason_DepcheckBlocked(t *testing.T) {
	got := classifyAttentionReason(state.NeedsAttentionBead{
		Anvil:      "heimdall",
		Kind:       state.AttentionKindDepcheck,
		NeedsHuman: true,
		Reason:     "git said: fatal: You are not currently on a branch.",
	})
	if got != AttentionDepcheckBlocked {
		t.Fatalf("classifyAttentionReason = %v, want AttentionDepcheckBlocked", got)
	}
	if icon := attentionReasonIcon(got); !strings.Contains(icon, "DEPCHECK") {
		t.Errorf("expected a DEPCHECK label, got %q", icon)
	}
}

// TestDepcheckAttentionItem_NotBeadScoped is the branch that matters most:
// nonBeadAttentionHint has no default-by-kind, so a blocked-scan entry whose
// predicate was dropped (or whose Kind string drifted between state and hearth)
// silently renders the wedged-beads remediation — an instruction to resolve a
// merge conflict that does not exist.
func TestDepcheckAttentionItem_NotBeadScoped(t *testing.T) {
	item := depcheckAttentionItem()
	if item.IsBeadScoped() {
		t.Fatal("a blocked dependency scan carries no bead")
	}
	if !item.IsDepcheckBlocked() || item.IsAnvil() || item.IsDeploy() {
		t.Fatalf("kind predicates disagree: %+v", item)
	}

	hint := nonBeadAttentionHint(item)
	if !strings.Contains(hint, "dependency") || !strings.Contains(hint, "checkout") {
		t.Errorf("hint %q should name the scan and the checkout that resolves it", hint)
	}
	if strings.Contains(hint, "merge conflict") {
		t.Errorf("hint %q is the wedged-anvil remediation, which does not apply here", hint)
	}
}

// TestRenderNeedsAttention_DepcheckBlocked is the end of the plumbing: the row
// carries its own label, the anvil, and what git said.
func TestRenderNeedsAttention_DepcheckBlocked(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{depcheckAttentionItem()}
	m.focused = PanelNeedsAttention

	rendered := m.renderNeedsAttention(120, 20)
	if !strings.Contains(rendered, "DEPCHECK") {
		t.Errorf("expected the DEPCHECK label:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Anvil heimdall: dependency scan blocked") {
		t.Errorf("expected the anvil headline:\n%s", rendered)
	}
	if strings.Contains(rendered, "DEPCHECK  Anvil") {
		t.Errorf("empty bead id must not leave a double space before the title:\n%s", rendered)
	}
}

// TestRemoveNeedsAttentionItem_SkipsDepcheckEntries guards the bead-id=""
// fallback in the removal matcher: a blocked-scan entry clears when the anvil
// scans again, never because a bead or PR action ran on the same anvil.
func TestRemoveNeedsAttentionItem_SkipsDepcheckEntries(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		depcheckAttentionItem(),
		{Anvil: "heimdall", PRID: 7, PRNumber: 42, Title: "PR #42"},
	}

	m.removeNeedsAttentionItem("", "heimdall", 7)
	if len(m.needsAttention) != 1 {
		t.Fatalf("expected exactly the PR entry to be removed, got %d items", len(m.needsAttention))
	}
	if !m.needsAttention[0].IsDepcheckBlocked() {
		t.Fatalf("the surviving entry must be the blocked scan, got %+v", m.needsAttention[0])
	}

	m.removeNeedsAttentionItem("", "heimdall")
	if len(m.needsAttention) != 1 {
		t.Fatalf("the blocked scan must not be removable by a bead/PR action, got %d items", len(m.needsAttention))
	}
}
