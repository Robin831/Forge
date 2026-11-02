package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/vardebeads"
)

// Through Varde a retry never asks to add a forge label back: Varde refuses it, and the release would then be half done.
func TestRestoreBeadAfterPauseThroughVardeAddsNoForgeLabel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as bd")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\necho \"$@\" > '"+argsFile+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(vardebeads.EnvURL, "http://varde.invalid")
	t.Setenv(vardebeads.EnvKeyFile, filepath.Join(dir, "no-key"))
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), runCtx: context.Background()}
	if _, err := d.restoreBeadAfterPause(context.Background(), "B-1", config.AnvilConfig{Path: dir, AutoDispatchTag: "forgeSkybert"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(argsFile)
	args := string(raw)
	if strings.Contains(args, "forgeSkybert") || !strings.Contains(args, "--remove-label=needs-human") {
		t.Errorf("restore args = %q, want needs-human taken off and no forge label added", args)
	}
}
