package burnish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/hooks"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vcs"
)

// testHarness saves and restores the package-level function stubs.
type testHarness struct {
	origSmithSpawn  func(ctx context.Context, worktreePath, promptText, logDir string, pv provider.Provider, extraFlags []string) (*smith.Process, error)
	origTemperRun   func(ctx context.Context, worktreePath string, cfg temper.Config, db *state.DB, beadID, anvil string) *temper.Result
	origGitPush     func(ctx context.Context, worktreePath, branch string) error
	origGitRevParse func(ctx context.Context, dir, ref string) (string, error)
	origHookRun     func(ctx context.Context, workerID, hookName, cmd string, env hooks.HookEnv) error
}

func newTestHarness() *testHarness {
	return &testHarness{
		origSmithSpawn:  smithSpawnFn,
		origTemperRun:   temperRunFn,
		origGitPush:     gitPushFn,
		origGitRevParse: gitRevParseFn,
		origHookRun:     hookRunFn,
	}
}

func (h *testHarness) restore() {
	smithSpawnFn = h.origSmithSpawn
	temperRunFn = h.origTemperRun
	gitPushFn = h.origGitPush
	gitRevParseFn = h.origGitRevParse
	hookRunFn = h.origHookRun
}

// fakeVCS implements vcs.Provider for testing.
type fakeVCS struct {
	comments []vcs.ReviewComment
	resolved []string
	mu       sync.Mutex
}

func (f *fakeVCS) CreatePR(_ context.Context, _ vcs.CreateParams) (*vcs.PR, error) {
	return nil, nil
}
func (f *fakeVCS) MergePR(_ context.Context, _ string, _ int, _ string) error { return nil }
func (f *fakeVCS) CheckStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (f *fakeVCS) CheckStatusLight(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (f *fakeVCS) ListOpenPRs(_ context.Context, _ string) ([]vcs.OpenPR, error) { return nil, nil }
func (f *fakeVCS) GetPRByHeadBranch(_ context.Context, _ string, _ string) (*vcs.OpenPR, error) {
	return nil, nil
}
func (f *fakeVCS) GetRepoOwnerAndName(_ context.Context, _ string) (string, string, error) {
	return "owner", "repo", nil
}
func (f *fakeVCS) FetchUnresolvedThreadCount(_ context.Context, _ string, _ int) (int, error) {
	return 0, nil
}
func (f *fakeVCS) FetchPendingReviewRequests(_ context.Context, _ string, _ int) ([]vcs.ReviewRequest, error) {
	return nil, nil
}
func (f *fakeVCS) FetchPRChecks(_ context.Context, _ string, _ int) (string, []vcs.CICheck, error) {
	return "", nil, nil
}
func (f *fakeVCS) FetchCILogs(_ context.Context, _ string, _ []vcs.CICheck) (map[string]string, error) {
	return nil, nil
}
func (f *fakeVCS) FetchReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return f.comments, nil
}
func (f *fakeVCS) ResolveThread(_ context.Context, _ string, threadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, threadID)
	return nil
}
func (f *fakeVCS) Platform() vcs.Platform { return vcs.GitHub }

func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func makeSmithStub(exitCode int) func(ctx context.Context, worktreePath, promptText, logDir string, pv provider.Provider, extraFlags []string) (*smith.Process, error) {
	return func(_ context.Context, _ string, _ string, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		return smith.NewProcessForTest(&smith.Result{
			ExitCode:      exitCode,
			ResultSubtype: "success",
		}), nil
	}
}

func makeTemperStub(passed bool, failedStep string) func(ctx context.Context, worktreePath string, cfg temper.Config, db *state.DB, beadID, anvil string) *temper.Result {
	return func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		r := &temper.Result{Passed: passed, FailedStep: failedStep}
		if !passed {
			r.Steps = []temper.StepResult{{
				Name:    failedStep,
				Command: "go test ./...",
				Output:  "FAIL some/pkg",
				Passed:  false,
			}}
		}
		return r
	}
}

func noPush(_ context.Context, _, _ string) error {
	return nil
}

func noRevParse(_ context.Context, _, _ string) (string, error) {
	return "", fmt.Errorf("not a git repo")
}

func fixedRevParse(local, remote string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, _ string, ref string) (string, error) {
		if strings.HasPrefix(ref, "origin/") {
			return remote, nil
		}
		return local, nil
	}
}

func defaultFixParams(db *state.DB, vcsP vcs.Provider) FixParams {
	return FixParams{
		WorktreePath: os.TempDir(),
		BeadID:       "test-1",
		AnvilName:    "test-anvil",
		PRNumber:     42,
		Branch:       "forge/test",
		DB:           db,
		MaxAttempts:  3,
		Providers:    []provider.Provider{{Kind: provider.Claude, Model: "test"}},
		VCS:          vcsP,
	}
}

func TestFilterActionableComments(t *testing.T) {
	tests := []struct {
		name    string
		input   []vcs.ReviewComment
		wantLen int
	}{
		{
			name:    "empty input",
			input:   nil,
			wantLen: 0,
		},
		{
			name: "skip approved",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "looks good", State: "APPROVED"},
			},
			wantLen: 0,
		},
		{
			name: "skip dismissed",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "fix this", State: "DISMISSED"},
			},
			wantLen: 0,
		},
		{
			name: "skip empty body",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "", State: "CHANGES_REQUESTED"},
			},
			wantLen: 0,
		},
		{
			name: "keep changes requested",
			input: []vcs.ReviewComment{
				{Author: "copilot", Body: "please fix the typo", State: "CHANGES_REQUESTED"},
			},
			wantLen: 1,
		},
		{
			name: "keep thread comment with no state",
			input: []vcs.ReviewComment{
				{Author: "copilot", Body: "this method is too long", ThreadID: "T_kwDO123"},
			},
			wantLen: 1,
		},
		{
			name: "mixed comments",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "LGTM", State: "APPROVED"},
				{Author: "copilot", Body: "fix the null check", State: "CHANGES_REQUESTED"},
				{Author: "bob", Body: "", State: "CHANGES_REQUESTED"},
				{Author: "copilot", Body: "this is unused", ThreadID: "T_kwDO456"},
			},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterActionableComments(tt.input)
			if len(got) != tt.wantLen {
				t.Errorf("filterActionableComments() returned %d comments, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestBuildReviewFixPrompt(t *testing.T) {
	p := FixParams{
		PRNumber:     42,
		Branch:       "forge/Forge-xyz",
		BeadID:       "Forge-xyz",
		WorktreePath: "/tmp/worktree",
	}
	comments := []vcs.ReviewComment{
		{Author: "copilot", Body: "Fix the nil pointer", Path: "main.go", Line: 10, State: "CHANGES_REQUESTED"},
		{Author: "alice", Body: "Rename this variable", Path: "util.go", Line: 25},
	}

	prompt := buildReviewFixPrompt(p, comments)

	if !strings.Contains(prompt, "PR #42") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "forge/Forge-xyz") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "Forge-xyz") {
		t.Error("prompt should contain bead ID")
	}
	if !strings.Contains(prompt, "Fix the nil pointer") {
		t.Error("prompt should contain first comment body")
	}
	if !strings.Contains(prompt, "Rename this variable") {
		t.Error("prompt should contain second comment body")
	}
	if !strings.Contains(prompt, "main.go") {
		t.Error("prompt should contain file path from first comment")
	}
	if !strings.Contains(prompt, "@copilot") {
		t.Error("prompt should contain comment author")
	}
	if !strings.Contains(prompt, "Do NOT push") {
		t.Error("prompt should instruct Smith not to push")
	}
}

func TestBuildReviewFixPrompt_NoAuthorOrPath(t *testing.T) {
	p := FixParams{PRNumber: 1, Branch: "main", BeadID: "Forge-abc"}
	comments := []vcs.ReviewComment{
		{Body: "fix something"},
	}
	prompt := buildReviewFixPrompt(p, comments)
	if !strings.Contains(prompt, "fix something") {
		t.Error("prompt should include body even when author and path are empty")
	}
}

func TestBuildBatchReviewPrompt(t *testing.T) {
	comments := []vcs.ReviewComment{
		{Author: "copilot", Body: "Fix the nil pointer", Path: "main.go", Line: 10},
		{Author: "alice", Body: "Rename this variable", Path: "util.go", Line: 25},
		{Author: "bob", Body: "Add error handling here"},
	}

	prompt := buildBatchReviewPrompt(42, "forge/Forge-xyz", "Forge-xyz", comments)

	if !strings.Contains(prompt, "PR #42") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "forge/Forge-xyz") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "Forge-xyz") {
		t.Error("prompt should contain bead ID")
	}

	// All comment bodies should be present.
	for _, body := range []string{"Fix the nil pointer", "Rename this variable", "Add error handling here"} {
		if !strings.Contains(prompt, body) {
			t.Errorf("prompt should contain comment body %q", body)
		}
	}

	// File paths should be present.
	if !strings.Contains(prompt, "main.go") {
		t.Error("prompt should contain file path")
	}
	if !strings.Contains(prompt, "util.go") {
		t.Error("prompt should contain file path")
	}

	// Authors should be present.
	if !strings.Contains(prompt, "@copilot") {
		t.Error("prompt should contain author")
	}
	if !strings.Contains(prompt, "@alice") {
		t.Error("prompt should contain author")
	}

	// Numbered format.
	if !strings.Contains(prompt, "1.") {
		t.Error("prompt should number comments")
	}
	if !strings.Contains(prompt, "3.") {
		t.Error("prompt should number all comments")
	}

	// Instructions should mention total count.
	if !strings.Contains(prompt, "3 review comments") {
		t.Error("prompt should mention total number of comments in instructions")
	}

	if !strings.Contains(prompt, "Do NOT push") {
		t.Error("prompt should instruct Smith not to push")
	}
}

func TestBuildBatchReviewPrompt_NoAuthorOrPath(t *testing.T) {
	comments := []vcs.ReviewComment{
		{Body: "fix something"},
	}
	prompt := buildBatchReviewPrompt(1, "main", "Forge-abc", comments)
	if !strings.Contains(prompt, "fix something") {
		t.Error("prompt should include body even when author and path are empty")
	}
}

func TestBatchFix_NoActionableComments(t *testing.T) {
	result := BatchFix(context.Background(), BatchFixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BeadID:       "test-1",
		WorktreePath: "/tmp/wt",
		Comments: []vcs.ReviewComment{
			{Author: "alice", Body: "looks good", State: "APPROVED"},
		},
	})

	if !result.Addressed {
		t.Error("BatchFix with no actionable comments should return Addressed=true")
	}
	if result.Error != nil {
		t.Errorf("BatchFix with no actionable comments should not error, got: %v", result.Error)
	}
}

func TestBatchFix_NoProviders(t *testing.T) {
	// With an empty provider list, the smith loop never executes and
	// smithResult stays nil. Verify BatchFix surfaces the error.
	result := BatchFix(context.Background(), BatchFixParams{
		PRNumber:     42,
		Branch:       "forge/test",
		BeadID:       "test-1",
		WorktreePath: t.TempDir(),
		Comments: []vcs.ReviewComment{
			{Author: "copilot", Body: "fix this bug", State: "CHANGES_REQUESTED"},
		},
		Providers: []provider.Provider{},
	})

	if result.Addressed {
		t.Error("BatchFix should not return Addressed=true when smith cannot spawn")
	}
	if result.Error == nil {
		t.Error("BatchFix should return an error when smith fails to spawn")
	}
}

// --- Temper verification tests ---

func TestFix_TemperPasses_Pushes(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)

	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(true, "")

	pushCalled := 0
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled++
		return nil
	}
	gitRevParseFn = noRevParse // HEAD != origin → push needed

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	result := Fix(context.Background(), defaultFixParams(db, v))

	if !result.Addressed {
		t.Error("expected Addressed=true when temper passes")
	}
	if pushCalled != 1 {
		t.Errorf("expected push called once, got %d", pushCalled)
	}
	if result.Error != nil {
		t.Errorf("unexpected error: %v", result.Error)
	}
}

func TestFix_TemperFails_LoopsAndDoesNotPush(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)

	smithSpawnFn = makeSmithStub(0)

	temperAttempt := 0
	var promptsSeen []string
	temperRunFn = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperAttempt++
		if temperAttempt == 1 {
			return &temper.Result{
				Passed:     false,
				FailedStep: "test",
				Steps: []temper.StepResult{{
					Name: "test", Command: "go test ./...", Output: "FAIL pkg/foo", Passed: false,
				}},
			}
		}
		return &temper.Result{Passed: true}
	}

	// Capture prompts to verify temper failure is included on attempt 2.
	origSmith := makeSmithStub(0)
	smithSpawnFn = func(ctx context.Context, wt, prompt, logDir string, pv provider.Provider, flags []string) (*smith.Process, error) {
		promptsSeen = append(promptsSeen, prompt)
		return origSmith(ctx, wt, prompt, logDir, pv, flags)
	}

	pushCalled := 0
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled++
		return nil
	}
	gitRevParseFn = noRevParse

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	result := Fix(context.Background(), defaultFixParams(db, v))

	if !result.Addressed {
		t.Error("expected Addressed=true after second attempt")
	}
	if result.Attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", result.Attempts)
	}
	if pushCalled != 1 {
		t.Errorf("expected push called once (after attempt 2), got %d", pushCalled)
	}
	// The second prompt should contain the temper failure feedback.
	if len(promptsSeen) < 2 {
		t.Fatalf("expected at least 2 prompts, got %d", len(promptsSeen))
	}
	if !strings.Contains(promptsSeen[1], "Previous Verification Failure") {
		t.Error("second prompt should contain temper failure feedback")
	}
	if !strings.Contains(promptsSeen[1], "FAIL pkg/foo") {
		t.Error("second prompt should contain temper output")
	}
}

func TestFix_TemperFailsAllAttempts_NoPush(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)

	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(false, "build")

	pushCalled := 0
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled++
		return nil
	}
	gitRevParseFn = noRevParse

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	params := defaultFixParams(db, v)
	params.MaxAttempts = 3
	result := Fix(context.Background(), params)

	if result.Addressed {
		t.Error("expected Addressed=false when temper fails all attempts")
	}
	if pushCalled != 0 {
		t.Errorf("expected push never called, got %d", pushCalled)
	}
	if result.Error == nil {
		t.Error("expected error when all attempts exhausted")
	}
}

func TestFix_PushFails_ReturnsError(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)

	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(true, "")
	gitRevParseFn = noRevParse // force push path

	gitPushFn = func(_ context.Context, _, _ string) error {
		return fmt.Errorf("network error")
	}

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	result := Fix(context.Background(), defaultFixParams(db, v))

	if result.Addressed {
		t.Error("expected Addressed=false when push fails")
	}
	if result.Error == nil {
		t.Error("expected error when push fails")
	}
	if !strings.Contains(result.Error.Error(), "push after temper verification") {
		t.Errorf("expected push error, got: %v", result.Error)
	}
}

func TestFix_SmithPushedAnyway_TemperStillAuthoritative(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)

	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(false, "test")

	// Simulate Smith already pushed: HEAD matches origin/branch.
	gitRevParseFn = fixedRevParse("abc123", "abc123")

	pushCalled := 0
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled++
		return nil
	}

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	params := defaultFixParams(db, v)
	params.MaxAttempts = 1
	result := Fix(context.Background(), params)

	// Temper failed, so even though Smith pushed, result should NOT be Addressed.
	if result.Addressed {
		t.Error("expected Addressed=false when temper fails, even if Smith pushed")
	}
	if pushCalled != 0 {
		t.Errorf("expected no gitPush call (Smith already pushed), got %d", pushCalled)
	}
}

func TestBatchFix_TemperFails_DoesNotPush(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)

	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(false, "lint")

	pushCalled := 0
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled++
		return nil
	}
	gitRevParseFn = noRevParse

	result := BatchFix(context.Background(), BatchFixParams{
		WorktreePath: os.TempDir(),
		BeadID:       "test-1",
		AnvilName:    "test-anvil",
		PRNumber:     42,
		Branch:       "forge/test",
		DB:           db,
		Providers:    []provider.Provider{{Kind: provider.Claude, Model: "test"}},
		Comments: []vcs.ReviewComment{
			{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
		},
	})

	if result.Addressed {
		t.Error("expected Addressed=false when temper fails in BatchFix")
	}
	if pushCalled != 0 {
		t.Errorf("expected push never called, got %d", pushCalled)
	}
	if result.Error == nil {
		t.Error("expected error when temper fails in BatchFix")
	}
	if !strings.Contains(result.Error.Error(), "temper verification failed") {
		t.Errorf("expected temper error, got: %v", result.Error)
	}
}

func TestFormatTemperFailureForPrompt(t *testing.T) {
	t.Run("nil result returns empty", func(t *testing.T) {
		if got := formatTemperFailureForPrompt(nil); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("passed result returns empty", func(t *testing.T) {
		r := &temper.Result{Passed: true}
		if got := formatTemperFailureForPrompt(r); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("failed result includes step info", func(t *testing.T) {
		r := &temper.Result{
			Passed:     false,
			FailedStep: "test",
			Steps: []temper.StepResult{
				{Name: "build", Command: "go build ./...", Output: "ok", Passed: true},
				{Name: "test", Command: "go test ./...", Output: "FAIL TestFoo", Passed: false},
			},
		}
		got := formatTemperFailureForPrompt(r)
		if !strings.Contains(got, "Previous Verification Failure") {
			t.Error("should contain header")
		}
		if !strings.Contains(got, "Failed step: test") {
			t.Error("should contain failed step name")
		}
		if !strings.Contains(got, "FAIL TestFoo") {
			t.Error("should contain step output")
		}
	})
}

func TestTruncateOutput(t *testing.T) {
	t.Run("short output unchanged", func(t *testing.T) {
		got := truncateOutput("hello", 100)
		if got != "hello" {
			t.Errorf("expected 'hello', got %q", got)
		}
	})

	t.Run("long output truncated from start", func(t *testing.T) {
		long := strings.Repeat("x", 5000)
		got := truncateOutput(long, 100)
		if len(got) > 120 { // 100 + truncation marker
			t.Errorf("expected truncated output, got length %d", len(got))
		}
		if !strings.HasPrefix(got, "... (truncated)") {
			t.Error("should have truncation marker")
		}
	})
}

// --- Hook tests ---

func TestFix_BeforeTemperHook_Invoked(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(true, "")
	gitPushFn = noPush
	gitRevParseFn = noRevParse

	var hooksCalled []string
	hookRunFn = func(_ context.Context, _, hookName, cmd string, env hooks.HookEnv) error {
		if cmd != "" {
			hooksCalled = append(hooksCalled, hookName)
		}
		return nil
	}

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	params := defaultFixParams(db, v)
	params.Hooks = &config.HooksConfig{
		BeforeTemper: "echo before",
		AfterTemper:  "echo after",
	}

	result := Fix(context.Background(), params)

	if !result.Addressed {
		t.Error("expected Addressed=true")
	}
	if len(hooksCalled) < 2 {
		t.Fatalf("expected at least 2 hook calls, got %d: %v", len(hooksCalled), hooksCalled)
	}
	if hooksCalled[0] != "before_temper" {
		t.Errorf("expected first hook to be before_temper, got %s", hooksCalled[0])
	}
	if hooksCalled[1] != "after_temper" {
		t.Errorf("expected second hook to be after_temper, got %s", hooksCalled[1])
	}
}

func TestFix_AfterTemperHook_Invoked(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(true, "")
	gitPushFn = noPush
	gitRevParseFn = noRevParse

	afterCalled := false
	hookRunFn = func(_ context.Context, _, hookName, cmd string, env hooks.HookEnv) error {
		if hookName == "after_temper" && cmd != "" {
			afterCalled = true
		}
		return nil
	}

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	params := defaultFixParams(db, v)
	params.Hooks = &config.HooksConfig{AfterTemper: "echo done"}

	result := Fix(context.Background(), params)

	if !result.Addressed {
		t.Error("expected Addressed=true")
	}
	if !afterCalled {
		t.Error("expected after_temper hook to be called")
	}
}

func TestFix_BeforeTemperHook_Fails_AbortsBurnish(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)

	temperCalled := false
	temperRunFn = func(_ context.Context, _ string, _ temper.Config, _ *state.DB, _, _ string) *temper.Result {
		temperCalled = true
		return &temper.Result{Passed: true}
	}

	pushCalled := false
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled = true
		return nil
	}
	gitRevParseFn = noRevParse

	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "before_temper" && cmd != "" {
			return fmt.Errorf("hook before_temper failed: exit status 1")
		}
		return nil
	}

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	params := defaultFixParams(db, v)
	params.Hooks = &config.HooksConfig{BeforeTemper: "exit 1"}

	result := Fix(context.Background(), params)

	if result.Addressed {
		t.Error("expected Addressed=false when before_temper hook fails")
	}
	if result.Error == nil {
		t.Fatal("expected error when before_temper hook fails")
	}
	if !strings.Contains(result.Error.Error(), "before_temper hook") {
		t.Errorf("expected error to mention before_temper hook, got: %v", result.Error)
	}
	if temperCalled {
		t.Error("temper.Run should NOT have been called after before_temper hook failure")
	}
	if pushCalled {
		t.Error("push should NOT have been called after before_temper hook failure")
	}
}

func TestFix_AfterTemperHook_Fails_Logged_Only(t *testing.T) {
	h := newTestHarness()
	defer h.restore()

	db := openTestDB(t)
	smithSpawnFn = makeSmithStub(0)
	temperRunFn = makeTemperStub(true, "")
	gitRevParseFn = noRevParse

	pushCalled := false
	gitPushFn = func(_ context.Context, _, _ string) error {
		pushCalled = true
		return nil
	}

	hookRunFn = func(_ context.Context, _, hookName, cmd string, _ hooks.HookEnv) error {
		if hookName == "after_temper" && cmd != "" {
			return fmt.Errorf("after_temper hook failed")
		}
		return nil
	}

	v := &fakeVCS{comments: []vcs.ReviewComment{
		{Author: "copilot", Body: "fix this", State: "CHANGES_REQUESTED"},
	}}

	params := defaultFixParams(db, v)
	params.Hooks = &config.HooksConfig{AfterTemper: "exit 1"}

	result := Fix(context.Background(), params)

	if !result.Addressed {
		t.Error("expected Addressed=true even when after_temper hook fails")
	}
	if result.Error != nil {
		t.Errorf("expected no error (after_temper is best-effort), got: %v", result.Error)
	}
	if !pushCalled {
		t.Error("push should still happen after after_temper hook failure")
	}
}
