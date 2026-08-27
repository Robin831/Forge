package assay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Robin831/Forge/internal/smith"
)

// minRetryTurns is the floor a retry's turn budget is never reduced below. A
// pass that is told to answer from what it already has still needs a turn to
// read the instruction, a turn to think and a turn to emit its JSON; cutting
// below that would make the retry fail on turns by construction, which is the
// one outcome worse than not retrying at all.
const minRetryTurns = 3

// maxTrackedFiles bounds how many opened files one session's tracker keeps.
// The list exists to narrow a retry's diff, and a session that opened two
// hundred distinct files was not being narrow about anything — past that the
// cap simply stops the map growing on a runaway, which is precisely the session
// shape that reaches this code.
const maxTrackedFiles = 200

// answerNowInstruction is appended to a pass prompt on the turn-budget retry.
//
// It is the difference that makes the retry a different question rather than
// the same one asked twice. A pass that ends on error_max_turns spent its whole
// budget exploring the repository and never emitted JSON; re-sending
// byte-identical inputs into a fresh session buys the identical exploration
// again at full price, and the second session has exactly as much reason to
// wander as the first did. So the retry says what the first attempt could not
// know: stop looking, answer from what is already in front of you.
//
// It goes at the very end of the prompt, after the pass's own instructions and
// the JSON contract, because that is where an instruction that overrides them
// has to be read to be obeyed — and because everything above it stays
// byte-identical to the first attempt, so the retry still reads the prompt
// prefix out of the provider's cache rather than writing it a second time.
const answerNowInstruction = "## Answer Now — Final Turn Budget\n\n" +
	"Your previous session for this pass ran out of turns before it produced any output. " +
	"This session has a deliberately smaller turn budget and is the last one this pass gets.\n\n" +
	"Do NOT explore further: read no more files, run no more searches, make no more tool calls. " +
	"Answer immediately, from the diff above and whatever you can already see, using the exact " +
	"JSON object described above and nothing else. If confirming something would have required " +
	"opening a file, leave it out rather than spending a turn on it — a shorter answer now is " +
	"worth more than a complete one you never get to give. " +
	"`{\"findings\": []}` is a valid answer."

// passTurnBudget resolves the per-session turn cap for a pass: the anvil's
// assay.max_turns_per_pass where it set one, else the engine default.
func passTurnBudget(cfg Config) int {
	if cfg.MaxTurnsPerPass > 0 {
		return cfg.MaxTurnsPerPass
	}
	return assayMaxTurns
}

// turnBudgetKey carries a session's turn cap into the pass runner.
type turnBudgetKey struct{}

// withTurnBudget returns a context carrying the --max-turns value the next
// session should run with.
//
// It rides on the context for the same reason the staggered fan-out's release
// signal does (see withFirstOutput): PassRunner is the engine's one seam to the
// backend and every caller and stub implements it, so an optional per-session
// knob is not worth widening a contract six call sites satisfy. A runner that
// ignores it — an injected stub, a backend with no turn concept — is not
// broken; it simply runs the session it always ran, and the retry still differs
// from the original by its appended instruction.
func withTurnBudget(ctx context.Context, turns int) context.Context {
	if turns <= 0 {
		return ctx
	}
	return context.WithValue(ctx, turnBudgetKey{}, turns)
}

// turnBudgetFrom returns the turn cap carried by ctx, or 0 when there is none.
func turnBudgetFrom(ctx context.Context) int {
	n, _ := ctx.Value(turnBudgetKey{}).(int)
	return n
}

// retryInputs is everything about a pass attempt that the provider is billed
// for: the prompt it sends, the diff that prompt was built around, and the turn
// budget the session runs under. Two attempts with equal retryInputs are the
// same request, and running the second one buys nothing the first did not
// already buy.
//
// The diff is carried alongside the prompt even though it is embedded in it,
// because scoping the retry means rebuilding the prompt from a narrower diff —
// so the diff is an input in its own right and the next attempt's scoping has
// to start from this attempt's, not from the run's.
type retryInputs struct {
	prompt string
	diff   string
	turns  int
}

func (r retryInputs) hash() string { return retryPayloadHash(r.prompt, r.diff, r.turns) }

// retryPayloadHash digests one attempt's billable payload. It is the mechanical
// form of the rule this file exists for: a retry whose hash equals the
// original's is the original, and is not worth paying for.
//
// Fields are length-prefixed rather than separated by a delimiter, since no
// byte is unavailable to a prompt: with a separator, a prompt ending in the
// separator followed by an empty diff would digest identically to a shorter
// prompt with a diff, and the guard would then wave through a payload it should
// have caught.
func retryPayloadHash(prompt, unifiedDiff string, turns int) string {
	h := sha256.New()
	for _, f := range [...]string{prompt, unifiedDiff, strconv.Itoa(turns)} {
		fmt.Fprintf(h, "%d:", len(f))
		h.Write([]byte(f))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// retryMods are the modifications a turn-budget retry applies to the attempt
// that failed. All three are optional and all three are ways of making the
// second session a cheaper, more constrained question than the first:
//
//   - turnBudget: a reduced --max-turns, so a session that wanders again costs
//     roughly half of what the first one did rather than the same.
//   - instruction: the appended "answer now" directive.
//   - scopedFiles: the changed files the failed session actually opened, which
//     is the closest thing to evidence of where it thought the risk was — the
//     retry's diff is narrowed to those, so the model has less to read and less
//     to be tempted by.
//
// A zero retryMods produces a retry identical to the original, which is exactly
// what planRetryInputs refuses.
type retryMods struct {
	turnBudget  int
	instruction string
	scopedFiles []string
}

func (m retryMods) empty() bool {
	return m.turnBudget <= 0 && m.instruction == "" && len(m.scopedFiles) == 0
}

// buildRetryMods assembles the modifications for a pass's turn-budget retry.
// openedFiles is what the failed session read (empty for a backend that streams
// no tool events) and diffFiles is the set of files present in the diff it was
// given.
//
// ok is false when nothing could be constructed. The caller drops the retry
// entirely in that case and lets the run report partial coverage: a pass that
// failed on turns and cannot be asked a different question is a pass whose
// second full-price session would end exactly where the first did.
//
// As it stands the instruction is unconditional, so ok is always true and the
// refusal is a backstop rather than a state a run reaches — the reachable
// refusal is planRetryInputs', which judges the assembled payload rather than
// the intent. Both are kept because the instruction is the only reason ok is
// always true today, and "the retry is free to be identical again the moment
// somebody makes that conditional" is exactly the regression this bead was
// filed about.
func buildRetryMods(cfg Config, openedFiles, diffFiles []string) (retryMods, bool) {
	m := retryMods{instruction: answerNowInstruction}
	if b, ok := reducedTurnBudget(passTurnBudget(cfg)); ok {
		m.turnBudget = b
	}
	m.scopedFiles = openedDiffFiles(openedFiles, diffFiles)
	if m.empty() {
		return retryMods{}, false
	}
	return m, true
}

// reducedTurnBudget halves a session's turn cap, floored at minRetryTurns. It
// reports ok=false when the floor is already at or above the original — an
// anvil that configured a budget of three turns has no room to give the retry a
// smaller one, and pretending otherwise would raise the budget rather than
// lower it.
func reducedTurnBudget(orig int) (int, bool) {
	if orig <= 0 {
		return 0, false
	}
	reduced := max(orig/2, minRetryTurns)
	if reduced >= orig {
		return 0, false
	}
	return reduced, true
}

// openedDiffFiles returns the diff files the failed session opened, in diff
// order. It returns nil when the session opened all of them (or none), since
// scoping to everything narrows nothing and would only cost a prompt rebuild.
//
// Only files already in the diff can be selected, so a path the session read
// from outside the change — a config file, a test helper, anything an injected
// tool name reported — can never widen the retry's scope, only fail to match.
func openedDiffFiles(opened, diffFiles []string) []string {
	if len(opened) == 0 || len(diffFiles) == 0 {
		return nil
	}
	var out []string
	for _, f := range diffFiles {
		for _, o := range opened {
			if pathRefersTo(o, f) {
				out = append(out, f)
				break
			}
		}
	}
	if len(out) == len(diffFiles) {
		return nil
	}
	return out
}

// pathRefersTo reports whether an opened path names the repo-relative diff path
// want. A provider reports the absolute path it read
// ("/home/x/.workers/y/internal/assay/passes.go") while a diff header names it
// relative to the repository root, so the comparison is a path-component suffix
// match rather than equality.
//
// The match can be too generous — an opened file in an unrelated checkout with
// the same tail matches — and that is the harmless direction: the only effect
// is that the retry keeps a diff file it would otherwise have dropped.
func pathRefersTo(opened, want string) bool {
	o := normalizeSlashes(opened)
	w := normalizeSlashes(want)
	if o == "" || w == "" {
		return false
	}
	return o == w || strings.HasSuffix(o, "/"+w)
}

// normalizeSlashes puts a path into the one form the suffix comparison can be
// made in: forward slashes, no "./" prefix, no trailing slash.
func normalizeSlashes(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.TrimSuffix(p, "/")
}

// planRetryInputs applies mods to the attempt that failed and returns the
// retry's inputs, or ok=false when the result would be the same request again.
//
// build rebuilds the pass prompt around a narrower diff; a nil build, or one
// that fails, simply leaves the diff alone — a scoping that cannot be assembled
// costs the retry its narrowing, never the retry itself.
//
// The hash comparison at the end is the guard the whole file is for, and it is
// deliberately on the assembled payload rather than on mods: a modification
// that turns out to change nothing (a "reduced" budget equal to the original, a
// scoping that scopeDiffToFiles declined because it would have emptied the
// diff) is indistinguishable from no modification at all by the only measure
// that matters, which is what the provider is asked to bill for.
func planRetryInputs(base retryInputs, mods retryMods, build func(unifiedDiff string) (string, error)) (retryInputs, bool) {
	next := base
	if mods.turnBudget > 0 {
		next.turns = mods.turnBudget
	}
	if len(mods.scopedFiles) > 0 && build != nil {
		if d := scopeDiffToFiles(base.diff, mods.scopedFiles); d != base.diff {
			if p, err := build(d); err == nil {
				next.diff, next.prompt = d, p
			}
		}
	}
	if mods.instruction != "" {
		next.prompt += "\n\n" + mods.instruction
	}
	if next.hash() == base.hash() {
		return base, false
	}
	return next, true
}

// fileTracker records what one pass session did with its tools: which files it
// opened, and how many tool calls it made at all, read off the provider's
// tool-use events as they stream.
//
// The file list exists for the retry: the files a session chose to open are the
// only in-band evidence of where it thought this diff's risk was, and a session
// that died on turns never got to say so any other way. A backend that streams
// no structured tool events yields an empty list, and the retry is then
// modified by budget and instruction alone.
//
// The call count exists for telemetry, and it is deliberately not derivable
// from the file list: a pass that greps, lists a directory or runs a command
// has explored without naming a file_path, and a pass that made no tool call at
// all answered from the diff text alone. That second case is the one worth
// seeing — it is what the security pass was doing on the majority of runs, and
// the turn count was only ever a weak proxy for it (see PassReport.ToolCalls).
// It keeps counting past maxTrackedFiles, since it is one int and the runaway
// session is exactly the one whose size is worth knowing.
//
// What this tracker counts is the Claude-shaped stream, and a zero from it is
// therefore not yet an answer: observedToolCalls is where a backend that
// reports the same figure another way is folded back in.
//
// It is written from the provider's stdout reader goroutine and read from the
// pass goroutine, so it is guarded.
type fileTracker struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	calls int
}

func newFileTracker() *fileTracker { return &fileTracker{} }

// toolUseEnvelope is the part of an assistant message this tracker reads: the
// content blocks, of which the tool_use ones may name a file.
//
// It matches on the presence of a file_path input rather than on a list of tool
// names, so a tool Forge has never heard of that opens a file still counts. The
// value is only ever compared against paths already in the diff
// (openedDiffFiles), so a nonsense one costs nothing.
type toolUseEnvelope struct {
	Content []struct {
		Type  string `json:"type"`
		Input struct {
			FilePath string `json:"file_path"`
		} `json:"input"`
	} `json:"content"`
}

// observe records the tool calls ev carries and any file they name. Safe to
// call from the stream reader.
func (t *fileTracker) observe(ev smith.StreamEvent) {
	if t == nil || ev.Type != "assistant" || len(ev.Message) == 0 {
		return
	}
	var env toolUseEnvelope
	if err := json.Unmarshal(ev.Message, &env); err != nil {
		return
	}
	for _, c := range env.Content {
		if c.Type != "tool_use" {
			continue
		}
		t.addCall()
		t.add(c.Input.FilePath)
	}
}

// addCall counts one tool_use block. A block naming no file still counts: the
// question this figure answers is whether the session used its tools at all.
func (t *fileTracker) addCall() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
}

func (t *fileTracker) add(path string) {
	p := strings.TrimSpace(path)
	if p == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.order) >= maxTrackedFiles {
		return
	}
	if _, dup := t.seen[p]; dup {
		return
	}
	if t.seen == nil {
		t.seen = make(map[string]struct{})
	}
	t.seen[p] = struct{}{}
	t.order = append(t.order, p)
}

// paths returns the files observed, in first-seen order. A nil tracker reports
// none, so callers never have to nil-check.
func (t *fileTracker) paths() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.order...)
}

// toolCalls returns how many tool_use blocks the session streamed. A nil
// tracker, and a backend that streams no per-message tool events, report 0 —
// which is why callers resolve the figure through observedToolCalls rather than
// reading this one directly: for a backend that reports its tool calls
// elsewhere (Gemini, in its result event) the zero here is a gap in the
// derivation, not a session that used no tool.
func (t *fileTracker) toolCalls() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// observedToolCalls resolves the tool-call figure a session reports: the count
// read off the stream when there is one, else whatever the provider reported in
// its own result accounting.
//
// It is observedTurns' rule applied to the other stream-derived figure, and for
// the same reason — a telemetry field that cannot be derived must degrade to
// the old number, never to zero. fileTracker counts Claude-shaped tool_use
// content blocks off assistant events; Gemini streams none of those and reports
// the session's tool calls once, in its result event
// (smith.StreamStats.ToolCalls), so a Gemini-backed pass that read ten files
// would otherwise report 0 — indistinguishable from a Claude pass that read
// nothing, which is the exact confusion this counter was added to remove.
//
// The file LIST has no such fallback, because there is nothing to fall back to:
// Gemini's stats carry a count and no paths. So FilesRead stays 0 for that
// backend while ToolCalls does not, which is why the two are separate fields
// and why neither implies the other.
func observedToolCalls(t *fileTracker, res *smith.Result) int {
	if n := t.toolCalls(); n > 0 {
		return n
	}
	if res != nil && res.GeminiStats != nil {
		return res.GeminiStats.ToolCalls
	}
	return 0
}

// withSessionTools attaches what a session did with its tools — the files it
// opened and the number of tool calls it made — to whichever of its two
// outcomes carries it out.
//
// The error path is the one that matters for the file list: a session that ends
// on error_max_turns is exactly the session whose retry wants to know what it
// read, and the only carrier it has is the PassError. The success path is
// populated for symmetry and for telemetry; nothing branches on it.
//
// The call count rides along on both paths for the same reason it is counted
// separately from the files: a session that explored without opening anything
// (a grep, a directory listing, a command) has an empty file list and a
// non-zero count, and folding the two would report it as a session that never
// used a tool.
//
// res is the finished session's result, and is here only so the count can fall
// back to the provider's own figure (observedToolCalls) for a backend that
// streams no per-message tool events. It may be nil, which reports the
// tracker's count alone.
func withSessionTools(out PassOutput, err error, t *fileTracker, res *smith.Result) (PassOutput, error) {
	files, calls := t.paths(), observedToolCalls(t, res)
	if err != nil {
		var pe *PassError
		if errors.As(err, &pe) {
			pe.ToolCalls = calls
			if len(files) > 0 {
				pe.OpenedFiles = files
			}
		}
		return out, err
	}
	out.ToolCalls = calls
	if len(files) > 0 {
		out.OpenedFiles = files
	}
	return out, nil
}
