package web

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// The types below are the frontend-facing contract for Kiln previews. They are
// deliberately kept together in one file so the SPA can mirror them field for
// field; change a JSON tag here and the preview panel changes with it.

// PreviewServiceStatus is one supervised service of a running preview.
type PreviewServiceStatus struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	// Health is one of starting / healthy / failed.
	Health string `json:"health"`
	// Entry marks the service whose URL is *the* preview link.
	Entry bool `json:"entry"`
	// UptimeSeconds is how long the service has been up. Services are spawned
	// together and per-process start times are not tracked, so it is measured
	// from when the preview started; a failed service reports 0.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// LogURL is the GET endpoint tailing this service's log.
	LogURL string `json:"log_url"`
	// Error explains a failed service (spawn error, health timeout, early exit).
	Error string `json:"error,omitempty"`
}

// PreviewSummary is one running preview environment.
type PreviewSummary struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Status is one of starting / running / degraded / failed / stopped.
	Status   string                 `json:"status"`
	Services []PreviewServiceStatus `json:"services"`
	// EntryURL is the link an operator opens, or "" when no entry service has a
	// port yet. See previewEntryURL for how its host is chosen.
	EntryURL     string    `json:"entry_url"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	// IdleDeadline is when the reaper will tear this preview down unless it is
	// touched again. null when the idle reaper is disabled.
	IdleDeadline *time.Time `json:"idle_deadline"`
}

// PreviewsListResponse is the body of GET /api/previews. Enabled is false when
// the daemon is running without a Kiln manager, which the SPA renders as
// "previews are disabled" rather than "none are running".
//
// Anvils names the anvils a preview can be started for (previews enabled AND a
// `.forge/preview.yaml` in their main checkout). The SPA gates its per-bead
// Preview affordance on it, which is why it rides along with the list the
// dashboard already polls instead of being a flag on every bead/PR payload.
// QuestAnvils names the anvils that additionally opted into running their E2E
// quests against a preview (`preview_quests`). It gates the "Run quests" action
// the same way Anvils gates the Preview one.
type PreviewsListResponse struct {
	Enabled     bool             `json:"enabled"`
	Anvils      []string         `json:"anvils"`
	QuestAnvils []string         `json:"quest_anvils"`
	Previews    []PreviewSummary `json:"previews"`
}

// validPreviewService restricts a service name to the manifest's own character
// set. It gates the {service} path segment before it is turned into a filename,
// so a traversal attempt never reaches the filesystem.
var validPreviewService = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// handlePreviewStart proxies POST /api/bead/{bead_id}/preview/start to the
// daemon's preview_start IPC. The anvil is required: the daemon reads the
// preview manifest from that anvil's main checkout. Starting is slow (worktree,
// setup script, service spawn, health checks) so the daemon queues it and the
// response is a 202 carrying the request_id + poll_url the SPA resolves through
// GET /api/requests/{request_id}.
func (s *Server) handlePreviewStart(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	s.logActor(r, "preview_start", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "preview_start", ipc.PreviewActionPayload{
		BeadID: beadID,
		Anvil:  req.Anvil,
	})
}

// handlePreviewStop proxies POST /api/bead/{bead_id}/preview/stop to the
// daemon's preview_stop IPC. Unlike start it needs no anvil — the bead id alone
// identifies the preview in the manager's registry — so the body may be empty.
// Teardown is queued for the same reason a start is.
func (s *Server) handlePreviewStop(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	req, err := decodeActionRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	s.logActor(r, "preview_stop", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "preview_stop", ipc.PreviewActionPayload{
		BeadID: beadID,
		Anvil:  req.Anvil,
	})
}

// handlePreviewsList serves GET /api/previews: every live preview with its
// per-service ports and health, an entry URL and the idle deadline. The list is
// always a JSON array (never null) so the SPA can render it without a nil check.
func (s *Server) handlePreviewsList(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("previews")
	if resp.Type != "ok" && resp.Type != "status" {
		// error / unexpected: writeIPCResponse renders the daemon's message.
		s.writeIPCResponse(w, resp)
		return
	}
	var payload ipc.PreviewsResponse
	if len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, &payload); err != nil {
			writeError(w, http.StatusInternalServerError, "invalid previews payload")
			return
		}
	}

	idle := time.Duration(payload.IdleTimeoutSeconds) * time.Second
	now := time.Now()
	out := PreviewsListResponse{
		Enabled:     payload.Enabled,
		Anvils:      payload.Anvils,
		QuestAnvils: payload.QuestAnvils,
		Previews:    make([]PreviewSummary, 0, len(payload.Previews)),
	}
	if out.Anvils == nil {
		out.Anvils = []string{}
	}
	if out.QuestAnvils == nil {
		out.QuestAnvils = []string{}
	}
	for _, p := range payload.Previews {
		out.Previews = append(out.Previews, previewSummary(r, p, payload.PublicHost, idle, now))
	}
	writeJSON(w, http.StatusOK, out)
}

// previewSummary maps one IPC preview record onto the frontend DTO, filling in
// the two things only the HTTP layer can know: the log tail URL for each
// service and the host the entry link should point at.
func previewSummary(r *http.Request, p ipc.PreviewInfo, publicHost string, idle time.Duration, now time.Time) PreviewSummary {
	summary := PreviewSummary{
		BeadID:       p.BeadID,
		Anvil:        p.Anvil,
		Branch:       p.Branch,
		Status:       p.Status,
		Services:     make([]PreviewServiceStatus, 0, len(p.Services)),
		EntryURL:     previewEntryURL(r, p, publicHost),
		CreatedAt:    p.CreatedAt,
		LastActiveAt: p.LastActiveAt,
	}
	if idle > 0 && !p.LastActiveAt.IsZero() {
		deadline := p.LastActiveAt.Add(idle)
		summary.IdleDeadline = &deadline
	}
	for _, svc := range p.Services {
		summary.Services = append(summary.Services, PreviewServiceStatus{
			Name:          svc.Name,
			Port:          svc.Port,
			Health:        svc.Health,
			Entry:         svc.Entry,
			UptimeSeconds: serviceUptimeSeconds(svc, p.CreatedAt, now),
			LogURL:        previewLogPath(p.BeadID, svc.Name),
			Error:         svc.Error,
		})
	}
	return summary
}

// serviceUptimeSeconds reports how long a service has been up, measured from
// the preview's start. A failed service reports 0 rather than a number that
// would read as "it has been serving for ten minutes".
func serviceUptimeSeconds(svc ipc.PreviewServiceInfo, createdAt, now time.Time) int64 {
	if svc.Health == state.PreviewServiceFailed || createdAt.IsZero() {
		return 0
	}
	secs := int64(now.Sub(createdAt) / time.Second)
	if secs < 0 {
		return 0
	}
	return secs
}

// previewLogPath is the tail endpoint for one service's log.
func previewLogPath(beadID, service string) string {
	return "/api/preview/" + url.PathEscape(beadID) + "/log/" + url.PathEscape(service)
}

// previewEntryURL builds the link an operator opens for a preview.
//
// The port comes from the manifest's entry service (falling back to the first
// service that has one, so a single-service preview still gets a link). The
// host comes from settings.preview_public_host when it is configured — that is
// the whole point of the setting, naming the box by its LAN/WireGuard hostname
// rather than the loopback the services actually bind. When it is unset the
// request's own Host is used, which is right far more often than 127.0.0.1: a
// browser that reached Hearth at some address can reach the preview there too.
//
// The scheme is always http: preview services bind a plain port and are not
// behind Hearth's TLS.
func previewEntryURL(r *http.Request, p ipc.PreviewInfo, publicHost string) string {
	port := 0
	for _, svc := range p.Services {
		if svc.Entry && svc.Port > 0 {
			port = svc.Port
			break
		}
		if port == 0 && svc.Port > 0 {
			port = svc.Port
		}
	}
	if port == 0 {
		return ""
	}
	host := previewHost(r, publicHost)
	if host == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
}

// previewHost resolves the hostname preview links point at: the configured
// preview_public_host, else the hostname half of the request's Host header
// (its port is Hearth's, not the preview's).
func previewHost(r *http.Request, publicHost string) string {
	if h := strings.TrimSpace(publicHost); h != "" {
		return h
	}
	if r == nil || r.Host == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}

// handlePreviewLog serves GET /api/preview/{bead_id}/log/{service}?tail=N,
// returning the tail of one preview service's log as {"lines": [...]}.
//
// Kiln writes those logs to ~/.forge/logs/<beadID>/preview-<service>.log — the
// same preserved directory the bead log browser reads — so this reuses the same
// bounded tail and the same allowlist check rather than opening a second path
// into the filesystem.
func (s *Server) handlePreviewLog(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	service := chi.URLParam(r, "service")
	if !validPreviewService.MatchString(service) {
		writeError(w, http.StatusBadRequest, "invalid service name")
		return
	}

	allow, err := newLogDirAllowlist()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve home directory")
		return
	}
	dir := filepath.Join(allow.forgeDir, "logs", forge.SanitizeBeadID(beadID))
	resolved, fi, ok := resolveBeadLogFile([]string{dir}, "preview-"+service+".log", allow)
	if !ok {
		writeError(w, http.StatusNotFound, "preview log not found")
		return
	}

	n := clampTailParam(r.URL.Query().Get("tail"), 500)
	lines, err := readTailLines(resolved, fi.Size(), n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read log file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}
