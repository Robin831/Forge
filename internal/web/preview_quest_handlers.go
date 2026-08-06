package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Robin831/Forge/internal/ipc"
)

// The types below are the frontend-facing contract for preview quest runs, kept
// beside the preview DTOs for the same reason those are kept together: the SPA
// mirrors them field for field.
//
// A quest run is informational. It reports what a browser found on a preview of
// one branch, and nothing reads it back: no pipeline stage, no Bellows check and
// no merge gate consults a run's status. A red run is a prompt for a human
// looking at the branch, never a block on it — which is why these types carry
// no "blocking" or "required" notion at all and why the panel styles a failure
// as a warning rather than an error.

// validQuestRunID matches the run ids questgiver.RunStore mints
// (`qr-<epoch>-<seq>`). It gates the {run_id} path segment before it reaches
// the daemon.
var validQuestRunID = regexp.MustCompile(`^qr-[0-9]+-[0-9]+$`)

// questScreenshotExtensions is what a quest's `screenshot` step is allowed to
// have produced for the image endpoint to serve it. Quest files live in the
// anvil and are as trusted as any other repo content, but the endpoint still
// refuses to stream something that is not an image: a path is a path, and the
// only thing this route exists to hand back is a screenshot.
var questScreenshotExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// maxQuestScreenshotBytes caps one screenshot response. A full-page capture is
// a few hundred KiB; anything past this is not a screenshot.
const maxQuestScreenshotBytes = 16 << 20

// QuestScreenshot is one captured image, addressed by an endpoint on this
// server rather than by its filesystem path — the browser never sees where the
// daemon put it.
type QuestScreenshot struct {
	// Name is the file's base name, for the thumbnail's alt text and title.
	Name string `json:"name"`
	// URL serves the image bytes.
	URL string `json:"url"`
}

// QuestOutcomeSummary is what one quest did during a run.
type QuestOutcomeSummary struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// FailedStep is the index of the step that failed, or -1 when none did.
	FailedStep      int               `json:"failed_step"`
	ErrorMessage    string            `json:"error_message,omitempty"`
	DurationSeconds float64           `json:"duration_seconds"`
	FilePath        string            `json:"file_path,omitempty"`
	Screenshots     []QuestScreenshot `json:"screenshots"`
}

// QuestRunSummary is one preview quest run.
type QuestRunSummary struct {
	RunID     string `json:"run_id"`
	BeadID    string `json:"bead_id"`
	Anvil     string `json:"anvil,omitempty"`
	PreviewID string `json:"preview_id,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	// Status is one of running / passed / failed / skipped / error.
	Status string `json:"status"`
	// SkipReason explains a skipped run (a gate said no — not a failure),
	// Error an errored one (the quests never got a verdict).
	SkipReason      string                `json:"skip_reason,omitempty"`
	Error           string                `json:"error,omitempty"`
	StartedAt       time.Time             `json:"started_at"`
	FinishedAt      *time.Time            `json:"finished_at"`
	DurationSeconds float64               `json:"duration_seconds"`
	Quests          []QuestOutcomeSummary `json:"quests"`
}

// QuestRunResponse is the body of the run endpoints. Found is false when the
// bead has never had a run (or its runs aged out of the daemon's bounded
// history), which the panel renders as "no runs yet" rather than an error.
type QuestRunResponse struct {
	Found bool             `json:"found"`
	Run   *QuestRunSummary `json:"run,omitempty"`
}

// QuestRunStartResponse is the 202 body of a dispatched run: the run id to poll
// and the freshly-created record, so the panel can render "running" without a
// round trip.
type QuestRunStartResponse struct {
	Started bool             `json:"started"`
	RunID   string           `json:"run_id"`
	Message string           `json:"message,omitempty"`
	Run     *QuestRunSummary `json:"run,omitempty"`
}

// questRejectStatus maps a daemon rejection reason onto an HTTP status.
//
// The gates the "Run quests" action is offered behind — the anvil's
// preview_quests opt-in and a healthy preview — are 403: the request was
// understood and refused, and a client that shows the button anyway should be
// told it had no business doing so. The rest describe a resource that is not
// there (404), a conflicting state (409) or a capability this daemon does not
// have (503).
func questRejectStatus(reason string) int {
	switch reason {
	case ipc.PreviewQuestRejectNotEnabled, ipc.PreviewQuestRejectNotHealthy:
		return http.StatusForbidden
	case ipc.PreviewQuestRejectNoPreview, ipc.PreviewQuestRejectDisabled:
		return http.StatusNotFound
	case ipc.PreviewQuestRejectAlreadyRunning, ipc.PreviewQuestRejectNoEntryURL:
		return http.StatusConflict
	case ipc.PreviewQuestRejectUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// handleQuestRunStart serves POST /api/bead/{bead_id}/quests: run the anvil's
// E2E quests against this bead's live preview.
//
// It answers 202 the moment the daemon has accepted the run — the browser work
// takes minutes — with the run id the SPA polls through the GET below. Unlike
// the preview start/stop actions this is not a "queued" IPC command with a
// request_id: a quest run has a richer outcome than ok/error and its own status
// endpoint, so the run id *is* the correlation handle.
func (s *Server) handleQuestRunStart(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	s.logActor(r, "preview_quest_run", "bead", beadID)

	payload, err := json.Marshal(ipc.PreviewQuestRunPayload{BeadID: beadID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode payload: "+err.Error())
		return
	}
	resp := s.handler(ipc.Command{Type: "preview_quest_run", Payload: payload})
	if resp.Type != "ok" && resp.Type != "status" {
		s.writeIPCResponse(w, resp)
		return
	}
	var out ipc.PreviewQuestRunResponse
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		writeError(w, http.StatusInternalServerError, "invalid quest run payload")
		return
	}
	if !out.Started {
		message := out.Message
		if message == "" {
			message = "quest run refused"
		}
		writeError(w, questRejectStatus(out.Reason), message)
		return
	}
	writeJSON(w, http.StatusAccepted, QuestRunStartResponse{
		Started: true,
		RunID:   out.RunID,
		Message: out.Message,
		Run:     questRunSummary(beadID, out.Run),
	})
}

// handleQuestRunStatus serves GET /api/bead/{bead_id}/quests (the bead's most
// recent run) and GET /api/bead/{bead_id}/quests/{run_id} (a specific one). The
// panel polls the former: it survives a reload that lost the run id, and a bead
// only ever has one run in flight.
func (s *Server) handleQuestRunStatus(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	runID := chi.URLParam(r, "run_id")
	if runID != "" && !validQuestRunID.MatchString(runID) {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}

	run, ok, errResp := s.fetchQuestRun(beadID, runID)
	if errResp != nil {
		s.writeIPCResponse(w, *errResp)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, QuestRunResponse{Found: false})
		return
	}
	writeJSON(w, http.StatusOK, QuestRunResponse{Found: true, Run: questRunSummary(beadID, run)})
}

// handleQuestScreenshot serves
// GET /api/bead/{bead_id}/quests/{run_id}/screenshot/{index}, streaming one of
// the images a run captured.
//
// The client addresses screenshots by position in the run, never by path: the
// index is resolved against the run record the daemon holds, so there is no
// caller-supplied path to traverse with. What the run does hold is a filesystem
// path written by a quest file, so the file itself is still checked — regular
// file, image extension, bounded size — before a byte is served.
func (s *Server) handleQuestScreenshot(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	runID := chi.URLParam(r, "run_id")
	if !validQuestRunID.MatchString(runID) {
		writeError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		writeError(w, http.StatusBadRequest, "invalid screenshot index")
		return
	}

	run, ok, errResp := s.fetchQuestRun(beadID, runID)
	if errResp != nil {
		s.writeIPCResponse(w, *errResp)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "quest run not found")
		return
	}
	paths := questScreenshotPaths(run)
	if index >= len(paths) {
		writeError(w, http.StatusNotFound, "screenshot not found")
		return
	}

	path := paths[index]
	contentType, allowed := questScreenshotExtensions[strings.ToLower(filepath.Ext(path))]
	if !allowed {
		writeError(w, http.StatusUnsupportedMediaType, "not an image")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "screenshot file is gone")
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "screenshot file is gone")
		return
	}
	if fi.Size() > maxQuestScreenshotBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "screenshot is too large to serve")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	// The bytes are immutable for the life of the run, but the run itself is
	// only in daemon memory, so caching is scoped to the browser session.
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(f, maxQuestScreenshotBytes))
}

// fetchQuestRun asks the daemon for a run — a specific id when given one, else
// the bead's latest. It returns (run, found, nil) on success, or a non-nil
// response for the caller to forward when the IPC layer itself failed.
func (s *Server) fetchQuestRun(beadID, runID string) (*ipc.PreviewQuestRun, bool, *ipc.Response) {
	payload, err := json.Marshal(ipc.PreviewQuestStatusPayload{BeadID: beadID, RunID: runID})
	if err != nil {
		resp := ipc.Response{Type: "error", Payload: json.RawMessage(`{"message":"failed to encode payload"}`)}
		return nil, false, &resp
	}
	resp := s.handler(ipc.Command{Type: "preview_quest_status", Payload: payload})
	if resp.Type != "ok" && resp.Type != "status" {
		return nil, false, &resp
	}
	var out ipc.PreviewQuestStatusResponse
	if len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, &out); err != nil {
			bad := ipc.Response{Type: "error", Payload: json.RawMessage(`{"message":"invalid quest run payload"}`)}
			return nil, false, &bad
		}
	}
	if !out.Found || out.Run == nil {
		return nil, false, nil
	}
	return out.Run, true, nil
}

// questScreenshotPaths flattens a run's screenshots into the run-wide order the
// index endpoint addresses: quest order, then step order within a quest. It is
// the one place that ordering is defined, so the URLs questRunSummary hands out
// and the file handleQuestScreenshot opens cannot disagree.
func questScreenshotPaths(run *ipc.PreviewQuestRun) []string {
	if run == nil {
		return nil
	}
	var paths []string
	for _, q := range run.Quests {
		paths = append(paths, q.Screenshots...)
	}
	return paths
}

// questRunSummary maps an IPC run onto the frontend DTO, replacing every
// screenshot path with an endpoint on this server. beadID comes from the URL so
// the links are built under the path the client already authenticated against.
func questRunSummary(beadID string, run *ipc.PreviewQuestRun) *QuestRunSummary {
	if run == nil {
		return nil
	}
	out := &QuestRunSummary{
		RunID:           run.RunID,
		BeadID:          run.BeadID,
		Anvil:           run.Anvil,
		PreviewID:       run.PreviewID,
		BaseURL:         run.BaseURL,
		Status:          run.Status,
		SkipReason:      run.SkipReason,
		Error:           run.Error,
		StartedAt:       run.StartedAt,
		FinishedAt:      run.FinishedAt,
		DurationSeconds: run.DurationSeconds,
		Quests:          make([]QuestOutcomeSummary, 0, len(run.Quests)),
	}
	if out.BeadID == "" {
		out.BeadID = beadID
	}
	index := 0
	for _, q := range run.Quests {
		outcome := QuestOutcomeSummary{
			Name:            q.Name,
			Passed:          q.Passed,
			FailedStep:      q.FailedStep,
			ErrorMessage:    q.ErrorMessage,
			DurationSeconds: q.DurationSeconds,
			FilePath:        q.FilePath,
			Screenshots:     make([]QuestScreenshot, 0, len(q.Screenshots)),
		}
		for _, path := range q.Screenshots {
			outcome.Screenshots = append(outcome.Screenshots, QuestScreenshot{
				Name: filepath.Base(path),
				URL:  questScreenshotPath(beadID, run.RunID, index),
			})
			index++
		}
		out.Quests = append(out.Quests, outcome)
	}
	return out
}

// questScreenshotPath is the endpoint serving one screenshot's bytes.
func questScreenshotPath(beadID, runID string, index int) string {
	return "/api/bead/" + url.PathEscape(beadID) + "/quests/" + url.PathEscape(runID) +
		"/screenshot/" + strconv.Itoa(index)
}
