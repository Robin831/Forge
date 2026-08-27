package assay

import (
	"strings"
	"testing"
)

// The security and repo-specific passes have tools to read the whole checkout
// and were not using them: over 20 logged runs the security pass averaged 2.5
// turns and ended within two turns on 13 of them, and repo-specific ended at
// exactly one turn on 7. A pass that answers in one or two turns emitted its
// JSON without calling a tool — it reviewed the diff text and nothing else,
// which is how a missing per-resource permission filter (visible only in a
// sibling endpoint) and an unsynchronized cache (visible only in the parallel
// loop that calls it, in another file) are both reviewed and both missed.
//
// The cause was in the prompt, not the turn budget: security.md closed with "Do
// not speculate about code you cannot see", intended against hallucination and
// read as an instruction to stay inside the diff. The tests below pin the
// property that replaced it, because it is a property of prose — nothing else
// in the package can fail when it regresses, and the symptom is silence.

// antiReadingClauses are phrasings that tell a pass to confine itself to the
// diff. They are matched case-insensitively against the shipped prompt.
//
// The list is of exact phrasings rather than a rule, since "do not speculate"
// on its own is a clause the prompt SHOULD carry — the distinction being
// whether it constrains what may be claimed or what may be read.
var antiReadingClauses = []string{
	"code you cannot see",
	"cannot see",
	"only issues grounded in the diff",
	"do not read",
	"read no",
	"only the diff",
	"restrict yourself to the diff",
}

func TestSecurityPromptDoesNotForbidReading(t *testing.T) {
	p, err := loadPrompt("security")
	if err != nil {
		t.Fatalf("loadPrompt: %v", err)
	}
	lower := strings.ToLower(p)
	for _, c := range antiReadingClauses {
		if strings.Contains(lower, c) {
			t.Errorf("security prompt contains %q — a clause that reads as an instruction not to open files", c)
		}
	}
	// The guard against invention has to survive the change: what moved is what
	// it constrains, not whether it is there.
	if !strings.Contains(lower, "do not speculate") {
		t.Error("security prompt no longer tells the pass not to speculate; the fix was to re-aim that clause, not to drop it")
	}
	for _, want := range []string{
		"open the implicated files",
		"caller",  // follow the call path one hop out
		"sibling", // the canonical auth pattern lives next door
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("security prompt does not mention %q; the exploration it asks for is the point of the change", want)
		}
	}
}

func TestRepoSpecificPromptRequiresPerItemVerification(t *testing.T) {
	p, err := loadPrompt("repo_specific")
	if err != nil {
		t.Fatalf("loadPrompt: %v", err)
	}
	lower := strings.ToLower(p)
	for _, c := range antiReadingClauses {
		if strings.Contains(lower, c) {
			t.Errorf("repo-specific prompt contains %q — a clause that reads as an instruction not to open files", c)
		}
	}
	// The minimum-exploration instruction: an item is checked against the file
	// it concerns, not against the diff text that happens to mention it.
	for _, want := range []string{
		"open the file it concerns",
		"checklist",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("repo-specific prompt does not mention %q; without it the checklist is matched against diff text", want)
		}
	}
}

// TestDeepPassPromptsStillLoad is the cheap backstop for an edit that renames or
// deletes a prompt file: every pass's template must still resolve out of the
// embedded FS, and none may be empty.
func TestDeepPassPromptsStillLoad(t *testing.T) {
	for _, p := range append([]passDef{passTriage}, deepPasses...) {
		got, err := loadPrompt(p.promptFile)
		if err != nil {
			t.Errorf("loadPrompt(%s): %v", p.promptFile, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("prompt %s is empty", p.promptFile)
		}
	}
}
