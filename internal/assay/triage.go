package assay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/diff"
	"github.com/Robin831/Forge/internal/provider"
)

// triageResult is what the Triage pass produces: the subset of changed files
// that warrant deeper review, plus free-form notes passed to the deep passes.
type triageResult struct {
	// ReviewFiles is the scoped file list. Empty means "review everything".
	ReviewFiles []string `json:"review_files"`
	// Notes is short guidance forwarded to the deep passes.
	Notes string `json:"notes"`
}

// triageRun is what a triage attempt reports back: the scoping decision plus
// the same telemetry a deep pass carries, so triage appears in the run's
// per-pass line alongside them rather than as a gap in it. It is a struct
// because these travel together and always have — a fifth return value is how
// a caller comes to drop one.
type triageRun struct {
	// result is the scoping decision. Zero-valued when the pass failed.
	result triageResult
	// usage is cumulative across every session the pass made — tokens, the
	// prompt-cache halves and the cost alike — and is reported on the error
	// paths too.
	usage cost.Usage
	// estCostUSD is costTracker's estimate of that same spend, cumulative on
	// usage's terms — the unit the per-pass spend ceiling is written in. Zero
	// when no ceiling is configured or the backend streams no per-turn usage.
	estCostUSD float64
	// turns is the recorded (final) session's turn count.
	turns int
	// toolCalls is how many tool calls every session of the pass made together,
	// cumulative like usage rather than final-session like turns. Triage is
	// expected to report zero — it reads the diff and answers — which is
	// precisely why the figure is carried: a triage pass that starts opening
	// files is scoping work that has stopped being cheap.
	toolCalls int
	// filesRead is how many distinct files those sessions opened between them,
	// carried beside toolCalls so the rendered line cannot report a pass that
	// opened files as having opened none. It counts the file-shaped entries of
	// the tracked list (countFilesRead), not its length: the list is also what
	// the retry's diff scoping selects from, and that consumer is happy with a
	// directory or a fragment that matches nothing while this one is not.
	filesRead int
	// opened is the union of the files those sessions named, kept so a
	// failover folds the next provider's reads into one set before filesRead
	// counts it.
	opened []string
	// provider, model, failedOver and byProvider are passResult's fields of the
	// same names, on the same terms: the entry triage ended on, the model that
	// session reported, whether it got there past a rate-limited head, and the
	// usage split by the provider each session ran on.
	provider   provider.Provider
	model      string
	failedOver bool
	byProvider []ProviderUsage
}

// runTriage runs the scoping pass. Like the deep passes it parses strict JSON
// with a single retry; a second failure is surfaced as a run error. It reports
// the cost, the turn count of the session it recorded and the session's
// prompt-cache accounting, so triage appears in the run's per-pass telemetry
// alongside the deep passes.
//
// The cost is cumulative across every session the pass made and is reported on
// the error paths too — the provider bills a session that failed, and Review
// banks this value before it checks the error, so a run that dies here still
// carries its spend out through RunError. The turn count is the recorded
// session's, i.e. the last one, for the same reason the deep passes report
// theirs that way: a sum says nothing about how close any one session came to
// the --max-turns budget.
//
// Triage gets no turn-budget retry: unlike a deep pass it is a hard gate — a
// triage failure aborts the whole run rather than costing one pass's coverage,
// so there is no partial outcome for a retry to salvage.
//
// It does walk its own provider chain, on runDeepPass's rule: the strict-JSON
// re-prompt runs on one provider, and only a rate limit moves the pass to the
// next entry. A hard gate is the pass that most needs the fallback — a
// rate-limited triage head otherwise fails the whole run — and the one that
// must least loop on an auth failure, which ends the walk like everything else
// that is not a rate limit.
func runTriage(ctx context.Context, runner PassRunner, cfg Config, req ReviewRequest, filteredDiff string) (triageRun, error) {
	chain := cfg.providersFor(passTriage.Name)
	prompt, err := buildTriagePrompt(req, filteredDiff)
	if err != nil {
		return triageRun{provider: chain[0], model: chain[0].Model}, err
	}

	var run triageRun
	var usage cost.Usage
	var est float64
	var calls int
	var opened []string
	var byProvider []ProviderUsage
	for i, pv := range chain {
		r, rerr := runTriageOn(withPassProvider(ctx, pv), runner, prompt)
		usage.Add(r.usage)
		est += r.estCostUSD
		calls += r.toolCalls
		opened = mergeOpenedFiles(opened, r.opened)
		byProvider = addProviderUsage(byProvider, pv.Kind, r.usage)

		run = r
		run.usage = usage
		run.estCostUSD = est
		run.toolCalls = calls
		run.opened = opened
		run.filesRead = countFilesRead(opened)
		run.byProvider = byProvider
		run.provider = pv
		run.model = sessionModel(r.model, pv)
		run.failedOver = i > 0
		err = rerr
		if err == nil || !failsOver(classifyPassError(passTriage.Name, err)) {
			break
		}
	}
	return run, err
}

// runTriageOn runs the triage session, and its strict-JSON re-prompt, on the
// provider withPassProvider put on ctx.
func runTriageOn(ctx context.Context, runner PassRunner, prompt string) (triageRun, error) {
	out, err := runner(ctx, passTriage.Name, passTriage.Tier, prompt)
	if err != nil {
		// One session has run, so its own list IS the union — there is nothing
		// earlier to merge with. The paths below, which do merge, are the ones
		// reached after a session has already produced a PassOutput.
		u, turns, calls, est := passErrorTelemetry(err)
		opened := passErrorFiles(err)
		return triageRun{usage: u, estCostUSD: est, turns: turns, toolCalls: calls,
			filesRead: countFilesRead(opened), opened: opened, model: passErrorModel(err)}, err
	}
	opened := out.OpenedFiles
	run := triageRun{
		usage:      out.usage(),
		estCostUSD: out.EstCostUSD,
		turns:      out.Turns,
		toolCalls:  out.ToolCalls,
		filesRead:  countFilesRead(opened),
		opened:     opened,
		model:      out.Model,
	}

	res, perr := parseTriage(out.Text)
	if perr != nil {
		out2, err2 := runner(ctx, passTriage.Name, passTriage.Tier, prompt+"\n\n"+strictJSONReminder)
		if err2 != nil {
			u2, turns2, calls2, est2 := passErrorTelemetry(err2)
			run.usage.Add(u2)
			run.estCostUSD += est2
			run.turns = turns2
			run.toolCalls += calls2
			run.opened = mergeOpenedFiles(opened, passErrorFiles(err2))
			run.filesRead = countFilesRead(run.opened)
			if m := passErrorModel(err2); m != "" {
				run.model = m
			}
			return run, err2
		}
		run.usage.Add(out2.usage())
		run.estCostUSD += out2.EstCostUSD
		run.turns = out2.Turns
		run.toolCalls += out2.ToolCalls
		run.opened = mergeOpenedFiles(opened, out2.OpenedFiles)
		run.filesRead = countFilesRead(run.opened)
		if out2.Model != "" {
			run.model = out2.Model
		}
		res, perr = parseTriage(out2.Text)
		if perr != nil {
			return run, fmt.Errorf("assay pass %s: invalid JSON output after retry: %w", passTriage.Name, perr)
		}
	}
	run.result = res
	return run, nil
}

// buildTriagePrompt assembles the Triage prompt: the same shared head every
// deep pass carries (writeSharedPromptHead) over the already-filtered diff,
// followed by triage's own instructions and its JSON contract.
//
// It is ordered shared-material-first for the same reason buildPassPrompt is —
// a prompt cache matches from the first byte — and shares the head with the
// deep passes as a consequence rather than as the plan: triage is fed the
// FILTERED diff while the deep passes get the SCOPED one, so the two prompts
// part company at the diff the moment triage actually narrows the file set.
// That is exactly why the fan-out is primed from a deep pass and not from
// triage (see Review): priming here would quietly stop working on the runs
// where triage does its job.
func buildTriagePrompt(req ReviewRequest, filteredDiff string) (string, error) {
	const contract = "## Required Output\n\n" +
		"Respond with a single JSON object and nothing else:\n\n" +
		"```json\n" +
		`{"review_files": ["path/one", "path/two"], "notes": "short guidance for the deep review"}` +
		"\n```\n\n" +
		"- `review_files` lists the changed files that deserve close review; omit purely " +
		"mechanical or trivial files. Use an empty array to mean \"review everything\".\n" +
		"- `notes` is optional one-paragraph guidance (risk areas, intent). Keep it brief."

	instructions, err := loadPrompt(passTriage.promptFile)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// No triage notes: they are what this pass produces, not what it reads.
	writeSharedPromptHead(&b, req, filteredDiff, "")
	b.WriteString("\n")
	b.WriteString(instructions)
	b.WriteString("\n\n")
	b.WriteString(contract)
	b.WriteString("\n")
	return b.String(), nil
}

// parseTriage extracts and decodes the triage JSON object. A missing or
// malformed object is an error so the caller can retry.
func parseTriage(text string) (triageResult, error) {
	js := extractJSONObject(text, "review_files")
	if js == "" {
		return triageResult{}, fmt.Errorf("no JSON object with a \"review_files\" key found")
	}
	var res triageResult
	if err := json.Unmarshal([]byte(js), &res); err != nil {
		return triageResult{}, fmt.Errorf("decoding triage JSON: %w", err)
	}
	return res, nil
}

// scopeDiffToFiles keeps only the diff blocks whose b-side path is in files.
// When files is empty the diff is returned unchanged. If scoping would drop
// every block (e.g. the model named files not present in the diff), the full
// diff is returned so the deep passes still have content to review.
func scopeDiffToFiles(unifiedDiff string, files []string) string {
	if len(files) == 0 || strings.TrimSpace(unifiedDiff) == "" {
		return unifiedDiff
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[strings.TrimSpace(f)] = true
	}

	const marker = "diff --git "
	const sep = "\ndiff --git "

	remaining := unifiedDiff
	var preamble string
	if !strings.HasPrefix(remaining, marker) {
		idx := strings.Index(remaining, sep)
		if idx == -1 {
			return unifiedDiff // no block headers
		}
		preamble = remaining[:idx+1]
		remaining = remaining[idx+1:]
	}

	var kept strings.Builder
	for len(remaining) > 0 {
		nextIdx := strings.Index(remaining[len(marker):], sep)
		var block string
		if nextIdx == -1 {
			block = remaining
			remaining = ""
		} else {
			end := len(marker) + nextIdx + 1
			block = remaining[:end]
			remaining = remaining[end:]
		}
		headerLine := block
		if nl := strings.IndexByte(block, '\n'); nl != -1 {
			headerLine = block[:nl]
		}
		if path := diff.ParseGitPath(headerLine); path != "" && keep[path] {
			kept.WriteString(block)
		}
	}

	if strings.TrimSpace(kept.String()) == "" {
		return unifiedDiff
	}
	return preamble + kept.String()
}
