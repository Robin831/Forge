package smelter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/warden"
)

// prNumberPattern matches the "copilot:PR#N" token embedded in a rule source.
// Only the literal copilot prefix is recognized — other source kinds (manual
// entries, quench fixes) do not have a remote PR with discoverable files.
var prNumberPattern = regexp.MustCompile(`copilot:PR#(\d+)`)

// extractPRNumber parses the PR number from a single source token. Returns
// (n, true) when the source contains a copilot:PR#N reference; (0, false)
// otherwise. Only the first match is returned — use extractPRNumbers when a
// source string may contain multiple copilot:PR#N tokens.
func extractPRNumber(source string) (int, bool) {
	m := prNumberPattern.FindStringSubmatch(source)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// extractPRNumbers returns all unique PR numbers found in a single source
// string. A source like "copilot:PR#1, copilot:PR#2" yields [1, 2]. Results
// are deduplicated but preserve order of first appearance.
func extractPRNumbers(source string) []int {
	matches := prNumberPattern.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[int]struct{})
	var nums []int
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		nums = append(nums, n)
	}
	return nums
}

// fetchChangedFiles is the package-level entry point used by runPathsBackfill
// to look up the files changed by a PR. It is a variable so tests can stub
// out the gh CLI invocation. The default implementation shells out to gh.
var fetchChangedFiles = fetchChangedFilesViaGH

// fetchChangedFilesViaGH calls `gh api repos/{owner}/{repo}/pulls/N/files`
// (paginated) and returns the changed file paths. repoDir must be inside a
// gh-recognized git repository so the {owner}/{repo} placeholders resolve.
func fetchChangedFilesViaGH(ctx context.Context, repoDir string, prNum int) ([]string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/files", prNum)
	cmd := executil.HideWindow(exec.CommandContext(fetchCtx, "gh", "api", endpoint, "--paginate"))
	cmd.Dir = repoDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	type ghPRFile struct {
		Filename string `json:"filename"`
	}

	// gh --paginate concatenates one JSON array per page (e.g. [..][..]). A
	// plain Unmarshal cannot handle that; loop the decoder until EOF.
	var all []ghPRFile
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var page []ghPRFile
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing gh response: %w", err)
		}
		all = append(all, page...)
	}

	files := make([]string, 0, len(all))
	for _, f := range all {
		if f.Filename != "" {
			files = append(files, f.Filename)
		}
	}
	return files, nil
}

// prFetchResult caches the outcome of a single gh API call for one PR.
type prFetchResult struct {
	files []string
	err   error
}

// backfillResult is what one Pass 3 run did, split by the claim it makes about
// a rule. Filling an empty Paths field and narrowing an over-broad one are two
// different statements — the first gates a rule that was firing everywhere, the
// second re-gates one that was already placed — and folded into a single count
// the commit subject reported a narrowed rule as "backfilled", which is a claim
// about a field that was never empty. Same argument as archivedByReason: one
// pass, two outcomes, and every aggregate rendered from it splits them again.
type backfillResult struct {
	// Filled holds the IDs of rules whose empty Paths field was populated.
	Filled []string
	// Narrowed holds the IDs of rules whose existing Paths were replaced by a
	// strictly narrower set derived from their own source PRs.
	Narrowed []string
}

// summary is the one sentence both entry points — the scheduled flush and
// `forge warden consolidate` — put in the daemon log and in the smelter_flushed
// event. One renderer rather than a copy each, and one that names the two
// outcomes separately: "backfilled paths on 40 rule(s)" over a run that filled
// none and narrowed forty describes work that did not happen. Empty when
// nothing changed, which is the caller's signal to say nothing at all.
func (r backfillResult) summary(anvilName string) string {
	var parts []string
	if n := len(r.Filled); n > 0 {
		parts = append(parts, fmt.Sprintf("backfilled paths on %d rule(s)", n))
	}
	if n := len(r.Narrowed); n > 0 {
		parts = append(parts, fmt.Sprintf("narrowed paths on %d rule(s)", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("%s for %s", strings.Join(parts, ", "), anvilName)
}

// runPathsBackfill iterates the active rules in rf and, for each rule whose
// Source carries one or more copilot:PR#N tokens, fetches the changed files for
// those PRs and derives the globs globsForRule reads out of those files and the
// rule's own text. What it then does with them depends on the rule:
//
//   - Paths empty → populated with the derived globs, the pass's original
//     behaviour, unchanged.
//   - Paths already set → replaced only when the derived set is STRICTLY
//     NARROWER than the one on file (isStrictlyNarrower) and still matches at
//     least one file of the rule's own source PR (matchesAnySource). Anything
//     else leaves the rule exactly as it is.
//
// The second half is why the pass no longer skips a rule that has paths. It
// used to, and a rule that has been through this pass once always has some, so
// the skip made Pass 3 a no-op on every rule that exists — this repository's own
// 727-rule file holds not one rule with an empty Paths field. The rules carrying
// `**/*.md` beside
// `**/*.go` because the PR that taught them also touched a doc — a Go rule
// gated to fire on documentation-only diffs — were exactly the rules the skip
// declined to look at. A rule whose current Paths cannot be narrowed at all
// (mayNarrow) is still skipped before any lookup, so the widened pass costs no
// PR fetch it cannot use.
//
// Idempotency survives the change because the derivation is a fixed point: the
// globs are a function of the rule's text and its PRs' files, neither of which
// a rewrite moves, so a second run derives the set already on file and
// isStrictlyNarrower declines an equal set.
//
// What it does NOT survive is the fetch: a rule whose paths are already narrow
// still has to be re-derived to find that out, so the pass now costs one gh
// call per distinct source PR on the file every flush rather than only on a
// file with empty paths (258 for this repository's own 727 rules, 529 for a
// 2295-rule one — a couple of minutes per anvil, once per smelter_interval).
// That is accepted rather than optimised away with a marker on the rule: a
// persisted "already narrowed" flag is a claim about a derivation that changes
// whenever languageSignals does, and a rule wrongly carrying it would never be
// looked at again. mayNarrow is the part that can be decided without the
// network, and it is decided there.
//
// Best-effort: a fetch failure for a single PR is logged and the next PR is
// tried. A rule is only modified when at least one fetch succeeded and produced
// at least one glob — except for a NARROWING, which additionally requires every
// one of the rule's source PRs to have been read. The globs replacing a
// populated Paths field are a claim about all of the rule's evidence, and one
// transient gh failure would otherwise re-gate the rule onto the surviving PR's
// files alone and never put the rest back: a later run re-derives the full,
// wider set, which isStrictlyNarrower declines.
//
// Returns the IDs it changed, split by which of the two things happened, in the
// order the rules were visited.
func (s *Smelter) runPathsBackfill(ctx context.Context, wtPath, anvilName string, rf *warden.RulesFile) backfillResult {
	return pathsBackfill(ctx, wtPath, anvilName, rf)
}

// pathsBackfill is the free-function form of runPathsBackfill. It carries no
// dependency on Smelter state so the off-cycle CLI consolidate command can
// share the same Pass 3 implementation as the scheduled smelter loop.
func pathsBackfill(ctx context.Context, wtPath, anvilName string, rf *warden.RulesFile) backfillResult {
	// Cache fetched file lists per PR number so that multiple rules referencing
	// the same PR number do not trigger redundant gh API calls.
	prCache := make(map[int]prFetchResult)

	var result backfillResult
	for i := range rf.Rules {
		if ctx.Err() != nil {
			return result
		}
		rule := &rf.Rules[i]
		filling := len(rule.Paths) == 0
		if !filling && !mayNarrow(rule.Paths) {
			continue
		}

		ev, ok := sourcePRFiles(ctx, wtPath, anvilName, rule, prCache)
		if !ok {
			continue
		}

		// A narrowing is a claim about the WHOLE of a rule's evidence: the
		// globs replacing what is on file must cover every source PR the rule
		// names, because the ones that failed to fetch are exactly the ones
		// whose paths would be dropped. A transient gh failure would otherwise
		// re-gate the rule onto the surviving PR's files alone, and permanently
		// — the next run re-derives the full, WIDER set, which
		// isStrictlyNarrower then declines, so nothing ever puts the lost
		// paths back. Filling an empty field is the opposite case and stays
		// best-effort: partial evidence there replaces a rule that is gated on
		// nothing at all, and a later run can still narrow it further.
		if !filling && !ev.complete() {
			log.Printf("[smelter] paths narrow: rule %s on %s: skipped, %d of %d source PR(s) could not be fetched",
				rule.ID, anvilName, ev.failed, ev.failed+ev.fetched)
			continue
		}
		files := ev.files

		// Derived from the rule's own text as well as the PR's files: the PR
		// says which extensions were touched, the rule says which language it
		// is about, and only the intersection is a path filter that narrows
		// anything. See globsForRule.
		globs := globsForRule(*rule, files)
		if len(globs) == 0 {
			continue
		}

		if filling {
			rule.Paths = globs
			result.Filled = append(result.Filled, rule.ID)
			logPathsDerived("backfill", rule, anvilName, nil, globs)
			continue
		}

		// A rewrite may only ever shrink what the rule matches, and what is
		// left has to still cover the evidence the rule was learned from.
		// Either test failing leaves the rule's own paths in place, which is
		// the safe direction: a set this pass got wrong is a rule that stops
		// being reviewed with nothing to say so.
		if !isStrictlyNarrower(globs, rule.Paths) || !matchesAnySource(globs, files) {
			continue
		}
		before := rule.Paths
		rule.Paths = globs
		result.Narrowed = append(result.Narrowed, rule.ID)
		logPathsDerived("narrow", rule, anvilName, before, globs)
	}
	return result
}

// sourceEvidence is what a rule's source PRs could be made to say: the union
// of their changed files, and how many of those PRs the union actually rests
// on. The two fetch counts are carried rather than folded into the file list
// because a caller cannot tell a rule whose PR touched only Go files from one
// whose second PR failed to fetch — and the two license different rewrites.
type sourceEvidence struct {
	files []string
	// fetched and failed count the rule's distinct source PRs by outcome, so
	// fetched+failed is the number of PRs the rule names.
	fetched int
	failed  int
}

// complete reports whether every source PR the rule names was read. Only then
// is the derived glob set the whole of what the rule's evidence covers.
func (e sourceEvidence) complete() bool { return e.failed == 0 }

// sourcePRFiles returns the union of the files changed by the PRs a rule's
// Source names, reporting false when the rule names no copilot:PR#N token or
// when every fetch for it failed. A partial result is returned with ok true and
// complete() false — whether partial evidence is enough is the caller's call,
// and it differs between filling an empty Paths field and narrowing a populated
// one. Fetches are served from prCache, which is what keeps one PR's files to
// one gh call however many rules cite it.
func sourcePRFiles(ctx context.Context, wtPath, anvilName string, rule *warden.Rule, prCache map[int]prFetchResult) (sourceEvidence, bool) {
	// Collect unique PR numbers referenced by this rule's sources.
	// extractPRNumbers handles source strings that contain multiple
	// copilot:PR#N tokens (e.g. "copilot:PR#1, copilot:PR#2").
	var prNums []int
	seenPR := make(map[int]struct{})
	for _, src := range rule.Source {
		for _, n := range extractPRNumbers(src) {
			if _, dup := seenPR[n]; dup {
				continue
			}
			seenPR[n] = struct{}{}
			prNums = append(prNums, n)
		}
	}
	if len(prNums) == 0 {
		return sourceEvidence{}, false
	}

	var ev sourceEvidence
	for _, prNum := range prNums {
		res, cached := prCache[prNum]
		if !cached {
			f, err := fetchChangedFiles(ctx, wtPath, prNum)
			res = prFetchResult{files: f, err: err}
			prCache[prNum] = res
		}
		if res.err != nil {
			ev.failed++
			log.Printf("[smelter] paths backfill: PR#%d for rule %s on %s: %v", prNum, rule.ID, anvilName, res.err)
			continue
		}
		ev.fetched++
		ev.files = append(ev.files, res.files...)
	}
	if ev.fetched == 0 {
		return sourceEvidence{}, false
	}
	return ev, true
}

// logPathsDerived writes the one line that says what a rule's Paths became and
// why. It names the languages the rule's own text was read as naming AND what
// became of each: the globs alone cannot say whether a rule was narrowed by its
// language or fell back to the PR's extensions, which is the one thing worth
// knowing when a backfilled rule stops firing. The outcome is read off the
// globs the rule now carries, so a discarded inference reads as discarded
// rather than as the narrowing that did not happen.
//
// A narrowing also prints what the rule carried BEFORE, since "this rule is now
// gated on **/*.go" and "this rule stopped being gated on **/*.md" are the same
// event described from the two ends, and only the second says what stopped
// being reviewed. Both lists are PR-derived, so both go out through
// safeGlobList.
func logPathsDerived(action string, rule *warden.Rule, anvilName string, before, after []string) {
	langs := languageOutcomes(ruleText(*rule), after)
	if len(langs) == 0 {
		langs = []string{"none"}
	}
	if len(before) > 0 {
		log.Printf("[smelter] paths %s: rule %s on %s: %s -> %s (languages: %s)",
			action, rule.ID, anvilName, safeGlobList(before), safeGlobList(after), strings.Join(langs, ", "))
		return
	}
	log.Printf("[smelter] paths %s: rule %s on %s -> %s (languages: %s)",
		action, rule.ID, anvilName, safeGlobList(after), strings.Join(langs, ", "))
}

// maxLoggedGlobs and maxLoggedGlobLen bound one rendered glob list, on the
// same argument diff.MaxElidedFilesListed is bounded on: a PR touching two
// hundred distinct extensions would otherwise put the whole set into a log
// line, in the shape most likely to be the attacker-controlled one.
const (
	maxLoggedGlobs   = 10
	maxLoggedGlobLen = 120
)

// safeGlobList renders derived globs as an inert label for the daemon log.
//
// A glob's extension comes from a filename `gh api .../pulls/N/files`
// reported, which is a string the author of that pull request chose — and on
// an ext-* PR that author is an external contributor. filepath.Ext returns
// everything after the LAST dot, so it stops nothing: a file named
// "a/b.go\n[smelter] forged line" yields the glob
// "**/*.go\n[smelter] forged line", which written straight into log.Printf is
// a line of the operator's daemon.log that Forge did not write, and an ANSI
// escape in the same position is a terminal injection when the daemon runs in
// the foreground.
//
// So the alphabet is closed rather than the dangerous bytes blocked, exactly
// as diff.SafePath argues: letters, digits, '.', '_', '-', '/' and '*'
// survive, every run of anything else collapses to a single "?", and a name
// that was scrubbed reads as scrubbed. diff.SafePath itself cannot be used
// here — '*' is not in its alphabet, so it renders every glob this package
// produces as "?/?.go" — and the shared half, the closed-alphabet argument,
// is the comment above rather than a call.
func safeGlobList(globs []string) string {
	extra := 0
	if len(globs) > maxLoggedGlobs {
		extra = len(globs) - maxLoggedGlobs
		globs = globs[:maxLoggedGlobs]
	}
	out := make([]string, 0, len(globs))
	for _, g := range globs {
		out = append(out, safeGlob(g))
	}
	list := strings.Join(out, ", ")
	if extra > 0 {
		list += fmt.Sprintf(", and %d more", extra)
	}
	return list
}

func safeGlob(glob string) string {
	var b strings.Builder
	dropped := false
	for _, r := range glob {
		if !safeGlobRune(r) {
			dropped = true
			continue
		}
		if dropped {
			b.WriteByte('?')
			dropped = false
		}
		b.WriteRune(r)
	}
	if dropped {
		b.WriteByte('?')
	}
	// Every rune kept above is one ASCII byte, so this cut cannot split one.
	out := b.String()
	if len(out) > maxLoggedGlobLen {
		out = out[:maxLoggedGlobLen] + "..."
	}
	return out
}

func safeGlobRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-', r == '/', r == '*':
		return true
	}
	return false
}
