package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/go-chi/chi/v5"
)

// resolveAction enumerates the queue-resolution verbs accepted by
// POST /api/forge/resolve. The first five map to the daemon's queue_*
// IPC handlers (Forge-6qh6); approve-as-is and warden-rerun are
// Forge-level dispatch overrides — bypass-warden and re-review-only —
// added in Forge-ts2r to give pod-hosted forges the same escape hatches
// the TUI exposes.
const (
	resolveActionClear       = "clear"
	resolveActionRetry       = "retry"
	resolveActionClarify     = "clarify"
	resolveActionUnclarify   = "unclarify"
	resolveActionStop        = "stop"
	resolveActionApproveAsIs = "approve-as-is"
	resolveActionWardenRerun = "warden-rerun"
	// resolveActionCreatePR opens a PR for an already-pushed forge/<bead>
	// branch without re-running Smith (Part B's openPRForExistingBranch helper,
	// exposed via the create_pr IPC). The Hearth "Create PR" button uses it to
	// recover a bead stuck in pr_create_failed after PR creation failed.
	resolveActionCreatePR = "create-pr"
)

// resolveActionToIPC maps the web-facing action verb to the daemon IPC
// command type. Keeping the table tiny and explicit (rather than computing
// the IPC type at the call site) makes it obvious which verbs the endpoint
// accepts and prevents an unrecognised action from sneaking through to the
// daemon as an "unknown command" error.
var resolveActionToIPC = map[string]string{
	resolveActionClear:       "queue_clear",
	resolveActionRetry:       "queue_retry",
	resolveActionClarify:     "queue_clarify",
	resolveActionUnclarify:   "queue_unclarify",
	resolveActionStop:        "queue_stop",
	resolveActionApproveAsIs: "approve_as_is",
	resolveActionWardenRerun: "warden_rerun",
	resolveActionCreatePR:    "create_pr",
}

// resolveRequest is the JSON body for POST /api/forge/resolve. anvil_name is
// optional in the JSON but required by the daemon; the handler enforces
// that locally so the response is a clean 400 rather than the daemon's
// "bead_id and anvil are required" 500. forge_id is purely passthrough —
// the daemon's multi-forge safety check uses it to refuse cross-forge
// actions; an empty value preserves historical single-forge behaviour.
type resolveRequest struct {
	BeadID    string `json:"bead_id"`
	Action    string `json:"action"`
	AnvilName string `json:"anvil_name,omitempty"`
	Note      string `json:"note,omitempty"`
	ForgeID   string `json:"forge_id,omitempty"`
}

// handleForgeResolve dispatches POST /api/forge/resolve to one of the
// daemon's queue_* IPC verbs. The endpoint is a single entry point for the
// Hearth 2.0 resolve-needs-attention page so the SPA can render a unified
// action picker rather than five distinct buttons hitting five URLs.
func (s *Server) handleForgeResolve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req resolveRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	req.BeadID = strings.TrimSpace(req.BeadID)
	req.Action = strings.TrimSpace(req.Action)
	req.AnvilName = strings.TrimSpace(req.AnvilName)
	req.Note = strings.TrimSpace(req.Note)
	req.ForgeID = strings.TrimSpace(req.ForgeID)

	if !isValidBeadID(req.BeadID) {
		writeError(w, http.StatusBadRequest, "invalid bead_id")
		return
	}
	cmdType, ok := resolveActionToIPC[req.Action]
	if !ok {
		writeError(w, http.StatusBadRequest, "action must be one of clear|retry|clarify|unclarify|stop|approve-as-is|warden-rerun|create-pr")
		return
	}
	if req.AnvilName == "" {
		writeError(w, http.StatusBadRequest, "anvil_name is required")
		return
	}
	// Clarify requires a note (the clarification reason). Catching it here
	// returns a 400 instead of the daemon's 500 "reason is required" path.
	if req.Action == resolveActionClarify && req.Note == "" {
		writeError(w, http.StatusBadRequest, "note is required for clarify")
		return
	}

	s.logActor(r, "forge_resolve", "bead", req.BeadID, "anvil", req.AnvilName, "action", req.Action)
	// approve_as_is and warden_rerun use a different payload shape than the
	// queue_* verbs — just bead_id + anvil, with the legacy "anvil" JSON tag
	// (not "anvil_name"). Dispatch them with the right payload type so the
	// daemon's existing handlers accept the message unchanged.
	switch req.Action {
	case resolveActionApproveAsIs:
		s.dispatchAction(w, cmdType, ipc.ApproveAsIsPayload{
			BeadID: req.BeadID,
			Anvil:  req.AnvilName,
		})
	case resolveActionWardenRerun:
		s.dispatchAction(w, cmdType, ipc.WardenRerunPayload{
			BeadID: req.BeadID,
			Anvil:  req.AnvilName,
		})
	case resolveActionCreatePR:
		// create_pr carries bead_id + anvil only (no note). The daemon runs it
		// synchronously and returns pr_number / pr_url in the ok payload, which
		// dispatchAction forwards verbatim so the SPA can render a PR link.
		s.dispatchAction(w, cmdType, ipc.CreatePRPayload{
			BeadID: req.BeadID,
			Anvil:  req.AnvilName,
		})
	default:
		s.dispatchAction(w, cmdType, ipc.QueueActionPayload{
			BeadID:    req.BeadID,
			ForgeID:   req.ForgeID,
			AnvilName: req.AnvilName,
			Note:      req.Note,
		})
	}
}

// escalationResponse is the JSON shape returned by
// GET /api/forge/escalation/{bead_id}. The page renders the full
// untruncated escalation message alongside the git context the operator
// needs to triage the worker's state — origin branch tip, local worker
// commits, and the diff against the parent base. Fields are best-effort:
// missing worktree directories or unresolvable refs produce empty values
// and an entry in Errors rather than a 5xx, so the SPA can still render
// the message itself.
type escalationResponse struct {
	BeadID            string         `json:"bead_id"`
	Anvil             string         `json:"anvil"`
	Branch            string         `json:"branch,omitempty"`
	WorktreePath      string         `json:"worktree_path,omitempty"`
	WorktreeExists    bool           `json:"worktree_exists"`
	EscalationMessage string         `json:"escalation_message"`
	Retry             *retryDetail   `json:"retry,omitempty"`
	Context           *escalationGit `json:"context,omitempty"`
	Errors            []string       `json:"errors,omitempty"`
}

// retryDetail is a slim projection of the retry row used by the escalation
// page. We expose only the fields the SPA renders so the JSON contract
// does not couple to internal columns that may change.
type retryDetail struct {
	NeedsHuman          bool   `json:"needs_human"`
	ClarificationNeeded bool   `json:"clarification_needed"`
	DispatchFailures    int    `json:"dispatch_failures"`
	RecoveryFailures    int    `json:"recovery_failures"`
	UpdatedAt           string `json:"updated_at,omitempty"`
}

// escalationGit bundles the git context shelled out from the worker's
// worktree. Commits lists are short (capped at maxEscalationCommits) so the
// response stays small even for long-running branches; full diff stats are
// summarised rather than emitted verbatim.
type escalationGit struct {
	ParentBase         string   `json:"parent_base,omitempty"`
	DiffRange          string   `json:"diff_range,omitempty"`
	OriginBranchRef    string   `json:"origin_branch_ref,omitempty"`
	OriginBranchExists bool     `json:"origin_branch_exists"`
	OriginCommits      []string `json:"origin_commits,omitempty"`
	LocalCommits       []string `json:"local_commits,omitempty"`
	DiffStat           string   `json:"diff_stat,omitempty"`
}

// maxEscalationCommits caps the per-side commit list so a long-running
// branch cannot bloat the response. 50 lines comfortably covers the
// typical case (a handful of commits) while still capping pathological
// branches.
const maxEscalationCommits = 50

// gitEnvGetter returns the git environment for confining git to a worktree.
// Package-level so tests can replace it without requiring a real git worktree.
var gitEnvGetter = worktree.GitEnv

// gitRunner runs a git subprocess in the given directory and returns stdout.
// env, when non-nil, replaces the process environment (use worktree.GitEnv to
// confine git to a specific worktree). The variable is package-level so tests
// can swap it without spawning real git processes.
var gitRunner = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	executil.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

// handleForgeEscalation handles GET /api/forge/escalation/{bead_id}. It
// returns the full untruncated escalation message (from the retry row's
// last_error) plus git context shelled from the worker's worktree. The
// optional `anvil` query parameter narrows the lookup when a bead ID exists
// in more than one anvil.
func (s *Server) handleForgeEscalation(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}
	anvilHint := strings.TrimSpace(r.URL.Query().Get("anvil"))

	resp := escalationResponse{
		BeadID: beadID,
		Anvil:  anvilHint,
	}

	// Resolve the owning anvil. When the caller supplied an anvil hint we
	// trust it; otherwise consult the queue cache for the single anvil the
	// bead lives in. Beads that exist in more than one anvil resolve to ""
	// and the caller must retry with ?anvil=.
	if resp.Anvil == "" {
		resp.Anvil = newAnvilLookup(s.db)(beadID)
		if resp.Anvil == "" {
			resp.Errors = append(resp.Errors, "anvil: ambiguous or not found; supply ?anvil= to narrow the lookup")
		}
	}

	// Retry row drives both the escalation message and the retry detail
	// block. A missing row is fine — escalations exist for orphaned beads
	// (no worker) too.
	if resp.Anvil != "" {
		if rec, err := s.db.GetRetry(beadID, resp.Anvil); err != nil {
			resp.Errors = append(resp.Errors, "retry: "+err.Error())
		} else if rec != nil {
			resp.EscalationMessage = rec.LastError
			detail := &retryDetail{
				NeedsHuman:          rec.NeedsHuman,
				ClarificationNeeded: rec.ClarificationNeeded,
				DispatchFailures:    rec.DispatchFailures,
				RecoveryFailures:    rec.RecoveryFailures,
			}
			if !rec.UpdatedAt.IsZero() {
				detail.UpdatedAt = rec.UpdatedAt.Format(time.RFC3339)
			}
			resp.Retry = detail
		}
	}

	// Resolve the branch and worktree path. Branch defaults to the most
	// recent worker row's branch; worktree path is derived from the
	// anvil's on-disk path (<anvil>/.workers/<sanitized-bead>).
	if resp.Anvil != "" {
		if branch, err := s.db.LastWorkerBranchForBead(beadID, resp.Anvil); err == nil {
			resp.Branch = branch
		}
	}
	anvilPath := s.resolveAnvilPath(resp.Anvil)
	if anvilPath != "" {
		resp.WorktreePath = filepath.Join(anvilPath, ".workers", worktree.SanitizePath(beadID))
		if info, err := os.Stat(resp.WorktreePath); err == nil && info.IsDir() {
			resp.WorktreeExists = true
		}
	}

	// Gather git context when we have a worktree to read from. GitEnv
	// pins git to the worktree so it cannot walk up into the anvil repo;
	// a nil return means the directory is not a valid worktree and we skip
	// the git calls entirely rather than risk cross-branch results.
	if resp.WorktreeExists && resp.Branch != "" {
		gitEnv := gitEnvGetter(resp.WorktreePath)
		if gitEnv == nil {
			resp.Errors = append(resp.Errors, "worktree: directory exists but is not a valid git worktree")
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			resp.Context = gatherEscalationContext(ctx, resp.WorktreePath, resp.Branch, gitEnv, &resp.Errors)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// gatherEscalationContext shells to git in the worker's worktree to
// produce the parent base, the origin-side and local-side commit lists,
// and a diff summary. env must be a non-nil slice from worktree.GitEnv
// so that git is confined to the worktree and cannot walk up into the
// parent anvil repo. Each step is independent: a failure in one does
// not prevent the others from running; errors land in *outErrs.
func gatherEscalationContext(ctx context.Context, worktreePath, branch string, env []string, outErrs *[]string) *escalationGit {
	ctxOut := &escalationGit{}

	addErr := func(stage string, err error) {
		if err == nil {
			return
		}
		*outErrs = append(*outErrs, stage+": "+err.Error())
	}

	// Parent base: origin/main, falling back to origin/master.
	if out, err := gitRunner(ctx, worktreePath, env, "rev-parse", "--verify", "--quiet", "origin/main"); err == nil && len(out) > 0 {
		ctxOut.ParentBase = "origin/main"
	} else if out, err := gitRunner(ctx, worktreePath, env, "rev-parse", "--verify", "--quiet", "origin/master"); err == nil && len(out) > 0 {
		ctxOut.ParentBase = "origin/master"
	} else {
		addErr("parent_base", errors.New("neither origin/main nor origin/master found"))
	}

	// Local commits on the worker branch versus the parent base.
	if ctxOut.ParentBase != "" {
		ctxOut.DiffRange = ctxOut.ParentBase + "..HEAD"
		if out, err := gitRunner(ctx, worktreePath, env, "log",
			"--pretty=format:%h %s",
			"-n", strconv.Itoa(maxEscalationCommits),
			ctxOut.DiffRange,
		); err == nil {
			ctxOut.LocalCommits = splitNonEmptyLines(string(out))
		} else {
			addErr("local_commits", err)
		}

		// Diff stat against the parent base — short summary so the
		// response stays bounded even for large diffs.
		if out, err := gitRunner(ctx, worktreePath, env, "diff", "--shortstat", ctxOut.DiffRange); err == nil {
			ctxOut.DiffStat = strings.TrimSpace(string(out))
		} else {
			addErr("diff_stat", err)
		}
	}

	// Origin-side commits on the same branch. The worker may have
	// already pushed; the SPA shows the divergence between origin/<branch>
	// (what reviewers see) and the local branch (what's still in the pod
	// worktree). When the branch has not been pushed yet, origin lookup
	// fails cleanly and OriginBranchExists stays false.
	ctxOut.OriginBranchRef = "origin/" + branch
	if out, err := gitRunner(ctx, worktreePath, env, "rev-parse", "--verify", "--quiet", ctxOut.OriginBranchRef); err == nil && len(out) > 0 {
		ctxOut.OriginBranchExists = true
		if logOut, logErr := gitRunner(ctx, worktreePath, env, "log",
			"--pretty=format:%h %s",
			"-n", strconv.Itoa(maxEscalationCommits),
			ctxOut.OriginBranchRef,
		); logErr == nil {
			ctxOut.OriginCommits = splitNonEmptyLines(string(logOut))
		} else {
			addErr("origin_commits", logErr)
		}
	}

	return ctxOut
}

// splitNonEmptyLines splits stdout by \n and trims blank entries. git
// log's --pretty output never has trailing newlines but its CombinedOutput
// rarely does either; the helper handles both.
func splitNonEmptyLines(s string) []string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
