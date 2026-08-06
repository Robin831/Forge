package questgiver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// Skip reasons a preview quest run can come back with. A skipped run is not a
// failure: it means a gate said no, and the reason is what the caller shows.
// They are constants so callers (and tests) can branch on them without matching
// prose.
const (
	// SkipReasonNotEnabled means the anvil did not opt in with preview_quests
	// (or previews are off for it, which amounts to the same thing).
	SkipReasonNotEnabled = "preview quests are not enabled for this anvil"
	// SkipReasonNoPreview means no preview is running for the requested head.
	SkipReasonNoPreview = "no preview is running for this anvil at the requested head"
	// SkipReasonPreviewNotHealthy means a preview exists but is starting,
	// degraded, failed or stopped. Running a browser against a half-started
	// stack produces failures that say nothing about the branch.
	SkipReasonPreviewNotHealthy = "preview is not healthy"
	// SkipReasonNoQuests means the anvil declares no quests to run.
	SkipReasonNoQuests = "anvil declares no quests"
)

// PreviewInfo describes the preview environment a quest run targets: its
// sanitized preview id and its current Kiln status (one of the state.Preview*
// values).
type PreviewInfo struct {
	// PreviewID identifies the preview the run targeted. It is carried into
	// QuestRunResult so downstream reporting can be idempotent per preview.
	PreviewID string
	// Status is the preview's overall status. Only state.PreviewRunning — every
	// service healthy — is allowed to host a quest run.
	Status string
}

// PreviewLookup resolves the preview serving a given anvil at a given head
// commit. The bool is false when there is no such preview.
//
// It is a function rather than a concrete dependency so questgiver stays free
// of the Kiln manager (and of the daemon's PR bookkeeping): the daemon supplies
// one backed by kiln.Manager, and tests supply a literal.
type PreviewLookup func(ctx context.Context, anvil, headSHA string) (PreviewInfo, bool)

// QuestOutcome is what one quest did during a run.
type QuestOutcome struct {
	Name         string        `json:"name"`
	Passed       bool          `json:"passed"`
	FailedStep   int           `json:"failed_step"`
	ErrorMessage string        `json:"error_message,omitempty"`
	Duration     time.Duration `json:"duration"`
	FilePath     string        `json:"file_path,omitempty"`
	// Screenshots are filesystem paths to the images this quest captured, in
	// step order. A UI turns them into thumbnails; a screenshot taken before the
	// failing step is often the only evidence of what the app actually looked
	// like, so they are kept for failed quests too.
	Screenshots []string `json:"screenshots,omitempty"`
}

// QuestRunResult is the outcome of running an anvil's quests once.
//
// PreviewID and HeadSHA identify exactly what was exercised, so a consumer
// posting the result (a PR comment, say) can key on them and stay idempotent
// across repeated runs of the same commit. They are empty for a run that did
// not target a preview.
//
// A skipped run — a gate said no — carries Skipped with a SkipReason and no
// quest outcomes; Passed is false so nothing reads a skip as a green run.
type QuestRunResult struct {
	Anvil      string         `json:"anvil"`
	PreviewID  string         `json:"preview_id,omitempty"`
	HeadSHA    string         `json:"head_sha,omitempty"`
	BaseURL    string         `json:"base_url,omitempty"`
	Skipped    bool           `json:"skipped"`
	SkipReason string         `json:"skip_reason,omitempty"`
	Passed     bool           `json:"passed"`
	Quests     []QuestOutcome `json:"quests"`
	StartedAt  time.Time      `json:"started_at"`
	Duration   time.Duration  `json:"duration"`
}

// Failures returns the quests that did not pass.
func (r *QuestRunResult) Failures() []QuestOutcome {
	if r == nil {
		return nil
	}
	var failed []QuestOutcome
	for _, q := range r.Quests {
		if !q.Passed {
			failed = append(failed, q)
		}
	}
	return failed
}

// SetPreviewQuestAnvils replaces the set of anvils whose quests may run against
// a preview, as anvil name → main checkout path. Membership is the opt-in:
// the daemon builds it from the per-anvil preview_quests flag, so an anvil that
// did not opt in is simply absent.
//
// It is separate from the monitor's scheduled-scan anvils because the two
// answer different questions: the scan set is filtered by questgiver_enabled,
// while a preview run is something a human asked for on a specific branch.
// Safe to call concurrently with Run.
func (m *Monitor) SetPreviewQuestAnvils(anvils map[string]string) {
	copied := make(map[string]string, len(anvils))
	for k, v := range anvils {
		copied[k] = v
	}
	m.mu.Lock()
	m.previewQuests = copied
	m.mu.Unlock()
}

// SetPreviewLookup installs the resolver RunQuestsForPreview uses to find the
// preview it should check the health of. Without one, preview quest runs are
// unwired and RunQuestsForPreview returns an error rather than running blind.
// Safe to call concurrently with Run.
func (m *Monitor) SetPreviewLookup(lookup PreviewLookup) {
	m.mu.Lock()
	m.previewLookup = lookup
	m.mu.Unlock()
}

// RunQuestsForPreview runs the anvil's quests against a preview environment
// instead of the anvil's fixed quest URL: baseURL (the preview's entry service
// URL) is substituted into the `{{.BaseURL}}` placeholder of every quest step.
//
// It is gated twice. The anvil must have opted in via preview_quests, and the
// preview for headSHA must exist and be healthy — a preview that is still
// starting, degraded or failed produces browser failures that say nothing about
// the branch. Either gate returns a skipped result with a reason and a nil
// error; errors are reserved for a caller mistake (missing anvil or base URL,
// no lookup wired) and for a run that could not be carried out (unreadable or
// unparsable quest files).
//
// Unlike the scheduled scan this never creates beads: the run belongs to one
// branch at one commit, and reporting it is the caller's job (which is why the
// result carries the preview id and head SHA). It also skips the anvil's
// questgiver setup/teardown commands — the preview already is the environment
// those commands would otherwise build.
func (m *Monitor) RunQuestsForPreview(ctx context.Context, anvilID, headSHA, baseURL string) (*QuestRunResult, error) {
	anvilID = strings.TrimSpace(anvilID)
	headSHA = strings.TrimSpace(headSHA)
	baseURL = strings.TrimSpace(baseURL)

	if anvilID == "" {
		return nil, errors.New("questgiver: preview quest run requires an anvil")
	}
	if baseURL == "" {
		return nil, errors.New("questgiver: preview quest run requires a preview base URL")
	}

	m.mu.RLock()
	anvilPath, optedIn := m.previewQuests[anvilID]
	lookup := m.previewLookup
	m.mu.RUnlock()

	started := time.Now()
	result := &QuestRunResult{
		Anvil:     anvilID,
		HeadSHA:   headSHA,
		BaseURL:   strings.TrimRight(baseURL, "/"),
		StartedAt: started,
		Quests:    []QuestOutcome{},
	}
	skip := func(reason string) (*QuestRunResult, error) {
		result.Skipped = true
		result.SkipReason = reason
		result.Duration = time.Since(started)
		m.logger.Info("skipping preview quest run",
			"anvil", anvilID, "head_sha", headSHA, "reason", reason)
		return result, nil
	}

	if !optedIn {
		return skip(SkipReasonNotEnabled)
	}
	if lookup == nil {
		return nil, errors.New("questgiver: preview quest runs are not wired up (no preview lookup configured)")
	}

	preview, ok := lookup(ctx, anvilID, headSHA)
	if !ok {
		return skip(SkipReasonNoPreview)
	}
	result.PreviewID = preview.PreviewID
	if preview.Status != state.PreviewRunning {
		return skip(fmt.Sprintf("%s (status %q)", SkipReasonPreviewNotHealthy, preview.Status))
	}

	quests, err := DiscoverQuests(anvilPath)
	if err != nil {
		return nil, fmt.Errorf("questgiver: discovering quests for anvil %s: %w", anvilID, err)
	}
	if len(quests) == 0 {
		return skip(SkipReasonNoQuests)
	}

	m.logger.Info("running quests against preview",
		"anvil", anvilID, "preview", preview.PreviewID, "head_sha", headSHA,
		"base_url", result.BaseURL, "quests", len(quests))

	passed := true
	for i := range quests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		expanded, err := Expand(&quests[i], baseURL)
		if err != nil {
			return nil, err
		}

		m.logEvent(state.EventAdventurerStarted, expanded.Name, anvilID)
		res := m.executeQuest(ctx, expanded)

		outcome := QuestOutcome{
			Name:         expanded.Name,
			Passed:       res.Passed,
			FailedStep:   res.FailedStep,
			ErrorMessage: res.ErrorMessage,
			Duration:     res.Duration,
			FilePath:     expanded.FilePath,
			Screenshots:  res.Screenshots,
		}
		result.Quests = append(result.Quests, outcome)

		if res.Passed {
			m.logEvent(state.EventAdventurerPassed, expanded.Name, anvilID)
			continue
		}
		passed = false
		m.logger.Warn("quest failed against preview",
			"anvil", anvilID, "preview", preview.PreviewID, "quest", expanded.Name,
			"step", res.FailedStep, "error", res.ErrorMessage)
		m.logEvent(state.EventAdventurerFailed,
			fmt.Sprintf("%s: step %d — %s", expanded.Name, res.FailedStep, res.ErrorMessage), anvilID)
	}

	result.Passed = passed
	result.Duration = time.Since(started)
	return result, nil
}
