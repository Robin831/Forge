package vardebeads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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
	// Windows has no POSIX mode bits (a writable file reads as 0666); the content checks below still run there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
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
	v := cfg.MCPServers[SmithMCPServer]
	if v.Type != "http" || v.URL != "https://varde.example/mcp" || v.Headers["Authorization"] != "Bearer smith-k" {
		t.Errorf("config = %+v", v)
	}
	t.Setenv(EnvURL, "http://varde.example")
	if _, err := SmithMCPConfig(filepath.Join(dir, "state")); err == nil {
		t.Error("a plain-http URL must not get the key")
	}
}

// Smiths spawned together each read a whole, valid config (DevInfra-gbx.8).
func TestSmithMCPConfigUnderConcurrentSpawns(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "smith.key")
	if err := os.WriteFile(key, []byte("smith-k"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvURL, "https://varde.example")
	t.Setenv(EnvKeyFile, "/nowhere")
	t.Setenv(EnvSmithKeyFile, key)
	state := filepath.Join(dir, "state")
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 1 {
				// A rotated key, swapped in whole as a mounted Secret is: the content changes, so writes really race.
				tmp := fmt.Sprintf("%s.%d", key, i)
				_ = os.WriteFile(tmp, []byte(fmt.Sprintf("smith-k%d", i)), 0o600)
				_ = os.Rename(tmp, key)
			}
			path, err := SmithMCPConfig(state)
			if err != nil {
				errs <- err
				return
			}
			raw, err := os.ReadFile(path)
			if err == nil {
				var v map[string]any
				err = json.Unmarshal(raw, &v)
			}
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(state, "smith-mcp-*.json"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left: %v", leftovers)
	}
}
