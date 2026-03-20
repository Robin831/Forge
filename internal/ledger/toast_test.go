package ledger

import (
	"strings"
	"testing"
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
	if len(got) > 25 { // visual width ≤ 20 + "..." suffix
		t.Errorf("expected truncated string, got length %d: %q", len(got), got)
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
