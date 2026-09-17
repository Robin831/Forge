package main

import (
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// The run-level model is only claimed when the pass rows agree on it: a mixed
// run has none, so its unnamed passes fall to the report's fallback rather than
// to whichever pass came first.
func TestRunLevelModel(t *testing.T) {
	for _, tt := range []struct {
		name   string
		passes []state.AssayPassFindings
		want   string
	}{
		{"no rows", nil, ""},
		{"no models recorded", []state.AssayPassFindings{{Name: "triage"}, {Name: "logic"}}, ""},
		{"one model", []state.AssayPassFindings{{Name: "triage", Model: "claude-opus-5"}, {Name: "logic", Model: "claude-opus-5"}}, "claude-opus-5"},
		{"unnamed passes ignored", []state.AssayPassFindings{{Name: "triage"}, {Name: "logic", Model: "claude-opus-5"}}, "claude-opus-5"},
		{"mixed models", []state.AssayPassFindings{{Name: "triage", Model: "claude-sonnet-5"}, {Name: "logic", Model: "claude-opus-5"}}, ""},
	} {
		if got := runLevelModel(tt.passes); got != tt.want {
			t.Errorf("%s: runLevelModel = %q, want %q", tt.name, got, tt.want)
		}
	}

	recs := projectPassRecords([]state.AssayPassFindings{{Name: "logic", Model: "claude-opus-5", CostUSD: 1.5, InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4}})
	if len(recs) != 1 || recs[0].Model != "claude-opus-5" || recs[0].CostUSD != 1.5 ||
		recs[0].InputTokens != 1 || recs[0].OutputTokens != 2 || recs[0].CacheCreationTokens != 3 || recs[0].CacheReadTokens != 4 {
		t.Errorf("projectPassRecords = %+v", recs)
	}
}
