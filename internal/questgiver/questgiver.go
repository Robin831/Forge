package questgiver

import (
	"context"
	"fmt"
	"log/slog"
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
	// Screenshots are the filesystem paths of the images the quest's
	// `screenshot` steps captured, in step order. They are what a preview quest
	// run surfaces as thumbnails, which is why they are carried here rather than
	// left behind in the adventurer's own result type.
	Screenshots []string
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
	// previewQuests holds the anvils that opted into running their quests
	// against a preview environment (name -> path). See SetPreviewQuestAnvils.
	previewQuests map[string]string
	// previewLookup resolves the preview a preview quest run targets. See
	// SetPreviewLookup.
	previewLookup PreviewLookup
	logger        *slog.Logger
	newExec       func() QuestExecutor
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
		db:            db,
		interval:      interval,
		timeout:       timeout,
		anvils:        anvils,
		previewQuests: map[string]string{},
		logger:        slog.Default(),
		newExec:       newExec,
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

// executeQuest runs one quest under the monitor's per-quest timeout. It is the
// single place a quest meets an executor, shared by the scheduled scan and by
// preview runs so both inherit the same timeout handling.
func (m *Monitor) executeQuest(ctx context.Context, quest *Quest) *QuestResult {
	questCtx := ctx
	questCancel := func() {}
	if m.timeout > 0 {
		questCtx, questCancel = context.WithTimeout(ctx, m.timeout)
	}
	defer questCancel()

	return m.newExec().Execute(questCtx, quest)
}

// logEvent records a quest event, tolerating a monitor without a database.
func (m *Monitor) logEvent(event state.EventType, detail, anvilName string) {
	if m.db == nil {
		return
	}
	if err := m.db.LogEvent(event, detail, "", anvilName); err != nil {
		m.logger.Warn("failed to log quest event", "event", event, "anvil", anvilName, "error", err)
	}
}

// runQuest executes a single quest and handles the result.
func (m *Monitor) runQuest(ctx context.Context, anvilName, anvilPath string, quest *Quest) {
	m.logger.Info("starting quest", "quest", quest.Name, "anvil", anvilName)
	m.logEvent(state.EventAdventurerStarted, quest.Name, anvilName)

	// A scheduled run has no preview to point at, so templates resolve against
	// the quest's own url field. A quest file whose templates do not parse is
	// not executed at all: navigating to a literal "{{.BaseURL}}/login" would
	// fail in a way that looks like a product bug and would create a bead
	// blaming the application for a broken quest file.
	expanded, err := Expand(quest, "")
	if err != nil {
		m.logger.Error("skipping quest with an invalid template",
			"quest", quest.Name, "anvil", anvilName, "file", quest.FilePath, "error", err)
		return
	}

	result := m.executeQuest(ctx, expanded)

	if result.Passed {
		m.logger.Info("quest passed", "quest", quest.Name, "anvil", anvilName, "duration", result.Duration)
		m.logEvent(state.EventAdventurerPassed, quest.Name, anvilName)
		return
	}

	m.logger.Warn("quest failed", "quest", quest.Name, "anvil", anvilName,
		"step", result.FailedStep, "error", result.ErrorMessage)
	m.logEvent(state.EventAdventurerFailed,
		fmt.Sprintf("%s: step %d — %s", quest.Name, result.FailedStep, result.ErrorMessage), anvilName)

	lookup := m.questBeadLookupFor(ctx, anvilName, anvilPath, quest.Name)
	existing, err := lookup.resolve()
	if err != nil {
		// The answer is unknown, so do not create: a scheduled scan that files a
		// bead whenever it cannot check would file one every cycle.
		m.logger.Error("could not look up this anvil's bead for the quest; skipping bead creation",
			"quest", quest.Name, "anvil", anvilName, "error", err)
		return
	}
	if existing != nil {
		m.logger.Info("bead already exists for this anvil's quest, skipping creation",
			"quest", quest.Name, "anvil", anvilName, "bead", existing.ID)
		return
	}

	m.createBead(ctx, anvilName, anvilPath, quest, result, lookup)
}

// createBead creates a bug bead for a failed quest.
func (m *Monitor) createBead(ctx context.Context, anvilName, anvilPath string, quest *Quest, result *QuestResult, lookup questBeadLookup) {
	stepAction := ""
	if result.FailedStep >= 0 && result.FailedStep < len(quest.Steps) {
		stepAction = quest.Steps[result.FailedStep].Action
	}

	title := fmt.Sprintf("%s%s — step %d (%s)", questBeadTitlePrefix, quest.Name, result.FailedStep, result.ErrorMessage)
	if len(title) > 200 {
		title = truncateUTF8(title, 197) + "..."
	}

	description := questBeadDescription(anvilName, quest, result, stepAction)

	cmd, cancel := executil.BdCommand(ctx,
		"create", "--title", title, "--description", description,
		"--type", "bug", "--priority=1", "--json")
	defer cancel()
	cmd.Dir = anvilPath

	out, err := cmd.CombinedOutput()
	if err != nil {
		m.logger.Error("failed to create bead for quest failure",
			"quest", quest.Name, "anvil", anvilName, "error", err, "output", string(out))
		return
	}

	// Pin the new bead to this anvil now. Without a pin the next run falls back
	// to the description scan, which finds it too; with one, a bead a human has
	// retitled or reworded is still this anvil's bead for this quest.
	if created := executil.BdCreatedBeadID(out); created != "" {
		lookup.Remember(created)
		m.logger.Info("created bead for quest failure",
			"quest", quest.Name, "anvil", anvilName, "bead", created)
	} else {
		m.logger.Info("created bead for quest failure", "quest", quest.Name, "anvil", anvilName)
	}
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
