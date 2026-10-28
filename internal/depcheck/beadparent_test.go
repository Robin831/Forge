package depcheck

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog routes slog.Default into a buffer for the test's duration.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestConsolidatedCreateArgs_PassesConfiguredParent(t *testing.T) {
	logs := captureSlog(t)
	s := newScanner(nil)
	s.SetBeadParent(func(anvil string) string {
		if anvil == "heimdall" {
			return "Heimdall-6tyq"
		}
		return ""
	})

	args := strings.Join(s.consolidatedCreateArgs("heimdall", "Package updates", "desc", "3"), " ")
	if !strings.Contains(args, "--parent Heimdall-6tyq --no-inherit-labels") {
		t.Fatalf("args lack the parent: %s", args)
	}
	if !strings.Contains(args, "--type=chore") {
		t.Errorf("args lack an explicit type: %s", args)
	}
	if logs.Len() != 0 {
		t.Errorf("a configured parent must not warn, logged %q", logs.String())
	}
}

func TestConsolidatedCreateArgs_NoParentKeepsOldArgsAndWarns(t *testing.T) {
	logs := captureSlog(t)
	s := newScanner(nil)

	got := s.consolidatedCreateArgs("munin", "Package updates", "desc", "2")
	want := []string{"create", "--title=Package updates", "--description=desc", "--type=chore",
		"--priority=2", "--labels=" + DepsUpdateLabel, "--json"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want today's %q", got, want)
	}
	if !strings.Contains(logs.String(), "no parent epic") || !strings.Contains(logs.String(), "anvil=munin") {
		t.Errorf("want a no-parent warning for munin, logged %q", logs.String())
	}
}
