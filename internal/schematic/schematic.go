// Package schematic implements the pre-worker that analyses bead scope before
// Smith starts. The schematic is drawn before the smith starts forging.
//
// Schematic can:
//  1. Emit a focused implementation plan appended to Smith's prompt
//  2. Decompose large beads into sub-beads via bd, blocking the parent
//  3. Skip entirely for small/simple beads
//  4. Request human clarification for ambiguous beads and block work until clarified
package schematic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// Action describes what the Schematic decided to do.
type Action string

const (
	// ActionPlan means the bead is implementable as-is; a focused plan was
	// produced to guide Smith.
	ActionPlan Action = "plan"

	// ActionDecompose means the bead was too large or multi-part and has been
	// split into sub-beads. The parent bead should be blocked.
	ActionDecompose Action = "decompose"

	// ActionSkip means the bead was simple enough that no schematic was needed.
	ActionSkip Action = "skip"

	// ActionClarify means the bead requires human clarification and should not
	// be worked on yet.
	ActionClarify Action = "clarify"

	// ActionCrucible means the bead has children that need to be orchestrated
	// together on a feature branch (crucible mode).
	ActionCrucible Action = "crucible"

	// ActionAlreadyDecomposed means the bead was previously decomposed into
	// children and kept open for dependents. Now that it is re-dispatched there
	// is no work left — the children were the work.
	ActionAlreadyDecomposed Action = "already_decomposed"
)

// LabelDecomposed is the label the daemon attaches to a decomposed parent bead
// when it must stay open (has dependents). Schematic detects this on
// re-dispatch and returns ActionAlreadyDecomposed so no smith is spawned.
const LabelDecomposed = "forge-decomposed"

// SchematicChildLabelPrefix is prepended to a parent bead ID to form the marker
// label attached to every sub-bead created by a schematic decomposition
// (e.g. "schematic:Forge-abc1"). It lets a later re-decomposition reliably
// detect children created by a previous — possibly partial — pass and reuse
// them instead of creating a duplicate set alongside the orphans.
const SchematicChildLabelPrefix = "schematic:"

// schematicChildLabel returns the marker label attached to sub-beads created
// when decomposing the given parent.
func schematicChildLabel(parentID string) string {
	return SchematicChildLabelPrefix + parentID
}

// Event kinds surfaced via Config.OnEvent. These make otherwise-silent
// schematic outcomes visible in the activity feed. The values match the
// state.EventType constants the pipeline logs them under.
const (
	// EventKindDecomposeFailed is emitted when decomposition fails partway
	// through creating sub-beads (a mid-loop create/parse/dep failure). Some
	// sub-beads may already exist; they are reused on the next re-decomposition
	// rather than duplicated.
	EventKindDecomposeFailed = "schematic_decompose_failed"

	// EventKindParseFailed is emitted when the AI verdict could not be parsed
	// and the schematic skipped rather than acting on unstructured output.
	EventKindParseFailed = "schematic_parse_failed"
)

// SubBead holds the ID and title of a created sub-bead.
type SubBead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Result captures the outcome of a Schematic analysis.
type Result struct {
	// Action is what the Schematic decided.
	Action Action
	// Plan is a focused implementation plan for Smith (only when Action=ActionPlan).
	Plan string
	// SubBeads is the list of sub-beads created (only when Action=ActionDecompose).
	SubBeads []SubBead
	// Reason is a human-readable explanation of the decision.
	Reason string
	// Duration is how long the analysis took.
	Duration time.Duration
	// CostUSD is the estimated cost of the AI session.
	CostUSD float64
	// Quota holds rate-limit quota data from the AI session, if available.
	Quota *provider.Quota
	// Error is set if the schematic failed.
	Error error
}

// CrucibleCheckResult captures whether a bead with children needs crucible
// orchestration or can be dispatched as a standalone bead.
type CrucibleCheckResult struct {
	// NeedsCrucible is true when the children should be orchestrated on a
	// feature branch rather than dispatched individually.
	NeedsCrucible bool
	// Reason explains the decision.
	Reason string
	// Duration is how long the check took.
	Duration time.Duration
	// CostUSD is the estimated cost of the AI session.
	CostUSD float64
	// Quota holds rate-limit quota data from the AI session, if available.
	Quota *provider.Quota
}

// Config controls Schematic behavior.
type Config struct {
	// Enabled controls whether Schematic runs at all. Defaults to true when
	// the global setting is enabled.
	Enabled bool
	// WordThreshold is the minimum word count in the bead description to
	// trigger automatic schematic analysis. Beads below this are skipped
	// unless they have the decompose tag. Default: 100.
	WordThreshold int
	// MaxTurns limits the AI session length. Default: 5. Kept low because
	// the schematic should emit its JSON verdict immediately without tool use.
	// Note: --max-turns is best-effort and only honored by providers that
	// support the flag (e.g. Claude CLI). Gemini and GitHub Copilot CLI
	// adapters drop this flag, so enforcement is provider-dependent.
	MaxTurns int
	// ExtraFlags are additional CLI flags forwarded to the Claude session
	// (e.g. model selection, auth tokens). Mirrors pipeline.Params.ExtraFlags.
	ExtraFlags []string
	// OnSpawn is an optional callback invoked immediately after the AI
	// subprocess is started, before waiting for it to finish. It receives the
	// process PID and the path to the session log file. Use this to update
	// monitoring records (e.g. the worker DB row) so that live-tail and
	// progress tracking work during the schematic phase.
	OnSpawn func(pid int, logPath string)
	// LogDir, when set, is where the schematic session log is written — a
	// durable location that survives the temp-workdir cleanup and passes the
	// Hearth log allowlist, so live panels can stream it and lingering panels
	// can tail it (Forge-x8ew). Typically the worktree's .forge-logs directory
	// (pipeline) or ~/.forge/logs/<bead> (daemon crucible check). When empty,
	// the log lands inside the temp workdir and dies with it.
	LogDir string
	// OnEvent, when set, surfaces notable schematic sub-events to the caller so
	// they can be recorded in the activity feed. It is used for partial
	// decomposition failures and verdict-parse skips, which would otherwise be
	// invisible. The kind is one of the EventKind* constants; message is a
	// human-readable description. Must be nil-safe (callers may leave it unset).
	OnEvent func(kind, message string)
}

// emitEvent invokes the OnEvent callback if one is configured. It is a no-op
// otherwise so callers can emit unconditionally.
func (c Config) emitEvent(kind, message string) {
	if c.OnEvent != nil {
		c.OnEvent(kind, message)
	}
}

// DefaultConfig returns sensible defaults for Schematic.
func DefaultConfig() Config {
	return Config{
		Enabled:       false,
		WordThreshold: 100,
		MaxTurns:      5,
	}
}

// ShouldRun determines whether the Schematic should analyse this bead based
// on the configuration and bead metadata.
func ShouldRun(cfg Config, bead poller.Bead) bool {
	if !cfg.Enabled {
		return false
	}

	// Explicit skip tag — used for beads that are already part of a
	// manually decomposed chain and should not be auto-decomposed.
	for _, tag := range bead.Labels {
		if strings.EqualFold(tag, "no-decompose") {
			return false
		}
	}

	// Explicit tag always triggers
	for _, tag := range bead.Labels {
		if strings.EqualFold(tag, "decompose") {
			return true
		}
	}

	// Word-count heuristic over the full spec, not just the description: a
	// terse description with rich design/acceptance criteria is still a complex
	// bead worth analysing (and likely decomposing).
	wordCount := len(strings.Fields(bead.Description + " " + bead.Design + " " + bead.AcceptanceCriteria))
	return wordCount >= cfg.WordThreshold
}

// subTaskVerdict holds a single sub-task from the AI decomposition verdict.
type subTaskVerdict struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// UnmarshalJSON supports both the new object format {"title":"...","description":"..."}
// and the legacy plain string format "Task title" for backward compatibility.
func (s *subTaskVerdict) UnmarshalJSON(data []byte) error {
	// Try as a plain string first (legacy format).
	var plain string
	if err := json.Unmarshal(data, &plain); err == nil {
		s.Title = plain
		s.Description = ""
		return nil
	}
	// Otherwise, decode as an object.
	type alias subTaskVerdict
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = subTaskVerdict(a)
	return nil
}

// schematicVerdict is the JSON structure we ask the AI to produce.
type schematicVerdict struct {
	Action   string           `json:"action"`
	Plan     string           `json:"plan,omitempty"`
	SubTasks []subTaskVerdict `json:"sub_tasks,omitempty"`
	Reason   string           `json:"reason"`
}

// Run executes the Schematic analysis for a bead. It spawns a lightweight AI
// session to determine whether the bead should be decomposed, planned, or
// skipped.
func Run(ctx context.Context, cfg Config, bead poller.Bead, anvilPath string, pv provider.Provider) *Result {
	start := time.Now()

	// If the bead was previously decomposed and kept open for dependents,
	// skip immediately — the children were the work, nothing left to do.
	for _, lbl := range bead.Labels {
		if strings.EqualFold(lbl, LabelDecomposed) {
			return &Result{
				Action:   ActionAlreadyDecomposed,
				Reason:   "Previously decomposed into children; no remaining work",
				Duration: time.Since(start),
			}
		}
	}

	if !ShouldRun(cfg, bead) {
		return &Result{
			Action:   ActionSkip,
			Reason:   "Below complexity threshold",
			Duration: time.Since(start),
		}
	}

	promptText := buildPrompt(bead)

	log.Printf("[schematic:%s] Analysing bead scope (provider: %s)", bead.ID, pv.Label())

	// Run the AI session in a temp dir so the schematic session cannot modify
	// the main repo.
	workDir, err := os.MkdirTemp("", "forge-schematic-*")
	if err != nil {
		return &Result{
			Action:   ActionSkip,
			Reason:   fmt.Sprintf("Failed to create temp dir: %v", err),
			Duration: time.Since(start),
			Error:    fmt.Errorf("creating schematic workdir: %w", err),
		}
	}
	defer os.RemoveAll(workDir)

	// Write the session log straight into the caller's durable log dir when
	// one is provided (the pipeline's worktree .forge-logs, or the daemon's
	// ~/.forge/logs/<bead> for crucible checks). The temp workdir is deleted
	// the moment the session ends, and its path fails the Hearth log
	// allowlist — so a panel could neither stream the log live (the 400
	// permanently closed the browser's EventSource, leaving "reconnecting…")
	// nor tail it after completion (Forge-x8ew).
	logDir := filepath.Join(workDir, "logs")
	if cfg.LogDir != "" {
		logDir = cfg.LogDir
	}
	extraFlags := append([]string{"--max-turns", fmt.Sprintf("%d", cfg.MaxTurns)}, cfg.ExtraFlags...)
	process, err := smith.SpawnWithOptions(ctx, workDir, promptText, logDir, pv, extraFlags, smith.SpawnOptions{LogPrefix: "schematic"})
	if err != nil {
		return &Result{
			Action:   ActionSkip,
			Reason:   fmt.Sprintf("Failed to spawn schematic session: %v", err),
			Duration: time.Since(start),
			Error:    fmt.Errorf("spawning schematic: %w", err),
		}
	}
	if cfg.OnSpawn != nil {
		cfg.OnSpawn(process.PID, process.LogPath)
	}

	smithResult := process.Wait()

	result := &Result{
		Duration: time.Since(start),
		CostUSD:  smithResult.CostUSD,
		Quota:    smithResult.Quota,
	}

	if smithResult.RateLimited {
		result.Action = ActionSkip
		result.Reason = "Rate limited — skipping schematic"
		result.Error = fmt.Errorf("schematic rate limited")
		return result
	}

	if smithResult.ExitCode != 0 {
		result.Action = ActionSkip
		result.Reason = fmt.Sprintf("Schematic session failed (exit %d) — skipping", smithResult.ExitCode)
		result.Error = fmt.Errorf("schematic exit code %d", smithResult.ExitCode)
		return result
	}

	// Parse structured verdict from output — prefer FullOutput (natural-language
	// response) over Output (raw stream-JSON protocol lines).
	output := smithResult.FullOutput
	if output == "" {
		output = smithResult.Output
	}
	verdict, err := parseVerdict(output)
	if err != nil {
		// On parse failure, skip rather than block the pipeline. Emit an event
		// so the skip is visible in the activity feed instead of being silent.
		result.Action = ActionSkip
		result.Reason = fmt.Sprintf("Could not parse schematic output — skipping: %v", err)
		result.Error = err
		cfg.emitEvent(EventKindParseFailed,
			fmt.Sprintf("Schematic verdict parse failed for %s — skipping: %v", bead.ID, err))
		return result
	}

	switch verdict.Action {
	case "plan":
		result.Action = ActionPlan
		result.Plan = verdict.Plan
		result.Reason = verdict.Reason

	case "decompose":
		result.Action = ActionDecompose
		result.Reason = verdict.Reason
		// Create sub-beads via bd
		subs, err := createSubBeads(ctx, bead, verdict.SubTasks, anvilPath, defaultRunCmd)
		if err != nil {
			// Failed to create sub-beads — escalate to ActionClarify (not ActionSkip) so the
			// pipeline releases the bead for human attention rather than silently continuing.
			// The partial sub-beads carry the schematic:<parent> marker label, so a later
			// re-decomposition reuses them instead of creating duplicates (see createSubBeads).
			log.Printf("[schematic:%s] Failed to create sub-beads: %v (partial: %v)", bead.ID, err, subs)
			result.Action = ActionClarify
			result.SubBeads = subs // preserve partial sub-beads for caller visibility
			result.Reason = fmt.Sprintf("Automatic decomposition failed, bead requires manual review: %v", err)
			result.Error = err
			// Emit an event so the partial failure is visible in the activity
			// feed rather than only surfacing as a generic clarification.
			cfg.emitEvent(EventKindDecomposeFailed,
				fmt.Sprintf("Partial decomposition of %s failed after creating %d sub-bead(s): %v",
					bead.ID, len(subs), err))
		} else {
			result.SubBeads = subs
		}

	case "clarify":
		result.Action = ActionClarify
		result.Reason = verdict.Reason

	default:
		result.Action = ActionSkip
		result.Reason = fmt.Sprintf("Unknown action %q — skipping", verdict.Action)
	}

	return result
}

// parseVerdict extracts the structured JSON verdict from the AI output.
func parseVerdict(output string) (*schematicVerdict, error) {
	// Try to find JSON block in ```json ... ``` fences
	if idx := strings.Index(output, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(output[start:], "```"); end >= 0 {
			var v schematicVerdict
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &v); err == nil {
				return &v, nil
			}
		}
	}

	// Try plain ``` fences containing "action"
	if idx := strings.Index(output, "```"); idx >= 0 {
		start := idx + 3
		// Skip optional language tag on same line
		if nl := strings.Index(output[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(output[start:], "```"); end >= 0 {
			block := strings.TrimSpace(output[start : start+end])
			if strings.Contains(block, "action") {
				var v schematicVerdict
				if err := json.Unmarshal([]byte(block), &v); err == nil {
					return &v, nil
				}
			}
		}
	}

	// Scan for raw JSON objects with "action"
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		// Find matching closing brace
		depth := 0
		for j := i; j < len(output); j++ {
			switch output[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := output[i : j+1]
					if strings.Contains(block, `"action"`) {
						var v schematicVerdict
						if err := json.Unmarshal([]byte(block), &v); err == nil {
							return &v, nil
						}
					}
					i = j // skip past this block
					break
				}
			}
			if depth == 0 {
				break
			}
		}
	}

	return nil, fmt.Errorf("no valid schematic verdict JSON found in output")
}

// bdRunner executes a bd command with a timeout and returns combined output.
// It is a function type so tests can inject a fake without spawning real processes.
type bdRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// defaultRunCmd is the production bdRunner that delegates to the real bd CLI.
func defaultRunCmd(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd, cancel := executil.BdCommand(ctx, args...)
	defer cancel()
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// depRetryAttempts is the maximum number of times addSequentialDepWithRetry
// will invoke bd dep add (initial try + retries) before surfacing an error.
// It is a var so tests can tighten the budget when exercising retry paths.
var depRetryAttempts = 3

// depRetryBackoffs is the sleep schedule applied between attempts. There is
// no sleep before the first attempt and no sleep after the last attempt, so
// this slice holds depRetryAttempts-1 entries. It is a var so tests can
// override it to avoid real delays.
var depRetryBackoffs = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
}

// depRetrySleep is the sleep implementation used between dep-add retries.
// Tests override this to avoid real waits.
var depRetrySleep = time.Sleep

// isTransientDepErr reports whether a bd dep add failure looks like a
// transient Dolt/MySQL connectivity blip that should be retried. Permanent
// errors (cycle detection, missing parent, validation failures) must NOT
// match — those need to fail fast so operators see them immediately.
func isTransientDepErr(err error, output []byte) bool {
	if err == nil {
		return false
	}
	combined := strings.ToLower(err.Error() + "\n" + string(output))
	// Markers observed in production: the MySQL driver surfaces connection
	// drops as "invalid connection" and network timeouts as "i/o timeout"
	// (often prefixed with "[mysql]"). Match broadly on those substrings.
	for _, marker := range []string{
		"i/o timeout",
		"invalid connection",
	} {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	return false
}

// depEdgeAlreadyExists reports whether bd's output indicates the dependency
// edge already exists. This happens during idempotent re-decomposition when a
// reused child's sequential link was created by an earlier pass. Matching is
// case-insensitive and broad to tolerate bd wording variations.
func depEdgeAlreadyExists(out []byte) bool {
	lower := strings.ToLower(string(out))
	for _, marker := range []string{
		"already exists",
		"already depends",
		"duplicate dependency",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// addSequentialDepWithRetry invokes `bd dep add child prev` with short
// exponential-backoff retries on transient Dolt/MySQL connectivity failures.
// Each attempt applies the existing tolerance for bd's quirk of exiting 1
// while stdout confirms the dependency was actually added. Permanent errors
// (see isTransientDepErr) fail fast. Returns the last command output, the
// number of attempts actually made, and the terminal error (nil on success).
func addSequentialDepWithRetry(ctx context.Context, anvilPath, parentID, childID, prevID string, run bdRunner) ([]byte, int, error) {
	var lastOut []byte
	var lastErr error
	for attempt := 1; attempt <= depRetryAttempts; attempt++ {
		depCtx, depCancel := context.WithTimeout(ctx, executil.BdTimeout())
		out, err := run(depCtx, anvilPath, "dep", "add", childID, prevID)
		depCtxErr := depCtx.Err() // capture before cancel clears it
		depCancel()
		// The bd helper bounds the call itself, so a deadline kill can surface
		// as *executil.BdTimeoutError while depCtx is still live. Either signal
		// means the attempt ran out of time.
		if depCtxErr == nil && errors.Is(err, context.DeadlineExceeded) {
			depCtxErr = context.DeadlineExceeded
		}

		if err == nil {
			return out, attempt, nil
		}

		// Tolerate bd's quirk: exit code 1 with "Added dependency" in stdout
		// means the dependency was actually added. Only accept this when the
		// context was not canceled/expired (to avoid masking real timeouts).
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && depCtxErr == nil &&
			strings.Contains(string(out), "Added dependency") {
			log.Printf("[schematic:%s] bd dep add exited non-zero but dependency was added (%s -> %s), ignoring exit code: %v",
				parentID, childID, prevID, err)
			return out, attempt, nil
		}

		// Idempotent re-decomposition reuses children from a prior pass, so the
		// sequential dependency may already exist. bd reports this as a failure,
		// but for our purposes the desired edge is present — treat it as success.
		if depCtxErr == nil && depEdgeAlreadyExists(out) {
			log.Printf("[schematic:%s] Sequential dependency %s -> %s already exists, treating as success: %v",
				parentID, childID, prevID, err)
			return out, attempt, nil
		}

		lastOut, lastErr = out, err

		// Permanent errors must not be retried — surface them immediately so
		// operators can act.
		if !isTransientDepErr(err, out) {
			return out, attempt, err
		}

		if attempt < depRetryAttempts {
			idx := attempt - 1
			if idx >= len(depRetryBackoffs) {
				idx = len(depRetryBackoffs) - 1
			}
			backoff := depRetryBackoffs[idx]
			log.Printf("[schematic:%s] Transient bd dep add error on attempt %d/%d (%s -> %s): %v — retrying in %s",
				parentID, attempt, depRetryAttempts, childID, prevID, err, backoff)
			depRetrySleep(backoff)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, attempt, ctxErr
			}
		}
	}
	return lastOut, depRetryAttempts, lastErr
}

// createSubBeads creates sub-beads via bd CLI with blocking dependency links.
// Each sub-bead blocks the parent so that bd ready excludes the parent until
// all sub-beads are closed. Children are chained sequentially (child N+1
// depends on child N) so the poller dispatches them in the order the AI
// specified. If adding a sequential dependency fails the function returns an
// error immediately (with the partial list of already-created sub-beads) so
// the caller can escalate to ActionClarify and prevent out-of-order dispatch.
//
// Creation is idempotent: every sub-bead is tagged with a schematic:<parent-id>
// marker label, and before creating anything the function queries bd for
// children that already carry that marker (created by a previous, possibly
// partial, decomposition pass). A task whose title matches an existing marked
// child reuses that child's ID instead of creating a duplicate. This guarantees
// that after a mid-loop failure and a subsequent re-decomposition, bd ends up
// with exactly one coherent set of children — no orphans, no duplicates.
func createSubBeads(ctx context.Context, parent poller.Bead, tasks []subTaskVerdict, anvilPath string, run bdRunner) ([]SubBead, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no sub-tasks to create")
	}

	marker := schematicChildLabel(parent.ID)

	// Idempotency: detect children left behind by a previous decomposition pass
	// so they are reused rather than duplicated. Best-effort — on any query
	// failure we proceed with an empty set (worst case matches the old behavior
	// of always creating fresh).
	existingByTitle := findExistingChildren(ctx, anvilPath, marker, run)

	resetParent := func() {
		rCtx, rCancel := context.WithTimeout(context.Background(), executil.BdTimeout())
		defer rCancel()
		if out, err := run(rCtx, anvilPath, "update", parent.ID, "--status=open", "--json"); err != nil {
			log.Printf("[schematic:%s] Warning: failed to reset parent to open: %v: %s", parent.ID, err, out)
		}
	}

	var subBeads []SubBead
	for _, task := range tasks {
		// Idempotent reuse: if a marker-labeled child with this title already
		// exists (from a prior partial pass), reuse it instead of creating a
		// duplicate. The sequential-dependency wiring below runs the same way
		// for reused children.
		if id, ok := existingByTitle[task.Title]; ok && id != "" {
			log.Printf("[schematic:%s] Reusing existing sub-bead %s for task %q (idempotent re-decomposition)", parent.ID, id, task.Title)
			subBeads = append(subBeads, SubBead{ID: id, Title: task.Title})
			if err := chainSequentialDep(ctx, anvilPath, parent, subBeads, run); err != nil {
				resetParent()
				return subBeads, err
			}
			continue
		}

		// Build the description: use the AI-generated description if available,
		// otherwise fall back to a generic placeholder.
		desc := task.Description
		if desc == "" {
			desc = "Sub-task decomposed from " + parent.ID + ": " + parent.Title
		}

		// Create sub-bead with blocks dependency so the parent is blocked
		// until all sub-beads are closed. The marker label lets a later
		// re-decomposition detect and reuse this child instead of duplicating.
		createCtx, cancel := context.WithTimeout(ctx, executil.BdTimeout())
		out, err := run(createCtx, anvilPath,
			"create",
			"--title="+task.Title,
			"--description="+desc,
			"--type=task",
			fmt.Sprintf("--priority=%d", parent.Priority),
			"--deps", "blocks:"+parent.ID,
			"--labels", marker,
			"--json",
		)
		cancel()
		if err != nil {
			log.Printf("[schematic:%s] Sub-bead creation failed after %d/%d sub-beads (task %q): %v", parent.ID, len(subBeads), len(tasks), task.Title, err)
			resetParent()
			return subBeads, fmt.Errorf("sub-bead creation failed %q: %w: %s", task.Title, err, out)
		}

		// Extract ID from JSON output. bd may emit trailing diagnostics
		// (e.g. orphan detection warnings) after the JSON object, so use a
		// streaming decoder that tolerates trailing data.
		var created struct {
			ID string `json:"id"`
		}
		if err := executil.DecodeJSON(out, &created); err != nil {
			log.Printf("[schematic:%s] Sub-bead creation failed after %d/%d sub-beads: could not parse ID from bd output", parent.ID, len(subBeads), len(tasks))
			resetParent()
			return subBeads, fmt.Errorf("sub-bead creation failed parsing ID: %w: %s", err, out)
		}
		if created.ID == "" {
			log.Printf("[schematic:%s] Sub-bead creation failed after %d/%d sub-beads: missing id in bd create JSON", parent.ID, len(subBeads), len(tasks))
			resetParent()
			return subBeads, fmt.Errorf("sub-bead creation failed: missing id in bd create JSON: %s", out)
		}

		subBeads = append(subBeads, SubBead{ID: created.ID, Title: task.Title})

		if err := chainSequentialDep(ctx, anvilPath, parent, subBeads, run); err != nil {
			resetParent()
			return subBeads, err
		}
	}

	// All tasks were created or reused successfully. Close any leftover
	// marker-labeled children from a previous pass that were NOT reused this
	// time — this happens when a re-decomposition produces different task
	// titles, which would otherwise leave the old children as orphans alongside
	// the new set. Closing them guarantees exactly one coherent set of children.
	// Best-effort: failures are logged, not fatal.
	closeSupersededChildren(ctx, parent, subBeads, existingByTitle, anvilPath, run)

	// Transfer chain relationships from the parent to the first/last sub-bead.
	// This preserves the dependency chain through decomposition so that:
	//   - upstream blockers still prevent B1 from starting prematurely, and
	//   - downstream beads stay blocked until B3 (the last sub-bead) completes,
	//     even if the parent bead is auto-closed after decomposition.
	//
	// NOTE: parent.Blocks and parent.DependsOn may be empty because the bd
	// JSON output uses "dependencies" and "dependents" fields (not "blocks"
	// and "depends_on"). We re-fetch the parent via "bd show --json" and
	// parse the dependents/dependencies to get accurate data.
	allBlocksTransferred := true
	if len(subBeads) > 0 {
		first := subBeads[0]
		last := subBeads[len(subBeads)-1]

		// Re-fetch parent to get accurate dependency data from bd.
		var upstreamIDs, downstreamIDs []string
		showCtx, showCancel := context.WithTimeout(ctx, executil.BdTimeout())
		// --include-dependents: bd omits the dependents array without it, which
		// would leave downstreamIDs empty and silently drop the parent's
		// downstream blocks instead of transferring them to the last sub-bead.
		showOut, showErr := run(showCtx, anvilPath,
			executil.BdShowDependentsArgs(parent.ID)...)
		showCancel()
		if showErr == nil {
			upstreamIDs, downstreamIDs = parseDepsFromShow(string(showOut))
		} else {
			log.Printf("[schematic:%s] Warning: could not re-fetch parent deps: %v", parent.ID, showErr)
			// Fall back to the (possibly empty) struct fields.
			upstreamIDs = parent.DependsOn
			downstreamIDs = parent.Blocks
		}

		// Transfer parent's upstream dependencies to B1 (first sub-bead).
		for _, depID := range upstreamIDs {
			xCtx, xCancel := context.WithTimeout(ctx, executil.BdTimeout())
			xOut, xErr := run(xCtx, anvilPath, "dep", "add", first.ID, depID)
			xCancel()
			if xErr != nil {
				log.Printf("[schematic:%s] Warning: failed to transfer DependsOn %s to first sub-bead %s: %v: %s",
					parent.ID, depID, first.ID, xErr, xOut)
			}
		}

		// Transfer parent's downstream blocks to B3 (last sub-bead).
		for _, blockedID := range downstreamIDs {
			xCtx, xCancel := context.WithTimeout(ctx, executil.BdTimeout())
			xOut, xErr := run(xCtx, anvilPath, "dep", "add", blockedID, last.ID)
			xCancel()
			if xErr != nil {
				log.Printf("[schematic:%s] Warning: failed to transfer Blocks %s to last sub-bead %s: %v: %s",
					parent.ID, blockedID, last.ID, xErr, xOut)
				allBlocksTransferred = false
			}
		}
	}

	// Close the parent bead only when all downstream block transfers succeeded.
	// If any transfer failed, keep the parent open so downstream beads remain
	// correctly gated by the parent until the dependency can be retried.
	if !allBlocksTransferred {
		log.Printf("[schematic:%s] Skipping parent close: not all downstream block transfers succeeded", parent.ID)
		return subBeads, nil
	}
	subIDs := make([]string, len(subBeads))
	for i, sb := range subBeads {
		subIDs[i] = sb.ID
	}
	closeReason := fmt.Sprintf("Decomposed into %d sub-beads: %s", len(subBeads), strings.Join(subIDs, ", "))
	closeCtx, closeCancel := context.WithTimeout(ctx, executil.BdTimeout())
	defer closeCancel()
	closeOut, closeErr := run(closeCtx, anvilPath, "close", parent.ID, "--force", "--reason", closeReason)
	if closeErr != nil {
		log.Printf("[schematic:%s] Warning: failed to close parent after decomposition: %v: %s", parent.ID, closeErr, closeOut)
	} else {
		log.Printf("[schematic:%s] Closed parent after decomposition into %d sub-beads: %s", parent.ID, len(subBeads), strings.Join(subIDs, ", "))
	}

	return subBeads, nil
}

// chainSequentialDep wires the most recently appended sub-bead to depend on the
// one before it (child N+1 depends on child N) so the poller dispatches them in
// order. It is a no-op for the first sub-bead. A failure is fatal: without the
// sequencing link a later child could be dispatched before an earlier one
// completes, reintroducing the original ordering problem, so the error is
// returned for the caller to surface via ActionClarify. When the dependency
// already exists (idempotent re-decomposition reusing prior children), bd
// treats the add as a success and no error is returned.
func chainSequentialDep(ctx context.Context, anvilPath string, parent poller.Bead, subBeads []SubBead, run bdRunner) error {
	if len(subBeads) < 2 {
		return nil
	}
	child := subBeads[len(subBeads)-1]
	prev := subBeads[len(subBeads)-2]
	depOut, attempts, depErr := addSequentialDepWithRetry(ctx, anvilPath, parent.ID, child.ID, prev.ID, run)
	if depErr != nil {
		log.Printf("[schematic:%s] Sequential dependency chaining failed %s -> %s after %d attempt(s): %v: %s",
			parent.ID, child.ID, prev.ID, attempts, depErr, depOut)
		return fmt.Errorf("sequential dependency chaining failed %s -> %s after %d attempt(s): %w: %s",
			child.ID, prev.ID, attempts, depErr, depOut)
	}
	return nil
}

// closeSupersededChildren closes marker-labeled children discovered from a
// prior decomposition pass that are not part of the current sub-bead set. These
// are orphans left when a re-decomposition produced different titles than the
// pass that created them; closing them keeps exactly one coherent set of
// children. It is best-effort — failures are logged and do not abort.
func closeSupersededChildren(ctx context.Context, parent poller.Bead, subBeads []SubBead, existingByTitle map[string]string, anvilPath string, run bdRunner) {
	if len(existingByTitle) == 0 {
		return
	}
	used := make(map[string]struct{}, len(subBeads))
	for _, sb := range subBeads {
		used[sb.ID] = struct{}{}
	}
	reason := "schematic-rollback: orphaned sub-bead from a superseded decomposition of " + parent.ID
	for _, id := range existingByTitle {
		if _, ok := used[id]; ok {
			continue
		}
		coCtx, coCancel := context.WithTimeout(ctx, executil.BdTimeout())
		out, err := run(coCtx, anvilPath, "close", id, "--force", "--reason", reason)
		coCancel()
		if err != nil {
			log.Printf("[schematic:%s] Warning: failed to close superseded sub-bead %s: %v: %s", parent.ID, id, err, out)
		} else {
			log.Printf("[schematic:%s] Closed superseded sub-bead %s from a prior decomposition", parent.ID, id)
		}
	}
}

// findExistingChildren queries bd for sub-beads carrying the given schematic
// marker label and returns a map from bead title to bead ID for the ones still
// open (not closed). It is used to make decomposition idempotent: a task whose
// title matches an existing marked child reuses that child instead of creating
// a duplicate. It is best-effort — on any query or parse failure it returns an
// empty map so decomposition proceeds by creating a fresh set.
func findExistingChildren(ctx context.Context, anvilPath, marker string, run bdRunner) map[string]string {
	result := map[string]string{}

	listCtx, cancel := context.WithTimeout(ctx, executil.BdTimeout())
	out, err := run(listCtx, anvilPath, "list", "--label", marker, "--json")
	cancel()
	if err != nil {
		log.Printf("[schematic] Warning: could not query existing sub-beads for label %q: %v", marker, err)
		return result
	}

	var items []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if derr := executil.DecodeJSON(out, &items); derr != nil {
		log.Printf("[schematic] Warning: could not parse existing sub-beads for label %q: %v", marker, derr)
		return result
	}

	for _, it := range items {
		if it.ID == "" || it.Title == "" {
			continue
		}
		// Skip children a prior pass already completed — reusing a closed bead
		// would leave the decomposition incomplete.
		if strings.EqualFold(it.Status, "closed") || strings.EqualFold(it.Status, "done") {
			continue
		}
		// First occurrence wins; ignore later duplicates of the same title.
		if _, seen := result[it.Title]; !seen {
			result[it.Title] = it.ID
		}
	}
	return result
}

// parseDepsFromShow extracts upstream (dependencies) and downstream (dependents)
// bead IDs from the JSON output of "bd show <id> --json". The bd JSON format
// uses "dependencies" for beads this bead depends on and "dependents" for beads
// that depend on this bead — different from the poller.Bead struct field names.
func parseDepsFromShow(jsonOutput string) (upstreamIDs, downstreamIDs []string) {
	// bd show --json returns an array with one element.
	var items []json.RawMessage
	// bd show --json may emit trailing diagnostics; use DecodeJSON to tolerate noise.
	if err := executil.DecodeJSON([]byte(jsonOutput), &items); err != nil || len(items) == 0 {
		// Try parsing as a single object.
		items = []json.RawMessage{json.RawMessage(jsonOutput)}
	}

	var parsed struct {
		Dependencies []struct {
			ID string `json:"id"`
		} `json:"dependencies"`
		Dependents []struct {
			ID string `json:"id"`
		} `json:"dependents"`
	}
	if err := executil.DecodeJSON(items[0], &parsed); err != nil {
		return nil, nil
	}

	for _, d := range parsed.Dependencies {
		if d.ID != "" {
			upstreamIDs = append(upstreamIDs, d.ID)
		}
	}
	for _, d := range parsed.Dependents {
		if d.ID != "" {
			downstreamIDs = append(downstreamIDs, d.ID)
		}
	}
	return upstreamIDs, downstreamIDs
}

// buildPrompt creates the analysis prompt for the Schematic AI session.
// It uses strings.Builder instead of fmt.Sprintf to avoid issues with
// user-controlled bead fields containing '%' characters.
func buildPrompt(bead poller.Bead) string {
	prompt := `You are a software architect analysing a work item (bead) to determine the best approach.

IMPORTANT: You have a very limited turn budget. You MUST output your JSON verdict IMMEDIATELY in your FIRST response. Do NOT use any tools — do NOT read files, do NOT run commands, do NOT explore the codebase. Your job is to analyse the bead specification below — description, design, AND acceptance criteria — and make a decision based solely on what is written. The acceptance criteria often reveal the true scope (multiple components/areas) even when the description reads as one feature. Output the JSON verdict and nothing else.

## Bead to Analyse

`

	var b strings.Builder
	b.WriteString(prompt)

	// Append bead metadata literally so that any '%' characters in the bead fields
	// are treated as plain text and cannot affect formatting.
	b.WriteString("**ID**: ")
	b.WriteString(bead.ID)
	b.WriteString("\n**Title**: ")
	b.WriteString(bead.Title)
	b.WriteString("\n**Type**: ")
	b.WriteString(bead.IssueType)
	b.WriteString("\n**Priority**: ")
	b.WriteString(fmt.Sprintf("%d", bead.Priority))
	b.WriteString("\n\n### Description\n\n")
	b.WriteString(bead.Description)
	b.WriteString("\n")
	// Design and Acceptance Criteria carry the real scope signal for the
	// decompose decision — a terse description can hide a multi-area feature
	// whose parts are only enumerated in the acceptance criteria (this is why
	// Fhi.Metadata-jnmj6, an API+GUI+export+migration+rule+tests bead, was
	// mis-classified as a single "plan" when schematic saw description only).
	if strings.TrimSpace(bead.Design) != "" {
		b.WriteString("\n### Design\n\n")
		b.WriteString(bead.Design)
		b.WriteString("\n")
	}
	if strings.TrimSpace(bead.AcceptanceCriteria) != "" {
		b.WriteString("\n### Acceptance Criteria\n\n")
		b.WriteString(bead.AcceptanceCriteria)
		b.WriteString("\n")
	}

	b.WriteString(`
## Your Task

Analyse this bead and decide ONE of the following actions:

1. **plan** — The bead is implementable as a single unit of work. Produce a focused, step-by-step implementation plan that a coding agent can follow.
2. **decompose** — The bead is too large, has multiple independent parts, or would benefit from being split. List the sub-tasks.
3. **clarify** — The bead is ambiguous or missing critical information and cannot be worked on yet.

## Decision Criteria

- If the description has multiple independent features/changes → decompose
- If the design or acceptance criteria span multiple independent areas or layers (e.g. API + GUI + DB migration + export + tests) → decompose, one sub-task per area
- If the scope is large (>500 lines of change expected) → decompose
- If the bead title and description are clear and focused → plan
- If the bead has contradictory or missing requirements → clarify
- Prefer "plan" over "decompose" when in doubt — avoid unnecessary splits

## Output Format

You MUST output your verdict as a JSON block in your VERY FIRST response. Do NOT make tool calls first. Do NOT investigate the codebase. Emit the JSON immediately:

` + "```json" + `
{
  "action": "plan|decompose|clarify",
  "plan": "Step-by-step implementation plan (only for action=plan)",
  "sub_tasks": [
    {
      "title": "Short task title",
      "description": "Detailed description including: files to create/modify, key function signatures, implementation approach, and how this sub-task connects to sibling sub-tasks"
    }
  ],
  "reason": "Brief explanation of your decision"
}
` + "```" + `

Keep sub_tasks to 2-5 items. Each must have both a "title" and a "description".

**Sub-task description requirements (for action=decompose):**
- Which files to create or modify
- Key function signatures or types to implement
- Implementation approach (algorithm, pattern, or technique)
- How this sub-task connects to or depends on sibling sub-tasks
- Ground each description in the parent bead's requirements above

For "plan", provide concrete steps: which files to modify, what to add, what to test.

REMINDER: Output the JSON verdict NOW. Do not use tools. Your turn budget is very limited.
`)

	return b.String()
}

// ChildBead is a lightweight summary of a child bead for the crucible check prompt.
type ChildBead struct {
	ID          string
	Title       string
	Description string
}

// RunCrucibleCheck determines whether a bead with children needs crucible
// orchestration (sequential work on a feature branch) or can be dispatched
// as a standalone bead with children handled individually.
//
// This is a lightweight schematic call that only inspects the relationship
// between parent and children — it does not produce implementation plans.
func RunCrucibleCheck(ctx context.Context, cfg Config, parent poller.Bead, children []ChildBead, anvilPath string, pv provider.Provider) *CrucibleCheckResult {
	start := time.Now()

	if !cfg.Enabled {
		return &CrucibleCheckResult{
			NeedsCrucible: false,
			Reason:        "Schematic disabled — defaulting to standalone dispatch",
			Duration:      time.Since(start),
		}
	}

	promptText := buildCruciblePrompt(parent, children)

	log.Printf("[schematic:%s] Running crucible check (provider: %s)", parent.ID, pv.Label())

	workDir, err := os.MkdirTemp("", "forge-crucible-check-*")
	if err != nil {
		return &CrucibleCheckResult{
			NeedsCrucible: false,
			Reason:        fmt.Sprintf("Failed to create temp dir: %v — defaulting to standalone", err),
			Duration:      time.Since(start),
		}
	}
	defer os.RemoveAll(workDir)

	// Same durable-log-dir handling as Analyze: the temp workdir (and any log
	// inside it) is deleted when the check ends, and its path fails the
	// Hearth log allowlist (Forge-x8ew).
	logDir := filepath.Join(workDir, "logs")
	if cfg.LogDir != "" {
		logDir = cfg.LogDir
	}
	extraFlags := append([]string{"--max-turns", fmt.Sprintf("%d", cfg.MaxTurns)}, cfg.ExtraFlags...)
	process, err := smith.SpawnWithOptions(ctx, workDir, promptText, logDir, pv, extraFlags, smith.SpawnOptions{LogPrefix: "schematic"})
	if err != nil {
		return &CrucibleCheckResult{
			NeedsCrucible: false,
			Reason:        fmt.Sprintf("Failed to spawn session: %v — defaulting to standalone", err),
			Duration:      time.Since(start),
		}
	}
	if cfg.OnSpawn != nil {
		cfg.OnSpawn(process.PID, process.LogPath)
	}

	smithResult := process.Wait()

	result := &CrucibleCheckResult{
		Duration: time.Since(start),
		CostUSD:  smithResult.CostUSD,
		Quota:    smithResult.Quota,
	}

	if smithResult.ExitCode != 0 || smithResult.RateLimited {
		result.Reason = "Schematic session failed — defaulting to standalone"
		return result
	}

	output := smithResult.FullOutput
	if output == "" {
		output = smithResult.Output
	}

	verdict, err := parseCrucibleVerdict(output)
	if err != nil {
		result.Reason = fmt.Sprintf("Could not parse crucible verdict — defaulting to standalone: %v", err)
		return result
	}

	result.NeedsCrucible = verdict.NeedsCrucible
	result.Reason = verdict.Reason
	return result
}

// crucibleVerdict is the JSON structure returned by the crucible check prompt.
type crucibleVerdict struct {
	NeedsCrucible bool   `json:"needs_crucible"`
	Reason        string `json:"reason"`
}

// parseCrucibleVerdict extracts the crucible decision from AI output.
func parseCrucibleVerdict(output string) (*crucibleVerdict, error) {
	// Try JSON in ```json fences
	if idx := strings.Index(output, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(output[start:], "```"); end >= 0 {
			var v crucibleVerdict
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &v); err == nil {
				return &v, nil
			}
		}
	}

	// Try plain ``` fences
	if idx := strings.Index(output, "```"); idx >= 0 {
		start := idx + 3
		if nl := strings.Index(output[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(output[start:], "```"); end >= 0 {
			block := strings.TrimSpace(output[start : start+end])
			if strings.Contains(block, "needs_crucible") {
				var v crucibleVerdict
				if err := json.Unmarshal([]byte(block), &v); err == nil {
					return &v, nil
				}
			}
		}
	}

	// Scan for raw JSON objects
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		depth := 0
		for j := i; j < len(output); j++ {
			switch output[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := output[i : j+1]
					if strings.Contains(block, `"needs_crucible"`) {
						var v crucibleVerdict
						if err := json.Unmarshal([]byte(block), &v); err == nil {
							return &v, nil
						}
					}
					i = j
					break
				}
			}
			if depth == 0 {
				break
			}
		}
	}

	return nil, fmt.Errorf("no valid crucible verdict JSON found in output")
}

// buildCruciblePrompt creates the prompt for the crucible check.
func buildCruciblePrompt(parent poller.Bead, children []ChildBead) string {
	var b strings.Builder

	b.WriteString(`You are a software architect determining how related work items should be orchestrated.

## Parent Bead

`)
	b.WriteString("**ID**: ")
	b.WriteString(parent.ID)
	b.WriteString("\n**Title**: ")
	b.WriteString(parent.Title)
	b.WriteString("\n**Type**: ")
	b.WriteString(parent.IssueType)
	b.WriteString("\n\n### Description\n\n")
	b.WriteString(parent.Description)
	b.WriteString("\n\n## Children (beads that depend on the parent)\n\n")

	for i, child := range children {
		b.WriteString(fmt.Sprintf("### Child %d: %s\n", i+1, child.ID))
		b.WriteString("**Title**: ")
		b.WriteString(child.Title)
		b.WriteString("\n**Description**: ")
		b.WriteString(child.Description)
		b.WriteString("\n\n")
	}

	b.WriteString(`## Your Task

Determine whether these beads need **crucible orchestration** (sequential work on a shared feature branch) or can be dispatched **individually** as standalone work items.

**Crucible orchestration** means: create a feature branch, complete the parent first, then each child in order — each building on the previous one's merged code. Use this when:
- The children depend on the parent's code changes being present
- The children modify the same files or build on each other's work
- The work forms a logical sequence where order matters for correctness

**Standalone dispatch** means: each bead is dispatched independently on its own branch. Use this when:
- The beads are independent pieces of work that happen to relate to the same goal
- Each bead can be implemented and tested without the others
- The dependency is just about priority/ordering, not code dependencies

## Output Format

Output your verdict as a JSON block:

` + "```json" + `
{
  "needs_crucible": true|false,
  "reason": "Brief explanation"
}
` + "```" + `
`)

	return b.String()
}
