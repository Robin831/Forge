package adventurer

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	timeout := 30 * time.Second

	exec := New(timeout, logger)

	if exec.timeout != timeout {
		t.Errorf("expected timeout %v, got %v", timeout, exec.timeout)
	}
	if exec.logger != logger {
		t.Error("expected logger to be set")
	}
}

func TestResultConstruction(t *testing.T) {
	sr := StepResult{
		Index:    0,
		Action:   "navigate",
		Passed:   true,
		Duration: 100 * time.Millisecond,
		Error:    "",
	}

	if sr.Index != 0 {
		t.Errorf("expected Index 0, got %d", sr.Index)
	}
	if sr.Action != "navigate" {
		t.Errorf("expected Action 'navigate', got %q", sr.Action)
	}
	if !sr.Passed {
		t.Error("expected Passed to be true")
	}

	result := Result{
		QuestName:    "login test",
		Passed:       true,
		Duration:     500 * time.Millisecond,
		FailedStep:   -1,
		ErrorMessage: "",
		Screenshots:  []string{"/tmp/shot.png"},
		StepResults:  []StepResult{sr},
	}

	if result.QuestName != "login test" {
		t.Errorf("expected QuestName 'login test', got %q", result.QuestName)
	}
	if !result.Passed {
		t.Error("expected Passed to be true")
	}
	if result.FailedStep != -1 {
		t.Errorf("expected FailedStep -1, got %d", result.FailedStep)
	}
	if len(result.Screenshots) != 1 {
		t.Errorf("expected 1 screenshot, got %d", len(result.Screenshots))
	}
	if len(result.StepResults) != 1 {
		t.Errorf("expected 1 step result, got %d", len(result.StepResults))
	}
}

func TestResultFailure(t *testing.T) {
	result := Result{
		QuestName:    "failing test",
		Passed:       false,
		Duration:     200 * time.Millisecond,
		FailedStep:   2,
		ErrorMessage: "element not found",
		StepResults: []StepResult{
			{Index: 0, Action: "navigate", Passed: true, Duration: 50 * time.Millisecond},
			{Index: 1, Action: "fill", Passed: true, Duration: 50 * time.Millisecond},
			{Index: 2, Action: "click", Passed: false, Duration: 100 * time.Millisecond, Error: "element not found"},
		},
	}

	if result.Passed {
		t.Error("expected Passed to be false")
	}
	if result.FailedStep != 2 {
		t.Errorf("expected FailedStep 2, got %d", result.FailedStep)
	}
	if result.ErrorMessage != "element not found" {
		t.Errorf("expected error message 'element not found', got %q", result.ErrorMessage)
	}
	if result.StepResults[2].Passed {
		t.Error("expected step 2 to have failed")
	}
}

// launchBrowser launches a headless browser for integration tests and returns
// the browser and a cleanup function. Calls t.Skip if Chrome is unavailable or
// if the test is running in short mode (-short flag).
func launchBrowser(t *testing.T) (*rod.Browser, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping browser integration test in short mode")
	}
	l := launcher.New().Headless(true)
	controlURL, err := l.Launch()
	if err != nil {
		t.Skipf("Chrome not available, skipping integration test: %v", err)
	}
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		l.Kill()
		l.Cleanup()
		t.Skipf("failed to connect to browser: %v", err)
	}
	return browser, func() {
		_ = browser.Close()
		l.Cleanup()
	}
}

func TestExecuteStepNavigate(t *testing.T) {
	browser, cleanup := launchBrowser(t)
	defer cleanup()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("failed to create page: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	exec := New(10*time.Second, logger)

	step := questgiver.Step{Action: "navigate", URL: "about:blank"}
	var screenshotPath string
	sr := exec.executeStep(page, step, 0, &screenshotPath)

	if !sr.Passed {
		t.Errorf("expected navigate step to pass, got error: %s", sr.Error)
	}
	if sr.Action != "navigate" {
		t.Errorf("expected action 'navigate', got %q", sr.Action)
	}
	if sr.Duration <= 0 {
		t.Error("expected positive duration")
	}
}

func TestExecuteStepUnknownAction(t *testing.T) {
	browser, cleanup := launchBrowser(t)
	defer cleanup()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("failed to create page: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	exec := New(10*time.Second, logger)

	step := questgiver.Step{Action: "dance"}
	var screenshotPath string
	sr := exec.executeStep(page, step, 0, &screenshotPath)

	if sr.Passed {
		t.Error("expected unknown action to fail")
	}
	if sr.Error == "" {
		t.Error("expected error message for unknown action")
	}
}

func TestExecuteStopsOnFirstFailure(t *testing.T) {
	// Verify Chrome is available without leaking a process.
	_, cleanup := launchBrowser(t)
	cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	exec := New(10*time.Second, logger)

	quest := &questgiver.Quest{
		Name: "stop on failure",
		Steps: []questgiver.Step{
			{Action: "navigate", URL: "about:blank"},
			{Action: "click", Selector: "#nonexistent-element-that-wont-exist"},
			{Action: "navigate", URL: "about:blank"}, // Should not be reached.
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := exec.Execute(ctx, quest)

	if result.Passed {
		t.Error("expected quest to fail")
	}
	if result.FailedStep != 1 {
		t.Errorf("expected FailedStep 1, got %d", result.FailedStep)
	}
	// Third step should not have been executed.
	if len(result.StepResults) != 2 {
		t.Errorf("expected 2 step results (stopped at failure), got %d", len(result.StepResults))
	}
}

func TestExecuteContextCancellation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	exec := New(10*time.Second, logger)

	// Cancel context before execution.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	quest := &questgiver.Quest{
		Name: "cancelled quest",
		Steps: []questgiver.Step{
			{Action: "navigate", URL: "about:blank"},
		},
	}

	result := exec.Execute(ctx, quest)

	// With a cancelled context, we expect either a launch failure or a
	// browser/page error — either way the result should not be Passed.
	if result.Passed {
		t.Error("expected quest to fail with cancelled context")
	}
	if result.ErrorMessage == "" {
		t.Error("expected error message for cancelled context")
	}
}

func TestIntegrationNavigate(t *testing.T) {
	// Verify Chrome is available without leaking a process.
	_, cleanup := launchBrowser(t)
	cleanup()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	exec := New(30*time.Second, logger)

	quest := &questgiver.Quest{
		Name: "navigate to blank",
		Steps: []questgiver.Step{
			{Action: "navigate", URL: "about:blank"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := exec.Execute(ctx, quest)

	if !result.Passed {
		t.Errorf("expected quest to pass, got error: %s", result.ErrorMessage)
	}
	if result.FailedStep != -1 {
		t.Errorf("expected FailedStep -1, got %d", result.FailedStep)
	}
	if len(result.StepResults) != 1 {
		t.Errorf("expected 1 step result, got %d", len(result.StepResults))
	}
}
