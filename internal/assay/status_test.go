package assay

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDeriveStatus(t *testing.T) {
	tests := []struct {
		name   string
		total  int
		failed []PassFailure
		want   RunStatus
	}{
		{"no failures", 5, nil, RunStatusComplete},
		{"one failure", 5, []PassFailure{{Name: "logic"}}, RunStatusPartial},
		{
			"some failures",
			5,
			[]PassFailure{{Name: "logic"}, {Name: "repo-specific"}},
			RunStatusPartial,
		},
		{
			"every pass failed",
			2,
			[]PassFailure{{Name: "logic"}, {Name: "security"}},
			RunStatusFailed,
		},
		// Nothing ran, so nothing reviewed the diff: the conservative status,
		// not the optimistic one that would read "complete: 0 of 0".
		{"no passes attempted", 0, nil, RunStatusFailed},
		{"no passes attempted, failures reported", 0, []PassFailure{{Name: "logic"}}, RunStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveStatus(tt.total, tt.failed); got != tt.want {
				t.Errorf("DeriveStatus(%d, %v) = %q; want %q", tt.total, tt.failed, got, tt.want)
			}
		})
	}
}

func TestRenderStatusTextPartialSharedReason(t *testing.T) {
	got := RenderStatusText(RunStatusPartial, 3, 5, []PassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "error_max_turns"},
	})
	want := "partial: 3 of 5 passes completed (failed: logic, repo-specific — error_max_turns)"
	if got != want {
		t.Errorf("RenderStatusText =\n  %q\nwant\n  %q", got, want)
	}
}

func TestRenderStatusTextPartialMixedReasons(t *testing.T) {
	got := RenderStatusText(RunStatusPartial, 3, 5, []PassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "rate_limited"},
	})
	want := "partial: 3 of 5 passes completed (failed: logic — error_max_turns, repo-specific — rate_limited)"
	if got != want {
		t.Errorf("RenderStatusText =\n  %q\nwant\n  %q", got, want)
	}
}

func TestRenderStatusTextCompleteNamesNoPasses(t *testing.T) {
	got := RenderStatusText(RunStatusComplete, 5, 5, nil)
	if got != "complete: 5 of 5 passes completed" {
		t.Errorf("RenderStatusText = %q", got)
	}
	if strings.Contains(got, "failed") {
		t.Errorf("a complete run must not mention failed passes: %q", got)
	}
}

func TestRenderStatusTextUnknownReason(t *testing.T) {
	got := RenderStatusText(RunStatusPartial, 4, 5, []PassFailure{{Name: "logic"}})
	want := "partial: 4 of 5 passes completed (failed: logic)"
	if got != want {
		t.Errorf("RenderStatusText = %q; want %q", got, want)
	}
}

func TestPartialCoverageNote(t *testing.T) {
	note := PartialCoverageNote([]PassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "error_max_turns"},
	})
	for _, want := range []string{"Partial coverage", "logic, repo-specific", "error_max_turns", "not a complete review"} {
		if !strings.Contains(note, want) {
			t.Errorf("coverage note missing %q, got: %s", want, note)
		}
	}
}

func TestPartialCoverageNoteEmptyWhenNothingFailed(t *testing.T) {
	if note := PartialCoverageNote(nil); note != "" {
		t.Errorf("expected no note for a complete run, got %q", note)
	}
}

func TestClassifyPassErrorUsesTypedError(t *testing.T) {
	err := newPassError("logic", "error_max_turns", "provider claude failed (exit 1, subtype error_max_turns)", nil)
	got := classifyPassError("logic", err)
	if got.Name != "logic" || got.Reason != "error_max_turns" {
		t.Errorf("classifyPassError = %+v; want {logic error_max_turns}", got)
	}
	if !strings.HasPrefix(err.Error(), "assay pass logic: ") {
		t.Errorf("PassError message lost its prefix: %q", err.Error())
	}
}

func TestClassifyPassErrorUnwrapsThroughWrapping(t *testing.T) {
	inner := newPassError("security", ReasonInvalidJSON, "invalid JSON output after retry", nil)
	wrapped := fmt.Errorf("running pass: %w", inner)
	got := classifyPassError("security", wrapped)
	if got.Reason != ReasonInvalidJSON {
		t.Errorf("classifyPassError reason = %q; want %q", got.Reason, ReasonInvalidJSON)
	}
}

// A reason reaches the public PR summary comment verbatim, so anything outside
// the label charset must not survive classification as live markdown.
func TestClassifyPassErrorSanitizesReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{"markdown link", "[click](https://evil.example)", ReasonUnknown},
		{"html", "<img src=x onerror=alert(1)>", ReasonUnknown},
		{"overlong prose", strings.Repeat("a", maxLabelLen+1), ReasonUnknown},
		{"uppercased subtype", "Error_Max_Turns", "error_max_turns"},
		{"plain subtype", "error_max_turns", "error_max_turns"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPassError("logic", newPassError("logic", tt.reason, "detail", nil))
			if got.Reason != tt.want {
				t.Errorf("classifyPassError reason = %q; want %q", got.Reason, tt.want)
			}
		})
	}

	// And the same for a reason inferred out of a foreign error's text, which
	// is the path the comment called out as the future risk.
	got := classifyPassError("logic", errors.New("backend failed (exit 1, subtype [x](https://evil.example))"))
	if got.Reason != ReasonUnknown {
		t.Errorf("inferred reason = %q; want %q", got.Reason, ReasonUnknown)
	}
	if note := PartialCoverageNote([]PassFailure{got}); strings.Contains(note, "evil.example") {
		t.Errorf("unsanitized reason reached the PR comment: %s", note)
	}
}

// A pass name reaches the same public PR comment as its reason, and PassError
// is exported — a foreign backend can set Pass to anything. An unsafe claim is
// dropped in favour of the pass that was actually running.
func TestClassifyPassErrorSanitizesName(t *testing.T) {
	tests := []struct {
		name    string
		claimed string
		want    string
	}{
		{"markdown link", "[click](https://evil.example)", "logic"},
		{"html", "<img src=x onerror=alert(1)>", "logic"},
		{"overlong prose", strings.Repeat("a", maxLabelLen+1), "logic"},
		{"empty", "", "logic"},
		{"uppercased", "Repo-Specific", "repo-specific"},
		{"plain", "security", "security"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPassError("logic", &PassError{Pass: tt.claimed, Reason: "error_max_turns", Message: "detail"})
			if got.Name != tt.want {
				t.Errorf("classifyPassError name = %q; want %q", got.Name, tt.want)
			}
			for _, sink := range []string{
				PartialCoverageNote([]PassFailure{got}),
				RenderStatusText(RunStatusPartial, 4, 5, []PassFailure{got}),
			} {
				if strings.Contains(sink, "evil.example") || strings.Contains(sink, "onerror") {
					t.Errorf("unsanitized pass name reached a rendered sink: %s", sink)
				}
			}
		})
	}
}

func TestClassifyPassErrorInfersReasonFromForeignError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{errors.New("assay pass logic: provider claude/opus failed (exit 1, subtype error_max_turns)"), "error_max_turns"},
		// The subtype is recoverable wherever it sits in the message, not only
		// when it is the last token.
		{errors.New("provider failed (exit 1, subtype error_max_turns) after 3 attempts"), "error_max_turns"},
		{errors.New("backend: subtype error_max_turns, giving up"), "error_max_turns"},
		{errors.New("assay pass logic: provider claude/opus rate limited"), ReasonRateLimited},
		{errors.New("assay pass logic: invalid JSON output after retry"), ReasonInvalidJSON},
		{errors.New("something else entirely"), ReasonUnknown},
		// A foreign error that merely names a .json path never parsed any
		// output — labelling it invalid_json would point the operator at the
		// wrong cause.
		{errors.New("open /tmp/findings.json: permission denied"), ReasonUnknown},
	}
	for _, tt := range tests {
		got := classifyPassError("logic", tt.err)
		if got.Name != "logic" {
			t.Errorf("classifyPassError name = %q; want logic", got.Name)
		}
		if got.Reason != tt.want {
			t.Errorf("classifyPassError(%v) reason = %q; want %q", tt.err, got.Reason, tt.want)
		}
	}
}

func TestRenderPassTelemetry(t *testing.T) {
	tests := []struct {
		name   string
		passes []PassReport
		want   string
	}{
		{"no passes", nil, ""},
		{
			"clean run",
			[]PassReport{
				{Name: "triage", Turns: 3, Attempts: 1},
				{Name: "logic", Turns: 9, Attempts: 1},
			},
			"pass=triage turns=3 term=success, pass=logic turns=9 term=success",
		},
		{
			// The retry marker only appears where a pass was actually re-run,
			// so a grep for "retry=" finds the rare case and nothing else.
			"retried then answered",
			[]PassReport{{Name: "logic", Turns: 7, Attempts: 2, Retried: true}},
			"pass=logic turns=7 term=success retry=1",
		},
		{
			"retried and still out of turns",
			[]PassReport{{Name: "logic", Turns: 12, TerminationReason: ReasonMaxTurns, Attempts: 2, Retried: true}},
			"pass=logic turns=12 term=error_max_turns retry=1",
		},
		{
			// A provider that reports no turn count leaves the field at 0
			// rather than omitting it: an absent key would read as a parse
			// failure in a log query, a zero reads as "not reported".
			"no turn count reported",
			[]PassReport{{Name: "security", TerminationReason: ReasonRateLimited, Attempts: 1}},
			"pass=security turns=0 term=rate_limited",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderPassTelemetry(tt.passes); got != tt.want {
				t.Errorf("RenderPassTelemetry() = %q; want %q", got, tt.want)
			}
		})
	}
}
