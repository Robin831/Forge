package state

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The dashboard reports idle Smith slots by reproducing the two axes of
// ActiveDispatchWorkers in TypeScript (DISPATCH_STATUSES and BACKGROUND_PHASES
// in internal/web/frontend/src/lib/workerStatus.ts). A TS-side assertion cannot
// guard that: it holds for any pair of values somebody edits both halves of,
// and it holds unchanged when the Go list moves — which is exactly the drift
// that let the UI omit 'stalled' and offer the operator a dispatch slot the
// daemon would never fill. These tests read the .ts file and compare it with
// the SQL the queries are actually built from.
const frontendWorkerStatusFile = "../web/frontend/src/lib/workerStatus.ts"

// stripTSLineComments removes `//` comments from TypeScript source, leaving the
// code alone. The module being parsed writes prose between the members of its
// literals, and every quoted word in such a comment is indistinguishable from a
// member to a regexp: a maintainer explaining one status in the surrounding
// house style ("// 'stalled' flips back to 'running' on worker_recovered")
// would otherwise add two phantom members and fail the guard with a diff naming
// statuses that exist only in a comment.
//
// A `//` inside a string literal is not a comment, so the scan tracks quoting
// rather than cutting at the first match.
func stripTSLineComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	for _, line := range strings.Split(src, "\n") {
		var quote byte
		cut := -1
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case quote != 0:
				if c == '\\' {
					i++
				} else if c == quote {
					quote = 0
				}
			case c == '\'' || c == '"' || c == '`':
				quote = c
			case c == '/' && i+1 < len(line) && line[i+1] == '/':
				cut = i
			}
			if cut >= 0 {
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// tsStringSet extracts the string literals of an exported `new Set([...])`
// declaration from the frontend module. The optional type argument is matched
// because `new Set<string>([...])` is the same declaration, and a regexp that
// missed it would report "no declaration found" for a file that plainly has
// one; both quote styles are accepted for the same reason.
func tsStringSet(t *testing.T, src, name string) []string {
	t.Helper()
	src = stripTSLineComments(src)
	decl := regexp.MustCompile(`export const ` + regexp.QuoteMeta(name) + ` = new Set(?:<[^>]*>)?\(\[([^\]]*)\]\)`)
	m := decl.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no `export const %s = new Set([...])` declaration found", frontendWorkerStatusFile, name)
	}
	var out []string
	for _, lit := range regexp.MustCompile(`'([^']*)'|"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
		if lit[1] != "" {
			out = append(out, lit[1])
		} else {
			out = append(out, lit[2])
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: %s parsed as empty", frontendWorkerStatusFile, name)
	}
	sort.Strings(out)
	return out
}

// sqlInList splits a SQL IN-list literal ("'a', 'b'") into its members.
func sqlInList(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		if v := strings.Trim(strings.TrimSpace(part), "'"); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func readFrontendWorkerStatus(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Clean(frontendWorkerStatusFile))
	if err != nil {
		t.Fatalf("read %s: %v", frontendWorkerStatusFile, err)
	}
	return string(src)
}

// The UI's status axis mirrors dispatchStatuses exactly, with no carve-out.
// 'monitoring' used to be subtracted here on the grounds that the dashboard
// filters those rows out as bellows pseudo-workers — which db.go's own
// WorkerStatus.IsMonitorOnly documents as wrong: a pipeline flips its row to
// monitoring at Warden approval and holds it through push, PR creation and
// worktree removal, and such a row has a real log_path and a smith- id, so the
// UI's isBellowsMonitor does not catch it. Nothing miscounts today because
// pipeline.go stamps phase 'bellows' alongside that status and the phase axis
// excludes the row — but a carve-out written as an unconditional skip made
// 'monitoring' the one status this guard structurally could not check, and it
// is precisely the status where the two sides are documented to be able to
// disagree. The UI now counts it and keeps it out of its panelling set instead,
// which is a question this guard has no opinion about.
func TestFrontendDispatchStatusesMatchDispatchQuery(t *testing.T) {
	got := tsStringSet(t, readFrontendWorkerStatus(t), "DISPATCH_STATUSES")
	want := sqlInList(dispatchStatuses)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DISPATCH_STATUSES in %s is %v, but ActiveDispatchWorkers counts %v.\n"+
			"The dashboard derives its idle-slot count from this set; update the .ts file to match.",
			frontendWorkerStatusFile, got, want)
	}
}

// The phase axis is compared against the vocabulary the PAYLOAD can carry, not
// against the column's. The workers IPC handler rewrites a ready monitor's
// stored 'bellows' to 'ready_to_merge' before the payload leaves the daemon
// (state.PhaseDisplayRewrite), so the dashboard reads one phase that is in no
// SQL list and in no row. Compared against the SQL list alone this guard
// demanded the frontend omit exactly that value — making the correct fix the
// one thing it forbade — while the phase never being counted rested on the
// other axis (every rewritten row also carries status 'monitoring') rather than
// on anything asserted here.
func TestFrontendBackgroundPhasesMatchPayloadPhases(t *testing.T) {
	got := tsStringSet(t, readFrontendWorkerStatus(t), "BACKGROUND_PHASES")

	// A rewrite target inherits its source's answer: 'ready_to_merge' belongs
	// in the background set because the 'bellows' it replaces is there. A
	// rewrite of a phase that DOES hold a dispatch slot must not be added — the
	// payload would be renaming a slot-holder, not a background worker — so the
	// source is tested rather than assumed.
	background := sqlInList(backgroundPhases)
	want := append([]string(nil), background...)
	for _, phase := range background {
		if rewritten, ok := PhaseDisplayRewrite(phase); ok {
			want = append(want, rewritten)
		}
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("BACKGROUND_PHASES in %s is %v, but the workers payload's non-dispatch phases are %v "+
			"(ActiveDispatchWorkers' exclusions plus the handler's display rewrites).\n"+
			"A phase missing from the .ts file makes that worker eat a Smith slot on the dashboard "+
			"that the daemon would happily dispatch into; update the .ts file to match.",
			frontendWorkerStatusFile, got, want)
	}
}

// TestPhaseDisplayRewriteTargetsAreNotStored pins the property the guard above
// relies on: a rewrite target names a phase no row holds, so adding it to the
// frontend's background set cannot mask a stored phase that should have been
// listed in backgroundPhases under its own name.
func TestPhaseDisplayRewriteTargetsAreNotStored(t *testing.T) {
	for source, target := range phaseDisplayRewrites {
		if _, ok := phaseDisplayRewrites[target]; ok {
			t.Errorf("phase %q is both a rewrite target and a rewrite source", target)
		}
		if source == target {
			t.Errorf("phase %q rewrites to itself", source)
		}
	}
}

// TestTSStringSetIgnoresCommentsAndQuoteStyles covers the parser itself against
// the edits the parsed module's own style invites: prose between the members of
// a literal (WORKER_STATUS_CLASSES already carries a three-line comment between
// two of its entries), a member written with double quotes, and the type
// argument of `new Set<string>([...])`. Each of those used to either invent
// members or make the declaration unfindable, and both failures name the
// frontend rather than the guard.
func TestTSStringSetIgnoresCommentsAndQuoteStyles(t *testing.T) {
	src := `// 'ignored' at file scope
export const EXAMPLE = new Set<string>([
  'alpha',
  // 'phantom' would be a member if comments were scanned
  "beta", // trailing 'noise'
])
const path = 'https://example.invalid//not-a-comment'
`
	got := tsStringSet(t, src, "EXAMPLE")
	if strings.Join(got, ",") != "alpha,beta" {
		t.Errorf("tsStringSet parsed %v, want [alpha beta]", got)
	}
	if !strings.Contains(stripTSLineComments(src), "https://example.invalid//not-a-comment") {
		t.Error("stripTSLineComments cut a `//` inside a string literal")
	}
}

// TestWorkersPhaseColumnIsNotNull pins the schema property the dashboard's
// isBackgroundPhase depends on. The capacity queries filter with `phase NOT IN
// (...)`, which in SQLite evaluates to NULL — and so drops the row — for a NULL
// phase, while the frontend treats an absent phase as a Smith row that holds a
// slot. The two only agree because the column cannot be NULL: an unstamped row
// holds '', which compares normally and is counted by both. Were the column
// ever made nullable, the dashboard would report one fewer idle slot than the
// daemon will dispatch into — the mirror image of the bug this file guards.
func TestWorkersPhaseColumnIsNotNull(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.conn.Query(`PRAGMA table_info(workers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			dflt             *string
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name != "phase" {
			continue
		}
		found = true
		if notNull != 1 {
			t.Errorf("workers.phase is nullable; ActiveDispatchWorkers' `phase NOT IN (...)` silently drops "+
				"a NULL row while the dashboard counts it (see isBackgroundPhase in %s)", frontendWorkerStatusFile)
		}
		if dflt == nil || strings.Trim(*dflt, "'") != "" {
			got := "<none>"
			if dflt != nil {
				got = *dflt
			}
			t.Errorf("workers.phase default is %s, want the empty string an unstamped row is read as", got)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("workers table has no phase column")
	}
}
