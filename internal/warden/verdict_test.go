package warden

import (
	"testing"

	"github.com/Robin831/Forge/internal/provider"
)

func TestParseVerdict_JSONBlock(t *testing.T) {
	// All providers should parse a proper ```json block.
	output := "Here is my review:\n\n```json\n{\"verdict\": \"request_changes\", \"summary\": \"Missing error handling\", \"issues\": [{\"file\": \"main.go\", \"line\": 42, \"severity\": \"error\", \"message\": \"unchecked error\"}]}\n```\n\nMore commentary."

	for _, kind := range []provider.Kind{provider.Claude, provider.Gemini, provider.Copilot} {
		t.Run(string(kind), func(t *testing.T) {
			r := &ReviewResult{}
			parseVerdict(output, kind, r)
			if r.Verdict != VerdictRequestChanges {
				t.Errorf("kind=%s: got %q, want request_changes", kind, r.Verdict)
			}
			if r.Summary != "Missing error handling" {
				t.Errorf("kind=%s: summary=%q", kind, r.Summary)
			}
			if len(r.Issues) != 1 {
				t.Errorf("kind=%s: got %d issues, want 1", kind, len(r.Issues))
			}
		})
	}
}

func TestParseVerdict_CopilotProseApproval(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   Verdict
	}{
		{"lgtm", "Overall, LGTM. The changes look reasonable.", VerdictApprove},
		{"approved", "I've reviewed the diff and this is Approved. No issues.", VerdictApprove},
		{"looks_good_to_me", "The code looks good to me.", VerdictApprove},
		{"ready_to_merge", "This is ready to merge.", VerdictApprove},
		{"no_issues", "After reviewing, no issues found with the implementation.", VerdictApprove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReviewResult{}
			parseVerdict(tt.output, provider.Copilot, r)
			if r.Verdict != tt.want {
				t.Errorf("got %q, want %q (summary: %s)", r.Verdict, tt.want, r.Summary)
			}
		})
	}
}

func TestParseVerdict_CopilotProseRequestChanges(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"request_changes", "I'm requesting changes on this PR. Several issues need attention."},
		{"please_fix", "There are some issues. Please fix the error handling in main.go."},
		{"must_be_fixed", "The SQL injection vulnerability must be fixed before merging."},
		{"some_issues", "I found some issues with the implementation."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReviewResult{}
			parseVerdict(tt.output, provider.Copilot, r)
			if r.Verdict != VerdictRequestChanges {
				t.Errorf("got %q, want request_changes (summary: %s)", r.Verdict, r.Summary)
			}
		})
	}
}

func TestParseVerdict_CopilotProseRejection(t *testing.T) {
	r := &ReviewResult{}
	parseVerdict("I reject this change. There is a fundamental problem with the approach.", provider.Copilot, r)
	if r.Verdict != VerdictReject {
		t.Errorf("got %q, want reject", r.Verdict)
	}
}

func TestParseVerdict_GeminiKeyValue(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   Verdict
	}{
		{"plain", "Verdict: approve\n\nThe code looks fine.", VerdictApprove},
		{"bold", "**Verdict:** request_changes\n\nSome issues found.", VerdictRequestChanges},
		{"bold_key", "**Verdict**: reject\n\nFundamental issues.", VerdictReject},
		{"with_quotes", "Verdict: \"approve\"\n\nAll good.", VerdictApprove},
		{"with_backticks", "Verdict: `request_changes`\n\nFix needed.", VerdictRequestChanges},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReviewResult{}
			parseVerdict(tt.output, provider.Gemini, r)
			if r.Verdict != tt.want {
				t.Errorf("got %q, want %q (summary: %s)", r.Verdict, tt.want, r.Summary)
			}
		})
	}
}

func TestParseVerdict_GeminiProse(t *testing.T) {
	r := &ReviewResult{}
	parseVerdict("After careful review, LGTM — the changes are well-structured.", provider.Gemini, r)
	if r.Verdict != VerdictApprove {
		t.Errorf("got %q, want approve", r.Verdict)
	}
}

func TestParseVerdict_ClaudeJSONFragment(t *testing.T) {
	// Claude sometimes emits JSON without fencing.
	output := `Here is my verdict: {"verdict": "approve", "summary": "All good", "issues": []} and then some more text.`
	r := &ReviewResult{}
	parseVerdict(output, provider.Claude, r)
	if r.Verdict != VerdictApprove {
		t.Errorf("got %q, want approve", r.Verdict)
	}
	if r.Summary != "All good" {
		t.Errorf("summary=%q, want 'All good'", r.Summary)
	}
}

func TestParseVerdict_ClaudeFallback(t *testing.T) {
	// Claude with broken JSON but verdict string present.
	output := `"verdict":"request_changes" but the JSON was malformed`
	r := &ReviewResult{}
	parseVerdict(output, provider.Claude, r)
	if r.Verdict != VerdictRequestChanges {
		t.Errorf("got %q, want request_changes", r.Verdict)
	}
}

func TestParseVerdict_DefaultsToApprove(t *testing.T) {
	// Completely unparseable output should default to approve for human review.
	for _, kind := range []provider.Kind{provider.Claude, provider.Gemini, provider.Copilot} {
		t.Run(string(kind), func(t *testing.T) {
			r := &ReviewResult{}
			parseVerdict("Random gibberish with no verdict signals.", kind, r)
			if r.Verdict != VerdictApprove {
				t.Errorf("kind=%s: got %q, want approve", kind, r.Verdict)
			}
			if r.Summary == "" {
				t.Error("expected non-empty summary for fallback")
			}
		})
	}
}

func TestParseVerdict_CopilotJSONOverridesProse(t *testing.T) {
	// When JSON is present, it should take priority even for Copilot.
	output := "I approve this change.\n\n```json\n{\"verdict\": \"reject\", \"summary\": \"Actually no\", \"issues\": []}\n```"
	r := &ReviewResult{}
	parseVerdict(output, provider.Copilot, r)
	if r.Verdict != VerdictReject {
		t.Errorf("JSON should override prose: got %q, want reject", r.Verdict)
	}
}

func TestExtractKeyValueVerdict(t *testing.T) {
	tests := []struct {
		input string
		want  Verdict
		ok    bool
	}{
		{"verdict: approve", VerdictApprove, true},
		{"verdict: reject", VerdictReject, true},
		{"verdict: request_changes", VerdictRequestChanges, true},
		{"verdict: request changes", VerdictRequestChanges, true},
		{"**verdict**: approve", VerdictApprove, true},
		{"**verdict:** reject", VerdictReject, true},
		{"no verdict here", "", false},
		{"verdict: unknown_thing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := extractKeyValueVerdict(tt.input)
			if ok != tt.ok || got != tt.want {
				t.Errorf("extractKeyValueVerdict(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("hello world", "world", "mars") {
		t.Error("expected true")
	}
	if containsAny("hello world", "mars", "venus") {
		t.Error("expected false")
	}
}
