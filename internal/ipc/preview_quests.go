package ipc

import "time"

// Rejection reasons a "preview_quest_run" can answer with. They are codes
// rather than prose so a client can map them onto an HTTP status (and its own
// wording) without matching on a message that may be reworded.
const (
	// PreviewQuestRejectDisabled means Kiln is not running in this daemon.
	PreviewQuestRejectDisabled = "previews_disabled"
	// PreviewQuestRejectNoPreview means the bead has no live preview.
	PreviewQuestRejectNoPreview = "no_preview"
	// PreviewQuestRejectNotEnabled means the anvil did not opt in with
	// preview_quests.
	PreviewQuestRejectNotEnabled = "not_enabled"
	// PreviewQuestRejectNotHealthy means the preview exists but is starting,
	// degraded, failed or stopped.
	PreviewQuestRejectNotHealthy = "not_healthy"
	// PreviewQuestRejectUnavailable means QuestGiver is not wired up in this
	// daemon, so no run can be dispatched at all.
	PreviewQuestRejectUnavailable = "unavailable"
	// PreviewQuestRejectAlreadyRunning means a run for this bead is still in
	// flight; the caller should poll the run it already has.
	PreviewQuestRejectAlreadyRunning = "already_running"
	// PreviewQuestRejectNoEntryURL means the preview has no entry service URL
	// to point a browser at yet.
	PreviewQuestRejectNoEntryURL = "no_entry_url"
)

// PreviewQuestRunPayload is the payload for the "preview_quest_run" command:
// run this bead's anvil's E2E quests against the preview it already has
// running. There is no anvil field — the live preview names its own anvil, and
// letting a caller pass a different one could only ever be wrong.
type PreviewQuestRunPayload struct {
	BeadID string `json:"bead_id"`
}

// PreviewQuestStatusPayload is the payload for the "preview_quest_status"
// command. Exactly one of the two is used: RunID reads a specific run, BeadID
// reads that bead's most recent one (what a polling panel asks for, since it
// survives a page reload that lost the run id).
type PreviewQuestStatusPayload struct {
	RunID  string `json:"run_id,omitempty"`
	BeadID string `json:"bead_id,omitempty"`
}

// PreviewQuestOutcome is what one quest did during a run.
type PreviewQuestOutcome struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// FailedStep is the index of the step that failed, or -1 when none did.
	FailedStep      int     `json:"failed_step"`
	ErrorMessage    string  `json:"error_message,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	// FilePath is the quest file this outcome came from.
	FilePath string `json:"file_path,omitempty"`
	// Screenshots are filesystem paths to images the quest captured. They are
	// paths rather than URLs because the daemon has no notion of the HTTP
	// surface in front of it; the web layer maps them onto its own endpoint and
	// never forwards the paths to a browser.
	Screenshots []string `json:"screenshots,omitempty"`
}

// PreviewQuestRun is one preview quest run as reported over IPC.
//
// It is informational only. No pipeline stage, Bellows check or merge gate
// reads it: a quest failing against a preview tells an operator something about
// the branch, it does not block anything.
type PreviewQuestRun struct {
	RunID     string `json:"run_id"`
	BeadID    string `json:"bead_id"`
	Anvil     string `json:"anvil,omitempty"`
	PreviewID string `json:"preview_id,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// Status is one of the questgiver.Run* values:
	// running / passed / failed / skipped / error.
	Status string `json:"status"`
	// SkipReason explains a skipped run, Error an errored one.
	SkipReason      string                `json:"skip_reason,omitempty"`
	Error           string                `json:"error,omitempty"`
	StartedAt       time.Time             `json:"started_at"`
	FinishedAt      *time.Time            `json:"finished_at"`
	DurationSeconds float64               `json:"duration_seconds"`
	Quests          []PreviewQuestOutcome `json:"quests"`
}

// PreviewQuestRunResponse is the answer to "preview_quest_run".
//
// A rejected request is a successful command with Started=false and a Reason
// from the PreviewQuestReject* set, not an IPC error: "this anvil never opted
// in" is a gate, and a caller needs to tell it apart from "the daemon fell
// over" to render the right thing.
type PreviewQuestRunResponse struct {
	Started bool   `json:"started"`
	BeadID  string `json:"bead_id"`
	RunID   string `json:"run_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Run is the freshly-created run record, so a caller can render it without
	// a follow-up status call. Nil when the request was rejected.
	Run *PreviewQuestRun `json:"run,omitempty"`
}

// PreviewQuestStatusResponse is the answer to "preview_quest_status". Run is
// nil when no matching run exists (never started, or evicted from the daemon's
// bounded history), which a client renders as "no runs yet" rather than an
// error.
type PreviewQuestStatusResponse struct {
	Found bool             `json:"found"`
	Run   *PreviewQuestRun `json:"run,omitempty"`
}
