package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
)

// withFakeBd puts a `bd` shell script on PATH so the real bdShowJSON closure
// runs end to end. Every other test in this package replaces that closure with
// a stub, which is exactly why these exist: the flag and the classification live
// in the closure a stub deletes, so without a fake bd nothing executes them and
// a revert to a bare BdCommand would regress in silence.
func withFakeBd(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The bead detail view's "blocks" list is read straight out of the dependents
// array, which bd emits only when asked. The fake answers an unflagged show the
// way bd 1.1.2 does — a bare dependent_count, no array — so a closure that lost
// the flag does not fail here, it renders the bead as blocking nothing.
func TestBdShowJSON_AsksForTheDependentsArray(t *testing.T) {
	withFakeBd(t, `flag=""
for a in "$@"; do
  case "$a" in --include-dependents) flag=1 ;; esac
done
if [ -n "$flag" ]; then
  echo '[{"id":"Forge-root","status":"open","dependent_count":1,"dependents":[{"id":"Forge-child","dependency_type":"blocks","status":"open"}]}]'
else
  echo '[{"id":"Forge-root","status":"open","dependent_count":1}]'
fi`)

	entry, err := fetchBeadShow(context.Background(), "", "Forge-root", nil)
	if err != nil {
		t.Fatalf("fetchBeadShow: %v", err)
	}
	if len(entry.Dependents) != 1 || entry.Dependents[0].ID != "Forge-child" {
		t.Fatalf("dependents = %+v, want the child bd only reports when asked", entry.Dependents)
	}
}

// A bd too old for the flag must surface as the named sentinel rather than as an
// empty dependency list, which is indistinguishable from a bead that really
// blocks nothing.
func TestBdShowJSON_OlderBdIsANamedError(t *testing.T) {
	withFakeBd(t, `echo "Error: unknown flag: --include-dependents" >&2
exit 1`)

	_, err := bdShowJSON(context.Background(), "", "Forge-root")
	if err == nil {
		t.Fatal("bdShowJSON succeeded against a bd without the flag")
	}
	if !errors.Is(err, executil.ErrIncludeDependentsUnsupported) {
		t.Errorf("error %v does not wrap ErrIncludeDependentsUnsupported", err)
	}
}

// Capturing stderr for the classification stops exec.ExitError from carrying
// it, so the closure has to fold it back in: every caller here degrades to an
// empty dep list, and bd's own diagnostic is all that separates a real failure
// from a bead with no dependencies.
func TestBdShowJSON_CarriesBdsDiagnostic(t *testing.T) {
	withFakeBd(t, `echo "Error: database is locked" >&2
exit 1`)

	_, err := bdShowJSON(context.Background(), "", "Forge-root")
	if err == nil {
		t.Fatal("bdShowJSON succeeded against a failing bd")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("error %q drops bd's diagnostic", err)
	}
}

// And the failure every caller swallows is logged once, at Warn and by name,
// when it is the operator-fixable one.
func TestFetchBeadShow_LogsAnOlderBd(t *testing.T) {
	withFakeBd(t, `echo "Error: unknown flag: --include-dependents" >&2
exit 1`)

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if _, err := fetchBeadShow(context.Background(), "", "Forge-root", logger); err == nil {
		t.Fatal("fetchBeadShow succeeded against a bd without the flag")
	}
	if !strings.Contains(logged.String(), executil.BdIncludeDependentsFlag) {
		t.Errorf("log %q does not name %s", logged.String(), executil.BdIncludeDependentsFlag)
	}
}
