package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// A relative anvil path is read against the daemon's directory (--base), not the anvil it runs in.
func TestAnvilParentForCwdReadsRelativePathsAgainstTheBase(t *testing.T) {
	base := t.TempDir()
	anvil := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(anvil, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := cfg
	defer func() { cfg = old }()
	cfg = &config.Config{Anvils: map[string]config.AnvilConfig{"r": {Path: "./repo", BeadsParent: "EP-1"}, "other": {Path: "/elsewhere", BeadsParent: "EP-2"}}}
	t.Chdir(filepath.Join(anvil, "sub"))
	if got := anvilParentForCwd(base); got != "EP-1" {
		t.Errorf("relative anvil: %q", got)
	}
	if got := anvilParentForCwd(""); got != "" {
		t.Errorf("without a base a relative path matches nothing: %q", got)
	}
}

// A read goes to the anvil's varde_pool, else to the pool its own .beads names; outside every anvil, to the pool of
// the directory itself.
func TestPoolForCwd(t *testing.T) {
	base := t.TempDir()
	for name, body := range map[string]string{
		"munin/.beads/issues.jsonl": `{"id":"Fhi.Metadata-1"}` + "\n",
		"loose/.beads/config.yaml":  "sync.remote: \"http://x/devinfra\"\n",
	} {
		path := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "set", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := cfg
	defer func() { cfg = old }()
	cfg = &config.Config{Anvils: map[string]config.AnvilConfig{
		"munin": {Path: filepath.Join(base, "munin")},
		"set":   {Path: filepath.Join(base, "set"), VardePool: "heimdall"},
	}}
	cases := []struct{ dir, want string }{{"munin", "munin"}, {"set/sub", "heimdall"}, {"loose", "devinfra"}}
	for _, c := range cases {
		t.Chdir(filepath.Join(base, c.dir))
		anvil, path := anvilForCwd(base)
		if got, err := poolForCwd(anvil, path); err != nil || got != c.want {
			t.Errorf("in %s: pool %q, err %v; want %q", c.dir, got, err, c.want)
		}
	}
	t.Chdir(base)
	anvil, path := anvilForCwd(base)
	if _, err := poolForCwd(anvil, path); err == nil {
		t.Error("a directory with no beads and no anvil must fail loudly")
	}
}
