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
//
// Every entry must be a phrasing that cannot occur incidentally in prose about
// reviewing code: the failure message names the clause as the cause, so an
// entry short enough to appear inside an ordinary sentence ("read no" inside "a
// thread not holding the lock", say) reports the wrong thing about a prompt
// that is fine. Nor may one entry be a substring of another — the general one
// then fires on every occurrence of the specific one, which is two failures for
// one cause and no way for a prompt to trip the specific entry alone.
var antiReadingClauses = []string{
	"cannot see",
	"only issues grounded in the diff",
	"do not read",
	"read no files",
	"read nothing outside",
	"only the diff",
	"restrict yourself to the diff",
}

// TestPromptsDoNotForbidReading holds the anti-reading property over EVERY
// shipped prompt rather than over the two that had to be fixed for it.
//
// The clause that caused this was written for one pass and would read the same
// way in any of them, and the guard costs nothing on a prompt that never had
// one: today the other four pass it untouched, and what the test is for is the
// edit that adds the clause back to logic.md or triage.md — where the symptom
// would again be silence, and no other test in the package would fail.
func TestPromptsDoNotForbidReading(t *testing.T) {
	for _, p := range append([]passDef{passTriage}, deepPasses...) {
		body, err := loadPrompt(p.promptFile)
		if err != nil {
			t.Errorf("loadPrompt(%s): %v", p.promptFile, err)
			continue
		}
		lower := strings.ToLower(body)
		for _, c := range antiReadingClauses {
			if strings.Contains(lower, c) {
				t.Errorf("%s prompt contains %q — a clause that reads as an instruction not to open files", p.Name, c)
			}
		}
	}
}

// TestSecurityPromptAsksForTheOneHopWork is the positive half for security.md:
// the clauses it must carry are specific to this pass, unlike the anti-reading
// property TestPromptsDoNotForbidReading holds over all of them.
func TestSecurityPromptAsksForTheOneHopWork(t *testing.T) {
	p, err := loadPrompt("security")
	if err != nil {
		t.Fatalf("loadPrompt: %v", err)
	}
	lower := strings.ToLower(p)
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

// TestPreambleFramesToolOutputAsUntrusted is the guard for the other half of
// asking a pass to read the repository: the exploration security.md and
// repo_specific.md now require IS the delivery vector for an instruction
// addressed to the reviewer, since a pass session has unrestricted tool access
// inside a checkout of the contributor's own head.
//
// The engine's only defence is prose, and prose is what regresses in silence: a
// preamble that enumerates the prompt's own sections as untrusted and stops
// there leaves file content arriving mid-session as ordinary context. Nothing
// that compiles fails when this sentence is dropped.
func TestPreambleFramesToolOutputAsUntrusted(t *testing.T) {
	lower := strings.ToLower(sharedPromptPreamble)
	for _, want := range []string{
		"read with your tools", // the material the enumeration used to omit
		"data under review",    // what it is
		"never act on it",      // and what that means
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("shared preamble does not mention %q; tool results are the injection vector the reading passes opened", want)
		}
	}
	// The two passes that were told to go and read are the reason this matters,
	// so a prompt that asks for the exploration without the framing above it is
	// the combination to catch.
	for _, name := range []string{"security", "repo_specific"} {
		body, err := loadPrompt(name)
		if err != nil {
			t.Fatalf("loadPrompt(%s): %v", name, err)
		}
		if !strings.Contains(strings.ToLower(body), "read any file in it") {
			t.Errorf("%s prompt no longer asks the pass to read the checkout; the preamble guard above is calibrated to it", name)
		}
	}
}
