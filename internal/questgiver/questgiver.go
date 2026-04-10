package questgiver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
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
	mu       sync.RWMutex
	anvils   map[string]string // name -> path
	logger   *slog.Logger
	newExec  func() QuestExecutor
}

// New creates a Monitor that polls anvils for quests at the given interval.
// The newExec function is called to create a QuestExecutor for each quest
// execution. Passing nil for newExec is permitted for testing but produces a
// warning — the resulting monitor will pass every quest without executing it.
func New(db *state.DB, interval, timeout time.Duration, anvils map[string]string, newExec func() QuestExecutor) *Monitor {
	if newExec == nil {
		slog.Warn("questgiver: newExec is nil; using no-op executor — quests will not be executed and failures will never produce beads")
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

// UpdateAnvilPaths replaces the set of anvils the monitor polls.
// Safe to call concurrently with Run.
func (m *Monitor) UpdateAnvilPaths(paths map[string]string) {
	copied := make(map[string]string, len(paths))
	for k, v := range paths {
		copied[k] = v
	}
	m.mu.Lock()
	m.anvils = copied
	m.mu.Unlock()
}

type nopExecutor struct{}

func (nopExecutor) Execute(context.Context, *Quest) *QuestResult {
	return &QuestResult{Passed: true, FailedStep: -1}
}

// Run starts the quest polling loop. It blocks until ctx is cancelled.
// Returns an error if the configured interval is non-positive.
func (m *Monitor) Run(ctx context.Context) error {
	if m.interval <= 0 {
		return fmt.Errorf("questgiver: monitor interval must be > 0, got %s", m.interval)
	}
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
	m.mu.RLock()
	anvils := make(map[string]string, len(m.anvils))
	for k, v := range m.anvils {
		anvils[k] = v
	}
	m.mu.RUnlock()

	for anvilName, anvilPath := range anvils {
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

	if m.db != nil {
		if err := m.db.LogEvent(state.EventQuestgiverScanDone, "quest scan complete", "", ""); err != nil {
			m.logger.Warn("failed to log scan-done event", "error", err)
		}
	}
}

// runQuest executes a single quest and handles the result.
func (m *Monitor) runQuest(ctx context.Context, anvilName, anvilPath string, quest *Quest) {
	m.logger.Info("starting quest", "quest", quest.Name, "anvil", anvilName)
	if m.db != nil {
		if err := m.db.LogEvent(state.EventAdventurerStarted, quest.Name, "", anvilName); err != nil {
			m.logger.Warn("failed to log adventurer-started event", "quest", quest.Name, "error", err)
		}
	}

	questCtx := ctx
	questCancel := func() {}
	if m.timeout > 0 {
		questCtx, questCancel = context.WithTimeout(ctx, m.timeout)
	}
	defer questCancel()

	executor := m.newExec()
	result := executor.Execute(questCtx, quest)

	if result.Passed {
		m.logger.Info("quest passed", "quest", quest.Name, "anvil", anvilName, "duration", result.Duration)
		if m.db != nil {
			if err := m.db.LogEvent(state.EventAdventurerPassed, quest.Name, "", anvilName); err != nil {
				m.logger.Warn("failed to log adventurer-passed event", "quest", quest.Name, "error", err)
			}
		}
		return
	}

	m.logger.Warn("quest failed", "quest", quest.Name, "anvil", anvilName,
		"step", result.FailedStep, "error", result.ErrorMessage)
	if m.db != nil {
		if err := m.db.LogEvent(state.EventAdventurerFailed,
			fmt.Sprintf("%s: step %d — %s", quest.Name, result.FailedStep, result.ErrorMessage),
			"", anvilName); err != nil {
			m.logger.Warn("failed to log adventurer-failed event", "quest", quest.Name, "error", err)
		}
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

// isDuplicate checks if an open or in_progress bead already exists for this
// quest. If bd list fails, it returns true to prevent creating duplicate beads
// when deduplication state is unknown.
func isDuplicate(ctx context.Context, anvilPath, questName string) bool {
	prefix := "E2E failure: " + questName

	for _, status := range []string{"open", "in_progress"} {
		cmdCtx, cancel := context.WithTimeout(ctx, executil.DefaultBdTimeout)
		cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
			"bd", "list", "--status="+status, "--limit", "0", "--json"))
		cmd.Dir = anvilPath
		out, err := cmd.Output()
		cancel()

		if err != nil {
			slog.Error("bd list failed during quest deduplication; treating as duplicate and skipping bead creation",
				"anvil_path", anvilPath,
				"quest_name", questName,
				"status", status,
				"error", err)
			return true
		}

		var beads []bdBead
		if err := json.Unmarshal(out, &beads); err != nil {
			if strings.Contains(string(out), prefix) {
				return true
			}
			continue
		}
		for _, b := range beads {
			if strings.Contains(b.Title, prefix) {
				return true
			}
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

	cmdCtx, cancel := context.WithTimeout(ctx, executil.DefaultBdTimeout)
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
	if m.db != nil {
		if err := m.db.LogEvent(state.EventTestBeadCreated,
			fmt.Sprintf("E2E failure: %s", quest.Name), "", anvilName); err != nil {
			m.logger.Warn("failed to log test-bead-created event", "quest", quest.Name, "error", err)
		}
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
