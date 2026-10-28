package ledger

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestNewBeadArgs_PassesParent(t *testing.T) {
	logs := captureSlog(t)
	args := strings.Join(newBeadArgs("heimdall", "Heimdall-6tyq", "T", "D", "feature", 2), " ")
	if !strings.Contains(args, "--parent Heimdall-6tyq --no-inherit-labels") || !strings.Contains(args, "--type feature") {
		t.Fatalf("args = %s", args)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected warning %q", logs.String())
	}
}

func TestNewBeadArgs_NoParentWarnsAndEmptyTypeIsTask(t *testing.T) {
	logs := captureSlog(t)
	got := newBeadArgs("munin", "", "T", "D", "", 2)
	want := []string{"create", "--title", "T", "--description", "D", "--type", "task", "--priority", "2", "--json"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if !strings.Contains(logs.String(), "no parent epic") || !strings.Contains(logs.String(), "kind=ledger") {
		t.Errorf("want a no-parent warning, logged %q", logs.String())
	}
}

// The TUI discards log output while it runs, so the missing parent is shown in its event log.
func TestBeadCreatedMsg_NoParentShowsWarning(t *testing.T) {
	m := NewModel(map[string]string{"munin": "/m"}, map[string]config.AnvilConfig{}, nil)
	updated, _ := m.Update(BeadCreatedMsg{ID: "Fhi.Metadata-x", NoParent: true})
	var found bool
	for _, e := range updated.(*Model).eventLog {
		if e.Level == EventWarn && strings.Contains(e.Message, "no parent epic") {
			found = true
		}
	}
	if !found {
		t.Error("want a warning event for a bead created with no parent")
	}
}
