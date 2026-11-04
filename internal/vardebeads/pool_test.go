package vardebeads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolvePool(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		// Munin as checked in: no issue-prefix, issues.jsonl with its ids.
		"fhi.metadata/.beads/config.yaml":  "# issue-prefix: \"\"\nsync.remote: \"http://127.0.0.1:50051/munin\"\n",
		"fhi.metadata/.beads/issues.jsonl": `{"id":"Fhi.Metadata-06w","title":"x"}` + "\n",
		// Heimdall: its database is bd's default name, so only the ids tell.
		"heimdall/.beads/config.yaml":  "sync.remote: \"http://127.0.0.1:50051/beads\"\n",
		"heimdall/.beads/issues.jsonl": `{"id":"Heimdall-01lw"}` + "\n",
		// DevInfra: no issues.jsonl; the database names the pool.
		"devinfra/.beads/config.yaml": "sync.remote: \"http://127.0.0.1:50051/devinfra\" # the pool\n",
		// Explorer shares Munin's beads through a hand-written redirect.
		"fhi.munin.explorer/.beads/redirect": "# Beads live in Munin.\n../fhi.metadata/.beads\n",
		"prefixed/.beads/config.yaml":        "issue-prefix: 'DevInfra'\nsync.remote: \"http://x/munin\"\n",
		"unknown/.beads/config.yaml":         "sync.remote: \"http://127.0.0.1:50051/beads\"\n",
	})
	cases := []struct {
		name, explicit, anvil, want, err string
	}{
		{"explicit wins", "heimdall", "fhi.metadata", "heimdall", ""},
		{"munin from issues.jsonl", "", "fhi.metadata", "munin", ""},
		{"heimdall from issues.jsonl", "", "heimdall", "heimdall", ""},
		{"devinfra from sync.remote", "", "devinfra", "devinfra", ""},
		{"explorer through the redirect", "", "fhi.munin.explorer", "munin", ""},
		{"issue-prefix first", "", "prefixed", "devinfra", ""},
		{"nothing names one", "", "unknown", "", "set varde_pool"},
		{"no beads at all", "", "missing", "", "set varde_pool"},
	}
	for _, c := range cases {
		got, err := ResolvePool(c.explicit, filepath.Join(root, c.anvil))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: got %q, err %v; want an error naming %q", c.name, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, err %v; want %q", c.name, got, err, c.want)
		}
	}
	if _, err := ResolvePool("", ""); err == nil {
		t.Error("no path and no varde_pool must fail")
	}
}
