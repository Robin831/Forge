package questgiver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// recordingExecutor captures the quests it is handed so a test can assert on
// the URLs a run would actually have driven a browser to.
type recordingExecutor struct {
	mu     sync.Mutex
	seen   []*Quest
	result *QuestResult
}

func (r *recordingExecutor) Execute(_ context.Context, quest *Quest) *QuestResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, quest)
	if r.result != nil {
		copied := *r.result
		return &copied
	}
	return &QuestResult{Passed: true, FailedStep: -1}
}

func (r *recordingExecutor) quests() []*Quest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Quest, len(r.seen))
	copy(out, r.seen)
	return out
}

// writeQuest writes a quest file into <dir>/.forge/quests and returns dir.
func writeQuest(t *testing.T, dir, name, body string) string {
	t.Helper()
	questsDir := filepath.Join(dir, ".forge", "quests")
	if err := os.MkdirAll(questsDir, 0o755); err != nil {
		t.Fatalf("creating quests dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(questsDir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("writing quest: %v", err)
	}
	return dir
}

const templatedQuest = `name: login-flow
url: "http://localhost:3000"
steps:
  - action: navigate
    url: "{{.BaseURL}}/login"
  - action: fill
    selector: "#email"
    value: "test@example.com"
`

// previewMonitor builds a monitor with one opted-in anvil, a healthy preview
// and a recording executor.
func previewMonitor(t *testing.T, anvilPath string, exec *recordingExecutor) *Monitor {
	t.Helper()
	m := New(nil, time.Minute, time.Minute, map[string]string{}, func() QuestExecutor { return exec })
	m.SetPreviewQuestAnvils(map[string]string{"api": anvilPath})
	m.SetPreviewLookup(func(context.Context, string, string) (PreviewInfo, bool) {
		return PreviewInfo{PreviewID: "forge_ynr7", Status: state.PreviewRunning}, true
	})
	return m
}

// TestRunQuestsForPreviewSkipsWhenNotEnabled is the opt-in: an anvil that never
// set preview_quests must run nothing at all, and say why.
func TestRunQuestsForPreviewSkipsWhenNotEnabled(t *testing.T) {
	exec := &recordingExecutor{}
	anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)

	m := previewMonitor(t, anvil, exec)
	m.SetPreviewQuestAnvils(map[string]string{}) // nobody opted in

	res, err := m.RunQuestsForPreview(context.Background(), "api", "abc1234", "http://127.0.0.1:42001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Skipped || res.SkipReason != SkipReasonNotEnabled {
		t.Errorf("skipped=%v reason=%q, want a %q skip", res.Skipped, res.SkipReason, SkipReasonNotEnabled)
	}
	if res.Passed {
		t.Error("a skipped run must not report as passed")
	}
	if len(res.Quests) != 0 {
		t.Errorf("quests = %d, want 0", len(res.Quests))
	}
	if got := exec.quests(); len(got) != 0 {
		t.Errorf("executed %d quests, want 0", len(got))
	}
}

// TestRunQuestsForPreviewSkipsUnhealthyPreview covers the second gate: a
// preview that is still starting (or degraded) produces browser failures that
// say nothing about the branch, so it is not driven at all.
func TestRunQuestsForPreviewSkipsUnhealthyPreview(t *testing.T) {
	statuses := []string{state.PreviewStarting, state.PreviewDegraded, state.PreviewFailed, state.PreviewStopped}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			exec := &recordingExecutor{}
			anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)

			m := previewMonitor(t, anvil, exec)
			m.SetPreviewLookup(func(context.Context, string, string) (PreviewInfo, bool) {
				return PreviewInfo{PreviewID: "forge_ynr7", Status: status}, true
			})

			res, err := m.RunQuestsForPreview(context.Background(), "api", "abc1234", "http://127.0.0.1:42001")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !res.Skipped || !strings.HasPrefix(res.SkipReason, SkipReasonPreviewNotHealthy) {
				t.Errorf("skipped=%v reason=%q, want a %q skip", res.Skipped, res.SkipReason, SkipReasonPreviewNotHealthy)
			}
			if res.PreviewID != "forge_ynr7" {
				t.Errorf("preview id = %q, want it recorded even on a skip", res.PreviewID)
			}
			if got := exec.quests(); len(got) != 0 {
				t.Errorf("executed %d quests, want 0", len(got))
			}
		})
	}
}

// TestRunQuestsForPreviewSkipsWithoutPreview covers a head with nothing running.
func TestRunQuestsForPreviewSkipsWithoutPreview(t *testing.T) {
	exec := &recordingExecutor{}
	anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)

	m := previewMonitor(t, anvil, exec)
	m.SetPreviewLookup(func(context.Context, string, string) (PreviewInfo, bool) {
		return PreviewInfo{}, false
	})

	res, err := m.RunQuestsForPreview(context.Background(), "api", "abc1234", "http://127.0.0.1:42001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Skipped || res.SkipReason != SkipReasonNoPreview {
		t.Errorf("skipped=%v reason=%q, want a %q skip", res.Skipped, res.SkipReason, SkipReasonNoPreview)
	}
	if got := exec.quests(); len(got) != 0 {
		t.Errorf("executed %d quests, want 0", len(got))
	}
}

// TestRunQuestsForPreviewUsesPreviewURL is the point of the whole path: the
// quest runs against the preview's entry URL, not the quest file's fixed url.
func TestRunQuestsForPreviewUsesPreviewURL(t *testing.T) {
	exec := &recordingExecutor{}
	anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)
	m := previewMonitor(t, anvil, exec)

	// A trailing slash on the preview URL must not produce "//login".
	res, err := m.RunQuestsForPreview(context.Background(), "api", "abcdef1234567890", "http://127.0.0.1:42001/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Skipped {
		t.Fatalf("unexpected skip: %s", res.SkipReason)
	}

	seen := exec.quests()
	if len(seen) != 1 {
		t.Fatalf("executed %d quests, want 1", len(seen))
	}
	if got, want := seen[0].Steps[0].URL, "http://127.0.0.1:42001/login"; got != want {
		t.Errorf("navigate url = %q, want %q", got, want)
	}
	if got, want := seen[0].Steps[1].Value, "test@example.com"; got != want {
		t.Errorf("fill value = %q, want it untouched, got %q", want, got)
	}
	if !res.Passed {
		t.Error("a run whose quests all passed must report passed")
	}
}

// TestRunQuestsForPreviewTagsResult pins the idempotency handles downstream
// reporting keys on: which preview ran, at which commit, against which URL.
func TestRunQuestsForPreviewTagsResult(t *testing.T) {
	exec := &recordingExecutor{result: &QuestResult{Passed: false, FailedStep: 1, ErrorMessage: "element not found"}}
	anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)
	m := previewMonitor(t, anvil, exec)

	res, err := m.RunQuestsForPreview(context.Background(), "api", "abcdef1234567890", "http://127.0.0.1:42001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.PreviewID != "forge_ynr7" {
		t.Errorf("preview id = %q, want forge_ynr7", res.PreviewID)
	}
	if res.HeadSHA != "abcdef1234567890" {
		t.Errorf("head sha = %q, want abcdef1234567890", res.HeadSHA)
	}
	if res.Anvil != "api" {
		t.Errorf("anvil = %q, want api", res.Anvil)
	}
	if res.BaseURL != "http://127.0.0.1:42001" {
		t.Errorf("base url = %q, want http://127.0.0.1:42001", res.BaseURL)
	}
	if res.Passed {
		t.Error("a run with a failing quest must not report passed")
	}
	if len(res.Quests) != 1 || res.Quests[0].Name != "login-flow" {
		t.Fatalf("quests = %+v, want one login-flow outcome", res.Quests)
	}
	if res.Quests[0].Passed || res.Quests[0].FailedStep != 1 || res.Quests[0].ErrorMessage != "element not found" {
		t.Errorf("outcome = %+v, want the executor's failure carried through", res.Quests[0])
	}
	if failures := res.Failures(); len(failures) != 1 {
		t.Errorf("Failures() = %d, want 1", len(failures))
	}
	if res.StartedAt.IsZero() {
		t.Error("started_at must be set")
	}
}

// TestRunQuestsForPreviewSkipsAnvilWithoutQuests keeps "nothing to run" apart
// from "everything passed".
func TestRunQuestsForPreviewSkipsAnvilWithoutQuests(t *testing.T) {
	exec := &recordingExecutor{}
	m := previewMonitor(t, t.TempDir(), exec)

	res, err := m.RunQuestsForPreview(context.Background(), "api", "abc1234", "http://127.0.0.1:42001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Skipped || res.SkipReason != SkipReasonNoQuests {
		t.Errorf("skipped=%v reason=%q, want a %q skip", res.Skipped, res.SkipReason, SkipReasonNoQuests)
	}
	if res.Passed {
		t.Error("a skipped run must not report as passed")
	}
}

// TestRunQuestsForPreviewArgumentErrors covers the caller mistakes that are
// errors rather than skips: they cannot be fixed by waiting.
func TestRunQuestsForPreviewArgumentErrors(t *testing.T) {
	exec := &recordingExecutor{}
	anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)
	m := previewMonitor(t, anvil, exec)

	if _, err := m.RunQuestsForPreview(context.Background(), "", "abc1234", "http://x"); err == nil {
		t.Error("expected an error for a missing anvil")
	}
	if _, err := m.RunQuestsForPreview(context.Background(), "api", "abc1234", "  "); err == nil {
		t.Error("expected an error for a missing base URL")
	}

	unwired := New(nil, time.Minute, time.Minute, map[string]string{}, func() QuestExecutor { return exec })
	unwired.SetPreviewQuestAnvils(map[string]string{"api": anvil})
	if _, err := unwired.RunQuestsForPreview(context.Background(), "api", "abc1234", "http://x"); err == nil {
		t.Error("expected an error when no preview lookup is wired up")
	}
}

// TestExpand covers the templating hook both paths share.
func TestExpand(t *testing.T) {
	quest := &Quest{
		Name: "login-flow",
		URL:  "http://localhost:3000",
		Steps: []Step{
			{Action: "navigate", URL: "{{.BaseURL}}/login"},
			{Action: "fill", Selector: "#email", Value: "test@example.com"},
			{Action: "navigate", URL: "https://example.com/absolute"},
		},
	}

	// No override: templates resolve against the quest's own url.
	own, err := Expand(quest, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := own.Steps[0].URL, "http://localhost:3000/login"; got != want {
		t.Errorf("navigate url = %q, want %q", got, want)
	}
	if got, want := own.Steps[2].URL, "https://example.com/absolute"; got != want {
		t.Errorf("absolute url = %q, want it untouched", got)
	}

	// An override wins, and the original quest is never modified.
	over, err := Expand(quest, "http://127.0.0.1:42001/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := over.Steps[0].URL, "http://127.0.0.1:42001/login"; got != want {
		t.Errorf("navigate url = %q, want %q", got, want)
	}
	if got, want := over.URL, "http://127.0.0.1:42001"; got != want {
		t.Errorf("quest url = %q, want the effective base %q", got, want)
	}
	if quest.Steps[0].URL != "{{.BaseURL}}/login" {
		t.Errorf("source quest was mutated: %q", quest.Steps[0].URL)
	}

	// A misspelled placeholder is an error, not a silent "<no value>".
	if _, err := Expand(&Quest{Name: "bad", Steps: []Step{{URL: "{{.BasURL}}/x"}}}, "http://x"); err == nil {
		t.Error("expected an error for an unknown placeholder")
	}
	if _, err := Expand(nil, "http://x"); err == nil {
		t.Error("expected an error for a nil quest")
	}
}

// TestScanExpandsAgainstQuestURL is the regression guard for the scheduled
// path: it keeps using each quest's own url as the base.
func TestScanExpandsAgainstQuestURL(t *testing.T) {
	exec := &recordingExecutor{}
	anvil := writeQuest(t, t.TempDir(), "login.yaml", templatedQuest)

	m := New(nil, time.Minute, time.Minute, map[string]string{"api": anvil},
		func() QuestExecutor { return exec })
	m.scan(context.Background())

	seen := exec.quests()
	if len(seen) != 1 {
		t.Fatalf("executed %d quests, want 1", len(seen))
	}
	if got, want := seen[0].Steps[0].URL, "http://localhost:3000/login"; got != want {
		t.Errorf("navigate url = %q, want %q", got, want)
	}
}
