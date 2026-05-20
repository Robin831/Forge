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
// otherwise.
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
		seen["**/*"+ext] = struct{}{}
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

// runPathsBackfill iterates the active rules in rf and, for each rule whose
// Paths field is empty and whose Source carries one or more copilot:PR#N
// tokens, fetches the changed files for those PRs and populates Paths with
// the derived file-extension globs.
//
// Idempotency: rules whose Paths is already populated are skipped, so a
// repeated flush is a no-op for the same rule.
//
// Best-effort: a fetch failure for a single PR is logged and the next PR is
// tried. A rule is only modified when at least one fetch succeeded and
// produced at least one glob.
//
// Returns the number of rules whose Paths was populated.
func (s *Smelter) runPathsBackfill(ctx context.Context, wtPath, anvilName string, rf *warden.RulesFile) int {
	var updated int
	for i := range rf.Rules {
		if ctx.Err() != nil {
			return updated
		}
		rule := &rf.Rules[i]
		if len(rule.Paths) > 0 {
			continue
		}

		// Collect unique PR numbers referenced by this rule's sources.
		var prNums []int
		seenPR := make(map[int]struct{})
		for _, src := range rule.Source {
			n, ok := extractPRNumber(src)
			if !ok {
				continue
			}
			if _, dup := seenPR[n]; dup {
				continue
			}
			seenPR[n] = struct{}{}
			prNums = append(prNums, n)
		}
		if len(prNums) == 0 {
			continue
		}

		var allFiles []string
		var anySuccess bool
		for _, prNum := range prNums {
			files, err := fetchChangedFiles(ctx, wtPath, prNum)
			if err != nil {
				log.Printf("[smelter] paths backfill: PR#%d for rule %s on %s: %v", prNum, rule.ID, anvilName, err)
				continue
			}
			anySuccess = true
			allFiles = append(allFiles, files...)
		}
		if !anySuccess {
			continue
		}

		globs := globsFromExtensions(allFiles)
		if len(globs) == 0 {
			continue
		}
		rule.Paths = globs
		updated++
	}
	return updated
}
