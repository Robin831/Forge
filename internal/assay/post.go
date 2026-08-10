package assay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// hashMarkerPrefix opens every inline comment body. The hex hash that follows
// is the finding's stable identity (independent of head SHA), so a finding maps
// to the same marker across re-reviews. The marker is what the consecutive-miss
// resolver matches review threads against.
const hashMarkerPrefix = "assay-hash:"

// resolveMissThreshold is the number of consecutive reviews a previously-posted
// finding must go undetected before its review thread is auto-resolved.
const resolveMissThreshold = 2

// ghExec runs a gh CLI invocation in dir and returns its stdout. It is the seam
// the posting layer uses to reach the gh CLI; tests replace it with a stub so
// no real process is spawned. The default implementation shells out to gh.
type ghExec func(ctx context.Context, dir string, args ...string) ([]byte, error)

// ThreadResolver locates and resolves review threads on a PR. It is satisfied
// by *github.Provider (see ThreadIDByBodyHeader / ResolveThread); the posting
// layer depends on the interface, not the concrete provider, so it stays
// testable and free of an import cycle.
type ThreadResolver interface {
	// ThreadIDByBodyHeader returns the platform thread ID whose top comment
	// body contains header, or "" when none matches.
	ThreadIDByBodyHeader(ctx context.Context, worktreePath string, prNumber int, header string) (string, error)
	// ResolveThread marks the identified review thread as resolved.
	ResolveThread(ctx context.Context, worktreePath string, threadID string) error
}

// PostRequest carries everything the posting layer needs to publish a review.
type PostRequest struct {
	// Anvil is the repository (anvil) name; keys the persisted rows.
	Anvil string
	// PRNumber is the pull request to comment on.
	PRNumber int
	// HeadSHA is the commit the inline comments are anchored to (commit_id).
	HeadSHA string
	// WorktreePath is the directory gh commands run in (the PR worktree). gh
	// resolves the {owner}/{repo} API placeholders from this checkout.
	WorktreePath string
	// SummaryLine is the one-line verdict shown above the severity table in the
	// top-level review comment. When empty (and there are no findings) the
	// summary review is skipped.
	SummaryLine string
	// FailedPasses names the deep passes that did not review this head. When
	// non-empty the summary review carries an explicit partial-coverage line,
	// so a short findings list is never read as a clean review of the whole
	// diff. Read from the run record, so the comment and Hearth agree.
	FailedPasses []PassFailure
	// Findings is the aggregated set to post (already deduped/suppressed/capped
	// by Review).
	Findings []Finding
	// Diff is the PR's unified diff. When non-empty it is used to keep inline
	// comments to positions that actually exist in the diff (GitHub 422s
	// otherwise); findings outside the diff are listed in the summary instead.
	// When empty, all findings are attempted inline (legacy behaviour).
	Diff string
}

// PostResult summarizes a posting run.
type PostResult struct {
	// SummaryPosted reports whether the top-level review comment was posted.
	SummaryPosted bool
	// Posted is the number of inline comments successfully posted.
	Posted int
	// Failed is the number of inline comments whose gh call failed (left with
	// posted=0 so they retry on the next head).
	Failed int
	// Resolved is the number of stale findings whose threads were resolved
	// after crossing the consecutive-miss threshold.
	Resolved int
	// OutOfDiff is the number of findings that could not be anchored to the PR
	// diff and were listed in the summary instead of posted inline.
	OutOfDiff int
}

// Poster publishes Assay findings to a GitHub PR: a top-level summary review,
// one inline comment per finding, and auto-resolution of threads for findings
// that have disappeared across consecutive reviews. Build one with NewPoster.
//
// The zero value is not usable; gh and logf are populated by NewPoster. db and
// resolver may be nil — a nil db skips all persistence (so nothing is recorded
// or auto-resolved), a nil resolver skips thread resolution only.
type Poster struct {
	gh       ghExec
	db       *state.DB
	resolver ThreadResolver
	logf     func(format string, args ...any)
}

// NewPoster builds a Poster wired to the real gh CLI. db persists posting state
// (gh_comment_id / gh_thread_id / consecutive_misses); resolver performs thread
// lookup and resolution. Either may be nil.
func NewPoster(db *state.DB, resolver ThreadResolver) *Poster {
	return &Poster{
		gh:       defaultGhExec,
		db:       db,
		resolver: resolver,
		logf:     log.Printf,
	}
}

// Post publishes req's findings to the PR. When cfg.ShadowMode is true it is a
// no-op (the engine must not produce public side effects in shadow mode) and
// returns a zero result.
//
// Posting never aborts on a single gh failure: each inline comment is attempted
// independently, failures are logged, and the corresponding finding keeps
// posted=0 so it is retried on the next head SHA. The only returned error is a
// fatal one that prevents the whole run (currently none — Post always returns a
// result and nil unless a future fatal path is added).
func (p *Poster) Post(ctx context.Context, cfg Config, req PostRequest) (*PostResult, error) {
	res := &PostResult{}
	if cfg.ShadowMode {
		return res, nil
	}

	// Partition findings into those anchorable to the PR diff (postable inline)
	// and those that are not (cross-file notes, wrong path, file-level/line=1).
	// GitHub 422s any inline comment whose (path, line) is not in the diff, so
	// out-of-diff findings are surfaced in the summary instead of silently lost.
	// When no diff is supplied, fall back to attempting every finding inline.
	idx := buildDiffIndex(req.Diff)
	var outOfDiff []Finding
	inlineEligible := req.Findings
	if len(idx) > 0 {
		inlineEligible = inlineEligible[:0:0]
		for _, f := range req.Findings {
			if anchorableInDiff(idx, f) {
				inlineEligible = append(inlineEligible, f)
			} else {
				outOfDiff = append(outOfDiff, f)
			}
		}
	}
	res.OutOfDiff = len(outOfDiff)

	// 1. Top-level summary review: severity table over all findings, plus the
	// full detail of any out-of-diff findings so their content is never lost.
	// A run with missing passes always posts a summary, even with no findings
	// and no summary line: "nothing to report" from a review that only half
	// ran is exactly the impression the coverage line exists to prevent.
	if req.SummaryLine != "" || len(req.Findings) > 0 || len(req.FailedPasses) > 0 {
		body := buildSummaryBody(req.SummaryLine, req.FailedPasses, req.Findings) + buildOutOfDiffSection(outOfDiff)
		if _, err := p.gh(ctx, req.WorktreePath,
			"pr", "review", strconv.Itoa(req.PRNumber), "--comment", "--body", body,
		); err != nil {
			p.logf("assay: posting summary review for PR #%d: %v", req.PRNumber, err)
		} else {
			res.SummaryPosted = true
		}
	}

	// Findings already posted (and still open) on this PR. Used to avoid
	// double-posting and to detect consecutive misses.
	var postedHashes map[string]bool
	var openPosted []state.Finding
	if p.db != nil {
		var err error
		openPosted, err = p.db.OpenPostedFindings(req.Anvil, req.PRNumber)
		if err != nil {
			p.logf("assay: loading posted findings for PR #%d: %v", req.PRNumber, err)
		}
		postedHashes = make(map[string]bool, len(openPosted))
		for _, f := range openPosted {
			postedHashes[f.FindingHash] = true
		}
	}

	// 2. Inline comments — one per anchorable finding. `current` tracks ALL
	// findings (including out-of-diff ones) so resolveMisses does not treat a
	// finding we deliberately routed to the summary as a disappeared one.
	current := make(map[string]bool, len(req.Findings))
	for _, f := range req.Findings {
		current[f.Hash] = true
	}
	for _, f := range inlineEligible {
		if postedHashes[f.Hash] {
			// Already posted on a prior head; it is re-detected, so clear any
			// accumulated misses and skip re-posting.
			if p.db != nil {
				if err := p.db.ResetConsecutiveMiss(f.Hash); err != nil {
					p.logf("assay: resetting misses for %s: %v", f.Hash, err)
				}
			}
			continue
		}

		commentID, err := p.postInline(ctx, req, f)
		if err != nil {
			// Leave posted=0 so this finding retries on the next SHA.
			p.logf("assay: posting inline comment for %s (%s): %v", f.File, f.Hash, err)
			res.Failed++
			continue
		}
		res.Posted++
		if p.db != nil {
			if err := p.db.MarkFindingPosted(f.Hash, commentID); err != nil {
				p.logf("assay: marking %s posted: %v", f.Hash, err)
			}
		}
	}

	// 3. Consecutive-miss thread resolution. A finding posted earlier but not
	// re-detected this round is a miss; on the threshold we resolve its thread.
	res.Resolved = p.resolveMisses(ctx, req, openPosted, current)

	return res, nil
}

// postInline posts a single inline review comment for f and returns the new
// GitHub comment ID. The body opens with the hash marker; line anchoring uses
// start_line/line for a multi-line anchor and line alone otherwise.
func (p *Poster) postInline(ctx context.Context, req PostRequest, f Finding) (int64, error) {
	start, end, ok := parseLineSpec(f.Anchor)
	if !ok {
		return 0, fmt.Errorf("anchor %q has no parseable line", f.Anchor)
	}

	endpoint := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", req.PRNumber)
	args := []string{
		"api", "--method", "POST", endpoint,
		"-f", "body=" + buildInlineBody(f),
		"-f", "commit_id=" + req.HeadSHA,
		"-f", "path=" + f.File,
	}
	if start > 0 && start < end {
		// Multi-line range comment.
		args = append(args,
			"-F", "start_line="+strconv.Itoa(start),
			"-f", "start_side=RIGHT",
			"-F", "line="+strconv.Itoa(end),
			"-f", "side=RIGHT",
		)
	} else {
		args = append(args,
			"-F", "line="+strconv.Itoa(end),
			"-f", "side=RIGHT",
		)
	}

	out, err := p.gh(ctx, req.WorktreePath, args...)
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, fmt.Errorf("parsing comment response: %w", err)
	}
	return resp.ID, nil
}

// resolveMisses bumps the consecutive-miss counter for every previously-posted
// finding absent from current, and resolves the thread for any that reaches the
// threshold. Returns the number of threads resolved.
func (p *Poster) resolveMisses(ctx context.Context, req PostRequest, openPosted []state.Finding, current map[string]bool) int {
	if p.db == nil {
		return 0
	}
	resolved := 0
	for _, f := range openPosted {
		if current[f.FindingHash] {
			continue // re-detected — handled in the inline loop
		}
		if err := p.db.IncrementConsecutiveMiss(f.FindingHash); err != nil {
			p.logf("assay: incrementing misses for %s: %v", f.FindingHash, err)
			continue
		}
		if f.ConsecutiveMisses+1 < resolveMissThreshold {
			continue
		}
		if p.resolveThread(ctx, req, f) {
			resolved++
		}
	}
	return resolved
}

// resolveThread locates the review thread for f by its hash marker, persists
// the matched thread ID, resolves it, and marks the finding resolved. It
// returns true only when the thread was resolved.
func (p *Poster) resolveThread(ctx context.Context, req PostRequest, f state.Finding) bool {
	if p.resolver == nil {
		return false
	}
	threadID := f.GHThreadID
	if threadID == "" {
		header := markerFor(f.FindingHash)
		id, err := p.resolver.ThreadIDByBodyHeader(ctx, req.WorktreePath, req.PRNumber, header)
		if err != nil {
			p.logf("assay: looking up thread for %s: %v", f.FindingHash, err)
			return false
		}
		if id == "" {
			return false // thread not found yet; try again next round
		}
		threadID = id
		if err := p.db.SetFindingThreadID(f.FindingHash, threadID); err != nil {
			p.logf("assay: persisting thread id for %s: %v", f.FindingHash, err)
		}
	}
	if err := p.resolver.ResolveThread(ctx, req.WorktreePath, threadID); err != nil {
		p.logf("assay: resolving thread %s for %s: %v", threadID, f.FindingHash, err)
		return false
	}
	if err := p.db.MarkResolved(f.FindingHash); err != nil {
		p.logf("assay: marking %s resolved: %v", f.FindingHash, err)
	}
	return true
}

// buildSummaryBody renders the top-level review body: the summary line, the
// partial-coverage caveat when passes are missing, then a markdown severity
// table aggregated from the findings. Severities are rendered in a fixed order
// (Important, Nit, PreExisting); zero-count rows are omitted.
//
// The coverage line sits directly under the headline — above the table and
// every finding — because it qualifies all of them: a reader who sees it after
// the findings has already drawn the wrong conclusion.
func buildSummaryBody(summaryLine string, failedPasses []PassFailure, findings []Finding) string {
	counts := map[Severity]int{}
	for _, f := range findings {
		counts[f.Severity]++
	}

	var b strings.Builder
	if summaryLine != "" {
		b.WriteString(summaryLine)
		b.WriteString("\n\n")
	}
	if note := PartialCoverageNote(failedPasses); note != "" {
		b.WriteString(note)
		b.WriteString("\n\n")
	}
	b.WriteString("| Severity | Count |\n")
	b.WriteString("| --- | --- |\n")
	for _, sev := range []Severity{SeverityImportant, SeverityNit, SeverityPreExisting} {
		if n := counts[sev]; n > 0 {
			fmt.Fprintf(&b, "| %s | %d |\n", sev, n)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildOutOfDiffSection renders the findings that could not be anchored to the
// PR diff as a markdown list appended to the summary review, so their content
// (location, severity, title, body) is preserved even though no inline comment
// could be posted. Returns "" when there are none.
func buildOutOfDiffSection(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\n\n")
	b.WriteString("**Findings outside the diff** (not on changed lines, so not posted inline):\n\n")
	for _, f := range findings {
		loc := f.File
		if _, end, ok := parseLineSpec(f.Anchor); ok && end > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, end)
		}
		if f.Severity != "" {
			fmt.Fprintf(&b, "- **[%s] %s** — `%s`\n", f.Severity, f.Title, loc)
		} else {
			fmt.Fprintf(&b, "- **%s** — `%s`\n", f.Title, loc)
		}
		if f.Body != "" {
			for _, line := range strings.Split(strings.TrimRight(f.Body, "\n"), "\n") {
				b.WriteString("  ")
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildInlineBody renders a single inline comment body. It OPENS with the hash
// marker (an HTML comment, invisible in rendered markdown) so re-reviews can
// match the thread, followed by the severity-tagged title, the body, and any
// evidence as a fenced block.
func buildInlineBody(f Finding) string {
	var b strings.Builder
	b.WriteString(markerFor(f.Hash))
	b.WriteByte('\n')
	if f.Severity != "" {
		fmt.Fprintf(&b, "**[%s] %s**\n\n", f.Severity, f.Title)
	} else {
		fmt.Fprintf(&b, "**%s**\n\n", f.Title)
	}
	if f.Body != "" {
		b.WriteString(f.Body)
		b.WriteByte('\n')
	}
	if f.Evidence != "" {
		b.WriteString("\n```\n")
		b.WriteString(f.Evidence)
		b.WriteString("\n```\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// markerFor returns the hash marker line for a finding hash, e.g.
// "<!-- assay-hash: deadbeef -->".
func markerFor(hash string) string {
	return fmt.Sprintf("<!-- %s %s -->", hashMarkerPrefix, hash)
}

// parseLineSpec extracts the line anchor from a finding anchor of the form
// "path:line" or "path:start-end". It returns (start, end, true) for a range,
// (line, line, true) for a single line, and ok=false when no trailing line
// number is present. The path may itself contain colons; only the final
// colon-delimited segment is interpreted as the line spec.
func parseLineSpec(anchor string) (start, end int, ok bool) {
	idx := strings.LastIndex(anchor, ":")
	if idx < 0 {
		return 0, 0, false
	}
	spec := strings.TrimSpace(anchor[idx+1:])
	if spec == "" {
		return 0, 0, false
	}
	if dash := strings.Index(spec, "-"); dash >= 0 {
		s, errS := strconv.Atoi(strings.TrimSpace(spec[:dash]))
		e, errE := strconv.Atoi(strings.TrimSpace(spec[dash+1:]))
		if errS != nil || errE != nil {
			return 0, 0, false
		}
		if e < s {
			s, e = e, s
		}
		return s, e, true
	}
	n, err := strconv.Atoi(spec)
	if err != nil {
		return 0, 0, false
	}
	return n, n, true
}

// defaultGhExec is the production ghExec: it shells out to the gh CLI in dir.
func defaultGhExec(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// gh writes the HTTP summary ("Validation Failed (HTTP 422)") to stderr
		// but the JSON error body — which carries the field-level reason, e.g.
		// pull_request_review_thread.path could not be resolved — to stdout.
		// Include both so the real cause is logged, not just the 422 summary.
		msg := fmt.Sprintf("gh %s: %v", strings.Join(args, " "), err)
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += "\nstderr: " + s
		}
		if s := strings.TrimSpace(stdout.String()); s != "" {
			msg += "\nstdout: " + s
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return stdout.Bytes(), nil
}
