package web

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/state"
	"github.com/go-chi/chi/v5"
)

// beadLogFile is one stage log file surfaced by GET /api/bead/{id}/logs.
type beadLogFile struct {
	Filename  string `json:"filename"`
	Stage     string `json:"stage"`
	SizeBytes int64  `json:"size_bytes"`
	MTime     string `json:"mtime"`
	// Live is true for the file an active worker is currently writing to; the
	// frontend subscribes to that worker's SSE stream instead of a one-shot
	// tail. WorkerID is set only for live files so the client can build the
	// /api/worker/{id}/stream URL.
	Live     bool   `json:"live"`
	WorkerID string `json:"worker_id,omitempty"`
}

// beadLogsResponse is the body of GET /api/bead/{id}/logs.
type beadLogsResponse struct {
	BeadID string        `json:"bead_id"`
	Files  []beadLogFile `json:"files"`
}

// knownLogStages maps a log-file name prefix to the stage label rendered in the
// UI. Files whose prefix is not in this set are labelled "other".
var knownLogStages = map[string]struct{}{
	"smith":     {},
	"warden":    {},
	"temper":    {},
	"quench":    {},
	"burnish":   {},
	"rebase":    {},
	"assay":     {},
	"steer":     {},
	"schematic": {},
}

// stageFromFilename derives the pipeline stage from a log filename of the form
// "<stage>-<ts>-<seq>.log". Unknown prefixes map to "other".
func stageFromFilename(name string) string {
	if i := strings.IndexByte(name, '-'); i > 0 {
		if _, ok := knownLogStages[name[:i]]; ok {
			return name[:i]
		}
	}
	return "other"
}

// activeLogDir describes the live .forge-logs directory of a currently-active
// worker for a bead. liveBase is the basename of the file the worker is
// writing right now; every other file in dir is a completed earlier stage.
type activeLogDir struct {
	dir      string
	liveBase string
	workerID string
}

const maxWorkersPerBeadScan = 200

// beadLogFileFromEntry builds a beadLogFile from a directory entry and its
// FileInfo. Callers set Live/WorkerID on the returned value when appropriate.
func beadLogFileFromEntry(name string, info os.FileInfo) beadLogFile {
	return beadLogFile{
		Filename:  name,
		Stage:     stageFromFilename(name),
		SizeBytes: info.Size(),
		MTime:     info.ModTime().UTC().Format(time.RFC3339),
	}
}

// isTerminalWorkerStatus reports whether a worker has finished and its worktree
// (and live .forge-logs dir) may already be gone.
func isTerminalWorkerStatus(s state.WorkerStatus) bool {
	switch s {
	case state.WorkerDone, state.WorkerFailed, state.WorkerPartial, state.WorkerTimeout, state.WorkerStalled:
		return true
	default:
		return false
	}
}

// activeWorkerLogDirs returns the live worktree log directories for a bead's
// currently-active workers, filtered to the allowlisted roots. Bellows
// pseudo-workers (no log path) and terminal workers are skipped.
func (s *Server) activeWorkerLogDirs(beadID string, allow logDirAllowlist) []activeLogDir {
	workers, err := s.db.WorkersByBead(beadID, "", maxWorkersPerBeadScan)
	if err != nil {
		return nil
	}
	var dirs []activeLogDir
	for _, ww := range workers {
		if isTerminalWorkerStatus(ww.Status) || ww.LogPath == "" {
			continue
		}
		p := ww.LogPath
		if !filepath.IsAbs(p) {
			p = filepath.Clean(filepath.Join(allow.forgeDir, p))
		} else {
			p = filepath.Clean(p)
		}
		dir := filepath.Dir(p)
		if !allow.allows(dir) {
			continue
		}
		dirs = append(dirs, activeLogDir{dir: dir, liveBase: filepath.Base(p), workerID: ww.ID})
	}
	return dirs
}

// handleBeadLogs serves GET /api/bead/{bead_id}/logs. It lists every preserved
// stage log file under ~/.forge/logs/<beadID>/ plus any files in the live
// worktree .forge-logs dir of a currently-active worker for the bead, sorted by
// mtime ascending. The file an active worker is writing right now is flagged
// live=true with its worker id so the frontend can stream it instead of tailing.
func (s *Server) handleBeadLogs(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}

	allow, err := newLogDirAllowlist()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve home directory")
		return
	}

	// De-dupe by filename. Live worktree entries are collected first so that,
	// in the rare window where preservation has already copied a file while the
	// worktree still exists, the live entry wins.
	byName := map[string]beadLogFile{}

	active := s.activeWorkerLogDirs(beadID, allow)
	for _, ad := range active {
		entries, derr := os.ReadDir(ad.dir)
		if derr != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			name := e.Name()
			f := beadLogFileFromEntry(name, info)
			if name == ad.liveBase {
				f.Live = true
				f.WorkerID = ad.workerID
			}
			byName[name] = f
		}
	}

	preservedDir := filepath.Join(allow.forgeDir, "logs", forge.SanitizeBeadID(beadID))
	if entries, derr := os.ReadDir(preservedDir); derr == nil {
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			name := e.Name()
			if _, seen := byName[name]; seen {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			byName[name] = beadLogFileFromEntry(name, info)
		}
	}

	files := make([]beadLogFile, 0, len(byName))
	for _, f := range byName {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].MTime != files[j].MTime {
			return files[i].MTime < files[j].MTime
		}
		return files[i].Filename < files[j].Filename
	})

	writeJSON(w, http.StatusOK, beadLogsResponse{BeadID: beadID, Files: files})
}

// handleBeadLogFile serves GET /api/bead/{bead_id}/logs/{filename}?tail=N. It
// returns the tail of one stage log file as {"lines": [...]} using the same
// bounded read as the worker log endpoint (default 500, clamped to <=10000, at
// most 1 MiB from EOF). The filename is validated as a bare basename and the
// resolved file must live under an allowlisted directory for the bead.
func (s *Server) handleBeadLogFile(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}

	filename := chi.URLParam(r, "filename")
	if !isSafeLogFilename(filename) {
		writeError(w, http.StatusBadRequest, "invalid log filename")
		return
	}

	allow, err := newLogDirAllowlist()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve home directory")
		return
	}

	// Candidate directories: the persistent preserved dir plus any live
	// worktree dirs of active workers for the bead. The first candidate that
	// resolves (after symlink evaluation) to a regular file under an
	// allowlisted root wins.
	dirs := []string{filepath.Join(allow.forgeDir, "logs", forge.SanitizeBeadID(beadID))}
	for _, ad := range s.activeWorkerLogDirs(beadID, allow) {
		dirs = append(dirs, ad.dir)
	}

	resolved, fi, ok := resolveBeadLogFile(dirs, filename, allow)
	if !ok {
		writeError(w, http.StatusNotFound, "log file not found")
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

// isSafeLogFilename accepts a bare log filename: non-empty, no path separators,
// no "." / ".." components, and unchanged by filepath.Base. This blocks path
// traversal (e.g. "../../etc/passwd") before the name ever hits the filesystem.
func isSafeLogFilename(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	if strings.ContainsRune(name, 0) {
		return false
	}
	return filepath.Base(name) == name
}

// resolveBeadLogFile finds filename within one of dirs and returns the
// symlink-resolved path and its FileInfo. Each candidate is Lstat'd (must be a
// regular file, not a symlink pointing elsewhere), then EvalSymlinks'd and
// re-checked against the allowlist so a poisoned directory cannot leak files
// outside the forge-owned roots.
func resolveBeadLogFile(dirs []string, filename string, allow logDirAllowlist) (string, os.FileInfo, bool) {
	for _, dir := range dirs {
		candidate := filepath.Join(dir, filename)
		lfi, err := os.Lstat(candidate)
		if err != nil || !lfi.Mode().IsRegular() {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		if !allow.allows(resolved) {
			continue
		}
		fi, err := os.Stat(resolved)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		return resolved, fi, true
	}
	return "", nil, false
}
