package web

import (
	"bufio"
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
var validWorkerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9\-_]{0,127}$`)

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

// resolveWorkerLogPath looks the worker up in state.db and returns the
// fully-resolved log file path. It enforces the same allowlist as Hytte's
// WorkerLogHandler so a poisoned workers row cannot leak arbitrary files
// outside the forge-owned directories. The returned os.FileInfo is the
// pre-symlink stat for the original path; callers that need fresh size
// after symlink resolution should re-stat resolvedPath.
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
		return "", nil, http.StatusNotFound, errors.New("worker has no log file")
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
			return "", nil, http.StatusNotFound, errors.New("log file not found")
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

	raw := strings.TrimRight(string(data), "\n")
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

	f, err := os.Open(logPath) //nolint:gosec
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\":\"log file not accessible\"}\n\n")
		flusher.Flush()
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		entry := map[string]string{"line": line, "timestamp": time.Now().UTC().Format(time.RFC3339)}
		data, _ := json.Marshal(entry)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	var offset int64
	if fi, err := f.Stat(); err == nil {
		offset = fi.Size()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	var partial string

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
				f.Close()
				f, err = os.Open(logPath) //nolint:gosec
				if err != nil {
					continue
				}
				offset = 0
				partial = ""
				continue
			}
			if fi.Size() <= offset {
				continue
			}
			buf := make([]byte, fi.Size()-offset)
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
