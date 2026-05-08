package forgechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// MaterializedBead is the result of a successful bd create for one proposal.
// ProposalID matches the input BeadProposal.ProposalID; BeadID is the real
// bd-assigned ID returned by `bd create --json`. AnvilPath is captured on
// the way in so rollback can run `bd close` against the same database
// without re-resolving the lookup (which may be stale after a crash).
type MaterializedBead struct {
	ProposalID string
	BeadID     string
	Anvil      string
	AnvilPath  string
	Title      string
}

// MaterializeResult is the outcome of MaterializeEmission. Created lists
// every bead that was successfully created (in creation order). When
// RolledBack is true, the listed beads were closed via `bd close --reason`
// after the failure that aborted the run. Err is the failure that triggered
// the rollback, or nil when every bead was created successfully.
type MaterializeResult struct {
	Created       []MaterializedBead
	RolledBack    bool
	RollbackError error
	Err           error
}

// AnvilLookup resolves an anvil name to its on-disk path. The materializer
// uses it to set cmd.Dir so bd connects to the right Dolt database. Returns
// ok=false when the name is not registered with the daemon — callers should
// treat that as a validation failure (we already check before materialising,
// but a stale resolver could miss one).
type AnvilLookup func(name string) (path string, ok bool)

// BdRunner runs a `bd <args...>` command in dir and returns its stdout (or
// an error containing stderr). Tests inject a fake to avoid spawning real
// subprocesses; production wires DefaultBdRunner.
type BdRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// DefaultBdRunner is the production BdRunner. Inherits the parent process
// environment so `bd` finds the same Dolt config as the daemon.
func DefaultBdRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", args...))
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		se := strings.TrimSpace(stderr.String())
		if se != "" {
			return stdout.Bytes(), fmt.Errorf("bd %s: %v: %s", strings.Join(args, " "), err, se)
		}
		return stdout.Bytes(), fmt.Errorf("bd %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// bdCreatePerOp is the per-`bd create` timeout. bd talks to a remote Dolt
// server so individual creates can take 20-30s; 90s leaves headroom under
// load without dragging the HTTP request out indefinitely.
const bdCreatePerOp = 90 * time.Second

// MaterializeEmission creates each proposed bead via `bd create`. Beads are
// processed in topological order so a bead is never created before its
// dependencies — that lets us pass already-resolved bd IDs into `--deps`
// rather than going back with `bd dep add` afterwards (fewer subprocess
// calls, simpler rollback).
//
// Atomicity: if any step fails, every bead created so far is closed via
// `bd close --reason="rollback: ..."`. The function returns the partial
// MaterializeResult with Err set to the original failure and RolledBack
// true. The original Err is preferred over the rollback error — operators
// usually care more about why creation failed than about a stuck rollback.
//
// Cycles must be caught upstream by ValidateEmission. If a cycle slips
// through, the topo sort returns an error and no bd subprocess runs.
func MaterializeEmission(
	ctx context.Context,
	logger *slog.Logger,
	env *EmissionEnvelope,
	lookup AnvilLookup,
	runner BdRunner,
) MaterializeResult {
	if logger == nil {
		logger = slog.Default()
	}
	res := MaterializeResult{}

	if env == nil || len(env.Beads) == 0 {
		res.Err = errors.New("forgechat: empty emission")
		return res
	}
	if lookup == nil {
		res.Err = errors.New("forgechat: nil anvil lookup")
		return res
	}
	if runner == nil {
		runner = DefaultBdRunner
	}

	order, err := topoSort(env.Beads)
	if err != nil {
		res.Err = fmt.Errorf("forgechat: %w", err)
		return res
	}

	byID := make(map[string]BeadProposal, len(env.Beads))
	for _, b := range env.Beads {
		byID[b.ProposalID] = b
	}
	proposalToBead := make(map[string]string, len(env.Beads))

	for _, propID := range order {
		bead := byID[propID]
		anvilPath, ok := lookup(bead.Anvil)
		if !ok || strings.TrimSpace(anvilPath) == "" {
			// Empty path is treated as "not registered" — running bd in the
			// daemon's cwd would silently target the wrong database, which is
			// far worse than failing fast.
			res.Err = fmt.Errorf("forgechat: anvil %q not registered or has no path", bead.Anvil)
			rollback(ctx, logger, &res, runner)
			return res
		}

		// Defensive: validation should have caught unresolved deps, but if
		// it was skipped (or a future caller forgets) we'd otherwise create
		// a bead without its declared edges. Refuse instead.
		for _, dep := range bead.DependsOn {
			if _, resolved := proposalToBead[dep]; !resolved {
				res.Err = fmt.Errorf("forgechat: bead %q depends on unresolved sibling %q (validate before materialising)", bead.ProposalID, dep)
				rollback(ctx, logger, &res, runner)
				return res
			}
		}

		args := buildCreateArgs(bead, proposalToBead)
		opCtx, cancel := context.WithTimeout(ctx, bdCreatePerOp)
		out, err := runner(opCtx, anvilPath, args...)
		cancel()
		if err != nil {
			res.Err = fmt.Errorf("forgechat: bd create failed for %q (anvil %s): %w", bead.Title, bead.Anvil, err)
			rollback(ctx, logger, &res, runner)
			return res
		}
		beadID, err := parseCreatedID(out)
		if err != nil {
			res.Err = fmt.Errorf("forgechat: could not parse bd create output for %q: %w", bead.Title, err)
			rollback(ctx, logger, &res, runner)
			return res
		}
		proposalToBead[bead.ProposalID] = beadID
		res.Created = append(res.Created, MaterializedBead{
			ProposalID: bead.ProposalID,
			BeadID:     beadID,
			Anvil:      bead.Anvil,
			AnvilPath:  anvilPath,
			Title:      bead.Title,
		})
		logger.Info("forgechat: created bead",
			"proposal_id", bead.ProposalID,
			"bead_id", beadID,
			"anvil", bead.Anvil,
			"title", bead.Title,
		)
	}

	return res
}

// buildCreateArgs assembles the bd create flags for a proposal. Sibling deps
// resolve to real bd IDs because we materialise in topological order.
func buildCreateArgs(b BeadProposal, resolved map[string]string) []string {
	// Defensive default: ValidateEmission normalises blank types to "task",
	// but if a caller skips validation we'd otherwise pass `--type ""` to bd
	// and get an opaque CLI error. Mirror the validator's choice here.
	typ := strings.TrimSpace(b.Type)
	if typ == "" {
		typ = "task"
	}
	args := []string{
		"create",
		"--title", b.Title,
		"--description", b.Description,
		"--type", typ,
		"--priority", fmt.Sprintf("%d", b.Priority),
		"--json",
	}
	if len(b.Labels) > 0 {
		args = append(args, "--labels", strings.Join(b.Labels, ","))
	}
	for _, dep := range b.DependsOn {
		if realID, ok := resolved[dep]; ok && realID != "" {
			// `bd create --deps <id>` defaults to the depends-on (blocked-by)
			// relation, which is exactly what BeadProposal.DependsOn means.
			args = append(args, "--deps", realID)
		}
	}
	return args
}

// parseCreatedID pulls the bead id from `bd create --json` output. bd may
// emit trailing diagnostics (orphan detection, etc.) after the JSON object,
// so we use a streaming decoder that tolerates trailing data.
//
// Two output shapes are accepted:
//   - {"id":"forge-aaa", ...}                — the common case
//   - [{"id":"forge-aaa", ...}]              — emitted by some bd builds
//     when the create flows through the multi-bead graph form
//
// An empty/missing id from either shape is treated as a parse failure rather
// than silently materialising a bead we cannot later roll back.
func parseCreatedID(out []byte) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := executil.DecodeJSON(out, &created); err == nil && created.ID != "" {
		return created.ID, nil
	}
	// Fallback: some bd builds emit a top-level array with a single entry
	// for the create-from-graph form; try that too.
	var arr []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &arr); err == nil && len(arr) > 0 && arr[0].ID != "" {
		return arr[0].ID, nil
	}
	snippet := strings.TrimSpace(string(out))
	if len(snippet) > 240 {
		snippet = snippet[:240] + "…"
	}
	return "", fmt.Errorf("missing id in bd create output: %s", snippet)
}

// rollback closes every bead in res.Created with a rollback reason, in
// reverse creation order so dependent beads are closed before the beads they
// depend on. Failures during rollback are logged and aggregated into
// res.RollbackError but do not change res.Err — the original failure is the
// one operators need to act on.
//
// The caller's ctx is detached via context.WithoutCancel because the most
// common reason rollback runs is that ctx itself was cancelled (an HTTP
// client disconnect, or the failing `bd create` was aborted by a timeout).
// Inheriting that cancellation would make every `bd close` return
// context.Canceled immediately, leaving orphan beads in the database. The
// per-op timeout below caps each close so we still bound total work.
func rollback(ctx context.Context, logger *slog.Logger, res *MaterializeResult, runner BdRunner) {
	res.RolledBack = true
	if len(res.Created) == 0 {
		return
	}
	reason := "rollback: parent create failed in Beads-Forge emission"
	if res.Err != nil {
		// Trim the reason so we don't blow past bd's TEXT field bounds with a
		// huge error chain; operators can find the full error in the daemon log.
		short := truncateRunes(res.Err.Error(), 160)
		reason = "rollback: " + short
	}
	rollbackCtx := context.WithoutCancel(ctx)
	var failures []string
	for i := len(res.Created) - 1; i >= 0; i-- {
		b := res.Created[i]
		opCtx, cancel := context.WithTimeout(rollbackCtx, bdCreatePerOp)
		_, err := runner(opCtx, b.AnvilPath, "close", b.BeadID, "--reason", reason)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", b.BeadID, err))
			logger.Warn("forgechat: rollback close failed",
				"bead_id", b.BeadID,
				"anvil", b.Anvil,
				"error", err,
			)
			continue
		}
		logger.Info("forgechat: rollback closed bead",
			"bead_id", b.BeadID,
			"anvil", b.Anvil,
		)
	}
	if len(failures) > 0 {
		res.RollbackError = fmt.Errorf("rollback partial failure: %s", strings.Join(failures, "; "))
	}
}

// topoSort returns a permutation of proposal IDs such that every bead
// appears after the beads it depends on. Returns an error if the graph
// contains a cycle (validation should have caught this earlier; the check
// here is defensive).
func topoSort(beads []BeadProposal) ([]string, error) {
	indeg := make(map[string]int, len(beads))
	adj := make(map[string][]string, len(beads))
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		ids = append(ids, b.ProposalID)
		if _, exists := indeg[b.ProposalID]; !exists {
			indeg[b.ProposalID] = 0
		}
		for _, d := range b.DependsOn {
			adj[d] = append(adj[d], b.ProposalID)
			indeg[b.ProposalID]++
		}
	}
	// Process equally-eligible nodes alphabetically so output ordering is
	// deterministic across runs (helps tests and human operators).
	sort.Strings(ids)
	queue := make([]string, 0, len(ids))
	for _, id := range ids {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	out := make([]string, 0, len(beads))
	for len(queue) > 0 {
		sort.Strings(queue)
		head := queue[0]
		queue = queue[1:]
		out = append(out, head)
		for _, succ := range adj[head] {
			indeg[succ]--
			if indeg[succ] == 0 {
				queue = append(queue, succ)
			}
		}
	}
	if len(out) != len(beads) {
		return nil, errors.New("dependency graph contains a cycle")
	}
	return out, nil
}

// truncateRunes is a local copy of the rune-safe truncate helper to avoid a
// cross-file dependency. Used to keep rollback reasons under bd's text
// limits while still preserving the leading characters of the error.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
