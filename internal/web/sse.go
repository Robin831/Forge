package web

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/go-chi/chi/v5"
)

// validWorkerID accepts the worker IDs forge writes to state.db: alphanumeric
// strings with hyphens, up to 128 chars. Anchoring the pattern keeps the
// regex from matching path traversal sequences inside the URL parameter.
// validWorkerID matches worker IDs of the form `<anvil>-<bead-id>-<unix-nano>`.
// The dot is allowed in non-leading positions so bead prefixes that contain
// one (e.g. `Fhi.Metadata-2rtrj`) produce IDs that survive the gate. The
// leading character is still restricted to [A-Za-z0-9] so neither the
// allowlist-bypassing `.` nor the path-traversal `..` can appear at the
// start, and the worker ID never hits the filesystem directly anyway —
// resolveWorkerLogPath always re-checks the on-disk path against its
// own allowlist.
var validWorkerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9\-_.]{0,127}$`)

// activityEvent is the SSE payload shape for /api/activity/stream. It is a
// transport-friendly subset of state.Event with snake_case field names so the
// React frontend can consume it directly.
type activityEvent struct {
	ID        int       `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	BeadID    string    `json:"bead_id,omitempty"`
	Anvil     string    `json:"anvil,omitempty"`
}

// handleActivityStream serves /api/activity/stream as Server-Sent Events.
//
// On connect (without a Last-Event-ID header) it ships the 50 most recent
// events oldest-first so the client can render a populated timeline before
// live delivery begins. A 30s keep-alive comment keeps the connection warm
// through the skybert nginx ingress (proxy-read-timeout 3600).
//
// Delivery has two modes, selected by whether the in-process event Bus is
// wired into the state DB (settings.bus_enabled) and whether the poll fallback
// is engaged (settings.sse_poll_fallback):
//
//   - Bus enabled AND fallback off: replay-then-live. We subscribe to the Bus
//     BEFORE replaying the backlog so events published mid-replay queue on the
//     bounded channel rather than being lost, replay via EventsSince, then hand
//     over to the live channel dropping any event whose Seq was already emitted
//     during replay. See streamActivityBus.
//   - Bus nil (legacy) OR settings.sse_poll_fallback=true: the 2s poll loop
//     re-reading EventsSince. See streamActivityPolling. The fallback flag is a
//     one-release safety valve slated for removal next release.
func (s *Server) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// Seed the replay start point from the Last-Event-ID header (the standard
	// EventSource resume mechanism); fall back to 0, meaning "no prior cursor".
	var lastID int
	if s := r.Header.Get("Last-Event-ID"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			lastID = n
		}
	}

	// Path selection: take the real-time replay-then-live Bus path when a Bus
	// is wired, UNLESS settings.sse_poll_fallback forces the legacy 2s poll
	// loop. The fallback flag is a one-release safety valve (see
	// pollFallbackEnabled / SetSSEPollFallback) that reverts just this endpoint
	// to polling without disabling the Bus for other consumers. When the Bus is
	// nil (bus disabled) polling is used regardless.
	if bus := s.db.Bus(); bus != nil && !s.pollFallbackEnabled() {
		s.streamActivityBus(w, r, flusher, lastID, bus)
		return
	}
	s.streamActivityPolling(w, r, flusher, lastID)
}

// streamActivityPolling is the legacy delivery path used when no event Bus is
// wired: it primes the timeline (for a fresh connection) then polls
// events.id > lastID every 2s, emitting any new rows.
func (s *Server) streamActivityPolling(w http.ResponseWriter, r *http.Request, flusher http.Flusher, lastID int) {
	if lastID == 0 {
		// RecentEvents returns newest-first; reverse so the client sees the
		// timeline in chronological order.
		initial, err := s.db.RecentEvents(50)
		if err == nil {
			for i := len(initial) - 1; i >= 0; i-- {
				e := initial[i]
				if e.ID > lastID {
					lastID = e.ID
				}
				data, _ := json.Marshal(toActivityEvent(e))
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, data)
			}
			flusher.Flush()
		}
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
			newEvents, err := s.db.EventsSince(lastID, 100)
			if err != nil {
				continue
			}
			for _, e := range newEvents {
				if e.ID > lastID {
					lastID = e.ID
				}
				data, _ := json.Marshal(toActivityEvent(e))
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, data)
			}
			if len(newEvents) > 0 {
				flusher.Flush()
			}
		}
	}
}

// activityReplayPageSize bounds each EventsSince page during backlog replay
// and gap re-sync so a large backlog is drained in order rather than truncated
// to a single page.
const activityReplayPageSize = 100

// streamActivityBus implements the replay-then-live handover against the
// in-process event Bus.
//
// Ordering is the crux: we subscribe BEFORE replaying the backlog so any event
// published between the replay snapshot and the live handover is buffered on
// the bounded channel instead of being lost. After replay we range over the
// live channel and drop any event whose Seq was already emitted during replay
// (or a prior gap re-sync), guaranteeing each event is delivered exactly once
// across the seam — no dupes, no loss.
//
// lastEmittedSeq tracks the highest Seq written to the client and drives the
// dedup. Seq mirrors the event row ID, so it doubles as the SSE `id:` and as
// lastDeliveredID — the cursor shared with the gap-resync sibling sub-task.
func (s *Server) streamActivityBus(w http.ResponseWriter, r *http.Request, flusher http.Flusher, lastID int, bus *state.Bus) {
	// Subscribe first: from here on every published event is buffered on our
	// channel, closing the window between the replay snapshot and live
	// handover. defer Unsubscribe so a client disconnect (or any return) drops
	// us from the fan-out and closes the channel.
	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	var lastEmittedSeq int64
	lastDeliveredID := lastID

	emit := func(e state.Event) {
		data, _ := json.Marshal(toActivityEvent(e))
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, data)
		if int64(e.ID) > lastEmittedSeq {
			lastEmittedSeq = int64(e.ID)
		}
		lastDeliveredID = e.ID
	}

	// resyncFrom drains EventsSince from the shared cursor in pages, emitting
	// each event and advancing lastDeliveredID/lastEmittedSeq. Used both for
	// the initial backlog replay and to close an overflow gap.
	resyncFrom := func() {
		for {
			batch, err := s.db.EventsSince(lastDeliveredID, activityReplayPageSize)
			if err != nil {
				return
			}
			for _, e := range batch {
				emit(e)
			}
			if len(batch) < activityReplayPageSize {
				return
			}
		}
	}

	// Replay phase. A fresh connection (no Last-Event-ID) primes the timeline
	// with the 50 most recent events oldest-first; then EventsSince drains any
	// remaining backlog past the cursor (also covering a resuming connection).
	if lastDeliveredID == 0 {
		if initial, err := s.db.RecentEvents(50); err == nil {
			for i := len(initial) - 1; i >= 0; i-- {
				emit(initial[i])
			}
		}
	}
	resyncFrom()
	flusher.Flush()

	// Live phase: hand over to the Bus channel.
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				// The Bus closed our subscription (unsubscribe / shutdown).
				return
			}
			if ev.GapMarker {
				// Our bounded buffer overflowed and the Bus dropped events.
				// Re-sync the gap from the DB via the shared lastDeliveredID
				// cursor so no events are lost, then resume the live stream.
				resyncFrom()
				flusher.Flush()
				continue
			}
			if ev.Seq <= lastEmittedSeq {
				// Already delivered during replay (or a gap re-sync). Skipping
				// here is what makes the replay→live boundary exactly-once.
				continue
			}
			emit(ev.Event)
			flusher.Flush()
		}
	}
}

// handlePRFindingsStream serves GET /api/prs/{id}/findings/stream as Server-Sent
// Events. It resolves the PR from state.db, emits the current findings/run
// snapshot immediately, then polls every 2s and re-emits only when the snapshot
// changes (findings set or latest run status). Each update is delivered as a
// named `findings` event whose data payload is a prFindingsResponse — the same
// shape GET /api/prs/{id}/findings returns — so the frontend can apply it
// directly. A 30s keep-alive comment keeps the connection warm behind proxies.
func (s *Server) handlePRFindingsStream(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	anvil := ctx.pr.Anvil
	prNumber := ctx.pr.Number

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// lastFingerprint is the JSON of the last snapshot we emitted. Comparing
	// the marshalled payload lets us suppress no-op ticks (no new findings, no
	// run-status change) so the client only re-renders on real updates.
	var lastFingerprint string
	emit := func() {
		resp, err := s.collectPRFindings(anvil, prNumber)
		if err != nil {
			s.logger.Warn("pr findings stream collect failed", "anvil", anvil, "pr", prNumber, "error", err)
			return
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		if string(data) == lastFingerprint {
			return
		}
		lastFingerprint = string(data)
		fmt.Fprintf(w, "event: findings\ndata: %s\n\n", data)
		flusher.Flush()
	}
	emit()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
			emit()
		}
	}
}

// logDirAllowlist holds the forge-owned directory prefixes that worker and
// bead log files must resolve under before they may be read. Building it once
// (via newLogDirAllowlist) avoids repeating the UserHomeDir lookup on every
// allowlist check. The same two roots gate every log endpoint: the persistent
// ~/.forge tree and any worktree under ~/.workers/ (the live .forge-logs dir).
type logDirAllowlist struct {
	forgeDir         string
	forgePrefix      string
	homePrefix       string
	workersComponent string
}

func newLogDirAllowlist() (logDirAllowlist, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return logDirAllowlist{}, err
	}
	forgeDir := filepath.Join(home, ".forge")
	return logDirAllowlist{
		forgeDir:         forgeDir,
		forgePrefix:      forgeDir + string(filepath.Separator),
		homePrefix:       home + string(filepath.Separator),
		workersComponent: string(filepath.Separator) + ".workers" + string(filepath.Separator),
	}, nil
}

// allows reports whether the cleaned, symlink-resolved path p lies under one of
// the allowlisted roots.
func (a logDirAllowlist) allows(p string) bool {
	underForge := p == a.forgeDir || strings.HasPrefix(p, a.forgePrefix)
	underWorkers := strings.HasPrefix(p, a.homePrefix) && strings.Contains(p, a.workersComponent)
	return underForge || underWorkers
}

func toActivityEvent(e state.Event) activityEvent {
	return activityEvent{
		ID:        e.ID,
		Timestamp: e.Timestamp,
		Type:      string(e.Type),
		Message:   e.Message,
		BeadID:    e.BeadID,
		Anvil:     e.Anvil,
	}
}

// openLogWaiting polls logPath once a second until either the file appears or
// budget elapses. Keep-alive comments are flushed to the SSE stream every
// poll so the connection isn't reaped by an intermediary proxy while we wait
// for the smith to create its log file.
//
// Each time the file materialises the function re-runs the same
// Lstat → IsRegular → EvalSymlinks → allowlist checks that
// resolveWorkerLogPath performs when the file is present at resolve time.
// This prevents a window in which a symlink could be placed at logPath
// between the initial (file-absent) resolve and the file appearing on disk.
//
// Returns an error if the context is cancelled, the budget expires, the path
// fails validation, or os.Open fails with anything other than ErrNotExist.
func openLogWaiting(ctx context.Context, logPath string, budget time.Duration, w http.ResponseWriter, flusher http.Flusher) (*os.File, error) {
	deadline := time.Now().Add(budget)

	// Pre-compute the allowlist once so we can re-validate on each poll
	// without repeating the UserHomeDir lookup inside the loop.
	allow, herr := newLogDirAllowlist()
	if herr != nil {
		return nil, herr
	}

	for {
		lfi, lserr := os.Lstat(logPath)
		if lserr == nil {
			// File exists; re-validate before opening.
			if !lfi.Mode().IsRegular() {
				return nil, errors.New("log path is not a regular file")
			}
			resolved, rerr := filepath.EvalSymlinks(logPath)
			if rerr != nil {
				return nil, errors.New("failed to resolve log path")
			}
			resolved = filepath.Clean(resolved)
			if !allow.allows(resolved) {
				return nil, errors.New("invalid log path")
			}
			f, err := os.Open(resolved) //nolint:gosec
			if err == nil {
				return f, nil
			}
			if !os.IsNotExist(err) {
				return nil, err
			}
		} else if !os.IsNotExist(lserr) {
			return nil, lserr
		}
		if time.Now().After(deadline) {
			return nil, os.ErrNotExist
		}
		// Emit a keep-alive so the SSE connection stays alive and the client
		// observes an "open" status instead of falling into the error /
		// "reconnecting…" branch.
		fmt.Fprint(w, ": waiting-for-log\n\n")
		flusher.Flush()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// resolveWorkerLogPath looks the worker up in state.db and returns the
// fully-resolved log file path. It enforces an allowlist so a poisoned
// workers row cannot leak arbitrary files outside forge-owned directories.
// The returned os.FileInfo is the pre-symlink stat for the original path;
// callers that need fresh size after symlink resolution should re-stat
// resolvedPath.
//
// Return values for the two "no log yet" cases differ:
//   - worker.LogPath == "" (bellows pseudo-workers, etc.): returns ("", nil, 0, nil)
//   - log path is set but the file is not yet on disk (smith startup race):
//     returns (logPath, nil, 0, nil) — the non-empty path lets the SSE
//     stream handler poll until the file appears via openLogWaiting.
//
// Callers distinguish the two by checking whether resolvedPath is empty:
// empty means "this worker has no log"; non-empty with nil FileInfo means
// "log path is known but the file is not yet present".
func resolveWorkerLogPath(db *state.DB, workerID string) (resolvedPath string, fi os.FileInfo, status int, err error) {
	worker, qerr := db.GetWorker(workerID)
	if qerr != nil {
		// state.DB.GetWorker returns a wrapped "not found" error rather than
		// sql.ErrNoRows. Treat both shapes as 404 so callers don't see a
		// generic 500 for a missing worker ID.
		if errors.Is(qerr, sql.ErrNoRows) || strings.Contains(qerr.Error(), "not found") {
			return "", nil, http.StatusNotFound, errors.New("worker not found")
		}
		return "", nil, http.StatusInternalServerError, errors.New("failed to load worker")
	}
	if worker.LogPath == "" {
		// Bellows pseudo-workers and any other rows that legitimately have no
		// claude log file land here. Return an empty path so callers can emit
		// an empty-but-200 response rather than 404.
		return "", nil, 0, nil
	}

	allow, herr := newLogDirAllowlist()
	if herr != nil {
		return "", nil, http.StatusInternalServerError, errors.New("failed to resolve home directory")
	}

	logPath := worker.LogPath
	if !filepath.IsAbs(logPath) {
		logPath = filepath.Clean(filepath.Join(allow.forgeDir, logPath))
	} else {
		logPath = filepath.Clean(logPath)
	}

	if !allow.allows(logPath) {
		return "", nil, http.StatusBadRequest, errors.New("invalid log path")
	}

	stat, serr := os.Lstat(logPath)
	if serr != nil {
		if os.IsNotExist(serr) {
			// The worker row exists but the smith hasn't created its log file
			// yet (or it was rotated away). Return the would-be path so callers
			// that want to poll for the file (the SSE stream) can do so, with
			// a nil FileInfo signalling "not yet present".
			return logPath, nil, 0, nil
		}
		return "", nil, http.StatusInternalServerError, errors.New("failed to stat log file")
	}
	if !stat.Mode().IsRegular() {
		return "", nil, http.StatusBadRequest, errors.New("log path is not a regular file")
	}

	resolved, rerr := filepath.EvalSymlinks(logPath)
	if rerr != nil {
		return "", nil, http.StatusInternalServerError, errors.New("failed to resolve log path")
	}
	resolved = filepath.Clean(resolved)
	if !allow.allows(resolved) {
		return "", nil, http.StatusBadRequest, errors.New("invalid log path")
	}
	return resolved, stat, 0, nil
}

// handleWorkerLogTail serves GET /api/worker/{id}/log?tail=N. It returns a
// JSON object {"lines": [...]} containing the last N log lines. N defaults
// to 100 and is clamped to [1, 10000]; at most 1 MiB is read from the end
// of the file so very large logs do not pull megabytes into memory.
func (s *Server) handleWorkerLogTail(w http.ResponseWriter, r *http.Request) {
	workerID := chi.URLParam(r, "id")
	if workerID == "" || !validWorkerID.MatchString(workerID) {
		writeError(w, http.StatusBadRequest, "invalid worker ID")
		return
	}

	logPath, fi, status, err := resolveWorkerLogPath(s.db, workerID)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	// Worker exists but its log file isn't yet on disk (active worker race
	// or bellows pseudo-worker). Return 200 with an empty array so the SPA
	// renders an empty log instead of an error banner.
	if logPath == "" || fi == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}})
		return
	}

	n := clampTailParam(r.URL.Query().Get("tail"), 100)
	lines, err := readTailLines(logPath, fi.Size(), n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read log file")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// maxTailReadBytes bounds how many bytes readTailLines reads from the end of a
// log file so very large logs do not pull megabytes into memory.
const maxTailReadBytes int64 = 1 << 20 // 1 MiB

// clampTailParam parses a `tail` query value, falling back to def when it is
// empty or non-numeric, and clamps the result to [1, 10000].
func clampTailParam(raw string, def int) int {
	n := def
	if raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		}
	}
	if n > 10000 {
		n = 10000
	}
	if n < 1 {
		n = 1
	}
	return n
}

// readTailLines returns the last n fully-terminated lines of the file at
// logPath. At most maxTailReadBytes is read from the end of the file; when the
// file is larger the first (partial) line of the read window is discarded. A
// trailing partial line (the file may still be mid-write) is always dropped so
// callers only observe complete records.
func readTailLines(logPath string, size int64, n int) ([]string, error) {
	var data []byte
	var err error
	var seeked bool
	if size <= maxTailReadBytes {
		data, err = os.ReadFile(logPath) //nolint:gosec
	} else {
		seeked = true
		f, ferr := os.Open(logPath) //nolint:gosec
		if ferr != nil {
			return nil, ferr
		}
		defer f.Close()
		if _, ferr = f.Seek(-maxTailReadBytes, io.SeekEnd); ferr != nil {
			return nil, ferr
		}
		data, err = io.ReadAll(f)
	}
	if err != nil {
		return nil, err
	}
	if seeked {
		if idx := strings.IndexByte(string(data), '\n'); idx >= 0 {
			data = data[idx+1:]
		} else {
			data = nil
		}
	}

	raw := string(data)
	if idx := strings.LastIndexByte(raw, '\n'); idx >= 0 {
		raw = raw[:idx]
	} else {
		raw = ""
	}
	lines := []string{}
	if raw != "" {
		lines = strings.Split(raw, "\n")
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// handleWorkerLogStream serves GET /api/worker/{id}/stream as Server-Sent
// Events. It first scans the existing log content line-by-line, then polls
// the file size every 500ms; any new bytes are split on newline and each
// complete line is emitted as a `{"line": ..., "timestamp": ...}` SSE event.
//
// Truncation/rotation is handled by reopening the file and resetting the
// offset when the on-disk size shrinks below the last-known offset.
func (s *Server) handleWorkerLogStream(w http.ResponseWriter, r *http.Request) {
	workerID := chi.URLParam(r, "id")
	if workerID == "" || !validWorkerID.MatchString(workerID) {
		writeError(w, http.StatusBadRequest, "invalid worker ID")
		return
	}

	logPath, _, status, err := resolveWorkerLogPath(s.db, workerID)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if logPath == "" {
		// Worker has no log file (bellows pseudo-worker, etc.). The SPA gates
		// these so the modal should never be opened in the first place; if
		// something else hits this URL anyway, return 404 so EventSource
		// fails fast instead of spinning on "reconnecting…" forever.
		writeError(w, http.StatusNotFound, "worker has no log file")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// Wait briefly for the log file to appear if the smith hasn't written
	// anything yet (race between the workers row insert and claude creating
	// its log on disk). Capped at 30s; the loop also honours client
	// disconnect and emits keep-alives so the connection isn't reaped by a
	// reverse proxy. Once the file shows up we proceed to the normal stream.
	f, err := openLogWaiting(r.Context(), logPath, 30*time.Second, w, flusher)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\":\"log file not accessible\"}\n\n")
		flusher.Flush()
		return
	}
	// Use a closure so the defer always closes the current value of f, even
	// after truncation/rotation causes f to be reassigned to a new handle.
	defer func() { f.Close() }()

	// Read the existing file contents and emit fully-terminated lines only.
	// Any trailing partial line (file is mid-write) becomes the initial value
	// of `partial` so the ticker loop below stitches it onto the next chunk
	// instead of double-emitting it.
	var (
		offset  int64
		partial string
	)
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := reader.ReadString('\n')
		if line != "" {
			if strings.HasSuffix(line, "\n") {
				trimmed := strings.TrimRight(line, "\n")
				offset += int64(len(line))
				if trimmed != "" {
					entry := map[string]string{"line": trimmed, "timestamp": time.Now().UTC().Format(time.RFC3339)}
					data, _ := json.Marshal(entry)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
			} else {
				// Unterminated tail — hold it as the partial buffer.
				partial = line
			}
		}
		if rerr != nil {
			break
		}
	}
	flusher.Flush()
	offset += int64(len(partial))

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-ticker.C:
			fi, err := f.Stat()
			if err != nil {
				continue
			}
			if fi.Size() < offset {
				// File was truncated or rotated — reopen from the beginning.
				old := f
				newF, openErr := os.Open(logPath) //nolint:gosec
				if openErr != nil {
					// Can't continue streaming; deferred f.Close() will close old.
					return
				}
				old.Close()
				f = newF
				offset = 0
				partial = ""
				continue
			}
			if fi.Size() <= offset {
				continue
			}
			const maxChunkSize int64 = 64 * 1024 // 64 KiB per tick
			chunkSize := fi.Size() - offset
			if chunkSize > maxChunkSize {
				chunkSize = maxChunkSize
			}
			buf := make([]byte, int(chunkSize))
			n, rerr := f.ReadAt(buf, offset)
			if n == 0 {
				continue
			}
			if rerr != nil && rerr != io.EOF {
				continue
			}
			offset += int64(n)
			chunk := partial + string(buf[:n])
			lines := strings.Split(chunk, "\n")
			partial = lines[len(lines)-1]
			lines = lines[:len(lines)-1]
			flushed := false
			for _, line := range lines {
				if line == "" {
					continue
				}
				entry := map[string]string{"line": line, "timestamp": time.Now().UTC().Format(time.RFC3339)}
				data, _ := json.Marshal(entry)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flushed = true
			}
			if flushed {
				flusher.Flush()
			}
		}
	}
}
