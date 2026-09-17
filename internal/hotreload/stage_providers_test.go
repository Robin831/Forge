package hotreload

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// TestReload_PicksUpAssayPassStageProviders writes a per-pass Assay chain into
// the config file — global and per-anvil — and checks reload() swaps it into
// the live config with no restart. The key is dotted, which is exactly what a
// viper-decoded map could not carry: config.Load failed outright on it, so the
// watcher logged a reload error and kept the old config.
func TestReload_PicksUpAssayPassStageProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	write("settings:\n  stage_providers:\n    smith: [claude]\nanvils:\n  forge:\n    path: /tmp/forge\n")
	initial, err := config.Load(path)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}

	w := NewWatcher(path, initial, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var notified bool
	w.OnChange(func(_, _ *config.Config) { notified = true })

	write("settings:\n  stage_providers:\n    smith: [claude]\n    assay.conventions: [gemini, claude]\n" +
		"anvils:\n  forge:\n    path: /tmp/forge\n    stage_providers:\n      assay.logic: [copilot]\n")
	w.reload()

	cur := w.Current()
	if cur == initial {
		t.Fatal("reload kept the old config; the assay.conventions edit was not applied")
	}
	if got := cur.Settings.StageProviders["assay.conventions"]; len(got) != 2 || got[0] != "gemini" || got[1] != "claude" {
		t.Errorf("settings.stage_providers[assay.conventions] = %v, want [gemini claude] (map %v)", got, cur.Settings.StageProviders)
	}
	if got := cur.Settings.StageProviders["smith"]; len(got) != 1 || got[0] != "claude" {
		t.Errorf("settings.stage_providers[smith] = %v, want [claude]", got)
	}
	if got := cur.Anvils["forge"].StageProviders["assay.logic"]; len(got) != 1 || got[0] != "copilot" {
		t.Errorf("anvils.forge.stage_providers[assay.logic] = %v, want [copilot]", got)
	}
	if !notified {
		t.Error("OnChange callbacks were not notified of the stage_providers change")
	}
}
