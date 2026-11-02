package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// The pipeline's smith prompt names the anvil's epic, so a smith with Varde's tools files under it (DevInfra-gbx.8).
func TestSmithPromptCarriesTheAnvilsEpicThroughVarde(t *testing.T) {
	key := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_VARDE_URL", "https://varde.invalid")
	t.Setenv("FORGE_VARDE_KEY_FILE", "/nowhere")
	t.Setenv("FORGE_VARDE_SMITH_KEY_FILE", key)
	db := newTestDB(t)
	params, _, _ := baseParams(t, db)
	params.AnvilConfig.BeadsParent = "EP-5"
	var prompt string
	orig := params.SmithRunner
	params.SmithRunner = func(ctx context.Context, wt, promptText, logDir string, pv provider.Provider, flags []string) (*smith.Process, error) {
		if prompt == "" {
			prompt = promptText
		}
		return orig(ctx, wt, promptText, logDir, pv, flags)
	}
	_ = Run(context.Background(), params)
	if !strings.Contains(prompt, "`parent`: `EP-5`") {
		t.Errorf("the smith prompt does not name the anvil's epic")
	}
}
