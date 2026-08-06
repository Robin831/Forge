package ipc

// Dispatch pause reasons carried in StatusPayload.DispatchPauseReason. The set
// is deliberately open: a client that meets an unknown reason must render it
// verbatim rather than mislabel it, so new causes can be added daemon-side
// without breaking older clients. An empty reason on a paused daemon means the
// daemon predates this field and is treated as a manual pause.
const (
	// PauseReasonManual is an operator pause (forge pause / Hearth toggle).
	PauseReasonManual = "manual"
	// PauseReasonSelfDeploy is the transient pause a self-deploy takes while it
	// drains active workers before rebuilding and restarting the daemon.
	PauseReasonSelfDeploy = "self-deploy"
)

// FormatDispatchPause renders the human-facing dispatch-pause line shared by
// `forge status` and the Hearth TUI. It returns "" when dispatch is not paused.
//
// detail is optional supplementary context supplied by the daemon (for a
// self-deploy: "waiting on 2 workers, max 30m"); it is appended inside the
// parentheses when present. An unknown reason is printed verbatim so a newer
// daemon talking to an older client still says something true.
func FormatDispatchPause(paused bool, reason, detail string) string {
	if !paused {
		return ""
	}
	var cause string
	switch reason {
	case PauseReasonSelfDeploy:
		cause = "self-deploy drain"
	case PauseReasonManual, "":
		cause = "manual"
	default:
		cause = reason
	}
	if detail != "" {
		cause += ", " + detail
	}
	return "PAUSED (" + cause + ") — running workers continue"
}
