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

// TestCostRendersInTelemetry pins the two dollar fields and, above all, keeps
// them apart. assay.max_cost_per_pass_usd is compared against costTracker's
// running estimate and never against the provider's billed total, and the two
// differ by a structural factor (1.61x measured) because a message's usage
// block is stamped when the message starts. So a line carrying only the billed
// figure cannot size the ceiling — which is exactly the state that forced the
// tracker to be reproduced by hand over raw session logs, a reproduction the
// sessions the ceiling actually killed are absent from by construction.
func TestCostRendersInTelemetry(t *testing.T) {
	tests := []struct {
		name   string
		passes []PassReport
		want   string
	}{
		{
			// Both known: exact placement, after the cache pair and before
			// primer=, so a grep for primer=1 still anchors to a segment's end.
			"both figures",
			[]PassReport{{
				Name: "logic", Turns: 12, Attempts: 1,
				CacheCreationTokens: 41500, CacheReadTokens: 900,
				CostUSD: 1.28, EstCostUSD: 0.79, Primer: true,
			}},
			"pass=logic turns=12 term=success cache_w=41500 cache_r=900 cost_usd=1.2800 cost_est=0.7900 primer=1",
		},
		{
			// A backend that streams no per-turn usage (Gemini deltas, plain
			// text) can be billed while nothing measures it in the ceiling's
			// unit. cost_est=0 there would read as a free pass, so it is
			// omitted — the same discipline tools=/files= follow.
			"billed but unmeasured",
			[]PassReport{{Name: "security", Turns: 6, Attempts: 1, CostUSD: 0.51}},
			"pass=security turns=6 term=success cost_usd=0.5100",
		},
		{
			// The mirror case, and the one the whole field exists for: a
			// session Assay stopped at the ceiling emits no result event, so
			// there is no provider total for it at all — only the tracker's
			// snapshot, which is what PassError carries under both names.
			// cost_usd == cost_est beside term=error_max_cost is the signature.
			"stopped at the ceiling",
			[]PassReport{{
				Name: "logic", Turns: 13, Attempts: 1,
				TerminationReason: ReasonMaxCost, CostUSD: 1.5732, EstCostUSD: 1.5732,
			}},
			"pass=logic turns=13 term=error_max_cost cost_usd=1.5732 cost_est=1.5732",
		},
		{
			// No ceiling configured and a provider that reported nothing:
			// neither field, and no stray separator left behind.
			"nothing measured",
			[]PassReport{{Name: "tests-missing", Turns: 4, Attempts: 1}},
			"pass=tests-missing turns=4 term=success",
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
