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
	"path/filepath"
	"regexp"
	"sort"
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

// extGlobPrefix is the shape globsFromExtensions emits: one doublestar glob
// per file extension. It is a constant rather than a literal inside the loop
// below because rulelang.go classifies an inferred glob by testing for it —
// a glob carrying it names an extension and can be compared against the
// PR-derived set, anything else (a directory glob like changelog.d/**) cannot.
//
// What the constant buys is that the emitted shape and the shape the
// classifier tests for are one string: the intersection rulelang.go takes is
// only meaningful while an inferred **/*.go and a PR-derived **/*.go are
// byte-identical, and written twice they could drift apart. It does not make
// the classification itself safe from a change of shape — the globs actually
// tested against it are the hand-written literals in languageSignals, so
// changing this constant would silently move every one of them into the
// directory branch without failing to compile. TestGlobsForRule is what
// catches that, not the compiler.
const extGlobPrefix = "**/*."

// globsFromExtensions returns the unique doublestar globs derived from the
// extensions of files. Files without an extension are skipped. The result is
// sorted so the field encoded into warden-rules.yaml is deterministic across
// runs — this keeps the smelter PR's diff stable when nothing material has
// changed.
func globsFromExtensions(files []string) []string {
	seen := make(map[string]struct{})
	for _, f := range files {
		ext := filepath.Ext(f)
		if ext == "" || ext == "." {
			continue
		}
		seen[extGlobPrefix+strings.TrimPrefix(ext, ".")] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	globs := make([]string, 0, len(seen))
	for g := range seen {
		globs = append(globs, g)
	}
	sort.Strings(globs)
	return globs
}

// prFetchResult caches the outcome of a single gh API call for one PR.
type prFetchResult struct {
	files []string
	err   error
}

// runPathsBackfill iterates the active rules in rf and, for each rule whose
// Paths field is empty and whose Source carries one or more copilot:PR#N
// tokens, fetches the changed files for those PRs and populates Paths with
// the globs globsForRule derives from those files and the rule's own text.
//
// Idempotency: rules whose Paths is already populated are skipped, so a
// repeated flush is a no-op for the same rule.
//
// Best-effort: a fetch failure for a single PR is logged and the next PR is
// tried. A rule is only modified when at least one fetch succeeded and
// produced at least one glob.
//
// Returns the IDs of rules whose Paths was populated, in the order they were
// visited. The caller can use len() to obtain the count for logging or the
// list itself to render the "Backfilled:" section of the smelter commit.
func (s *Smelter) runPathsBackfill(ctx context.Context, wtPath, anvilName string, rf *warden.RulesFile) []string {
	return pathsBackfill(ctx, wtPath, anvilName, rf)
}

// pathsBackfill is the free-function form of runPathsBackfill. It carries no
// dependency on Smelter state so the off-cycle CLI consolidate command can
// share the same Pass 3 implementation as the scheduled smelter loop.
func pathsBackfill(ctx context.Context, wtPath, anvilName string, rf *warden.RulesFile) []string {
	// Cache fetched file lists per PR number so that multiple rules referencing
	// the same PR number do not trigger redundant gh API calls.
	prCache := make(map[int]prFetchResult)

	var updated []string
	for i := range rf.Rules {
		if ctx.Err() != nil {
			return updated
		}
		rule := &rf.Rules[i]
		if len(rule.Paths) > 0 {
			continue
		}

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
			continue
		}

		var allFiles []string
		var anySuccess bool
		for _, prNum := range prNums {
			result, cached := prCache[prNum]
			if !cached {
				files, err := fetchChangedFiles(ctx, wtPath, prNum)
				result = prFetchResult{files: files, err: err}
				prCache[prNum] = result
			}
			if result.err != nil {
				log.Printf("[smelter] paths backfill: PR#%d for rule %s on %s: %v", prNum, rule.ID, anvilName, result.err)
				continue
			}
			anySuccess = true
			allFiles = append(allFiles, result.files...)
		}
		if !anySuccess {
			continue
		}

		// Derived from the rule's own text as well as the PR's files: the PR
		// says which extensions were touched, the rule says which language it
		// is about, and only the intersection is a path filter that narrows
		// anything. See globsForRule.
		globs := globsForRule(*rule, allFiles)
		if len(globs) == 0 {
			continue
		}
		rule.Paths = globs
		updated = append(updated, rule.ID)
		// Name the languages the rule's own text was read as naming AND what
		// became of each: the globs alone cannot say whether a rule was
		// narrowed by its language or fell back to the PR's extensions, which
		// is the one thing worth knowing when a backfilled rule stops firing.
		// The outcome is read off the globs the rule now carries, so a
		// discarded inference reads as discarded rather than as the narrowing
		// that did not happen. The globs themselves are PR-derived and so go
		// out through safeGlobList.
		langs := languageOutcomes(ruleText(*rule), globs)
		if len(langs) == 0 {
			langs = []string{"none"}
		}
		log.Printf("[smelter] paths backfill: rule %s on %s -> %s (languages: %s)",
			rule.ID, anvilName, safeGlobList(globs), strings.Join(langs, ", "))
	}
	return updated
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
