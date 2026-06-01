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
// the first poll fires. Every 2s the daemon polls events.id > lastID and
// emits any new rows. A 30s keep-alive comment keeps the connection warm
// through the skybert nginx ingress (proxy-read-timeout 3600).
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

	var lastID int
	if s := r.Header.Get("Last-Event-ID"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			lastID = n
		}
	}

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
	home, herr := os.UserHomeDir()
	if herr != nil {
		return nil, herr
	}
	forgeDir := filepath.Join(home, ".forge")
	forgePrefix := forgeDir + string(filepath.Separator)
	homePrefix := home + string(filepath.Separator)
	workersComponent := string(filepath.Separator) + ".workers" + string(filepath.Separator)
	allowed := func(p string) bool {
		underForge := p == forgeDir || strings.HasPrefix(p, forgePrefix)
		underWorkers := strings.HasPrefix(p, homePrefix) && strings.Contains(p, workersComponent)
		return underForge || underWorkers
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
			if !allowed(resolved) {
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

	home, herr := os.UserHomeDir()
	if herr != nil {
		return "", nil, http.StatusInternalServerError, errors.New("failed to resolve home directory")
	}
	forgeDir := filepath.Join(home, ".forge")

	logPath := worker.LogPath
	if !filepath.IsAbs(logPath) {
		logPath = filepath.Clean(filepath.Join(forgeDir, logPath))
	} else {
		logPath = filepath.Clean(logPath)
	}

	forgePrefix := forgeDir + string(filepath.Separator)
	homePrefix := home + string(filepath.Separator)
	workersComponent := string(filepath.Separator) + ".workers" + string(filepath.Separator)
	allowed := func(p string) bool {
		underForge := p == forgeDir || strings.HasPrefix(p, forgePrefix)
		underWorkers := strings.HasPrefix(p, homePrefix) && strings.Contains(p, workersComponent)
		return underForge || underWorkers
	}
	if !allowed(logPath) {
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
	if !allowed(resolved) {
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

	n := 100
	if raw := r.URL.Query().Get("tail"); raw != "" {
		if v, perr := strconv.Atoi(raw); perr == nil && v > 0 {
			n = v
		}
	}
	if n > 10000 {
		n = 10000
	}

	const maxTailReadBytes int64 = 1 << 20 // 1 MiB
	var data []byte
	var seeked bool
	if fi.Size() <= maxTailReadBytes {
		data, err = os.ReadFile(logPath) //nolint:gosec
	} else {
		seeked = true
		f, ferr := os.Open(logPath) //nolint:gosec
		if ferr != nil {
			writeError(w, http.StatusInternalServerError, "failed to read log file")
			return
		}
		defer f.Close()
		if _, ferr = f.Seek(-maxTailReadBytes, io.SeekEnd); ferr != nil {
			writeError(w, http.StatusInternalServerError, "failed to read log file")
			return
		}
		data, err = io.ReadAll(f)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read log file")
		return
	}
	if seeked {
		if idx := strings.IndexByte(string(data), '\n'); idx >= 0 {
			data = data[idx+1:]
		} else {
			data = nil
		}
	}

	// Trim a trailing partial line (file is still being written to and the
	// last record may not be complete). Splitting on the rightmost newline
	// ensures the response only contains fully-written lines.
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
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
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
