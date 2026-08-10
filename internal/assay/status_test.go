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

func TestClassifyPassErrorInfersReasonFromForeignError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{errors.New("assay pass logic: provider claude/opus failed (exit 1, subtype error_max_turns)"), "error_max_turns"},
		{errors.New("assay pass logic: provider claude/opus rate limited"), ReasonRateLimited},
		{errors.New("assay pass logic: invalid JSON output after retry"), ReasonInvalidJSON},
		{errors.New("something else entirely"), ReasonUnknown},
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
