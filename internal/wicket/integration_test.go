package wicket

// Integration tests for the full Wicket lifecycle. These tests exercise the
// end-to-end wiring of all Phase 4 features using mock clients and stub
// runners — no real GitHub API calls, AI providers, or bd CLI processes are
// spawned. Each test scenario drives the complete pipeline:
//
//   issue arrives → triage → bead created → dispatch confirmed
//   → PR linked → merged → issue closed

import (
	"context"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIntegrationMonitor builds a fully-wired Monitor suitable for integration
// tests. It wires a real (temp) state.DB and a configurable MockGitHubClient.
// Callers are responsible for stubbing bdRunner/bdUpdateRunner (e.g. via the
// helpers below) so that no external bd CLI processes are spawned. The caller
// may further customise mock callbacks and triageFunc.
func newIntegrationMonitor(t *testing.T, triage func(context.Context, Issue, []Comment, TriageConfig) TriageDecision) (*Monitor, *MockGitHubClient, *state.DB) {
	t.Helper()
	db := openTestDB(t)
	mock := &MockGitHubClient{}
	m := &Monitor{
		ghClient:   mock,
		db:         db,
		cfg:        &config.Config{Settings: defaultSettings()},
		rl:         newRateLimiter(),
		triageFunc: triage,
	}
	return m, mock, db
}

// stubBDRunner replaces bdRunner for the duration of the test and registers
// cleanup via t.Cleanup to restore the original. The stub returns beadID.
func stubBDRunner(t *testing.T, beadID string) {
	t.Helper()
	orig := bdRunner
	t.Cleanup(func() { bdRunner = orig })
	bdRunner = func(_ context.Context, _ []string, _ string) (string, error) {
		return beadID + "\n", nil
	}
}

// stubBDUpdateRunner replaces bdUpdateRunner for the duration of the test.
func stubBDUpdateRunner(t *testing.T) {
	t.Helper()
	orig := bdUpdateRunner
	t.Cleanup(func() { bdUpdateRunner = orig })
	bdUpdateRunner = func(_ context.Context, _ string, _ []string) error {
		return nil
	}
}

// ---- Full lifecycle: issue → bead → dispatch → PR → merge → close ----------

// TestIntegration_FullLifecycle exercises the happy path end-to-end:
//
//  1. Trusted user opens issue.
//  2. Issue triaged → bead created, labels applied, comment posted.
//  3. Rocket reaction triggers dispatch confirmation.
//  4. PR is created and linked to the issue.
//  5. PR is merged → issue is auto-closed.
func TestIntegration_FullLifecycle(t *testing.T) {
	ctx := context.Background()

	stubBDRunner(t, "Forge-integ1")
	stubBDUpdateRunner(t)

	// Triage always returns create_bead (trusted users do too via direct path,
	// but we inject a matching triageFunc to avoid the AI provider call).
	m, mock, db := newIntegrationMonitor(t, func(_ context.Context, issue Issue, _ []Comment, _ TriageConfig) TriageDecision {
		return TriageDecision{
			Action:          ActionCreateBead,
			Reason:          "clear feature request",
			BeadTitle:       issue.Title,
			BeadDescription: issue.Body,
		}
	})

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketRepos:        []string{"org/repo"},
	}

	issue := Issue{
		Number:    1,
		Repo:      "org/repo",
		Title:     "Support dark mode",
		Body:      "Please add a dark mode toggle to the settings page.",
		Author:    "alice",
		CreatedAt: time.Now(),
	}

	// ── Step 1: Triage → bead created ────────────────────────────────────────
	mock.OnListComments = func(_ context.Context, _ string, _ int) ([]Comment, error) {
		return nil, nil
	}
	m.triageIssue(ctx, "test-anvil", issue, anvilCfg, settings)

	wi, err := db.GetWicketIssue("org/repo", 1)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, StateBeadCreated, wi.State)
	assert.Equal(t, "Forge-integ1", wi.BeadID)

	// A bead-created comment and labels should have been applied.
	require.Len(t, mock.CommentCalls, 1, "expected one comment after triage")
	assert.Contains(t, mock.CommentCalls[0].Body, "Forge-integ1")
	require.Len(t, mock.AddLabelCalls, 1, "expected one AddLabels call")
	assert.Contains(t, mock.AddLabelCalls[0].Labels, settings.WicketProcessedLabel)
	assert.Contains(t, mock.AddLabelCalls[0].Labels, settings.WicketBeadCreatedLabel)

	// ── Step 2: Dispatch confirmation via rocket reaction ─────────────────────
	mock.OnListReactions = func(_ context.Context, _ string, _ int) ([]Reaction, error) {
		return []Reaction{{Content: "rocket", User: "alice"}}, nil
	}

	// Reset call counters so we only count calls from this step onwards.
	mock.CommentCalls = nil
	mock.AddLabelCalls = nil

	m.checkDispatch(ctx, "test-anvil", anvilCfg, settings)

	wi, err = db.GetWicketIssue("org/repo", 1)
	require.NoError(t, err)
	assert.Equal(t, StateDispatched, wi.State)

	// ── Step 3: PR created → linked to issue ──────────────────────────────────
	mock.CommentCalls = nil

	m.HandlePRCreated(ctx, "Forge-integ1", "https://github.com/org/repo/pull/10", 10)

	wi, err = db.GetWicketIssue("org/repo", 1)
	require.NoError(t, err)
	assert.Equal(t, StatePRCreated, wi.State)
	assert.Equal(t, 10, wi.PRNumber)
	require.Len(t, mock.CommentCalls, 1, "expected PR-created comment on issue")

	// ── Step 4: PR merged → issue auto-closed ─────────────────────────────────
	mock.CommentCalls = nil
	mock.CloseCalls = nil

	m.HandlePRMerged(ctx, "Forge-integ1", "https://github.com/org/repo/pull/10", "main", 10)

	wi, err = db.GetWicketIssue("org/repo", 1)
	require.NoError(t, err)
	assert.Equal(t, StateMerged, wi.State)

	require.Len(t, mock.CloseCalls, 1, "expected issue close on PR merge")
	assert.Equal(t, 1, mock.CloseCalls[0].Number)
	require.Len(t, mock.CommentCalls, 1, "expected merged comment on issue")
}

// ---- Duplicate detection ----------------------------------------------------

// TestIntegration_DuplicateDetection verifies that when the AI returns
// ActionDuplicate, the issue gets a duplicate comment, the processed label is
// applied, and state is "rejected" (duplicates are recorded as rejected).
func TestIntegration_DuplicateDetection(t *testing.T) {
	ctx := context.Background()
	stubBDRunner(t, "Forge-dup1")
	stubBDUpdateRunner(t)

	duplicateDecision := TriageDecision{
		Action:      ActionDuplicate,
		Reason:      "matches existing bead Forge-existing1",
		DuplicateID: "Forge-existing1",
	}

	m, mock, db := newIntegrationMonitor(t, func(_ context.Context, _ Issue, _ []Comment, _ TriageConfig) TriageDecision {
		return duplicateDecision
	})

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketRepos:        []string{"org/repo"},
	}

	issue := Issue{
		Number:    2,
		Repo:      "org/repo",
		Title:     "Support dark mode",
		Body:      "We need dark mode.",
		Author:    "alice",
		CreatedAt: time.Now(),
	}

	mock.OnListComments = func(_ context.Context, _ string, _ int) ([]Comment, error) {
		return nil, nil
	}

	m.triageIssue(ctx, "test-anvil", issue, anvilCfg, settings)

	// The issue should be recorded as rejected in the DB.
	wi, err := db.GetWicketIssue("org/repo", 2)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, StateRejected, wi.State)

	// A duplicate comment should have been posted.
	require.Len(t, mock.CommentCalls, 1)
	assert.Contains(t, mock.CommentCalls[0].Body, "Forge-existing1")

	// The processed label should have been applied.
	require.Len(t, mock.AddLabelCalls, 1)
	assert.Contains(t, mock.AddLabelCalls[0].Labels, settings.WicketProcessedLabel)
}

// ---- Already-fixed detection ------------------------------------------------

// TestIntegration_AlreadyFixedDetection verifies that when the AI returns
// ActionAlreadyFixed, the issue gets an already-fixed comment referencing the
// PR, the processed label is applied, and state is "rejected".
func TestIntegration_AlreadyFixedDetection(t *testing.T) {
	ctx := context.Background()
	stubBDRunner(t, "Forge-af1")
	stubBDUpdateRunner(t)

	alreadyFixedDecision := TriageDecision{
		Action:      ActionAlreadyFixed,
		Reason:      "resolved in PR #55",
		ReferencePR: "https://github.com/org/repo/pull/55",
	}

	m, mock, db := newIntegrationMonitor(t, func(_ context.Context, _ Issue, _ []Comment, _ TriageConfig) TriageDecision {
		return alreadyFixedDecision
	})

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"bob"},
		WicketRepos:        []string{"org/repo"},
	}

	issue := Issue{
		Number:    3,
		Repo:      "org/repo",
		Title:     "Login button missing",
		Body:      "The login button disappeared after the last update.",
		Author:    "bob",
		CreatedAt: time.Now(),
	}

	mock.OnListComments = func(_ context.Context, _ string, _ int) ([]Comment, error) {
		return nil, nil
	}

	m.triageIssue(ctx, "test-anvil", issue, anvilCfg, settings)

	wi, err := db.GetWicketIssue("org/repo", 3)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, StateRejected, wi.State)

	require.Len(t, mock.CommentCalls, 1)
	assert.Contains(t, mock.CommentCalls[0].Body, "pull/55")

	require.Len(t, mock.AddLabelCalls, 1)
	assert.Contains(t, mock.AddLabelCalls[0].Labels, settings.WicketProcessedLabel)
}

// ---- Out-of-scope with custom triage prompt ---------------------------------

// TestIntegration_OutOfScope_WithCustomPrompt verifies that when the triage
// prompt includes project-specific context and the AI returns ActionOutOfScope,
// the issue receives an out-of-scope comment with the AI reasoning.
func TestIntegration_OutOfScope_WithCustomPrompt(t *testing.T) {
	ctx := context.Background()
	stubBDRunner(t, "Forge-oos1")
	stubBDUpdateRunner(t)

	var capturedTriageCfg TriageConfig
	outOfScopeDecision := TriageDecision{
		Action: ActionOutOfScope,
		Reason: "this project handles backend services only; UI features are out of scope",
	}

	m, mock, db := newIntegrationMonitor(t, func(_ context.Context, _ Issue, _ []Comment, cfg TriageConfig) TriageDecision {
		capturedTriageCfg = cfg
		return outOfScopeDecision
	})

	customPrompt := "This project handles backend API services only. Frontend and UI concerns are out of scope."
	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers:  []string{"charlie"},
		WicketRepos:         []string{"org/repo"},
		WicketTriagePrompt:  customPrompt,
	}

	issue := Issue{
		Number:    4,
		Repo:      "org/repo",
		Title:     "Add a nicer UI for the settings page",
		Body:      "The settings page looks outdated. Please redesign the UI.",
		Author:    "charlie",
		CreatedAt: time.Now(),
	}

	mock.OnListComments = func(_ context.Context, _ string, _ int) ([]Comment, error) {
		return nil, nil
	}

	m.triageIssue(ctx, "test-anvil", issue, anvilCfg, settings)

	// Verify the custom triage prompt was forwarded to the triage function.
	assert.Equal(t, customPrompt, capturedTriageCfg.ExtraPrompt, "custom triage prompt should be passed to triage function")

	wi, err := db.GetWicketIssue("org/repo", 4)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, StateRejected, wi.State)

	// An out-of-scope comment should have been posted.
	require.Len(t, mock.CommentCalls, 1)
	assert.Contains(t, mock.CommentCalls[0].Body, "scope")

	// The processed label should be applied.
	require.Len(t, mock.AddLabelCalls, 1)
	assert.Contains(t, mock.AddLabelCalls[0].Labels, settings.WicketProcessedLabel)
}

// ---- Rate limiting behavior -------------------------------------------------

// TestIntegration_RateLimiting_BackoffPreventsScanning verifies that when the
// rate limiter is in backoff state, scanAnvil returns rateLimited=true and
// skips listing issues entirely, protecting the remaining quota.
func TestIntegration_RateLimiting_BackoffPreventsScanning(t *testing.T) {
	ctx := context.Background()

	m, mock, _ := newIntegrationMonitor(t, nil)

	// Manually activate the rate-limit backoff so the next scan is skipped.
	_ = m.rl.RecordRateLimitHit()

	listCalled := false
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		listCalled = true
		return nil, nil
	}

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{WicketRepos: []string{"org/repo"}}
	// The backoff check lives in scanAnvil, not scanRepo, so test at the right level.
	rateLimited := m.scanAnvil(ctx, "test-anvil", anvilCfg, settings)

	assert.True(t, rateLimited, "scanAnvil should report rate-limited when backoff is active")
	assert.False(t, listCalled, "ListIssues should not be called during backoff")
}

// TestIntegration_RateLimiting_ListIssuesError_TriggersBackoff verifies that
// when ListIssues returns a rate-limit error, the rate limiter enters backoff
// state and scanRepo returns rateLimited=true.
func TestIntegration_RateLimiting_ListIssuesError_TriggersBackoff(t *testing.T) {
	ctx := context.Background()

	m, mock, _ := newIntegrationMonitor(t, nil)

	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return nil, &RateLimitError{
			Message:   "API rate limit exceeded",
			Remaining: 0,
			ResetAt:   time.Now().Add(10 * time.Minute),
		}
	}

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{WicketRepos: []string{"org/repo"}}
	rateLimited := m.scanRepo(ctx, "test-anvil", "org/repo", anvilCfg, settings)

	assert.True(t, rateLimited, "scanRepo should return rate-limited=true on RateLimitError")
	assert.True(t, m.rl.IsLimited(), "rate limiter should be in backoff state after rate-limit error")
}

// TestIntegration_RateLimiting_RecordSuccess_ClearsBackoff verifies that after
// a successful API call the rate limiter exits backoff mode.
func TestIntegration_RateLimiting_RecordSuccess_ClearsBackoff(t *testing.T) {
	ctx := context.Background()

	m, mock, _ := newIntegrationMonitor(t, nil)

	// First hit: enter backoff.
	_ = m.rl.RecordRateLimitHit()
	require.True(t, m.rl.IsLimited(), "precondition: rate limiter should be limited after hit")

	// Clear backoff manually (simulating what scanRepo does after a successful call).
	m.rl.RecordSuccess()

	assert.False(t, m.rl.IsLimited(), "rate limiter should not be limited after RecordSuccess")

	// A normal scan should now proceed without issue.
	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return []Issue{}, nil
	}
	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{WicketRepos: []string{"org/repo"}}
	rateLimited := m.scanAnvil(ctx, "test-anvil", anvilCfg, settings)
	assert.False(t, rateLimited, "scan should succeed after backoff is cleared")
}

// ---- Double-processing guard ------------------------------------------------

// TestIntegration_NoDuplicateProcessing verifies that when the same issue is
// encountered on a second scan cycle it is skipped — no second bead or comment.
func TestIntegration_NoDuplicateProcessing(t *testing.T) {
	ctx := context.Background()
	stubBDRunner(t, "Forge-dup-guard")
	stubBDUpdateRunner(t)

	m, mock, db := newIntegrationMonitor(t, func(_ context.Context, issue Issue, _ []Comment, _ TriageConfig) TriageDecision {
		return TriageDecision{
			Action:          ActionCreateBead,
			Reason:          "valid request",
			BeadTitle:       issue.Title,
			BeadDescription: issue.Body,
		}
	})

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketRepos:        []string{"org/repo"},
	}

	issue := Issue{
		Number:    5,
		Repo:      "org/repo",
		Title:     "Add export feature",
		Body:      "We need CSV export.",
		Author:    "alice",
		CreatedAt: time.Now(),
	}

	mock.OnListComments = func(_ context.Context, _ string, _ int) ([]Comment, error) {
		return nil, nil
	}

	// First scan: issue is triaged.
	m.triageIssue(ctx, "test-anvil", issue, anvilCfg, settings)

	wi, err := db.GetWicketIssue("org/repo", 5)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, StateBeadCreated, wi.State)
	commentCountAfterFirstScan := len(mock.CommentCalls)
	require.Greater(t, commentCountAfterFirstScan, 0, "expected at least one comment after first triage")

	// Second scan: same issue is skipped because it is already tracked in DB.
	mock.CommentCalls = nil
	mock.AddLabelCalls = nil
	m.triageIssue(ctx, "test-anvil", issue, anvilCfg, settings)

	// No additional comments or labels should have been applied.
	assert.Empty(t, mock.CommentCalls, "no comment expected on second scan of same issue")
	assert.Empty(t, mock.AddLabelCalls, "no label call expected on second scan of same issue")
}

// ---- scanRepo integration ---------------------------------------------------

// TestIntegration_ScanRepo_FullTriage exercises the scanRepo method end-to-end:
// ListIssues returns a realistic list, the trusted user path creates a bead,
// and the non-trusted user path flags for human review.
func TestIntegration_ScanRepo_FullTriage(t *testing.T) {
	ctx := context.Background()
	stubBDRunner(t, "Forge-scan1")
	stubBDUpdateRunner(t)

	m, mock, db := newIntegrationMonitor(t, func(_ context.Context, issue Issue, _ []Comment, _ TriageConfig) TriageDecision {
		return TriageDecision{
			Action:          ActionCreateBead,
			Reason:          "valid request",
			BeadTitle:       issue.Title,
			BeadDescription: issue.Body,
		}
	})

	trustedIssue := Issue{
		Number:    10,
		Repo:      "org/repo",
		Title:     "Improve logging",
		Body:      "Add structured logging support.",
		Author:    "alice", // trusted
		CreatedAt: time.Now(),
	}
	untrustedIssue := Issue{
		Number:    11,
		Repo:      "org/repo",
		Title:     "What about adding feature X?",
		Body:      "Feature X would be nice.",
		Author:    "external-contributor", // not trusted → flag_human path
		CreatedAt: time.Now(),
	}

	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return []Issue{trustedIssue, untrustedIssue}, nil
	}
	mock.OnListComments = func(_ context.Context, _ string, _ int) ([]Comment, error) {
		return nil, nil
	}

	settings := defaultSettings()
	anvilCfg := config.AnvilConfig{
		WicketTrustedUsers: []string{"alice"},
		WicketRepos:        []string{"org/repo"},
	}

	rateLimited := m.scanRepo(ctx, "test-anvil", "org/repo", anvilCfg, settings)
	assert.False(t, rateLimited, "scan should not be rate-limited")

	// Trusted issue → bead created.
	wi, err := db.GetWicketIssue("org/repo", 10)
	require.NoError(t, err)
	require.NotNil(t, wi)
	assert.Equal(t, StateBeadCreated, wi.State)

	// Non-trusted issue → needs_human.
	wi2, err := db.GetWicketIssue("org/repo", 11)
	require.NoError(t, err)
	require.NotNil(t, wi2)
	assert.Equal(t, StateNeedsHuman, wi2.State)
}

// ---- PR lifecycle (PR created → PR merged) ----------------------------------

// TestIntegration_PRLifecycle_CreateThenMerge verifies the PR lifecycle hooks
// in sequence without going through scanRepo first.
func TestIntegration_PRLifecycle_CreateThenMerge(t *testing.T) {
	ctx := context.Background()

	m, mock, db := newIntegrationMonitor(t, nil)

	// Seed an issue in bead_created state (as if triage already ran).
	wi := state.WicketIssue{
		Repo:        "org/repo",
		IssueNumber: 20,
		Title:       "Support webhooks",
		Author:      "dave",
		State:       StateBeadCreated,
		BeadID:      "Forge-pr-lc",
	}
	require.NoError(t, db.InsertWicketIssue(wi))

	// PR created.
	m.HandlePRCreated(ctx, "Forge-pr-lc", "https://github.com/org/repo/pull/30", 30)

	updated, err := db.GetWicketIssue("org/repo", 20)
	require.NoError(t, err)
	assert.Equal(t, StatePRCreated, updated.State)
	assert.Equal(t, 30, updated.PRNumber)

	require.Len(t, mock.CommentCalls, 1, "expected PR-created comment")

	// PR merged.
	mock.CommentCalls = nil
	mock.CloseCalls = nil
	m.HandlePRMerged(ctx, "Forge-pr-lc", "https://github.com/org/repo/pull/30", "main", 30)

	updated, err = db.GetWicketIssue("org/repo", 20)
	require.NoError(t, err)
	assert.Equal(t, StateMerged, updated.State)

	require.Len(t, mock.CloseCalls, 1, "expected issue close on merge")
	assert.Equal(t, 20, mock.CloseCalls[0].Number)

	require.Len(t, mock.CommentCalls, 1, "expected merged comment")
	assert.Contains(t, mock.CommentCalls[0].Body, "merged")
}
