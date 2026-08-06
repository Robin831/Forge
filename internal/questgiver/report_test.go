package questgiver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubComment is one issue comment as the fake gh serves it.
type stubComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// fakeGH stands in for the gh CLI. It records every invocation verbatim — which
// is how the tests assert both what was called (create vs edit) and, just as
// importantly, what was never called (any check-run or commit-status endpoint).
type fakeGH struct {
	mu       sync.Mutex
	calls    [][]string
	comments []stubComment
	nextID   int64
	listErr  error
	writeErr error
}

func (f *fakeGH) exec(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))

	method, endpoint, body := parseGHArgs(args)
	switch method {
	case "GET":
		if f.listErr != nil {
			return nil, f.listErr
		}
		out, err := json.Marshal(f.comments)
		if err != nil {
			return nil, err
		}
		return out, nil
	case "POST":
		if f.writeErr != nil {
			return nil, f.writeErr
		}
		f.nextID++
		id := 1000 + f.nextID
		f.comments = append(f.comments, stubComment{ID: id, Body: body})
		return []byte(fmt.Sprintf(`{"id": %d}`, id)), nil
	case "PATCH":
		if f.writeErr != nil {
			return nil, f.writeErr
		}
		id := commentIDFromEndpoint(endpoint)
		for i := range f.comments {
			if f.comments[i].ID == id {
				f.comments[i].Body = body
				return []byte(fmt.Sprintf(`{"id": %d}`, id)), nil
			}
		}
		return nil, fmt.Errorf("no such comment %d", id)
	}
	return nil, fmt.Errorf("unexpected gh call: %v", args)
}

// countMethod returns how many recorded calls used the given HTTP method.
func (f *fakeGH) countMethod(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, args := range f.calls {
		if m, _, _ := parseGHArgs(args); m == method {
			n++
		}
	}
	return n
}

// endpoints returns every endpoint the fake was asked for, in order.
func (f *fakeGH) endpoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, args := range f.calls {
		_, ep, _ := parseGHArgs(args)
		out = append(out, ep)
	}
	return out
}

// parseGHArgs pulls the method, endpoint and body out of a `gh api` argv.
func parseGHArgs(args []string) (method, endpoint, body string) {
	method = "GET"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--method":
			if i+1 < len(args) {
				method = args[i+1]
				i++
			}
		case "-f", "-F":
			if i+1 < len(args) {
				if v, ok := strings.CutPrefix(args[i+1], "body="); ok {
					body = v
				}
				i++
			}
		case "api", "--paginate":
		default:
			if endpoint == "" && !strings.HasPrefix(args[i], "-") && args[i] != method {
				endpoint = args[i]
			}
		}
	}
	return method, endpoint, body
}

// commentIDFromEndpoint extracts the trailing id of an issues/comments/<id>
// endpoint.
func commentIDFromEndpoint(endpoint string) int64 {
	idx := strings.LastIndex(endpoint, "/")
	if idx < 0 {
		return 0
	}
	var id int64
	_, _ = fmt.Sscanf(endpoint[idx+1:], "%d", &id)
	return id
}

// newTestReporter builds a Reporter wired to fake and swallows its log output.
func newTestReporter(fake *fakeGH, upload ScreenshotUploader) *Reporter {
	return &Reporter{
		gh:     fake.exec,
		upload: upload,
		logf:   func(string, ...any) {},
	}
}

// passingRun is a two-quest, all-green run against the given head.
func passingRun(headSHA string) *QuestRunResult {
	return &QuestRunResult{
		Anvil:     "forge",
		PreviewID: "forge-juzy",
		HeadSHA:   headSHA,
		BaseURL:   "http://localhost:8123",
		Passed:    true,
		Duration:  9 * time.Second,
		Quests: []QuestOutcome{
			{Name: "login", Passed: true, FailedStep: -1, Duration: 3 * time.Second},
			{Name: "checkout", Passed: true, FailedStep: -1, Duration: 6 * time.Second},
		},
	}
}

// failingRun has one green and one red quest.
func failingRun(headSHA string) *QuestRunResult {
	return &QuestRunResult{
		Anvil:     "forge",
		PreviewID: "forge-juzy",
		HeadSHA:   headSHA,
		BaseURL:   "http://localhost:8123",
		Passed:    false,
		Duration:  7 * time.Second,
		Quests: []QuestOutcome{
			{Name: "login", Passed: true, FailedStep: -1, Duration: 3 * time.Second},
			{
				Name:         "checkout",
				Passed:       false,
				FailedStep:   2,
				ErrorMessage: "element #pay not found",
				Duration:     4 * time.Second,
			},
		},
	}
}

func baseRequest(headSHA string, res *QuestRunResult) ReportRequest {
	return ReportRequest{
		Anvil:        "forge",
		BeadID:       "Forge-juzy",
		PRNumber:     42,
		HeadSHA:      headSHA,
		WorktreePath: "/tmp/anvil",
		Result:       res,
	}
}

func TestReportPreviewQuestResults_CreatesCommentOnFirstReport(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)

	out, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", passingRun("abc1234def")))
	if err != nil {
		t.Fatalf("reporting: %v", err)
	}
	if !out.Created || out.Updated {
		t.Fatalf("expected a created comment, got created=%v updated=%v", out.Created, out.Updated)
	}
	if got := fake.countMethod("POST"); got != 1 {
		t.Fatalf("expected exactly 1 create call, got %d (%v)", got, fake.endpoints())
	}
	if got := fake.countMethod("PATCH"); got != 0 {
		t.Fatalf("expected no edit calls, got %d", got)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("expected 1 comment on the PR, got %d", len(fake.comments))
	}
	body := fake.comments[0].Body
	if !strings.Contains(body, previewQuestMarker("abc1234def")) {
		t.Fatalf("comment body is missing the head-SHA marker:\n%s", body)
	}
	if !strings.HasPrefix(body, previewQuestMarker("abc1234def")) {
		t.Fatalf("marker must open the body so truncation cannot lose it:\n%s", body)
	}
	if !strings.Contains(body, "login") || !strings.Contains(body, "checkout") {
		t.Fatalf("comment body is missing quest rows:\n%s", body)
	}
}

func TestReportPreviewQuestResults_SameHeadSHAEditsInPlace(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)
	ctx := context.Background()
	req := baseRequest("abc1234def", passingRun("abc1234def"))

	first, err := r.ReportPreviewQuestResults(ctx, req)
	if err != nil {
		t.Fatalf("first report: %v", err)
	}

	// Re-run the same commit: now failing, so the body must change while the
	// comment does not.
	req.Result = failingRun("abc1234def")
	second, err := r.ReportPreviewQuestResults(ctx, req)
	if err != nil {
		t.Fatalf("second report: %v", err)
	}

	if !second.Updated || second.Created {
		t.Fatalf("expected an edit, got created=%v updated=%v", second.Created, second.Updated)
	}
	if second.CommentID != first.CommentID {
		t.Fatalf("expected the same comment to be edited: first=%d second=%d", first.CommentID, second.CommentID)
	}
	if got := fake.countMethod("POST"); got != 1 {
		t.Fatalf("expected no second create, got %d creates", got)
	}
	if got := fake.countMethod("PATCH"); got != 1 {
		t.Fatalf("expected exactly 1 edit, got %d", got)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("re-running the same head must not duplicate the comment, got %d", len(fake.comments))
	}
	if !strings.Contains(fake.comments[0].Body, "1 passed, 1 failed") {
		t.Fatalf("edited body did not pick up the new outcome:\n%s", fake.comments[0].Body)
	}
}

func TestReportPreviewQuestResults_NewHeadSHACreatesNewComment(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)
	ctx := context.Background()

	if _, err := r.ReportPreviewQuestResults(ctx, baseRequest("aaaa111", passingRun("aaaa111"))); err != nil {
		t.Fatalf("first report: %v", err)
	}
	out, err := r.ReportPreviewQuestResults(ctx, baseRequest("bbbb222", passingRun("bbbb222")))
	if err != nil {
		t.Fatalf("second report: %v", err)
	}

	if !out.Created || out.Updated {
		t.Fatalf("a new head must create a new comment, got created=%v updated=%v", out.Created, out.Updated)
	}
	if got := fake.countMethod("POST"); got != 2 {
		t.Fatalf("expected 2 creates, got %d", got)
	}
	if got := fake.countMethod("PATCH"); got != 0 {
		t.Fatalf("expected 0 edits, got %d", got)
	}
	if len(fake.comments) != 2 {
		t.Fatalf("expected one comment per head, got %d", len(fake.comments))
	}
	if !strings.Contains(fake.comments[0].Body, previewQuestMarker("aaaa111")) ||
		!strings.Contains(fake.comments[1].Body, previewQuestMarker("bbbb222")) {
		t.Fatal("each comment must carry its own head marker")
	}
}

func TestReportPreviewQuestResults_FailureIsNonBlocking(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)

	out, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", failingRun("abc1234def")))
	if err != nil {
		t.Fatalf("reporting a failing run: %v", err)
	}
	if !out.Created {
		t.Fatal("a failing run must still produce a comment")
	}

	body := fake.comments[0].Body
	for _, want := range []string{"❌ fail", "step 2: element #pay not found", "do not gate this pull request"} {
		if !strings.Contains(body, want) {
			t.Fatalf("failing-run comment is missing %q:\n%s", want, body)
		}
	}

	// The whole point: no check run, no commit status, nothing that could turn a
	// preview quest failure into a merge blocker.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, args := range fake.calls {
		joined := strings.Join(args, " ")
		for _, forbidden := range []string{"check-runs", "check-suites", "statuses", "commits/"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("reporter touched a merge-gating endpoint (%q): %v", forbidden, args)
			}
		}
	}
}

func TestReportPreviewQuestResults_EmbedsUploadedScreenshots(t *testing.T) {
	fake := &fakeGH{}
	uploaded := map[string]string{
		"/logs/Forge-juzy/shot-1.png": "https://cdn.example/shot-1.png",
		"/logs/Forge-juzy/shot-2.png": "https://cdn.example/shot-2.png",
	}
	r := newTestReporter(fake, func(_ context.Context, path string) (string, error) {
		url, ok := uploaded[path]
		if !ok {
			return "", fmt.Errorf("unknown path %s", path)
		}
		return url, nil
	})

	res := failingRun("abc1234def")
	res.Quests[0].Screenshots = []string{"/logs/Forge-juzy/shot-1.png"}
	res.Quests[1].Screenshots = []string{"/logs/Forge-juzy/shot-2.png"}

	out, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", res))
	if err != nil {
		t.Fatalf("reporting: %v", err)
	}
	if out.ScreenshotsUploaded != 2 || out.ScreenshotsFailed != 0 {
		t.Fatalf("expected 2 uploads and 0 failures, got %d/%d", out.ScreenshotsUploaded, out.ScreenshotsFailed)
	}

	body := fake.comments[0].Body
	for _, url := range uploaded {
		if !strings.Contains(body, url) {
			t.Fatalf("uploaded screenshot %s is not linked in the comment:\n%s", url, body)
		}
	}
	if !strings.Contains(body, "![shot-1.png](https://cdn.example/shot-1.png)") {
		t.Fatalf("expected an inline image embed for the uploaded screenshot:\n%s", body)
	}
}

func TestReportPreviewQuestResults_UploadFailureStillPostsComment(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, func(context.Context, string) (string, error) {
		return "", errors.New("artifact store unavailable")
	})

	res := failingRun("abc1234def")
	res.Quests[1].Screenshots = []string{"/logs/Forge-juzy/shot-2.png"}

	out, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", res))
	if err != nil {
		t.Fatalf("an upload failure must not fail the report: %v", err)
	}
	if !out.Created {
		t.Fatal("expected the comment to be posted despite the upload failure")
	}
	if out.ScreenshotsUploaded != 0 || out.ScreenshotsFailed != 1 {
		t.Fatalf("expected 0 uploads and 1 failure, got %d/%d", out.ScreenshotsUploaded, out.ScreenshotsFailed)
	}

	body := fake.comments[0].Body
	if !strings.Contains(body, "/logs/Forge-juzy/shot-2.png") {
		t.Fatalf("an unpublished screenshot must still be named by path:\n%s", body)
	}
	if !strings.Contains(body, "artifact store unavailable") {
		t.Fatalf("the upload failure should be visible in the comment:\n%s", body)
	}
}

func TestReportPreviewQuestResults_NoUploaderNamesPaths(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)

	res := passingRun("abc1234def")
	res.Quests[0].Screenshots = []string{"/logs/Forge-juzy/shot-1.png"}

	out, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", res))
	if err != nil {
		t.Fatalf("reporting: %v", err)
	}
	if out.ScreenshotsUploaded != 0 || out.ScreenshotsFailed != 0 {
		t.Fatalf("with no uploader nothing is attempted, got %d/%d", out.ScreenshotsUploaded, out.ScreenshotsFailed)
	}
	if !strings.Contains(fake.comments[0].Body, "`/logs/Forge-juzy/shot-1.png` (on the Forge host)") {
		t.Fatalf("expected the screenshot path to be named:\n%s", fake.comments[0].Body)
	}
}

func TestReportPreviewQuestResults_SkippedRunPostsNothing(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)

	res := &QuestRunResult{Anvil: "forge", Skipped: true, SkipReason: SkipReasonNoQuests}
	out, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", res))
	if err != nil {
		t.Fatalf("reporting a skipped run: %v", err)
	}
	if out.Created || out.Updated {
		t.Fatal("a skipped run must not produce a comment")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a skipped run must not call gh at all, got %v", fake.calls)
	}
}

func TestReportPreviewQuestResults_ListFailureDoesNotDuplicate(t *testing.T) {
	fake := &fakeGH{listErr: errors.New("gh: 502")}
	r := newTestReporter(fake, nil)

	_, err := r.ReportPreviewQuestResults(context.Background(), baseRequest("abc1234def", passingRun("abc1234def")))
	if err == nil {
		t.Fatal("expected an error when the comment lookup fails")
	}
	if got := fake.countMethod("POST"); got != 0 {
		t.Fatalf("a failed lookup must not blind-post a duplicate, got %d creates", got)
	}
}

func TestReportPreviewQuestResults_RejectsBadRequests(t *testing.T) {
	fake := &fakeGH{}
	r := newTestReporter(fake, nil)
	ctx := context.Background()

	if _, err := r.ReportPreviewQuestResults(ctx, baseRequest("abc", nil)); err == nil {
		t.Fatal("expected an error for a nil result")
	}
	req := baseRequest("abc", passingRun("abc"))
	req.PRNumber = 0
	if _, err := r.ReportPreviewQuestResults(ctx, req); err == nil {
		t.Fatal("expected an error for a missing PR number")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a rejected request must not reach gh, got %v", fake.calls)
	}
}

func TestReportKey_FallsBackToPreviewID(t *testing.T) {
	res := &QuestRunResult{PreviewID: "forge-juzy"}
	if got := reportKey("", res); got != "preview-forge-juzy" {
		t.Fatalf("expected the preview id fallback, got %q", got)
	}
	if got := reportKey("", &QuestRunResult{}); got != "unknown" {
		t.Fatalf("expected the unknown fallback, got %q", got)
	}
	if got := reportKey("  deadbeef  ", res); got != "deadbeef" {
		t.Fatalf("expected the trimmed head SHA, got %q", got)
	}
}

// TestFormatPreviewQuestComment_Golden pins the rendered body so table layout,
// escaping and the non-blocking note cannot drift unnoticed.
func TestFormatPreviewQuestComment_Golden(t *testing.T) {
	res := &QuestRunResult{
		Anvil:     "forge",
		PreviewID: "forge-juzy",
		HeadSHA:   "abc1234def5678",
		BaseURL:   "http://localhost:8123",
		Duration:  12500 * time.Millisecond,
		Quests: []QuestOutcome{
			{Name: "login", Passed: true, FailedStep: -1, Duration: 3200 * time.Millisecond},
			{
				Name:         "checkout",
				Passed:       false,
				FailedStep:   2,
				ErrorMessage: "element |#pay| not found\nafter 5s",
				Duration:     1450 * time.Millisecond,
			},
		},
	}
	shots := []ScreenshotRef{
		{Quest: "checkout", Name: "shot-2.png", Path: "/logs/shot-2.png", URL: "https://cdn.example/shot-2.png"},
		{Quest: "checkout", Name: "shot-3.png", Path: "/logs/shot-3.png", Err: "upload failed"},
	}

	want := strings.Join([]string{
		"<!-- forge-preview-quest: abc1234def5678 -->",
		"### Preview E2E quests — 1 passed, 1 failed",
		"",
		"Ran against a Kiln preview environment, preview `forge-juzy`, commit `abc1234`, base URL `http://localhost:8123`. Total run time 12.5s.",
		"",
		"| Quest | Result | Duration | Detail |",
		"| --- | --- | --- | --- |",
		"| login | ✅ pass | 3.2s |  |",
		"| checkout | ❌ fail | 1.5s | step 2: element \\|#pay\\| not found<br>after 5s |",
		"",
		"#### Screenshots",
		"",
		"**checkout**",
		"",
		"- [shot-2.png](https://cdn.example/shot-2.png)",
		"",
		"  ![shot-2.png](https://cdn.example/shot-2.png)",
		"- `/logs/shot-3.png` — could not be uploaded: upload failed",
		"",
		"> Informational only: preview quest results do not gate this pull request. " +
			"No check run or commit status is created from them, and nothing in the pipeline, " +
			"Bellows or the merge path reads them.",
		"",
	}, "\n")

	got := formatPreviewQuestComment(previewQuestMarker("abc1234def5678"), "abc1234def5678", res, shots)
	if got != want {
		t.Fatalf("rendered comment drifted.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
