package adventurer

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/go-rod/rod/lib/launcher"
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

func TestIntegrationNavigate(t *testing.T) {
	// Skip if Chrome is not available.
	_, err := launcher.New().Headless(true).Launch()
	if err != nil {
		t.Skipf("Chrome not available, skipping integration test: %v", err)
	}

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
