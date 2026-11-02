package smith

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/vardebeads"
)

// Only the implementing Claude session gets Varde's bead tools, and only with the smiths' key mounted (DevInfra-gbx.8).
func TestBeadToolArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	claude, copilot := provider.Provider{Kind: provider.Claude}, provider.Provider{Kind: provider.Copilot}
	if got := beadToolArgs(SpawnOptions{BeadTools: true}, claude); got != nil {
		t.Errorf("plain bd: %v", got)
	}
	key := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(vardebeads.EnvURL, "https://varde.example")
	t.Setenv(vardebeads.EnvKeyFile, "/nowhere")
	t.Setenv(vardebeads.EnvSmithKeyFile, key)
	if got := beadToolArgs(SpawnOptions{BeadTools: true}, claude); len(got) != 2 || got[0] != "--mcp-config" {
		t.Errorf("smith through Varde: %v", got)
	}
	if got := beadToolArgs(SpawnOptions{LogPrefix: "warden"}, claude); got != nil {
		t.Errorf("a review stage: %v", got)
	}
	if got := beadToolArgs(SpawnOptions{BeadTools: true}, copilot); got != nil {
		t.Errorf("a provider without --mcp-config: %v", got)
	}
}
