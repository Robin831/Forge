package hearth

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestTruncateToVisualWidth_ASCIIFits verifies that ASCII strings within the
// limit are returned unchanged (no suffix appended).
func TestTruncateToVisualWidth_ASCIIFits(t *testing.T) {
	s := "hello world"
	got := truncateToVisualWidth(s, 80, "...")
	if got != s {
		t.Errorf("expected %q unchanged, got %q", s, got)
	}
}

// TestTruncateToVisualWidth_ASCIITruncated verifies ASCII strings longer than
// maxWidth are truncated and the suffix is appended.
func TestTruncateToVisualWidth_ASCIITruncated(t *testing.T) {
	s := strings.Repeat("a", 70)
	got := truncateToVisualWidth(s, 60, "...")
	if lipgloss.Width(got) > 60 {
		t.Errorf("got visual width %d > 60: %q", lipgloss.Width(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected suffix '...', got %q", got)
	}
}

// TestTruncateToVisualWidth_WideChars ensures wide (CJK) characters do not
// cause a panic or produce output exceeding maxWidth.
func TestTruncateToVisualWidth_WideChars(t *testing.T) {
	// Each CJK character has visual width 2. 35 chars = visual width 70.
	// With maxWidth=60 a rune-count slice [:57] would panic (only 35 runes).
	s := strings.Repeat("日", 35) // visual width 70
	got := truncateToVisualWidth(s, 60, "...")
	w := lipgloss.Width(got)
	if w > 60 {
		t.Errorf("wide-char truncation produced visual width %d > 60", w)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected suffix '...', got %q", got)
	}
}

// TestTruncateToVisualWidth_ExactBoundary verifies a string whose width exactly
// equals maxWidth is returned unchanged.
func TestTruncateToVisualWidth_ExactBoundary(t *testing.T) {
	s := strings.Repeat("x", 60)
	got := truncateToVisualWidth(s, 60, "...")
	if got != s {
		t.Errorf("expected exact-width string unchanged, got %q", got)
	}
}

// TestTruncateToVisualWidth_EmptyString verifies empty input returns empty.
func TestTruncateToVisualWidth_EmptyString(t *testing.T) {
	got := truncateToVisualWidth("", 60, "...")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// TestRenderToasts_Empty verifies that an empty toast list returns "".
func TestRenderToasts_Empty(t *testing.T) {
	m := &Model{}
	got := m.renderToasts()
	if got != "" {
		t.Errorf("expected empty string for no toasts, got %q", got)
	}
}

// TestRenderToasts_SingleSuccess verifies a success toast is rendered.
func TestRenderToasts_SingleSuccess(t *testing.T) {
	m := &Model{
		toasts: []toast{{id: 1, message: "PR merged", isError: false}},
	}
	got := m.renderToasts()
	if !strings.Contains(got, "PR merged") {
		t.Errorf("expected 'PR merged' in rendered toast, got %q", got)
	}
}

// TestRenderToasts_SingleError verifies an error toast is rendered.
func TestRenderToasts_SingleError(t *testing.T) {
	m := &Model{
		toasts: []toast{{id: 2, message: "Smith failed", isError: true}},
	}
	got := m.renderToasts()
	if !strings.Contains(got, "Smith failed") {
		t.Errorf("expected 'Smith failed' in rendered toast, got %q", got)
	}
}

// TestRenderToasts_MultipleToasts verifies multiple toasts are joined with newlines.
func TestRenderToasts_MultipleToasts(t *testing.T) {
	m := &Model{
		toasts: []toast{
			{id: 1, message: "Toast A", isError: false},
			{id: 2, message: "Toast B", isError: true},
		},
	}
	got := m.renderToasts()
	if !strings.Contains(got, "Toast A") {
		t.Errorf("expected 'Toast A' in output, got %q", got)
	}
	if !strings.Contains(got, "Toast B") {
		t.Errorf("expected 'Toast B' in output, got %q", got)
	}
}

// TestRenderToasts_WideCharNoPanic verifies that a wide-character message
// does not cause a panic and is truncated to fit within toastMaxWidth.
func TestRenderToasts_WideCharNoPanic(t *testing.T) {
	// 35 CJK chars = visual width 70 > toastMaxWidth (60).
	// Previous code would panic slicing []rune(text)[:57] on a 35-rune slice.
	msg := strings.Repeat("漢", 35)
	m := &Model{
		toasts: []toast{{id: 1, message: msg, isError: false}},
	}
	// Must not panic.
	got := m.renderToasts()
	if got == "" {
		t.Errorf("expected non-empty rendered toast")
	}
}

// TestRenderToasts_LongASCIITruncated verifies long ASCII messages are truncated.
func TestRenderToasts_LongASCIITruncated(t *testing.T) {
	msg := strings.Repeat("x", 80)
	m := &Model{
		toasts: []toast{{id: 1, message: msg, isError: false}},
	}
	got := m.renderToasts()
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncation with '...' in output, got %q", got)
	}
}

// TestToastForEvent_WardenPass verifies warden_pass event produces a success toast.
func TestToastForEvent_WardenPass(t *testing.T) {
	ev := EventItem{Type: "warden_pass", Message: "bd-42 review passed"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for warden_pass")
	}
	if isError {
		t.Error("expected isError=false for warden_pass")
	}
	if !strings.Contains(msg, "review passed") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_WardenHardReject verifies warden_hard_reject event produces
// an error toast with the correct message and isError=true.
func TestToastForEvent_WardenHardReject(t *testing.T) {
	t.Run("with message", func(t *testing.T) {
		ev := EventItem{Type: "warden_hard_reject", Message: "review failed permanently"}
		msg, isError, ok := toastForEvent(ev)
		if !ok {
			t.Fatal("expected ok=true for warden_hard_reject")
		}
		if !isError {
			t.Error("expected isError=true for warden_hard_reject")
		}
		if !strings.Contains(msg, "review failed permanently") {
			t.Errorf("unexpected message: %q", msg)
		}
	})
	t.Run("fallback message", func(t *testing.T) {
		ev := EventItem{Type: "warden_hard_reject", Message: ""}
		msg, isError, ok := toastForEvent(ev)
		if !ok {
			t.Fatal("expected ok=true for warden_hard_reject with empty message")
		}
		if !isError {
			t.Error("expected isError=true for warden_hard_reject")
		}
		if !strings.Contains(msg, "Warden hard-rejected") {
			t.Errorf("expected fallback 'Warden hard-rejected' in message, got %q", msg)
		}
	})
}

// TestToastForEvent_PRCreated verifies pr_created event produces a toast.
func TestToastForEvent_PRCreated(t *testing.T) {
	ev := EventItem{Type: "pr_created", Message: "PR #77 created"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for pr_created")
	}
	if isError {
		t.Error("expected isError=false for pr_created")
	}
	if !strings.Contains(msg, "PR #77") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_PRMerged verifies pr_merged event produces a toast.
func TestToastForEvent_PRMerged(t *testing.T) {
	ev := EventItem{Type: "pr_merged", Message: "PR #99 merged"}
	_, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for pr_merged")
	}
	if isError {
		t.Error("expected isError=false for pr_merged")
	}
}

// TestToastForEvent_BeadClosed verifies bead_closed event uses synthesized fallback
// "bd-55 closed" (not just the raw BeadID) when Message is empty.
func TestToastForEvent_BeadClosed(t *testing.T) {
	ev := EventItem{Type: "bead_closed", BeadID: "bd-55", Message: ""}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for bead_closed")
	}
	if isError {
		t.Error("expected isError=false for bead_closed")
	}
	if !strings.Contains(msg, "bd-55 closed") {
		t.Errorf("expected synthesized fallback 'bd-55 closed' in message, got %q", msg)
	}
}

// TestToastForEvent_SmithFailed verifies smith_failed event is an error toast.
func TestToastForEvent_SmithFailed(t *testing.T) {
	ev := EventItem{Type: "smith_failed", Message: "timeout"}
	_, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for smith_failed")
	}
	if !isError {
		t.Error("expected isError=true for smith_failed")
	}
}

// TestToastForEvent_SmithRecheck verifies smith_recheck event produces a
// non-error toast with the expected prefix and message content.
func TestToastForEvent_SmithRecheck(t *testing.T) {
	t.Run("with message", func(t *testing.T) {
		ev := EventItem{Type: "smith_recheck", Message: "Iteration 2: test runner flaked"}
		msg, isError, ok := toastForEvent(ev)
		if !ok {
			t.Fatal("expected ok=true for smith_recheck")
		}
		if isError {
			t.Error("expected isError=false for smith_recheck")
		}
		if !strings.Contains(msg, "test runner flaked") {
			t.Errorf("unexpected message: %q", msg)
		}
	})
	t.Run("fallback message", func(t *testing.T) {
		ev := EventItem{Type: "smith_recheck", Message: ""}
		msg, isError, ok := toastForEvent(ev)
		if !ok {
			t.Fatal("expected ok=true for smith_recheck with empty message")
		}
		if isError {
			t.Error("expected isError=false for smith_recheck with empty message")
		}
		if !strings.Contains(msg, "RECHECK_PREVIOUS") {
			t.Errorf("expected fallback containing 'RECHECK_PREVIOUS', got %q", msg)
		}
	})
}

// TestToastForEvent_LifecycleExhausted verifies lifecycle_exhausted is an error toast
// with synthesized fallback "bd-7 needs attention" (not just the raw BeadID).
func TestToastForEvent_LifecycleExhausted(t *testing.T) {
	ev := EventItem{Type: "lifecycle_exhausted", BeadID: "bd-7", Message: ""}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for lifecycle_exhausted")
	}
	if !isError {
		t.Error("expected isError=true for lifecycle_exhausted")
	}
	if !strings.Contains(msg, "bd-7 needs attention") {
		t.Errorf("expected synthesized fallback 'bd-7 needs attention' in message, got %q", msg)
	}
}

// TestToastForEvent_CrucibleComplete verifies crucible_complete is a success toast.
func TestToastForEvent_CrucibleComplete(t *testing.T) {
	ev := EventItem{Type: "crucible_complete", Message: "epic done"}
	_, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for crucible_complete")
	}
	if isError {
		t.Error("expected isError=false for crucible_complete")
	}
}

// TestToastForEvent_UnknownType verifies unknown event types return ok=false.
func TestToastForEvent_UnknownType(t *testing.T) {
	ev := EventItem{Type: "some_random_event", Message: "whatever"}
	_, _, ok := toastForEvent(ev)
	if ok {
		t.Error("expected ok=false for unknown event type")
	}
}

// TestToastForEvent_MultiLineMessageTrimmed verifies only the first line is used.
func TestToastForEvent_MultiLineMessageTrimmed(t *testing.T) {
	ev := EventItem{Type: "pr_created", Message: "line one\nline two\nline three"}
	msg, _, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if strings.Contains(msg, "line two") {
		t.Errorf("expected only first line, got %q", msg)
	}
	if !strings.Contains(msg, "line one") {
		t.Errorf("expected first line in message, got %q", msg)
	}
}

// TestPlaceToastsOverlay_Basics verifies the overlay function returns a string
// with the expected number of lines for the given height.
func TestPlaceToastsOverlay_Basics(t *testing.T) {
	bg := strings.Repeat("background line\n", 20)
	bg = strings.TrimSuffix(bg, "\n")
	overlay := "[ Toast notification ]"
	result := placeToastsOverlay(80, 20, 1, overlay, bg)
	lines := strings.Split(result, "\n")
	if len(lines) != 20 {
		t.Errorf("expected 20 lines, got %d", len(lines))
	}
	// The overlay should appear somewhere in the result.
	if !strings.Contains(result, "Toast notification") {
		t.Errorf("expected overlay text in result, got: %q", result)
	}
}

// TestPlaceToastsOverlay_EmptyBackground verifies the function handles a
// shorter-than-height background without panicking.
func TestPlaceToastsOverlay_EmptyBackground(t *testing.T) {
	overlay := "[ notice ]"
	// Should not panic with a shorter background.
	result := placeToastsOverlay(40, 10, 1, overlay, "")
	lines := strings.Split(result, "\n")
	if len(lines) != 10 {
		t.Errorf("expected 10 lines, got %d", len(lines))
	}
}

// TestToastForEvent_WicketBeadCreated verifies wicket_bead_created produces a success toast.
func TestToastForEvent_WicketBeadCreated(t *testing.T) {
	ev := EventItem{Type: "wicket_bead_created", Message: "bd-99 created from issue #12"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_bead_created")
	}
	if isError {
		t.Error("expected isError=false for wicket_bead_created")
	}
	if !strings.Contains(msg, "bd-99 created") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_WicketBeadCreated_Fallback verifies the fallback message when
// wicket_bead_created has an empty event message.
func TestToastForEvent_WicketBeadCreated_Fallback(t *testing.T) {
	ev := EventItem{Type: "wicket_bead_created", Message: ""}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_bead_created with empty message")
	}
	if isError {
		t.Error("expected isError=false for wicket_bead_created")
	}
	if !strings.Contains(msg, "Wicket: bead created") {
		t.Errorf("expected fallback 'Wicket: bead created', got %q", msg)
	}
}

// TestToastForEvent_WicketClarification verifies wicket_clarification produces a toast.
func TestToastForEvent_WicketClarification(t *testing.T) {
	ev := EventItem{Type: "wicket_clarification", Message: "asked author for more details"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_clarification")
	}
	if isError {
		t.Error("expected isError=false for wicket_clarification")
	}
	if !strings.Contains(msg, "asked author") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_WicketFlaggedHuman verifies wicket_flagged_human produces a toast.
func TestToastForEvent_WicketFlaggedHuman(t *testing.T) {
	ev := EventItem{Type: "wicket_flagged_human", Message: "issue #5 needs human review"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_flagged_human")
	}
	if isError {
		t.Error("expected isError=false for wicket_flagged_human")
	}
	if !strings.Contains(msg, "issue #5") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_WicketFlaggedHuman_Fallback verifies the fallback for wicket_flagged_human.
func TestToastForEvent_WicketFlaggedHuman_Fallback(t *testing.T) {
	ev := EventItem{Type: "wicket_flagged_human", Message: ""}
	msg, _, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_flagged_human with empty message")
	}
	if !strings.Contains(msg, "needs human review") {
		t.Errorf("expected fallback 'needs human review', got %q", msg)
	}
}

// TestToastForEvent_WicketRejected verifies wicket_rejected produces a toast.
func TestToastForEvent_WicketRejected(t *testing.T) {
	ev := EventItem{Type: "wicket_rejected", Message: "issue #8 discarded as spam"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_rejected")
	}
	if isError {
		t.Error("expected isError=false for wicket_rejected")
	}
	if !strings.Contains(msg, "issue #8") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_WicketRejected_Fallback verifies the fallback for wicket_rejected.
func TestToastForEvent_WicketRejected_Fallback(t *testing.T) {
	ev := EventItem{Type: "wicket_rejected", Message: ""}
	msg, _, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_rejected with empty message")
	}
	if !strings.Contains(msg, "spam") {
		t.Errorf("expected fallback containing 'spam', got %q", msg)
	}
}

// TestToastForEvent_WicketError verifies wicket_error produces an error toast.
func TestToastForEvent_WicketError(t *testing.T) {
	ev := EventItem{Type: "wicket_error", Message: "failed to post comment: 403 Forbidden"}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_error")
	}
	if !isError {
		t.Error("expected isError=true for wicket_error")
	}
	if !strings.Contains(msg, "403 Forbidden") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestToastForEvent_WicketError_Fallback verifies the fallback for wicket_error.
func TestToastForEvent_WicketError_Fallback(t *testing.T) {
	ev := EventItem{Type: "wicket_error", Message: ""}
	msg, isError, ok := toastForEvent(ev)
	if !ok {
		t.Fatal("expected ok=true for wicket_error with empty message")
	}
	if !isError {
		t.Error("expected isError=true for wicket_error with empty message")
	}
	if !strings.Contains(msg, "Wicket error") {
		t.Errorf("expected fallback 'Wicket error', got %q", msg)
	}
}

// TestFirstOf verifies firstOf returns s when non-empty, else fallback.
func TestFirstOf(t *testing.T) {
	if firstOf("hello", "world") != "hello" {
		t.Error("expected 'hello'")
	}
	if firstOf("", "world") != "world" {
		t.Error("expected 'world' fallback")
	}
	if firstOf("", "") != "" {
		t.Error("expected empty string")
	}
}
