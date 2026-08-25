package assay

import "testing"

func TestPassLogPrefix(t *testing.T) {
	cases := []struct {
		name   string
		runKey string
		pass   string
		want   string
	}{
		{"run key and pass", "1755000000000", "logic", "assay-1755000000000-logic"},
		{"dashed pass name", "1755000000000", "tests-missing", "assay-1755000000000-tests-missing"},
		{"triage", "1755000000000", "triage", "assay-1755000000000-triage"},
		// No key: still named by pass, just not grouped into a run.
		{"no run key", "", "logic", "assay-logic"},
		// A non-numeric key is dropped rather than written: the reader
		// distinguishes the key from the pass name by it being all-digits.
		{"non-numeric run key", "run-a", "logic", "assay-logic"},
		{"neither", "", "", "assay"},
		// The prefix reaches a filesystem path, so it is screened.
		{"path traversal in pass", "1755000000000", "../../etc/passwd", "assay-1755000000000-etcpasswd"},
		{"upper case and spaces", "1755000000000", " Repo Specific ", "assay-1755000000000-repospecific"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PassLogPrefix(tc.runKey, tc.pass); got != tc.want {
				t.Errorf("PassLogPrefix(%q, %q) = %q, want %q", tc.runKey, tc.pass, got, tc.want)
			}
		})
	}
}

// TestPassLogPrefixCoversEveryPass guards the naming for the whole pass table:
// a pass whose name sanitises away would produce a session the panel cannot
// label, which is the state this bead exists to leave behind.
func TestPassLogPrefixCoversEveryPass(t *testing.T) {
	all := append([]passDef{passTriage}, deepPasses...)
	seen := map[string]bool{}
	for _, p := range all {
		got := PassLogPrefix("1755000000000", p.Name)
		want := "assay-1755000000000-" + p.Name
		if got != want {
			t.Errorf("pass %q: prefix = %q, want %q", p.Name, got, want)
		}
		if seen[got] {
			t.Errorf("pass %q: prefix %q collides with an earlier pass", p.Name, got)
		}
		seen[got] = true
	}
}
