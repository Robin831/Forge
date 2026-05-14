package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/go-chi/chi/v5"
)

// isValidBeadID restricts {bead_id} URL parameters to the shape bd
// produces — ASCII letters, digits, dots, hyphens, underscores — keeping
// SQL queries from observing path-traversal sequences.
func isValidBeadID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, r := range s {
		isAlnum := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 && !isAlnum {
			return false
		}
		if !isAlnum && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// handleCrucibles mirrors the IPC "crucibles" command and returns the list of
// active crucibles (parent beads being orchestrated across child beads).
func (s *Server) handleCrucibles(w http.ResponseWriter, _ *http.Request) {
	resp := s.dispatchIPC("crucibles")
	s.writeIPCResponse(w, resp)
}

// handleIngots mirrors the IPC "get_ingots" command. Optional query params
// `anvil`, `status`, and `limit` narrow the result set.
func (s *Server) handleIngots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	payload := ipc.GetIngotsPayload{
		Anvil:  strings.TrimSpace(q.Get("anvil")),
		Status: strings.TrimSpace(q.Get("status")),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > 500 {
			n = 500
		}
		payload.Limit = n
	}
	body, _ := json.Marshal(payload)
	s.writeIPCResponse(w, s.handler(ipc.Command{Type: "get_ingots", Payload: body}))
}

// handleIngot mirrors the IPC "get_ingot" command. Optional `anvil` query
// param disambiguates when a bead exists in multiple anvils.
func (s *Server) handleIngot(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	payload := ipc.GetIngotPayload{
		BeadID: beadID,
		Anvil:  strings.TrimSpace(r.URL.Query().Get("anvil")),
	}
	body, _ := json.Marshal(payload)
	resp := s.handler(ipc.Command{Type: "get_ingot", Payload: body})
	// "get_ingot" returns errors with the daemon's standard {"message": ...}
	// shape; map "not found" messages to 404 so the SPA can render an empty
	// state instead of a generic error banner.
	if resp.Type == "error" {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(resp.Payload, &body)
		if strings.Contains(strings.ToLower(body.Message), "not found") {
			writeError(w, http.StatusNotFound, body.Message)
			return
		}
	}
	s.writeIPCResponse(w, resp)
}

// historyWorker is the JSON shape returned by /api/history/workers.
type historyWorker struct {
	ID          string  `json:"id"`
	BeadID      string  `json:"bead_id"`
	Anvil       string  `json:"anvil"`
	Branch      string  `json:"branch,omitempty"`
	Title       string  `json:"title,omitempty"`
	Status      string  `json:"status"`
	Phase       string  `json:"phase,omitempty"`
	Kind        string  `json:"kind,omitempty"`
	PID         int     `json:"pid,omitempty"`
	StartedAt   string  `json:"started_at"`
	CompletedAt string  `json:"completed_at,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	LogPath     string  `json:"log_path,omitempty"`
	PRNumber    int     `json:"pr_number,omitempty"`
}

// handleHistoryWorkers returns recent completed workers. ?limit= controls
// how many rows are returned (default 50, clamped 1..500).
func (s *Server) handleHistoryWorkers(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		if n < 1 {
			n = 1
		} else if n > 500 {
			n = 500
		}
		limit = n
	}
	workers, err := s.db.CompletedWorkers(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load history: "+err.Error())
		return
	}
	out := make([]historyWorker, 0, len(workers))
	for _, ww := range workers {
		hw := historyWorker{
			ID:        ww.ID,
			BeadID:    ww.BeadID,
			Anvil:     ww.Anvil,
			Branch:    ww.Branch,
			Title:     ww.Title,
			Status:    string(ww.Status),
			Phase:     ww.Phase,
			Kind:      ipc.WorkerKindFromPhase(ww.Phase),
			PID:       ww.PID,
			StartedAt: ww.StartedAt.Format(time.RFC3339),
			LogPath:   ww.LogPath,
			PRNumber:  ww.PRNumber,
		}
		if ww.CompletedAt != nil {
			hw.CompletedAt = ww.CompletedAt.Format(time.RFC3339)
			hw.DurationSec = ww.CompletedAt.Sub(ww.StartedAt).Seconds()
		}
		out = append(out, hw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

// costRow is one calendar day in the costs response.
type costRow struct {
	Date          string  `json:"date"`
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// providerCostRow is one provider's slice of today's spend.
type providerCostRow struct {
	Provider      string  `json:"provider"`
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	CacheRead     int     `json:"cache_read"`
	CacheWrite    int     `json:"cache_write"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// costsResponse is the JSON shape returned by /api/costs.
type costsResponse struct {
	Today          costRow           `json:"today"`
	TodayLimit     float64           `json:"today_limit"`
	Recent         []costRow         `json:"recent"`
	TodayProviders []providerCostRow `json:"today_providers"`
}

// handleCosts returns the daily cost summary used by the Costs view.
func (s *Server) handleCosts(w http.ResponseWriter, r *http.Request) {
	days := 14
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "days must be a positive integer")
			return
		}
		if n > 90 {
			n = 90
		}
		days = n
	}

	today := cost.Today()
	resp := costsResponse{Today: costRow{Date: today}}

	in, out, _, _, c, limit, err := s.db.GetDailyCost(today)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to load today's cost: "+err.Error())
		return
	}
	if err == nil {
		resp.Today = costRow{
			Date:          today,
			InputTokens:   in,
			OutputTokens:  out,
			EstimatedCost: c,
		}
		resp.TodayLimit = limit
	}

	recent, err := s.db.RecentDailyCosts(days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load daily costs: "+err.Error())
		return
	}
	resp.Recent = make([]costRow, 0, len(recent))
	for _, c := range recent {
		resp.Recent = append(resp.Recent, costRow{
			Date:          c.Date,
			InputTokens:   c.InputTokens,
			OutputTokens:  c.OutputTokens,
			EstimatedCost: c.EstimatedCost,
		})
	}

	provs, err := s.db.GetProviderDailyCosts(today)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load provider costs: "+err.Error())
		return
	}
	resp.TodayProviders = make([]providerCostRow, 0, len(provs))
	for _, p := range provs {
		resp.TodayProviders = append(resp.TodayProviders, providerCostRow{
			Provider:      p.Provider,
			InputTokens:   p.InputTokens,
			OutputTokens:  p.OutputTokens,
			CacheRead:     p.CacheRead,
			CacheWrite:    p.CacheWrite,
			EstimatedCost: p.EstimatedCost,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// beadDetailQueue is the queue cache row shown in the bead detail view.
type beadDetailQueue struct {
	BeadID      string   `json:"bead_id"`
	Anvil       string   `json:"anvil"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Priority    int      `json:"priority"`
	Status      string   `json:"status"`
	Section     string   `json:"section"`
	Labels      []string `json:"labels"`
	Assignee    string   `json:"assignee,omitempty"`
}

// beadDetailRetry mirrors state.RetryRecord for the JSON response.
type beadDetailRetry struct {
	RetryCount          int    `json:"retry_count"`
	NextRetry           string `json:"next_retry,omitempty"`
	NeedsHuman          bool   `json:"needs_human"`
	ClarificationNeeded bool   `json:"clarification_needed"`
	DispatchFailures    int    `json:"dispatch_failures"`
	RecoveryFailures    int    `json:"recovery_failures"`
	LastError           string `json:"last_error,omitempty"`
	UpdatedAt           string `json:"updated_at,omitempty"`
}

// beadDetailCost is the cumulative cost for the bead.
type beadDetailCost struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheRead        int     `json:"cache_read"`
	CacheWrite       int     `json:"cache_write"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
	UpdatedAt        string  `json:"updated_at,omitempty"`
}

// beadDetailEvent is one row from the events log scoped to this bead.
type beadDetailEvent struct {
	ID        int    `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Message   string `json:"message"`
}

// beadDetailPR is the PR summary shown in the bead detail view.
type beadDetailPR struct {
	ID          int    `json:"id"`
	Number      int    `json:"number"`
	Anvil       string `json:"anvil"`
	Branch      string `json:"branch,omitempty"`
	BaseBranch  string `json:"base_branch,omitempty"`
	Status      string `json:"status"`
	Title       string `json:"title,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	LastChecked string `json:"last_checked,omitempty"`
}

// beadDetailWorker is one row from the worker history scoped to this bead.
type beadDetailWorker struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Phase       string  `json:"phase,omitempty"`
	Branch      string  `json:"branch,omitempty"`
	StartedAt   string  `json:"started_at"`
	CompletedAt string  `json:"completed_at,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	LogPath     string  `json:"log_path,omitempty"`
	PRNumber    int     `json:"pr_number,omitempty"`
}

// beadDetailComment is one comment from `bd comments <id> --json`. The bd CLI
// names the body field `text`, but the response surfaces it as `body` so the
// SPA can render it uniformly with other markdown-ish text blocks.
type beadDetailComment struct {
	ID        string `json:"id,omitempty"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// beadDetailDepRef is a lightweight reference to a related bead. It is used
// both for the immediate Blocks/BlockedBy lists on beadDetailResponse and
// for the recursive tree returned by /api/bead/{id}/deps. The nested
// Blocks/BlockedBy fields are populated only when the dep walker recurses
// past depth 1; at the leaves they are nil and serialise as omitted.
type beadDetailDepRef struct {
	BeadID    string             `json:"bead_id"`
	Anvil     string             `json:"anvil,omitempty"`
	Title     string             `json:"title"`
	Status    string             `json:"status"`
	Priority  int                `json:"priority"`
	Blocks    []beadDetailDepRef `json:"blocks,omitempty"`
	BlockedBy []beadDetailDepRef `json:"blocked_by,omitempty"`
}

// beadDetailResponse is the consolidated JSON shape for /api/bead/{id}.
type beadDetailResponse struct {
	BeadID    string              `json:"bead_id"`
	Anvil     string              `json:"anvil,omitempty"`
	Queue     *beadDetailQueue    `json:"queue,omitempty"`
	Ingot     *ingot.Ingot        `json:"ingot,omitempty"`
	Retry     *beadDetailRetry    `json:"retry,omitempty"`
	Cost      *beadDetailCost     `json:"cost,omitempty"`
	Workers   []beadDetailWorker  `json:"workers"`
	Events    []beadDetailEvent   `json:"events"`
	PRs       []beadDetailPR      `json:"prs"`
	Blocks    []beadDetailDepRef  `json:"blocks"`
	BlockedBy []beadDetailDepRef  `json:"blocked_by"`
	Notes     string              `json:"notes,omitempty"`
	Comments  []beadDetailComment `json:"comments"`
}

// beadDepsResponse is the JSON shape returned by /api/bead/{id}/deps.
type beadDepsResponse struct {
	BeadID    string             `json:"bead_id"`
	Depth     int                `json:"depth"`
	Blocks    []beadDetailDepRef `json:"blocks"`
	BlockedBy []beadDetailDepRef `json:"blocked_by"`
}

// bdShowEntry mirrors the subset of `bd show <id> --json` we care about.
// The bd CLI returns a JSON array; each entry exposes outgoing edges as
// `dependents` (beads blocked by this one) and incoming edges as
// `dependencies` (beads this one is blocked by). The `dependency_type`
// field on each edge entry distinguishes "blocks" relations from softer
// links (e.g. "discovered-from", "related"); only "blocks" is surfaced
// as Blocks/BlockedBy.
type bdShowEntry struct {
	ID             string        `json:"id"`
	Title          string        `json:"title"`
	Status         string        `json:"status"`
	Priority       int           `json:"priority"`
	Notes          string        `json:"notes"`
	DependencyType string        `json:"dependency_type"`
	Dependencies   []bdShowEntry `json:"dependencies"`
	Dependents     []bdShowEntry `json:"dependents"`
}

// isBlockingDep reports whether a dep edge counts as a "blocks" relation.
// Edges without an explicit dependency_type (e.g. nested arrays in older
// bd versions) are treated as "blocks" so we don't silently drop them.
func isBlockingDep(e bdShowEntry) bool {
	return e.DependencyType == "" || e.DependencyType == "blocks"
}

// bdShowJSON is the command runner used by the dep helpers to invoke
// `bd show <id> --json`. The variable is package-level so tests can swap it
// for a fake without spawning real subprocesses. The dir parameter is the
// anvil's on-disk path; passing a non-empty value sets cmd.Dir so bd can
// locate the Dolt database.
var bdShowJSON = func(ctx context.Context, dir, beadID string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bd", "show", beadID, "--json")
	if dir != "" {
		cmd.Dir = dir
	}
	executil.HideWindow(cmd)
	return cmd.Output()
}

// maxDepDepth caps the recursion depth on /api/bead/{id}/deps. The modal's
// nested view only needs a couple of levels; 3 is enough for that without
// pathological fan-out on large graphs.
const maxDepDepth = 3

// fetchBeadShow runs `bd show <id> --json` and returns the first entry.
// Empty output, unparseable output, or non-zero exit codes are all reported
// as errors so callers can degrade to empty dep lists. Both the array form
// ([{...}]) and bare object form ({...}) are accepted, and leading/trailing
// diagnostic noise from bd is tolerated via executil.DecodeJSON.
func fetchBeadShow(ctx context.Context, dir, beadID string) (*bdShowEntry, error) {
	out, err := bdShowJSON(ctx, dir, beadID)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("bd show: empty output")
	}
	// Try array form first (most bd versions wrap output in []).
	var entries []bdShowEntry
	if executil.DecodeJSON(out, &entries) == nil && len(entries) > 0 {
		return &entries[0], nil
	}
	// Fall back to bare object form.
	var single bdShowEntry
	if executil.DecodeJSON(out, &single) == nil && single.ID != "" {
		return &single, nil
	}
	return nil, errors.New("bd show: no entries")
}

// anvilLookup resolves a bead ID to its anvil name. The default lookup uses
// the queue cache, which is the only in-process source of bead→anvil
// associations. When the bead is not cached (e.g. closed beads outside the
// queue) the lookup returns "".
type anvilLookup func(beadID string) string

// newAnvilLookup builds a lookup backed by a single queue_cache scan so
// callers can resolve many bead IDs without re-querying the DB. When the
// cache cannot be loaded, the returned lookup always returns "". When a bead
// ID appears in more than one anvil the lookup returns "" to avoid
// non-deterministically attaching the wrong anvil to a dep ref.
func newAnvilLookup(db *state.DB) anvilLookup {
	cache := map[string]string{}
	ambiguous := map[string]bool{}
	if db != nil {
		if items, err := db.QueueCache(); err == nil {
			for _, it := range items {
				if ambiguous[it.BeadID] {
					continue
				}
				if _, ok := cache[it.BeadID]; ok {
					// Bead exists in multiple anvils — omit rather than guess.
					delete(cache, it.BeadID)
					ambiguous[it.BeadID] = true
					continue
				}
				cache[it.BeadID] = it.Anvil
			}
		}
	}
	return func(beadID string) string { return cache[beadID] }
}

// fetchBeadDeps returns the immediate (depth=1) Blocks and BlockedBy lists
// for the given bead. dir is the anvil's on-disk path passed to bdShowJSON.
// Errors yield empty slices so the bead detail handler can degrade gracefully
// when bd is missing or returns unexpected output.
func fetchBeadDeps(ctx context.Context, dir, beadID string, lookup anvilLookup) (blocks, blockedBy []beadDetailDepRef) {
	entry, err := fetchBeadShow(ctx, dir, beadID)
	if err != nil {
		return []beadDetailDepRef{}, []beadDetailDepRef{}
	}
	return extractBeadDeps(entry, lookup)
}

// extractBeadDeps projects an already-fetched bd show entry into immediate
// Blocks/BlockedBy lists. Split out so handleBeadDetail can reuse the same
// `bd show` invocation that supplies Notes.
func extractBeadDeps(entry *bdShowEntry, lookup anvilLookup) (blocks, blockedBy []beadDetailDepRef) {
	blocks = make([]beadDetailDepRef, 0, len(entry.Dependents))
	for _, d := range entry.Dependents {
		if !isBlockingDep(d) {
			continue
		}
		blocks = append(blocks, makeDepRef(d, lookup))
	}
	blockedBy = make([]beadDetailDepRef, 0, len(entry.Dependencies))
	for _, d := range entry.Dependencies {
		if !isBlockingDep(d) {
			continue
		}
		blockedBy = append(blockedBy, makeDepRef(d, lookup))
	}
	return blocks, blockedBy
}

// bdCommentsJSON shells out to `bd comments <id> --json`. The variable is
// package-level so tests can swap it without spawning real subprocesses. The
// dir parameter is the anvil's on-disk path; passing a non-empty value sets
// cmd.Dir so bd can locate the Dolt database.
var bdCommentsJSON = func(ctx context.Context, dir, beadID string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bd", "comments", beadID, "--json")
	if dir != "" {
		cmd.Dir = dir
	}
	executil.HideWindow(cmd)
	return cmd.Output()
}

// fetchBeadComments returns the comment list for the bead, or an empty slice
// on any failure. bd's JSON shape uses `text` for the comment body; this
// helper renames it to `body` for the web layer's response.
func fetchBeadComments(ctx context.Context, dir, beadID string, logger *slog.Logger) []beadDetailComment {
	out, err := bdCommentsJSON(ctx, dir, beadID)
	if err != nil {
		if logger != nil {
			logger.Warn("bd comments failed", "bead_id", beadID, "error", err)
		}
		return []beadDetailComment{}
	}
	if len(out) == 0 {
		return []beadDetailComment{}
	}
	var raw []struct {
		ID        string `json:"id"`
		Author    string `json:"author"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
	}
	if err := executil.DecodeJSON(out, &raw); err != nil {
		if logger != nil {
			logger.Warn("bd comments decode failed", "bead_id", beadID, "error", err)
		}
		return []beadDetailComment{}
	}
	comments := make([]beadDetailComment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, beadDetailComment{
			ID:        r.ID,
			Author:    r.Author,
			Body:      r.Text,
			CreatedAt: r.CreatedAt,
		})
	}
	return comments
}

// makeDepRef projects a bd show entry into the web layer's dep ref shape.
func makeDepRef(e bdShowEntry, lookup anvilLookup) beadDetailDepRef {
	ref := beadDetailDepRef{
		BeadID:   e.ID,
		Title:    e.Title,
		Status:   e.Status,
		Priority: e.Priority,
	}
	if lookup != nil {
		ref.Anvil = lookup(e.ID)
	}
	return ref
}

// walkBeadDeps recursively expands a bead's dep graph up to `depth` levels
// in each direction. dir is the anvil's on-disk path passed to bdShowJSON for
// every fetch in the walk. The `visited` map both detects cycles and prevents
// double-walking diamond-shaped graphs (where a bead is reachable through
// multiple paths). When a bead is encountered that has already been
// walked, its ref is included but its own Blocks/BlockedBy children are
// elided so the response stays a tree rather than a DAG.
func walkBeadDeps(ctx context.Context, dir, beadID string, depth int, lookup anvilLookup, visited map[string]bool) (blocks, blockedBy []beadDetailDepRef) {
	if depth <= 0 {
		return nil, nil
	}
	entry, err := fetchBeadShow(ctx, dir, beadID)
	if err != nil {
		return []beadDetailDepRef{}, []beadDetailDepRef{}
	}
	blocks = make([]beadDetailDepRef, 0, len(entry.Dependents))
	for _, d := range entry.Dependents {
		if !isBlockingDep(d) {
			continue
		}
		ref := makeDepRef(d, lookup)
		if !visited[d.ID] {
			visited[d.ID] = true
			if depth-1 > 0 {
				ref.Blocks, ref.BlockedBy = walkBeadDeps(ctx, dir, d.ID, depth-1, lookup, visited)
			}
		}
		blocks = append(blocks, ref)
	}
	blockedBy = make([]beadDetailDepRef, 0, len(entry.Dependencies))
	for _, d := range entry.Dependencies {
		if !isBlockingDep(d) {
			continue
		}
		ref := makeDepRef(d, lookup)
		if !visited[d.ID] {
			visited[d.ID] = true
			if depth-1 > 0 {
				ref.Blocks, ref.BlockedBy = walkBeadDeps(ctx, dir, d.ID, depth-1, lookup, visited)
			}
		}
		blockedBy = append(blockedBy, ref)
	}
	return blocks, blockedBy
}

// resolveAnvilPath maps an anvil name to its on-disk path using the live
// registry. Returns "" when the name is empty, the registry is not
// configured, or the name is not registered.
func (s *Server) resolveAnvilPath(name string) string {
	if name == "" || s.anvils == nil {
		return ""
	}
	return s.anvils()[name]
}

// handleBeadDetail returns a consolidated view of one bead used by the
// /bead/:id page in the SPA. It bundles the queue cache row, ingot record,
// retry/cost state, recent events, PR summaries, and worker history into a
// single response so the page only needs one round-trip.
func (s *Server) handleBeadDetail(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	anvilHint := strings.TrimSpace(r.URL.Query().Get("anvil"))

	resp := beadDetailResponse{
		BeadID:    beadID,
		Anvil:     anvilHint,
		Workers:   []beadDetailWorker{},
		Events:    []beadDetailEvent{},
		PRs:       []beadDetailPR{},
		Blocks:    []beadDetailDepRef{},
		BlockedBy: []beadDetailDepRef{},
		Comments:  []beadDetailComment{},
	}

	// Queue cache lookup. The DB only exposes a list helper, so we filter in
	// Go; queue_cache is small (capped at <= a few hundred rows in practice).
	if items, err := s.db.QueueCache(); err == nil {
		for _, it := range items {
			if it.BeadID != beadID {
				continue
			}
			if anvilHint != "" && !strings.EqualFold(it.Anvil, anvilHint) {
				continue
			}
			labels := parseQueueLabels(it.Labels)
			resp.Queue = &beadDetailQueue{
				BeadID:      it.BeadID,
				Anvil:       it.Anvil,
				Title:       it.Title,
				Description: it.Description,
				Priority:    it.Priority,
				Status:      it.Status,
				Section:     string(it.Section),
				Labels:      labels,
				Assignee:    it.Assignee,
			}
			if resp.Anvil == "" {
				resp.Anvil = it.Anvil
			}
			break
		}
	}

	// Ingot lookup. We fall back to GetIngotByBeadID when no anvil hint is
	// available so the bead resolves regardless of which anvil owns it.
	conn := s.db.Conn()
	if conn != nil {
		var ig *ingot.Ingot
		if resp.Anvil != "" {
			ig, _ = ingot.GetIngot(conn, beadID, resp.Anvil)
		} else {
			matches, _ := ingot.GetIngotByBeadID(conn, beadID)
			if len(matches) == 1 {
				ig, _ = ingot.GetIngot(conn, beadID, matches[0].Anvil)
			}
		}
		if ig != nil {
			resp.Ingot = ig
			if resp.Anvil == "" {
				resp.Anvil = ig.Anvil
			}
		}
	}

	// Retry record. Only meaningful when we know the anvil.
	if resp.Anvil != "" {
		if rec, err := s.db.GetRetry(beadID, resp.Anvil); err == nil && rec != nil {
			detail := &beadDetailRetry{
				RetryCount:          rec.RetryCount,
				NeedsHuman:          rec.NeedsHuman,
				ClarificationNeeded: rec.ClarificationNeeded,
				DispatchFailures:    rec.DispatchFailures,
				RecoveryFailures:    rec.RecoveryFailures,
				LastError:           rec.LastError,
			}
			if rec.NextRetry != nil {
				detail.NextRetry = rec.NextRetry.Format(time.RFC3339)
			}
			if !rec.UpdatedAt.IsZero() {
				detail.UpdatedAt = rec.UpdatedAt.Format(time.RFC3339)
			}
			resp.Retry = detail
		}
	}

	// Bead cost.
	if resp.Anvil != "" {
		if c, err := getBeadCost(s.db, beadID, resp.Anvil); err == nil && c != nil {
			resp.Cost = c
		}
	}

	// Workers for this bead — queried directly by bead_id so the load is
	// proportional to this bead's history, not the entire workers table.
	if workers, err := s.db.WorkersByBead(beadID, resp.Anvil, 200); err == nil {
		for _, ww := range workers {
			row := beadDetailWorker{
				ID:        ww.ID,
				Status:    string(ww.Status),
				Phase:     ww.Phase,
				Branch:    ww.Branch,
				StartedAt: ww.StartedAt.Format(time.RFC3339),
				LogPath:   ww.LogPath,
				PRNumber:  ww.PRNumber,
			}
			if ww.CompletedAt != nil {
				row.CompletedAt = ww.CompletedAt.Format(time.RFC3339)
				row.DurationSec = ww.CompletedAt.Sub(ww.StartedAt).Seconds()
			}
			resp.Workers = append(resp.Workers, row)
		}
	}

	// Events for this bead. We pull a generous chunk and filter rather than
	// adding a dedicated DB helper, since the events table stays small in
	// practice (<<10k rows for typical setups).
	if events, err := s.db.RecentEvents(500); err == nil {
		for _, e := range events {
			if e.BeadID != beadID {
				continue
			}
			if resp.Anvil != "" && e.Anvil != "" && !strings.EqualFold(e.Anvil, resp.Anvil) {
				continue
			}
			resp.Events = append(resp.Events, beadDetailEvent{
				ID:        e.ID,
				Timestamp: e.Timestamp.Format(time.RFC3339),
				Type:      string(e.Type),
				Message:   e.Message,
			})
		}
		sort.SliceStable(resp.Events, func(i, j int) bool {
			return resp.Events[i].ID > resp.Events[j].ID
		})
	}

	// PRs for this bead. The state package does not expose a per-bead
	// helper, so we union the open PR listing with any PRs found via
	// PRByBeadAnvil-style lookups by scanning the conn directly via raw
	// SQL would be a layering violation — instead we iterate OpenPRs and
	// the worker history, and resolve any worker-bound PR numbers.
	prs := collectBeadPRs(s.db, beadID, resp.Anvil)
	resp.PRs = prs

	// Dependency lists + notes share a single `bd show <id> --json` call.
	// Failures fall back to empty slices/strings so the response shape
	// stays stable when bd is missing or returns unexpected output.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	anvilPath := s.resolveAnvilPath(resp.Anvil)
	if entry, err := fetchBeadShow(ctx, anvilPath, beadID); err == nil && entry != nil {
		resp.Notes = entry.Notes
		resp.Blocks, resp.BlockedBy = extractBeadDeps(entry, newAnvilLookup(s.db))
	}

	// Comments come from a separate `bd comments <id> --json` shell-out.
	// Errors are non-fatal: the rest of the page still renders.
	resp.Comments = fetchBeadComments(ctx, anvilPath, beadID, s.logger)

	writeJSON(w, http.StatusOK, resp)
}

// handleBeadDeps walks the dependency graph for the given bead and returns
// a tree shape that the modal/graph views in the SPA consume. The depth
// query parameter defaults to 1 and is clamped to [1, maxDepDepth].
func (s *Server) handleBeadDeps(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	depth := 1
	if raw := r.URL.Query().Get("depth"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "depth must be an integer")
			return
		}
		if n < 1 {
			n = 1
		} else if n > maxDepDepth {
			n = maxDepDepth
		}
		depth = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Seed the visited set with the root bead so it cannot reappear as a
	// child of itself (e.g. a corrupt or pathological dep graph).
	lookup := newAnvilLookup(s.db)
	visited := map[string]bool{beadID: true}
	blocks, blockedBy := walkBeadDeps(ctx, s.resolveAnvilPath(lookup(beadID)), beadID, depth, lookup, visited)
	if blocks == nil {
		blocks = []beadDetailDepRef{}
	}
	if blockedBy == nil {
		blockedBy = []beadDetailDepRef{}
	}
	writeJSON(w, http.StatusOK, beadDepsResponse{
		BeadID:    beadID,
		Depth:     depth,
		Blocks:    blocks,
		BlockedBy: blockedBy,
	})
}

// getBeadCost returns the cumulative bead_costs row for (beadID, anvil), or
// nil when no row exists. We inline the SQL here rather than threading a
// new helper through the state package since the schema is stable. Called
// by handleBeadDetail and the costs view.
func getBeadCost(db *state.DB, beadID, anvil string) (*beadDetailCost, error) {
	conn := db.Conn()
	if conn == nil {
		return nil, errors.New("no db conn")
	}
	row := conn.QueryRow(
		`SELECT input_tokens, output_tokens, cache_read, cache_write, estimated_cost, updated_at
		 FROM bead_costs WHERE bead_id = ? AND anvil = ?`, beadID, anvil)
	var c beadDetailCost
	var updated string
	if err := row.Scan(&c.InputTokens, &c.OutputTokens, &c.CacheRead, &c.CacheWrite, &c.EstimatedCostUSD, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	c.UpdatedAt = updated
	return &c, nil
}

// collectBeadPRs returns PR summaries for a bead by querying the prs table
// directly via state.DB.Conn, which includes open, closed, and merged PRs.
func collectBeadPRs(db *state.DB, beadID, anvil string) []beadDetailPR {
	conn := db.Conn()
	if conn == nil {
		return []beadDetailPR{}
	}
	q := `SELECT id, number, anvil, branch, COALESCE(base_branch,''), status, COALESCE(title,''), created_at, last_checked
		FROM prs WHERE bead_id = ?`
	args := []any{beadID}
	if anvil != "" {
		q += ` AND anvil = ?`
		args = append(args, anvil)
	}
	q += ` ORDER BY created_at DESC LIMIT 50`
	rows, err := conn.Query(q, args...)
	if err != nil {
		return []beadDetailPR{}
	}
	defer rows.Close()
	out := []beadDetailPR{}
	for rows.Next() {
		var (
			id, number  int
			a, br, base string
			status      string
			title       string
			createdAt   string
			lastChecked sql.NullString
		)
		if err := rows.Scan(&id, &number, &a, &br, &base, &status, &title, &createdAt, &lastChecked); err != nil {
			continue
		}
		row := beadDetailPR{
			ID:         id,
			Number:     number,
			Anvil:      a,
			Branch:     br,
			BaseBranch: base,
			Status:     status,
			Title:      title,
			CreatedAt:  createdAt,
		}
		if lastChecked.Valid {
			row.LastChecked = lastChecked.String
		}
		out = append(out, row)
	}
	return out
}

// parseQueueLabels deserializes a queue_cache labels column. Invalid JSON
// (older rows, manual edits) yields an empty slice rather than an error so
// the API stays best-effort.
func parseQueueLabels(raw string) []string {
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var labels []string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return []string{}
	}
	return labels
}
