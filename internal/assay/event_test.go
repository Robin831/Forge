package assay

import (
	"strings"
	"testing"
	"time"
	"unicode"
)

// TestRunEventMessage pins the one-line terminal message for each outcome. The
// numbers are the point: the feed row is the only place a shadow-mode run is
// ever reported, so a message missing the findings count, the pass tally, the
// cost or the duration sends the operator back to the daemon log — which is the
// thing this event exists to replace.
func TestRunEventMessage(t *testing.T) {
	tests := []struct {
		name string
		ev   RunEvent
		want string
	}{
		{
			"complete",
			RunEvent{
				PRNumber: 347, Status: RunStatusComplete,
				CompletedPasses: 5, TotalPasses: 5,
				Findings: 7, CostUSD: 2.8, Duration: 152 * time.Second,
			},
			"Assay PR #347: complete — 5/5 passes, 7 findings ($2.80, 152s)",
		},
		{
			"complete with no findings",
			RunEvent{
				PRNumber: 4767, Status: RunStatusComplete,
				CompletedPasses: 5, TotalPasses: 5,
				Findings: 0, CostUSD: 1, Duration: 90*time.Second + 400*time.Millisecond,
			},
			"Assay PR #4767: complete — 5/5 passes, 0 findings ($1.00, 90s)",
		},
		{
			"complete in shadow mode",
			RunEvent{
				PRNumber: 347, Status: RunStatusComplete,
				CompletedPasses: 5, TotalPasses: 5,
				Findings: 7, CostUSD: 2.8, Duration: 152 * time.Second,
				ShadowMode: true,
			},
			"Assay PR #347: complete — 5/5 passes, 7 findings ($2.80, 152s) (shadow — findings in panel only)",
		},
		{
			"partial names the passes that did not run",
			RunEvent{
				PRNumber: 347, Status: RunStatusPartial,
				CompletedPasses: 3, TotalPasses: 5,
				FailedPasses: []PassFailure{
					{Name: "logic", Reason: "error_max_turns"},
					{Name: "repo-specific", Reason: "error_max_turns"},
				},
				Findings: 4, CostUSD: 1.2, Duration: 90 * time.Second,
			},
			"Assay PR #347: partial — 3/5 passes (failed: logic, repo-specific — error_max_turns), 4 findings ($1.20, 90s)",
		},
		{
			"partial in shadow mode",
			RunEvent{
				PRNumber: 347, Status: RunStatusPartial,
				CompletedPasses: 4, TotalPasses: 5,
				FailedPasses: []PassFailure{{Name: "logic", Reason: "error_max_turns"}},
				Findings:     2, CostUSD: 0.5, Duration: 30 * time.Second,
				ShadowMode: true,
			},
			"Assay PR #347: partial — 4/5 passes (failed: logic — error_max_turns), 2 findings ($0.50, 30s) (shadow — findings in panel only)",
		},
		{
			// Triage died, so no deep pass was ever attempted: no tally to
			// render, and the cause is what the row has to carry instead.
			"failed before the deep passes",
			RunEvent{
				PRNumber: 347, Status: RunStatusFailed,
				CostUSD: 0.4, Duration: 30 * time.Second,
				Reason: "assay triage: provider claude failed (exit 1)",
			},
			"Assay PR #347: failed — assay triage: provider claude failed (exit 1) ($0.40, 30s)",
		},
		{
			"failed with every deep pass dead",
			RunEvent{
				PRNumber: 347, Status: RunStatusFailed,
				CompletedPasses: 0, TotalPasses: 2,
				FailedPasses: []PassFailure{
					{Name: "logic", Reason: "error_max_turns"},
					{Name: "security", Reason: "rate_limited"},
				},
				CostUSD: 3.1, Duration: 240 * time.Second,
				Reason: "all assay deep passes failed",
			},
			"Assay PR #347: failed — 0/2 passes (failed: logic — error_max_turns, security — rate_limited), all assay deep passes failed ($3.10, 240s)",
		},
		{
			// A failed run found nothing, so the shadow clause ("findings in
			// panel only") would only promise findings that do not exist.
			"failed in shadow mode carries no shadow clause",
			RunEvent{
				PRNumber: 347, Status: RunStatusFailed,
				CostUSD: 0.4, Duration: 30 * time.Second,
				Reason: "diff fetch failed", ShadowMode: true,
			},
			"Assay PR #347: failed — diff fetch failed ($0.40, 30s)",
		},
		{
			"failed with no cause still reads as a run",
			RunEvent{PRNumber: 12, Status: RunStatusFailed},
			"Assay PR #12: failed ($0.00, 0s)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ev.Message(); got != tt.want {
				t.Errorf("Message() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// TestRunEventMessageIsOneBoundedLine: a provider error arrives multi-line and
// can be arbitrarily long. The feed renders one row per event, so the message
// must not smuggle a stack trace into it and push the numbers out of sight.
func TestRunEventMessageIsOneBoundedLine(t *testing.T) {
	ev := RunEvent{
		PRNumber: 9, Status: RunStatusFailed,
		Reason: "assay triage failed: " + strings.Repeat("x", 500) + "\nstack trace line\nanother line",
	}
	got := ev.Message()
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Message() spans multiple lines: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("Message() did not mark the truncated reason: %q", got)
	}
	if !strings.HasSuffix(got, "($0.00, 0s)") {
		t.Errorf("Message() lost its trailing numbers: %q", got)
	}
	if len(got) > eventReasonMax+120 {
		t.Errorf("Message() unbounded (%d chars): %q", len(got), got)
	}
}

// TestRunEventMessageStripsControlSequences: the reason is the one part of the
// message Forge did not write — it can quote provider output shaped by the diff
// under review — and Hearth renders a feed row straight to the terminal. An
// escape sequence surviving into it could repaint or spoof neighbouring rows,
// so the row must carry the text of a hostile reason and none of its commands.
func TestRunEventMessageStripsControlSequences(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{
			"csi colour and cursor movement",
			"triage failed \x1b[31mRED\x1b[0m\x1b[2A\x1b[Kspoofed",
			"triage failed REDspoofed",
		},
		{
			"osc title/clipboard write",
			"triage failed \x1b]0;pwned\apost",
			"triage failed post",
		},
		{
			"bare control runes",
			"triage\x07 failed\x08\x00 hard",
			"triage failed hard",
		},
		{
			"tabs become spaces rather than vanishing",
			"triage failed:\tconnection reset",
			"triage failed: connection reset",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RunEvent{PRNumber: 9, Status: RunStatusFailed, Reason: tt.reason}.Message()
			want := "Assay PR #9: failed — " + tt.wantReason + " ($0.00, 0s)"
			if got != want {
				t.Errorf("Message() =\n  %q\nwant\n  %q", got, want)
			}
			if strings.ContainsRune(got, '\x1b') {
				t.Errorf("Message() kept an escape byte: %q", got)
			}
			for _, r := range got {
				if !unicode.IsPrint(r) {
					t.Errorf("Message() kept non-printable %q in %q", r, got)
				}
			}
		})
	}
}

// TestRunEventMessageDropsTrailingReasonLines keeps a short first line intact
// rather than eliding it just because later lines exist.
func TestRunEventMessageDropsTrailingReasonLines(t *testing.T) {
	ev := RunEvent{PRNumber: 9, Status: RunStatusFailed, Reason: "triage failed\nexit status 1"}
	want := "Assay PR #9: failed — triage failed ($0.00, 0s)"
	if got := ev.Message(); got != want {
		t.Errorf("Message() = %q; want %q", got, want)
	}
}
