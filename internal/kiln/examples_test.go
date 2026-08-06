package kiln

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleManifestDir holds the reference manifests documented in
// docs/preview-manifests.md.
const exampleManifestDir = "testdata/manifests"

// exampleManifestDoc is the page those manifests are printed in. The tests
// below keep the two in sync in both directions: every fixture must be a
// manifest the real loader accepts, and every fixture must appear verbatim in
// the doc, so a documented example can never claim a schema Kiln rejects.
const exampleManifestDoc = "../../docs/preview-manifests.md"

// exampleManifestExpectations pins the shape of each documented example, so a
// well-meaning edit that guts an example (drops the DB lifecycle scripts, say)
// fails here rather than quietly making the prose wrong.
var exampleManifestExpectations = map[string]struct {
	services  []string
	entry     string
	lifecycle bool // declares setup and teardown
}{
	"go-vite.yaml": {
		services:  []string{"api", "web"},
		entry:     "web",
		lifecycle: false,
	},
	"dotnet-react-mssql.yaml": {
		// The MSSQL server is shared and long-lived, so it is deliberately not
		// a service — setup/teardown manage a database on it instead.
		services:  []string{"api", "client"},
		entry:     "client",
		lifecycle: true,
	},
}

func TestExampleManifestsLoad(t *testing.T) {
	files := exampleManifestFiles(t)

	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			want, ok := exampleManifestExpectations[name]
			if !ok {
				t.Fatalf("no expectations for example manifest %s — add an entry to exampleManifestExpectations (and document the example in %s)", name, exampleManifestDoc)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			m, err := Parse(data)
			if err != nil {
				t.Fatalf("the documented example %s does not load: %v", name, err)
			}

			if got := m.Services.Names(); !equalStrings(got, want.services) {
				t.Errorf("services: got %v, want %v", got, want.services)
			}
			entry, ok := m.Entry()
			if !ok {
				t.Fatal("no entry service — the preview link would have nowhere to point")
			}
			if entry.Name != want.entry {
				t.Errorf("entry service: got %q, want %q", entry.Name, want.entry)
			}
			if hasLifecycle := m.Setup != "" && m.Teardown != ""; hasLifecycle != want.lifecycle {
				t.Errorf("setup+teardown declared: got %t, want %t (setup=%q teardown=%q)",
					hasLifecycle, want.lifecycle, m.Setup, m.Teardown)
			}

			// Expansion against a realistic context is what a real start does;
			// Parse already validates the templates with probe values, but this
			// also proves the examples reference only declared services.
			ports := make(map[string]int, len(m.Services))
			for i, svc := range m.Services {
				ports[svc.Name] = 42000 + i
			}
			if _, err := m.Expand(Context{PreviewID: "forge_1t6c", Host: "forge.local", Ports: ports}); err != nil {
				t.Fatalf("expanding %s: %v", name, err)
			}
		})
	}
}

// TestExampleManifestsAppearVerbatimInDocs is the anti-drift guard: the YAML a
// reader copies out of the docs is the YAML the test above loads.
func TestExampleManifestsAppearVerbatimInDocs(t *testing.T) {
	raw, err := os.ReadFile(exampleManifestDoc)
	if err != nil {
		t.Fatalf("reading %s: %v", exampleManifestDoc, err)
	}
	doc := normalizeNewlines(string(raw))

	for _, path := range exampleManifestFiles(t) {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(doc, normalizeNewlines(string(data))) {
			t.Errorf("%s does not contain %s verbatim — update the fenced YAML block in the doc to match the fixture", exampleManifestDoc, path)
		}
	}
}

// exampleManifestFiles lists the fixture manifests, failing when there are
// none (a moved directory would otherwise make both tests vacuously pass).
func exampleManifestFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(exampleManifestDir, "*.yaml"))
	if err != nil {
		t.Fatalf("globbing %s: %v", exampleManifestDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no example manifests found in %s", exampleManifestDir)
	}
	return files
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
