// Package anvilhealth detects an anvil whose beads (Dolt) working set is left
// mid-merge with unresolved conflicts — a "wedged" anvil.
//
// While an anvil is wedged every bd write against it fails with
//
//	Error 1105: Merge conflict detected, @autocommit transaction rolled back.
//
// so Forge's status transitions, label updates and dispatch bookkeeping are
// silently rolled back. Detection is deliberately based on positive state (the
// dolt_conflicts system table) rather than on matching bd's error text, which
// drifts between bd versions.
//
// This package only observes. Resolving a merge conflict is a semantic decision
// and belongs in bd / the operator's hands.
package anvilhealth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// DefaultTimeout bounds a single health check (one to four small SQL queries
// against the anvil's beads database). It is far below executil.DefaultBdTimeout
// because a wedged-anvil probe must never hold up a poll cycle: if the beads
// backend is too slow to answer a COUNT(*), the check reports "unknown" and the
// next full poll retries.
const DefaultTimeout = 30 * time.Second

// conflictsQuery is the detection query. `table` is a reserved word, hence the
// backticks; both columns are aliased so the JSON keys are stable regardless of
// how bd renders reserved identifiers.
const conflictsQuery = "SELECT `table` AS conflict_table, num_conflicts AS conflict_count FROM dolt_conflicts"

// ConflictedTable is one row of dolt_conflicts: a table with unresolved merge
// conflicts and how many rows are conflicted.
type ConflictedTable struct {
	Table        string
	NumConflicts int64
}

// Report is the outcome of one health check for one anvil.
//
// A Report with no conflicted tables means the anvil is healthy. Divergence is
// best-effort and only computed when the anvil is wedged (it is the number that
// tells an operator how urgent the wedge is); DivergenceKnown reports whether
// Ahead/Behind could be determined.
type Report struct {
	Tables          []ConflictedTable
	TotalConflicts  int64
	Branch          string
	Upstream        string
	Ahead           int
	Behind          int
	DivergenceKnown bool
}

// Wedged reports whether the anvil's working set is mid-merge with unresolved
// conflicts, i.e. whether bd writes against it will fail.
func (r Report) Wedged() bool { return len(r.Tables) > 0 }

// TableNames returns the conflicted table names in the order reported.
func (r Report) TableNames() []string {
	names := make([]string, 0, len(r.Tables))
	for _, t := range r.Tables {
		names = append(names, t.Table)
	}
	return names
}

// TablesSummary renders the conflicted tables with their per-table conflict
// counts, e.g. "issues (3), labels (1)".
func (r Report) TablesSummary() string {
	parts := make([]string, 0, len(r.Tables))
	for _, t := range r.Tables {
		parts = append(parts, fmt.Sprintf("%s (%d)", t.Table, t.NumConflicts))
	}
	return strings.Join(parts, ", ")
}

// DivergenceSummary renders the branch divergence, or "unknown" when it could
// not be determined.
func (r Report) DivergenceSummary() string {
	if !r.DivergenceKnown {
		return "divergence unknown"
	}
	branch := r.Branch
	if branch == "" {
		branch = "local"
	}
	return fmt.Sprintf("%s ahead %d / behind %d", branch, r.Ahead, r.Behind)
}

// Detail renders the operator-facing description used for the needs-attention
// entry and the WARN log line. It names the conflicted table(s), the total
// conflict count and the branch divergence, and ends with the remediation hint
// distilled from the 2026-08-05 incidents.
func (r Report) Detail() string {
	return fmt.Sprintf(
		"Beads database is mid-merge with unresolved conflicts — every bd write against this anvil fails. "+
			"Conflicted tables: %s. Total conflicts: %d. Divergence: %s. "+
			"Resolve the conflict by hand in the anvil's beads database (if the bead has an open PR, "+
			"Forge's in_progress status is the correct side — take --ours). "+
			"Forge clears this entry automatically once dolt_conflicts is empty.",
		r.TablesSummary(), r.TotalConflicts, r.DivergenceSummary(),
	)
}

// Runner executes a read-only SQL query against the beads database of the anvil
// rooted at dir and returns the raw JSON result. It is a field on Checker so
// tests can substitute a fake without a live Dolt server.
type Runner func(ctx context.Context, dir, query string) ([]byte, error)

// Checker probes anvils for unresolved beads merge conflicts.
type Checker struct {
	// Run executes a query. Defaults to BdSQL when nil.
	Run Runner
	// Timeout bounds a single Check call. Defaults to DefaultTimeout when zero.
	Timeout time.Duration
}

// New returns a Checker backed by `bd sql`.
func New() *Checker { return &Checker{Run: BdSQL, Timeout: DefaultTimeout} }

// Check probes one anvil. A non-nil error means the state is *unknown* (the
// query itself failed, the beads backend is unreachable, or it is not Dolt);
// callers must not read that as "healthy" and must not clear an existing
// wedged flag on it.
func (c *Checker) Check(ctx context.Context, anvilPath string) (Report, error) {
	if anvilPath == "" {
		return Report{}, fmt.Errorf("anvilhealth: empty anvil path")
	}
	run := c.Run
	if run == nil {
		run = BdSQL
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := run(ctx, anvilPath, conflictsQuery)
	if err != nil {
		return Report{}, fmt.Errorf("querying dolt_conflicts: %w", err)
	}
	rows, err := parseRows(out)
	if err != nil {
		return Report{}, fmt.Errorf("parsing dolt_conflicts result: %w", err)
	}

	var rep Report
	for _, row := range rows {
		name := stringField(row, "conflict_table", "table")
		if name == "" {
			continue
		}
		n := intField(row, "conflict_count", "num_conflicts")
		rep.Tables = append(rep.Tables, ConflictedTable{Table: name, NumConflicts: n})
		rep.TotalConflicts += n
	}
	if !rep.Wedged() {
		// Healthy: skip the divergence queries entirely so a healthy anvil costs
		// exactly one query per full poll.
		return rep, nil
	}
	sort.Slice(rep.Tables, func(i, j int) bool { return rep.Tables[i].Table < rep.Tables[j].Table })

	// Divergence is informational. A failure here must never suppress the
	// conflict report, so errors are swallowed and DivergenceKnown stays false.
	c.fillDivergence(ctx, run, anvilPath, &rep)
	return rep, nil
}

// refPattern restricts branch and remote names to characters that are safe to
// interpolate into a dolt_log() revision range. Anything else leaves divergence
// unknown rather than building a query from unvalidated input.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// fillDivergence best-effort populates Branch/Upstream/Ahead/Behind. Every step
// is allowed to fail: the caller still reports the conflict without divergence.
func (c *Checker) fillDivergence(ctx context.Context, run Runner, anvilPath string, rep *Report) {
	branch := c.queryScalarString(ctx, run, anvilPath, "SELECT active_branch() AS branch")
	if branch == "" || !refPattern.MatchString(branch) {
		return
	}
	rep.Branch = branch

	upstream := c.upstreamRef(ctx, run, anvilPath, branch)
	if upstream == "" {
		return
	}
	rep.Upstream = upstream

	ahead, aheadOK := c.countCommits(ctx, run, anvilPath, upstream, branch)
	behind, behindOK := c.countCommits(ctx, run, anvilPath, branch, upstream)
	if !aheadOK || !behindOK {
		return
	}
	rep.Ahead, rep.Behind, rep.DivergenceKnown = ahead, behind, true
}

// upstreamRef resolves the remote-tracking ref for branch, e.g.
// "remotes/origin/beads-sync". It prefers the branch's configured upstream and
// falls back to the sole configured remote when no upstream is recorded.
func (c *Checker) upstreamRef(ctx context.Context, run Runner, anvilPath, branch string) string {
	remote, remoteBranch := "", ""
	if out, err := run(ctx, anvilPath, "SELECT remote, branch FROM dolt_branches WHERE name = active_branch()"); err == nil {
		if rows, perr := parseRows(out); perr == nil && len(rows) > 0 {
			remote = stringField(rows[0], "remote")
			remoteBranch = stringField(rows[0], "branch")
		}
	}
	if remote == "" {
		// No upstream configured for this branch — fall back to the first
		// remote and assume a same-named branch, which is how bd's
		// beads-sync remote is set up.
		remote = c.queryScalarString(ctx, run, anvilPath, "SELECT name FROM dolt_remotes ORDER BY name LIMIT 1")
	}
	if remoteBranch == "" {
		remoteBranch = branch
	}
	if remote == "" || !refPattern.MatchString(remote) || !refPattern.MatchString(remoteBranch) {
		return ""
	}
	return "remotes/" + remote + "/" + remoteBranch
}

// countCommits returns the number of commits reachable from head but not from
// base, i.e. git's `base..head` semantics.
func (c *Checker) countCommits(ctx context.Context, run Runner, anvilPath, base, head string) (int, bool) {
	q := fmt.Sprintf("SELECT COUNT(*) AS n FROM dolt_log('%s..%s')", base, head)
	out, err := run(ctx, anvilPath, q)
	if err != nil {
		return 0, false
	}
	rows, err := parseRows(out)
	if err != nil || len(rows) == 0 {
		return 0, false
	}
	return int(intField(rows[0], "n")), true
}

// queryScalarString runs a single-column query and returns the first value, or
// "" when the query fails or returns no rows.
func (c *Checker) queryScalarString(ctx context.Context, run Runner, anvilPath, query string) string {
	out, err := run(ctx, anvilPath, query)
	if err != nil {
		return ""
	}
	rows, err := parseRows(out)
	if err != nil || len(rows) == 0 {
		return ""
	}
	for _, v := range rows[0] {
		return coerceString(v)
	}
	return ""
}

// BdSQL is the default Runner: `bd sql --json --readonly <query>` executed in
// the anvil directory. --readonly guarantees a probe can never mutate an
// anvil's beads database.
func BdSQL(ctx context.Context, dir, query string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "sql", "--json", "--readonly", "--quiet", query))
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("bd sql in %s: %w: %s", dir, err, msg)
	}
	return out, nil
}

// parseRows decodes a `bd sql --json` result set. bd prints a JSON array of
// objects; an empty result set is "[]" (or, defensively, "null"/empty output).
// Any leading non-JSON preamble bd might print is skipped.
func parseRows(out []byte) ([]map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if i := bytes.IndexByte(trimmed, '['); i > 0 {
		trimmed = trimmed[i:]
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// stringField returns the first present key's value as a string.
func stringField(row map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if raw, ok := row[k]; ok {
			return coerceString(raw)
		}
	}
	return ""
}

// intField returns the first present key's value as an int64. Values arrive as
// JSON numbers today, but bd has historically rendered some columns as strings,
// so both are accepted.
func intField(row map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		raw, ok := row[k]
		if !ok {
			continue
		}
		s := coerceString(raw)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			// Tolerate a numeric value rendered with a fractional part.
			if f, ferr := strconv.ParseFloat(s, 64); ferr == nil {
				return int64(f)
			}
			return 0
		}
		return n
	}
	return 0
}

// coerceString renders a JSON scalar as a string, unquoting JSON strings and
// mapping null to "".
func coerceString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var out string
		if err := json.Unmarshal(raw, &out); err == nil {
			return out
		}
	}
	return s
}
