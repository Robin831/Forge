package questgiver

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestCreateArgs_PassesParent(t *testing.T) {
	var logs bytes.Buffer
	m := New(nil, time.Minute, time.Minute, nil, func() QuestExecutor { return nopExecutor{} })
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	m.SetBeadParent(func(anvil string) string { return map[string]string{"munin": "Fhi.Metadata-eeuey"}[anvil] })

	args := strings.Join(m.createArgs("munin", "Quest failed: login", "d"), " ")
	if !strings.Contains(args, "--parent Fhi.Metadata-eeuey --no-inherit-labels") || !strings.Contains(args, "--type bug") {
		t.Fatalf("args = %s", args)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected warning %q", logs.String())
	}
}

func TestCreateArgs_NoParentKeepsOldArgsAndWarns(t *testing.T) {
	var logs bytes.Buffer
	m := New(nil, time.Minute, time.Minute, nil, func() QuestExecutor { return nopExecutor{} })
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))

	got := m.createArgs("munin", "T", "D")
	want := []string{"create", "--title", "T", "--description", "D", "--type", "bug", "--priority=1", "--json"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %q, want today's %q", got, want)
	}
	if !strings.Contains(logs.String(), "no parent epic") || !strings.Contains(logs.String(), "kind=questgiver") {
		t.Errorf("want a no-parent warning, logged %q", logs.String())
	}
}
