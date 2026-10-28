package vulncheck

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

func TestCreateArgs_ParentFromAnvilConfig(t *testing.T) {
	var logs bytes.Buffer
	s := &Scanner{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
		anvils: map[string]config.AnvilConfig{
			"forge": {Path: "/p", Beads: config.BeadsConfig{DefaultParent: "Forge-epic"}},
		},
	}
	args := strings.Join(s.createArgs("forge", "Vulnerability: GO-1", "d", 1), " ")
	if !strings.Contains(args, "--parent Forge-epic --no-inherit-labels") || !strings.Contains(args, "--type bug") {
		t.Fatalf("args = %s", args)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected warning %q", logs.String())
	}
}

func TestCreateArgs_ResolverWinsOverConstructionConfig(t *testing.T) {
	s := &Scanner{
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		anvils: map[string]config.AnvilConfig{"forge": {Beads: config.BeadsConfig{DefaultParent: "stale"}}},
	}
	s.SetBeadParent(func(string) string { return "Forge-live" })
	if args := strings.Join(s.createArgs("forge", "t", "d", 1), " "); !strings.Contains(args, "--parent Forge-live") {
		t.Fatalf("the live resolver must win, args = %s", args)
	}
}

func TestCreateArgs_NoParentWarns(t *testing.T) {
	var logs bytes.Buffer
	s := &Scanner{logger: slog.New(slog.NewTextHandler(&logs, nil)), anvils: map[string]config.AnvilConfig{}}
	args := s.createArgs("forge", "t", "d", 2)
	if strings.Contains(strings.Join(args, " "), "--parent") {
		t.Fatalf("no parent configured, yet args = %q", args)
	}
	if !strings.Contains(logs.String(), "no parent epic") || !strings.Contains(logs.String(), "kind=vulncheck") {
		t.Errorf("want a no-parent warning, logged %q", logs.String())
	}
}
