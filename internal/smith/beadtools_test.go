package smith

import (
	"context"
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

// SpawnSmith asks for the bead tools; SpawnWithProvider (review stages, triage) never does.
func TestSpawnSmithAsksForTheToolsAndOthersDoNot(t *testing.T) {
	var got []SpawnOptions
	orig := spawnWithOptions
	defer func() { spawnWithOptions = orig }()
	spawnWithOptions = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string, opts SpawnOptions) (*Process, error) {
		got = append(got, opts)
		return nil, nil
	}
	pv := provider.Provider{Kind: provider.Claude}
	_, _ = SpawnSmith(context.Background(), "", "", "", pv, nil)
	_, _ = SpawnWithProvider(context.Background(), "", "", "", pv, nil)
	_, _ = Spawn(context.Background(), "", "", "", nil)
	if len(got) != 3 || !got[0].BeadTools || got[1].BeadTools || !got[2].BeadTools {
		t.Errorf("options = %+v, want SpawnSmith and Spawn (the CLI worker's smith) with BeadTools, SpawnWithProvider without", got)
	}
}
