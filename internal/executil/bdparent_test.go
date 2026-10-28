package executil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestBdParentArgs_WithParent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	got := BdParentArgs(logger, "heimdall", "depcheck", " Heimdall-6tyq ")
	want := []string{"--parent", "Heimdall-6tyq", "--no-inherit-labels"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("BdParentArgs = %q, want %q", got, want)
	}
	if buf.Len() != 0 {
		t.Errorf("a configured parent must not warn, logged %q", buf.String())
	}
}

func TestBdParentArgs_EmptyWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if got := BdParentArgs(logger, "munin", "wicket", ""); got != nil {
		t.Fatalf("an empty parent must add no flags, got %q", got)
	}
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 {
		t.Fatalf("want exactly one warning, got %q", out)
	}
	for _, s := range []string{"no parent epic", "anvil=munin", "kind=wicket", "beads.default_parent"} {
		if !strings.Contains(out, s) {
			t.Errorf("warning %q lacks %q", out, s)
		}
	}
}
