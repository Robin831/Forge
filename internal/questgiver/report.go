package questgiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/textfmt"
)

// previewQuestMarkerPrefix opens the hidden HTML comment that identifies the
// preview quest comment on a pull request. The key that follows is the head the
// quests ran against, which is what makes reporting idempotent: a re-run of the
// same commit finds its own comment and edits it, while a new head has no
// matching marker and therefore gets a fresh comment. It mirrors the shape of
// Assay's inline hash marker (`<!-- assay-hash: … -->`) on purpose — same
// mechanism, different key.
const previewQuestMarkerPrefix = "forge-preview-quest:"

// maxCellLen bounds a single markdown table cell. Quest error messages can carry
// a whole browser stack trace, and a PR comment is a summary, not a log.
const maxCellLen = 300

// maxCommentBytes bounds the rendered comment body. GitHub rejects issue
// comments over 65536 characters, so a run with many failing quests is truncated
// rather than lost to a 422.
const maxCommentBytes = 60000

// ghCommentExec runs a gh CLI invocation in dir and returns its stdout. It is
// the seam the reporter uses to reach the gh CLI; tests replace it so no real
// process is spawned and so the exact argv can be asserted on.
type ghCommentExec func(ctx context.Context, dir string, args ...string) ([]byte, error)

// ScreenshotUploader publishes one locally captured screenshot and returns a URL
// that renders inside a PR comment. It is optional: with no uploader wired (the
// daemon's current state — Forge hosts no artifact store), screenshots are
// listed by path instead of embedded, which is the same graceful degradation an
// individual upload failure produces.
type ScreenshotUploader func(ctx context.Context, path string) (string, error)

// ScreenshotRef is one screenshot as it will appear in the rendered comment.
// URL is empty when the screenshot could not be published, in which case Err
// carries the reason (empty when no uploader was configured at all) and the
// comment falls back to naming the path.
type ScreenshotRef struct {
	// Quest is the quest that captured the screenshot.
	Quest string
	// Name is the display label (the file's base name).
	Name string
	// Path is the local filesystem path the run captured.
	Path string
	// URL is the published location, or "" when publishing did not happen.
	URL string
	// Err explains a failed upload; "" when there was nothing to upload with.
	Err string
}

// ReportRequest carries everything the reporter needs to publish one run.
type ReportRequest struct {
	// Anvil and BeadID are log context only.
	Anvil  string
	BeadID string
	// PRNumber is the pull request to comment on.
	PRNumber int
	// HeadSHA is the commit the quests exercised. It keys the comment marker;
	// when empty the run's preview id is used instead so distinct previews still
	// get distinct comments.
	HeadSHA string
	// WorktreePath is the directory gh runs in. gh resolves the {owner}/{repo}
	// API placeholders from this checkout, so it must be inside the repository.
	WorktreePath string
	// Result is the run to report. A nil result is an error; a skipped run is
	// not reported at all (nothing was exercised, so there is nothing to say).
	Result *QuestRunResult
}

// ReportResult summarizes what the reporter did.
type ReportResult struct {
	// Marker is the hidden key the comment was written under.
	Marker string
	// CommentID is the GitHub comment that now holds the report.
	CommentID int64
	// Created and Updated are mutually exclusive; both false means nothing was
	// posted (a skipped run).
	Created bool
	Updated bool
	// ScreenshotsUploaded / ScreenshotsFailed count the publish attempts. Both
	// stay zero when no uploader is configured.
	ScreenshotsUploaded int
	ScreenshotsFailed   int
}

// Reporter posts a preview quest run's outcome to a pull request as a single
// comment, keyed by a hidden marker containing the head SHA so repeated runs of
// the same commit edit that comment instead of piling up duplicates.
//
// It deliberately creates NO check run and NO commit status. Preview quest
// results are informational: they describe a throwaway environment at a moment
// in time, and a red check would block a merge on evidence nothing downstream is
// allowed to reason about. The only artifact this type produces is a comment,
// and its error return exists to be logged by the caller — never to fail a
// pipeline.
//
// Build one with NewReporter; the zero value is not usable.
type Reporter struct {
	gh     ghCommentExec
	upload ScreenshotUploader
	logf   func(format string, args ...any)
}

// NewReporter builds a Reporter wired to the real gh CLI. upload may be nil, in
// which case screenshots are reported by path rather than embedded.
func NewReporter(upload ScreenshotUploader) *Reporter {
	return &Reporter{
		gh:     defaultGhCommentExec,
		upload: upload,
		logf:   log.Printf,
	}
}

// ReportPreviewQuestResults upserts the PR comment for req.Result.
//
// The flow is: publish whatever screenshots there are (best effort), render the
// comment body under the head-SHA marker, look for an existing comment carrying
// that marker, and PATCH it if found or POST a new one if not. A run whose
// screenshots all fail to upload still produces a comment — the pass/fail table
// is the point, the images are evidence.
//
// A skipped run (a gate said no, so no quest ran) is not reported: it would say
// nothing about the branch. That returns a zero result and a nil error.
func (r *Reporter) ReportPreviewQuestResults(ctx context.Context, req ReportRequest) (*ReportResult, error) {
	if req.Result == nil {
		return nil, errors.New("questgiver: cannot report a nil quest run result")
	}
	if req.PRNumber <= 0 {
		return nil, fmt.Errorf("questgiver: cannot report quest run for bead %q without a PR number", req.BeadID)
	}
	if req.Result.Skipped {
		return &ReportResult{}, nil
	}

	key := reportKey(req.HeadSHA, req.Result)
	marker := previewQuestMarker(key)
	res := &ReportResult{Marker: marker}

	shots := r.uploadScreenshots(ctx, req.Result)
	for _, s := range shots {
		switch {
		case s.URL != "":
			res.ScreenshotsUploaded++
		case s.Err != "":
			res.ScreenshotsFailed++
		}
	}

	body := formatPreviewQuestComment(marker, req.HeadSHA, req.Result, shots)

	existing, err := r.findComment(ctx, req, marker)
	if err != nil {
		// A failed lookup must not silently duplicate: without knowing whether a
		// comment already exists, posting a new one would break the very
		// idempotency this reporter exists for.
		return res, fmt.Errorf("questgiver: listing PR #%d comments: %w", req.PRNumber, err)
	}

	if existing != 0 {
		if err := r.editComment(ctx, req, existing, body); err != nil {
			return res, fmt.Errorf("questgiver: editing quest comment %d on PR #%d: %w", existing, req.PRNumber, err)
		}
		res.CommentID = existing
		res.Updated = true
		return res, nil
	}

	id, err := r.createComment(ctx, req, body)
	if err != nil {
		return res, fmt.Errorf("questgiver: posting quest comment on PR #%d: %w", req.PRNumber, err)
	}
	res.CommentID = id
	res.Created = true
	return res, nil
}

// uploadScreenshots publishes every screenshot the run captured, in quest and
// step order. Failures degrade to a path-only entry rather than aborting the
// report: an unpublished image is worth less than the table it accompanies, and
// losing the table because an upload 500'd would be the wrong trade.
func (r *Reporter) uploadScreenshots(ctx context.Context, res *QuestRunResult) []ScreenshotRef {
	var refs []ScreenshotRef
	for _, q := range res.Quests {
		for _, path := range q.Screenshots {
			ref := ScreenshotRef{Quest: q.Name, Name: filepath.Base(path), Path: path}
			if r.upload != nil {
				url, err := r.upload(ctx, path)
				switch {
				case err != nil:
					ref.Err = err.Error()
					r.logf("questgiver: uploading screenshot %s: %v", path, err)
				case strings.TrimSpace(url) == "":
					ref.Err = "uploader returned no URL"
				default:
					ref.URL = strings.TrimSpace(url)
				}
			}
			refs = append(refs, ref)
		}
	}
	return refs
}

// findComment returns the id of the PR comment carrying marker, or 0 when there
// is none. Pagination is left to gh (`--paginate` merges the JSON arrays), so a
// PR with hundreds of comments still resolves.
func (r *Reporter) findComment(ctx context.Context, req ReportRequest, marker string) (int64, error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments?per_page=100", req.PRNumber)
	out, err := r.gh(ctx, req.WorktreePath, "api", "--paginate", endpoint)
	if err != nil {
		return 0, err
	}
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &comments); err != nil {
		return 0, fmt.Errorf("parsing comment list: %w", err)
	}
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return c.ID, nil
		}
	}
	return 0, nil
}

// createComment posts a new issue comment on the PR and returns its id.
func (r *Reporter) createComment(ctx context.Context, req ReportRequest, body string) (int64, error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", req.PRNumber)
	out, err := r.gh(ctx, req.WorktreePath,
		"api", "--method", "POST", endpoint, "-f", "body="+body)
	if err != nil {
		return 0, err
	}
	return commentID(out)
}

// editComment rewrites an existing issue comment in place.
func (r *Reporter) editComment(ctx context.Context, req ReportRequest, id int64, body string) error {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d", id)
	_, err := r.gh(ctx, req.WorktreePath,
		"api", "--method", "PATCH", endpoint, "-f", "body="+body)
	return err
}

// commentID pulls the id out of a comment create response.
func commentID(out []byte) (int64, error) {
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, fmt.Errorf("parsing comment response: %w", err)
	}
	return resp.ID, nil
}

// previewQuestMarker returns the hidden marker line for a report key, e.g.
// "<!-- forge-preview-quest: deadbeef -->".
func previewQuestMarker(key string) string {
	return fmt.Sprintf("<!-- %s %s -->", previewQuestMarkerPrefix, key)
}

// reportKey picks what the comment is keyed on. The head SHA is the intended
// key — it is what "same code, same comment" means. A run whose head could not
// be resolved falls back to the preview id so two previews of the same anvil do
// not collapse onto one comment, and a run with neither is keyed "unknown",
// which at least keeps repeated runs from duplicating.
func reportKey(headSHA string, res *QuestRunResult) string {
	if s := strings.TrimSpace(headSHA); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.HeadSHA); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.PreviewID); s != "" {
		return "preview-" + s
	}
	return "unknown"
}

// formatPreviewQuestComment renders the comment body: the hidden marker, a
// headline counting passes and failures, a table with one row per quest, an
// optional screenshot section, and a closing note that the result is
// informational. The marker is written first so it survives any truncation of
// the tail — a body that lost its marker would be re-created rather than edited
// on the next run.
func formatPreviewQuestComment(marker, headSHA string, res *QuestRunResult, shots []ScreenshotRef) string {
	passed, failed := 0, 0
	for _, q := range res.Quests {
		if q.Passed {
			passed++
		} else {
			failed++
		}
	}

	var b strings.Builder
	b.WriteString(marker)
	b.WriteString("\n### ")
	b.WriteString(questHeadline(passed, failed))
	b.WriteString("\n\n")
	b.WriteString(questContextLine(headSHA, res))
	b.WriteString("\n\n")

	if len(res.Quests) > 0 {
		b.WriteString("| Quest | Result | Duration | Detail |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, q := range res.Quests {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				cell(questLabel(q)), questVerdict(q), formatQuestDuration(q.Duration), cell(questDetail(q)))
		}
		b.WriteByte('\n')
	}

	b.WriteString(screenshotSection(shots))

	// Deliberately no check run and no commit status is created anywhere in this
	// package — see the Reporter doc comment. The note below is the user-facing
	// half of that promise.
	b.WriteString("> Informational only: preview quest results do not gate this pull request. " +
		"No check run or commit status is created from them, and nothing in the pipeline, " +
		"Bellows or the merge path reads them.\n")

	return truncateComment(b.String())
}

// questHeadline is the one-line verdict above the table.
func questHeadline(passed, failed int) string {
	switch {
	case passed+failed == 0:
		return "Preview E2E quests — no quests ran"
	case failed == 0:
		return fmt.Sprintf("Preview E2E quests — %s passed", textfmt.Count(passed, "quest"))
	case passed == 0:
		return fmt.Sprintf("Preview E2E quests — %s failed", textfmt.Count(failed, "quest"))
	default:
		return fmt.Sprintf("Preview E2E quests — %d passed, %d failed", passed, failed)
	}
}

// questContextLine names what was exercised: the preview, the head and the base
// URL the browser was pointed at.
func questContextLine(headSHA string, res *QuestRunResult) string {
	parts := []string{"Ran against a Kiln preview environment"}
	if s := strings.TrimSpace(res.PreviewID); s != "" {
		parts = append(parts, fmt.Sprintf("preview `%s`", s))
	}
	sha := strings.TrimSpace(headSHA)
	if sha == "" {
		sha = strings.TrimSpace(res.HeadSHA)
	}
	if sha != "" {
		parts = append(parts, fmt.Sprintf("commit `%s`", shortSHA(sha)))
	}
	if s := strings.TrimSpace(res.BaseURL); s != "" {
		parts = append(parts, fmt.Sprintf("base URL `%s`", s))
	}
	line := strings.Join(parts, ", ") + "."
	if res.Duration > 0 {
		line += fmt.Sprintf(" Total run time %s.", formatQuestDuration(res.Duration))
	}
	return line
}

// questLabel is the table's first column: the quest name, falling back to its
// file when a quest declares no name.
func questLabel(q QuestOutcome) string {
	if s := strings.TrimSpace(q.Name); s != "" {
		return s
	}
	if s := strings.TrimSpace(q.FilePath); s != "" {
		return filepath.Base(s)
	}
	return "(unnamed quest)"
}

// questVerdict renders the pass/fail cell. The emoji is the scannable part; the
// word is what a screen reader (and a plain-text diff of the comment) reads.
func questVerdict(q QuestOutcome) string {
	if q.Passed {
		return "✅ pass"
	}
	return "❌ fail"
}

// questDetail renders the last column: where a failing quest stopped and why.
func questDetail(q QuestOutcome) string {
	if q.Passed {
		return ""
	}
	msg := strings.TrimSpace(q.ErrorMessage)
	if q.FailedStep >= 0 {
		if msg == "" {
			return fmt.Sprintf("failed at step %d", q.FailedStep)
		}
		return fmt.Sprintf("step %d: %s", q.FailedStep, msg)
	}
	if msg == "" {
		return "failed"
	}
	return msg
}

// screenshotSection renders the captured screenshots grouped by quest. Published
// screenshots embed; unpublished ones name their path (with the upload error
// when there was one) so the evidence is still findable on the daemon host.
// Returns "" when the run captured none.
func screenshotSection(shots []ScreenshotRef) string {
	if len(shots) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("#### Screenshots\n\n")
	current := ""
	for _, s := range shots {
		if s.Quest != current {
			current = s.Quest
			fmt.Fprintf(&b, "**%s**\n\n", strings.TrimSpace(current))
		}
		switch {
		case s.URL != "":
			fmt.Fprintf(&b, "- [%s](%s)\n\n  ![%s](%s)\n", s.Name, s.URL, s.Name, s.URL)
		case s.Err != "":
			fmt.Fprintf(&b, "- `%s` — could not be uploaded: %s\n", s.Path, oneLine(s.Err))
		default:
			fmt.Fprintf(&b, "- `%s` (on the Forge host)\n", s.Path)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// cell makes an arbitrary string safe inside a markdown table cell: pipes are
// escaped, newlines become <br>, and the whole thing is bounded.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = oneLine(s)
	return truncate(s, maxCellLen)
}

// oneLine collapses newlines into <br> so a multi-line error does not break the
// table it sits in.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "<br>")
}

// truncate bounds s to max characters (not bytes), appending an ellipsis when
// it had to cut.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// truncateComment bounds the whole body so GitHub's 65536-character comment
// limit cannot turn a noisy run into a failed report.
func truncateComment(s string) string {
	if len(s) <= maxCommentBytes {
		return s
	}
	cut := s[:maxCommentBytes]
	// Back off to a rune boundary so the cut never emits a broken code point.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n\n_(report truncated)_\n"
}

// formatQuestDuration renders a duration at a resolution a human cares about
// for a browser run: tenths of a second.
func formatQuestDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// shortSHA abbreviates a commit for display, leaving anything already short
// (or not a SHA at all) alone.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// defaultGhCommentExec is the production ghCommentExec: it shells out to gh in
// dir. Both streams are folded into the error because gh writes the HTTP summary
// to stderr and the JSON error body to stdout.
func defaultGhCommentExec(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", args...))
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
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
