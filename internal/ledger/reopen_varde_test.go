package ledger

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/vardebeads"
)

// Through Varde reopen is bd reopen (an update to open would be a release); with bd it is unchanged (DevInfra-gbx.8).
func TestReopenThroughVardeAndWithBd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as bd")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\necho \"$@\" > '"+argsFile+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func() string { ReopenBeadCmd(dir, "B-1")(); raw, _ := os.ReadFile(argsFile); return strings.TrimSpace(string(raw)) }
	if got := run(); got != "update B-1 --status=open" {
		t.Errorf("with bd: %q", got)
	}
	t.Setenv(vardebeads.EnvURL, "http://varde.invalid")
	t.Setenv(vardebeads.EnvKeyFile, "/nowhere")
	if got := run(); got != "reopen B-1" {
		t.Errorf("through Varde: %q", got)
	}
}
