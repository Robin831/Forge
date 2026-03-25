package ledger

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestTruncateToast(t *testing.T) {
	// Short string — no truncation.
	s := "hello"
	got := truncateToast(s, 60)
	if got != s {
		t.Errorf("expected %q, got %q", s, got)
	}

	// Long string — should be truncated with "...".
	long := strings.Repeat("x", 100)
	got = truncateToast(long, 20)
	if lipgloss.Width(got) > 20 {
		t.Errorf("expected visual width ≤ 20, got %d: %q", lipgloss.Width(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected '...' suffix, got %q", got)
	}
}

func TestAddAndDismissToast(t *testing.T) {
	m := &Model{}

	m.addToast("success message", false)
	if len(m.toasts) != 1 {
		t.Fatalf("expected 1 toast, got %d", len(m.toasts))
	}
	if m.toasts[0].isError {
		t.Error("expected success toast, got error")
	}

	m.addToast("error message", true)
	if len(m.toasts) != 2 {
		t.Fatalf("expected 2 toasts, got %d", len(m.toasts))
	}

	// Dismiss first toast.
	m.dismissToast(m.toasts[0].id)
	if len(m.toasts) != 1 {
		t.Fatalf("expected 1 toast after dismiss, got %d", len(m.toasts))
	}
}

func TestMaxToasts(t *testing.T) {
	m := &Model{}
	for i := 0; i < maxToasts+2; i++ {
		m.addToast("msg", false)
	}
	if len(m.toasts) != maxToasts {
		t.Errorf("expected %d toasts, got %d", maxToasts, len(m.toasts))
	}
}

func TestRenderToastsEmpty(t *testing.T) {
	m := &Model{}
	got := m.renderToasts()
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestRenderToastsNonEmpty(t *testing.T) {
	m := &Model{}
	m.addToast("hello", false)
	m.addToast("oops", true)
	got := m.renderToasts()
	if got == "" {
		t.Error("expected non-empty toast render")
	}
	// Should contain both messages.
	if !strings.Contains(got, "hello") {
		t.Error("expected toast to contain 'hello'")
	}
	if !strings.Contains(got, "oops") {
		t.Error("expected toast to contain 'oops'")
	}
}

// TestPendingToastOnlyWhenActionNonNil verifies that a pending toast is enqueued
// only when executeFormAction returns a non-nil cmd, and NOT for intentional no-op
// submits (e.g. FormViewDeps with "done" selected, FormLabel with empty label).
func TestPendingToastOnlyWhenActionNonNil(t *testing.T) {
	target := &Bead{ID: "Forge-abc1", Anvil: "test"}

	tests := []struct {
		name       string
		setupModel func(m *Model)
		wantToast  bool
	}{
		{
			name: "FormCloseBead with valid target enqueues pending toast",
			setupModel: func(m *Model) {
				m.activeFormKind = FormCloseBead
				m.formTarget = target
			},
			wantToast: true,
		},
		{
			name: "FormViewDeps with empty depID (done selected) does not enqueue toast",
			setupModel: func(m *Model) {
				m.activeFormKind = FormViewDeps
				m.formTarget = target
				m.formDepID = "" // "— done —" was selected
			},
			wantToast: false,
		},
		{
			name: "FormLabel with empty label does not enqueue toast",
			setupModel: func(m *Model) {
				m.activeFormKind = FormLabel
				m.formTarget = target
				m.formLabel = ""
			},
			wantToast: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{
				anvils: map[string]string{"test": "/tmp/test"},
			}
			tt.setupModel(m)

			pendingMsg := m.pendingToastForForm()
			actionCmd := m.executeFormAction()

			// Simulate the conditional toast logic from updateForm.
			if actionCmd != nil {
				m.addToast(pendingMsg, false)
			}

			if tt.wantToast && len(m.toasts) == 0 {
				t.Error("expected a pending toast to be enqueued, but got none")
			}
			if !tt.wantToast && len(m.toasts) > 0 {
				t.Errorf("expected no pending toast for no-op submit, but got %q", m.toasts[0].message)
			}
		})
	}
}
