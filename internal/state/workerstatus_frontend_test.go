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
// ActiveDispatchWorkers in TypeScript (SLOT_STATUSES and BACKGROUND_PHASES in
// internal/web/frontend/src/lib/workerStatus.ts). A TS-side assertion cannot
// guard that: it holds for any pair of values somebody edits both halves of,
// and it holds unchanged when the Go list moves — which is exactly the drift
// that let the UI omit 'stalled' and offer the operator a dispatch slot the
// daemon would never fill. These tests read the .ts file and compare it with
// the SQL the queries are actually built from.
const frontendWorkerStatusFile = "../web/frontend/src/lib/workerStatus.ts"

// tsStringSet extracts the string literals of an exported `new Set([...])`
// declaration from the frontend module.
func tsStringSet(t *testing.T, src, name string) []string {
	t.Helper()
	decl := regexp.MustCompile(`export const ` + regexp.QuoteMeta(name) + ` = new Set\(\[([^\]]*)\]\)`)
	m := decl.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no `export const %s = new Set([...])` declaration found", frontendWorkerStatusFile, name)
	}
	var out []string
	for _, lit := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, lit[1])
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

func TestFrontendSlotStatusesMatchDispatchQuery(t *testing.T) {
	got := tsStringSet(t, readFrontendWorkerStatus(t), "SLOT_STATUSES")

	// 'monitoring' is deliberately absent from the UI set: those rows are the
	// bellows PR-monitor pseudo-workers, which the dashboard filters out by
	// isBellowsMonitor because they carry no claude log to show.
	var want []string
	for _, s := range sqlInList(dispatchStatuses) {
		if s == "monitoring" {
			continue
		}
		want = append(want, s)
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("SLOT_STATUSES in %s is %v, but ActiveDispatchWorkers counts %v (minus 'monitoring').\n"+
			"The dashboard derives its idle-slot count from this set; update the .ts file to match.",
			frontendWorkerStatusFile, got, want)
	}
}

func TestFrontendBackgroundPhasesMatchDispatchQuery(t *testing.T) {
	got := tsStringSet(t, readFrontendWorkerStatus(t), "BACKGROUND_PHASES")
	want := sqlInList(backgroundPhases)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("BACKGROUND_PHASES in %s is %v, but ActiveDispatchWorkers excludes %v.\n"+
			"A phase missing from the .ts file makes that worker eat a Smith slot on the dashboard "+
			"that the daemon would happily dispatch into; update the .ts file to match.",
			frontendWorkerStatusFile, got, want)
	}
}
