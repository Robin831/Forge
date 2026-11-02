package poller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/vardebeads"
)

// Only through Varde does the poller skip a bead marked needs-human; with bd it is offered as before (DevInfra-gbx.8).
func TestPollSkipsNeedsHumanOnlyThroughVarde(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as bd")
	}
	dir := t.TempDir()
	ready := `[{"id":"B-1","title":"a","status":"open","labels":["forgeSkybert"]},{"id":"B-2","title":"b","status":"open","labels":["forgeSkybert","Needs-Human"]},{"id":"B-3","title":"c","status":"open","labels":["forgeSkybert","schematic-wiring"]}]`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\necho '"+ready+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ids := func() []string {
		beads, err := New(nil).pollAnvil(context.Background(), "a", config.AnvilConfig{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range beads {
			out = append(out, b.ID)
		}
		return out
	}
	if got := ids(); len(got) != 3 {
		t.Errorf("with bd: %v", got)
	}
	t.Setenv(vardebeads.EnvURL, "https://varde.invalid")
	t.Setenv(vardebeads.EnvKeyFile, "/nowhere")
	if got := ids(); len(got) != 1 || got[0] != "B-1" {
		t.Errorf("through Varde: %v", got)
	}
}
