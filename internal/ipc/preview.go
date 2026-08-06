package ipc

import "time"

// PreviewActionPayload is the payload for the "preview_start" and
// "preview_stop" commands.
//
// Anvil is required to start (the daemon resolves the anvil's main checkout to
// read the preview manifest from) and ignored on stop, where the bead id alone
// identifies the preview. Branch is optional: an empty value means the bead's
// canonical forge branch.
type PreviewActionPayload struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// PreviewServiceInfo is one supervised service of a running preview, as
// reported by the "previews" command.
type PreviewServiceInfo struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	// Health is one of the state.PreviewService* values
	// (starting/healthy/failed).
	Health string `json:"health"`
	// Entry marks the service whose URL is *the* preview link.
	Entry bool `json:"entry,omitempty"`
	// Error explains a failed service (spawn error, health timeout, early exit).
	Error string `json:"error,omitempty"`
}

// PreviewInfo is one running preview environment.
type PreviewInfo struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Status is one of the state.Preview* values
	// (starting/running/degraded/failed/stopped).
	Status       string               `json:"status"`
	Services     []PreviewServiceInfo `json:"services,omitempty"`
	CreatedAt    time.Time            `json:"created_at"`
	LastActiveAt time.Time            `json:"last_active_at"`
	// EntryURL is the link to the preview's entry service exactly as Kiln built
	// it — settings.preview_public_host when configured, else the bind host.
	// Empty when no entry service has a port yet.
	//
	// The web layer deliberately rebuilds this host-side from the browser's own
	// request (see internal/web.previewEntryURL); a CLI has no request to fall
	// back on, so it prints this.
	EntryURL string `json:"entry_url,omitempty"`
	// Port is the entry service's port — the one EntryURL points at — or the
	// first allocated port when no service is marked as the entry. 0 while
	// ports are still being allocated.
	Port int `json:"port,omitempty"`
	// IdleRemainingSeconds is how many seconds are left before the idle reaper
	// tears this preview down, counted from LastActiveAt. It is null when the
	// reaper is disabled (settings.preview_idle_timeout of 0), which is not the
	// same as 0 — that means the deadline has already passed and the next
	// reaper tick will collect it.
	IdleRemainingSeconds *int64 `json:"idle_remaining_seconds"`
	// ResourceNote is a short human summary of what this preview is holding
	// while it is up (supervised services and their ports), for a status column
	// that has room for one line.
	ResourceNote string `json:"resource_note,omitempty"`
}

// PreviewsResponse is the response payload for the "previews" command.
//
// Enabled reports whether the Kiln manager is running at all; when it is false
// Previews is empty and callers should render "previews are disabled" rather
// than "no previews are running". PublicHost is the raw
// settings.preview_public_host — empty means unconfigured, which is what lets
// the web layer fall back to the request's own Host header when building links.
type PreviewsResponse struct {
	Enabled    bool   `json:"enabled"`
	PublicHost string `json:"public_host,omitempty"`
	// IdleTimeoutSeconds is settings.preview_idle_timeout; 0 means the idle
	// reaper is disabled and previews have no deadline.
	IdleTimeoutSeconds int64 `json:"idle_timeout_seconds,omitempty"`
	// Anvils names every anvil a preview can actually be started for: previews
	// are enabled for it AND its main checkout declares a `.forge/preview.yaml`.
	// It is what lets a client gate a "Preview" affordance per row without a
	// probe request per bead — an anvil missing here would only ever answer a
	// start with "no preview manifest".
	Anvils []string `json:"anvils"`
	// QuestAnvils names the anvils that additionally opted into running their
	// E2E quests against a preview (`preview_quests: true`). It gates the "Run
	// quests" affordance the same way Anvils gates the Preview one — from the
	// list the dashboard already polls, rather than a probe per bead.
	QuestAnvils []string      `json:"quest_anvils"`
	Previews    []PreviewInfo `json:"previews"`
}

// PreviewListResponse is the response payload for the "preview_list" command.
//
// "preview_list" and "previews" are the same read served under two names — the
// CLI's `forge preview list` and the web dashboard's GET /api/previews — so
// this is an alias rather than a parallel struct: one payload shape, one place
// to change a field.
type PreviewListResponse = PreviewsResponse

// PreviewStopResponse is the outcome of a "preview_stop" command.
//
// Teardown is queued (it kills process groups, runs the manifest's teardown
// command and removes a git worktree), so the caller first gets a "queued"
// response and resolves it through "request_status"; this is the payload the
// daemon completes that request with. Message is what the outcome tracker
// surfaces, so it always names the bead.
type PreviewStopResponse struct {
	Stopped bool   `json:"stopped"`
	BeadID  string `json:"bead_id"`
	Message string `json:"message,omitempty"`
}
