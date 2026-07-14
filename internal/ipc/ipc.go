// Package ipc provides inter-process communication between the Forge daemon
// and CLI/TUI clients via platform-native mechanisms (named pipe on Windows,
// Unix domain socket on Linux/macOS).
//
// Protocol: newline-delimited JSON messages.
//
// Server side (daemon):
//
//	svr := ipc.NewServer()
//	svr.OnCommand(func(cmd Command) Response { ... })
//	svr.Start(ctx)
//
// Client side (CLI/TUI):
//
//	client := ipc.NewClient()
//	resp, err := client.Send(Command{Type: "status"})
//	events := client.Subscribe(ctx) // stream events
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/provider"
)

// Command is a message sent from a client to the daemon.
type Command struct {
	Type    string          `json:"type"`    // "status", "kill_worker", "refresh", "queue", "run_bead", "create_pr", "set_clarification", "clear_clarification", "assay_rerun", "pause_dispatch", "resume_dispatch", "steer_bead", "pause_bead", "resume_bead", "resume_bead_with_message"
	Payload json.RawMessage `json:"payload"` // Type-specific data
	// ReadTimeout is an optional client-side timeout for reading the response.
	// Zero uses DefaultReadTimeout. Long-running commands that go through bd or
	// gh (e.g. run_bead) should set BdBackedReadTimeout. This field is not sent
	// on the wire — it only influences Client.Send's read deadline.
	ReadTimeout time.Duration `json:"-"`
}

// Per-command read-deadline presets applied by Client.Send.
const (
	// DefaultReadTimeout is used when a Command leaves ReadTimeout unset. It is
	// tuned for fast daemon-local handlers (status, view_logs, DB-only ops).
	DefaultReadTimeout = 3 * time.Second

	// BdBackedReadTimeout covers handlers that synchronously shell out to bd
	// (bd show / bd ready) before replying. bd against remote Dolt plus
	// GitHub auto-sync can take 20-30s per call; run_bead may chain multiple
	// such calls. Two minutes leaves headroom while still bounding a hung
	// daemon well under executil.DefaultBdTimeout (5 min).
	BdBackedReadTimeout = 2 * time.Minute
)

// defaultReadTimeout is the value applied when Command.ReadTimeout is zero.
// It mirrors DefaultReadTimeout at runtime but lives in a var so tests can
// override it directly to keep suite runtime bounded.
var defaultReadTimeout = DefaultReadTimeout

// Response is a message sent from the daemon to a client.
type Response struct {
	Type      string          `json:"type"`                 // "ok", "error", "status", "event", "queued"
	Payload   json.RawMessage `json:"payload"`              // Type-specific data
	RequestID string          `json:"request_id,omitempty"` // Set when Type is "queued"; correlates async completion
}

// Event is a push notification from daemon to subscribed clients.
type Event struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// StatusPayload is the response for a "status" command.
type StatusPayload struct {
	Running        bool                      `json:"running"`
	PID            int                       `json:"pid"`
	Uptime         string                    `json:"uptime"`
	Workers        int                       `json:"workers"`
	QueueSize      int                       `json:"queue_size"`
	OpenPRs        int                       `json:"open_prs"`
	LastPoll       string                    `json:"last_poll"`
	Quotas         map[string]provider.Quota `json:"quotas,omitempty"`
	DailyCost      float64                   `json:"daily_cost"`
	DailyCostLimit float64                   `json:"daily_cost_limit,omitempty"`
	// ReservedCost is the sum of estimated in-flight (not-yet-recorded) spend
	// for currently active workers. The daily_cost_limit gate projects
	// DailyCost + ReservedCost (+ one per-worker estimate) against the limit so
	// concurrent workers cannot overshoot it by ~N × per-bead cost (Forge-s3w7).
	ReservedCost float64 `json:"reserved_cost,omitempty"`
	// CostLimitPaused reports that cost-based auto-dispatch via the Poller is
	// currently paused due to hitting the daily cost limit. This accounts for
	// projected in-flight spend (DailyCost + ReservedCost), not just recorded
	// spend. Manual run_bead dispatch remains allowed while this flag is true.
	CostLimitPaused bool `json:"cost_limit_paused,omitempty"`
	// DispatchPaused reports that auto-dispatch is manually paused via the
	// pause_dispatch IPC command (forge pause / Hearth toggle). Running workers
	// keep going; only new dispatch is suspended. Manual run_bead dispatch
	// remains allowed. The flag is persisted in state.db so it survives daemon
	// restarts.
	DispatchPaused bool `json:"dispatch_paused,omitempty"`
	// PausedSince reports when the manual dispatch pause began. It is only set
	// when DispatchPaused is true (not for the cost-limit pause). Nil/omitted
	// when dispatch is not manually paused or the start time is unknown.
	PausedSince *time.Time `json:"paused_since,omitempty"`
	// CopilotPremiumRequests is the weighted count of Copilot premium requests used today.
	CopilotPremiumRequests float64 `json:"copilot_premium_requests,omitempty"`
	// CopilotRequestLimit is the configured daily limit (0 = no limit).
	CopilotRequestLimit int `json:"copilot_request_limit,omitempty"`
	// CopilotLimitReached is true when the copilot daily request limit has been reached.
	CopilotLimitReached bool `json:"copilot_limit_reached,omitempty"`
	// AnvilLastPoll carries the most recent poll outcome per anvil, populated
	// from the daemon's in-memory snapshot. Successful polls are no longer
	// persisted as events, so Hearth and `forge status` consume this field for
	// fresh per-anvil timestamps instead of querying the events table.
	AnvilLastPoll []AnvilPollItem `json:"anvil_last_poll,omitempty"`
	// MaxTotalSmiths is the configured global cap on concurrent Smith workers.
	// Hearth 2.0 uses this to size the "Idle" placeholder slots in the
	// Workers pane (max_total_smiths - active_workers).
	MaxTotalSmiths int `json:"max_total_smiths,omitempty"`
}

// AnvilPollItem reports the most recent poll outcome for a single anvil.
// It is carried inside StatusPayload.AnvilLastPoll and is the IPC-facing
// projection of the daemon's in-memory last-poll map.
type AnvilPollItem struct {
	Anvil     string    `json:"anvil"`
	Timestamp time.Time `json:"timestamp"`
	OK        bool      `json:"ok"`                // true when the last poll completed without error
	Message   string    `json:"message,omitempty"` // human-readable summary, e.g. "5 ready" or the error text
}

// KillWorkerPayload is the payload for a "kill_worker" command.
type KillWorkerPayload struct {
	WorkerID string `json:"worker_id"`
	PID      int    `json:"pid"`
}

// RunBeadPayload is the payload for a "run_bead" command.
type RunBeadPayload struct {
	BeadID   string `json:"bead_id"`
	Anvil    string `json:"anvil"`     // Optional: narrows search if multiple anvils have same bead ID
	ForceRun bool   `json:"force_run"` // When true, fetch via bd show (bypass bd ready), skip crucible/parent checks
}

// ClarificationPayload is the payload for "set_clarification" / "clear_clarification" commands.
type ClarificationPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`  // Required: which anvil the bead belongs to
	Reason string `json:"reason"` // Why clarification is needed (used when setting)
}

// RetryBeadPayload is the payload for a "retry_bead" command.
// Clears needs_human flag and resets retry count so the bead re-enters the queue.
// When PRID > 0, the retry targets an exhausted PR (resetting fix counters and
// status back to open) rather than the retries table.
type RetryBeadPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	PRID   int    `json:"pr_id,omitempty"`
}

// ClearBeadPayload is the payload for a "clear_bead" command.
// Clears the needs-attention flags from a bead's retry row without triggering
// a re-dispatch. Idempotent — succeeds even when the row is already clean or
// missing.
type ClearBeadPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
}

// DismissBeadPayload is the payload for a "dismiss_bead" command.
// Removes the bead from the Needs Attention list. When PRID > 0, the dismiss
// targets an exhausted PR (setting status to closed) rather than the retries table.
type DismissBeadPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	PRID   int    `json:"pr_id,omitempty"`
}

// ViewLogsPayload is the payload for a "view_logs" command.
type ViewLogsPayload struct {
	BeadID string `json:"bead_id"`
}

// ViewLogsResponse is the response payload for a "view_logs" command.
type ViewLogsResponse struct {
	LogPath   string   `json:"log_path"`
	LastLines []string `json:"last_lines"`
}

// MergePRPayload is the payload for a "merge_pr" command.
// Triggers a squash merge (or configured strategy) of a ready-to-merge PR.
type MergePRPayload struct {
	PRID     int    `json:"pr_id"`
	PRNumber int    `json:"pr_number"`
	Anvil    string `json:"anvil"`
}

// AppendNotesPayload is the payload for an "append_notes" command.
// Adds human notes to a bead's history via bd update.
type AppendNotesPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	Notes  string `json:"notes"`
}

// TagBeadPayload is the payload for a "tag_bead" command that adds the
// anvil's configured auto-dispatch label to a bead via bd update.
// The daemon derives the tag from its own config; the client only needs
// to supply the bead identity.
type TagBeadPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
}

// CloseBeadPayload is the payload for a "close_bead" command that closes
// a bead via bd close.
type CloseBeadPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
}

// UpdateLabelPayload is the payload for an "update_label" command that
// adds or removes an arbitrary label on a bead via bd update --add-label /
// --remove-label. Used by Hearth 2.0 to manage labels from the web UI.
type UpdateLabelPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	Label  string `json:"label"`
	Action string `json:"action"` // "add" | "remove"
}

// StopBeadPayload is the payload for a "stop_bead" command.
// Stops all processing of a bead: kills any running worker, marks the bead
// as needing clarification (so the poller skips it), and releases it back to
// open. The bead will not be dispatched again until unclarified.
type StopBeadPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	Reason string `json:"reason"` // Optional; defaults to "manually stopped"
}

// CreatePRPayload is the payload for a "create_pr" command. It triggers the
// manual create-PR-from-existing-branch recovery: the daemon opens a PR for the
// already-pushed forge/<bead> branch without re-running Smith. Used by
// `forge queue create-pr <id> --anvil <name>` and the Hearth "Create PR" button.
type CreatePRPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
}

// QueueActionPayload is the payload for the queue resolution verbs:
// "queue_clarify", "queue_unclarify", "queue_retry", "queue_clear", and
// "queue_stop". These are thin IPC wrappers around the shared queueactions
// package functions and accept a forge_id for multi-forge safety: when
// supplied the daemon rejects the request unless its local forge_id matches.
//
// AnvilName uses the legacy "anvil" JSON tag so existing callers can reuse
// the same field name they pass to set_clarification / stop_bead.
type QueueActionPayload struct {
	BeadID    string `json:"bead_id"`
	ForgeID   string `json:"forge_id,omitempty"`
	AnvilName string `json:"anvil_name,omitempty"`
	Note      string `json:"note,omitempty"`
}

// SteerBeadPayload is the payload for a "steer_bead" command. It delivers a
// human steering message to a bead's in-flight pipeline: the daemon pushes the
// message into the bead's control-handle steer mailbox and, when a Smith spawn
// is currently running, interrupts it so the message is consumed immediately
// (steer mode A); otherwise the message is picked up between spawns (mode B).
// The daemon rejects the command with an actionable error when the bead has no
// active pipeline, the message is empty, or the session is not a Claude session
// (only Claude reports a resumable session_id).
type SteerBeadPayload struct {
	BeadID  string `json:"bead_id"`
	Message string `json:"message"`
}

// PauseBeadPayload is the payload for a "pause_bead" command. It requests that a
// bead's in-flight pipeline park its currently running Claude spawn: the daemon
// validates the bead has an active pipeline whose worker is in the running state
// (the only state a pause is permitted from, per the paused-status transition
// table) and dispatches the pause request into the pipeline goroutine via the
// control handle. The spawn is gracefully interrupted and the goroutine parks
// awaiting a resume_bead; no failure is recorded. The daemon rejects the command
// when the bead has no active pipeline (not found) or its worker is not running
// (illegal transition).
//
// A paused bead's worker row and worktree survive a daemon restart (the parked
// goroutine does not). After a restart the bead is surfaced in Needs Attention
// and can be resumed (resume_bead cold-resumes it in place) or discarded
// (stop_bead / queue_stop). Pausing is only possible while a live pipeline is
// running, so pause_bead itself is not used after a restart — only resume/discard.
type PauseBeadPayload struct {
	BeadID string `json:"bead_id"`
}

// ResumeBeadPayload is the payload for a "resume_bead" command. It requests that
// a paused bead's pipeline resume, respawning `claude --resume <session>` with
// Message as the new prompt. Message is optional; when empty (or whitespace) the
// daemon substitutes the default "Continue with the task." prompt.
//
// Two cases are handled transparently:
//   - Warm resume: a live pipeline goroutine is still parked. The daemon
//     validates its worker is in the paused state and signals the goroutine
//     through the control handle to respawn and continue.
//   - Cold resume (after a daemon restart): the parked goroutine did not survive
//     the restart, but the paused worker row and worktree did. The daemon
//     reconstructs the resume state from the persisted session and re-dispatches
//     the bead into a fresh pipeline that resumes the recorded session in the
//     retained worktree.
//
// The daemon rejects the command when the bead has neither a live pipeline nor a
// paused worker row (not found), or when a live worker is not in the paused state
// (illegal transition).
type ResumeBeadPayload struct {
	BeadID  string `json:"bead_id"`
	Message string `json:"message,omitempty"`
}

// PauseBeadResponse is the response payload for a successful "pause_bead"
// command. Status is the worker status the bead is transitioning to ("paused");
// Message is a human-readable confirmation.
type PauseBeadResponse struct {
	BeadID  string `json:"bead_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ResumeBeadResponse is the response payload for a successful "resume_bead"
// command. Status is the worker status the bead is transitioning to ("running");
// Message is a human-readable confirmation.
type ResumeBeadResponse struct {
	BeadID  string `json:"bead_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ResumeBeadWithMessagePayload is the payload for a "resume_bead_with_message"
// command. Unlike "resume_bead" (which resumes a paused, still-parked pipeline),
// this verb resumes a needs-attention bead whose worktree was torn down but
// whose forge/<bead> branch survives: the daemon recreates the worktree from the
// surviving branch and resumes the recorded Claude session (falling back to a
// fresh session seeded with the operator message when the transcript or branch
// is gone). Like "steer_bead" it is keyed purely by bead id — the daemon
// resolves the resumable worker row from state — so no anvil is required.
//
// Message is the operator message the resumed (or fresh-fallback) session
// continues with. It is optional; when empty (or whitespace) the daemon
// substitutes DefaultResumeMessage. The daemon rejects the command when the
// bead already has a live pipeline (use "resume_bead"), has no resumable worker
// row (no recorded branch + session), or its resume preconditions are unmet.
type ResumeBeadWithMessagePayload struct {
	BeadID  string `json:"bead_id"`
	Message string `json:"message,omitempty"`
}

// ResumeBeadWithMessageResponse is the response payload for a successful
// "resume_bead_with_message" command. WorkerID is the reused worker id the
// resumed pipeline runs under; Message is a human-readable confirmation.
type ResumeBeadWithMessageResponse struct {
	BeadID   string `json:"bead_id"`
	WorkerID string `json:"worker_id"`
	Message  string `json:"message,omitempty"`
}

// ResolveOrphanPayload is the payload for a "resolve_orphan" command.
// Sent by Hearth to the daemon when the user picks an action for an orphaned bead.
// Action is one of "recover", "close", or "discard".
type ResolveOrphanPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	Action string `json:"action"` // "recover" | "close" | "discard"
}

// PRActionPayload is the payload for a "pr_action" command.
// Triggers an action on an open PR.
type PRActionPayload struct {
	PRID     int    `json:"pr_id"`
	PRNumber int    `json:"pr_number"`
	Anvil    string `json:"anvil"`
	BeadID   string `json:"bead_id"`
	Branch   string `json:"branch"`
	Action   string `json:"action"` // "open_browser" | "merge" | "quench" | "burnish" | "rebase" | "close" | "approve" | "assign_bellows" | "unassign_bellows"
}

// WardenRerunPayload is the payload for a "warden_rerun" command.
// Re-runs warden on the existing worktree branch. If warden approves,
// proceeds to PR creation normally.
type WardenRerunPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
}

// AssayRerunPayload is the payload for an "assay_rerun" command.
// Triggers a fresh Assay AI review pass over a PR's current head, bypassing the
// Bellows trigger gate's head-SHA debounce. PR is the state.db PR id (matching
// the {pr} path parameter the web rerun endpoint forwards, and the `pr` field
// the api.ts client posts); Anvil resolves the worktree/config for the
// re-review. The field shape mirrors api.ts RerunAssayParams so the web backend
// can forward the request without translation.
type AssayRerunPayload struct {
	Anvil string `json:"anvil"`
	PR    int    `json:"pr"`
}

// ApproveAsIsPayload is the payload for an "approve_as_is" command.
// Bypasses warden entirely and creates a PR from the current branch state.
type ApproveAsIsPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
}

// ForceSmithPayload is the payload for a "force_smith" command.
// Pushes smith into another iteration on the same branch, keeping
// existing warden feedback attached. UserNote is optionally prepended
// to the prompt.
type ForceSmithPayload struct {
	BeadID   string `json:"bead_id"`
	Anvil    string `json:"anvil"`
	UserNote string `json:"user_note,omitempty"`
}

// CrucibleActionPayload is the payload for a "crucible_action" command.
// Triggers a resume or stop action on a paused Crucible.
type CrucibleActionPayload struct {
	ParentID string `json:"parent_id"` // Parent bead ID of the Crucible
	Anvil    string `json:"anvil"`
	Action   string `json:"action"` // "resume" | "stop"
}

// CrucibleStatusItem represents an active Crucible's current state.
type CrucibleStatusItem struct {
	ParentID          string `json:"parent_id"`
	ParentTitle       string `json:"parent_title"`
	Anvil             string `json:"anvil"`
	Branch            string `json:"branch"`
	Phase             string `json:"phase"` // "started", "dispatching", "waiting", "final_pr", "complete", "paused"
	TotalChildren     int    `json:"total_children"`
	CompletedChildren int    `json:"completed_children"`
	CurrentChild      string `json:"current_child"`
	StartedAt         string `json:"started_at"`
}

// CruciblesResponse is the response payload for a "crucibles" command.
type CruciblesResponse struct {
	Crucibles []CrucibleStatusItem `json:"crucibles"`
}

// GetIngotsPayload is the payload for a "get_ingots" command.
type GetIngotsPayload struct {
	Anvil  string `json:"anvil,omitempty"`
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"` // default 50
}

// GetIngotPayload is the payload for a "get_ingot" command.
type GetIngotPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil,omitempty"`
}

// WicketStatusPayload is the response for a "wicket_status" command.
type WicketStatusPayload struct {
	Enabled        bool           `json:"enabled"`
	Interval       string         `json:"interval"`
	MonitoredRepos []string       `json:"monitored_repos"` // explicitly configured repos
	DerivedAnvils  int            `json:"derived_anvils"`  // anvil count deriving repo from git remote at runtime
	IssueCounts    map[string]int `json:"issue_counts"`    // state -> count
	LastScanAt     *time.Time     `json:"last_scan_at,omitempty"`
}

// QueuedPayload is the payload for a "queued" response, indicating the
// command was accepted and will be processed asynchronously. The request ID is
// carried only at the top-level Response.RequestID field; it is not duplicated
// here to avoid diverging sources of truth.
type QueuedPayload struct {
	Message string `json:"message,omitempty"`
}

// QueueItem is the IPC representation of a single cached queue entry. It
// mirrors state.QueueItem but uses JSON-friendly tags and a parsed labels
// slice. CreatedAt and UpdatedAt are sourced from the in-memory poller
// snapshot (not the queue_cache table) so timestamps flow through without
// a SQLite schema migration; both are empty strings when the snapshot has
// no matching entry (e.g. before the first poll completes).
type QueueItem struct {
	BeadID      string   `json:"bead_id"`
	Anvil       string   `json:"anvil"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Priority    int      `json:"priority"`
	Status      string   `json:"status"`
	Labels      []string `json:"labels"`
	Section     string   `json:"section"`
	Assignee    string   `json:"assignee,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	// AutoDispatchTag is the anvil's configured dispatch label (forge.yaml
	// `auto_dispatch_tag`). Surfaced on each queue row so the Hearth web UI
	// can render a one-click "apply tag" button on Unlabeled beads without
	// an extra round-trip to the anvil registry. Empty when the owning
	// anvil has no tag configured.
	AutoDispatchTag string `json:"auto_dispatch_tag,omitempty"`
}

// QueueResponse is the response payload for a "queue" command.
type QueueResponse struct {
	Items []QueueItem `json:"items"`
}

// WorkerInfo is the IPC representation of a single active worker.
// Kind is a coarse worker-class label derived from Phase: "bellows" for the
// PR-monitor pseudo-workers (no log, no log-modal affordance) and "smith"
// for everything else — pipeline Smiths plus the lifecycle sub-workers
// (quench/burnish/rebase) that produce real claude log files.
type WorkerInfo struct {
	ID          string `json:"id"`
	BeadID      string `json:"bead_id"`
	Anvil       string `json:"anvil"`
	Branch      string `json:"branch,omitempty"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
	Phase       string `json:"phase,omitempty"`
	Kind        string `json:"kind,omitempty"`
	PID         int    `json:"pid,omitempty"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at,omitempty"`
	LogPath     string `json:"log_path,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Model       string `json:"model,omitempty"`
}

// WorkerKindFromPhase returns the coarse worker-class label used by the
// Hearth web UI to decide whether a worker card opens a log modal. Bellows
// PR-monitor rows return "bellows" (no underlying claude log); everything
// else — pipeline Smiths and the quench/burnish/rebase lifecycle workers —
// returns "smith".
func WorkerKindFromPhase(phase string) string {
	if phase == "bellows" {
		return "bellows"
	}
	return "smith"
}

// WorkersResponse is the response payload for a "workers" command.
type WorkersResponse struct {
	Workers []WorkerInfo `json:"workers"`
}

// EventInfo is the IPC representation of a single event log entry.
type EventInfo struct {
	ID        int    `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	BeadID    string `json:"bead_id,omitempty"`
	Anvil     string `json:"anvil,omitempty"`
}

// EventsResponse is the response payload for an "events" command.
type EventsResponse struct {
	Events []EventInfo `json:"events"`
}

// CompletionResult is the outcome delivered when an async request finishes.
type CompletionResult struct {
	Response Response
	Err      error
}

// NewQueuedResponse builds a Response of type "queued" for the given request ID.
// Returns an error if requestID is empty, since an empty ID breaks correlation.
func NewQueuedResponse(requestID string, message string) (Response, error) {
	if requestID == "" {
		return Response{}, fmt.Errorf("requestID must not be empty")
	}
	payload := QueuedPayload{Message: message}
	data, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("marshal queued payload: %w", err)
	}
	return Response{
		Type:      "queued",
		Payload:   data,
		RequestID: requestID,
	}, nil
}

// RequestTracker maps request IDs to completion channels so the daemon can
// deliver results for asynchronously queued commands.
type RequestTracker struct {
	mu       sync.Mutex
	pending  map[string]chan CompletionResult
	idSeq    uint64
	idPrefix string
}

// NewRequestTracker creates a tracker. The prefix is prepended to generated
// request IDs (e.g. "forge-") for easy identification in logs.
func NewRequestTracker(prefix string) *RequestTracker {
	return &RequestTracker{
		pending:  make(map[string]chan CompletionResult),
		idPrefix: prefix,
	}
}

// Track registers a new async request and returns its ID and a channel that
// will receive exactly one CompletionResult when the work finishes.
func (rt *RequestTracker) Track() (string, <-chan CompletionResult) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.pending == nil {
		rt.pending = make(map[string]chan CompletionResult)
	}
	rt.idSeq++
	id := fmt.Sprintf("%s%d-%d", rt.idPrefix, time.Now().UnixMilli(), rt.idSeq)
	ch := make(chan CompletionResult, 1)
	rt.pending[id] = ch
	return id, ch
}

// Complete delivers a result for the given request ID and removes it from the
// tracker. Returns false if the ID is not found (already completed or unknown).
func (rt *RequestTracker) Complete(requestID string, result CompletionResult) bool {
	rt.mu.Lock()
	ch, ok := rt.pending[requestID]
	if ok {
		delete(rt.pending, requestID)
	}
	rt.mu.Unlock()
	if !ok {
		return false
	}
	ch <- result
	close(ch)
	return true
}

// Cancel removes a pending request without delivering a result, closing its
// channel so any waiter unblocks.
func (rt *RequestTracker) Cancel(requestID string) {
	rt.mu.Lock()
	ch, ok := rt.pending[requestID]
	if ok {
		delete(rt.pending, requestID)
	}
	rt.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Pending returns the number of in-flight requests.
func (rt *RequestTracker) Pending() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.pending)
}

// CommandHandler is called by the server for each incoming command.
type CommandHandler func(cmd Command) Response

// Server listens for IPC connections from CLI/TUI clients.
type Server struct {
	listener net.Listener
	handler  CommandHandler
	clients  map[net.Conn]bool
	// subscribers is the subset of clients that issued a "subscribe" command and
	// are draining the pushed event stream. Broadcast targets only these
	// long-lived connections so a high-volume event stream never interleaves an
	// unsolicited "event" line into a transient request/response connection
	// (e.g. `forge status`), which would corrupt that client's single-line read.
	subscribers map[net.Conn]bool
	mu          sync.RWMutex
}

// NewServer creates a new IPC server.
func NewServer() *Server {
	return &Server{
		clients:     make(map[net.Conn]bool),
		subscribers: make(map[net.Conn]bool),
	}
}

// OnCommand sets the handler for incoming commands.
func (s *Server) OnCommand(h CommandHandler) {
	s.handler = h
}

// Start begins listening for IPC connections. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	listener, err := listen()
	if err != nil {
		return fmt.Errorf("ipc listen: %w", err)
	}
	s.listener = listener

	// Close listener on context cancellation
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil // Normal shutdown
			default:
				return fmt.Errorf("ipc accept: %w", err)
			}
		}
		s.mu.Lock()
		s.clients[conn] = true
		s.mu.Unlock()

		go s.handleConn(ctx, conn)
	}
}

// Broadcast pushes an event to every subscribed client (those that issued a
// "subscribe" command). Non-subscriber connections are skipped so a pushed
// event never races the response of an in-flight request/response command.
func (s *Server) Broadcast(evt Event) {
	data, err := json.Marshal(Response{
		Type:    "event",
		Payload: mustMarshal(evt),
	})
	if err != nil {
		return
	}
	data = append(data, '\n')

	s.mu.RLock()
	defer s.mu.RUnlock()

	for conn := range s.subscribers {
		// Non-blocking write with short deadline
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write(data)
	}
}

// HasClients reports whether any IPC clients are currently connected.
func (s *Server) HasClients() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients) > 0
}

// Close shuts down the server and all client connections.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for conn := range s.clients {
		conn.Close()
		delete(s.clients, conn)
		delete(s.subscribers, conn)
	}

	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		delete(s.subscribers, conn)
		s.mu.Unlock()
		conn.Close()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB max message

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var cmd Command
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			resp := Response{
				Type:    "error",
				Payload: mustMarshal(map[string]string{"message": "invalid JSON"}),
			}
			writeResponse(conn, resp)
			continue
		}

		var resp Response
		if s.handler != nil {
			resp = s.handler(cmd)
		} else {
			resp = Response{
				Type:    "error",
				Payload: mustMarshal(map[string]string{"message": "no handler"}),
			}
		}
		writeResponse(conn, resp)

		// A "subscribe" command promotes this connection into the pushed-event
		// fan-out set. Registration is deferred until after the ack response is
		// written so a concurrent Broadcast cannot slip an event line ahead of
		// the ack (which the subscribing client reads as a single response). The
		// connection then keeps blocking in scanner.Scan(), never sending another
		// command, while Broadcast writes events to it.
		if cmd.Type == "subscribe" {
			s.mu.Lock()
			s.subscribers[conn] = true
			s.mu.Unlock()
		}
	}
}

// Client connects to the daemon's IPC socket.
type Client struct {
	conn net.Conn
}

// NewClient creates a new IPC client connected to the daemon.
func NewClient() (*Client, error) {
	conn, err := dial()
	if err != nil {
		return nil, fmt.Errorf("ipc connect: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Send sends a command and waits for a response.
func (c *Client) Send(cmd Command) (*Response, error) {
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshaling command: %w", err)
	}
	data = append(data, '\n')

	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}

	readTimeout := cmd.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		return nil, fmt.Errorf("no response from daemon")
	}

	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &resp, nil
}

// Subscribe returns a channel that receives events from the daemon.
// The channel is closed when ctx is cancelled or the connection drops.
func (c *Client) Subscribe(ctx context.Context) <-chan Event {
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)

		// Send subscribe command
		_, _ = c.Send(Command{Type: "subscribe"})

		// Clear the read deadline set by Send so the event stream can
		// block indefinitely waiting for the next pushed event.
		_ = c.conn.SetReadDeadline(time.Time{})

		// When ctx is cancelled, close the connection to unblock scanner.Scan().
		// Without this, the goroutine would only check ctx.Done() after a line
		// is read, which may never happen if no events arrive.
		go func() {
			<-ctx.Done()
			c.conn.Close()
		}()

		scanner := bufio.NewScanner(c.conn)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var resp Response
			if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
				continue
			}
			if resp.Type != "event" {
				continue
			}

			var evt Event
			if err := json.Unmarshal(resp.Payload, &evt); err != nil {
				continue
			}

			select {
			case ch <- evt:
			default:
				// Drop event if channel full
			}
		}
	}()
	return ch
}

// IsQueued reports whether the response indicates the command was accepted
// for asynchronous processing. Callers can use resp.RequestID to correlate
// the eventual completion.
func (r *Response) IsQueued() bool {
	return r != nil && r.Type == "queued"
}

// Close closes the client connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// PingTimeout bounds the liveness round-trip so a dead or absent socket cannot
// stall a CLI command. The daemon answers "ping" synchronously without any DB
// access or bd/gh shell-out, and each connection is served on its own
// goroutine, so a short timeout is safe even when the daemon is busy.
const PingTimeout = 2 * time.Second

// Ping performs a lightweight liveness round-trip against the daemon's IPC
// socket. It returns true only when the socket dials successfully AND the
// daemon answers the ping. This is the authoritative liveness signal for
// commands like `forge status`, `forge pause`, and `forge resume`: a daemon
// that answers is running even if its pidfile is missing or stale.
func Ping() bool {
	client, err := NewClient()
	if err != nil {
		return false
	}
	defer client.Close()
	resp, err := client.Send(Command{Type: "ping", ReadTimeout: PingTimeout})
	if err != nil {
		return false
	}
	return resp != nil && resp.Type == "pong"
}

// --- Helpers ---

func writeResponse(conn net.Conn, resp Response) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(data)
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
