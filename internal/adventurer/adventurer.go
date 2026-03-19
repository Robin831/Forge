package adventurer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Result holds the outcome of executing a quest.
type Result struct {
	QuestName    string
	Passed       bool
	Duration     time.Duration
	FailedStep   int // -1 if all passed
	ErrorMessage string
	Screenshots  []string // file paths to captured screenshots
	StepResults  []StepResult
}

// StepResult holds the outcome of a single quest step.
type StepResult struct {
	Index    int
	Action   string
	Passed   bool
	Duration time.Duration
	Error    string
}

// Executor drives a headless browser through quest steps.
type Executor struct {
	timeout time.Duration
	logger  *slog.Logger
}

// New creates an Executor with the given default timeout and logger.
func New(timeout time.Duration, logger *slog.Logger) *Executor {
	return &Executor{
		timeout: timeout,
		logger:  logger,
	}
}

// Execute runs all steps of a quest in a headless Chrome browser and returns
// the result. On the first step failure, execution stops immediately.
func (e *Executor) Execute(ctx context.Context, quest *questgiver.Quest) *Result {
	start := time.Now()
	result := &Result{
		QuestName:   quest.Name,
		FailedStep:  -1,
		StepResults: make([]StepResult, 0, len(quest.Steps)),
	}

	// Launch headless browser.
	l, err := launcher.New().Headless(true).Launch()
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to launch browser: %v", err)
		result.Duration = time.Since(start)
		return result
	}

	browser := rod.New().ControlURL(l)
	if err := browser.Connect(); err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to connect to browser: %v", err)
		result.Duration = time.Since(start)
		return result
	}
	defer browser.Close()

	// Close browser on context cancellation.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			browser.Close()
		case <-done:
		}
	}()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to create page: %v", err)
		result.Duration = time.Since(start)
		return result
	}

	for i, step := range quest.Steps {
		var screenshotPath string
		sr := e.executeStep(page, step, i, &screenshotPath)
		result.StepResults = append(result.StepResults, sr)

		if step.Action == "screenshot" && sr.Passed && screenshotPath != "" {
			result.Screenshots = append(result.Screenshots, screenshotPath)
		}

		if !sr.Passed {
			result.FailedStep = i
			result.ErrorMessage = sr.Error
			result.Duration = time.Since(start)
			return result
		}
	}

	result.Passed = true
	result.Duration = time.Since(start)
	return result
}

// executeStep runs a single quest step and returns the result.
func (e *Executor) executeStep(page *rod.Page, step questgiver.Step, index int, screenshotPath *string) StepResult {
	start := time.Now()
	sr := StepResult{
		Index:  index,
		Action: step.Action,
	}

	var err error
	switch step.Action {
	case "navigate":
		err = page.Navigate(step.URL)
	case "fill":
		err = e.doFill(page, step)
	case "click":
		err = e.doClick(page, step)
	case "wait":
		err = e.doWait(page, step)
	case "assert":
		err = e.doAssert(page, step)
	case "screenshot":
		err = e.doScreenshot(page, step, screenshotPath)
	default:
		err = fmt.Errorf("unknown action: %s", step.Action)
	}

	sr.Duration = time.Since(start)
	if err != nil {
		sr.Error = err.Error()
		sr.Passed = false
		e.logger.Warn("step failed", "index", index, "action", step.Action, "error", err)
	} else {
		sr.Passed = true
	}
	return sr
}

func (e *Executor) doFill(page *rod.Page, step questgiver.Step) error {
	el, err := page.Element(step.Selector)
	if err != nil {
		return fmt.Errorf("element %q not found: %w", step.Selector, err)
	}
	return el.Input(step.Value)
}

func (e *Executor) doClick(page *rod.Page, step questgiver.Step) error {
	el, err := page.Element(step.Selector)
	if err != nil {
		return fmt.Errorf("element %q not found: %w", step.Selector, err)
	}
	return el.Click(proto.InputMouseButtonLeft, 1)
}

func (e *Executor) doWait(page *rod.Page, step questgiver.Step) error {
	timeout := e.timeout
	if step.Timeout > 0 {
		timeout = step.Timeout
	}
	_, err := page.Timeout(timeout).Element(step.Selector)
	if err != nil {
		return fmt.Errorf("timed out waiting for %q: %w", step.Selector, err)
	}
	return nil
}

func (e *Executor) doAssert(page *rod.Page, step questgiver.Step) error {
	el, err := page.Element(step.Selector)
	if err != nil {
		return fmt.Errorf("element %q not found: %w", step.Selector, err)
	}
	text, err := el.Text()
	if err != nil {
		return fmt.Errorf("failed to get text from %q: %w", step.Selector, err)
	}
	if !strings.Contains(text, step.Contains) {
		return fmt.Errorf("element %q text %q does not contain %q", step.Selector, text, step.Contains)
	}
	return nil
}

func (e *Executor) doScreenshot(page *rod.Page, step questgiver.Step, outPath *string) error {
	path := step.Value
	if path == "" {
		path = fmt.Sprintf("screenshot_%d.png", time.Now().UnixNano())
	}
	data, err := page.Screenshot(true, nil)
	if err != nil {
		return fmt.Errorf("screenshot failed: %w", err)
	}
	if err := writeFile(path, data); err != nil {
		return err
	}
	*outPath = path
	return nil
}
