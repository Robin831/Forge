package questgiver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// QuestResult holds the outcome of executing a quest, decoupled from the
// adventurer package to avoid an import cycle.
type QuestResult struct {
	Passed       bool
	FailedStep   int
	ErrorMessage string
	Duration     time.Duration
}

// QuestExecutor executes a quest and returns the result. This interface
// decouples the monitor from the adventurer package to avoid an import cycle
// (adventurer imports questgiver for the Quest type).
type QuestExecutor interface {
	Execute(ctx context.Context, quest *Quest) *QuestResult
}

// Monitor polls anvils for quests and executes them, creating beads on failure
// with deduplication.
type Monitor struct {
	db       *state.DB
	interval time.Duration
	timeout  time.Duration
	anvils   map[string]string // name -> path
	logger   *slog.Logger
	newExec  func() QuestExecutor
}

// New creates a Monitor that polls anvils for quests at the given interval.
// The newExec function is called to create a QuestExecutor for each quest
// execution. Pass nil to use a no-op executor (useful for testing).
func New(db *state.DB, interval, timeout time.Duration, anvils map[string]string, newExec func() QuestExecutor) *Monitor {
	if newExec == nil {
		newExec = func() QuestExecutor { return nopExecutor{} }
	}
	return &Monitor{
		db:       db,
		interval: interval,
		timeout:  timeout,
		anvils:   anvils,
		logger:   slog.Default(),
		newExec:  newExec,
	}
}

type nopExecutor struct{}

func (nopExecutor) Execute(context.Context, *Quest) *QuestResult {
	return &QuestResult{Passed: true, FailedStep: -1}
}

// Run starts the quest polling loop. It blocks until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.scan(ctx)
		}
	}
}

// scan iterates all anvils, discovers quests, and executes them.
func (m *Monitor) scan(ctx context.Context) {
	for anvilName, anvilPath := range m.anvils {
		if ctx.Err() != nil {
			return
		}

		quests, err := DiscoverQuests(anvilPath)
		if err != nil {
			m.logger.Error("failed to discover quests", "anvil", anvilName, "error", err)
			continue
		}

		for i := range quests {
			if ctx.Err() != nil {
				return
			}
			m.runQuest(ctx, anvilName, anvilPath, &quests[i])
		}
	}

	if err := m.db.LogEvent(state.EventQuestgiverScanDone, "quest scan complete", "", ""); err != nil {
		m.logger.Warn("failed to log scan-done event", "error", err)
	}
}

// runQuest executes a single quest and handles the result.
func (m *Monitor) runQuest(ctx context.Context, anvilName, anvilPath string, quest *Quest) {
	m.logger.Info("starting quest", "quest", quest.Name, "anvil", anvilName)
	if err := m.db.LogEvent(state.EventAdventurerStarted, quest.Name, "", anvilName); err != nil {
		m.logger.Warn("failed to log adventurer-started event", "quest", quest.Name, "error", err)
	}

	executor := m.newExec()
	result := executor.Execute(ctx, quest)

	if result.Passed {
		m.logger.Info("quest passed", "quest", quest.Name, "anvil", anvilName, "duration", result.Duration)
		if err := m.db.LogEvent(state.EventAdventurerPassed, quest.Name, "", anvilName); err != nil {
			m.logger.Warn("failed to log adventurer-passed event", "quest", quest.Name, "error", err)
		}
		return
	}

	m.logger.Warn("quest failed", "quest", quest.Name, "anvil", anvilName,
		"step", result.FailedStep, "error", result.ErrorMessage)
	if err := m.db.LogEvent(state.EventAdventurerFailed,
		fmt.Sprintf("%s: step %d — %s", quest.Name, result.FailedStep, result.ErrorMessage),
		"", anvilName); err != nil {
		m.logger.Warn("failed to log adventurer-failed event", "quest", quest.Name, "error", err)
	}

	if isDuplicate(ctx, anvilPath, quest.Name) {
		m.logger.Info("duplicate bead exists, skipping creation", "quest", quest.Name, "anvil", anvilName)
		return
	}

	m.createBead(ctx, anvilName, anvilPath, quest, result)
}

// bdBead is a minimal struct for parsing bd list --json output.
type bdBead struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

// isDuplicate checks if an open/in_progress bead already exists for this quest.
func isDuplicate(ctx context.Context, anvilPath, questName string) bool {
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "list", "--status=open", "--limit", "0", "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.Output()
	if err != nil {
		return false
	}

	prefix := "E2E failure: " + questName
	var beads []bdBead
	if err := json.Unmarshal(out, &beads); err != nil {
		return strings.Contains(string(out), prefix)
	}
	// bd list --status=open returns both open and in_progress beads,
	// so a single call is sufficient for deduplication.
	for _, b := range beads {
		if strings.Contains(b.Title, prefix) {
			return true
		}
	}

	return false
}

// createBead creates a bug bead for a failed quest.
func (m *Monitor) createBead(ctx context.Context, anvilName, anvilPath string, quest *Quest, result *QuestResult) {
	stepAction := ""
	if result.FailedStep >= 0 && result.FailedStep < len(quest.Steps) {
		stepAction = quest.Steps[result.FailedStep].Action
	}

	title := fmt.Sprintf("E2E failure: %s — step %d (%s)", quest.Name, result.FailedStep, result.ErrorMessage)
	if len(title) > 200 {
		title = truncateUTF8(title, 197) + "..."
	}

	description := fmt.Sprintf(
		"Quest: %s\nFailed step: %d (action: %s)\nError: %s\nQuest file: %s\nReproduce: forge quest run %s",
		quest.Name, result.FailedStep, stepAction, result.ErrorMessage, quest.FilePath, quest.Name,
	)

	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "create", "--title", title, "--description", description,
		"--type", "bug", "--priority=1", "--json"))
	cmd.Dir = anvilPath

	out, err := cmd.CombinedOutput()
	if err != nil {
		m.logger.Error("failed to create bead for quest failure",
			"quest", quest.Name, "anvil", anvilName, "error", err, "output", string(out))
		return
	}

	m.logger.Info("created bead for quest failure", "quest", quest.Name, "anvil", anvilName)
	if err := m.db.LogEvent(state.EventTestBeadCreated,
		fmt.Sprintf("E2E failure: %s", quest.Name), "", anvilName); err != nil {
		m.logger.Warn("failed to log test-bead-created event", "quest", quest.Name, "error", err)
	}
}

// truncateUTF8 truncates s to at most maxBytes bytes without splitting a
// multi-byte UTF-8 character.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
