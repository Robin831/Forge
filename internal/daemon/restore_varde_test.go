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
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vardebeads"
)

// fakeBd puts a bd on PATH that records its arguments, and returns a reader for the last call.
func fakeBd(t *testing.T) (dir string, last func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as bd")
	}
	dir = t.TempDir()
	argsFile := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\necho \"$@\" > '"+argsFile+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, func() string { raw, _ := os.ReadFile(argsFile); return strings.TrimSpace(string(raw)) }
}

func vardeMode(t *testing.T) {
	t.Setenv(vardebeads.EnvURL, "http://varde.invalid")
	t.Setenv(vardebeads.EnvKeyFile, "/nowhere")
}

func testDaemon(t *testing.T, dir string) *Daemon {
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	d := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), runCtx: context.Background()}
	d.cfg.Store(&config.Config{Anvils: map[string]config.AnvilConfig{"a": {Path: dir, AutoDispatchTag: "forgeSkybert"}}})
	return d
}

// Through Varde an escalation keeps the tag and marks the bead; a retry takes the mark off and adds no forge label.
func TestEscalationAndRetryThroughVarde(t *testing.T) {
	dir, last := fakeBd(t)
	vardeMode(t)
	d := testDaemon(t, dir)
	d.releaseBeadClaim("B-1", "a", true)
	if got := last(); !strings.Contains(got, "--add-label=needs-human") || strings.Contains(got, "forgeSkybert") {
		t.Errorf("escalation ran %q", got)
	}
	if _, err := d.restoreBeadAfterPause(context.Background(), "B-1", config.AnvilConfig{Path: dir, AutoDispatchTag: "forgeSkybert"}); err != nil {
		t.Fatal(err)
	}
	if got := last(); !strings.Contains(got, "--remove-label=needs-human") || strings.Contains(got, "forgeSkybert") {
		t.Errorf("retry ran %q", got)
	}
}

// Without Varde's environment the Forge behaves as before DevInfra-gbx.8 (Robin's private Forge stays on bd).
func TestEscalationAndRetryWithBdAreUnchanged(t *testing.T) {
	dir, last := fakeBd(t)
	d := testDaemon(t, dir)
	d.releaseBeadClaim("B-1", "a", true)
	if got := last(); !strings.Contains(got, "--remove-label=forgeSkybert") || strings.Contains(got, "needs-human") {
		t.Errorf("escalation ran %q", got)
	}
	if _, err := d.restoreBeadAfterPause(context.Background(), "B-1", config.AnvilConfig{Path: dir, AutoDispatchTag: "forgeSkybert"}); err != nil {
		t.Fatal(err)
	}
	if got := last(); !strings.Contains(got, "--add-label=forgeSkybert") || strings.Contains(got, "needs-human") {
		t.Errorf("retry ran %q", got)
	}
}

func TestVardeDispatchWarnsForUntaggedAnvils(t *testing.T) {
	cfg := &config.Config{Anvils: map[string]config.AnvilConfig{"tagged": {AutoDispatch: "tagged", AutoDispatchTag: "forgeSkybert"}, "all": {AutoDispatch: "all"}}}
	if got := untaggedUnderVarde(cfg); len(got) != 0 {
		t.Errorf("with bd: %v", got)
	}
	vardeMode(t)
	if got := untaggedUnderVarde(cfg); len(got) != 1 || got[0] != "all" {
		t.Errorf("through Varde: %v", got)
	}
}
