package poller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/vardebeads"
)

// Only through Varde does the poller skip needs-human; with bd it asks bd ready exactly as before (DevInfra-gbx.8).
func TestPollSkipsNeedsHumanOnlyThroughVarde(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as bd")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\necho \"$@\" > '"+argsFile+"'\necho '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	poll := func() string {
		p := New(nil)
		if _, err := p.pollAnvil(context.Background(), "a", config.AnvilConfig{Path: dir}); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(argsFile)
		return string(raw)
	}
	if got := poll(); strings.Contains(got, "exclude-label") {
		t.Errorf("with bd: %q", got)
	}
	t.Setenv(vardebeads.EnvURL, "http://varde.invalid")
	t.Setenv(vardebeads.EnvKeyFile, "/nowhere")
	if got := poll(); !strings.Contains(got, "--exclude-label needs-human") {
		t.Errorf("through Varde: %q", got)
	}
}
