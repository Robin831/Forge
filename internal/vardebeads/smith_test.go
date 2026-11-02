package vardebeads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSmithMCPConfig(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "smith.key")
	if err := os.WriteFile(key, []byte("smith-k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvURL, "https://varde.example/")
	t.Setenv(EnvKeyFile, "/nowhere")
	if SmithEnabled() {
		t.Fatal("no smith key set, yet enabled")
	}
	t.Setenv(EnvSmithKeyFile, key)
	if !SmithEnabled() {
		t.Fatal("smith key set, Varde on, not enabled")
	}
	path, err := SmithMCPConfig(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var cfg struct {
		MCPServers map[string]struct {
			Type, URL string
			Headers   map[string]string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	v := cfg.MCPServers["varde"]
	if v.Type != "http" || v.URL != "https://varde.example/mcp" || v.Headers["Authorization"] != "Bearer smith-k" {
		t.Errorf("config = %+v", v)
	}
	t.Setenv(EnvURL, "http://varde.example")
	if _, err := SmithMCPConfig(filepath.Join(dir, "state")); err == nil {
		t.Error("a plain-http URL must not get the key")
	}
}
