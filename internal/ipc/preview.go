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
	IdleTimeoutSeconds int64         `json:"idle_timeout_seconds,omitempty"`
	Previews           []PreviewInfo `json:"previews"`
}
