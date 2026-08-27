package assay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/smith"
)

// --- the guard ------------------------------------------------------------

// TestRetryPayloadDiffersFromOriginal is the mechanical form of the rule this
// whole path exists for: whatever buildRetryMods produces, the payload the
// retry sends must not hash to the payload the failed attempt already sent.
func TestRetryPayloadDiffersFromOriginal(t *testing.T) {
	base := retryInputs{prompt: "instructions and a diff", diff: diffFixture, turns: 12}
	build := func(d string) (string, error) { return "instructions and a diff\n" + d, nil }

	mods, ok := buildRetryMods(DefaultConfig(), []string{"/repo/a.go"}, []string{"a.go", "b.go"})
	if !ok {
		t.Fatal("buildRetryMods reported no modification for a healthy default config")
	}
	next, ok := planRetryInputs(base, mods, build)
	if !ok {
		t.Fatal("planRetryInputs refused a payload it should have accepted")
	}
	if next.hash() == base.hash() {
		t.Fatal("retry payload hashes identically to the original; the retry is the same request again")
	}
	if retryPayloadHash(next.prompt, next.diff, next.turns) != next.hash() {
		t.Error("retryPayloadHash and retryInputs.hash disagree")
	}
}

// TestPlanRetryInputsRefusesUnmodifiedPayload is the other half of the guard:
// when nothing could be changed, the retry is dropped rather than paid for.
// Removing the hash comparison in planRetryInputs makes this fail.
func TestPlanRetryInputsRefusesUnmodifiedPayload(t *testing.T) {
	base := retryInputs{prompt: "p", diff: diffFixture, turns: 12}
	if _, ok := planRetryInputs(base, retryMods{}, nil); ok {
		t.Error("planRetryInputs accepted an unmodified payload")
	}
	// A "reduction" that reduces nothing and a scoping that scopes nothing are
	// indistinguishable from no modification by the only measure that counts.
	if _, ok := planRetryInputs(base, retryMods{turnBudget: 12}, nil); ok {
		t.Error("planRetryInputs accepted a turn budget equal to the original")
	}
	if _, ok := planRetryInputs(base, retryMods{scopedFiles: []string{"a.go", "b.go"}}, nil); ok {
		t.Error("planRetryInputs accepted a scoping that left the diff unchanged")
	}
}

// TestRetryPayloadHashSeparatesFields pins the length-prefixing: without it a
// prompt and a diff could be shuffled across the boundary between them and
// digest the same, so a genuinely different payload would read as a repeat and
// be dropped.
func TestRetryPayloadHashSeparatesFields(t *testing.T) {
	if retryPayloadHash("ab", "c", 1) == retryPayloadHash("a", "bc", 1) {
		t.Error("field boundary is not encoded in the hash")
	}
	if retryPayloadHash("a", "b", 12) == retryPayloadHash("a", "b", 6) {
		t.Error("turn budget is not part of the hash")
	}
}

// --- the modifications ----------------------------------------------------

// TestRetryTurnBudgetHalvedWithFloor pins the reduction and its floor. A retry
// that runs on the same budget costs the same as the session that just spent
// it; one cut below the floor cannot answer at all.
func TestRetryTurnBudgetHalvedWithFloor(t *testing.T) {
	cases := []struct {
		orig, want int
		ok         bool
	}{
		{orig: 12, want: 6, ok: true}, // the engine default
		{orig: 10, want: 5, ok: true},
		{orig: 8, want: 4, ok: true},
		{orig: 4, want: minRetryTurns, ok: true},
		{orig: minRetryTurns, ok: false}, // the floor is already the budget
		{orig: 2, ok: false},
		{orig: 0, ok: false},
	}
	for _, c := range cases {
		got, ok := reducedTurnBudget(c.orig)
		if ok != c.ok {
			t.Errorf("reducedTurnBudget(%d) ok = %v; want %v", c.orig, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("reducedTurnBudget(%d) = %d; want %d", c.orig, got, c.want)
		}
		if ok && got >= c.orig {
			t.Errorf("reducedTurnBudget(%d) = %d; a retry budget must be smaller", c.orig, got)
		}
	}
}

// TestBuildRetryModsHonoursConfiguredBudget checks the reduction is taken from
// the anvil's own cap, not from the engine default it may have overridden.
func TestBuildRetryModsHonoursConfiguredBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTurnsPerPass = 30
	mods, ok := buildRetryMods(cfg, nil, nil)
	if !ok {
		t.Fatal("buildRetryMods reported no modification")
	}
	if mods.turnBudget != 15 {
		t.Errorf("turnBudget = %d; want 15 (half of the configured 30)", mods.turnBudget)
	}

	cfg.MaxTurnsPerPass = minRetryTurns
	mods, ok = buildRetryMods(cfg, nil, nil)
	if !ok {
		t.Fatal("buildRetryMods reported no modification; the instruction alone is one")
	}
	if mods.turnBudget != 0 {
		t.Errorf("turnBudget = %d; want 0 — a budget at the floor cannot be reduced", mods.turnBudget)
	}
}

// TestRetryScopedToOpenedFiles pins the third modification: the retry's diff is
// narrowed to the changed files the failed session actually opened.
func TestRetryScopedToOpenedFiles(t *testing.T) {
	diffFiles := []string{"a.go", "pkg/b.go", "c.go"}

	// Absolute paths, as a provider reports them.
	got := openedDiffFiles([]string{"/w/tree/pkg/b.go", "/w/tree/README.md"}, diffFiles)
	if len(got) != 1 || got[0] != "pkg/b.go" {
		t.Errorf("openedDiffFiles = %v; want [pkg/b.go]", got)
	}

	// A file read from outside the diff cannot widen the scope.
	got = openedDiffFiles([]string{"/w/tree/internal/other.go"}, diffFiles)
	if len(got) != 0 {
		t.Errorf("openedDiffFiles = %v; want none — only diff files may be selected", got)
	}

	// Opening everything narrows nothing, so there is nothing to scope.
	got = openedDiffFiles([]string{"a.go", "pkg/b.go", "c.go"}, diffFiles)
	if got != nil {
		t.Errorf("openedDiffFiles = %v; want nil when every diff file was opened", got)
	}

	// No evidence at all: budget and instruction carry the retry alone.
	if got := openedDiffFiles(nil, diffFiles); got != nil {
		t.Errorf("openedDiffFiles = %v; want nil", got)
	}
}

// TestRetryScopingRebuildsTheDiff checks the scoping reaches the payload: the
// retry's diff holds the opened file's block and not its sibling's.
func TestRetryScopingRebuildsTheDiff(t *testing.T) {
	base := retryInputs{prompt: "P:" + diffFixture, diff: diffFixture, turns: 12}
	build := func(d string) (string, error) { return "P:" + d, nil }

	next, ok := planRetryInputs(base, retryMods{scopedFiles: []string{"a.go"}}, build)
	if !ok {
		t.Fatal("planRetryInputs refused a scoping that narrows the diff")
	}
	if !strings.Contains(next.diff, "a/a.go") {
		t.Errorf("retry diff dropped the opened file:\n%s", next.diff)
	}
	if strings.Contains(next.diff, "a/b.go") {
		t.Errorf("retry diff kept a file the session never opened:\n%s", next.diff)
	}
	if !strings.HasPrefix(next.prompt, "P:") {
		t.Error("retry prompt was not rebuilt from the narrowed diff")
	}
}

// TestRetryScopingFailsOpen: a prompt builder that errors costs the retry its
// narrowing, never the retry itself.
func TestRetryScopingFailsOpen(t *testing.T) {
	base := retryInputs{prompt: "P", diff: diffFixture, turns: 12}
	build := func(string) (string, error) { return "", context.Canceled }
	mods := retryMods{turnBudget: 6, instruction: answerNowInstruction, scopedFiles: []string{"a.go"}}

	next, ok := planRetryInputs(base, mods, build)
	if !ok {
		t.Fatal("a failed prompt rebuild must not cancel the retry")
	}
	if next.diff != base.diff {
		t.Error("diff was narrowed despite the rebuild failing")
	}
	if next.turns != 6 || !strings.Contains(next.prompt, answerNowInstruction) {
		t.Error("the other two modifications were lost with the scoping")
	}
}

// TestAnswerNowInstructionIsAppendedLast pins where the instruction goes. It
// has to override the pass instructions and the JSON contract above it, which
// only works if it is read after them — and being a pure suffix is what keeps
// the retry reading the shared prompt prefix out of the provider's cache
// instead of paying to write it again.
func TestAnswerNowInstructionIsAppendedLast(t *testing.T) {
	base := retryInputs{prompt: "the original prompt", diff: diffFixture, turns: 12}
	next, ok := planRetryInputs(base, retryMods{instruction: answerNowInstruction}, nil)
	if !ok {
		t.Fatal("planRetryInputs refused an appended instruction")
	}
	if !strings.HasPrefix(next.prompt, base.prompt) {
		t.Error("retry prompt does not open with the original's bytes")
	}
	if !strings.HasSuffix(next.prompt, answerNowInstruction) {
		t.Error("the answer-now instruction is not the last thing the model reads")
	}
}

// --- what the runner does with a budget -----------------------------------

// TestTurnBudgetRidesOnTheContext pins the seam: the reduced budget reaches the
// runner without widening PassRunner, and a runner that ignores it is not
// broken.
func TestTurnBudgetRidesOnTheContext(t *testing.T) {
	if n := turnBudgetFrom(context.Background()); n != 0 {
		t.Errorf("turnBudgetFrom(bare ctx) = %d; want 0", n)
	}
	if n := turnBudgetFrom(withTurnBudget(context.Background(), 6)); n != 6 {
		t.Errorf("turnBudgetFrom = %d; want 6", n)
	}
	// A non-positive budget is not a budget: it must leave the context alone
	// so the runner falls back to the configured cap rather than passing
	// "--max-turns 0" to the provider.
	if n := turnBudgetFrom(withTurnBudget(context.Background(), 0)); n != 0 {
		t.Errorf("turnBudgetFrom = %d; want 0", n)
	}
}

// TestPassTurnBudgetPrefersConfig checks which number the reduction halves.
func TestPassTurnBudgetPrefersConfig(t *testing.T) {
	if got := passTurnBudget(Config{}); got != assayMaxTurns {
		t.Errorf("passTurnBudget(zero cfg) = %d; want %d", got, assayMaxTurns)
	}
	if got := passTurnBudget(Config{MaxTurnsPerPass: 20}); got != 20 {
		t.Errorf("passTurnBudget = %d; want 20", got)
	}
}

// --- the opened-file tracker ----------------------------------------------

func TestFileTrackerRecordsToolUsePaths(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Read","input":{"file_path":"/w/a.go"}}]`))
	tr.observe(toolEvent(t, `[{"type":"text","text":"thinking"},{"type":"tool_use","name":"Edit","input":{"file_path":"/w/pkg/b.go"}}]`))
	// A repeat is the same file, and a tool with no file_path names none.
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Read","input":{"file_path":"/w/a.go"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]`))

	got := tr.paths()
	if len(got) != 2 || got[0] != "/w/a.go" || got[1] != "/w/pkg/b.go" {
		t.Errorf("paths = %v; want [/w/a.go /w/pkg/b.go] in first-seen order", got)
	}
}

func TestFileTrackerIgnoresNonAssistantAndGarbage(t *testing.T) {
	tr := newFileTracker()
	tr.observe(smithEvent("system", `[{"type":"tool_use","input":{"file_path":"/w/a.go"}}]`))
	tr.observe(smithEvent("assistant", `not json`))
	if got := tr.paths(); len(got) != 0 {
		t.Errorf("paths = %v; want none", got)
	}
	// A nil tracker answers rather than panicking: the runner installs one
	// unconditionally, but nothing downstream should have to know that.
	var nilTracker *fileTracker
	nilTracker.observe(toolEvent(t, `[{"type":"tool_use","input":{"file_path":"/w/a.go"}}]`))
	if got := nilTracker.paths(); got != nil {
		t.Errorf("nil tracker paths = %v; want nil", got)
	}
}

func TestFileTrackerIsBounded(t *testing.T) {
	tr := newFileTracker()
	for i := 0; i < maxTrackedFiles+50; i++ {
		tr.add("/w/f" + string(rune('a'+i%26)) + "/" + string(rune('a'+i/26)) + ".go")
	}
	if got := len(tr.paths()); got > maxTrackedFiles {
		t.Errorf("tracked %d files; cap is %d", got, maxTrackedFiles)
	}
}

// TestWithSessionToolsAttachesToTheFailure pins the carrier that matters: a
// session that died on turns has no PassOutput to hang its file list on, so it
// has to ride out on the error.
func TestWithSessionToolsAttachesToTheFailure(t *testing.T) {
	perr := newPassError("logic", ReasonMaxTurns, "out of turns", nil)
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Read","input":{"file_path":"/w/a.go"}}]`))
	if _, err := withSessionTools(PassOutput{}, perr, tr); err != perr {
		t.Fatalf("withSessionTools replaced the error: %v", err)
	}
	if len(perr.OpenedFiles) != 1 || perr.OpenedFiles[0] != "/w/a.go" {
		t.Errorf("PassError.OpenedFiles = %v; want [/w/a.go]", perr.OpenedFiles)
	}
	if perr.ToolCalls != 1 {
		t.Errorf("PassError.ToolCalls = %d; want 1", perr.ToolCalls)
	}
	if got := passErrorFiles(perr); len(got) != 1 {
		t.Errorf("passErrorFiles = %v; want the attached list", got)
	}
	if got := passErrorFiles(context.Canceled); got != nil {
		t.Errorf("passErrorFiles(foreign error) = %v; want nil", got)
	}

	out, err := withSessionTools(PassOutput{Text: "{}"}, nil, tr)
	if err != nil || len(out.OpenedFiles) != 1 || out.ToolCalls != 1 {
		t.Errorf("success path: out = %+v err = %v", out, err)
	}
}

// TestWithSessionToolsCountsToolsThatNameNoFile is the whole reason the call
// count is not derived from the file list: a session that only grepped or ran a
// command explored, and reporting it as a session that never used a tool is the
// exact misreading this telemetry exists to prevent.
func TestWithSessionToolsCountsToolsThatNameNoFile(t *testing.T) {
	tr := newFileTracker()
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]`))
	tr.observe(toolEvent(t, `[{"type":"tool_use","name":"Grep","input":{"pattern":"x"}}]`))

	out, err := withSessionTools(PassOutput{Text: "{}"}, nil, tr)
	if err != nil {
		t.Fatalf("withSessionTools: %v", err)
	}
	if out.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d; want 2", out.ToolCalls)
	}
	if len(out.OpenedFiles) != 0 {
		t.Errorf("OpenedFiles = %v; want none — neither tool named a file", out.OpenedFiles)
	}
}

func TestMergeOpenedFilesDedupesAcrossSessions(t *testing.T) {
	got := mergeOpenedFiles([]string{"a", "b"}, []string{"b", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("mergeOpenedFiles = %v; want [a b c]", got)
	}
	if got := mergeOpenedFiles([]string{"a"}, nil); len(got) != 1 {
		t.Errorf("mergeOpenedFiles = %v; want the left list unchanged", got)
	}
}

// --- end to end -----------------------------------------------------------

// TestReviewRetryRunsOnAReducedTurnBudget pins the cost half of the change:
// the retry's session is given half the budget the one that just burned it
// had, so a pass that wanders twice costs about 1.5 sessions rather than 2.
func TestReviewRetryRunsOnAReducedTurnBudget(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil)}}
	}
	script["logic"] = []stubResp{
		{err: maxTurnsErr("logic", 12, 0.25)},
		{text: findingsJSON(t, nil), turns: 5},
	}

	runner := newScriptRunner(script)
	if _, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run)); err != nil {
		t.Fatalf("Review: %v", err)
	}
	calls := runner.callsFor("logic")
	if len(calls) != 2 {
		t.Fatalf("expected 2 logic sessions; got %d", len(calls))
	}
	if calls[0].turnBudget != assayMaxTurns {
		t.Errorf("first session budget = %d; want %d", calls[0].turnBudget, assayMaxTurns)
	}
	want, ok := reducedTurnBudget(assayMaxTurns)
	if !ok {
		t.Fatal("the engine default must be reducible")
	}
	if calls[1].turnBudget != want {
		t.Errorf("retry budget = %d; want %d", calls[1].turnBudget, want)
	}
}

// TestReviewRetryScopesDiffToOpenedFiles pins the third modification against a
// real run: the failed session reported reading one of the two changed files,
// so the retry is handed that file's hunk and not the other's.
func TestReviewRetryScopesDiffToOpenedFiles(t *testing.T) {
	failed := maxTurnsErr("logic", 12, 0.25)
	failed.OpenedFiles = []string{"/w/tree/a.go"}

	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil)}}
	}
	script["logic"] = []stubResp{{err: failed}, {text: findingsJSON(t, nil)}}

	req := testRequest()
	req.Diff = diffFixture
	runner := newScriptRunner(script)
	if _, err := Review(context.Background(), req, openTestDB(t), DefaultConfig().WithRunner(runner.run)); err != nil {
		t.Fatalf("Review: %v", err)
	}
	calls := runner.callsFor("logic")
	if len(calls) != 2 {
		t.Fatalf("expected 2 logic sessions; got %d", len(calls))
	}
	if !strings.Contains(calls[0].prompt, "b/b.go") {
		t.Fatal("the first session was not given the whole diff; the fixture is wrong")
	}
	if strings.Contains(calls[1].prompt, "b/b.go") {
		t.Error("retry still carries a file the failed session never opened")
	}
	if !strings.Contains(calls[1].prompt, "b/a.go") {
		t.Error("retry dropped the file the failed session was reading")
	}
}

// TestReviewDoesNotRetryPastTheBudgetedAttempt guards the loop bound: the
// modified retry is still one retry. A pass that exhausts its budget twice is
// telling us the budget is wrong, and a third session at full price is not the
// answer.
func TestReviewDoesNotRetryPastTheBudgetedAttempt(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil)}}
	}
	script["logic"] = []stubResp{{err: maxTurnsErr("logic", 12, 0.25)}}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if calls := runner.callsFor("logic"); len(calls) != 1+maxTurnsRetries {
		t.Errorf("logic sessions = %d; want %d", len(calls), 1+maxTurnsRetries)
	}
	if res.Status != RunStatusPartial {
		t.Errorf("Status = %q; want %q", res.Status, RunStatusPartial)
	}
	rep := passReport(res, "logic")
	if rep == nil {
		t.Fatal("no PassReport for logic")
	}
	if !rep.Retried || rep.RetrySkipped {
		t.Errorf("telemetry = {Retried:%v RetrySkipped:%v}; want {true false} — the retry ran and failed",
			rep.Retried, rep.RetrySkipped)
	}
}

// TestRetrySkippedRendersInTelemetry pins the one surface a dropped retry has.
// A pass that earned a re-run and did not get one looks exactly like a pass
// that was never eligible everywhere else — both are just a failed pass in the
// coverage text — so if this line does not say so, nothing does.
func TestRetrySkippedRendersInTelemetry(t *testing.T) {
	got := RenderPassTelemetry([]PassReport{
		{Name: "logic", Turns: 12, TerminationReason: ReasonMaxTurns, Attempts: 1, RetrySkipped: true},
	})
	if !strings.Contains(got, "retry=skipped") {
		t.Errorf("RenderPassTelemetry = %q; want it to carry retry=skipped", got)
	}
	// Retried wins: a pass cannot both have been re-run and have had its
	// re-run dropped, and the count is the more specific statement.
	got = RenderPassTelemetry([]PassReport{
		{Name: "logic", Turns: 12, Attempts: 2, Retried: true},
	})
	if !strings.Contains(got, "retry=1") || strings.Contains(got, "skipped") {
		t.Errorf("RenderPassTelemetry = %q; want retry=1", got)
	}
	// And a pass that was never eligible carries neither.
	got = RenderPassTelemetry([]PassReport{{Name: "logic", Turns: 4, Attempts: 1}})
	if strings.Contains(got, "retry") {
		t.Errorf("RenderPassTelemetry = %q; want no retry field", got)
	}
}

// TestToolCallsRenderInTelemetry pins the field the whole exploration question
// is answered by. A pass that answered in two turns having made no tool call
// reviewed the diff text and nothing else; by turns alone it is
// indistinguishable from a cheap pass that did its job, so tools=0 has to be
// visible on the line.
func TestToolCallsRenderInTelemetry(t *testing.T) {
	got := RenderPassTelemetry([]PassReport{
		{Name: "security", Turns: 7, Attempts: 1, ToolCalls: 9, FilesRead: 4},
	})
	if !strings.Contains(got, "tools=9 files=4") {
		t.Errorf("RenderPassTelemetry = %q; want it to carry tools=9 files=4", got)
	}
	// A pass that used no tool, and a backend that streams no tool events, are
	// one value here — so both are omitted rather than rendered as a zero that
	// claims to know which of the two happened.
	got = RenderPassTelemetry([]PassReport{{Name: "security", Turns: 2, Attempts: 1}})
	if strings.Contains(got, "tools=") || strings.Contains(got, "files=") {
		t.Errorf("RenderPassTelemetry = %q; want no tool fields when nothing was observed", got)
	}
}

// --- fixtures -------------------------------------------------------------

// smithEvent builds a stream event of the given type carrying content blocks.
func smithEvent(typ, content string) smith.StreamEvent {
	return smith.StreamEvent{Type: typ, Message: json.RawMessage(`{"id":"msg_1","content":` + content + `}`)}
}

// toolEvent is smithEvent for the one event type the tracker reads, with the
// content blocks checked as valid JSON so a malformed fixture fails as a test
// bug rather than as the tracker silently ignoring it. (cost_test.go's
// assistantEvent is the usage-carrying sibling: the two fixtures build the same
// event type for the two different things read off it.)
func toolEvent(t *testing.T, content string) smith.StreamEvent {
	t.Helper()
	if !json.Valid([]byte(content)) {
		t.Fatalf("fixture is not valid JSON: %s", content)
	}
	return smithEvent("assistant", content)
}

// diffFixture is a two-file unified diff, so a scoping that keeps one file has
// something to drop.
const diffFixture = "diff --git a/a.go b/a.go\n" +
	"--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n" +
	"diff --git a/b.go b/b.go\n" +
	"--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old\n+new\n"
